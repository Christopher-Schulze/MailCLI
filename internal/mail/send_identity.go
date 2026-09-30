package mail

import (
	"context"
	"fmt"
	stdmail "net/mail"
	"strings"

	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

type resolvedSendIdentity struct {
	Sender     string
	Credential string
	Binding    *AccountBinding
}

func cloneSendRecoveryIdentity(value *SendRecoveryIdentity) *SendRecoveryIdentity {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func normalizeSendRecoveryIdentity(value SendRecoveryIdentity) (SendRecoveryIdentity, error) {
	value.AccountID = strings.ToUpper(strings.TrimSpace(value.AccountID))
	if strings.ContainsAny(value.AccountID, "\r\n\x00") {
		return SendRecoveryIdentity{}, fmt.Errorf("invalid send recovery account ID")
	}
	var err error
	value.Host, value.Port, err = normalizeBindingEndpoint("imap", value.Host, value.Port)
	if err != nil || value.Host == "" {
		return SendRecoveryIdentity{}, fmt.Errorf("invalid send recovery endpoint")
	}
	value.Username, err = normalizeBindingAddress(value.Username)
	if err != nil {
		return SendRecoveryIdentity{}, fmt.Errorf("invalid send recovery username: %w", err)
	}
	return value, nil
}

func newSendRecoveryIdentity(identity resolvedSendIdentity, host string, port int) (*SendRecoveryIdentity, error) {
	value := SendRecoveryIdentity{Host: host, Port: port, Username: identity.Credential}
	if identity.Binding != nil {
		value.AccountID = identity.Binding.AccountID
	}
	normalized, err := normalizeSendRecoveryIdentity(value)
	if err != nil {
		return nil, err
	}
	return &normalized, nil
}

func (s *Service) resolveSendRecoveryTarget(ctx context.Context, draft Draft, attempt SendAttempt) (resolvedSendIdentity, transport.ImapConfig, error) {
	if attempt.RecoveryIdentity == nil {
		return resolvedSendIdentity{}, transport.ImapConfig{}, &OperationError{
			Code: "send_identity_unverifiable", DraftRef: draft.Ref,
			Message: "the retained send claim has no original IMAP target; inspect the draft and verify Sent manually; automatic recovery and SMTP replay remain blocked",
		}
	}
	identity, err := s.resolveSendIdentity(ctx, draft)
	if err != nil {
		return resolvedSendIdentity{}, transport.ImapConfig{}, err
	}
	_, _, host, port, err := ResolveTransportHosts(identity.Sender, identity.Binding)
	if err != nil {
		return resolvedSendIdentity{}, transport.ImapConfig{}, err
	}
	current, err := newSendRecoveryIdentity(identity, host, port)
	if err != nil {
		return resolvedSendIdentity{}, transport.ImapConfig{}, err
	}
	if *current != *attempt.RecoveryIdentity {
		return resolvedSendIdentity{}, transport.ImapConfig{}, &OperationError{
			Code: "send_identity_unverifiable", DraftRef: draft.Ref,
			Message: "the configured IMAP account, endpoint or username differs from the original send target; inspect send_attempt.recovery_identity and restore that target before reconciliation; SMTP replay remains blocked",
		}
	}
	return identity, transport.ImapConfig{Host: host, Port: port, Username: identity.Credential}, nil
}

func resolveTransportIdentity(send SendTransport, draft Draft) (resolvedSendIdentity, error) {
	sender, err := sendSender(draft.From)
	if err != nil {
		return resolvedSendIdentity{}, err
	}
	accountID, err := accountIDForDraft(draft.AccountRef)
	if err != nil {
		return resolvedSendIdentity{}, err
	}
	if send.AccountBindings == nil {
		if accountID != "" {
			return resolvedSendIdentity{}, accountBindingMissingError(sender, accountID)
		}
		return resolvedSendIdentity{Sender: sender, Credential: sender}, nil
	}
	document, err := send.AccountBindings.LoadAccountBindings()
	if err != nil {
		return resolvedSendIdentity{}, err
	}
	binding, found, err := ResolveAccountBinding(document, sender, accountID)
	if err != nil {
		return resolvedSendIdentity{}, err
	}
	if !found {
		if accountID != "" {
			return resolvedSendIdentity{}, accountBindingMissingError(sender, accountID)
		}
		return resolvedSendIdentity{Sender: sender, Credential: sender}, nil
	}
	return resolvedSendIdentity{Sender: sender, Credential: binding.CredentialAccount, Binding: &binding}, nil
}

func accountIDForDraft(accountRef string) (string, error) {
	accountRef = strings.TrimSpace(accountRef)
	if accountRef == "" {
		return "", nil
	}
	ref, err := mailref.DecodeAccount(accountRef)
	if err != nil {
		return "", &OperationError{
			Code:    "account_reference_invalid",
			Message: fmt.Sprintf("account ref %q cannot be decoded: %v", accountRef, err),
		}
	}
	return ref.AccountID, nil
}

func accountBindingMissingError(sender, accountID string) error {
	message := "sender " + sender + " has no configured account binding"
	if accountID != "" {
		message = fmt.Sprintf("sender %s is not bound to account %s", sender, accountID)
	}
	return &OperationError{Code: "account_binding_missing", Message: message + "; run 'mailcli send setup --from " + sender + " --account <account-ref>'"}
}

func (s *Service) resolveSendIdentity(ctx context.Context, draft Draft) (resolvedSendIdentity, error) {
	identity, err := resolveTransportIdentity(s.send, draft)
	if err != nil {
		return resolvedSendIdentity{}, err
	}
	if identity.Binding == nil || s.gateway == nil {
		return identity, nil
	}
	if err := s.validateBindingCatalog(ctx, draft.AccountRef, *identity.Binding, identity.Sender); err != nil {
		return resolvedSendIdentity{}, err
	}
	return identity, nil
}

func (s *Service) validateBindingCatalog(
	ctx context.Context,
	accountRef string,
	binding AccountBinding,
	sender string,
) error {
	accountID, err := accountIDForDraft(accountRef)
	if err != nil {
		return err
	}
	if accountID == "" {
		accountID = binding.AccountID
	}
	var accounts []Account
	if reader, ok := s.gateway.(BindingValidationCatalogReader); ok {
		catalog, err := reader.ListBindingValidationCatalog(ctx)
		if err != nil {
			return &OperationError{
				Code:    "account_catalog_incomplete",
				Message: fmt.Sprintf("cannot validate account binding %s before send: %v", binding.AccountID, err),
			}
		}
		accounts = catalog.Accounts
	} else {
		accounts, err = s.ListAccounts(ctx)
		if err != nil {
			return &OperationError{
				Code:    "account_catalog_incomplete",
				Message: fmt.Sprintf("cannot validate account binding %s before send: %v", binding.AccountID, err),
			}
		}
	}
	for _, account := range accounts {
		ref, decodeErr := mailref.DecodeAccount(account.Ref)
		if decodeErr != nil || !strings.EqualFold(ref.AccountID, accountID) {
			continue
		}
		if account.State == "disabled" {
			return &OperationError{Code: "account_binding_stale", Message: fmt.Sprintf("account binding %s refers to a disabled account", binding.AccountID)}
		}
		if account.State == "degraded" {
			return &OperationError{Code: "account_binding_account_degraded", Message: fmt.Sprintf("account binding %s refers to a degraded account: %s", binding.AccountID, account.DegradedReason)}
		}
		if !accountContainsAddress(account, sender) {
			return &OperationError{Code: "account_binding_stale", Message: fmt.Sprintf("sender %s is no longer permitted by account binding %s", sender, binding.AccountID)}
		}
		return nil
	}
	return &OperationError{Code: "account_binding_stale", Message: fmt.Sprintf("account binding %s refers to an account that is no longer enabled in Mail.app", binding.AccountID)}
}

func accountContainsAddress(account Account, address string) bool {
	sender, err := stdmail.ParseAddress(address)
	if err != nil {
		return false
	}
	for _, group := range [][]string{
		account.EmailAddresses, account.DiscoveredSenderIdentities, account.ConfiguredSenderAliases,
	} {
		for _, candidate := range group {
			parsed, err := stdmail.ParseAddress(candidate)
			if err == nil && strings.EqualFold(parsed.Address, sender.Address) {
				return true
			}
		}
	}
	return false
}
