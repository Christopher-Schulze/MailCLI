package mailstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

const stalenessExplanation = "changes applied to IMAP server; local read store updates on next Mail.app sync"

const (
	accountReferenceInvalidCode            = "account_reference_invalid"
	accountReferenceCorruptCode            = "account_reference_corrupt"
	accountReferenceVersionUnsupportedCode = "account_reference_version_unsupported"
	accountDisabledCode                    = "account_disabled"
	accountIdentityMissingCode             = "account_identity_missing"
	accountBindingStaleCode                = "account_binding_stale"
)

type imapTarget struct {
	cfg              transport.ImapConfig
	imapMailbox      string
	trashMailbox     string
	uid              uint32
	uidvalidity      uint32
	duplicateMatches int
	messageID        string
	accountID        string
	summary          mail.MessageSummary
}

const mailboxCacheTTL = 5 * time.Minute

const (
	syncCheckMissingLocalMailboxCode      = "sync_check_missing_local_mailbox"
	syncCheckServerCatalogIncompleteCode  = "sync_check_server_catalog_incomplete"
	syncCheckLocalMessagesUnavailableCode = "sync_check_local_messages_unavailable"
)

type syncStatusJobKind uint8

const (
	syncStatusLocal syncStatusJobKind = iota
	syncStatusServerOnly
)

type syncStatusJob struct {
	kind       syncStatusJobKind
	mailbox    string
	local      mail.Mailbox
	server     transport.MailboxInfo
	serverName string
}

type syncStatusResult struct {
	status transport.MailboxStatus
	err    error
}

type syncStatusPlanItem struct {
	statusJobIndex int
	delta          mail.MailboxDelta
	failures       []mail.SyncCheckFailure
}

type mailboxCacheEntry struct {
	boxes     []transport.MailboxInfo
	expiresAt time.Time
}

func (c *Client) resolveImapTarget(ctx context.Context, messageRef string) (imapTarget, error) {
	return c.resolveImapTargetWithOptions(ctx, messageRef, false, true)
}

func (c *Client) resolveImapTargetForMutation(ctx context.Context, messageRef string) (imapTarget, error) {
	return c.resolveImapTargetWithOptions(ctx, messageRef, false, true)
}

func (c *Client) resolveImapTargetForDelete(ctx context.Context, messageRef string) (imapTarget, error) {
	return c.resolveImapTargetWithOptions(ctx, messageRef, true, true)
}

