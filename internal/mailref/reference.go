package mailref

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unsafe"
)

const (
	LegacyFormatVersion          = 1
	FormatVersion                = 2
	compactReferenceVersion byte = FormatVersion
)

var errCompactPayloadNil = errors.New("compact payload is nil")

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
	Version                 int      `json:"version"`
	AccountID               string   `json:"account_id"`
	MailboxPath             []string `json:"mailbox_path"`
	LibraryID               string   `json:"library_id"`
	ExpectedMessageID       string   `json:"expected_message_id"`
	ExpectedIMAPUID         uint32   `json:"expected_imap_uid,omitempty"`
	ExpectedIMAPUIDValidity uint32   `json:"expected_imap_uid_validity,omitempty"`
	ExpectedIMAPMailboxID   int64    `json:"expected_imap_mailbox_id,omitempty"`
	ExpectedSubject         string   `json:"expected_subject,omitempty"`
	ExpectedStoreUUID       string   `json:"expected_store_uuid,omitempty"`
	ExpectedStoreMailboxID  int64    `json:"expected_store_mailbox_id,omitempty"`
	ExpectedStoreMessageID  int64    `json:"expected_store_message_id,omitempty"`
	ExpectedStoreGlobalID   int64    `json:"expected_store_global_id,omitempty"`
}

type ListCursor struct {
	Version    int    `json:"version"`
	MailboxRef string `json:"mailbox_ref"`
	Offset     int    `json:"offset"`
	PreviousID string `json:"previous_id"`
}

func EncodeAccount(accountID string) (string, error) {
	token, err := EncodeCompactTokenPayload("acct_", &compactPayload{AccountID: accountID}, compactReferenceVersion)
	if err != nil {
		return "", fmt.Errorf("encode account ref: %w", err)
	}
	return token, nil
}

func DecodeAccount(value string) (Account, error) {
	payload, err := DecodeTokenPayload("acct_", value)
	if err != nil {
		return Account{}, &AccountReferenceError{Kind: AccountReferenceCorrupt, Err: err}
	}
	ref, version, err := decodeAccountPayload(payload)
	if err != nil {
		unsupported := version != 0 && ((isCompactPayload(payload) && version != FormatVersion) ||
			(!isCompactPayload(payload) && version != LegacyFormatVersion))
		if unsupported {
			return Account{}, &AccountReferenceError{Kind: AccountReferenceVersionUnsupported, Version: version}
		}
		return Account{}, &AccountReferenceError{Kind: AccountReferenceCorrupt, Err: err}
	}
	if ref.AccountID == "" {
		return Account{}, &AccountReferenceError{
			Kind: AccountReferenceCorrupt, Err: fmt.Errorf("account ID is empty"),
		}
	}
	return *ref, nil
}

func EncodeMailbox(accountID string, path []string) (string, error) {
	token, err := EncodeCompactTokenPayload("mbx_", &compactPayload{AccountID: accountID, MailboxPath: path}, compactReferenceVersion)
	if err != nil {
		return "", fmt.Errorf("encode mailbox ref: %w", err)
	}
	return token, nil
}

func DecodeMailbox(value string) (Mailbox, error) {
	payload, err := DecodeTokenPayload("mbx_", value)
	if err != nil {
		return Mailbox{}, err
	}
	ref, err := decodeMailboxPayload(payload)
	if err != nil {
		return Mailbox{}, err
	}
	if ref.AccountID == "" || len(ref.Path) == 0 {
		return Mailbox{}, fmt.Errorf("invalid mailbox ref payload")
	}
	return *ref, nil
}

