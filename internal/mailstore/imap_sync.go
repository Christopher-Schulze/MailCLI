package mailstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

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
				_, _, imapHost, imapPort, providerErr := mail.ResolveTransportHosts(candidate, &binding)
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
