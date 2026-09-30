package mailstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

// newMessagesWindow is how many of the newest server messages one mailbox
// comparison looks at; older messages are outside this evidence.
const newMessagesWindow = 100

// NewMessages compares the newest server messages of the selected mailboxes
// with the local store and returns the ones the store lacks. It reads headers
// only, changes nothing on the server and keeps going after a failing account.
func (c *Client) NewMessages(ctx context.Context, request mail.NewMessagesRequest) (mail.NewMessagesResult, error) {
	result := mail.NewMessagesResult{
		Mailboxes: []mail.NewMailbox{}, Failures: []mail.NewMessagesFailure{}, Skipped: []mail.SyncCheckSkip{},
	}
	if c.store == nil {
		return result, c.safeWriteUnavailableError()
	}
	if strings.HasPrefix(request.MailboxRef, "mbx_") {
		mailbox, err := mailref.DecodeMailbox(request.MailboxRef)
		if err != nil {
			return result, &mail.ValidationError{Code: "invalid_reference", Message: fmt.Sprintf("invalid mailbox ref: %v", err)}
		}
		if request.AccountRef != "" {
			account, err := mailref.DecodeAccount(request.AccountRef)
			if err != nil {
				return result, &mail.ValidationError{Code: "invalid_reference", Message: fmt.Sprintf("invalid account ref: %v", err)}
			}
			if !strings.EqualFold(account.AccountID, mailbox.AccountID) {
				return result, &mail.ValidationError{Code: "invalid_argument", Message: "mailbox ref does not belong to the selected account"}
			}
		}
		accountRef, err := mailref.EncodeAccount(strings.ToUpper(mailbox.AccountID))
		if err != nil {
			return result, err
		}
		request.AccountRef = accountRef
	}
	lister, ok := c.send.ImapClient().(transport.RecentMessageLister)
	if !ok {
		return result, &transport.TransportError{
			Code:    transport.CodeIMAPMutationFailed,
			Message: "the IMAP operator cannot list recent messages",
		}
	}
	accounts, err := c.store.ListAccounts(ctx)
	if err != nil {
		return result, err
	}
	targets := accounts
	if request.AccountRef != "" {
		targets = nil
		for _, account := range accounts {
			if account.Ref == request.AccountRef {
				targets = append(targets, account)
			}
		}
		if len(targets) == 0 {
			return result, operationError("account_not_found", "account ref not found: "+request.AccountRef)
		}
	}
	bindings, err := c.loadAccountBindings()
	if err != nil {
		return result, err
	}
	selector := request.MailboxRef
	if selector == "" {
		selector = "inbox"
	}
	for _, account := range targets {
		c.newMessagesForAccount(ctx, lister, account, selector, bindings, request.Limit, &result)
	}
	result.Complete = len(result.Failures) == 0
	for _, mailbox := range result.Mailboxes {
		result.NewCount += mailbox.NewCount
		if mailbox.State != mail.NewMailboxStateChecked {
			result.Complete = false
		}
	}
	return result, nil
}

func (c *Client) loadAccountBindings() (mail.AccountBindingFile, error) {
	bindings := mail.AccountBindingFile{Version: mail.AccountBindingVersion, Bindings: []mail.AccountBinding{}}
	store := c.send.AccountBindings
	if store == nil && c.store != nil {
		store = c.store.accountBindings
	}
	if store == nil {
		return bindings, nil
	}
	return store.LoadAccountBindings()
}

