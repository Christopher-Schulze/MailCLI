package mailref

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

const FormatVersion = 1

type AccountReferenceErrorKind string

const (
	AccountReferenceCorrupt            AccountReferenceErrorKind = "account_reference_corrupt"
	AccountReferenceVersionUnsupported AccountReferenceErrorKind = "account_reference_version_unsupported"
)

type AccountReferenceError struct {
	Kind    AccountReferenceErrorKind
	Version int
	Err     error
}

func (e *AccountReferenceError) Error() string {
	if e.Kind == AccountReferenceVersionUnsupported {
		return fmt.Sprintf("unsupported account reference version %d (expected %d)", e.Version, FormatVersion)
	}
	if e.Err != nil {
		return "corrupt account reference: " + e.Err.Error()
	}
	return "corrupt account reference"
}

func (e *AccountReferenceError) Unwrap() error {
	return e.Err
}

func (e *AccountReferenceError) ErrorCode() string {
	return string(e.Kind)
}

type Account struct {
	Version   int    `json:"version"`
	AccountID string `json:"account_id"`
}

type Mailbox struct {
	Version   int      `json:"version"`
	AccountID string   `json:"account_id"`
	Path      []string `json:"path"`
}

type Message struct {
	Version                int      `json:"version"`
	AccountID              string   `json:"account_id"`
	MailboxPath            []string `json:"mailbox_path"`
	LibraryID              string   `json:"library_id"`
	ExpectedMessageID      string   `json:"expected_message_id"`
	ExpectedSubject        string   `json:"expected_subject,omitempty"`
	ExpectedStoreUUID      string   `json:"expected_store_uuid,omitempty"`
	ExpectedStoreMailboxID int64    `json:"expected_store_mailbox_id,omitempty"`
	ExpectedStoreMessageID int64    `json:"expected_store_message_id,omitempty"`
	ExpectedStoreGlobalID  int64    `json:"expected_store_global_id,omitempty"`
}

type ListCursor struct {
	Version    int    `json:"version"`
	MailboxRef string `json:"mailbox_ref"`
	Offset     int    `json:"offset"`
	PreviousID string `json:"previous_id"`
}

func EncodeAccount(accountID string) (string, error) {
	payload, err := json.Marshal(Account{Version: FormatVersion, AccountID: accountID})
	if err != nil {
		return "", fmt.Errorf("encode account ref: %w", err)
	}
	return encodeToken("acct_", payload), nil
}

func DecodeAccount(value string) (Account, error) {
	ref, err := decodeToken[Account]("acct_", value)
	if err != nil {
		return Account{}, &AccountReferenceError{Kind: AccountReferenceCorrupt, Err: err}
	}
	if ref.Version != FormatVersion {
		return Account{}, &AccountReferenceError{
			Kind: AccountReferenceVersionUnsupported, Version: ref.Version,
		}
	}
	if ref.AccountID == "" {
		return Account{}, &AccountReferenceError{
			Kind: AccountReferenceCorrupt, Err: fmt.Errorf("account ID is empty"),
		}
	}
	return ref, nil
}

func EncodeMailbox(accountID string, path []string) (string, error) {
	payload, err := json.Marshal(Mailbox{
		Version: FormatVersion, AccountID: accountID, Path: path,
	})
	if err != nil {
		return "", fmt.Errorf("encode mailbox ref: %w", err)
	}
	return encodeToken("mbx_", payload), nil
}

func DecodeMailbox(value string) (Mailbox, error) {
	ref, err := decodeToken[Mailbox]("mbx_", value)
	if err != nil {
		return Mailbox{}, err
	}
	if ref.Version != FormatVersion || ref.AccountID == "" || len(ref.Path) == 0 {
		return Mailbox{}, fmt.Errorf("invalid mailbox ref payload")
	}
	return ref, nil
}

func EncodeMessage(ref Message) (string, error) {
	ref.Version = FormatVersion
	if ref.hasAnyStoreIdentity() && !ref.IsStoreBound() {
		return "", fmt.Errorf("invalid message ref store identity")
	}
	payload, err := json.Marshal(ref)
	if err != nil {
		return "", fmt.Errorf("encode message ref: %w", err)
	}
	return encodeToken("msg_", payload), nil
}

func DecodeMessage(value string) (Message, error) {
	ref, err := decodeToken[Message]("msg_", value)
	if err != nil {
		return Message{}, err
	}
	if ref.Version != FormatVersion {
		return Message{}, fmt.Errorf("invalid message ref payload")
	}
	if ref.AccountID == "" || len(ref.MailboxPath) == 0 || ref.LibraryID == "" {
		return Message{}, fmt.Errorf("invalid message ref payload")
	}
	if ref.hasAnyStoreIdentity() && !ref.IsStoreBound() {
		return Message{}, fmt.Errorf("invalid message ref store identity")
	}
	return ref, nil
}

func (m Message) IsStoreBound() bool {
	return m.ExpectedStoreUUID != "" && m.ExpectedStoreMailboxID > 0
}

func (m Message) hasAnyStoreIdentity() bool {
	return m.ExpectedStoreUUID != "" || m.ExpectedStoreMailboxID != 0 ||
		m.ExpectedStoreMessageID != 0 || m.ExpectedStoreGlobalID != 0
}

func EncodeListCursor(ref ListCursor) (string, error) {
	ref.Version = FormatVersion
	payload, err := json.Marshal(ref)
	if err != nil {
		return "", fmt.Errorf("encode list cursor: %w", err)
	}
	return encodeToken("cur_", payload), nil
}

func DecodeListCursor(value string) (ListCursor, error) {
	cursor, err := decodeToken[ListCursor]("cur_", value)
	if err != nil {
		return ListCursor{}, err
	}
	if cursor.Version != FormatVersion || cursor.MailboxRef == "" || cursor.Offset < 1 || cursor.PreviousID == "" {
		return ListCursor{}, fmt.Errorf("invalid list cursor payload")
	}
	return cursor, nil
}

func encodeToken(prefix string, payload []byte) string {
	return prefix + base64.RawURLEncoding.EncodeToString(payload)
}

type referencePayload interface {
	Account | Mailbox | Message | ListCursor
}

func decodeToken[T referencePayload](prefix string, value string) (T, error) {
	var result T
	if !strings.HasPrefix(value, prefix) {
		return result, fmt.Errorf("invalid %s token prefix", strings.TrimSuffix(prefix, "_"))
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return result, fmt.Errorf("decode %s token: %w", strings.TrimSuffix(prefix, "_"), err)
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return result, fmt.Errorf("parse %s token: %w", strings.TrimSuffix(prefix, "_"), err)
	}
	return result, nil
}
