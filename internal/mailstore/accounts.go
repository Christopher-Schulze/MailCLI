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

const (
	accountDegradedMailboxCache        = "mailbox_cache_unreadable"
	accountDegradedSpecialUse          = "special_use_mailbox_unresolved"
	accountDegradedSenderIdentity      = "sender_identity_unreadable"
	accountDegradedNoSenderIdentity    = "no_provably_sent_identity"
	accountRemediationMailboxCache     = "run `mailcli doctor` and let Mail.app rebuild the affected mailbox cache"
	accountRemediationSpecialUse       = "run `mailcli doctor` and let Mail.app resync the affected account so its special-use mailbox is present"
	accountRemediationSenderIdentity   = "run `mailcli doctor` and repair the affected Sent data before retrying"
	accountRemediationNoSenderIdentity = "run `mailcli send setup --from ADDRESS` or complete one successful send, then rerun `mailcli accounts list`"
)

type senderIdentity struct {
	Address      string
	Name         string
	MessageCount int64
	LatestSent   int64
	nameCount    int64
}

type accountCatalogIssue struct {
	accountID string
	account   mail.Account
	cause     error
}

func (e *accountCatalogIssue) Error() string {
	message := fmt.Sprintf(
		"account %s is degraded: %s; remediation: %s",
		e.accountID, e.account.DegradedReason, e.account.DegradedRemediation,
	)
	if e.cause != nil {
		message += ": " + e.cause.Error()
	}
	return message
}

func (e *accountCatalogIssue) Unwrap() error {
	return e.cause
}

type senderIdentityDataError struct {
	cause error
}

func (e *senderIdentityDataError) Error() string {
	return e.cause.Error()
}

func (e *senderIdentityDataError) Unwrap() error {
	return e.cause
}

func (s *Store) ListAccounts(ctx context.Context) ([]mail.Account, error) {
	catalog, err := s.ListAccountCatalog(ctx)
	return catalog.Accounts, err
}

func (s *Store) ListAccountCatalog(ctx context.Context) (mail.AccountCatalog, error) {
	records, err := s.mailboxRecords(ctx)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return mail.AccountCatalog{}, contextErr
		}
		return mail.AccountCatalog{}, accountCatalogError("", err)
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
			var issue *accountCatalogIssue
			if errors.As(err, &issue) {
				accounts = append(accounts, issue.account)
				continue
			}
			if contextErr := ctx.Err(); contextErr != nil {
				return mail.AccountCatalog{}, contextErr
			}
			return mail.AccountCatalog{}, err
		}
		accounts = append(accounts, account)
	}
	complete := true
	for _, account := range accounts {
		if account.State == "degraded" {
			complete = false
			break
		}
	}
	return mail.AccountCatalog{Accounts: accounts, Complete: complete}, nil
}

func (s *Store) loadAccount(
	ctx context.Context,
	location mailboxLocation,
	records map[string]mailboxRecord,
) (mail.Account, error) {
	ref, err := mailref.EncodeAccount(location.AccountID)
	if err != nil {
		return mail.Account{}, accountCatalogError(location.AccountID, err)
	}
	degraded := func(reason string, remediation string, cause error) (mail.Account, error) {
		account := mail.Account{
			Ref: ref, Name: "On My Mac", EmailAddresses: []string{},
			State: "degraded", DegradedReason: reason,
			DegradedRemediation: remediation,
		}
		return mail.Account{}, &accountCatalogIssue{
			accountID: location.AccountID, account: account, cause: cause,
		}
	}

	// Unreadable mailbox cache: listed degraded, other accounts unaffected.
	cached, err := s.loadMailboxCache(ctx, location.AccountID)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return mail.Account{}, contextErr
		}
		return degraded(accountDegradedMailboxCache, accountRemediationMailboxCache, err)
	}
	sentMailboxIDs, foundSent, err := strictSpecialMailboxIDs(
		cached.Mailboxes, location, mailboxAttributeSent, records,
	)
	if err != nil {
		return degraded(accountDegradedSpecialUse, accountRemediationSpecialUse, err)
	}
	identities := []senderIdentity(nil)
	if foundSent {
		identities, err = s.loadSenderIdentities(ctx, sentMailboxIDs)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return mail.Account{}, contextErr
			}
			var dataErr *senderIdentityDataError
			if errors.As(err, &dataErr) {
				return degraded(accountDegradedSenderIdentity, accountRemediationSenderIdentity, err)
			}
			// Query and iteration failures can indicate a global SQL/schema
			// problem, so they abort the catalog instead of degrading an account.
			return mail.Account{}, accountCatalogError(location.AccountID, err)
		}
	}
	if location.Scheme == "imap" && (!foundSent || len(identities) == 0) {
		return degraded(accountDegradedNoSenderIdentity, accountRemediationNoSenderIdentity, nil)
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
	if len(mailboxIDs) == 0 {
		return []senderIdentity{}, nil
	}
	arguments := make([]any, 0, len(mailboxIDs)*4+1)
	arms := make([]string, 0, len(mailboxIDs)*2)
	for range mailboxIDs {
		arms = append(arms,
			`SELECT id FROM (
				SELECT ROWID AS id FROM messages
				WHERE mailbox = ? AND deleted = 0
				ORDER BY COALESCE(date_sent, date_received) DESC, ROWID DESC
				LIMIT ?
			)`,
			`SELECT id FROM (
				SELECT labels.message_id AS id
				FROM labels
				JOIN messages ON messages.ROWID = labels.message_id
				WHERE labels.mailbox_id = ? AND messages.deleted = 0
				ORDER BY COALESCE(messages.date_sent, messages.date_received) DESC, messages.ROWID DESC
				LIMIT ?
			)`,
		)
	}
	for _, identifier := range mailboxIDs {
		arguments = append(arguments, identifier, senderIdentityRecentLimit, identifier, senderIdentityRecentLimit)
	}
	arguments = append(arguments, senderIdentityRecentLimit)
	rows, err := s.database.QueryContext(ctx, `
		WITH sent_membership(id) AS (
			`+strings.Join(arms, "\n\t\t\tUNION\n\t\t\t")+`
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
			return nil, &senderIdentityDataError{cause: fmt.Errorf("scan Sent sender identity: %w", err)}
		}
		parsed, err := stdmail.ParseAddress(strings.TrimSpace(address))
		if err != nil || strings.TrimSpace(parsed.Address) == "" {
			return nil, &senderIdentityDataError{
				cause: fmt.Errorf("sent mailbox contains an invalid sender address %q", address),
			}
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
	scope := "the account catalog"
	if accountID != "" {
		scope = "the account catalog for account " + accountID
	}
	return operationErrorWithCause(
		"account_catalog_incomplete",
		fmt.Sprintf("cannot prove %s; run `mailcli doctor` and retry: %v", scope, err),
		err,
	)
}