func EncodeMessage(ref Message) (string, error) {
	ref.Version = FormatVersion
	if ref.hasAnyStoreIdentity() && !ref.IsStoreBound() {
		return "", fmt.Errorf("invalid message ref store identity")
	}
	if err := ref.validateIMAPIdentity(); err != nil {
		return "", err
	}
	token, err := EncodeCompactTokenPayload("msg_", &compactPayload{
		AccountID: ref.AccountID, MailboxPath: ref.MailboxPath, LibraryID: ref.LibraryID,
		ExpectedMessageID: ref.ExpectedMessageID, ExpectedIMAPUID: ref.ExpectedIMAPUID,
		ExpectedIMAPUIDValidity: ref.ExpectedIMAPUIDValidity, ExpectedIMAPMailboxID: ref.ExpectedIMAPMailboxID,
		ExpectedSubject: ref.ExpectedSubject, ExpectedStoreUUID: ref.ExpectedStoreUUID,
		ExpectedStoreMailboxID: ref.ExpectedStoreMailboxID, ExpectedStoreMessageID: ref.ExpectedStoreMessageID,
		ExpectedStoreGlobalID: ref.ExpectedStoreGlobalID,
	}, compactReferenceVersion)
	if err != nil {
		return "", fmt.Errorf("encode message ref: %w", err)
	}
	return token, nil
}

func DecodeMessage(value string) (Message, error) {
	payload, err := DecodeTokenPayload("msg_", value)
	if err != nil {
		return Message{}, err
	}
	ref, err := decodeMessagePayload(payload)
	if err != nil {
		return Message{}, err
	}
	if ref.AccountID == "" || len(ref.MailboxPath) == 0 || ref.LibraryID == "" {
		return Message{}, fmt.Errorf("invalid message ref payload")
	}
	if ref.hasAnyStoreIdentity() && !ref.IsStoreBound() {
		return Message{}, fmt.Errorf("invalid message ref store identity")
	}
	if err := ref.validateIMAPIdentity(); err != nil {
		return Message{}, err
	}
	return *ref, nil
}

func (m Message) IsStoreBound() bool {
	return m.ExpectedStoreUUID != "" && m.ExpectedStoreMailboxID > 0
}

func (m Message) hasAnyStoreIdentity() bool {
	return m.ExpectedStoreUUID != "" || m.ExpectedStoreMailboxID != 0 ||
		m.ExpectedStoreMessageID != 0 || m.ExpectedStoreGlobalID != 0
}

func (m Message) validateIMAPIdentity() error {
	if m.ExpectedIMAPUID == 0 && m.ExpectedIMAPUIDValidity != 0 {
		return fmt.Errorf("invalid message ref IMAP identity: UIDVALIDITY requires a UID")
	}
	if m.ExpectedIMAPUID == 0 && m.ExpectedIMAPMailboxID != 0 {
		return fmt.Errorf("invalid message ref IMAP identity: mailbox ID requires a UID")
	}
	if m.ExpectedIMAPMailboxID < 0 {
		return fmt.Errorf("invalid message ref IMAP identity: mailbox ID must be non-negative")
	}
	return nil
}

func EncodeListCursor(ref ListCursor) (string, error) {
	ref.Version = FormatVersion
	token, err := EncodeCompactTokenPayload("cur_", &compactPayload{
		MailboxRef: ref.MailboxRef, Offset: int64(ref.Offset), PreviousID: ref.PreviousID,
	}, compactReferenceVersion)
	if err != nil {
		return "", fmt.Errorf("encode list cursor: %w", err)
	}
	return token, nil
}

func DecodeListCursor(value string) (ListCursor, error) {
	payload, err := DecodeTokenPayload("cur_", value)
	if err != nil {
		return ListCursor{}, err
	}
	cursor, err := decodeListCursorPayload(payload)
	if err != nil {
		return ListCursor{}, err
	}
	if cursor.MailboxRef == "" || cursor.Offset < 1 || cursor.PreviousID == "" {
		return ListCursor{}, fmt.Errorf("invalid list cursor payload")
	}
	return *cursor, nil
}

func encodeToken(prefix string, payload []byte) string {
	return prefix + base64.RawURLEncoding.EncodeToString(payload)
}

