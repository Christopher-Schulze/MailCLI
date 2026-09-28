package mailref

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// BinaryFormatVersion is the version of the binary account, mailbox and message refs.
const BinaryFormatVersion = 3

const binaryPayloadMarker byte = 0xd8

const (
	subjectHashBytes = 4
	wideVarintBytes  = 8
	uuidRawBytes     = 16
	uuidTextLength   = 36
)

const (
	flagAccountUUID uint64 = 1 << iota
	flagStoreIdentity
	flagLibraryNumeric
	flagIMAPUID
	flagIMAPMailbox
	flagSubjectHash
	flagWideStoreMessageID
	flagMessageID
	flagIMAPUIDValidity
	flagStoreUUIDText
)

const (
	accountFlagMask = flagAccountUUID
	mailboxFlagMask = flagAccountUUID
	messageFlagMask = flagAccountUUID | flagStoreIdentity | flagLibraryNumeric | flagIMAPUID | flagIMAPMailbox |
		flagSubjectHash | flagWideStoreMessageID | flagMessageID | flagIMAPUIDValidity | flagStoreUUIDText
)

// SubjectHash is the 4-byte subject fingerprint carried by message refs; 0 means no subject check.
func SubjectHash(subject string) uint32 {
	sum := sha256.Sum256([]byte(subject))
	hash := binary.BigEndian.Uint32(sum[:subjectHashBytes])
	if hash == 0 {
		return 1
	}
	return hash
}

// HasSubjectCheck reports whether the ref can verify the subject of the message it names.
func (m Message) HasSubjectCheck() bool {
	return m.ExpectedSubject != "" || m.ExpectedSubjectHash != 0
}

// SubjectMatches compares a store subject with the subject identity recorded in the ref.
func (m Message) SubjectMatches(subject string) bool {
	if m.ExpectedSubject != "" {
		return subject == m.ExpectedSubject
	}
	return m.ExpectedSubjectHash == 0 || SubjectHash(subject) == m.ExpectedSubjectHash
}

func isBinaryPayload(payload []byte) bool {
	return len(payload) >= 2 && payload[0] == binaryPayloadMarker
}

type binaryWriter struct {
	buffer []byte
}

func newBinaryWriter(flags uint64) *binaryWriter {
	writer := &binaryWriter{buffer: []byte{binaryPayloadMarker}}
	writer.uvarint(flags)
	return writer
}

func (w *binaryWriter) uvarint(value uint64) {
	w.buffer = binary.AppendUvarint(w.buffer, value)
}

func (w *binaryWriter) varint(value int64) {
	w.buffer = binary.AppendVarint(w.buffer, value)
}

func (w *binaryWriter) text(value string) {
	w.uvarint(uint64(len(value)))
	w.buffer = append(w.buffer, value...)
}

func (w *binaryWriter) identifier(value string, raw [uuidRawBytes]byte, isUUID bool) {
	if isUUID {
		w.buffer = append(w.buffer, raw[:]...)
		return
	}
	w.text(value)
}

func (w *binaryWriter) path(path []string) {
	w.uvarint(uint64(len(path)))
	for _, component := range path {
		w.text(component)
	}
}

type binaryReader struct {
	data []byte
	err  error
}

func newBinaryReader(payload []byte) (*binaryReader, uint64) {
	reader := &binaryReader{data: payload[1:]}
	return reader, reader.uvarint()
}

func (r *binaryReader) fail(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf(format, args...)
	}
}

func (r *binaryReader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	value, length := binary.Uvarint(r.data)
	if length <= 0 {
		r.fail("binary ref has a truncated or oversized integer")
		return 0
	}
	r.data = r.data[length:]
	return value
}

func (r *binaryReader) varint() int64 {
	if r.err != nil {
		return 0
	}
	value, length := binary.Varint(r.data)
	if length <= 0 {
		r.fail("binary ref has a truncated or oversized integer")
		return 0
	}
	r.data = r.data[length:]
	return value
}

