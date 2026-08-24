package mailstore

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

type mailboxRecord struct {
	RowID        int64
	URL          string
	Location     mailboxLocation
	MessageCount int
	UnreadCount  int
}

type mailboxCacheNode struct {
	PathComponent string                      `json:"MailboxPathComponent"`
	UnreadCount   int                         `json:"MailboxUnreadCount"`
	Attributes    int                         `json:"IMAPMailboxAttributes"`
	Children      map[string]mailboxCacheNode `json:"IMAPMailboxChildren"`
}

type mailboxCache struct {
	Mailboxes map[string]mailboxCacheNode `json:"mboxes"`
}

func (s *Store) ListMailboxes(ctx context.Context, request mail.ListMailboxesRequest) ([]mail.Mailbox, error) {
	accountID, err := s.requestedAccountID(request.AccountRef)
	if err != nil {
		return nil, err
	}
	records, err := s.loadMailboxRecords(ctx)
	if err != nil {
		return nil, err
	}
	recordsByPath := make(map[string]mailboxRecord, len(records))
	accountSchemes := make(map[string]string)
	for key := range s.activeAccountKeys {
		location, err := parseAccountRoot(key + "/")
		if err != nil {
			return nil, operationError("mail_store_preferences_invalid", err.Error())
		}
		if accountID == "" || location.AccountID == accountID {
			accountSchemes[location.AccountID] = location.Scheme
		}
	}
	for _, record := range records {
		if accountID != "" && record.Location.AccountID != accountID {
			continue
		}
		recordsByPath[mailboxPathKey(record.Location.AccountID, record.Location.VisiblePath)] = record
		accountSchemes[record.Location.AccountID] = record.Location.Scheme
	}
	mailboxes := make([]mail.Mailbox, 0, len(records))
	seen := make(map[string]struct{}, len(records))
	for candidateAccountID, scheme := range accountSchemes {
		cached, err := s.loadMailboxCache(ctx, candidateAccountID)
		if err != nil {
			return nil, operationError(
				"mailbox_catalog_incomplete",
				fmt.Sprintf("cannot prove the complete mailbox catalog for account %s: %v", candidateAccountID, err),
			)
		}
		if err := appendCachedMailboxes(
			&mailboxes, seen, recordsByPath, candidateAccountID, scheme,
			nil, cached.Mailboxes,
		); err != nil {
			return nil, operationError(
				"mailbox_catalog_incomplete",
				fmt.Sprintf("cannot prove the complete mailbox catalog for account %s: %v", candidateAccountID, err),
			)
		}
	}
	for _, record := range records {
		if accountID != "" && record.Location.AccountID != accountID {
			continue
		}
		key := mailboxPathKey(record.Location.AccountID, record.Location.VisiblePath)
		if _, exists := seen[key]; exists {
			continue
		}
		item, err := mapMailbox(record.Location, record.MessageCount, record.UnreadCount, true)
		if err != nil {
			return nil, err
		}
		mailboxes = append(mailboxes, item)
		seen[key] = struct{}{}
	}
	sort.Slice(mailboxes, func(left int, right int) bool {
		leftKey := mailboxes[left].AccountRef + "\x00" + strings.Join(mailboxes[left].Path, "\x00")
		rightKey := mailboxes[right].AccountRef + "\x00" + strings.Join(mailboxes[right].Path, "\x00")
		return leftKey < rightKey
	})
	return mailboxes, nil
}

func (s *Store) loadMailboxRecords(ctx context.Context) ([]mailboxRecord, error) {
	rows, err := s.database.QueryContext(ctx, `
		SELECT ROWID, url, total_count, unread_count
		FROM mailboxes
		ORDER BY url
	`)
	if err != nil {
		return nil, fmt.Errorf("list Envelope Index mailboxes: %w", err)
	}
	defer rows.Close()
	var records []mailboxRecord
	for rows.Next() {
		var record mailboxRecord
		if err := rows.Scan(&record.RowID, &record.URL, &record.MessageCount, &record.UnreadCount); err != nil {
			return nil, fmt.Errorf("scan Envelope Index mailbox: %w", err)
		}
		location, err := parseMailboxURL(record.URL)
		if err != nil {
			return nil, operationError(
				"unsupported_mail_store_schema", fmt.Sprintf("Envelope Index contains an unsafe mailbox URL: %v", err),
			)
		}
		if _, active := s.activeAccountKeys[location.rootKey()]; !active {
			continue
		}
		record.Location = location
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Envelope Index mailboxes: %w", err)
	}
	return records, nil
}

