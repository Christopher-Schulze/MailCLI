package mailstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

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
	email, credential, binding, err := c.resolveAccountIdentity(ctx, resolved.Reference.AccountID)
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

	_, _, imapHost, imapPort, err := mail.ResolveTransportHosts(email, binding)
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

func (c *Client) resolveAccountIdentity(ctx context.Context, accountID string) (string, string, *mail.AccountBinding, error) {
	account, found, err := c.store.accountForIdentity(ctx, accountID)
	if err != nil {
		return "", "", nil, operationErrorWithCause(
			"account_catalog_incomplete",
			fmt.Sprintf("cannot resolve account %s from the local Mail store; run 'mailcli doctor' and retry: %v", accountID, err),
			err,
		)
	}
	accounts := []mail.Account{account}
	if !found {
		accounts, err = c.store.ListAccounts(ctx)
		if err != nil {
			return "", "", nil, operationErrorWithCause(
				"account_catalog_incomplete",
				fmt.Sprintf("cannot resolve account %s from the local Mail store; run 'mailcli doctor' and retry: %v", accountID, err),
				err,
			)
		}
	}
	var bindings mail.AccountBindingFile
	bindingStore := c.send.AccountBindings
	if bindingStore == nil && c.store != nil {
		bindingStore = c.store.accountBindings
	}
	if bindingStore != nil {
		bindings, err = bindingStore.LoadAccountBindings()
		if err != nil {
			return "", "", nil, err
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
	sender, _, _, err := resolveAccountIdentityFromCatalog(accounts, accountID, credentials, mail.AccountBindingFile{
		Version: mail.AccountBindingVersion, Bindings: []mail.AccountBinding{},
	})
	return sender, err
}

func resolveAccountIdentityFromCatalog(
	accounts []mail.Account,
	accountID string,
	credentials transport.CredentialStore,
	bindings mail.AccountBindingFile,
) (string, string, *mail.AccountBinding, error) {
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
			password, loadErr := credentials.Load(binding.CredentialAccount)
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
			if pw, lerr := credentials.Load(address); lerr == nil && pw != "" {
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