func (c *Client) resolveImapTargetWithOptions(
	ctx context.Context,
	messageRef string,
	rejectTrash bool,
	rejectDuplicate bool,
) (imapTarget, error) {
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
	var localIdentityErr error
	imapOp := c.send.ImapClient()
	if resolved.Reference.ExpectedIMAPUID != 0 {
		uid, uidval, usable, directErr := c.directIMAPIdentity(ctx, resolved)
		if directErr != nil {
			var stale *Error
			if errors.As(directErr, &stale) && stale.Code == "stale_reference" {
				return target, directErr
			}
			localIdentityErr = directErr
		} else if usable {
			target.uid, target.uidvalidity, target.duplicateMatches = uid, uidval, 1
		}
	}
	if target.messageID == "" && target.uid == 0 {
		target.messageID, localIdentityErr = c.readLocalMessageID(ctx, messageRef)
		if localIdentityErr != nil && !safeTargetedFallback(localIdentityErr) {
			return target, fmt.Errorf("resolve IMAP message identity: %w", localIdentityErr)
		}
	}
	if resolved.PhysicalLocation.Scheme != "imap" || resolved.Reference.AccountID == "local" {
		return target, &transport.TransportError{
			Code:    transport.CodeLocalOnlyMailbox,
			Message: "mutations are not supported on local-only mailboxes (On My Mac)",
		}
	}
	if target.uid == 0 && target.messageID == "" {
		if imapOp == nil {
			return target, identityResolutionError(messageRef, localIdentityErr, &transport.TransportError{
				Code:    transport.CodeIMAPMessageUIDUnknown,
				Message: "IMAP identity discovery is unavailable; refresh the local catalog or provide a fresh Message-ID-backed reference",
			})
		}
		if _, supported := imapOp.(transport.MessageIdentityResolver); !supported {
			return target, identityResolutionError(messageRef, localIdentityErr, &transport.TransportError{
				Code:    transport.CodeIMAPMessageUIDUnknown,
				Message: "message has no Message-ID or independent server UID mapping, and this IMAP transport has no bounded metadata resolver; refresh the local catalog",
			})
		}
	}

	// Resolve email address for this AccountID without consulting the
	// Apple Events gateway. Mutations are IMAP-only.
	email, credential, err := c.resolveAccountIdentity(ctx, resolved.Reference.AccountID)
	if err != nil {
		var typed interface{ ErrorCode() string }
		if errors.As(err, &typed) {
			return target, err
		}
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

	password, err := credStore.Load(credential)
	if err != nil || password == "" {
		return target, &transport.TransportError{
			Code:    transport.CodeSMTPCredentialsMissing,
			Message: fmt.Sprintf("no stored credentials for %s (run '%s')", credential, credentialSetupCommand(email, credential)),
		}
	}

	cfg := transport.ImapConfig{
		Host:     imapHost,
		Port:     imapPort,
		Username: credential,
		Password: password,
	}
	target.cfg = cfg

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
	if rejectTrash {
		target.trashMailbox, err = transport.ResolveTrashMailbox(boxes)
		if err != nil {
			return target, err
		}
	}

	imapBox, err := mapPathToIMAP(boxes, resolved.Reference.MailboxPath)
	if err != nil {
		return target, err
	}
	target.imapMailbox = imapBox
	if rejectTrash {
		if err := rejectAlreadyTrashed(target); err != nil {
			return target, err
		}
	}

	if target.uid == 0 && target.messageID == "" {
		resolver, supported := imapOp.(transport.MessageIdentityResolver)
		if supported {
			identity, err := resolver.ResolveMessageIdentity(ctx, cfg, imapBox, transport.MessageIdentityHint{
				Subject: resolved.Record.Subject, SenderAddress: resolved.Record.SenderAddress,
			})
			if err != nil {
				return target, identityResolutionError(messageRef, localIdentityErr, err)
			}
			if identity.UID == 0 || identity.UIDValidity == 0 {
				return target, identityResolutionError(messageRef, localIdentityErr, &transport.TransportError{
					Code:    transport.CodeIMAPMessageUIDUnknown,
					Message: "server metadata resolver returned an incomplete UID identity",
				})
			}
			target.uid, target.uidvalidity, target.duplicateMatches = identity.UID, identity.UIDValidity, 1
			target.messageID = identity.MessageID
		}
	}
	if target.messageID != "" && target.uid == 0 {
		uid, uidval, matchCount, err := imapOp.SearchUID(ctx, cfg, imapBox, target.messageID)
		if err != nil {
			return target, err
		}
		target.uid = uid
		target.uidvalidity = uidval
		target.duplicateMatches = matchCount
		if err := validateResolvedUID(uid, target.messageID, imapBox); err != nil {
			return target, err
		}
		if rejectDuplicate && matchCount > 1 {
			return target, &transport.TransportError{
				Code: transport.CodeIMAPAmbiguousMessageID,
				Message: fmt.Sprintf(
					"message ID %s matched %d messages in mailbox %s; refusing mutation because the target is ambiguous; resolve the duplicate messages and rerun the command",
					target.messageID, matchCount, imapBox,
				),
			}
		}
	}
	if target.uid == 0 || target.uidvalidity == 0 {
		return target, identityResolutionError(messageRef, localIdentityErr, &transport.TransportError{
			Code:    transport.CodeIMAPMessageUIDUnknown,
			Message: "no independently verified IMAP UID and UIDVALIDITY are available; refresh the local Mail catalog or provide a fresh Message-ID-backed reference",
		})
	}

	// Build base summary
	mailboxRef, _ := mailref.EncodeMailbox(resolved.Reference.AccountID, resolved.Reference.MailboxPath)
	if s, err := mapMessageSummary(resolved.Record, mailboxRef, resolved.Reference.AccountID, resolved.Reference.MailboxPath, c.store.storeUUID); err == nil {
		s.MessageID = target.messageID
		s.Ref = updateSummaryIdentity(s.Ref, target.messageID, target.uid, target.uidvalidity)
		target.summary = s
	}

	return target, nil
}

func (c *Client) readLocalMessageID(ctx context.Context, messageRef string) (string, error) {
	_, source, err := c.store.openMessageSource(ctx, messageRef)
	if err != nil {
		return "", fmt.Errorf("open local message source: %w", err)
	}
	messageID, readErr := messageIDFromSource(source.Reader())
	closeErr := source.Close()
	if readErr != nil {
		return "", fmt.Errorf("read IMAP message identity: %w", readErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close IMAP message source: %w", closeErr)
	}
	return messageID, nil
}

func (c *Client) directIMAPIdentity(
	ctx context.Context,
	resolved resolvedMessage,
) (uint32, uint32, bool, error) {
	ref := resolved.Reference
	if ref.ExpectedIMAPUID == 0 {
		return 0, 0, false, nil
	}
	if resolved.Record.RemoteID != int64(ref.ExpectedIMAPUID) {
		return 0, 0, false, operationError("stale_reference", "message server UID mapping changed")
	}
	if ref.ExpectedIMAPMailboxID != 0 && resolved.Record.RemoteMailboxID != ref.ExpectedIMAPMailboxID {
		return 0, 0, false, operationError("stale_reference", "message server mailbox mapping changed")
	}
	localValidity, err := c.store.mailboxUIDValidity(ctx, resolved.PhysicalLocation)
	if err != nil && ref.ExpectedIMAPUIDValidity == 0 {
		return 0, 0, false, err
	}
	if ref.ExpectedIMAPUIDValidity != 0 && localValidity != 0 &&
		ref.ExpectedIMAPUIDValidity != localValidity {
		return 0, 0, false, operationError("stale_reference", "message mailbox UIDVALIDITY changed locally")
	}
	validity := ref.ExpectedIMAPUIDValidity
	if validity == 0 {
		validity = localValidity
	}
	return ref.ExpectedIMAPUID, validity, validity != 0, nil
}

func identityResolutionError(messageRef string, localErr, remoteErr error) error {
	if remoteErr == nil {
		return localErr
	}
	var typed *transport.TransportError
	if errors.As(remoteErr, &typed) {
		message := fmt.Sprintf("cannot resolve IMAP identity for %s: %s", messageRef, typed.Message)
		if localErr != nil {
			message += "; local source evidence: " + localErr.Error()
		}
		return &transport.TransportError{Code: typed.Code, Message: message, Err: errors.Join(localErr, remoteErr)}
	}
	return &transport.TransportError{
		Code:    transport.CodeIMAPMessageUIDUnknown,
		Message: fmt.Sprintf("cannot resolve IMAP identity for %s: %v", messageRef, remoteErr),
		Err:     errors.Join(localErr, remoteErr),
	}
}

func updateSummaryIdentity(refValue, messageID string, uid, uidvalidity uint32) string {
	ref, err := mailref.DecodeMessage(refValue)
	if err != nil {
		return refValue
	}
	ref.ExpectedMessageID = messageID
	ref.ExpectedIMAPUID = uid
	ref.ExpectedIMAPUIDValidity = uidvalidity
	updated, err := mailref.EncodeMessage(ref)
	if err != nil {
		return refValue
	}
	return updated
}

func credentialSetupCommand(sender, credential string) string {
	command := "mailcli send setup --from " + sender
	if !strings.EqualFold(sender, credential) {
		command += " --credential-account " + credential
	}
	return command
}

func (c *Client) resolveAccountIdentity(ctx context.Context, accountID string) (string, string, error) {
	accounts, err := c.store.ListAccounts(ctx)
	if err != nil {
		return "", "", operationErrorWithCause(
			"account_catalog_incomplete",
			fmt.Sprintf("cannot resolve account %s from the local Mail store; run 'mailcli doctor' and retry: %v", accountID, err),
			err,
		)
	}
	var bindings mail.AccountBindingFile
	bindingStore := c.send.AccountBindings
	if bindingStore == nil && c.store != nil {
		bindingStore = c.store.accountBindings
	}
	if bindingStore != nil {
		bindings, err = bindingStore.LoadAccountBindings()
		if err != nil {
			return "", "", err
		}
	} else {
		bindings = mail.AccountBindingFile{Version: mail.AccountBindingVersion, Bindings: []mail.AccountBinding{}}
	}
	return resolveAccountIdentityFromCatalog(accounts, accountID, c.send.Credentials, bindings)
}

func resolveAccountEmailFromCatalog(
	accounts []mail.Account,
	accountID string,
	credentials transport.CredentialStore,
) (string, error) {
	sender, _, err := resolveAccountIdentityFromCatalog(accounts, accountID, credentials, mail.AccountBindingFile{
		Version: mail.AccountBindingVersion, Bindings: []mail.AccountBinding{},
	})
	return sender, err
}

func resolveAccountIdentityFromCatalog(
	accounts []mail.Account,
	accountID string,
	credentials transport.CredentialStore,
	bindings mail.AccountBindingFile,
) (string, string, error) {
	decodeFailures := make([]error, 0)
	for index, acct := range accounts {
		acctRef, err := mailref.DecodeAccount(acct.Ref)
		if err != nil {
			decodeFailures = append(decodeFailures, accountReferenceFailure(index, acct.Ref, err))
			continue
		}
		if acctRef.AccountID != accountID {
			continue
		}
		if acct.State == "disabled" {
			return "", "", operationError(
				accountDisabledCode,
				fmt.Sprintf("account %s is disabled in the local Mail catalog; enable it in Mail.app and retry", accountID),
			)
		}
		if acct.State == "degraded" {
			return "", "", operationError(
				"account_degraded",
				fmt.Sprintf("account %s is degraded (%s): %s", accountID, acct.DegradedReason, acct.DegradedRemediation),
			)
		}
		binding, bindingFound, err := mail.FindAccountBinding(bindings, accountID)
		if err != nil {
			return "", "", err
		}
		if bindingFound {
			sender := ""
			for _, candidate := range binding.SenderAliases {
				if accountCatalogContainsAddress(acct, candidate) {
					sender = candidate
					break
				}
			}
			if sender == "" {
				return "", "", operationError(
					accountBindingStaleCode,
					fmt.Sprintf("account binding %s has no sender alias present in the local account catalog", accountID),
				)
			}
			if _, _, _, _, err := transport.ProviderHosts(sender); err != nil {
				return "", "", err
			}
			if credentials == nil {
				return "", "", operationError(
					accountIdentityMissingCode,
					"no credential store configured; run 'mailcli send setup --from "+sender+" --credential-account "+binding.CredentialAccount+"' first",
				)
			}
			password, loadErr := credentials.Load(binding.CredentialAccount)
			if loadErr != nil || password == "" {
				return "", "", operationError(
					accountIdentityMissingCode,
					fmt.Sprintf("account %s has no stored credentials for %s; run 'mailcli send setup --from %s --account <account-ref>'", accountID, binding.CredentialAccount, sender),
				)
			}
			return sender, binding.CredentialAccount, nil
		}
		if len(acct.EmailAddresses) == 0 {
			return "", "", operationError(
				accountIdentityMissingCode,
				fmt.Sprintf("account %s has no provable sender identity; run 'mailcli send setup --from ADDRESS' or complete one successful send", accountID),
			)
		}
		usableAddresses := make([]string, 0, len(acct.EmailAddresses))
		var providerErr error
		for _, address := range acct.EmailAddresses {
			if _, _, _, _, err := transport.ProviderHosts(address); err != nil {
				providerErr = err
				continue
			}
			usableAddresses = append(usableAddresses, address)
		}
		if len(usableAddresses) == 0 && providerErr != nil {
			return "", "", providerErr
		}
		if credentials == nil {
			return "", "", operationError(
				accountIdentityMissingCode,
				"no credential store configured; run 'mailcli send setup --from ADDRESS' first",
			)
		}
		for _, address := range usableAddresses {
			if pw, lerr := credentials.Load(address); lerr == nil && pw != "" {
				return address, address, nil
			}
		}
		return "", "", operationError(
			accountIdentityMissingCode,
			fmt.Sprintf("account %s has no address with stored credentials; run 'mailcli send setup --from ADDRESS' for one of %v",
				accountID, acct.EmailAddresses,
			),
		)
	}
	if len(decodeFailures) > 0 {
		return "", "", accountReferenceDecodeError(accountID, decodeFailures)
	}
	if binding, found, bindingErr := mail.FindAccountBinding(bindings, accountID); bindingErr != nil {
		return "", "", bindingErr
	} else if found {
		return "", "", operationError(
			accountBindingStaleCode,
			fmt.Sprintf("account binding %s refers to an account that is no longer enabled in Mail.app", binding.AccountID),
		)
	}
	return "", "", operationError(
		accountDisabledCode,
		fmt.Sprintf("account %s is not enabled in the local Mail catalog; enable it in Mail.app and retry", accountID),
	)
}

func accountCatalogContainsAddress(account mail.Account, address string) bool {
	for _, group := range [][]string{
		account.EmailAddresses, account.DiscoveredSenderIdentities, account.ConfiguredSenderAliases,
	} {
		for _, candidate := range group {
			if strings.EqualFold(candidate, address) {
				return true
			}
		}
	}
	return false
}

func accountReferenceFailure(index int, value string, cause error) error {
	fingerprint := sha256.Sum256([]byte(value))
	return fmt.Errorf("catalog entry %d (ref sha256:%x): %w", index+1, fingerprint[:6], cause)
}

func accountReferenceDecodeError(accountID string, failures []error) error {
	allCorrupt := true
	allUnsupported := true
	for _, failure := range failures {
		var typed *mailref.AccountReferenceError
		if !errors.As(failure, &typed) {
			allCorrupt = false
			allUnsupported = false
			continue
		}
		if typed.Kind != mailref.AccountReferenceCorrupt {
			allCorrupt = false
		}
		if typed.Kind != mailref.AccountReferenceVersionUnsupported {
			allUnsupported = false
		}
	}
	code := accountReferenceInvalidCode
	if allCorrupt {
		code = accountReferenceCorruptCode
	} else if allUnsupported {
		code = accountReferenceVersionUnsupportedCode
	}
	details := make([]string, len(failures))
	for index, failure := range failures {
		details[index] = failure.Error()
	}
	return operationErrorWithCause(
		code,
		fmt.Sprintf("cannot resolve account %s because the catalog contains invalid account references: %s", accountID, strings.Join(details, "; ")),
		errors.Join(failures...),
	)
}

func (c *Client) getOrLoadMailboxes(ctx context.Context, op transport.ImapOperator, cfg transport.ImapConfig, email string) ([]transport.MailboxInfo, error) {
	key := mailboxCacheKey(cfg, email)
	now := time.Now()
	c.mailboxCacheMu.Lock()
	if entry, ok := c.mailboxCache[key]; ok && now.Before(entry.expiresAt) {
		boxes := cloneMailboxInfos(entry.boxes)
		c.mailboxCacheMu.Unlock()
		return boxes, nil
	}
	if c.mailboxCache != nil {
		delete(c.mailboxCache, key)
	}
	c.mailboxCacheMu.Unlock()

	boxes, err := op.ListMailboxes(ctx, cfg)
	if err != nil {
		return boxes, err
	}
	c.mailboxCacheMu.Lock()
	if c.mailboxCache == nil {
		c.mailboxCache = make(map[string]mailboxCacheEntry)
	}
	c.mailboxCache[key] = mailboxCacheEntry{
		boxes:     cloneMailboxInfos(boxes),
		expiresAt: time.Now().Add(mailboxCacheTTL),
	}
	c.mailboxCacheMu.Unlock()
	return boxes, nil
}

func mailboxCacheKey(cfg transport.ImapConfig, email string) string {
	// LIST results are scoped to every non-secret server/account identity input;
	// NUL separators avoid collisions between values containing punctuation.
	return fmt.Sprintf("%s\x00%d\x00%s\x00%s", cfg.Host, cfg.Port, cfg.Username, email)
}

func cloneMailboxInfos(boxes []transport.MailboxInfo) []transport.MailboxInfo {
	clone := make([]transport.MailboxInfo, len(boxes))
	for index, box := range boxes {
		clone[index] = box
		clone[index].DisplayPath = append([]string(nil), box.DisplayPath...)
		clone[index].Flags = append([]string(nil), box.Flags...)
	}
	return clone
}

func (c *Client) invalidateMailboxCache() {
	c.mailboxCacheMu.Lock()
	c.mailboxCache = nil
	c.mailboxCacheMu.Unlock()
}

func mapPathToIMAP(boxes []transport.MailboxInfo, path []string) (string, error) {
	return transport.ResolveMailboxPath(boxes, path)
}

// MarkMessage updates read, flagged, and junk status over IMAP.
func (c *Client) MarkMessage(ctx context.Context, request mail.MarkMessageRequest) (mail.MessageSummary, error) {
	if c.store == nil {
		return mail.MessageSummary{}, c.safeWriteUnavailableError()
	}
	if err := c.rejectUnconfirmedDraftMutation(ctx, request.Ref, request.AllowDraftMutation); err != nil {
		return mail.MessageSummary{}, err
	}

	target, err := c.resolveImapTargetForMutation(ctx, request.Ref)
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
			addFlags = append(addFlags, "$Junk")
			removeFlags = append(removeFlags, "$NotJunk")
		} else {
			addFlags = append(addFlags, "$NotJunk")
			removeFlags = append(removeFlags, "$Junk")
		}
	}

	ev, err := imapOp.SetFlags(ctx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity, addFlags, removeFlags)
	if transport.ErrorCode(err) == "mailbox_uidvalidity_changed" &&
		(ev.Outcome == "" || ev.Outcome == transport.MutationOutcomeNotStarted) {
		retried, retryErr := c.resolveImapTargetForMutation(ctx, request.Ref)
		if retryErr != nil {
			return mail.MessageSummary{}, retryErr
		}
		ev, err = imapOp.SetFlags(ctx, retried.cfg, retried.imapMailbox, retried.uid, retried.uidvalidity, addFlags, removeFlags)
		target = retried
	}
	if err != nil && ev.Command == "" {
		return mail.MessageSummary{}, err
	}
	identityMismatch := ev.UID != target.uid || ev.Mailbox != target.imapMailbox || ev.UIDValidity != target.uidvalidity
	if (ev.FlagsState == transport.FlagObservationObserved && identityMismatch) ||
		(err == nil && (ev.FlagsState != transport.FlagObservationObserved || ev.Outcome != transport.MutationOutcomeCompleted)) {
		ev.Outcome, ev.FlagsState, ev.ActualFlags = transport.MutationOutcomeUnknown, transport.FlagObservationUnverified, nil
		err = &transport.MutationOutcomeError{
			Code: transport.CodeIMAPFlagsOutcomeUnknown, Evidence: ev, Err: err,
			Message: "IMAP STORE returned no complete flag observation for the resolved message; inspect server state before retrying",
		}
	}
	ev.DuplicateMatches = duplicateMatchEvidence(target.duplicateMatches)

	summary := target.summary
	if ev.FlagsState == transport.FlagObservationObserved {
		summary.Read, summary.Flagged, summary.Junk, summary.Deleted = false, false, false, false
		var notJunk bool
		for _, flag := range ev.ActualFlags {
			switch strings.ToLower(flag) {
			case "\\seen":
				summary.Read = true
			case "\\flagged":
				summary.Flagged = true
			case "$junk":
				summary.Junk = true
			case "$notjunk":
				notJunk = true
			case "\\deleted":
				summary.Deleted = true
			}
		}
		// RFC 9051 treats a conflicting pair as no definite classification.
		summary.Junk = summary.Junk && !notJunk
	}
	summary.ServerTruth = &mail.ServerMutationEvidence{
		OperationID:         ev.OperationID,
		Outcome:             ev.Outcome,
		SourceAccount:       ev.SourceAccount,
		Command:             ev.Command,
		ServerResponse:      ev.ServerResponse,
		Mailbox:             ev.Mailbox,
		UID:                 ev.UID,
		ExpectedUIDValidity: ev.ExpectedUIDValidity,
		UIDValidity:         ev.UIDValidity,
		DuplicateMatches:    ev.DuplicateMatches,
		FlagsState:          string(ev.FlagsState),
		ActualFlags:         append([]string(nil), ev.ActualFlags...),
		FlagsSource:         ev.FlagsSource,
	}
	summary.StalenessNote = "flags observed on IMAP server at command completion; concurrent clients may change them; local metadata updates on the next Mail.app sync"
	if ev.FlagsState != transport.FlagObservationObserved {
		summary.StalenessNote = "server flags are not verified; summary booleans retain local cached values"
	}
	return summary, err
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

	target, err := c.resolveImapTargetForMutation(ctx, request.Ref)
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
	dstImapBox, err := mapPathToIMAP(boxes, dstRef.Path)
	if err != nil {
		return mail.MessageSummary{}, err
	}

	copyKey := ""
	if request.Copy {
		copyKey = copyAttemptKey(target, dstImapBox)
		if priorEvidence, exists := c.copyAttempt(copyKey); exists {
			observation, observeErr := observeCopyDestination(
				ctx, imapOp, target.cfg, dstImapBox, target.messageID,
			)
			if observeErr != nil {
				return mail.MessageSummary{}, copyOutcomeUnknownError(
					priorEvidence,
					fmt.Sprintf(
						"COPY operation %s remains unresolved; destination reconciliation failed",
						priorEvidence.OperationID,
					),
					nil,
					observeErr,
				)
			}
			if observation.matchCount > 0 {
				if priorEvidence.DestinationUIDValidity != 0 &&
					priorEvidence.DestinationUIDValidity != observation.uidvalidity {
					return mail.MessageSummary{}, copyOutcomeUnknownError(
						priorEvidence,
						fmt.Sprintf(
							"COPY operation %s cannot adopt destination identity after UIDVALIDITY rollover (%d -> %d)",
							priorEvidence.OperationID, priorEvidence.DestinationUIDValidity, observation.uidvalidity,
						),
						nil,
						nil,
					)
				}
				priorEvidence = evidenceWithCopyDestination(priorEvidence, observation)
			}
			return mail.MessageSummary{}, copyOutcomeUnknownError(
				priorEvidence,
				fmt.Sprintf(
					"COPY operation %s remains unresolved; reconcile mailbox %s before retrying",
					priorEvidence.OperationID, dstImapBox,
				),
				nil,
				nil,
			)
		}
		if _, err := verifyCopyDestinationBeforeDispatch(
			ctx, imapOp, target.cfg, dstImapBox, target.messageID, copyAttemptEvidence(target, dstImapBox),
		); err != nil {
			return mail.MessageSummary{}, err
		}
		c.rememberCopyAttempt(copyKey, copyAttemptEvidence(target, dstImapBox))
	}

	if !request.Copy {
		if err := verifyMoveDestination(
			ctx, imapOp, target.cfg, dstImapBox, target.messageID, nil,
		); err != nil {
			return mail.MessageSummary{}, err
		}
	}

	var ev transport.MutationEvidence
	if request.Copy {
		ev, err = imapOp.CopyMessage(ctx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity, dstImapBox)
	} else {
		ev, err = imapOp.MoveMessage(ctx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity, dstImapBox)
	}
	if isUIDValidityChangedError(err) {
		if request.Copy {
			c.forgetCopyAttempt(copyKey)
		}
		retried, retryErr := c.resolveImapTargetForMutation(ctx, request.Ref)
		if retryErr != nil {
			return mail.MessageSummary{}, retryErr
		}
		if request.Copy {
			copyKey = copyAttemptKey(retried, dstImapBox)
			if _, err := verifyCopyDestinationBeforeDispatch(
				ctx, imapOp, retried.cfg, dstImapBox, retried.messageID, copyAttemptEvidence(retried, dstImapBox),
			); err != nil {
				return mail.MessageSummary{}, err
			}
			c.rememberCopyAttempt(copyKey, copyAttemptEvidence(retried, dstImapBox))
			ev, err = imapOp.CopyMessage(ctx, retried.cfg, retried.imapMailbox, retried.uid, retried.uidvalidity, dstImapBox)
		} else {
			ev, err = imapOp.MoveMessage(ctx, retried.cfg, retried.imapMailbox, retried.uid, retried.uidvalidity, dstImapBox)
		}
		target = retried
	}
	if err != nil {
		if request.Copy {
			ev = mergeCopyEvidence(copyAttemptEvidence(target, dstImapBox), ev)
			if ev.Outcome == transport.MutationOutcomeRejected || ev.Outcome == transport.MutationOutcomeNotStarted {
				c.forgetCopyAttempt(copyKey)
				return mail.MessageSummary{}, err
			}
			if ev.Outcome == "" {
				ev.Outcome = transport.MutationOutcomeUnknown
			}
			observedEvidence, outcomeErr := verifyCopyDestinationAfterDispatch(
				ctx, imapOp, target.cfg, dstImapBox, target.messageID, ev, err,
			)
			if outcomeErr != nil {
				c.rememberCopyAttempt(copyKey, outcomeEvidenceFromError(outcomeErr, observedEvidence))
				return mail.MessageSummary{}, outcomeErr
			}
			observedEvidence.Outcome = transport.MutationOutcomeUnknown
			c.rememberCopyAttempt(copyKey, observedEvidence)
			return mail.MessageSummary{}, copyOutcomeUnknownError(
				observedEvidence,
				fmt.Sprintf(
					"COPY operation %s returned an incomplete result; reconcile the destination before retrying",
					observedEvidence.OperationID,
				),
				err,
				nil,
			)
		}
		if !request.Copy {
			if outcomeErr := verifyMoveDestination(
				ctx, imapOp, target.cfg, dstImapBox, target.messageID, err,
			); outcomeErr != nil {
				err = outcomeErr
			}
		}
		if ev.Command == "" {
			return mail.MessageSummary{}, err
		}
	}
	if request.Copy {
		var outcomeErr error
		ev = mergeCopyEvidence(copyAttemptEvidence(target, dstImapBox), ev)
		ev, outcomeErr = verifyCopyDestinationAfterDispatch(
			ctx, imapOp, target.cfg, dstImapBox, target.messageID, ev, nil,
		)
		if outcomeErr != nil {
			c.rememberCopyAttempt(copyKey, outcomeEvidenceFromError(outcomeErr, ev))
			return mail.MessageSummary{}, outcomeErr
		}
		c.forgetCopyAttempt(copyKey)
	}
	ev.DuplicateMatches = duplicateMatchEvidence(target.duplicateMatches)

	summary := target.summary
	if !request.Copy && err == nil {
		summary.MailboxRef = request.DestinationMailbox
	}
	summary.ServerTruth = &mail.ServerMutationEvidence{
		OperationID:            ev.OperationID,
		Outcome:                ev.Outcome,
		SourceAccount:          ev.SourceAccount,
		Command:                ev.Command,
		ServerResponse:         ev.ServerResponse,
		Mailbox:                ev.Mailbox,
		TargetMailbox:          ev.TargetMailbox,
		UID:                    ev.UID,
		ExpectedUIDValidity:    ev.ExpectedUIDValidity,
		UIDValidity:            ev.UIDValidity,
		DuplicateMatches:       ev.DuplicateMatches,
		ExpungeBranch:          ev.ExpungeBranch,
		ForeignDeletedCount:    ev.ForeignDeletedCount,
		DestinationUIDValidity: ev.DestinationUIDValidity,
		DestinationUID:         ev.DestinationUID,
		CopyUIDResponse:        ev.CopyUIDResponse,
		CopyUIDValidity:        ev.CopyUIDValidity,
		CopySourceUID:          ev.CopySourceUID,
		CopyDestinationUID:     ev.CopyDestinationUID,
		CompletedEffects:       append([]string(nil), ev.CompletedEffects...),
		FlagsState:             string(ev.FlagsState),
		ActualFlags:            append([]string(nil), ev.ActualFlags...),
		FlagsSource:            ev.FlagsSource,
	}
	summary.StalenessNote = stalenessExplanation
	if err != nil {
		summary.StalenessNote = "MOVE is incomplete; retained source flags and COPY effects are in server_truth; summary booleans retain local cached values"
	}
	return summary, err
}