func decodeAccountPayload(payload []byte) (*Account, int, error) {
	if !isCompactPayload(payload) {
		var ref Account
		if err := json.Unmarshal(payload, &ref); err != nil {
			return nil, 0, fmt.Errorf("parse account token: %w", err)
		}
		if ref.Version != LegacyFormatVersion {
			return nil, ref.Version, unsupportedVersion("account reference", ref.Version)
		}
		return &ref, ref.Version, nil
	}
	compact, err := DecodeCompactPayload(payload, compactReferenceVersion)
	if err != nil {
		version := 0
		if isCompactPayload(payload) {
			version = int(payload[1])
		}
		return nil, version, err
	}
	compact.Version = FormatVersion
	return (*Account)(unsafe.Pointer(compact)), FormatVersion, nil
}

func decodeMailboxPayload(payload []byte) (*Mailbox, error) {
	if !isCompactPayload(payload) {
		var ref Mailbox
		if err := json.Unmarshal(payload, &ref); err != nil {
			return nil, fmt.Errorf("parse mailbox token: %w", err)
		}
		if ref.Version != LegacyFormatVersion {
			return nil, unsupportedVersion("mailbox reference", ref.Version)
		}
		return &ref, nil
	}
	compact, err := DecodeCompactPayload(payload, compactReferenceVersion)
	if err != nil {
		return nil, err
	}
	compact.Version = FormatVersion
	return (*Mailbox)(unsafe.Pointer(compact)), nil
}

func decodeMessagePayload(payload []byte) (*Message, error) {
	if !isCompactPayload(payload) {
		var ref Message
		if err := json.Unmarshal(payload, &ref); err != nil {
			return nil, fmt.Errorf("parse message token: %w", err)
		}
		if ref.Version != LegacyFormatVersion {
			return nil, unsupportedVersion("message reference", ref.Version)
		}
		return &ref, nil
	}
	compact, err := DecodeCompactPayload(payload, compactReferenceVersion)
	if err != nil {
		return nil, err
	}
	compact.Version = FormatVersion
	return (*Message)(unsafe.Pointer(compact)), nil
}

func decodeListCursorPayload(payload []byte) (*ListCursor, error) {
	if !isCompactPayload(payload) {
		var cursor ListCursor
		if err := json.Unmarshal(payload, &cursor); err != nil {
			return nil, fmt.Errorf("parse list cursor token: %w", err)
		}
		if cursor.Version != LegacyFormatVersion {
			return nil, unsupportedVersion("list cursor", cursor.Version)
		}
		return &cursor, nil
	}
	compact, err := DecodeCompactPayload(payload, compactReferenceVersion)
	if err != nil {
		return nil, err
	}
	if compact.Offset < int64(-int(^uint(0)>>1)-1) || compact.Offset > int64(int(^uint(0)>>1)) {
		return nil, fmt.Errorf("list cursor offset overflows int")
	}
	compact.ExpectedStoreGlobalID = int64(FormatVersion)
	return (*ListCursor)(unsafe.Pointer(&compact.ExpectedStoreGlobalID)), nil
}

// CompactSearchCursor carries the fields needed to continue a search.
type CompactSearchCursor struct {
	Fingerprint    string
	StoreUUID      string
	IndexRevision  string
	ReceivedAt     int64
	ReceivedAtNull bool
	RowID          int64
	Inclusive      bool
}

// EncodeSearchCursorPayload encodes a versioned search cursor payload.
func EncodeSearchCursorPayload(cursor CompactSearchCursor, version byte) ([]byte, error) {
	var flags uint8
	if cursor.ReceivedAtNull {
		flags |= 1
	}
	if cursor.Inclusive {
		flags |= 2
	}
	return encodeCompactJSON(version, compactPayload{
		Fingerprint: cursor.Fingerprint, StoreUUID: cursor.StoreUUID, IndexRevision: cursor.IndexRevision,
		ReceivedAt: cursor.ReceivedAt, Flags: flags, RowID: cursor.RowID,
	})
}

