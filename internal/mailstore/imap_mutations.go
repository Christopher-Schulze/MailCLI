package mailstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

const stalenessExplanation = "changes applied to IMAP server; local read store updates on next Mail.app sync"

type imapTarget struct {
	cfg         transport.ImapConfig
	imapMailbox string
	uid         uint32
	uidvalidity uint32
	messageID   string
	accountID   string
	summary     mail.MessageSummary
}

var (
	mailboxCacheMu  sync.Mutex
	cachedMailboxes = make(map[string][]transport.MailboxInfo) // key: email
)

func (c *Client) resolveImapTarget(ctx context.Context, messageRef string) (imapTarget, error) {
	var target imapTarget
	if c.store == nil {
		return target, c.safeWriteUnavailableError()
	}

	resolved, err := c.store.resolveMessage(ctx, messageRef)
	if err != nil {
		return target, err
	}

	target.accountID = resolved.Reference.AccountID
	target.messageID = resolved.Reference.ExpectedMessageID
	if target.messageID == "" {
		if _, source, err := c.store.openMessageSource(ctx, messageRef); err == nil {
			if id, err := messageIDFromSource(source.Reader()); err == nil {
				target.messageID = id
			}
			_ = source.Close()
		}
	}
	if resolved.PhysicalLocation.Scheme != "imap" || resolved.Reference.AccountID == "local" {
		return target, &transport.TransportError{
			Code:    transport.CodeLocalOnlyMailbox,
			Message: "mutations are not supported on local-only mailboxes (On My Mac)",
		}
	}

	// Resolve email address for this AccountID
	email, err := c.resolveAccountEmail(ctx, resolved.Reference.AccountID)
	if err != nil {
		return target, &transport.TransportError{
			Code:    transport.CodeLocalOnlyMailbox,
			Message: err.Error(),
		}
	}

	_, _, imapHost, imapPort, err := transport.ProviderHosts(email)
	if err != nil {
		return target, err
	}

	credStore := c.send.Credentials
	if credStore == nil {
		return target, &transport.TransportError{
			Code:    transport.CodeSMTPCredentialsMissing,
			Message: "credential store is not available",
		}
	}

	password, err := credStore.Load(email)
	if err != nil || password == "" {
		return target, &transport.TransportError{
			Code:    transport.CodeSMTPCredentialsMissing,
			Message: fmt.Sprintf("no stored credentials for %s (run 'mailcli send setup --from %s')", email, email),
		}
	}

	cfg := transport.ImapConfig{
		Host:     imapHost,
		Port:     imapPort,
		Username: email,
		Password: password,
	}
	target.cfg = cfg

	imapOp := c.send.ImapClient()
	if imapOp == nil {
		return target, &transport.TransportError{
			Code:    transport.CodeIMAPMutationFailed,
			Message: "IMAP operator is not configured",
		}
	}

	// Discover and cache mailboxes
	boxes, err := c.getOrLoadMailboxes(ctx, imapOp, cfg, email)
	if err != nil {
		return target, err
	}

	imapBox := mapPathToIMAP(boxes, resolved.Reference.MailboxPath)
	if imapBox == "" {
		imapBox = strings.Join(resolved.Reference.MailboxPath, "/")
	}
	target.imapMailbox = imapBox

	if target.messageID != "" {
		uid, uidval, err := imapOp.SearchUID(ctx, cfg, imapBox, target.messageID)
		if err != nil {
			return target, err
		}
		target.uid = uid
		target.uidvalidity = uidval
	}

	// Build base summary
	mailboxRef, _ := mailref.EncodeMailbox(resolved.Reference.AccountID, resolved.Reference.MailboxPath)
	if s, err := mapMessageSummary(resolved.Record, mailboxRef, resolved.Reference.AccountID, resolved.Reference.MailboxPath, c.store.storeUUID); err == nil {
		s.MessageID = target.messageID
		target.summary = s
	}

	return target, nil
}

