package transport

import (
	"fmt"
	"sort"
	"strings"
)

type mailboxRole string

const (
	mailboxRoleSent    mailboxRole = "Sent"
	mailboxRoleTrash   mailboxRole = "Trash"
	mailboxRoleJunk    mailboxRole = "Junk"
	mailboxRoleDrafts  mailboxRole = "Drafts"
	mailboxRoleArchive mailboxRole = "Archive"
)

var mailboxRoleAliases = map[mailboxRole][]string{
	mailboxRoleSent: {
		"sent", "sent mail", "sent messages", "gesendet", "gesendete elemente",
	},
	mailboxRoleTrash: {
		"trash", "bin", "deleted", "deleted messages", "papierkorb", "gelöschte elemente",
	},
	mailboxRoleJunk:    {"junk", "spam"},
	mailboxRoleDrafts:  {"drafts", "entwürfe"},
	mailboxRoleArchive: {"archive", "archiv"},
}

var mailboxRoleFlags = map[mailboxRole]string{
	mailboxRoleSent:    "\\Sent",
	mailboxRoleTrash:   "\\Trash",
	mailboxRoleJunk:    "\\Junk",
	mailboxRoleDrafts:  "\\Drafts",
	mailboxRoleArchive: "\\Archive",
}

var mailboxRoleOrder = []mailboxRole{
	mailboxRoleSent, mailboxRoleTrash, mailboxRoleJunk, mailboxRoleDrafts, mailboxRoleArchive,
}

var mailboxRoleFallbacks = map[mailboxRole][]string{
	mailboxRoleSent: {
		"Sent", "Sent Mail", "Sent Messages", "Gesendet", "Gesendete Elemente", "[Gmail]/Sent Mail",
	},
	mailboxRoleTrash: {
		"Trash", "Bin", "Deleted", "Deleted Messages", "[Gmail]/Trash", "[Gmail]/Papierkorb",
		"Papierkorb", "Gelöschte Elemente", "INBOX.Trash",
	},
	mailboxRoleJunk: {
		"Junk", "Spam", "[Gmail]/Spam",
	},
	mailboxRoleDrafts: {
		"Drafts", "Entwürfe", "[Gmail]/Drafts",
	},
	mailboxRoleArchive: {
		"Archive", "Archiv", "[Gmail]/All Mail",
	},
}

// ResolveMailboxPath maps an account-relative local mailbox path to exactly
// one server mailbox. Exact path matches are preferred over role heuristics;
// every ambiguous candidate set is rejected with its sorted evidence.
func ResolveMailboxPath(mailboxes []MailboxInfo, path []string) (string, error) {
	if len(path) == 0 || (len(path) == 1 && strings.EqualFold(path[0], "INBOX")) {
		return resolveInboxMailbox(mailboxes)
	}
	if invalidMailboxPath(path) {
		return "", &TransportError{
			Code:    CodeIMAPInvalidValue,
			Message: "mailbox path contains an empty segment",
		}
	}

	if exact := exactMailboxPathMatches(mailboxes, path); len(exact) > 0 {
		return chooseMailbox(exact, "exact account-relative mailbox path", CodeIMAPMailboxNotFound)
	}

	if role, ok := mailboxRoleForPath(path); ok {
		flagged := matchingMailboxNames(mailboxes, func(mailbox MailboxInfo) bool {
			return hasMailboxFlag(mailbox, mailboxRoleFlags[role])
		})
		if len(flagged) > 0 {
			return chooseMailbox(flagged, string(role)+" special-use mailbox", CodeIMAPMailboxNotFound)
		}
		fallbacks := mailboxRoleFallbacks[role]
		if heuristic := matchingMailboxNames(mailboxes, func(mailbox MailboxInfo) bool {
			return containsMailboxName(fallbacks, mailbox)
		}); len(heuristic) > 0 {
			return chooseMailbox(heuristic, string(role)+" localized mailbox name", CodeIMAPMailboxNotFound)
		}
	}

	if len(path) == 1 {
		leaf := path[0]
		if heuristic := matchingMailboxNames(mailboxes, func(mailbox MailboxInfo) bool {
			return mailboxLegacy(mailbox) && strings.EqualFold(mailboxLeaf(mailboxWireName(mailbox)), leaf)
		}); len(heuristic) > 0 {
			return chooseMailbox(heuristic, "mailbox leaf name", CodeIMAPMailboxNotFound)
		}
	}

	return "", &TransportError{
		Code: CodeIMAPMailboxNotFound,
		Message: fmt.Sprintf(
			"no IMAP mailbox matches account-relative path %q; use an exact mailbox path from the mailbox listing",
			strings.Join(path, "/"),
		),
	}
}

// ResolveSentMailbox selects one Sent mailbox and preserves ambiguity evidence.
func ResolveSentMailbox(mailboxes []MailboxInfo) (string, error) {
	return resolveRoleMailbox(mailboxes, mailboxRoleSent, CodeIMAPSentMailboxNotFound)
}

// ResolveTrashMailbox selects one Trash mailbox and preserves ambiguity evidence.
func ResolveTrashMailbox(mailboxes []MailboxInfo) (string, error) {
	return resolveRoleMailbox(mailboxes, mailboxRoleTrash, CodeIMAPMailboxNotFound)
}