// DecodeSearchCursorPayload decodes a versioned search cursor payload.
func DecodeSearchCursorPayload(payload []byte, version byte) (CompactSearchCursor, error) {
	compact, err := DecodeCompactPayload(payload, version)
	if err != nil {
		return CompactSearchCursor{}, err
	}
	if compact.Flags > 3 {
		return CompactSearchCursor{}, fmt.Errorf("unknown search cursor flags 0x%x", compact.Flags)
	}
	return CompactSearchCursor{
		Fingerprint: compact.Fingerprint, StoreUUID: compact.StoreUUID, IndexRevision: compact.IndexRevision,
		ReceivedAt: compact.ReceivedAt, ReceivedAtNull: compact.Flags&1 != 0,
		RowID: compact.RowID, Inclusive: compact.Flags&2 != 0,
	}, nil
}

// CompactStoreListCursor carries the fields needed to continue a store list.
type CompactStoreListCursor struct {
	StoreUUID        string
	MailboxRef       string
	DateReceived     int64
	DateReceivedNull bool
	RowID            int64
}

// EncodeStoreListCursorPayload encodes a versioned store-list cursor payload.
func EncodeStoreListCursorPayload(cursor CompactStoreListCursor, version byte) ([]byte, error) {
	var flags uint8
	if cursor.DateReceivedNull {
		flags = 1
	}
	return encodeCompactJSON(version, compactPayload{
		StoreUUID: cursor.StoreUUID, MailboxRef: cursor.MailboxRef,
		DateReceived: cursor.DateReceived, Flags: flags, RowID: cursor.RowID,
	})
}

// DecodeStoreListCursorPayload decodes a versioned store-list cursor payload.
func DecodeStoreListCursorPayload(payload []byte, version byte) (CompactStoreListCursor, error) {
	compact, err := DecodeCompactPayload(payload, version)
	if err != nil {
		return CompactStoreListCursor{}, err
	}
	if compact.Flags > 1 {
		return CompactStoreListCursor{}, fmt.Errorf("unknown list cursor flags 0x%x", compact.Flags)
	}
	return CompactStoreListCursor{
		StoreUUID: compact.StoreUUID, MailboxRef: compact.MailboxRef, DateReceived: compact.DateReceived,
		DateReceivedNull: compact.Flags != 0, RowID: compact.RowID,
	}, nil
}

func isCompactPayload(payload []byte) bool {
	return len(payload) >= 2 && payload[0] == compactPayloadMarker
}

func unsupportedVersion(kind string, version int) error {
	return fmt.Errorf("unsupported %s version %d (expected %d)", kind, version, FormatVersion)
}

const (
	// MaxCompactPayloadBytes bounds both legacy and compact token decoding.
	MaxCompactPayloadBytes = 1 << 20
	MaxCompactStringBytes  = 256 << 10
	MaxCompactListLength   = 1024
	compactPayloadMarker   = 0xd7
)

type compactPayload struct {
	Version                 int      `json:"-"`
	AccountID               string   `json:"a,omitempty"`
	MailboxPath             []string `json:"p,omitempty"`
	LibraryID               string   `json:"l,omitempty"`
	ExpectedMessageID       string   `json:"e,omitempty"`
	ExpectedIMAPUID         uint32   `json:"u,omitempty"`
	ExpectedIMAPUIDValidity uint32   `json:"v,omitempty"`
	ExpectedIMAPMailboxID   int64    `json:"c,omitempty"`
	ExpectedSubject         string   `json:"h,omitempty"`
	ExpectedStoreUUID       string   `json:"x,omitempty"`
	ExpectedStoreMailboxID  int64    `json:"b,omitempty"`
	ExpectedStoreMessageID  int64    `json:"n,omitempty"`
	ExpectedStoreGlobalID   int64    `json:"g,omitempty"`
	MailboxRef              string   `json:"m,omitempty"`
	Offset                  int64    `json:"o,omitempty"`
	PreviousID              string   `json:"q,omitempty"`
	Fingerprint             string   `json:"f,omitempty"`
	StoreUUID               string   `json:"s,omitempty"`
	IndexRevision           string   `json:"r,omitempty"`
	ReceivedAt              int64    `json:"t,omitempty"`
	DateReceived            int64    `json:"d,omitempty"`
	Flags                   uint8    `json:"z,omitempty"`
	RowID                   int64    `json:"i,omitempty"`
}