func appendCachedMailboxes(
	output *[]mail.Mailbox,
	seen map[string]struct{},
	records map[string]mailboxRecord,
	accountID string,
	scheme string,
	parent []string,
	nodes map[string]mailboxCacheNode,
) error {
	keys := make([]string, 0, len(nodes))
	for key := range nodes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		node := nodes[key]
		component := node.PathComponent
		if component == "" {
			component = key
		}
		if err := validatePathSegment(component); err != nil {
			return err
		}
		rawPath := append(append([]string(nil), parent...), component)
		visiblePath := append([]string(nil), rawPath...)
		if len(visiblePath) > 1 && visiblePath[0] == "[Gmail]" {
			visiblePath = visiblePath[1:]
		}
		if len(visiblePath) > 0 && visiblePath[0] != "[Gmail]" {
			location := mailboxLocation{
				Scheme: scheme, AccountID: accountID, RawPath: rawPath, VisiblePath: visiblePath,
			}
			pathKey := mailboxPathKey(accountID, visiblePath)
			if _, exists := seen[pathKey]; exists {
				return fmt.Errorf("mailbox cache contains duplicate path %q", strings.Join(visiblePath, "/"))
			}
			record, available := records[pathKey]
			messageCount := 0
			unreadCount := node.UnreadCount
			if available {
				messageCount = record.MessageCount
				unreadCount = record.UnreadCount
			}
			item, err := mapMailbox(location, messageCount, unreadCount, available)
			if err != nil {
				return err
			}
			*output = append(*output, item)
			seen[pathKey] = struct{}{}
		}
		if err := appendCachedMailboxes(output, seen, records, accountID, scheme, rawPath, node.Children); err != nil {
			return err
		}
	}
	return nil
}

func mapMailbox(
	location mailboxLocation,
	messageCount int,
	unreadCount int,
	available bool,
) (mail.Mailbox, error) {
	accountRef, err := mailref.EncodeAccount(location.AccountID)
	if err != nil {
		return mail.Mailbox{}, err
	}
	mailboxRef, err := mailref.EncodeMailbox(location.AccountID, location.VisiblePath)
	if err != nil {
		return mail.Mailbox{}, err
	}
	return mail.Mailbox{
		Ref: mailboxRef, AccountRef: accountRef,
		Name: location.VisiblePath[len(location.VisiblePath)-1], Path: location.VisiblePath,
		UnreadCount: unreadCount, MessageCount: messageCount,
		LocalMessagesAvailable: available,
	}, nil
}

func (s *Store) requestedAccountID(accountRef string) (string, error) {
	if accountRef == "" {
		return "", nil
	}
	account, err := mailref.DecodeAccount(accountRef)
	if err != nil {
		return "", operationError("invalid_reference", fmt.Sprintf("invalid account ref: %v", err))
	}
	accountID := strings.ToUpper(account.AccountID)
	if !s.activeAccountID(accountID) {
		return "", operationError("stale_reference", "account is not active in the local Mail store")
	}
	return accountID, nil
}

func (s *Store) activeAccountID(accountID string) bool {
	accountID = strings.ToUpper(accountID)
	_, imapActive := s.activeAccountKeys["imap://"+accountID]
	_, localActive := s.activeAccountKeys["local://"+accountID]
	return imapActive || localActive
}

func mailboxPathKey(accountID string, path []string) string {
	return strings.ToUpper(accountID) + "\x00" + strings.Join(path, "\x00")
}

func findMailboxRecord(records []mailboxRecord, accountID string, path []string) (mailboxRecord, bool) {
	wanted := mailboxPathKey(accountID, path)
	for _, record := range records {
		if mailboxPathKey(record.Location.AccountID, record.Location.VisiblePath) == wanted {
			return record, true
		}
	}
	return mailboxRecord{}, false
}
