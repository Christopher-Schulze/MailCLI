package mailstore

import (
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
	email, cfg, err := imapConfigForAccount(account, c.send.Credentials, bindings)
	if err != nil {
		fail("", err)
		return
	}
	mailboxes, err := c.store.selectedSearchMailboxes(ctx, selector, account.Ref)
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
	if recent.UIDValidity == 0 {
		compared.State, compared.Reason = mail.NewMailboxStateUnresolved, "the server did not report UIDVALIDITY"
		return compared, nil
	}
	localValidity, err := c.store.mailboxUIDValidity(ctx, record.Location)
	if err == nil && localValidity != 0 && localValidity != recent.UIDValidity {
		compared.State = mail.NewMailboxStateUIDValidityChange
		compared.Reason = fmt.Sprintf("local UIDVALIDITY %d differs from the server's %d", localValidity, recent.UIDValidity)
		return compared, nil
	}
	uids := make([]uint32, len(recent.Messages))
	for index, message := range recent.Messages {
		uids[index] = message.UID
	}
	local, err := c.store.localServerUIDs(ctx, record.RowID, uids)
	if err != nil {
		return compared, err
	}
	var missing []transport.RecentMessage
	for _, message := range recent.Messages {
		if !local[message.UID] {
			missing = append(missing, message)
		}
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

// localServerUIDs returns which of the server UIDs the local mailbox row
// already holds.
func (s *Store) localServerUIDs(ctx context.Context, mailboxRowID int64, uids []uint32) (map[uint32]bool, error) {
	known := make(map[uint32]bool, len(uids))
	if len(uids) == 0 {
		return known, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(uids)), ",")
	arguments := make([]any, 0, len(uids)+1)
	arguments = append(arguments, mailboxRowID)
	for _, uid := range uids {
		arguments = append(arguments, int64(uid))
	}
	rows, err := s.database.QueryContext(ctx,
		"SELECT remote_id FROM messages WHERE mailbox = ? AND remote_id IN ("+placeholders+")", arguments...)
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
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
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