func duplicateMatchEvidence(matchCount int) int {
	if matchCount <= 1 {
		return 0
	}
	return matchCount
}

type copyDestinationObservation struct {
	uid         uint32
	uidvalidity uint32
	matchCount  int
}

func (c *Client) copyAttempt(key string) (transport.MutationEvidence, bool) {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	evidence, ok := c.copyAttempts[key]
	return evidence, ok
}

func (c *Client) rememberCopyAttempt(key string, evidence transport.MutationEvidence) {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	if c.copyAttempts == nil {
		c.copyAttempts = make(map[string]transport.MutationEvidence)
	}
	c.copyAttempts[key] = evidence
}

func (c *Client) forgetCopyAttempt(key string) {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	delete(c.copyAttempts, key)
}

func copyAttemptKey(target imapTarget, destination string) string {
	return transport.MutationOperationID(
		"COPY", target.cfg.Username, target.imapMailbox, target.uid, target.uidvalidity, destination,
	)
}

func copyAttemptEvidence(target imapTarget, destination string) transport.MutationEvidence {
	return transport.MutationEvidence{
		OperationID:         copyAttemptKey(target, destination),
		Outcome:             transport.MutationOutcomeAttempted,
		SourceAccount:       target.cfg.Username,
		Command:             "COPY",
		Mailbox:             target.imapMailbox,
		TargetMailbox:       destination,
		UID:                 target.uid,
		UIDValidity:         target.uidvalidity,
		ExpectedUIDValidity: target.uidvalidity,
	}
}