func resolveRoleMailbox(mailboxes []MailboxInfo, role mailboxRole, missingCode string) (string, error) {
	flag := mailboxRoleFlags[role]
	flagged := matchingMailboxNames(mailboxes, func(mailbox MailboxInfo) bool {
		return hasMailboxFlag(mailbox, flag)
	})
	if len(flagged) > 0 {
		return chooseMailbox(flagged, string(role)+" special-use mailbox", missingCode)
	}
	fallback := matchingMailboxNames(mailboxes, func(mailbox MailboxInfo) bool {
		return containsMailboxName(mailboxRoleFallbacks[role], mailbox)
	})
	return chooseMailbox(fallback, string(role)+" mailbox name", missingCode)
}

// PickSentMailbox is retained for callers that only accept a string. It
// returns an empty string for missing or ambiguous selection; send paths use
// ResolveSentMailbox so callers receive the typed error and candidate list.
func PickSentMailbox(mailboxes []MailboxInfo) string {
	mailbox, err := ResolveSentMailbox(mailboxes)
	if err != nil {
		return ""
	}
	return mailbox
}

// PickTrashMailbox is the compatibility wrapper for ResolveTrashMailbox.
func PickTrashMailbox(mailboxes []MailboxInfo) string {
	mailbox, err := ResolveTrashMailbox(mailboxes)
	if err != nil {
		return ""
	}
	return mailbox
}

func mailboxRoleForPath(path []string) (mailboxRole, bool) {
	if len(path) != 1 {
		return "", false
	}
	for _, role := range mailboxRoleOrder {
		aliases := mailboxRoleAliases[role]
		for _, alias := range aliases {
			if strings.EqualFold(path[0], alias) {
				return role, true
			}
		}
	}
	return "", false
}

func matchingMailboxNames(mailboxes []MailboxInfo, predicate func(MailboxInfo) bool) []string {
	matches := make([]string, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		if mailboxWireName(mailbox) != "" && predicate(mailbox) {
			matches = append(matches, mailboxWireName(mailbox))
		}
	}
	sort.Strings(matches)
	return matches
}

func exactMailboxPathMatches(mailboxes []MailboxInfo, path []string) []string {
	return matchingMailboxNames(mailboxes, func(mailbox MailboxInfo) bool {
		for _, candidate := range mailboxDisplayPaths(mailbox) {
			if equalMailboxPath(candidate, path) {
				return true
			}
		}
		if mailboxLegacy(mailbox) {
			legacyNames := []string{strings.Join(path, "/")}
			if len(path) > 1 {
				legacyNames = append(legacyNames, strings.Join(path, "."))
			}
			for _, name := range legacyNames {
				if strings.EqualFold(mailboxWireName(mailbox), name) {
					return true
				}
			}
		}
		return false
	})
}

func resolveInboxMailbox(mailboxes []MailboxInfo) (string, error) {
	candidates := matchingMailboxNames(mailboxes, func(mailbox MailboxInfo) bool {
		return strings.EqualFold(mailboxWireName(mailbox), "INBOX")
	})
	if len(candidates) == 0 {
		return "INBOX", nil
	}
	for _, candidate := range candidates {
		if candidate == "INBOX" {
			return candidate, nil
		}
	}
	return chooseMailbox(candidates, "INBOX mailbox", CodeIMAPMailboxNotFound)
}

func chooseMailbox(candidates []string, source string, missingCode string) (string, error) {
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	if len(candidates) == 0 {
		return "", &TransportError{Code: missingCode, Message: "no " + source + " found on the IMAP server"}
	}
	return "", &TransportError{
		Code: CodeIMAPAmbiguousMailbox,
		Message: fmt.Sprintf(
			"multiple %s candidates exist on the IMAP server: %s; use an exact mailbox path or resolve the duplicate special-use folders before retrying",
			source, strings.Join(candidates, ", "),
		),
	}
}

func hasMailboxFlag(mailbox MailboxInfo, want string) bool {
	for _, flag := range mailbox.Flags {
		if strings.EqualFold(flag, want) {
			return true
		}
	}
	return false
}

func containsMailboxName(names []string, candidate MailboxInfo) bool {
	candidateName := mailboxDisplayName(candidate)
	for _, name := range names {
		if candidateName == name || (mailboxLegacy(candidate) && strings.EqualFold(name, candidateName)) {
			return true
		}
	}
	return false
}

func mailboxWireName(mailbox MailboxInfo) string {
	if mailbox.WireName != "" {
		return mailbox.WireName
	}
	return mailbox.Name
}

func mailboxDisplayName(mailbox MailboxInfo) string {
	if mailbox.DisplayName != "" {
		return mailbox.DisplayName
	}
	return mailboxWireName(mailbox)
}

func mailboxLegacy(mailbox MailboxInfo) bool {
	return mailbox.WireName == "" && mailbox.DisplayName == "" &&
		len(mailbox.DisplayPath) == 0 && mailbox.Delimiter == "" && mailbox.Encoding == ""
}

func mailboxDisplayPaths(mailbox MailboxInfo) [][]string {
	path := append([]string(nil), mailbox.DisplayPath...)
	if len(path) == 0 {
		path = []string{mailboxDisplayName(mailbox)}
	}
	paths := [][]string{path}
	if len(path) > 1 && path[0] == "[Gmail]" {
		paths = append(paths, append([]string(nil), path[1:]...))
	}
	return paths
}

func equalMailboxPath(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func mailboxLeaf(name string) string {
	separator := strings.LastIndexAny(name, "/.")
	if separator < 0 {
		return name
	}
	return name[separator+1:]
}

func invalidMailboxPath(path []string) bool {
	for _, segment := range path {
		if segment == "" {
			return true
		}
	}
	return false
}
