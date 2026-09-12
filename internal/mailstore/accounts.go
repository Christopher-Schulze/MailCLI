package mailstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	stdmail "net/mail"
	"sort"
	"strings"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

const senderIdentityRecentLimit = mail.DefaultSenderIdentityScanLimit

const (
	accountDegradedMailboxCache         = "mailbox_cache_unreadable"
	accountDegradedSpecialUse           = "special_use_mailbox_unresolved"
	accountDegradedSenderIdentity       = "sender_identity_unreadable"
	accountDegradedNoSenderIdentity     = "no_provably_sent_identity"
	accountDegradedSenderNotObserved    = "sender_identity_not_observed"
	accountRemediationMailboxCache      = "run `mailcli doctor` and let Mail.app rebuild the affected mailbox cache"
	accountRemediationSpecialUse        = "run `mailcli doctor` and let Mail.app resync the affected account so its special-use mailbox is present"
	accountRemediationSenderIdentity    = "run `mailcli doctor` and repair the affected Sent data before retrying"
	accountRemediationNoSenderIdentity  = "run `mailcli send setup --from ADDRESS` or complete one successful send, then rerun `mailcli accounts list`"
	accountRemediationSenderNotObserved = "run `mailcli send setup --from ADDRESS` or increase the bounded sender identity scan before rerunning `mailcli accounts list`"
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
	cause    error
	coverage mail.SenderIdentityCoverage
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
	return s.listAccountCatalog(ctx, false)
}

// ListBindingValidationCatalog lists the account catalog for send-time
// binding validation. Bound accounts skip the Sent-history sender scan:
// their permitted senders come from configured aliases, so the scan cannot
// change the validation outcome. Unbound accounts keep full resolution.
func (s *Store) ListBindingValidationCatalog(ctx context.Context) (mail.AccountCatalog, error) {
	return s.listAccountCatalog(ctx, true)
}