// resolveAccountEmail returns the stored-credential-backed sender address for
// an IMAP account. Account identities come from the store's own account
// catalog; the fallback gateway is only consulted when the store cannot prove
// the account (it must still exist there as an active IMAP account).
func (c *Client) resolveAccountEmail(ctx context.Context, accountID string) (string, error) {
	accounts, err := c.ListAccounts(ctx)
	if err != nil {
		accounts = nil
		if c.fallback != nil {
			if fallbackAccounts, ferr := c.fallback.ListAccounts(ctx); ferr == nil {
				accounts = fallbackAccounts
			}
		}
	}
	if c.send.Credentials == nil {
		return "", fmt.Errorf("no credential store configured; run 'mailcli send setup --from ADDRESS' first")
	}
	for _, acct := range accounts {
		acctRef, _ := mailref.DecodeAccount(acct.Ref)
		if acctRef.AccountID != accountID {
			continue
		}
		for _, address := range acct.EmailAddresses {
			if pw, lerr := c.send.Credentials.Load(address); lerr == nil && pw != "" {
				return address, nil
			}
		}
		return "", fmt.Errorf(
			"account %s has no address with stored credentials; run 'mailcli send setup --from ADDRESS' for one of %v",
			accountID, acct.EmailAddresses,
		)
	}
	return "", fmt.Errorf("no active IMAP sender identity found for account %s", accountID)
}

func (c *Client) getOrLoadMailboxes(ctx context.Context, op transport.ImapOperator, cfg transport.ImapConfig, email string) ([]transport.MailboxInfo, error) {
	mailboxCacheMu.Lock()
	cached, ok := cachedMailboxes[email]
	mailboxCacheMu.Unlock()
	if ok && len(cached) > 0 {
		return cached, nil
	}

	boxes, err := op.ListMailboxes(ctx, cfg)
	if err != nil {
		return nil, err
	}

	mailboxCacheMu.Lock()
	cachedMailboxes[email] = boxes
	mailboxCacheMu.Unlock()
	return boxes, nil
}

func mapPathToIMAP(boxes []transport.MailboxInfo, path []string) string {
	if len(path) == 0 {
		return "INBOX"
	}
	if len(path) == 1 {
		p0 := path[0]
		if strings.EqualFold(p0, "INBOX") {
			return "INBOX"
		}
		// Match by special-use flags
		specialFlag := ""
		switch strings.ToLower(p0) {
		case "sent messages", "sent", "gesendet", "gesendete elemente":
			specialFlag = "\\Sent"
		case "trash", "papierkorb", "deleted messages", "gelöschte elemente":
			specialFlag = "\\Trash"
		case "junk", "spam":
			specialFlag = "\\Junk"
		case "drafts", "entwürfe":
			specialFlag = "\\Drafts"
		case "archive", "archiv":
			specialFlag = "\\Archive"
		}
		if specialFlag != "" {
			for _, m := range boxes {
				for _, f := range m.Flags {
					if strings.EqualFold(f, specialFlag) {
						return m.Name
					}
				}
			}
		}
		// Name matches
		for _, m := range boxes {
			if strings.EqualFold(m.Name, p0) {
				return m.Name
			}
		}
		for _, m := range boxes {
			// Strip prefix e.g. "[Gmail]/Sent Mail" -> "Sent Mail"
			if idx := strings.LastIndex(m.Name, "/"); idx != -1 {
				if strings.EqualFold(m.Name[idx+1:], p0) {
					return m.Name
				}
			}
		}
	}

	// Multiple path segments
	joinedSlash := strings.Join(path, "/")
	for _, m := range boxes {
		if strings.EqualFold(m.Name, joinedSlash) {
			return m.Name
		}
	}
	joinedDot := strings.Join(path, ".")
	for _, m := range boxes {
		if strings.EqualFold(m.Name, joinedDot) {
			return m.Name
		}
	}

	return strings.Join(path, "/")
}