func observeCopyDestination(
	ctx context.Context,
	imapOp transport.ImapOperator,
	cfg transport.ImapConfig,
	dstMailbox string,
	messageID string,
) (copyDestinationObservation, error) {
	if messageID == "" {
		return copyDestinationObservation{}, &transport.TransportError{
			Code:    transport.CodeIMAPCopyOutcomeUnknown,
			Message: "cannot reconcile COPY because the source Message-ID is unavailable",
		}
	}
	uid, uidvalidity, matchCount, err := imapOp.SearchUID(ctx, cfg, dstMailbox, messageID)
	if err != nil {
		if transport.ErrorCode(err) == transport.CodeIMAPMessageNotFound {
			return copyDestinationObservation{}, nil
		}
		return copyDestinationObservation{}, err
	}
	if matchCount == 0 {
		return copyDestinationObservation{}, nil
	}
	if uid == 0 || uidvalidity == 0 {
		return copyDestinationObservation{}, &transport.TransportError{
			Code: transport.CodeIMAPCopyOutcomeUnknown,
			Message: fmt.Sprintf(
				"destination %s returned an unusable COPY identity (UID %d, UIDVALIDITY %d)",
				dstMailbox, uid, uidvalidity,
			),
		}
	}
	return copyDestinationObservation{uid: uid, uidvalidity: uidvalidity, matchCount: matchCount}, nil
}

