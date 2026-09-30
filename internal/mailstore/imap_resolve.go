package mailstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

type imapTargetOptions struct {
	rejectTrash     bool
	rejectDuplicate bool
	forMutation     bool
	// localUIDWithoutValidity accepts the local server UID of a mailbox whose
	// local UIDVALIDITY is unknown instead of searching the server by
	// Message-ID. The target then has uidvalidity 0 and a local Message-ID,
	// and the caller must match the fetched Message-ID before using the data.
	localUIDWithoutValidity bool
}

func (c *Client) resolveImapTarget(ctx context.Context, messageRef string) (imapTarget, error) {
	return c.resolveImapTargetWithOptions(ctx, messageRef, imapTargetOptions{rejectDuplicate: true})
}

// resolveImapTargetForExcerpt is the read-only resolution for excerpt fetches;
// see imapTargetOptions.localUIDWithoutValidity.
func (c *Client) resolveImapTargetForExcerpt(ctx context.Context, messageRef string) (imapTarget, error) {
	return c.resolveImapTargetWithOptions(ctx, messageRef, imapTargetOptions{rejectDuplicate: true, localUIDWithoutValidity: true})
}

func (c *Client) resolveImapTargetForMutation(ctx context.Context, messageRef string) (imapTarget, error) {
	return c.resolveImapTargetWithOptions(ctx, messageRef, imapTargetOptions{rejectDuplicate: true, forMutation: true})
}

func (c *Client) resolveImapTargetForDelete(ctx context.Context, messageRef string) (imapTarget, error) {
	return c.resolveImapTargetWithOptions(ctx, messageRef, imapTargetOptions{rejectTrash: true, rejectDuplicate: true, forMutation: true})
}