func (c *Client) newMessagesForAccount(
	ctx context.Context,
	lister transport.RecentMessageLister,
	account mail.Account,
	selector string,
	bindings mail.AccountBindingFile,
	limit int,
	result *mail.NewMessagesResult,
) {
	fail := func(mailbox string, err error) {
		result.Failures = append(result.Failures, mail.NewMessagesFailure{
			Account: account.Ref, Mailbox: mailbox, Code: newMessagesFailureCode(ctx, err), Message: err.Error(),
		})
	}
	if account.State == "degraded" {
		fail("", operationError("account_degraded", "account is degraded: "+account.DegradedReason+"; remediation: "+account.DegradedRemediation))
		return
	}
	if skip, skipped := syncSkipForAccount(account); skipped {
		result.Skipped = append(result.Skipped, skip)
		return
	}
	mailboxes, err := c.store.selectedSearchMailboxes(ctx, selector, account.Ref)
	if err != nil {
		fail("", err)
		return
	}
	email, cfg, err := imapConfigForAccount(ctx, account, c.send.Credentials, bindings)
	if err != nil {
		fail("", err)
		return
	}
	serverBoxes, err := c.getOrLoadMailboxes(ctx, c.send.ImapClient(), cfg, email)
	if err != nil {
		fail("", err)
		return
	}
	for _, mailbox := range mailboxes {
		compared, err := c.compareMailbox(ctx, lister, cfg, serverBoxes, mailbox, limit)
		if err != nil {
			fail(strings.Join(mailbox.Path, "/"), err)
			continue
		}
		result.Mailboxes = append(result.Mailboxes, compared)
	}
}

func (c *Client) compareMailbox(
	ctx context.Context,
	lister transport.RecentMessageLister,
	cfg transport.ImapConfig,
	serverBoxes []transport.MailboxInfo,
	mailbox mail.Mailbox,
	limit int,
) (mail.NewMailbox, error) {
	compared := mail.NewMailbox{
		AccountRef: mailbox.AccountRef, MailboxRef: mailbox.Ref, Name: mailbox.Name,
		State: mail.NewMailboxStateChecked, Messages: []mail.NewMessage{},
	}
	reference, err := mailref.DecodeMailbox(mailbox.Ref)
	if err != nil {
		return compared, &mail.ValidationError{Code: "invalid_reference", Message: fmt.Sprintf("invalid mailbox ref: %v", err)}
	}
	records, err := c.store.mailboxRecords(ctx)
	if err != nil {
		return compared, err
	}
	record, found := findMailboxRecord(records, reference.AccountID, reference.Path)
	if !found {
		compared.State, compared.Reason = mail.NewMailboxStateUnresolved, "the mailbox has no local rows"
		return compared, nil
	}
	imapName, err := mapPathToIMAP(serverBoxes, reference.Path)
	if err != nil {
		return compared, err
	}
	recent, err := lister.ListRecentMessages(ctx, cfg, imapName, newMessagesWindow)
	if err != nil {
		return compared, err
	}
	compared.ServerMessages = recent.Exists
	compared.ScannedMessages = len(recent.Messages)
	compared.WindowLimited = recent.Exists > compared.ScannedMessages
	if recent.UIDValidity == 0 {
		compared.State, compared.Reason = mail.NewMailboxStateUnresolved, "the server did not report UIDVALIDITY"
		return compared, nil
	}
	hasPhysical, hasLabels, err := c.store.mailboxMembership(ctx, record.RowID)
	if err != nil {
		return compared, err
	}
	if hasPhysical {
		localValidity, validityErr := c.store.mailboxUIDValidity(ctx, record.Location)
		if validityErr != nil {
			return compared, validityErr
		}
		if localValidity == 0 {
			compared.State, compared.Reason = mail.NewMailboxStateUnresolved, "local mailbox UIDVALIDITY is unavailable; let Mail.app sync and retry"
			return compared, nil
		}
		if localValidity != recent.UIDValidity {
			compared.State = mail.NewMailboxStateUIDValidityChange
			compared.Reason = fmt.Sprintf("local UIDVALIDITY %d differs from the server's %d", localValidity, recent.UIDValidity)
			return compared, nil
		}
	}
	var missing []transport.RecentMessage
	compared.MatchedBy = mail.NewMatchedByUID
	if !hasPhysical && !hasLabels {
		// No local candidate exists, so no UID equality or generation is asserted.
		missing = append([]transport.RecentMessage(nil), recent.Messages...)
	} else if hasLabels {
		compared.MatchedBy = mail.NewMatchedByHeaders
		missing, err = c.store.missingByHeaderIdentity(ctx, record.RowID, reference.AccountID, reference.Path, recent.Messages)
	} else {
		missing, err = c.store.missingByServerUID(ctx, record.RowID, recent.Messages)
	}
	if err != nil {
		return compared, err
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].UID > missing[j].UID })
	compared.NewCount = len(missing)
	windowExhausted := len(recent.Messages) > 0 && len(missing) == len(recent.Messages) && recent.Exists > len(recent.Messages)
	compared.Truncated = len(missing) > limit || windowExhausted
	for _, message := range missing[:min(limit, len(missing))] {
		serverRef, err := mailref.EncodeServer(mailref.Server{
			AccountID: reference.AccountID, MailboxPath: reference.Path, UIDValidity: recent.UIDValidity, UID: message.UID,
		})
		if err != nil {
			return compared, err
		}
		compared.Messages = append(compared.Messages, mail.NewMessageFromHeader(serverRef, message.Header, message.Seen))
	}
	return compared, nil
}