func (s *Store) listAccountCatalog(ctx context.Context, skipBoundIdentityScan bool) (mail.AccountCatalog, error) {
	records, err := s.mailboxRecords(ctx)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return mail.AccountCatalog{}, contextErr
		}
		return mail.AccountCatalog{}, accountCatalogError("", err)
	}
	bindings, err := s.loadAccountBindings()
	if err != nil {
		return mail.AccountCatalog{}, err
	}
	recordsByPath := make(map[string]mailboxRecord, len(records))
	for _, record := range records {
		recordsByPath[record.pathKey] = record
	}
	accounts := make([]mail.Account, 0, len(s.activeAccounts))
	for _, location := range s.activeAccounts {
		// Degraded accounts stay listed (state+reason on the Account); only
		// hard SQL failures abort discovery for everyone.
		account, err := s.loadAccountWithBindings(ctx, location, recordsByPath, bindings, skipBoundIdentityScan)
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

func (s *Store) loadAccountBindings() (mail.AccountBindingFile, error) {
	if s.accountBindings == nil {
		return mail.AccountBindingFile{Version: mail.AccountBindingVersion, Bindings: []mail.AccountBinding{}}, nil
	}
	return s.accountBindings.LoadAccountBindings()
}

func (s *Store) loadAccount(
	ctx context.Context,
	location mailboxLocation,
	records map[string]mailboxRecord,
) (mail.Account, error) {
	return s.loadAccountWithBindings(ctx, location, records, mail.AccountBindingFile{
		Version: mail.AccountBindingVersion, Bindings: []mail.AccountBinding{},
	}, false)
}

func (s *Store) loadAccountWithBindings(
	ctx context.Context,
	location mailboxLocation,
	records map[string]mailboxRecord,
	bindings mail.AccountBindingFile,
	skipBoundIdentityScan bool,
) (mail.Account, error) {
	ref, err := mailref.EncodeAccount(location.AccountID)
	if err != nil {
		return mail.Account{}, accountCatalogError(location.AccountID, err)
	}
	binding, bindingFound, err := mail.FindAccountBinding(bindings, location.AccountID)
	if err != nil {
		return mail.Account{}, err
	}
	accountType := mail.AccountType(location.Scheme)
	displayName := "On My Mac"
	if accountType == mail.AccountTypeIMAP {
		displayName = "IMAP account"
	}
	baseAccount := mail.Account{
		Ref: ref, Name: displayName, Type: accountType, DisplayName: displayName,
		EmailAddresses: []string{}, DiscoveredSenderIdentities: []string{},
		ConfiguredSenderAliases: []string{}, State: "ok",
	}
	if bindingFound && accountType == mail.AccountTypeIMAP {
		baseAccount.ConfiguredSenderAliases = append([]string(nil), binding.SenderAliases...)
	}
	limit := s.senderIdentityLimit()
	unavailableCoverage := senderIdentityCoverage(
		mail.SenderIdentityCoverageStateUnavailable, limit, 0, false,
	)
	degraded := func(
		reason string,
		remediation string,
		coverage mail.SenderIdentityCoverage,
		cause error,
	) (mail.Account, error) {
		account := baseAccount
		account.State = "degraded"
		account.DegradedReason = reason
		account.DegradedRemediation = remediation
		account.IdentityCoverage = coverage
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
		return degraded(accountDegradedMailboxCache, accountRemediationMailboxCache, unavailableCoverage, err)
	}
	sentMailboxIDs, foundSent, err := strictSpecialMailboxIDs(
		cached.Mailboxes, location, mailboxAttributeSent, records,
	)
	if err != nil {
		return degraded(accountDegradedSpecialUse, accountRemediationSpecialUse, unavailableCoverage, err)
	}
	if location.Scheme != "imap" && !foundSent {
		baseAccount.IdentityCoverage = mail.SenderIdentityCoverage{
			Source: mail.SenderIdentityCoverageSourceNotApplicable,
			State:  mail.SenderIdentityCoverageStateNotApplicable,
		}
		return baseAccount, nil
	}
	if skipBoundIdentityScan && bindingFound && accountType == mail.AccountTypeIMAP {
		if len(binding.SenderAliases) > 0 {
			baseAccount.DisplayName = binding.SenderAliases[0]
		}
		baseAccount.Name = baseAccount.DisplayName
		baseAccount.EmailAddresses = mergeAccountAddresses(baseAccount.ConfiguredSenderAliases)
		baseAccount.IdentityCoverage = mail.SenderIdentityCoverage{
			Source: mail.SenderIdentityCoverageSourceAccountBinding,
			State:  mail.SenderIdentityCoverageStateConfigured,
		}
		return baseAccount, nil
	}
	identityResult := senderIdentityResult{
		coverage: senderIdentityCoverage(
			mail.SenderIdentityCoverageStateNoSentMailbox, limit, 0, false,
		),
	}
	if foundSent {
		identityResult, err = s.loadSenderIdentityResult(ctx, sentMailboxIDs)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return mail.Account{}, contextErr
			}
			var dataErr *senderIdentityDataError
			if errors.As(err, &dataErr) {
				return degraded(accountDegradedSenderIdentity, accountRemediationSenderIdentity, dataErr.coverage, err)
			}
			// Query and iteration failures can indicate a global SQL/schema
			// problem, so they abort the catalog instead of degrading an account.
			return mail.Account{}, accountCatalogError(location.AccountID, err)
		}
	}
	if location.Scheme == "imap" && !foundSent && !bindingFound {
		return degraded(accountDegradedNoSenderIdentity, accountRemediationNoSenderIdentity, identityResult.coverage, nil)
	}
	if location.Scheme == "imap" && len(identityResult.identities) == 0 && !bindingFound {
		if identityResult.coverage.State == mail.SenderIdentityCoverageStateNotObserved {
			return degraded(accountDegradedSenderNotObserved, accountRemediationSenderNotObserved, identityResult.coverage, nil)
		}
		return degraded(accountDegradedNoSenderIdentity, accountRemediationNoSenderIdentity, identityResult.coverage, nil)
	}
	discovered := make([]string, len(identityResult.identities))
	for index, identity := range identityResult.identities {
		discovered[index] = identity.Address
	}
	if len(identityResult.identities) > 0 {
		baseAccount.DisplayName = identityResult.identities[0].Name
		if baseAccount.DisplayName == "" {
			baseAccount.DisplayName = identityResult.identities[0].Address
		}
	} else if bindingFound && len(binding.SenderAliases) > 0 {
		baseAccount.DisplayName = binding.SenderAliases[0]
	}
	baseAccount.Name = baseAccount.DisplayName
	baseAccount.DiscoveredSenderIdentities = discovered
	baseAccount.EmailAddresses = mergeAccountAddresses(discovered, baseAccount.ConfiguredSenderAliases)
	baseAccount.IdentityCoverage = identityResult.coverage
	if bindingFound && accountType == mail.AccountTypeIMAP {
		baseAccount.IdentityCoverage.Source = mail.SenderIdentityCoverageSourceAccountBinding
		baseAccount.IdentityCoverage.State = mail.SenderIdentityCoverageStateConfigured
	}
	return baseAccount, nil
}

