package mailstore

import (
	"fmt"
	"net/url"
	"strings"
)

type mailboxLocation struct {
	Scheme      string
	AccountID   string
	RawPath     []string
	VisiblePath []string
}

func (m mailboxLocation) rootKey() string {
	return m.Scheme + "://" + strings.ToUpper(m.AccountID)
}

func parseAccountRoot(value string) (mailboxLocation, error) {
	location, escapedPath, err := splitMailboxURL(value)
	if err != nil {
		return mailboxLocation{}, fmt.Errorf("parse account root URL: %w", err)
	}
	if escapedPath != "/" {
		return mailboxLocation{}, fmt.Errorf("account root URL must end at its authority")
	}
	return location, nil
}

func parseMailboxURL(value string) (mailboxLocation, error) {
	location, escapedPath, err := splitMailboxURL(value)
	if err != nil {
		return mailboxLocation{}, fmt.Errorf("parse mailbox URL: %w", err)
	}
	escapedPath = strings.TrimPrefix(escapedPath, "/")
	if escapedPath == "" || strings.HasSuffix(escapedPath, "/") {
		return mailboxLocation{}, fmt.Errorf("mailbox URL has an empty path")
	}
	segments := strings.Split(escapedPath, "/")
	location.RawPath = make([]string, 0, len(segments))
	for _, encoded := range segments {
		segment, err := url.PathUnescape(encoded)
		if err != nil {
			return mailboxLocation{}, fmt.Errorf("decode mailbox URL segment: %w", err)
		}
		if err := validatePathSegment(segment); err != nil {
			return mailboxLocation{}, err
		}
		location.RawPath = append(location.RawPath, segment)
	}
	location.VisiblePath = append([]string(nil), location.RawPath...)
	if len(location.VisiblePath) > 1 && location.VisiblePath[0] == "[Gmail]" {
		location.VisiblePath = location.VisiblePath[1:]
	}
	return location, nil
}

func splitMailboxURL(value string) (mailboxLocation, string, error) {
	separator := strings.Index(value, "://")
	if separator < 1 {
		return mailboxLocation{}, "", fmt.Errorf("mailbox URL has no scheme separator")
	}
	scheme := ""
	switch {
	case strings.EqualFold(value[:separator], "imap"):
		scheme = "imap"
	case strings.EqualFold(value[:separator], "local"):
		scheme = "local"
	default:
		return mailboxLocation{}, "", fmt.Errorf("unsupported mailbox URL scheme %q", value[:separator])
	}
	remainder := value[separator+3:]
	pathIndex := strings.IndexByte(remainder, '/')
	if pathIndex < 0 {
		return mailboxLocation{}, "", fmt.Errorf("mailbox URL has no path")
	}
	authority := remainder[:pathIndex]
	escapedPath := remainder[pathIndex:]
	if !validUUID(authority) {
		return mailboxLocation{}, "", fmt.Errorf("mailbox URL authority is not a UUID")
	}
	if strings.ContainsAny(escapedPath, "?#") || containsASCIIControl(value) {
		return mailboxLocation{}, "", fmt.Errorf("mailbox URL contains unsupported data")
	}
	return mailboxLocation{Scheme: scheme, AccountID: strings.ToUpper(authority)}, escapedPath, nil
}

func validatePathSegment(value string) error {
	if value == "" || value == "." || value == ".." {
		return fmt.Errorf("mailbox URL contains an unsafe path segment")
	}
	if strings.ContainsAny(value, "/\x00") || containsASCIIControl(value) {
		return fmt.Errorf("mailbox URL segment escapes its directory")
	}
	return nil
}

// unsafePathSegmentError marks a cache path component that escapes its
// directory. Degrade the account instead of failing the whole listing.
func unsafePathSegmentError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unsafe path segment")
}

func containsASCIIControl(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] < 0x20 || value[index] == 0x7f {
			return true
		}
	}
	return false
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') &&
			(character < 'A' || character > 'F') {
			return false
		}
	}
	return true
}
