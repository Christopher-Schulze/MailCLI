package mailstore

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

func (s *Store) selectedListMailboxes(ctx context.Context, request mail.ListMessagesRequest) ([]mail.Mailbox, error) {
	selected, err := s.matchedListMailboxes(ctx, request)
	if err != nil {
		return nil, err
	}
	if request.MailboxRef != "" && len(selected) != 1 {
		return nil, listMailboxSelectionError(selected)
	}
	return selected, nil
}

// selectedSearchMailboxes resolves a role or exact path for search and filter:
// every account contributes its own matching mailbox, so `inbox` covers the
// inbox of each account. Two matches inside one account stay ambiguous.
func (s *Store) selectedSearchMailboxes(ctx context.Context, selector string, accountRef string) ([]mail.Mailbox, error) {
	selected, err := s.matchedListMailboxes(ctx, mail.ListMessagesRequest{MailboxRef: selector, AccountRef: accountRef})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, mailbox := range selected {
		if seen[mailbox.AccountRef] {
			return nil, listMailboxSelectionError(selected)
		}
		seen[mailbox.AccountRef] = true
	}
	if len(selected) == 0 {
		return nil, listMailboxSelectionError(selected)
	}
	return selected, nil
}

func (s *Store) matchedListMailboxes(ctx context.Context, request mail.ListMessagesRequest) ([]mail.Mailbox, error) {
	mailboxes, err := s.ListMailboxes(ctx, mail.ListMailboxesRequest{AccountRef: request.AccountRef})
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(request.MailboxRef, "mbx_") {
		reference, err := mailref.DecodeMailbox(request.MailboxRef)
		if err != nil {
			return nil, &mail.ValidationError{Code: "invalid_reference", Message: fmt.Sprintf("invalid mailbox ref: %v", err)}
		}
		accountRef, err := mailref.EncodeAccount(strings.ToUpper(reference.AccountID))
		if err != nil {
			return nil, err
		}
		for _, mailbox := range mailboxes {
			if mailbox.AccountRef == accountRef && slices.Equal(mailbox.Path, reference.Path) {
				return []mail.Mailbox{mailbox}, nil
			}
		}
		return nil, nil
	}
	selector := request.MailboxRef
	if selector == "" {
		selector = "inbox"
	}
	byAccount := map[string][]mail.Mailbox{}
	for _, mailbox := range mailboxes {
		byAccount[mailbox.AccountRef] = append(byAccount[mailbox.AccountRef], mailbox)
	}
	selected := []mail.Mailbox{}
	accounts := make([]string, 0, len(byAccount))
	for account := range byAccount {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	for _, account := range accounts {
		matches, err := s.matchListMailboxes(ctx, byAccount[account], selector)
		if err != nil {
			return nil, err
		}
		selected = append(selected, matches...)
	}
	return selected, nil
}

func (s *Store) matchListMailboxes(ctx context.Context, mailboxes []mail.Mailbox, selector string) ([]mail.Mailbox, error) {
	path := strings.Split(selector, "/")
	if !slices.Contains([]string{"inbox", "sent", "drafts", "trash", "junk", "archive"}, strings.ToLower(selector)) {
		matches := []mail.Mailbox{}
		for _, mailbox := range mailboxes {
			if slices.Equal(mailbox.Path, path) {
				matches = append(matches, mailbox)
			}
		}
		return matches, nil
	}
	infos, err := s.listMailboxRoleInfos(ctx, mailboxes, selector)
	if err != nil {
		return nil, err
	}
	name, err := transport.ResolveMailboxPath(infos, path)
	if err != nil {
		if transport.ErrorCode(err) == transport.CodeIMAPMailboxNotFound {
			return nil, nil
		}
		candidates := ambiguousListMailboxCandidates(infos, mailboxes, path)
		return nil, operationErrorWithCause("ambiguous_mailbox", listMailboxSelectionError(candidates).Error(), err)
	}
	for index, info := range infos {
		if info.WireName == name {
			return []mail.Mailbox{mailboxes[index]}, nil
		}
	}
	return nil, nil
}

func ambiguousListMailboxCandidates(infos []transport.MailboxInfo, mailboxes []mail.Mailbox, path []string) []mail.Mailbox {
	flag := ""
	if len(path) == 1 && strings.EqualFold(path[0], "sent") {
		flag = "\\Sent"
	} else if len(path) == 1 && strings.EqualFold(path[0], "drafts") {
		flag = "\\Drafts"
	}
	flagged := []mail.Mailbox{}
	matched := []mail.Mailbox{}
	for index, info := range infos {
		name, err := transport.ResolveMailboxPath([]transport.MailboxInfo{info}, path)
		if err != nil || name != info.WireName {
			continue
		}
		matched = append(matched, mailboxes[index])
		if flag != "" && slices.Contains(info.Flags, flag) {
			flagged = append(flagged, mailboxes[index])
		}
	}
	if len(flagged) > 0 {
		return flagged
	}
	return matched
}

func (s *Store) listMailboxRoleInfos(ctx context.Context, mailboxes []mail.Mailbox, selector string) ([]transport.MailboxInfo, error) {
	if len(mailboxes) == 0 {
		return nil, nil
	}
	flags := map[string][]string{}
	if strings.EqualFold(selector, "sent") || strings.EqualFold(selector, "drafts") {
		account, err := mailref.DecodeAccount(mailboxes[0].AccountRef)
		if err != nil {
			return nil, err
		}
		cache, err := s.loadMailboxCache(ctx, account.AccountID)
		if err != nil {
			return nil, err
		}
		collectListMailboxRoleFlags(cache.Mailboxes, nil, flags)
	}
	infos := make([]transport.MailboxInfo, len(mailboxes))
	for index, mailbox := range mailboxes {
		name := strings.Join(mailbox.Path, "/")
		infos[index] = transport.MailboxInfo{
			Name: name, WireName: name, DisplayName: name, DisplayPath: mailbox.Path,
			Delimiter: "/", Flags: flags[name],
		}
	}
	return infos, nil
}

func collectListMailboxRoleFlags(nodes map[string]mailboxCacheNode, parent []string, flags map[string][]string) {
	for key, node := range nodes {
		component := node.PathComponent
		if component == "" {
			component = key
		}
		path := append(slices.Clone(parent), component)
		visible := path
		if len(visible) > 1 && visible[0] == "[Gmail]" {
			visible = visible[1:]
		}
		name := strings.Join(visible, "/")
		if node.Attributes&mailboxAttributeSent != 0 {
			flags[name] = append(flags[name], "\\Sent")
		}
		if node.Attributes&mailboxAttributeDrafts != 0 {
			flags[name] = append(flags[name], "\\Drafts")
		}
		collectListMailboxRoleFlags(node.Children, path, flags)
	}
}

func listMailboxSelectionError(mailboxes []mail.Mailbox) error {
	if len(mailboxes) == 0 {
		return operationError("not_found", "mailbox role or exact path was not found in the selected active accounts")
	}
	refs := make([]string, len(mailboxes))
	for index, mailbox := range mailboxes {
		refs[index] = mailbox.Ref
	}
	sort.Strings(refs)
	return operationError("ambiguous_mailbox", fmt.Sprintf("mailbox selection matches multiple candidates: %s; narrow with --account or use one mailbox ref", strings.Join(refs, ", ")))
}