func (c *Client) resolveImapTargetWithOptions(
	ctx context.Context,
	messageRef string,
	options imapTargetOptions,
) (imapTarget, error) {
	rejectTrash, rejectDuplicate, forMutation := options.rejectTrash, options.rejectDuplicate, options.forMutation
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
	// A label ref names the selected mailbox, but its local UID belongs to
	// the physical mailbox (for Gmail, usually All Mail).
	if resolved.Reference.ExpectedIMAPUID != 0 &&
		slices.Equal(resolved.PhysicalLocation.VisiblePath, resolved.Reference.MailboxPath) {
		uid, uidval, usable, directErr := c.directIMAPIdentity(ctx, resolved)
		if directErr != nil {
			var stale *Error
			if errors.As(directErr, &stale) && stale.Code == "stale_reference" {
				return target, directErr
			}
			localIdentityErr = directErr
		} else if usable {
			target.uid, target.uidvalidity, target.duplicateMatches = uid, uidval, 1
		} else if options.localUIDWithoutValidity {
			target.uid, target.duplicateMatches = uid, 1
		}
	}
	if target.messageID == "" && (target.uid == 0 || target.uidvalidity == 0) {
		target.messageID, localIdentityErr = c.readLocalMessageID(ctx, messageRef)
		if localIdentityErr != nil && !safeTargetedFallback(localIdentityErr) {
			return target, fmt.Errorf("resolve IMAP message identity: %w", localIdentityErr)
		}
	}
	if target.uidvalidity == 0 && target.messageID == "" {
		// An unverified local UID is only usable with a Message-ID to match.
		target.uid, target.duplicateMatches = 0, 0
	}
	if resolved.PhysicalLocation.Scheme != "imap" || resolved.Reference.AccountID == "local" {
		return target, &transport.TransportError{
			Code:    transport.CodeLocalOnlyMailbox,
			Message: "mutations are not supported on local-only mailboxes (On My Mac)",
		}
	}
	if target.uid == 0 && target.messageID == "" {
		return target, identityResolutionError(messageRef, localIdentityErr, &transport.TransportError{
			Code:    transport.CodeIMAPMessageUIDUnknown,
			Message: "message has no Message-ID or independently verified server UID and UIDVALIDITY; let Mail.app synchronize and resolve a fresh reference",
		})
	}

	email, cfg, err := c.imapConfigForAccountID(ctx, resolved.Reference.AccountID)
	if err != nil {
		return target, err
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

	resolveMailbox := transport.ResolveMailboxPath
	if forMutation {
		resolveMailbox = transport.ResolveMailboxPathForMutation
	}
	imapBox, err := resolveMailbox(boxes, resolved.Reference.MailboxPath)
	if err != nil {
		return target, err
	}
	target.imapMailbox = imapBox
	if rejectTrash {
		if err := rejectAlreadyTrashed(target); err != nil {
			return target, err
		}
	}

	if target.messageID != "" && target.uid == 0 {
		if err := ctx.Err(); err != nil {
			return target, err
		}
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
					"message ID %s matched %d messages in mailbox %s; refusing identity resolution because the message is ambiguous; resolve the duplicate messages and rerun the command",
					target.messageID, matchCount, imapBox,
				),
			}
		}
	}
	if target.uid == 0 || (target.uidvalidity == 0 && !options.localUIDWithoutValidity) {
		return target, identityResolutionError(messageRef, localIdentityErr, &transport.TransportError{
			Code:    transport.CodeIMAPMessageUIDUnknown,
			Message: "no independently verified IMAP UID and UIDVALIDITY are available; refresh the local Mail catalog or provide a fresh Message-ID-backed reference",
		})
	}

	// Build base summary
	mailboxRef, _ := mailref.EncodeMailbox(resolved.Reference.AccountID, resolved.Reference.MailboxPath)
	if s, err := mapMessageSummary(resolved.Record, mailboxRef, resolved.Reference.AccountID, resolved.Reference.MailboxPath, c.store.storeUUID); err == nil {
		s.MessageID = target.messageID
		refUID, refValidity := target.uid, target.uidvalidity
		if !slices.Equal(resolved.PhysicalLocation.VisiblePath, resolved.Reference.MailboxPath) ||
			resolved.Record.RemoteID != int64(target.uid) {
			refUID, refValidity = 0, 0
		}
		s.Ref = updateSummaryIdentity(s.Ref, target.messageID, refUID, refValidity)
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

// imapConfigForAccountID resolves the IMAP endpoint and login of one account
// without consulting the Apple Events gateway: the provider or bound hosts and
// the stored password of the account's credential identity.
func (c *Client) imapConfigForAccountID(ctx context.Context, accountID string) (string, transport.ImapConfig, error) {
	if err := ctx.Err(); err != nil {
		return "", transport.ImapConfig{}, err
	}
	email, credential, binding, err := c.resolveAccountIdentity(ctx, accountID)
	if err != nil {
		var typed interface{ ErrorCode() string }
		if errors.As(err, &typed) {
			return "", transport.ImapConfig{}, err
		}
		return "", transport.ImapConfig{}, &transport.TransportError{
			Code:    transport.CodeLocalOnlyMailbox,
			Message: err.Error(),
		}
	}
	_, _, imapHost, imapPort, err := mail.ResolveTransportHosts(email, binding)
	if err != nil {
		return "", transport.ImapConfig{}, err
	}
	credStore := c.send.Credentials
	if credStore == nil {
		return "", transport.ImapConfig{}, &transport.TransportError{
			Code:    transport.CodeSMTPCredentialsMissing,
			Message: "credential store is not available",
		}
	}
	if err := ctx.Err(); err != nil {
		return "", transport.ImapConfig{}, err
	}
	password, err := credStore.Load(credential)
	if contextErr := ctx.Err(); contextErr != nil {
		return "", transport.ImapConfig{}, contextErr
	}
	if err != nil || password == "" {
		return "", transport.ImapConfig{}, &transport.TransportError{
			Code:    transport.CodeSMTPCredentialsMissing,
			Message: fmt.Sprintf("no stored credentials for %s (run '%s')", credential, credentialSetupCommand(email, credential)),
		}
	}
	return email, transport.ImapConfig{Host: imapHost, Port: imapPort, Username: credential, Password: password}, nil
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
	if uid == 0 {
		ref.ExpectedIMAPMailboxID = 0
	}
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

func (c *Client) resolveAccountIdentity(ctx context.Context, accountID string) (string, string, *mail.AccountBinding, error) {
	if err := ctx.Err(); err != nil {
		return "", "", nil, err
	}
	ctx = c.WithAccountBindingSnapshot(ctx)
	bindings, err := c.accountBindingsForResolution(ctx)
	if err != nil {
		return "", "", nil, err
	}
	account, found, err := c.store.accountForIdentity(ctx, accountID, bindings)
	if err != nil {
		return "", "", nil, operationErrorWithCause(
			"account_catalog_incomplete",
			fmt.Sprintf("cannot resolve account %s from the local Mail store; run 'mailcli doctor' and retry: %v", accountID, err),
			err,
		)
	}
	accounts := []mail.Account{account}
	if !found {
		accounts, err = c.store.listAccountsWithBindings(ctx, bindings)
		if err != nil {
			return "", "", nil, operationErrorWithCause(
				"account_catalog_incomplete",
				fmt.Sprintf("cannot resolve account %s from the local Mail store; run 'mailcli doctor' and retry: %v", accountID, err),
				err,
			)
		}
	}
	return resolveAccountIdentityFromCatalog(ctx, accounts, accountID, c.send.Credentials, bindings)
}

func resolveAccountEmailFromCatalog(
	ctx context.Context,
	accounts []mail.Account,
	accountID string,
	credentials transport.CredentialStore,
) (string, error) {
	sender, _, _, err := resolveAccountIdentityFromCatalog(ctx, accounts, accountID, credentials, mail.AccountBindingFile{
		Version: mail.AccountBindingVersion, Bindings: []mail.AccountBinding{},
	})
	return sender, err
}

func resolveAccountIdentityFromCatalog(
	ctx context.Context,
	accounts []mail.Account,
	accountID string,
	credentials transport.CredentialStore,
	bindings mail.AccountBindingFile,
) (string, string, *mail.AccountBinding, error) {
	if err := ctx.Err(); err != nil {
		return "", "", nil, err
	}
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
			return "", "", nil, operationError(
				accountDisabledCode,
				fmt.Sprintf("account %s is disabled in the local Mail catalog; enable it in Mail.app and retry", accountID),
			)
		}
		if acct.State == "degraded" {
			return "", "", nil, operationError(
				"account_degraded",
				fmt.Sprintf("account %s is degraded (%s): %s", accountID, acct.DegradedReason, acct.DegradedRemediation),
			)
		}
		binding, bindingFound, err := mail.FindAccountBinding(bindings, accountID)
		if err != nil {
			return "", "", nil, err
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
				return "", "", nil, operationError(
					accountBindingStaleCode,
					fmt.Sprintf("account binding %s has no sender alias present in the local account catalog", accountID),
				)
			}
			if _, _, _, _, err := mail.ResolveTransportHosts(sender, &binding); err != nil {
				return "", "", nil, err
			}
			if credentials == nil {
				return "", "", nil, operationError(
					accountIdentityMissingCode,
					"no credential store configured; run 'mailcli send setup --from "+sender+" --credential-account "+binding.CredentialAccount+"' first",
				)
			}
			if err := ctx.Err(); err != nil {
				return "", "", nil, err
			}
			password, loadErr := credentials.Load(binding.CredentialAccount)
			if err := ctx.Err(); err != nil {
				return "", "", nil, err
			}
			if loadErr != nil || password == "" {
				return "", "", nil, operationError(
					accountIdentityMissingCode,
					fmt.Sprintf("account %s has no stored credentials for %s; run 'mailcli send setup --from %s --account <account-ref>'", accountID, binding.CredentialAccount, sender),
				)
			}
			return sender, binding.CredentialAccount, &binding, nil
		}
		if len(acct.EmailAddresses) == 0 {
			return "", "", nil, operationError(
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
			return "", "", nil, providerErr
		}
		if credentials == nil {
			return "", "", nil, operationError(
				accountIdentityMissingCode,
				"no credential store configured; run 'mailcli send setup --from ADDRESS' first",
			)
		}
		for _, address := range usableAddresses {
			if err := ctx.Err(); err != nil {
				return "", "", nil, err
			}
			pw, lerr := credentials.Load(address)
			if err := ctx.Err(); err != nil {
				return "", "", nil, err
			}
			if lerr == nil && pw != "" {
				return address, address, nil, nil
			}
		}
		return "", "", nil, operationError(
			accountIdentityMissingCode,
			fmt.Sprintf("account %s has no address with stored credentials; run 'mailcli send setup --from ADDRESS' for one of %v",
				accountID, acct.EmailAddresses,
			),
		)
	}
	if len(decodeFailures) > 0 {
		return "", "", nil, accountReferenceDecodeError(accountID, decodeFailures)
	}
	if binding, found, bindingErr := mail.FindAccountBinding(bindings, accountID); bindingErr != nil {
		return "", "", nil, bindingErr
	} else if found {
		return "", "", nil, operationError(
			accountBindingStaleCode,
			fmt.Sprintf("account binding %s refers to an account that is no longer enabled in Mail.app", binding.AccountID),
		)
	}
	return "", "", nil, operationError(
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

	// Concurrent rows of one page share a single LIST per account.
	loaded, err, _ := c.mailboxLoads.Do(key, func() (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		boxes, err := op.ListMailboxes(ctx, cfg)
		if err != nil {
			return boxes, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
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
	})
	boxes, _ := loaded.([]transport.MailboxInfo)
	return cloneMailboxInfos(boxes), err
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