// MarkMessage updates read, flagged, and junk status over IMAP.
func (c *Client) MarkMessage(ctx context.Context, request mail.MarkMessageRequest) (mail.MessageSummary, error) {
	if c.store == nil {
		return mail.MessageSummary{}, c.safeWriteUnavailableError()
	}
	if err := c.rejectUnconfirmedDraftMutation(ctx, request.Ref, request.AllowDraftMutation); err != nil {
		return mail.MessageSummary{}, err
	}

	target, err := c.resolveImapTarget(ctx, request.Ref)
	if err != nil {
		return mail.MessageSummary{}, err
	}

	imapOp := c.send.ImapClient()
	if imapOp == nil {
		return mail.MessageSummary{}, &transport.TransportError{
			Code:    transport.CodeIMAPMutationFailed,
			Message: "IMAP operator is not configured",
		}
	}

	var addFlags []string
	var removeFlags []string

	if request.Read != nil {
		if *request.Read {
			addFlags = append(addFlags, "\\Seen")
		} else {
			removeFlags = append(removeFlags, "\\Seen")
		}
	}
	if request.Flagged != nil {
		if *request.Flagged {
			addFlags = append(addFlags, "\\Flagged")
		} else {
			removeFlags = append(removeFlags, "\\Flagged")
		}
	}
	if request.Junk != nil {
		if *request.Junk {
			addFlags = append(addFlags, "$Junk", "Junk")
		} else {
			removeFlags = append(removeFlags, "$Junk", "Junk", "$NotJunk")
		}
	}

	ev, err := imapOp.SetFlags(ctx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity, addFlags, removeFlags)
	if isUIDValidityChangedError(err) {
		retried, retryErr := c.resolveImapTarget(ctx, request.Ref)
		if retryErr != nil {
			return mail.MessageSummary{}, retryErr
		}
		ev, err = imapOp.SetFlags(ctx, retried.cfg, retried.imapMailbox, retried.uid, retried.uidvalidity, addFlags, removeFlags)
	}
	if err != nil {
		return mail.MessageSummary{}, err
	}

	summary := target.summary
	if request.Read != nil {
		summary.Read = *request.Read
	}
	if request.Flagged != nil {
		summary.Flagged = *request.Flagged
	}
	if request.Junk != nil {
		summary.Junk = *request.Junk
	}
	summary.ServerTruth = &mail.ServerMutationEvidence{
		Command:             ev.Command,
		ServerResponse:      ev.ServerResponse,
		Mailbox:             ev.Mailbox,
		UID:                 ev.UID,
		ExpectedUIDValidity: ev.ExpectedUIDValidity,
		UIDValidity:         ev.UIDValidity,
	}
	summary.StalenessNote = stalenessExplanation
	return summary, nil
}