func encodeCompactArray(version byte, fields []any) ([]byte, error) {
	payload, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	if len(payload) > MaxCompactPayloadBytes-2 {
		return nil, fmt.Errorf("compact payload exceeds %d bytes", MaxCompactPayloadBytes)
	}
	framed := make([]byte, 2, len(payload)+2)
	framed[0], framed[1] = compactPayloadMarker, version
	return append(framed, payload...), nil
}

func decodeCompactArray(payload []byte, version byte, count int) ([]json.RawMessage, error) {
	if len(payload) > MaxCompactPayloadBytes {
		return nil, fmt.Errorf("compact payload exceeds %d bytes", MaxCompactPayloadBytes)
	}
	if len(payload) < 3 || payload[0] != compactPayloadMarker {
		return nil, fmt.Errorf("invalid compact payload framing")
	}
	if payload[1] != version {
		return nil, fmt.Errorf("unsupported compact payload version %d", payload[1])
	}
	if payload[len(payload)-1] != ']' {
		return nil, fmt.Errorf("compact payload has trailing bytes")
	}
	var fields []json.RawMessage
	if err := json.Unmarshal(payload[2:], &fields); err != nil {
		return nil, err
	}
	if len(fields) != count {
		return nil, fmt.Errorf("compact payload has %d fields, expected %d", len(fields), count)
	}
	return fields, nil
}

// EncodeCompactArray frames a bounded compact array payload.
func EncodeCompactArray(version byte, fields []any) ([]byte, error) {
	return encodeCompactArray(version, fields)
}

// DecodeCompactArray validates and decodes a compact array payload.
func DecodeCompactArray(payload []byte, version byte, count int) ([]json.RawMessage, error) {
	return decodeCompactArray(payload, version, count)
}

// CompactPayload is the bounded compact wire shape shared by cursor callers.
type CompactPayload = compactPayload

func encodeCompactJSON(version byte, value compactPayload) ([]byte, error) {
	if err := validateCompactPayload(&value); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(payload) > MaxCompactPayloadBytes-2 {
		return nil, fmt.Errorf("compact payload exceeds %d bytes", MaxCompactPayloadBytes)
	}
	framed := make([]byte, 2, len(payload)+2)
	framed[0], framed[1] = compactPayloadMarker, version
	return append(framed, payload...), nil
}

// EncodeCompactPayload frames a compact payload for a caller-owned token.
func EncodeCompactPayload(value CompactPayload, version byte) ([]byte, error) {
	return encodeCompactJSON(version, value)
}

// DecodeCompactPayload validates and decodes a compact payload.
func DecodeCompactPayload(payload []byte, version byte) (*CompactPayload, error) {
	if len(payload) > MaxCompactPayloadBytes {
		return nil, fmt.Errorf("compact payload exceeds %d bytes", MaxCompactPayloadBytes)
	}
	if len(payload) < 3 {
		return nil, fmt.Errorf("compact payload is truncated")
	}
	if payload[0] != compactPayloadMarker || payload[len(payload)-1] != '}' {
		return nil, fmt.Errorf("invalid compact payload marker")
	}
	if payload[1] != version {
		return nil, fmt.Errorf("unsupported compact payload version %d", payload[1])
	}
	var value compactPayload
	if err := json.Unmarshal(payload[2:], &value); err != nil {
		return nil, err
	}
	if err := validateCompactPayload(&value); err != nil {
		return nil, err
	}
	return &value, nil
}

func validateCompactPayload(value *compactPayload) error {
	if err := validateCompactPath(value.MailboxPath); err != nil {
		return err
	}
	if len(value.AccountID) > MaxCompactStringBytes || len(value.LibraryID) > MaxCompactStringBytes ||
		len(value.ExpectedMessageID) > MaxCompactStringBytes || len(value.ExpectedSubject) > MaxCompactStringBytes ||
		len(value.ExpectedStoreUUID) > MaxCompactStringBytes || len(value.MailboxRef) > MaxCompactStringBytes ||
		len(value.PreviousID) > MaxCompactStringBytes || len(value.Fingerprint) > MaxCompactStringBytes ||
		len(value.StoreUUID) > MaxCompactStringBytes || len(value.IndexRevision) > MaxCompactStringBytes {
		return fmt.Errorf("compact string exceeds %d bytes", MaxCompactStringBytes)
	}
	return nil
}

