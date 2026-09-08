package mailstore

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maximumMailboxInfoBytes = int64(1 << 20)

// mailboxUIDValidity reads the mailbox-local server generation marker. Mail
// stores may omit Info.plist while a mailbox is being rebuilt, so absence is
// represented by zero and lets the caller try another independently verified
// identity path.
func (s *Store) mailboxUIDValidity(ctx context.Context, location mailboxLocation) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	path, err := mailboxInfoPath(s.versionRoot, location)
	if err != nil {
		return 0, err
	}
	file, info, err := openRegularPath(s.versionDirectory, s.versionRoot, path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("open mailbox Info.plist: %w", err)
	}
	if info.Size() < 1 || info.Size() > maximumMailboxInfoBytes {
		resultErr := mailboxInfoMalformedError("mailbox Info.plist is not a bounded regular file", nil)
		joinCloseError(&resultErr, file, "mailbox Info.plist")
		return 0, resultErr
	}
	defer func() { _ = file.Close() }()
	validity, err := parseMailboxInfoXML(io.LimitReader(file, maximumMailboxInfoBytes+1))
	if err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return validity, nil
}

func mailboxInfoPath(root string, location mailboxLocation) (string, error) {
	if root == "" || location.AccountID == "" || len(location.RawPath) == 0 {
		return "", operationError("unsafe_message_source", "mailbox Info.plist identity is invalid")
	}
	parts := []string{root, location.AccountID}
	for _, segment := range location.RawPath {
		if err := validatePathSegment(segment); err != nil {
			return "", err
		}
		parts = append(parts, segment+".mbox")
	}
	path := filepath.Join(append(parts, "Info.plist")...)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", operationError("unsafe_message_source", "mailbox Info.plist escapes the Mail store")
	}
	return path, nil
}

func parseMailboxInfoXML(reader io.Reader) (uint32, error) {
	limited := &io.LimitedReader{R: reader, N: maximumMailboxInfoBytes + 1}
	decoder := xml.NewDecoder(limited)
	root, err := nextPlistStart(decoder)
	if err != nil {
		return 0, mailboxInfoParseError(err)
	}
	if root.Name.Local != "plist" {
		return 0, mailboxInfoMalformedError("parse mailbox Info.plist: expected plist root", nil)
	}
	dictionary, err := nextPlistStart(decoder)
	if err != nil {
		return 0, mailboxInfoParseError(err)
	}
	if dictionary.Name.Local != "dict" {
		return 0, mailboxInfoMalformedError("parse mailbox Info.plist: expected root dictionary", nil)
	}
	var validity uint32
	seen := false
	for {
		key, value, done, err := nextPlistPair(decoder)
		if err != nil {
			return 0, mailboxInfoParseError(err)
		}
		if done {
			break
		}
		if key != "UIDVALIDITY" {
			if err := decoder.Skip(); err != nil {
				return 0, mailboxInfoParseError(err)
			}
			continue
		}
		if seen {
			return 0, mailboxInfoMalformedError("mailbox Info.plist contains duplicate UIDVALIDITY", nil)
		}
		integer, err := decodePlistInteger(decoder, value)
		if err != nil || integer < 1 || uint64(integer) > uint64(^uint32(0)) {
			if err == nil {
				err = errors.New("UIDVALIDITY must be a positive 32-bit integer")
			}
			return 0, mailboxInfoMalformedError("parse mailbox Info.plist UIDVALIDITY", err)
		}
		validity = uint32(integer)
		seen = true
	}
	if !seen {
		return 0, mailboxInfoMalformedError("mailbox Info.plist has no UIDVALIDITY", nil)
	}
	if err := consumeMailboxCacheEnd(decoder, "plist"); err != nil {
		return 0, mailboxInfoParseError(err)
	}
	if err := requireMailboxCacheEOF(decoder); err != nil {
		return 0, mailboxInfoParseError(err)
	}
	if limited.N == 0 {
		return 0, mailboxInfoMalformedError(
			fmt.Sprintf("mailbox Info.plist exceeds %d bytes", maximumMailboxInfoBytes), nil,
		)
	}
	return validity, nil
}

func mailboxInfoMalformedError(message string, cause error) error {
	return operationErrorWithCause("mailbox_info_malformed", message, cause)
}

func mailboxInfoParseError(err error) error {
	if err == nil {
		return nil
	}
	var typed *Error
	if errors.As(err, &typed) && typed.Code == "mailbox_info_malformed" {
		return err
	}
	return mailboxInfoMalformedError("parse mailbox Info.plist: "+err.Error(), err)
}