func (r *binaryReader) take(length uint64) []byte {
	if r.err != nil {
		return nil
	}
	if length > uint64(len(r.data)) {
		r.fail("binary ref is truncated")
		return nil
	}
	chunk := r.data[:length]
	r.data = r.data[length:]
	return chunk
}

func (r *binaryReader) text() string {
	length := r.uvarint()
	if length > MaxCompactStringBytes {
		r.fail("compact string exceeds %d bytes", MaxCompactStringBytes)
		return ""
	}
	chunk := r.take(length)
	if r.err == nil && !utf8.Valid(chunk) {
		r.fail("binary ref carries a string that is not valid UTF-8")
	}
	return string(chunk)
}

func (r *binaryReader) identifier(isUUID bool) string {
	if !isUUID {
		return r.text()
	}
	chunk := r.take(uuidRawBytes)
	if r.err != nil {
		return ""
	}
	return formatUUID(chunk)
}

func (r *binaryReader) path() []string {
	count := r.uvarint()
	if count > MaxCompactListLength {
		r.fail("compact collection length %d exceeds bound %d", count, MaxCompactListLength)
		return nil
	}
	path := make([]string, 0, count)
	for range count {
		path = append(path, r.text())
		if r.err != nil {
			return nil
		}
	}
	return path
}

func (r *binaryReader) finish() error {
	if r.err == nil && len(r.data) != 0 {
		r.fail("binary ref has %d trailing bytes", len(r.data))
	}
	return r.err
}

func parseCanonicalUUID(value string) ([uuidRawBytes]byte, bool) {
	var raw [uuidRawBytes]byte
	if len(value) != uuidTextLength || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return raw, false
	}
	compact := value[:8] + value[9:13] + value[14:18] + value[19:23] + value[24:]
	if compact != strings.ToUpper(compact) {
		return raw, false
	}
	decoded, err := hex.DecodeString(compact)
	if err != nil || len(decoded) != uuidRawBytes {
		return raw, false
	}
	copy(raw[:], decoded)
	return raw, true
}

func formatUUID(raw []byte) string {
	text := strings.ToUpper(hex.EncodeToString(raw))
	return text[:8] + "-" + text[8:12] + "-" + text[12:16] + "-" + text[16:20] + "-" + text[20:]
}

func sanitizeUTF8(value string) string {
	if utf8.ValidString(value) {
		return value
	}
	return string([]rune(value))
}

func sanitizePath(path []string) []string {
	clean := make([]string, len(path))
	for index, component := range path {
		clean[index] = sanitizeUTF8(component)
	}
	return clean
}

func validateBinaryBounds(path []string, texts ...string) error {
	if err := validateCompactPath(path); err != nil {
		return err
	}
	for _, value := range texts {
		if len(value) > MaxCompactStringBytes {
			return fmt.Errorf("compact string exceeds %d bytes", MaxCompactStringBytes)
		}
	}
	return nil
}

func encodeBinaryAccount(accountID string) ([]byte, error) {
	accountID = sanitizeUTF8(accountID)
	if err := validateBinaryBounds(nil, accountID); err != nil {
		return nil, err
	}
	raw, isUUID := parseCanonicalUUID(accountID)
	var flags uint64
	if isUUID {
		flags |= flagAccountUUID
	}
	writer := newBinaryWriter(flags)
	writer.identifier(accountID, raw, isUUID)
	return writer.buffer, nil
}

func decodeBinaryAccount(payload []byte) (*Account, error) {
	reader, flags := newBinaryReader(payload)
	if flags&^accountFlagMask != 0 {
		return nil, fmt.Errorf("binary account ref has unknown flags 0x%x", flags)
	}
	account := &Account{Version: BinaryFormatVersion, AccountID: reader.identifier(flags&flagAccountUUID != 0)}
	if err := reader.finish(); err != nil {
		return nil, err
	}
	canonical, err := encodeBinaryAccount(account.AccountID)
	if err != nil || !bytes.Equal(canonical, payload) {
		return nil, errNoncanonicalBinary
	}
	return account, nil
}