func validateCompactPath(path []string) error {
	if len(path) > MaxCompactListLength {
		return fmt.Errorf("compact collection length %d exceeds bound %d", len(path), MaxCompactListLength)
	}
	for _, component := range path {
		if len(component) > MaxCompactStringBytes {
			return fmt.Errorf("compact string exceeds %d bytes", MaxCompactStringBytes)
		}
	}
	return nil
}

// CompactEncoder writes a bounded versioned payload using length-prefixed
// strings and unsigned or signed varints.
type CompactEncoder struct {
	data []byte
	err  error
}

// NewCompactEncoder starts a payload with its explicit format version.
//
//go:noinline
func NewCompactEncoder(version byte, capacity int) *CompactEncoder {
	if capacity < 0 || capacity > MaxCompactPayloadBytes-2 {
		capacity = 0
	}
	data := make([]byte, 0, capacity+2)
	data = append(data, compactPayloadMarker, version)
	return &CompactEncoder{data: data}
}

// PutString appends one bounded string.
//
//go:noinline
func (e *CompactEncoder) PutString(value string) {
	if e.err != nil {
		return
	}
	if len(value) > MaxCompactStringBytes {
		e.err = fmt.Errorf("compact string exceeds %d bytes", MaxCompactStringBytes)
		return
	}
	e.PutUvarint(uint64(len(value)))
	e.data = append(e.data, value...)
}

// PutUvarint appends an unsigned varint.
//
//go:noinline
func (e *CompactEncoder) PutUvarint(value uint64) {
	if e.err != nil {
		return
	}
	e.data = binary.AppendUvarint(e.data, value)
}

// PutCount appends a collection length after checking its bound.
//
//go:noinline
func (e *CompactEncoder) PutCount(value int, max int) {
	if e.err != nil {
		return
	}
	if value < 0 || value > max {
		e.err = fmt.Errorf("compact collection length %d exceeds bound %d", value, max)
		return
	}
	e.PutUvarint(uint64(value))
}

// PutVarint appends a signed varint.
//
//go:noinline
func (e *CompactEncoder) PutVarint(value int64) {
	if e.err != nil {
		return
	}
	e.data = binary.AppendVarint(e.data, value)
}

// Bytes returns the complete payload or its first encoding error.
//
//go:noinline
func (e *CompactEncoder) Bytes() ([]byte, error) {
	if e.err != nil {
		return nil, e.err
	}
	if len(e.data) > MaxCompactPayloadBytes {
		return nil, fmt.Errorf("compact payload exceeds %d bytes", MaxCompactPayloadBytes)
	}
	return e.data, nil
}

// CompactDecoder reads a bounded payload after checking its explicit version.
type CompactDecoder struct {
	data  []byte
	index int
}

// NewCompactDecoder validates the payload version and creates a reader.
func NewCompactDecoder(payload []byte, expectedVersion byte) (*CompactDecoder, error) {
	if len(payload) > MaxCompactPayloadBytes {
		return nil, fmt.Errorf("compact payload exceeds %d bytes", MaxCompactPayloadBytes)
	}
	if len(payload) < 2 {
		return nil, fmt.Errorf("compact payload header is truncated")
	}
	if payload[0] != compactPayloadMarker {
		return nil, fmt.Errorf("invalid compact payload marker")
	}
	if payload[1] != expectedVersion {
		return nil, fmt.Errorf("unsupported compact payload version %d", payload[1])
	}
	return &CompactDecoder{data: payload[2:]}, nil
}

// CompactPayloadVersion returns the version when payload framing is present.
//
//go:noinline
func CompactPayloadVersion(payload []byte) (byte, bool) {
	if len(payload) < 2 || payload[0] != compactPayloadMarker {
		return 0, false
	}
	return payload[1], true
}