func evidenceWithCopyDestination(
	evidence transport.MutationEvidence,
	observation copyDestinationObservation,
) transport.MutationEvidence {
	evidence.DestinationUID = observation.uid
	evidence.DestinationUIDValidity = observation.uidvalidity
	return evidence
}

func mergeCopyEvidence(base, actual transport.MutationEvidence) transport.MutationEvidence {
	if actual.OperationID == "" {
		actual.OperationID = base.OperationID
	}
	if actual.Outcome == "" {
		actual.Outcome = base.Outcome
	}
	if actual.SourceAccount == "" {
		actual.SourceAccount = base.SourceAccount
	}
	if actual.Command == "" {
		actual.Command = base.Command
	}
	if actual.Mailbox == "" {
		actual.Mailbox = base.Mailbox
	}
	if actual.TargetMailbox == "" {
		actual.TargetMailbox = base.TargetMailbox
	}
	if actual.UID == 0 {
		actual.UID = base.UID
	}
	if actual.UIDValidity == 0 {
		actual.UIDValidity = base.UIDValidity
	}
	if actual.ExpectedUIDValidity == 0 {
		actual.ExpectedUIDValidity = base.ExpectedUIDValidity
	}
	return actual
}

func copyOutcomeUnknownError(
	evidence transport.MutationEvidence,
	message string,
	previousErr error,
	probeErr error,
) error {
	evidence.Outcome = transport.MutationOutcomeUnknown
	return &transport.MutationOutcomeError{
		Code:     transport.CodeIMAPCopyOutcomeUnknown,
		Message:  message,
		Evidence: evidence,
		Err:      errors.Join(previousErr, probeErr),
	}
}

func outcomeEvidenceFromError(err error, fallback transport.MutationEvidence) transport.MutationEvidence {
	var outcomeErr *transport.MutationOutcomeError
	if errors.As(err, &outcomeErr) {
		return outcomeErr.Evidence
	}
	return fallback
}

func verifyCopyDestinationBeforeDispatch(
	ctx context.Context,
	imapOp transport.ImapOperator,
	cfg transport.ImapConfig,
	dstMailbox string,
	messageID string,
	evidence transport.MutationEvidence,
) (copyDestinationObservation, error) {
	observation, err := observeCopyDestination(ctx, imapOp, cfg, dstMailbox, messageID)
	if err != nil {
		return observation, copyOutcomeUnknownError(
			evidence,
			fmt.Sprintf("cannot safely start COPY for message ID %s to mailbox %s; destination observation failed", messageID, dstMailbox),
			nil,
			err,
		)
	}
	if observation.matchCount == 0 {
		return observation, nil
	}
	observationEvidence := evidenceWithCopyDestination(evidence, observation)
	return observation, copyOutcomeUnknownError(
		observationEvidence,
		fmt.Sprintf(
			"cannot safely start or replay COPY for message ID %s to mailbox %s; destination contains %d matching message(s)",
			messageID, dstMailbox, observation.matchCount,
		),
		nil,
		nil,
	)
}

func verifyCopyDestinationAfterDispatch(
	ctx context.Context,
	imapOp transport.ImapOperator,
	cfg transport.ImapConfig,
	dstMailbox string,
	messageID string,
	evidence transport.MutationEvidence,
	previousErr error,
) (transport.MutationEvidence, error) {
	observation, err := observeCopyDestination(ctx, imapOp, cfg, dstMailbox, messageID)
	if err != nil {
		return evidence, copyOutcomeUnknownError(
			evidence,
			fmt.Sprintf(
				"COPY outcome for message ID %s to mailbox %s is unknown; destination observation failed",
				messageID, dstMailbox,
			),
			previousErr,
			err,
		)
	}
	if observation.matchCount == 0 {
		return evidence, copyOutcomeUnknownError(
			evidence,
			fmt.Sprintf(
				"COPY outcome for message ID %s to mailbox %s is unknown; destination contains no matching message",
				messageID, dstMailbox,
			),
			previousErr,
			nil,
		)
	}
	evidence = evidenceWithCopyDestination(evidence, observation)
	if observation.matchCount > 1 {
		return evidence, copyOutcomeUnknownError(
			evidence,
			fmt.Sprintf(
				"COPY outcome for message ID %s to mailbox %s is ambiguous; destination contains %d matching messages",
				messageID, dstMailbox, observation.matchCount,
			),
			previousErr,
			nil,
		)
	}
	if evidence.CopyUIDValidity != 0 && evidence.CopyUIDValidity != observation.uidvalidity {
		return evidence, copyOutcomeUnknownError(
			evidence,
			fmt.Sprintf(
				"COPY destination UIDVALIDITY changed from %d to %d while reconciling message ID %s",
				evidence.CopyUIDValidity, observation.uidvalidity, messageID,
			),
			previousErr,
			nil,
		)
	}
	if evidence.CopyDestinationUID != 0 && evidence.CopyDestinationUID != observation.uid {
		return evidence, copyOutcomeUnknownError(
			evidence,
			fmt.Sprintf(
				"COPY destination UID changed from %d to %d while reconciling message ID %s",
				evidence.CopyDestinationUID, observation.uid, messageID,
			),
			previousErr,
			nil,
		)
	}
	return evidence, nil
}

func validateResolvedUID(uid uint32, messageID, mailbox string) error {
	if uid != 0 {
		return nil
	}
	return &transport.TransportError{
		Code: transport.CodeIMAPMessageUIDUnknown,
		Message: fmt.Sprintf(
			"IMAP Message-ID search for %s in %s returned UID zero; refusing the operation",
			messageID, mailbox,
		),
	}
}

func verifyMoveDestination(
	ctx context.Context,
	imapOp transport.ImapOperator,
	cfg transport.ImapConfig,
	dstMailbox string,
	messageID string,
	previousErr error,
) error {
	if messageID == "" {
		return &transport.TransportError{
			Code:    transport.CodeIMAPMoveOutcomeUnknown,
			Message: "cannot safely execute or replay MOVE because the message identity is unavailable",
			Err:     previousErr,
		}
	}
	uid, uidvalidity, matchCount, err := imapOp.SearchUID(ctx, cfg, dstMailbox, messageID)
	if err != nil {
		if transport.ErrorCode(err) == transport.CodeIMAPMessageNotFound {
			if previousErr != nil {
				return moveOutcomeUnknownError(dstMailbox, messageID, 0, 0, 0, previousErr, nil)
			}
			return nil
		}
		return moveOutcomeUnknownError(dstMailbox, messageID, 0, 0, 0, previousErr, err)
	}
	return moveOutcomeUnknownError(
		dstMailbox, messageID, uid, uidvalidity, matchCount, previousErr, nil,
	)
}

func moveOutcomeUnknownError(
	dstMailbox string,
	messageID string,
	uid uint32,
	uidvalidity uint32,
	matchCount int,
	previousErr error,
	probeErr error,
) error {
	message := fmt.Sprintf(
		"cannot safely replay MOVE for message ID %s to mailbox %s",
		messageID, dstMailbox,
	)
	if matchCount > 0 {
		message = fmt.Sprintf(
			"%s; destination contains %d matching message(s) at UID %d with UIDVALIDITY %d",
			message, matchCount, uid, uidvalidity,
		)
	} else if probeErr != nil {
		message += "; destination verification failed"
	}
	message += "; reconcile the source and destination before retrying"
	return &transport.TransportError{
		Code:    transport.CodeIMAPMoveOutcomeUnknown,
		Message: message,
		Err:     errors.Join(previousErr, probeErr),
	}
}