func encodeBinaryMailbox(accountID string, path []string) ([]byte, error) {
	accountID, path = sanitizeUTF8(accountID), sanitizePath(path)
	if err := validateBinaryBounds(path, accountID); err != nil {
		return nil, err
	}
	raw, isUUID := parseCanonicalUUID(accountID)
	var flags uint64
	if isUUID {
		flags |= flagAccountUUID
	}
	writer := newBinaryWriter(flags)
	writer.identifier(accountID, raw, isUUID)
	writer.path(path)
	return writer.buffer, nil
}

func decodeBinaryMailbox(payload []byte) (*Mailbox, error) {
	reader, flags := newBinaryReader(payload)
	if flags&^mailboxFlagMask != 0 {
		return nil, fmt.Errorf("binary mailbox ref has unknown flags 0x%x", flags)
	}
	mailbox := &Mailbox{Version: BinaryFormatVersion, AccountID: reader.identifier(flags&flagAccountUUID != 0)}
	mailbox.Path = reader.path()
	if err := reader.finish(); err != nil {
		return nil, err
	}
	canonical, err := encodeBinaryMailbox(mailbox.AccountID, mailbox.Path)
	if err != nil || !bytes.Equal(canonical, payload) {
		return nil, errNoncanonicalBinary
	}
	return mailbox, nil
}

var errNoncanonicalBinary = errors.New("binary ref is not in canonical form")

func encodeBinaryMessage(ref Message) ([]byte, error) {
	ref.AccountID, ref.MailboxPath = sanitizeUTF8(ref.AccountID), sanitizePath(ref.MailboxPath)
	ref.LibraryID, ref.ExpectedMessageID = sanitizeUTF8(ref.LibraryID), sanitizeUTF8(ref.ExpectedMessageID)
	ref.ExpectedStoreUUID = sanitizeUTF8(ref.ExpectedStoreUUID)
	if err := validateBinaryBounds(ref.MailboxPath, ref.AccountID, ref.LibraryID, ref.ExpectedMessageID, ref.ExpectedStoreUUID); err != nil {
		return nil, err
	}
	accountRaw, accountIsUUID := parseCanonicalUUID(ref.AccountID)
	storeRaw, storeIsUUID := parseCanonicalUUID(ref.ExpectedStoreUUID)
	libraryNumber, libraryIsNumber := canonicalDecimal(ref.LibraryID)
	subjectHash := ref.ExpectedSubjectHash
	if ref.ExpectedSubject != "" {
		subjectHash = SubjectHash(ref.ExpectedSubject)
	}
	flags := messageFlags(ref, accountIsUUID, storeIsUUID, libraryIsNumber, subjectHash)
	writer := newBinaryWriter(flags)
	writer.identifier(ref.AccountID, accountRaw, accountIsUUID)
	writer.path(ref.MailboxPath)
	if libraryIsNumber {
		writer.uvarint(libraryNumber)
	} else {
		writer.text(ref.LibraryID)
	}
	if flags&flagMessageID != 0 {
		writer.text(ref.ExpectedMessageID)
	}
	if flags&flagIMAPUID != 0 {
		writer.uvarint(uint64(ref.ExpectedIMAPUID))
	}
	if flags&flagIMAPUIDValidity != 0 {
		writer.uvarint(uint64(ref.ExpectedIMAPUIDValidity))
	}
	if flags&flagIMAPMailbox != 0 {
		writer.uvarint(uint64(ref.ExpectedIMAPMailboxID))
	}
	if flags&flagSubjectHash != 0 {
		writer.buffer = binary.BigEndian.AppendUint32(writer.buffer, subjectHash)
	}
	if flags&flagStoreIdentity != 0 {
		writer.identifier(ref.ExpectedStoreUUID, storeRaw, storeIsUUID)
		writer.varint(ref.ExpectedStoreMailboxID)
		if flags&flagWideStoreMessageID != 0 {
			writer.buffer = binary.BigEndian.AppendUint64(writer.buffer, uint64(ref.ExpectedStoreMessageID))
		} else {
			writer.varint(ref.ExpectedStoreMessageID)
		}
		writer.varint(ref.ExpectedStoreGlobalID)
	}
	return writer.buffer, nil
}

