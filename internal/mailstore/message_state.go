package mailstore

import (
	"context"
	"strings"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

const messageStateStalenessNote = "server flags are read over IMAP now; local index flags reflect the last Mail.app sync and may lag"

// MessageState reads the verified server-side flag snapshot for one
// message and pairs it with the local Envelope Index projection so callers
// can see whether a mutation landed before Mail.app synchronizes again. It
// resolves the IMAP target through the same identity, credential, and mailbox
// path as mutations, then issues one bounded UID FETCH ... FLAGS. Nothing is
// stored, marked, or expunged.
func (c *Client) MessageState(ctx context.Context, messageRef string) (mail.MessageState, error) {
	if c.store == nil {
		return mail.MessageState{}, c.readUnavailableError()
	}
	target, err := c.resolveImapTarget(ctx, messageRef)
	if err != nil {
		return mail.MessageState{}, err
	}
	reader, supported := c.send.ImapClient().(transport.FlagStateReader)
	if !supported {
		return mail.MessageState{}, operationError(
			"imap_flag_read_unsupported",
			"the configured IMAP transport cannot read server flag state",
		)
	}
	server, err := reader.FetchFlags(ctx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity)
	if err != nil {
		return mail.MessageState{}, err
	}
	state := mail.MessageState{
		Ref:               messageRef,
		ServerState:       mail.MessageServerStateObserved,
		ServerUID:         target.uid,
		ServerUIDValidity: server.UIDValidity,
		ServerFlags:       append([]string{}, server.Flags...),
		StalenessNote:     messageStateStalenessNote,
	}
	if server.UIDValidity == 0 {
		state.ServerUIDValidity = target.uidvalidity
	}
	state.LocalIndexFlags = mail.LocalIndexFlags{
		Read:    target.summary.Read,
		Flagged: target.summary.Flagged,
		Junk:    target.summary.Junk,
		Deleted: target.summary.Deleted,
	}
	if server.Missing {
		state.ServerState = mail.MessageServerStateMissing
		state.ServerFlags = nil
		return state, nil
	}
	state.FlagsAgree = flagsAgreeWithLocal(state.ServerFlags, state.LocalIndexFlags)
	return state, nil
}

// flagsAgreeWithLocal compares the server's raw flag list with the local
// projection using the same standardized flag mapping mutations write:
// \Seen, \Flagged, \Deleted, and the $Junk/$NotJunk pair. A server pair with
// both junk markers classifies as not-junk, matching the mutation contract.
func flagsAgreeWithLocal(serverFlags []string, local mail.LocalIndexFlags) bool {
	return local.Read == hasServerFlag(serverFlags, "\\Seen") &&
		local.Flagged == hasServerFlag(serverFlags, "\\Flagged") &&
		local.Deleted == hasServerFlag(serverFlags, "\\Deleted") &&
		local.Junk == serverJunkClassification(serverFlags)
}

func serverJunkClassification(flags []string) bool {
	return hasServerFlag(flags, "$Junk") && !hasServerFlag(flags, "$NotJunk")
}

func hasServerFlag(flags []string, wanted string) bool {
	for _, flag := range flags {
		if strings.EqualFold(flag, wanted) {
			return true
		}
	}
	return false
}