func rejectAlreadyTrashed(target imapTarget) error {
	if target.trashMailbox == "" || !strings.EqualFold(target.imapMailbox, target.trashMailbox) {
		return nil
	}
	return &transport.TransportError{
		Code:    transport.CodeMessageAlreadyTrashed,
		Message: "message is already in trash; restore it in Mail or use Mail.app to empty the trash",
	}
}

// DeleteMessage deletes a message over IMAP by moving it to the Trash mailbox.
func (c *Client) DeleteMessage(ctx context.Context, request mail.DeleteMessageRequest) (mail.DeleteResult, error) {
	if c.store == nil {
		return mail.DeleteResult{}, c.safeWriteUnavailableError()
	}
	if err := c.rejectUnconfirmedDraftMutation(ctx, request.Ref, request.AllowDraftMutation); err != nil {
		return mail.DeleteResult{}, err
	}

	target, err := c.resolveImapTargetForDelete(ctx, request.Ref)
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
		retried, retryErr := c.resolveImapTargetForDelete(ctx, request.Ref)
		if retryErr != nil {
			return mail.DeleteResult{}, retryErr
		}
		if retryErr := rejectAlreadyTrashed(retried); retryErr != nil {
			return mail.DeleteResult{}, retryErr
		}
		ev, err = imapOp.DeleteMessage(ctx, retried.cfg, retried.imapMailbox, retried.uid, retried.uidvalidity)
		target.duplicateMatches = retried.duplicateMatches
	}
	if err != nil && ev.Command == "" {
		return mail.DeleteResult{}, err
	}
	ev.DuplicateMatches = duplicateMatchEvidence(target.duplicateMatches)
	return mail.DeleteResult{
		MessageRef: request.Ref, Deleted: err == nil,
		ServerTruth: &mail.ServerMutationEvidence{
			OperationID: ev.OperationID, Outcome: ev.Outcome, SourceAccount: ev.SourceAccount,
			Command: ev.Command, ServerResponse: ev.ServerResponse,
			Mailbox: ev.Mailbox, TargetMailbox: ev.TargetMailbox, UID: ev.UID,
			ExpectedUIDValidity: ev.ExpectedUIDValidity, UIDValidity: ev.UIDValidity,
			DuplicateMatches: ev.DuplicateMatches,
			ExpungeBranch:    ev.ExpungeBranch, ForeignDeletedCount: ev.ForeignDeletedCount,
			DestinationUIDValidity: ev.DestinationUIDValidity, DestinationUID: ev.DestinationUID,
			CopyUIDResponse: ev.CopyUIDResponse, CopyUIDValidity: ev.CopyUIDValidity,
			CopySourceUID: ev.CopySourceUID, CopyDestinationUID: ev.CopyDestinationUID,
			CompletedEffects: append([]string(nil), ev.CompletedEffects...),
			FlagsState:       string(ev.FlagsState),
			ActualFlags:      append([]string(nil), ev.ActualFlags...),
			FlagsSource:      ev.FlagsSource,
		},
	}, err
}

// hydrateMessage resolves the IMAP target and fetches the complete raw RFC
// 5322 source. It also returns the summary derived from the local store
// record, so the hydration fallback can fill metadata the raw message alone
// cannot provide (ref, subject, sender, dates, flags, mailbox).
func (c *Client) hydrateMessage(ctx context.Context, messageRef string, enforceRawCap bool) ([]byte, mail.MessageSummary, error) {
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

	raw, err := imapOp.FetchMessage(ctx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity, rawFetchBound(enforceRawCap))
	return raw, target.summary, err
}

// HydrateMessageBytes fetches the complete raw RFC 5322 source of a message over IMAP.
func (c *Client) HydrateMessageBytes(ctx context.Context, messageRef string, enforceRawCap bool) ([]byte, error) {
	raw, _, err := c.hydrateMessage(ctx, messageRef, enforceRawCap)
	return raw, err
}

// rawFetchBound applies the same in-memory bound to both raw-source and
// content hydration so an IMAP literal cannot bypass the local size limit.
func rawFetchBound(_ bool) int64 {
	return mail.MaximumRawSourceBytes
}

// SyncCheck inspects server-vs-local message counts across mailboxes without launching Mail.app.
func syncIdentity(account mail.Account, credentials transport.CredentialStore) (string, string, int, string, error) {
	return syncIdentityWithBindings(account, credentials, mail.AccountBindingFile{
		Version: mail.AccountBindingVersion, Bindings: []mail.AccountBinding{},
	})
}

func syncIdentityWithBindings(
	account mail.Account,
	credentials transport.CredentialStore,
	bindings mail.AccountBindingFile,
) (string, string, int, string, error) {
	candidates := append([]string(nil), account.EmailAddresses...)
	if accountRef, err := mailref.DecodeAccount(account.Ref); err == nil {
		if binding, found, bindingErr := mail.FindAccountBinding(bindings, accountRef.AccountID); bindingErr != nil {
			return "", "", 0, "", bindingErr
		} else if found {
			candidates = make([]string, 0, len(binding.SenderAliases))
			for _, candidate := range binding.SenderAliases {
				if accountCatalogContainsAddress(account, candidate) {
					candidates = append(candidates, candidate)
				}
			}
			if len(candidates) == 0 {
				return "", "", 0, "", operationError(
					accountBindingStaleCode,
					"account binding has no sender alias present in the local account catalog",
				)
			}
			if credentials == nil {
				return "", "", 0, "", operationError(
					"imap_credentials_missing",
					"no credential store configured; run '"+credentialSetupCommand(candidates[0], binding.CredentialAccount)+"'",
				)
			}
			password, loadErr := credentials.Load(binding.CredentialAccount)
			if loadErr != nil || password == "" {
				return "", "", 0, "", operationError(
					"imap_credentials_missing",
					"no stored password for "+binding.CredentialAccount+"; run 'mailcli send setup --from "+candidates[0]+" --account "+account.Ref+" --credential-account "+binding.CredentialAccount+"'",
				)
			}
			for _, candidate := range candidates {
				_, _, imapHost, imapPort, providerErr := transport.ProviderHosts(candidate)
				if providerErr == nil {
					return candidate, imapHost, imapPort, password, nil
				}
			}
			return "", "", 0, "", operationError("account_binding_stale", "account binding has no supported sender alias")
		}
	}
	if len(candidates) == 0 {
		return "", "", 0, "", operationError("account_no_email", "account has no email addresses; cannot resolve provider endpoints")
	}
	if credentials == nil {
		return "", "", 0, "", operationError("imap_credentials_missing", "no credential store configured; run 'mailcli send setup --from "+candidates[0]+"'")
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return strings.ToLower(candidates[i]) < strings.ToLower(candidates[j])
	})
	var lastErr error
	for _, candidate := range candidates {
		_, _, imapHost, imapPort, err := transport.ProviderHosts(candidate)
		if err != nil {
			lastErr = err
			continue
		}
		password, err := credentials.Load(candidate)
		if err != nil {
			lastErr = err
			continue
		}
		if password == "" {
			lastErr = operationError("imap_credentials_missing", "no stored password; run 'mailcli send setup --from "+candidate+"'")
			continue
		}
		return candidate, imapHost, imapPort, password, nil
	}
	if lastErr == nil {
		lastErr = operationError("imap_credentials_missing", "no usable account identity is configured")
	}
	return "", "", 0, "", lastErr
}

func syncStatusWorkerLimit(op transport.ImapOperator) int {
	const fallback = 1
	provider, ok := op.(transport.ImapConcurrencyProvider)
	if !ok {
		return fallback
	}
	limit := provider.MaxConnectionsPerAccount()
	if limit <= 0 {
		return fallback
	}
	return limit
}

func runSyncStatusJobs(
	ctx context.Context,
	op transport.ImapOperator,
	cfg transport.ImapConfig,
	jobs []syncStatusJob,
) []syncStatusResult {
	results := make([]syncStatusResult, len(jobs))
	workerCount := min(syncStatusWorkerLimit(op), len(jobs))
	if workerCount == 0 {
		return results
	}
	var next atomic.Int64
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for {
				index := int(next.Add(1)) - 1
				if index >= len(jobs) {
					return
				}
				if err := ctx.Err(); err != nil {
					results[index].err = err
					continue
				}
				status, err := op.CheckStatus(ctx, cfg, jobs[index].mailbox)
				results[index] = syncStatusResult{status: status, err: err}
			}
		}()
	}
	workers.Wait()
	return results
}