// mailboxMembership observes visible physical and label-backed candidates.
// Empty membership does not establish how a provider represents the mailbox.
func (s *Store) mailboxMembership(ctx context.Context, mailboxRowID int64) (bool, bool, error) {
	var hasPhysical, hasLabels bool
	if err := s.database.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM messages WHERE mailbox = ? AND deleted = 0),
		       EXISTS(SELECT 1 FROM labels l JOIN messages m ON m.ROWID = l.message_id
		              WHERE l.mailbox_id = ? AND m.deleted = 0)
	`, mailboxRowID, mailboxRowID).Scan(&hasPhysical, &hasLabels); err != nil {
		return false, false, fmt.Errorf("check Envelope Index visible mailbox membership: %w", err)
	}
	return hasPhysical, hasLabels, nil
}

// missingByServerUID returns the server messages whose UID the local mailbox
// row does not hold.
func (s *Store) missingByServerUID(ctx context.Context, mailboxRowID int64, messages []transport.RecentMessage) ([]transport.RecentMessage, error) {
	uids := make([]uint32, len(messages))
	for index, message := range messages {
		uids[index] = message.UID
	}
	local, err := s.localServerUIDs(ctx, mailboxRowID, uids)
	if err != nil {
		return nil, err
	}
	var missing []transport.RecentMessage
	for _, message := range messages {
		if !local[message.UID] {
			missing = append(missing, message)
		}
	}
	return missing, nil
}

func headerIdentityKey(identity mail.HeaderIdentity) string {
	return fmt.Sprintf("%s\x00%d\x00%s", identity.Address, identity.SentUnix, identity.Subject)
}

// missingByHeaderIdentity narrows candidates by sender, sent time and subject,
// then verifies their RFC Message-ID from the local source. A server header
// without sender or date cannot match; an ambiguous candidate fails closed.
func (s *Store) missingByHeaderIdentity(ctx context.Context, mailboxRowID int64, accountID string, mailboxPath []string, messages []transport.RecentMessage) ([]transport.RecentMessage, error) {
	identities := make([]mail.HeaderIdentity, len(messages))
	known := make([]bool, len(messages))
	var first, last int64
	any := false
	for index, message := range messages {
		identity, ok := mail.ParseHeaderIdentity(message.Header)
		identities[index], known[index] = identity, ok
		if !ok {
			continue
		}
		if !any || identity.SentUnix < first {
			first = identity.SentUnix
		}
		if !any || identity.SentUnix > last {
			last = identity.SentUnix
		}
		any = true
	}
	local := map[string][]int64{}
	if any {
		rows, err := s.database.QueryContext(ctx, `
			SELECT m.ROWID, LOWER(sender.address), COALESCE(m.date_sent, 0), subject.subject
			FROM messages m
			JOIN subjects subject ON subject.ROWID = m.subject
			JOIN addresses sender ON sender.ROWID = m.sender
			WHERE m.deleted = 0 AND m.date_sent BETWEEN ? AND ?
			  AND (m.mailbox = ? OR m.ROWID IN (SELECT message_id FROM labels WHERE mailbox_id = ?))
		`, first, last, mailboxRowID, mailboxRowID)
		if err != nil {
			return nil, fmt.Errorf("read local header identities: %w", err)
		}
		for rows.Next() {
			var rowID int64
			var address, subject string
			var sent int64
			if err := rows.Scan(&rowID, &address, &sent, &subject); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan local header identity: %w", err)
			}
			key := headerIdentityKey(mail.HeaderIdentity{Address: address, SentUnix: sent, Subject: mail.NormalizeIdentitySubject(subject)})
			local[key] = append(local[key], rowID)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	var missing []transport.RecentMessage
	localIDs := map[int64]string{}
	for index, message := range messages {
		if !known[index] {
			missing = append(missing, message)
			continue
		}
		candidates := local[headerIdentityKey(identities[index])]
		if len(candidates) == 0 {
			missing = append(missing, message)
			continue
		}
		serverID, err := messageIDFromSource(bytes.NewReader(message.Header))
		if err != nil || serverID == "" {
			return nil, fmt.Errorf("cannot compare label mailbox message without a valid Message-ID: %w", errors.Join(err, operationError("invalid_message_source", "server Message-ID is missing")))
		}
		matches := 0
		for _, rowID := range candidates {
			localID, cached := localIDs[rowID]
			if !cached {
				localID, err = s.localRFCMessageID(ctx, rowID, accountID, mailboxPath)
				if err != nil {
					return nil, fmt.Errorf("read local label candidate Message-ID: %w", err)
				}
				localIDs[rowID] = localID
			}
			if localID == "" {
				return nil, operationError("invalid_message_source", "local label candidate has no Message-ID; let Mail.app sync and retry")
			}
			if localID == serverID {
				matches++
			}
		}
		if matches > 1 {
			return nil, operationError("invalid_message_source", "multiple local label candidates share one Message-ID")
		}
		if matches == 0 {
			missing = append(missing, message)
		}
	}
	return missing, nil
}

// localServerUIDs returns UIDs held by non-deleted physical or label members.
// Callers must verify the applicable mailbox generation before comparing UIDs.
func (s *Store) localServerUIDs(ctx context.Context, mailboxRowID int64, uids []uint32) (map[uint32]bool, error) {
	known := make(map[uint32]bool, len(uids))
	if len(uids) == 0 {
		return known, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(uids)), ",")
	arguments := make([]any, 0, len(uids)+2)
	arguments = append(arguments, mailboxRowID, mailboxRowID)
	for _, uid := range uids {
		arguments = append(arguments, int64(uid))
	}
	rows, err := s.database.QueryContext(ctx,
		"SELECT remote_id FROM messages WHERE deleted = 0 AND (mailbox = ? OR ROWID IN (SELECT message_id FROM labels WHERE mailbox_id = ?)) AND remote_id IN ("+placeholders+")", arguments...)
	if err != nil {
		return nil, fmt.Errorf("read local server UIDs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var uid int64
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("scan local server UID: %w", err)
		}
		known[uint32(uid)] = true
	}
	return known, rows.Err()
}

func newMessagesFailureCode(ctx context.Context, err error) string {
	contextErr := ctx.Err()
	if contextErr == context.Canceled || contextErr == nil && errors.Is(err, context.Canceled) {
		return "operation_canceled"
	}
	if contextErr == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) {
		return "operation_timeout"
	}
	var typed interface{ ErrorCode() string }
	if errors.As(err, &typed) && typed.ErrorCode() != "" {
		return typed.ErrorCode()
	}
	if code := transport.ErrorCode(err); code != "" {
		return code
	}
	return "operation_failed"
}
