package mailstore

import (
	"context"
	"errors"
	"fmt"
	stdmail "net/mail"
	"sort"
	"strings"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

const senderIdentityRecentLimit = 2000

type senderIdentity struct {
	Address      string
	Name         string
	MessageCount int64
	LatestSent   int64
	nameCount    int64
}

func (s *Store) ListAccounts(ctx context.Context) ([]mail.Account, error) {
	records, err := s.mailboxRecords(ctx)
	if err != nil {
		return nil, err
	}
	recordsByPath := make(map[string]mailboxRecord, len(records))
	for _, record := range records {
		recordsByPath[record.pathKey] = record
	}
	accounts := make([]mail.Account, 0, len(s.activeAccounts))
	for _, location := range s.activeAccounts {
		// Degraded accounts stay listed (state+reason on the Account); only
		// hard SQL failures abort discovery for everyone.
		account, err := s.loadAccount(ctx, location, recordsByPath)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, nil
}

func (s *Store) loadAccount(
	ctx context.Context,
	location mailboxLocation,
	records map[string]mailboxRecord,
) (mail.Account, error) {
	ref, err := mailref.EncodeAccount(location.AccountID)
	if err != nil {
		return mail.Account{}, err
	}
	degraded := func(reason string) (mail.Account, error) {
		return mail.Account{
			Ref: ref, Name: "On My Mac", EmailAddresses: []string{},
			State: "degraded", DegradedReason: reason,
		}, nil
	}

	// Unreadable mailbox cache: listed degraded, other accounts unaffected.
	cached, err := s.loadMailboxCache(ctx, location.AccountID)
	if err != nil {
		if isHardCatalogError(err) {
			return mail.Account{}, accountCatalogError(location.AccountID, err)
		}
		return degraded("mailbox_cache_unreadable")
	}
	sentMailboxIDs, foundSent, err := strictSpecialMailboxIDs(
		cached.Mailboxes, location, mailboxAttributeSent, records,
	)
	if err != nil {
		if isHardCatalogError(err) {
			return mail.Account{}, accountCatalogError(location.AccountID, err)
		}
		return degraded("special_use_mailbox_unresolved")
	}
	identities := []senderIdentity(nil)
	if foundSent {
		identities, err = s.loadSenderIdentities(ctx, sentMailboxIDs)
		if err != nil {
			// SQL identity failures stay hard: identity correctness feeds
			// mutation credential resolution.
			return mail.Account{}, accountCatalogError(location.AccountID, err)
		}
	}
	if location.Scheme == "imap" && (!foundSent || len(identities) == 0) {
		return degraded("no_provably_sent_identity")
	}
	name := "On My Mac"
	addresses := make([]string, len(identities))
	for index, identity := range identities {
		addresses[index] = identity.Address
	}
	if len(identities) > 0 {
		name = identities[0].Name
		if name == "" {
			name = identities[0].Address
		}
	}
	return mail.Account{Ref: ref, Name: name, EmailAddresses: addresses, State: "ok"}, nil
}

// isHardCatalogError reports whether a catalog failure must abort the whole
// listing (SQL/store failures) instead of degrading one account. Degraded
// causes are typed codes: unreadable mailbox cache, unsafe cache path
// segments, and cache-declared special-use mailboxes missing from the
// Envelope Index.
func isHardCatalogError(err error) bool {
	if err == nil {
		return false
	}
	var typed *Error
	if errors.As(err, &typed) {
		switch typed.Code {
		case "invalid_mailbox_cache", "mailbox_cache_malformed", "invalid_path_segment", "special_use_mailbox_unresolved":
			return false
		}
		return true
	}
	return true
}

func strictSpecialMailboxIDs(
	nodes map[string]mailboxCacheNode,
	account mailboxLocation,
	attribute int,
	records map[string]mailboxRecord,
) ([]int64, bool, error) {
	unique := make(map[int64]struct{})
	var walk func(map[string]mailboxCacheNode, []string) error
	walk = func(current map[string]mailboxCacheNode, parent []string) error {
		keys := make([]string, 0, len(current))
		for key := range current {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			node := current[key]
			component := node.PathComponent
			if component == "" {
				component = key
			}
			if err := validatePathSegment(component); err != nil {
				return err
			}
			rawPath := make([]string, len(parent)+1)
			copy(rawPath, parent)
			rawPath[len(parent)] = component
			if node.Attributes&attribute != 0 {
				visiblePath := append([]string(nil), rawPath...)
				if len(visiblePath) > 1 && visiblePath[0] == "[Gmail]" {
					visiblePath = visiblePath[1:]
				}
				record, found := records[mailboxPathKey(account.AccountID, visiblePath)]
				if !found {
					return operationError(
						"special_use_mailbox_unresolved",
						fmt.Sprintf("special-use mailbox %q is missing from the Envelope Index",
							strings.Join(visiblePath, "/")),
					)
				}
				unique[record.RowID] = struct{}{}
			}
			if err := walk(node.Children, rawPath); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(nodes, nil); err != nil {
		return nil, false, err
	}
	identifiers := make([]int64, 0, len(unique))
	for identifier := range unique {
		identifiers = append(identifiers, identifier)
	}
	sort.Slice(identifiers, func(left int, right int) bool {
		return identifiers[left] < identifiers[right]
	})
	return identifiers, len(identifiers) > 0, nil
}

// loadSenderIdentities derives sender identity from the 2000 newest sent
// messages across the account's union of Sent mailboxes.
func (s *Store) loadSenderIdentities(
	ctx context.Context,
	mailboxIDs []int64,
) (result []senderIdentity, resultErr error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(mailboxIDs)), ",")
	arguments := make([]any, 0, len(mailboxIDs)*2+1)
	for _, identifier := range mailboxIDs {
		arguments = append(arguments, identifier)
	}
	for _, identifier := range mailboxIDs {
		arguments = append(arguments, identifier)
	}
	arguments = append(arguments, senderIdentityRecentLimit)
	rows, err := s.database.QueryContext(ctx, `
		WITH sent_membership(id) AS (
			SELECT ROWID FROM messages WHERE mailbox IN (`+placeholders+`)
			UNION
			SELECT message_id FROM labels WHERE mailbox_id IN (`+placeholders+`)
		), membership(id) AS (
			SELECT sent.id
			FROM sent_membership sent
			JOIN messages message ON message.ROWID = sent.id
			WHERE message.deleted = 0
			ORDER BY COALESCE(message.date_sent, message.date_received) DESC, message.ROWID DESC
			LIMIT ?
		)
		SELECT COALESCE(sender.address, ''), COALESCE(sender.comment, ''),
			count(*), max(COALESCE(message.date_sent, message.date_received, 0))
		FROM membership
		JOIN messages message ON message.ROWID = membership.id
		JOIN addresses sender ON sender.ROWID = message.sender
		GROUP BY sender.address, sender.comment
	`, arguments...)
	if err != nil {
		return nil, fmt.Errorf("query Sent sender identities: %w", err)
	}
	defer joinCloseError(&resultErr, rows, "Sent sender identity rows")
	identities := make(map[string]senderIdentity)
	for rows.Next() {
		var address string
		var name string
		var messageCount int64
		var latestSent int64
		if err := rows.Scan(&address, &name, &messageCount, &latestSent); err != nil {
			return nil, fmt.Errorf("scan Sent sender identity: %w", err)
		}
		parsed, err := stdmail.ParseAddress(strings.TrimSpace(address))
		if err != nil || strings.TrimSpace(parsed.Address) == "" {
			return nil, fmt.Errorf("sent mailbox contains an invalid sender address %q", address)
		}
		key := strings.ToLower(parsed.Address)
		identity := identities[key]
		if identity.Address == "" {
			identity.Address = parsed.Address
		}
		identity.MessageCount += messageCount
		if latestSent > identity.LatestSent {
			identity.LatestSent = latestSent
		}
		name = strings.TrimSpace(name)
		if name != "" && messageCount > identity.nameCount {
			identity.Name = name
			identity.nameCount = messageCount
		}
		identities[key] = identity
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Sent sender identities: %w", err)
	}
	result = make([]senderIdentity, 0, len(identities))
	for _, identity := range identities {
		result = append(result, identity)
	}
	sort.Slice(result, func(left int, right int) bool {
		if result[left].MessageCount != result[right].MessageCount {
			return result[left].MessageCount > result[right].MessageCount
		}
		if result[left].LatestSent != result[right].LatestSent {
			return result[left].LatestSent > result[right].LatestSent
		}
		return strings.ToLower(result[left].Address) < strings.ToLower(result[right].Address)
	})
	return result, nil
}

func accountCatalogError(accountID string, err error) error {
	return operationError(
		"account_catalog_incomplete",
		fmt.Sprintf("cannot prove the complete account catalog for account %s: %v", accountID, err),
	)
}