func appendMatchedSyncStatusResult(
	ctx context.Context,
	result *mail.SyncCheckResult,
	accountRef string,
	account string,
	job syncStatusJob,
	status syncStatusResult,
) {
	if status.err != nil {
		delta := localMailboxDelta(accountRef, job.local, mail.MailboxDeltaStateInaccessible)
		delta.ServerName = job.serverName
		result.Mailboxes = append(result.Mailboxes, delta)
		result.Failures = append(result.Failures, mail.SyncCheckFailure{
			Account: account,
			Mailbox: job.mailbox,
			Code:    failureCode(ctx, status.err),
			Message: status.err.Error(),
		})
		return
	}
	if !job.local.LocalMessagesAvailable {
		delta := localMailboxDelta(accountRef, job.local, mail.MailboxDeltaStateUnresolved)
		delta.ServerName = job.serverName
		delta.ServerMessagesAvailable = true
		delta.ServerMessages = status.status.Messages
		delta.Unseen = status.status.Unseen
		result.Mailboxes = append(result.Mailboxes, delta)
		result.Failures = append(result.Failures, mail.SyncCheckFailure{
			Account: account,
			Mailbox: job.mailbox,
			Code:    syncCheckLocalMessagesUnavailableCode,
			Message: "local mailbox message count is unavailable; count delta cannot be compared",
		})
		return
	}
	result.Mailboxes = append(result.Mailboxes, mail.MailboxDelta{
		MailboxRef:              job.local.Ref,
		AccountRef:              accountRef,
		State:                   mail.MailboxDeltaStateMatched,
		Name:                    job.local.Name,
		Path:                    append([]string(nil), job.local.Path...),
		ServerName:              job.serverName,
		LocalMessagesAvailable:  job.local.LocalMessagesAvailable,
		ServerMessagesAvailable: true,
		LocalMessages:           job.local.MessageCount,
		ServerMessages:          status.status.Messages,
		Delta:                   status.status.Messages - job.local.MessageCount,
		Unseen:                  status.status.Unseen,
	})
}

func appendServerOnlySyncStatusResult(
	ctx context.Context,
	result *mail.SyncCheckResult,
	accountRef string,
	account string,
	job syncStatusJob,
	status syncStatusResult,
) {
	serverName := syncServerMailboxWireName(job.server)
	if status.err != nil {
		delta := serverMailboxDelta(accountRef, job.server, mail.MailboxDeltaStateInaccessible)
		result.Mailboxes = append(result.Mailboxes, delta)
		result.Failures = append(result.Failures,
			mail.SyncCheckFailure{
				Account: account, Mailbox: serverName,
				Code: failureCode(ctx, status.err), Message: status.err.Error(),
			},
			mail.SyncCheckFailure{
				Account: account, Mailbox: serverName,
				Code:    syncCheckMissingLocalMailboxCode,
				Message: fmt.Sprintf("server mailbox %q has no matching local mailbox", serverName),
			},
		)
		return
	}
	path := syncServerMailboxPath(job.server)
	result.Mailboxes = append(result.Mailboxes, mail.MailboxDelta{
		MailboxRef:              syncServerMailboxRef(accountRef, path),
		AccountRef:              accountRef,
		State:                   mail.MailboxDeltaStateServerOnly,
		Name:                    syncServerMailboxName(job.server, path),
		Path:                    path,
		ServerName:              serverName,
		LocalMessagesAvailable:  false,
		ServerMessagesAvailable: true,
		ServerMessages:          status.status.Messages,
		Unseen:                  status.status.Unseen,
	})
	result.Failures = append(result.Failures, mail.SyncCheckFailure{
		Account: account,
		Mailbox: serverName,
		Code:    syncCheckMissingLocalMailboxCode,
		Message: fmt.Sprintf("server mailbox %q has no matching local mailbox", serverName),
	})
}

