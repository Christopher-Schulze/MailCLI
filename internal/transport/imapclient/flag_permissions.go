package imapclient

import (
	"errors"
	"strings"
)

type flagPermissions struct {
	present bool
	flags   []string
}

type flagChanges struct {
	add    []string
	remove []string
}

func (changes flagChanges) touchesJunk() bool {
	for _, flags := range [][]string{changes.add, changes.remove} {
		if containsFlag(flags, "$Junk") || containsFlag(flags, "$NotJunk") {
			return true
		}
	}
	return false
}

func (changes flagChanges) pending(actual []string) flagChanges {
	var needed flagChanges
	for _, flag := range changes.add {
		if !containsFlag(actual, flag) {
			needed.add = append(needed.add, flag)
		}
	}
	for _, flag := range changes.remove {
		if containsFlag(actual, flag) {
			needed.remove = append(needed.remove, flag)
		}
	}
	return needed
}

func (permissions flagPermissions) unsupported(changes flagChanges) string {
	// RFC 9051 section 6.3.2 defines the default when the code is omitted.
	if !permissions.present {
		return ""
	}
	for _, flags := range [][]string{changes.add, changes.remove} {
		for _, flag := range flags {
			if containsFlag(permissions.flags, flag) {
				continue
			}
			if !strings.HasPrefix(flag, "\\") && containsFlag(permissions.flags, "\\*") {
				continue
			}
			return flag
		}
	}
	return ""
}

func (permissions *flagPermissions) observeCode(code string) error {
	if strings.ContainsAny(code, "\t\r\n") {
		return errors.New("IMAP response code contains invalid whitespace")
	}
	name, value, _ := strings.Cut(code, " ")
	if !strings.EqualFold(name, "PERMANENTFLAGS") {
		return nil
	}
	flags, err := parseFlagListValue(value, true)
	if err != nil {
		return err
	}
	permissions.present, permissions.flags = true, flags
	return nil
}

func (info *selectInfo) observeFlagCode(code string) error {
	if err := info.permissions.observeCode(code); err != nil {
		return err
	}
	fields := strings.Fields(code)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "UIDVALIDITY") {
		return nil
	}
	if len(fields) != 2 {
		return errors.New("IMAP SELECT contains malformed UIDVALIDITY")
	}
	value, err := parsePositiveUIDValue(fields[1])
	if err != nil {
		return err
	}
	if info.uidvalidity != 0 && info.uidvalidity != value {
		return errors.New("IMAP SELECT contains contradictory UIDVALIDITY")
	}
	info.uidvalidity = value
	return nil
}

// Only the bracketed code immediately following a status is authoritative.
// FETCH bodies and bracketed phrases in human-readable text are not codes.
func flagResponseCode(line, tag string) (string, error) {
	prefix, rest, found := strings.Cut(line, " ")
	if !found || (prefix != "*" && prefix != tag) {
		return "", nil
	}
	status, rest, _ := strings.Cut(strings.TrimSpace(rest), " ")
	if strings.EqualFold(status, "BYE") {
		return "", errors.New("IMAP server closed the selected mailbox session")
	}
	if !strings.EqualFold(status, "OK") && !strings.EqualFold(status, "NO") && !strings.EqualFold(status, "BAD") {
		return "", nil
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "[") {
		return "", nil
	}
	code, present := bracketedResponseCode(rest)
	if !present {
		return "", errors.New("IMAP status contains a malformed response code")
	}
	return code, nil
}