func mergeAccountAddresses(groups ...[]string) []string {
	seen := make(map[string]struct{})
	addresses := make([]string, 0)
	for _, group := range groups {
		for _, address := range group {
			key := strings.ToLower(strings.TrimSpace(address))
			if key == "" {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			addresses = append(addresses, address)
		}
	}
	sort.Slice(addresses, func(left, right int) bool {
		return strings.ToLower(addresses[left]) < strings.ToLower(addresses[right])
	})
	return addresses
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

type senderIdentityResult struct {
	identities []senderIdentity
	coverage   mail.SenderIdentityCoverage
}

// loadSenderIdentities preserves the package-local helper used by existing
// callers while the account listing also consumes explicit coverage evidence.
func (s *Store) loadSenderIdentities(
	ctx context.Context,
	mailboxIDs []int64,
) ([]senderIdentity, error) {
	result, err := s.loadSenderIdentityResult(ctx, mailboxIDs)
	return result.identities, err
}

// loadSenderIdentityResult aggregates only the configured newest Sent
// messages and separately checks one bounded successor row for coverage.
func (s *Store) loadSenderIdentityResult(
	ctx context.Context,
	mailboxIDs []int64,
) (senderIdentityResult, error) {
	limit := s.senderIdentityLimit()
	result := senderIdentityResult{
		coverage: senderIdentityCoverage(
			mail.SenderIdentityCoverageStateNoValidSender, limit, 0, false,
		),
	}
	if len(mailboxIDs) == 0 {
		return result, nil
	}
	identities, observed, err := s.querySenderIdentityRows(ctx, mailboxIDs, limit)
	if err != nil {
		return result, err
	}
	moreAvailable, err := s.hasMoreSenderIdentityRows(ctx, mailboxIDs, limit)
	if err != nil {
		return result, err
	}
	state := mail.SenderIdentityCoverageStateComplete
	if moreAvailable {
		state = mail.SenderIdentityCoverageStateBounded
		if len(identities) == 0 {
			state = mail.SenderIdentityCoverageStateNotObserved
		}
	} else if len(identities) == 0 {
		state = mail.SenderIdentityCoverageStateNoValidSender
	}
	result.identities = identities
	result.coverage = senderIdentityCoverage(state, limit, observed, moreAvailable)
	return result, nil
}

func (s *Store) querySenderIdentityRows(
	ctx context.Context,
	mailboxIDs []int64,
	limit int,
) (result []senderIdentity, observed int, resultErr error) {
	cte, arguments := senderIdentityMembershipQuery(mailboxIDs, limit, limit, 0)
	rows, err := s.database.QueryContext(ctx, cte+`
		SELECT (SELECT count(*) FROM membership), sender.ROWID,
			COALESCE(sender.address, ''), COALESCE(sender.comment, ''),
			count(*), max(COALESCE(message.date_sent, message.date_received, 0))
		FROM membership
		JOIN messages message ON message.ROWID = membership.id
		LEFT JOIN addresses sender ON sender.ROWID = message.sender
		GROUP BY sender.address, sender.comment
	`, arguments...)
	if err != nil {
		return nil, 0, fmt.Errorf("query Sent sender identities: %w", err)
	}
	defer joinCloseError(&resultErr, rows, "Sent sender identity rows")
	identities := make(map[string]senderIdentity)
	for rows.Next() {
		var address string
		var name string
		var senderID sql.NullInt64
		var observedCount int64
		var messageCount int64
		var latestSent int64
		if err := rows.Scan(&observedCount, &senderID, &address, &name, &messageCount, &latestSent); err != nil {
			if observedCount > 0 {
				observed = int(observedCount)
			}
			coverage := senderIdentityCoverage(
				mail.SenderIdentityCoverageStateUnavailable,
				limit, min(observed, limit), false,
			)
			return nil, observed, &senderIdentityDataError{
				cause:    fmt.Errorf("scan Sent sender identity: %w", err),
				coverage: coverage,
			}
		}
		observed = int(observedCount)
		if !senderID.Valid {
			continue
		}
		parsed, err := stdmail.ParseAddress(strings.TrimSpace(address))
		if err != nil || strings.TrimSpace(parsed.Address) == "" {
			coverage := senderIdentityCoverage(
				mail.SenderIdentityCoverageStateUnavailable,
				limit, min(observed, limit), false,
			)
			return nil, observed, &senderIdentityDataError{
				cause: fmt.Errorf(
					"sent mailbox contains an invalid sender address %q", address,
				),
				coverage: coverage,
			}
		}
		key := strings.ToLower(parsed.Address)
		identity := identities[key]
		if identity.Address == "" {
			identity.Address = mail.MailboxAddrSpec(parsed.Address)
		}
		identity.MessageCount += messageCount
		if latestSent > identity.LatestSent {
			identity.LatestSent = latestSent
		}
		name = strings.TrimSpace(name)
		if name != "" &&
			(messageCount > identity.nameCount ||
				messageCount == identity.nameCount && strings.ToLower(name) < strings.ToLower(identity.Name)) {
			identity.Name = name
			identity.nameCount = messageCount
		}
		identities[key] = identity
	}
	if err := rows.Err(); err != nil {
		return nil, observed, fmt.Errorf("iterate Sent sender identities: %w", err)
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
	return result, observed, resultErr
}

func (s *Store) hasMoreSenderIdentityRows(
	ctx context.Context,
	mailboxIDs []int64,
	limit int,
) (more bool, resultErr error) {
	cte, arguments := senderIdentityMembershipQuery(mailboxIDs, limit+1, 1, limit)
	rows, err := s.database.QueryContext(ctx, cte+`
		SELECT 1 FROM membership
	`, arguments...)
	if err != nil {
		return false, fmt.Errorf("query Sent sender identity coverage: %w", err)
	}
	defer joinCloseError(&resultErr, rows, "Sent sender identity coverage rows")
	more = rows.Next()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate Sent sender identity coverage: %w", err)
	}
	return more, resultErr
}

func senderIdentityMembershipQuery(
	mailboxIDs []int64,
	perMailboxLimit int,
	resultLimit int,
	resultOffset int,
) (string, []any) {
	arguments := make([]any, 0, len(mailboxIDs)*4+2)
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
		arguments = append(arguments, identifier, perMailboxLimit, identifier, perMailboxLimit)
	}
	arguments = append(arguments, resultLimit, resultOffset)
	return `WITH sent_membership(id) AS (
		` + strings.Join(arms, "\n\t\tUNION\n\t\t") + `
	), membership(id) AS (
		SELECT sent.id
		FROM sent_membership sent
		JOIN messages message ON message.ROWID = sent.id
		WHERE message.deleted = 0
		ORDER BY COALESCE(message.date_sent, message.date_received) DESC, message.ROWID DESC
		LIMIT ? OFFSET ?
	)
	`, arguments
}

func (s *Store) senderIdentityLimit() int {
	if s.senderIdentityScanLimit >= 1 && s.senderIdentityScanLimit <= mail.MaximumSenderIdentityScanLimit {
		return s.senderIdentityScanLimit
	}
	return senderIdentityRecentLimit
}

func senderIdentityCoverage(
	state mail.SenderIdentityCoverageState,
	limit int,
	observed int,
	moreAvailable bool,
) mail.SenderIdentityCoverage {
	return mail.SenderIdentityCoverage{
		Source:           mail.SenderIdentityCoverageSourceSentHistory,
		State:            state,
		ObservedMessages: observed,
		Limit:            limit,
		MoreAvailable:    moreAvailable,
	}
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