// TransferMessage moves or copies a message to another mailbox over IMAP.
func (c *Client) TransferMessage(ctx context.Context, request mail.TransferMessageRequest) (mail.MessageSummary, error) {
	if c.store == nil {
		return mail.MessageSummary{}, c.safeWriteUnavailableError()
	}
	if !request.Copy {
		if err := c.rejectUnconfirmedDraftMutation(ctx, request.Ref, request.AllowDraftMutation); err != nil {
			return mail.MessageSummary{}, err
		}
	}

	target, err := c.resolveImapTarget(ctx, request.Ref)
	if err != nil {
		return mail.MessageSummary{}, err
	}

	dstRef, err := mailref.DecodeMailbox(request.DestinationMailbox)
	if err != nil {
		return mail.MessageSummary{}, operationError("invalid_reference", "invalid destination mailbox ref: "+err.Error())
	}
	if dstRef.AccountID != target.accountID {
		return mail.MessageSummary{}, &transport.TransportError{
			Code:    transport.CodeIMAPMutationFailed,
			Message: "cross-account IMAP moves/copies are not supported directly",
		}
	}

	imapOp := c.send.ImapClient()
	if imapOp == nil {
		return mail.MessageSummary{}, &transport.TransportError{
			Code:    transport.CodeIMAPMutationFailed,
			Message: "IMAP operator is not configured",
		}
	}

	boxes, err := c.getOrLoadMailboxes(ctx, imapOp, target.cfg, target.cfg.Username)
	if err != nil {
		return mail.MessageSummary{}, err
	}
	dstImapBox := mapPathToIMAP(boxes, dstRef.Path)

	var ev transport.MutationEvidence
	if request.Copy {
		ev, err = imapOp.CopyMessage(ctx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity, dstImapBox)
	} else {
		ev, err = imapOp.MoveMessage(ctx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity, dstImapBox)
	}
	if isUIDValidityChangedError(err) {
		retried, retryErr := c.resolveImapTarget(ctx, request.Ref)
		if retryErr != nil {
			return mail.MessageSummary{}, retryErr
		}
		if request.Copy {
			ev, err = imapOp.CopyMessage(ctx, retried.cfg, retried.imapMailbox, retried.uid, retried.uidvalidity, dstImapBox)
		} else {
			ev, err = imapOp.MoveMessage(ctx, retried.cfg, retried.imapMailbox, retried.uid, retried.uidvalidity, dstImapBox)
		}
	}
	if err != nil {
		return mail.MessageSummary{}, err
	}

	summary := target.summary
	if !request.Copy {
		summary.MailboxRef = request.DestinationMailbox
	}
	summary.ServerTruth = &mail.ServerMutationEvidence{
		Command:             ev.Command,
		ServerResponse:      ev.ServerResponse,
		Mailbox:             ev.Mailbox,
		TargetMailbox:       ev.TargetMailbox,
		UID:                 ev.UID,
		ExpectedUIDValidity: ev.ExpectedUIDValidity,
		UIDValidity:         ev.UIDValidity,
	}
	summary.StalenessNote = stalenessExplanation
	return summary, nil
}

// DeleteMessage deletes a message over IMAP by moving it to the Trash mailbox.
func (c *Client) DeleteMessage(ctx context.Context, request mail.DeleteMessageRequest) (mail.DeleteResult, error) {
	if c.store == nil {
		return mail.DeleteResult{}, c.safeWriteUnavailableError()
	}
	if err := c.rejectUnconfirmedDraftMutation(ctx, request.Ref, request.AllowDraftMutation); err != nil {
		return mail.DeleteResult{}, err
	}

	target, err := c.resolveImapTarget(ctx, request.Ref)
	if err != nil {
		return mail.DeleteResult{}, err
	}

	imapOp := c.send.ImapClient()
	if imapOp == nil {
		return mail.DeleteResult{}, &transport.TransportError{
			Code:    transport.CodeIMAPMutationFailed,
			Message: "IMAP operator is not configured",
		}
	}

	ev, err := imapOp.DeleteMessage(ctx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity)
	if isUIDValidityChangedError(err) {
		retried, retryErr := c.resolveImapTarget(ctx, request.Ref)
		if retryErr != nil {
			return mail.DeleteResult{}, retryErr
		}
		ev, err = imapOp.DeleteMessage(ctx, retried.cfg, retried.imapMailbox, retried.uid, retried.uidvalidity)
	}
	if err != nil {
		return mail.DeleteResult{}, err
	}
	return mail.DeleteResult{
		MessageRef: request.Ref, Deleted: true,
		ServerTruth: &mail.ServerMutationEvidence{
			Command: ev.Command, ServerResponse: ev.ServerResponse,
			Mailbox: ev.Mailbox, TargetMailbox: ev.TargetMailbox, UID: ev.UID,
			ExpectedUIDValidity: ev.ExpectedUIDValidity, UIDValidity: ev.UIDValidity,
		},
	}, nil
}

