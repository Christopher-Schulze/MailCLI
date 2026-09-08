package mail

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	stdmail "net/mail"
	"strings"

	"mailcli/internal/transport"
)

func sendSender(from string) (string, error) {
	parsed, err := stdmail.ParseAddress(strings.TrimSpace(from))
	if err != nil || parsed.Address == "" {
		return "", validationError("invalid from address")
	}
	return parsed.Address, nil
}

func missingCredentialsError(sender string) error {
	return &OperationError{
		Code: "smtp_credentials_missing",
		Message: "no app-specific password is stored for " + sender +
			"; run 'mailcli send setup --from " + sender + "' to store one",
	}
}

func missingCredentialsErrorFor(sender, credential string) error {
	if strings.EqualFold(sender, credential) {
		return missingCredentialsError(sender)
	}
	account := sender
	setup := "'mailcli send setup --from " + sender + "'"
	if !strings.EqualFold(sender, credential) {
		account = credential
		setup = "'mailcli send setup --from " + sender + " --credential-account " + credential + "'"
	}
	return &OperationError{
		Code:    "smtp_credentials_missing",
		Message: "no app-specific password is stored for " + account + "; run " + setup + " to store one",
	}
}

func mirrorPendingError(err error) error {
	message := "SMTP submission was accepted, but the Sent copy was not observed because mirroring failed; " +
		"recipient delivery is unverified, the draft is retained, and the submission will not be retried"
	code := transport.ErrorCode(err)
	if code == "" {
		code = "send_mirror_pending"
	}
	return &OperationError{Code: code, Message: message + ": " + err.Error()}
}

func newMessageID(sender string) (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate message id: %w", err)
	}
	domain := sender[strings.LastIndex(sender, "@")+1:]
	return "<" + hex.EncodeToString(value[:]) + "@" + domain + ">", nil
}

func validateStoredDraftAddresses(draft Draft) error {
	if strings.TrimSpace(draft.From) != "" {
		if _, err := stdmail.ParseAddress(draft.From); err != nil {
			return validationError("invalid from address")
		}
	}
	seen := make(map[string]struct{}, len(draft.To)+len(draft.CC)+len(draft.BCC))
	for _, group := range [][]Recipient{draft.To, draft.CC, draft.BCC} {
		for _, recipient := range group {
			address := recipient.Address
			if recipient.Name != "" {
				address = (&stdmail.Address{Name: recipient.Name, Address: recipient.Address}).String()
			}
			parsed, err := stdmail.ParseAddress(address)
			if err != nil || parsed.Address == "" {
				return validationError("invalid recipient address")
			}
			normalized := strings.ToLower(parsed.Address)
			if _, duplicate := seen[normalized]; duplicate {
				return validationError("duplicate recipient address")
			}
			seen[normalized] = struct{}{}
		}
	}
	return nil
}

func draftEnvelopeRecipients(draft Draft) ([]string, error) {
	if err := validateStoredDraftAddresses(draft); err != nil {
		return nil, err
	}
	recipients := make([]Recipient, 0, len(draft.To)+len(draft.CC)+len(draft.BCC))
	recipients = append(recipients, draft.To...)
	recipients = append(recipients, draft.CC...)
	recipients = append(recipients, draft.BCC...)
	addresses := make([]string, 0, len(recipients))
	for index, recipient := range recipients {
		parsed, err := stdmail.ParseAddress(recipient.Address)
		if err != nil || parsed.Address == "" {
			return nil, validationError(fmt.Sprintf("invalid recipient address at position %d", index+1))
		}
		addresses = append(addresses, parsed.Address)
	}
	return addresses, nil
}

func draftRecipientValues(draft Draft) []string {
	recipients := make([]Recipient, 0, len(draft.To)+len(draft.CC)+len(draft.BCC))
	recipients = append(recipients, draft.To...)
	recipients = append(recipients, draft.CC...)
	recipients = append(recipients, draft.BCC...)
	values := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		values = append(values, recipient.Address)
	}
	return values
}

// DeliverViaTransport submits a draft over direct SMTP and mirrors it into
// the account's Sent mailbox without any local claim handling. It never
// resubmits after an accepted submission, even when the mirror fails; the
// partial evidence is returned alongside the mirror error. Callers own
// at-most-once semantics.
func DeliverViaTransport(ctx context.Context, send SendTransport, draft Draft) (evidence TransportEvidence, resultErr error) {
	if send.Submitter == nil || send.Mirror == nil || send.Credentials == nil {
		return TransportEvidence{}, &OperationError{
			Code:    "send_transport_unavailable",
			Message: "direct SMTP send is unavailable because no send transport is configured",
		}
	}
	if len(draft.To)+len(draft.CC)+len(draft.BCC) == 0 {
		return TransportEvidence{}, validationError("sending a draft requires at least one recipient")
	}
	if err := validateThreadSource(draft.SourceMessageID, draft.SourceReferences); err != nil {
		return TransportEvidence{}, err
	}
	envelopeRecipients, err := draftEnvelopeRecipients(draft)
	if err != nil {
		return TransportEvidence{}, err
	}
	identity, err := resolveTransportIdentity(send, draft)
	if err != nil {
		return TransportEvidence{}, err
	}
	sender := identity.Sender
	smtpHost, smtpPort, imapHost, imapPort, err := transport.ProviderHosts(sender)
	if err != nil {
		return TransportEvidence{}, err
	}
	password, err := send.Credentials.Load(identity.Credential)
	if err != nil || password == "" {
		return TransportEvidence{}, missingCredentialsErrorFor(sender, identity.Credential)
	}
	messageID, err := newMessageID(sender)
	if err != nil {
		return TransportEvidence{}, err
	}
	message, err := composeDraftSpool(ctx, draft, messageID)
	if err != nil {
		return TransportEvidence{}, err
	}
	defer func() {
		if err := message.Remove(); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	submitEvidence, err := submitComposedMessage(
		ctx,
		send.Submitter,
		transport.SubmitConfig{Host: smtpHost, Port: smtpPort, Username: identity.Credential, Password: password},
		sender, envelopeRecipients, message,
	)
	if err != nil {
		return TransportEvidence{}, err
	}
	evidence = TransportEvidence{
		ServerResponse:     submitEvidence.ServerResponse,
		MessageID:          submitEvidence.MessageID,
		SubmissionAccepted: true,
	}
	appendEvidence, err := mirrorComposedMessage(
		ctx,
		send.Mirror,
		transport.ImapConfig{Host: imapHost, Port: imapPort, Username: identity.Credential, Password: password},
		message,
		submitEvidence.MessageID,
	)
	if err != nil {
		return evidence, err
	}
	evidence.MirrorMailbox = appendEvidence.Mailbox
	evidence.MirrorUIDValidity = appendEvidence.UIDValidity
	evidence.MirrorUID = appendEvidence.UID
	evidence.MirrorAppended = appendEvidence.Appended
	return evidence, nil
}

// available rejects sends when the Service was created without a full
// SendTransport instead of panicking on a nil dependency.
func (t SendTransport) available() error {
	if t.Submitter == nil || t.Mirror == nil || t.Credentials == nil {
		return &OperationError{
			Code:    "send_transport_unavailable",
			Message: "direct SMTP send is unavailable because no send transport is configured",
		}
	}
	return nil
}