func (c *Client) SyncCheck(ctx context.Context, accountRef string) (mail.SyncCheckResult, error) {
	var result mail.SyncCheckResult
	result.AccountRef = accountRef
	if c.store == nil {
		return result, c.safeWriteUnavailableError()
	}

	accounts, err := c.store.ListAccounts(ctx)
	if err != nil {
		result.Failures = append(result.Failures, mail.SyncCheckFailure{
			Account: accountRef,
			Code:    failureCode(ctx, err),
			Message: err.Error(),
		})
		result.Complete = false
		return result, nil
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
	bindings := mail.AccountBindingFile{Version: mail.AccountBindingVersion, Bindings: []mail.AccountBinding{}}
	bindingStore := c.send.AccountBindings
	if bindingStore == nil && c.store != nil {
		bindingStore = c.store.accountBindings
	}
	if bindingStore != nil {
		bindings, err = bindingStore.LoadAccountBindings()
		if err != nil {
			return result, err
		}
	}
	for _, acct := range targetAccounts {
		if acct.State == "degraded" {
			result.Failures = append(result.Failures, mail.SyncCheckFailure{
				Account: acct.Ref,
				Code:    "account_degraded",
				Message: "account is degraded: " + acct.DegradedReason + "; remediation: " + acct.DegradedRemediation,
			})
			continue
		}
		if len(acct.EmailAddresses) == 0 {
			result.Failures = append(result.Failures, mail.SyncCheckFailure{
				Account: acct.Ref,
				Code:    "account_no_email",
				Message: "account has no email addresses; cannot resolve provider endpoints",
			})
			continue
		}
		email, imapHost, imapPort, password, err := syncIdentityWithBindings(acct, credStore, bindings)
		if err != nil {
			code := failureCode(ctx, err)
			if code == "" {
				code = "imap_credentials_missing"
			}
			result.Failures = append(result.Failures, mail.SyncCheckFailure{
				Account: acct.Ref,
				Code:    code,
				Message: err.Error(),
			})
			continue
		}
		cfg := transport.ImapConfig{
			Host:     imapHost,
			Port:     imapPort,
			Username: email,
			Password: password,
		}
		if accountRefValue, decodeErr := mailref.DecodeAccount(acct.Ref); decodeErr == nil {
			if binding, found, bindingErr := mail.FindAccountBinding(bindings, accountRefValue.AccountID); bindingErr != nil {
				result.Failures = append(result.Failures, mail.SyncCheckFailure{
					Account: acct.Ref, Code: failureCode(ctx, bindingErr), Message: bindingErr.Error(),
				})
				continue
			} else if found {
				cfg.Username = binding.CredentialAccount
			}
		}

		localBoxes, err := c.store.ListMailboxes(ctx, mail.ListMailboxesRequest{AccountRef: acct.Ref})
		if err != nil {
			result.Failures = append(result.Failures, mail.SyncCheckFailure{
				Account: email,
				Code:    failureCode(ctx, err),
				Message: err.Error(),
			})
			continue
		}

		serverBoxes, err := c.getOrLoadMailboxes(ctx, imapOp, cfg, email)
		serverCatalogComplete := true
		if err != nil {
			result.Failures = append(result.Failures, mail.SyncCheckFailure{
				Account: email,
				Code:    failureCode(ctx, err),
				Message: err.Error(),
			})
			serverCatalogComplete = false
			if len(serverBoxes) == 0 {
				for _, lb := range localBoxes {
					result.Mailboxes = append(result.Mailboxes, localMailboxDelta(
						acct.Ref, lb, mail.MailboxDeltaStateUnresolved,
					))
				}
				continue
			}
		}

		if serverCatalogComplete && syncServerMailboxCount(serverBoxes) == 0 {
			result.Failures = append(result.Failures, mail.SyncCheckFailure{
				Account: email,
				Code:    syncCheckServerCatalogIncompleteCode,
				Message: "IMAP LIST returned no selectable mailbox identities; coverage is incomplete",
			})
		}

		matchedServer := make([]bool, len(serverBoxes))
		statusJobs := make([]syncStatusJob, 0, len(localBoxes)+len(serverBoxes))
		statusPlan := make([]syncStatusPlanItem, 0, len(localBoxes)+len(serverBoxes))
		for _, lb := range localBoxes {
			imapName, resolveErr := mapPathToIMAP(serverBoxes, lb.Path)
			if resolveErr != nil {
				state := mail.MailboxDeltaStateLocalOnly
				if !serverCatalogComplete || transport.ErrorCode(resolveErr) != transport.CodeIMAPMailboxNotFound {
					state = mail.MailboxDeltaStateUnresolved
				}
				statusPlan = append(statusPlan, syncStatusPlanItem{
					statusJobIndex: -1,
					delta:          localMailboxDelta(acct.Ref, lb, state),
					failures: []mail.SyncCheckFailure{{
						Account: email,
						Mailbox: strings.Join(lb.Path, "/"),
						Code:    failureCode(ctx, resolveErr),
						Message: resolveErr.Error(),
					}},
				})
				continue
			}
			serverIndex, found := syncServerMailboxIndex(serverBoxes, imapName)
			if !found {
				resolveErr = &transport.TransportError{
					Code:    transport.CodeIMAPMailboxNotFound,
					Message: fmt.Sprintf("resolved IMAP mailbox %q is absent from the server catalog", imapName),
				}
				state := mail.MailboxDeltaStateLocalOnly
				if !serverCatalogComplete {
					state = mail.MailboxDeltaStateUnresolved
				}
				statusPlan = append(statusPlan, syncStatusPlanItem{
					statusJobIndex: -1,
					delta:          localMailboxDelta(acct.Ref, lb, state),
					failures: []mail.SyncCheckFailure{{
						Account: email,
						Mailbox: strings.Join(lb.Path, "/"),
						Code:    failureCode(ctx, resolveErr),
						Message: resolveErr.Error(),
					}},
				})
				continue
			}
			if matchedServer[serverIndex] {
				resolveErr = &transport.TransportError{
					Code:    transport.CodeIMAPAmbiguousMailbox,
					Message: fmt.Sprintf("multiple local mailbox identities resolve to IMAP mailbox %q", imapName),
				}
				statusPlan = append(statusPlan, syncStatusPlanItem{
					statusJobIndex: -1,
					delta:          localMailboxDelta(acct.Ref, lb, mail.MailboxDeltaStateUnresolved),
					failures: []mail.SyncCheckFailure{{
						Account: email,
						Mailbox: strings.Join(lb.Path, "/"),
						Code:    failureCode(ctx, resolveErr),
						Message: resolveErr.Error(),
					}},
				})
				continue
			}
			matchedServer[serverIndex] = true
			serverName := syncServerMailboxWireName(serverBoxes[serverIndex])
			statusJobIndex := len(statusJobs)
			statusJobs = append(statusJobs, syncStatusJob{
				kind: syncStatusLocal, mailbox: imapName, local: lb, serverName: serverName,
			})
			statusPlan = append(statusPlan, syncStatusPlanItem{statusJobIndex: statusJobIndex})
		}
		for index, serverBox := range serverBoxes {
			if matchedServer[index] || !syncServerMailboxSelectable(serverBox) {
				continue
			}
			serverName := syncServerMailboxWireName(serverBox)
			statusJobIndex := len(statusJobs)
			statusJobs = append(statusJobs, syncStatusJob{
				kind: syncStatusServerOnly, mailbox: serverName, server: serverBox,
			})
			statusPlan = append(statusPlan, syncStatusPlanItem{statusJobIndex: statusJobIndex})
		}
		statusResults := runSyncStatusJobs(ctx, imapOp, cfg, statusJobs)
		for _, planItem := range statusPlan {
			if planItem.statusJobIndex < 0 {
				result.Mailboxes = append(result.Mailboxes, planItem.delta)
				result.Failures = append(result.Failures, planItem.failures...)
				continue
			}
			job := statusJobs[planItem.statusJobIndex]
			status := statusResults[planItem.statusJobIndex]
			if job.kind == syncStatusLocal {
				appendMatchedSyncStatusResult(ctx, &result, acct.Ref, email, job, status)
				continue
			}
			appendServerOnlySyncStatusResult(ctx, &result, acct.Ref, email, job, status)
		}
	}
	sortSyncCheckMailboxes(result.Mailboxes)
	result.Complete = len(result.Failures) == 0

	return result, nil
}

func localMailboxDelta(
	accountRef string,
	mailbox mail.Mailbox,
	state mail.MailboxDeltaState,
) mail.MailboxDelta {
	return mail.MailboxDelta{
		MailboxRef: mailbox.Ref, AccountRef: accountRef, State: state,
		Name: mailbox.Name, Path: append([]string(nil), mailbox.Path...),
		LocalMessagesAvailable: mailbox.LocalMessagesAvailable,
		LocalMessages:          mailbox.MessageCount,
	}
}

func serverMailboxDelta(
	accountRef string,
	mailbox transport.MailboxInfo,
	state mail.MailboxDeltaState,
) mail.MailboxDelta {
	path := syncServerMailboxPath(mailbox)
	return mail.MailboxDelta{
		MailboxRef: syncServerMailboxRef(accountRef, path), AccountRef: accountRef,
		State: state, Name: syncServerMailboxName(mailbox, path), Path: path,
		ServerName: syncServerMailboxWireName(mailbox),
	}
}

func syncServerMailboxWireName(mailbox transport.MailboxInfo) string {
	if mailbox.WireName != "" {
		return mailbox.WireName
	}
	return mailbox.Name
}

func syncServerMailboxPath(mailbox transport.MailboxInfo) []string {
	path := append([]string(nil), mailbox.DisplayPath...)
	if len(path) > 1 && path[0] == "[Gmail]" {
		path = path[1:]
	}
	if len(path) > 0 {
		return path
	}
	name := mailbox.DisplayName
	if name == "" {
		name = syncServerMailboxWireName(mailbox)
	}
	if name == "" {
		return nil
	}
	return []string{name}
}

func syncServerMailboxName(mailbox transport.MailboxInfo, path []string) string {
	if len(path) > 0 {
		return path[len(path)-1]
	}
	if mailbox.DisplayName != "" {
		return mailbox.DisplayName
	}
	return syncServerMailboxWireName(mailbox)
}

func syncServerMailboxRef(accountRef string, path []string) string {
	if len(path) == 0 {
		return ""
	}
	account, err := mailref.DecodeAccount(accountRef)
	if err != nil {
		return ""
	}
	ref, err := mailref.EncodeMailbox(account.AccountID, path)
	if err != nil {
		return ""
	}
	return ref
}

func syncServerMailboxCount(mailboxes []transport.MailboxInfo) int {
	count := 0
	for _, mailbox := range mailboxes {
		if syncServerMailboxSelectable(mailbox) {
			count++
		}
	}
	return count
}

func syncServerMailboxSelectable(mailbox transport.MailboxInfo) bool {
	if syncServerMailboxWireName(mailbox) == "" {
		return false
	}
	for _, flag := range mailbox.Flags {
		if strings.EqualFold(flag, "\\Noselect") {
			return false
		}
	}
	return true
}

func syncServerMailboxIndex(mailboxes []transport.MailboxInfo, wanted string) (int, bool) {
	if wanted == "" {
		return -1, false
	}
	if strings.EqualFold(wanted, "INBOX") {
		foldedIndex := -1
		foldedCount := 0
		for index, mailbox := range mailboxes {
			if !syncServerMailboxSelectable(mailbox) {
				continue
			}
			name := syncServerMailboxWireName(mailbox)
			if name == "INBOX" {
				return index, true
			}
			if strings.EqualFold(name, "INBOX") {
				foldedIndex = index
				foldedCount++
			}
		}
		if foldedCount == 1 {
			return foldedIndex, true
		}
	}
	matchedIndex := -1
	matchedCount := 0
	for index, mailbox := range mailboxes {
		if !syncServerMailboxSelectable(mailbox) {
			continue
		}
		if syncServerMailboxWireName(mailbox) == wanted {
			matchedIndex = index
			matchedCount++
		}
	}
	if matchedCount == 1 {
		return matchedIndex, true
	}
	return -1, false
}

func sortSyncCheckMailboxes(mailboxes []mail.MailboxDelta) {
	sort.SliceStable(mailboxes, func(left, right int) bool {
		leftKey := mailboxes[left].AccountRef + "\x00" + strings.Join(mailboxes[left].Path, "\x00")
		rightKey := mailboxes[right].AccountRef + "\x00" + strings.Join(mailboxes[right].Path, "\x00")
		if leftKey != rightKey {
			return leftKey < rightKey
		}
		if mailboxes[left].ServerName != mailboxes[right].ServerName {
			return mailboxes[left].ServerName < mailboxes[right].ServerName
		}
		return mailboxes[left].State < mailboxes[right].State
	})
}

// failureCode maps a sync-check error to its typed entry code. A dead
// context (or a deadline/cancelation error, which implies one) means the
// 30 s budget ran out: the entry is honest about partial coverage instead
// of repeating a stale server code.
func failureCode(ctx context.Context, err error) string {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "sync_check_timeout"
	}
	if code := transport.ErrorCode(err); code != "" {
		return code
	}
	return "sync_check_failed"
}

// Only a definitive pre-mutation rebuild permits a retry. An outer uncertain
// outcome retains precedence over any nested UIDVALIDITY diagnostic.
func isUIDValidityChangedError(err error) bool {
	return transport.ErrorCode(err) == "mailbox_uidvalidity_changed"
}