// hydrateMessage resolves the IMAP target and fetches the complete raw RFC
// 5322 source. It also returns the summary derived from the local store
// record, so the hydration fallback can fill metadata the raw message alone
// cannot provide (ref, subject, sender, dates, flags, mailbox).
func (c *Client) hydrateMessage(ctx context.Context, messageRef string) ([]byte, mail.MessageSummary, error) {
	target, err := c.resolveImapTarget(ctx, messageRef)
	if err != nil {
		return nil, mail.MessageSummary{}, err
	}

	imapOp := c.send.ImapClient()
	if imapOp == nil {
		return nil, mail.MessageSummary{}, &transport.TransportError{
			Code:    transport.CodeIMAPFetchFailed,
			Message: "IMAP operator is not configured",
		}
	}

	raw, err := imapOp.FetchMessage(ctx, target.cfg, target.imapMailbox, target.uid)
	return raw, target.summary, err
}

// HydrateMessageBytes fetches the complete raw RFC 5322 source of a message over IMAP.
func (c *Client) HydrateMessageBytes(ctx context.Context, messageRef string) ([]byte, error) {
	raw, _, err := c.hydrateMessage(ctx, messageRef)
	return raw, err
}

// SyncCheck inspects server-vs-local message counts across mailboxes without launching Mail.app.
func (c *Client) SyncCheck(ctx context.Context, accountRef string) (mail.SyncCheckResult, error) {
	var result mail.SyncCheckResult
	result.AccountRef = accountRef
	if c.store == nil {
		return result, c.safeWriteUnavailableError()
	}

	accounts, err := c.store.ListAccounts(ctx)
	if err != nil {
		return result, err
	}

	imapOp := c.send.ImapClient()
	if imapOp == nil {
		return result, &transport.TransportError{
			Code:    transport.CodeIMAPMutationFailed,
			Message: "IMAP operator is not configured",
		}
	}

	var targetAccounts []mail.Account
	if accountRef != "" {
		for _, acct := range accounts {
			if acct.Ref == accountRef {
				targetAccounts = append(targetAccounts, acct)
				break
			}
		}
		if len(targetAccounts) == 0 {
			return result, operationError("account_not_found", "account ref not found: "+accountRef)
		}
	} else {
		targetAccounts = accounts
	}

	credStore := c.send.Credentials
	for _, acct := range targetAccounts {
		if len(acct.EmailAddresses) == 0 {
			continue
		}
		email := acct.EmailAddresses[0]
		_, _, imapHost, imapPort, err := transport.ProviderHosts(email)
		if err != nil {
			continue
		}
		if credStore == nil {
			continue
		}
		password, err := credStore.Load(email)
		if err != nil || password == "" {
			continue
		}
		cfg := transport.ImapConfig{
			Host:     imapHost,
			Port:     imapPort,
			Username: email,
			Password: password,
		}

		localBoxes, err := c.store.ListMailboxes(ctx, mail.ListMailboxesRequest{AccountRef: acct.Ref})
		if err != nil {
			continue
		}

		serverBoxes, err := c.getOrLoadMailboxes(ctx, imapOp, cfg, email)
		if err != nil {
			continue
		}

		for _, lb := range localBoxes {
			imapName := mapPathToIMAP(serverBoxes, lb.Path)
			st, err := imapOp.CheckStatus(ctx, cfg, imapName)
			if err != nil {
				continue
			}
			delta := st.Messages - lb.MessageCount
			result.Mailboxes = append(result.Mailboxes, mail.MailboxDelta{
				MailboxRef:     lb.Ref,
				AccountRef:     acct.Ref,
				Name:           lb.Name,
				Path:           lb.Path,
				LocalMessages:  lb.MessageCount,
				ServerMessages: st.Messages,
				Delta:          delta,
				Unseen:         st.Unseen,
			})
		}
	}

	return result, nil
}

// isUIDValidityChangedError reports whether err is the typed mailbox rebuild
// error returned by checkUIDValidity in the transport layer.
func isUIDValidityChangedError(err error) bool {
	var typed *transport.TransportError
	return errors.As(err, &typed) && typed.Code == "mailbox_uidvalidity_changed"
}