func messageFlags(ref Message, accountIsUUID bool, storeIsUUID bool, libraryIsNumber bool, subjectHash uint32) uint64 {
	var flags uint64
	set := func(condition bool, flag uint64) {
		if condition {
			flags |= flag
		}
	}
	set(accountIsUUID, flagAccountUUID)
	set(ref.ExpectedStoreUUID != "", flagStoreIdentity)
	set(ref.ExpectedStoreUUID != "" && !storeIsUUID, flagStoreUUIDText)
	set(libraryIsNumber, flagLibraryNumeric)
	set(ref.ExpectedMessageID != "", flagMessageID)
	set(ref.ExpectedIMAPUID != 0, flagIMAPUID)
	set(ref.ExpectedIMAPUIDValidity != 0, flagIMAPUIDValidity)
	set(ref.ExpectedIMAPMailboxID != 0, flagIMAPMailbox)
	set(subjectHash != 0, flagSubjectHash)
	set(ref.ExpectedStoreUUID != "" && varintLength(ref.ExpectedStoreMessageID) > wideVarintBytes, flagWideStoreMessageID)
	return flags
}

func varintLength(value int64) int {
	var scratch [binary.MaxVarintLen64]byte
	return binary.PutVarint(scratch[:], value)
}

func canonicalDecimal(value string) (uint64, bool) {
	number, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(number, 10) != value {
		return 0, false
	}
	return number, true
}

func decodeBinaryMessage(payload []byte) (*Message, error) {
	reader, flags := newBinaryReader(payload)
	if flags&^messageFlagMask != 0 {
		return nil, fmt.Errorf("binary message ref has unknown flags 0x%x", flags)
	}
	message := &Message{Version: BinaryFormatVersion}
	message.AccountID = reader.identifier(flags&flagAccountUUID != 0)
	message.MailboxPath = reader.path()
	if flags&flagLibraryNumeric != 0 {
		message.LibraryID = strconv.FormatUint(reader.uvarint(), 10)
	} else {
		message.LibraryID = reader.text()
	}
	if flags&flagMessageID != 0 {
		message.ExpectedMessageID = reader.text()
	}
	if flags&flagIMAPUID != 0 {
		message.ExpectedIMAPUID = reader.uint32Value()
	}
	if flags&flagIMAPUIDValidity != 0 {
		message.ExpectedIMAPUIDValidity = reader.uint32Value()
	}
	if flags&flagIMAPMailbox != 0 {
		message.ExpectedIMAPMailboxID = reader.nonNegativeInt64()
	}
	if flags&flagSubjectHash != 0 {
		message.ExpectedSubjectHash = binary.BigEndian.Uint32(padded(reader.take(subjectHashBytes), subjectHashBytes))
	}
	if flags&flagStoreIdentity != 0 {
		message.ExpectedStoreUUID = reader.identifier(flags&flagStoreUUIDText == 0)
		message.ExpectedStoreMailboxID = reader.varint()
		if flags&flagWideStoreMessageID != 0 {
			message.ExpectedStoreMessageID = int64(binary.BigEndian.Uint64(padded(reader.take(wideVarintBytes), wideVarintBytes)))
		} else {
			message.ExpectedStoreMessageID = reader.varint()
		}
		message.ExpectedStoreGlobalID = reader.varint()
	}
	if err := reader.finish(); err != nil {
		return nil, err
	}
	canonical, err := encodeBinaryMessage(*message)
	if err != nil || !bytes.Equal(canonical, payload) {
		return nil, errNoncanonicalBinary
	}
	return message, nil
}

func padded(chunk []byte, length int) []byte {
	if len(chunk) == length {
		return chunk
	}
	return make([]byte, length)
}

func (r *binaryReader) uint32Value() uint32 {
	value := r.uvarint()
	if value > math.MaxUint32 {
		r.fail("binary ref integer exceeds 32 bits")
		return 0
	}
	return uint32(value)
}

func (r *binaryReader) nonNegativeInt64() int64 {
	value := r.uvarint()
	if value > math.MaxInt64 {
		r.fail("binary ref integer exceeds 63 bits")
		return 0
	}
	return int64(value)
}
