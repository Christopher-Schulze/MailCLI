package mailref

import (
	"bytes"
	"fmt"
	"strings"
)

// Server names one message on the IMAP server by account, mailbox path,
// UIDVALIDITY and UID. It carries no local store identity, so only commands
// that read over IMAP accept it.
type Server struct {
	Version     int
	AccountID   string
	MailboxPath []string
	UIDValidity uint32
	UID         uint32
}

const serverPrefix = "srv_"

// IsServerRef reports whether a token is a server ref.
func IsServerRef(value string) bool {
	return strings.HasPrefix(value, serverPrefix)
}

func EncodeServer(ref Server) (string, error) {
	if ref.AccountID == "" || len(ref.MailboxPath) == 0 || ref.UIDValidity == 0 || ref.UID == 0 {
		return "", fmt.Errorf("server ref needs an account, a mailbox path, a UIDVALIDITY and a UID")
	}
	payload, err := encodeBinaryServer(ref)
	if err != nil {
		return "", fmt.Errorf("encode server ref: %w", err)
	}
	return encodeBinaryToken(serverPrefix, payload, "server ref")
}

func DecodeServer(value string) (Server, error) {
	payload, err := DecodeTokenPayload(serverPrefix, value)
	if err != nil {
		return Server{}, err
	}
	if !isBinaryPayload(payload) {
		return Server{}, &invalidReferenceError{err: fmt.Errorf("server ref is not a binary payload")}
	}
	ref, err := decodeBinaryServer(payload)
	if err != nil {
		return Server{}, &invalidReferenceError{err: err}
	}
	return ref, nil
}

func encodeBinaryServer(ref Server) ([]byte, error) {
	accountID, path := sanitizeUTF8(ref.AccountID), sanitizePath(ref.MailboxPath)
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
	writer.uvarint(uint64(ref.UIDValidity))
	writer.uvarint(uint64(ref.UID))
	return writer.buffer, nil
}

func decodeBinaryServer(payload []byte) (Server, error) {
	reader, flags := newBinaryReader(payload)
	if flags&^accountFlagMask != 0 {
		return Server{}, fmt.Errorf("binary server ref has unknown flags 0x%x", flags)
	}
	ref := Server{Version: BinaryFormatVersion}
	ref.AccountID = reader.identifier(flags&flagAccountUUID != 0)
	ref.MailboxPath = reader.path()
	ref.UIDValidity = reader.uint32Value()
	ref.UID = reader.uint32Value()
	if err := reader.finish(); err != nil {
		return Server{}, err
	}
	if ref.AccountID == "" || len(ref.MailboxPath) == 0 || ref.UIDValidity == 0 || ref.UID == 0 {
		return Server{}, fmt.Errorf("server ref has an empty account, mailbox path, UIDVALIDITY or UID")
	}
	canonical, err := encodeBinaryServer(ref)
	if err != nil || !bytes.Equal(canonical, payload) {
		return Server{}, errNoncanonicalBinary
	}
	return ref, nil
}