// Count reads a bounded collection length.
//
//go:noinline
func (d *CompactDecoder) Count(max int) (int, error) {
	value, err := d.Uvarint()
	if err != nil {
		return 0, err
	}
	if max < 0 || value > uint64(max) || value > uint64(len(d.data)-d.index) {
		return 0, fmt.Errorf("compact collection length %d exceeds bound %d", value, max)
	}
	return int(value), nil
}

// String reads one bounded length-prefixed string.
//
//go:noinline
func (d *CompactDecoder) String() (string, error) {
	length, err := d.Uvarint()
	if err != nil {
		return "", err
	}
	if length > MaxCompactStringBytes || length > uint64(len(d.data)-d.index) {
		return "", fmt.Errorf("compact string length %d exceeds bound", length)
	}
	end := d.index + int(length)
	value := string(d.data[d.index:end])
	d.index = end
	return value, nil
}

// Uvarint reads one unsigned varint.
//
//go:noinline
func (d *CompactDecoder) Uvarint() (uint64, error) {
	value, count := binary.Uvarint(d.data[d.index:])
	if count == 0 {
		return 0, fmt.Errorf("truncated compact unsigned varint")
	}
	if count < 0 {
		return 0, fmt.Errorf("overflowed compact unsigned varint")
	}
	d.index += count
	return value, nil
}

// Varint reads one signed varint.
//
//go:noinline
func (d *CompactDecoder) Varint() (int64, error) {
	value, count := binary.Varint(d.data[d.index:])
	if count == 0 {
		return 0, fmt.Errorf("truncated compact signed varint")
	}
	if count < 0 {
		return 0, fmt.Errorf("overflowed compact signed varint")
	}
	d.index += count
	return value, nil
}

// Done rejects trailing bytes after the expected fields.
//
//go:noinline
func (d *CompactDecoder) Done() error {
	if d.index != len(d.data) {
		return fmt.Errorf("compact payload has %d trailing bytes", len(d.data)-d.index)
	}
	return nil
}

// EncodeToken wraps a bounded payload in the existing opaque token prefix.
func EncodeToken(prefix string, payload []byte) (string, error) {
	if len(payload) > MaxCompactPayloadBytes {
		return "", fmt.Errorf("token payload exceeds %d bytes", MaxCompactPayloadBytes)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(payload), nil
}

// EncodeCompactToken wraps a payload already bounded by a compact encoder.
func EncodeCompactToken(prefix string, payload []byte) string {
	return prefix + base64.RawURLEncoding.EncodeToString(payload)
}

// EncodeCompactTokenPayload encodes and frames a compact token in one step.
func EncodeCompactTokenPayload(prefix string, value *CompactPayload, version byte) (string, error) {
	if value == nil {
		return "", errCompactPayloadNil
	}
	if err := validateCompactPayload(value); err != nil {
		return "", err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if len(payload) > MaxCompactPayloadBytes-2 {
		return "", fmt.Errorf("compact payload exceeds %d bytes", MaxCompactPayloadBytes)
	}
	framed := make([]byte, 2, len(payload)+2)
	framed[0], framed[1] = compactPayloadMarker, version
	return prefix + base64.RawURLEncoding.EncodeToString(append(framed, payload...)), nil
}

// DecodeTokenPayload checks the prefix, bounds base64 input, and decodes it.
func DecodeTokenPayload(prefix string, value string) ([]byte, error) {
	kind := strings.TrimSuffix(prefix, "_")
	if len(value) < len(prefix) || value[:len(prefix)] != prefix {
		return nil, fmt.Errorf("invalid %s token prefix", kind)
	}
	encoded := value[len(prefix):]
	maxEncoded := ((MaxCompactPayloadBytes + 2) / 3) * 4
	if len(encoded) > maxEncoded {
		return nil, fmt.Errorf("%s token exceeds %d bytes", kind, MaxCompactPayloadBytes)
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode %s token: %w", kind, err)
	}
	if len(payload) > MaxCompactPayloadBytes {
		return nil, fmt.Errorf("%s token exceeds %d bytes", kind, MaxCompactPayloadBytes)
	}
	return payload, nil
}
