---
name: mailcli
description: Read/search, draft/reply/forward/send, save attachments, organize and sync configured macOS Mail accounts through MailCLI. Not for unrelated prose or provider login.
---

# MailCLI

Use only MailCLI for mail work. Keep message content out of logs. Never ask the user for passwords or tokens; credentials go into the Keychain through `mailcli send setup`.

Resolve the binary once with `command -v mailcli` and keep that path for the session.

Load the contract first:

1. Run `mailcli version --json` and read `data.contract_sha256`. If a cached contract has the same digest, use it and skip step 2.
2. Run `mailcli capabilities --for ID --schemas --outputs --json` for the command IDs you need. ID is a command ID, a comma-separated list or a family such as `messages.*`. Accept it only when `ok` is true, envelope `schema_version` is 1 and capabilities schema 2; cache it under its `contract_sha256`.
3. Obey each command's `dependencies`, `confirmation` and parameter schema. `error_codes` explains each code a command can return.

Add `--json` to every call. Check `ok` and the exit code of every call. Copy refs, cursors and revisions exactly from MailCLI output. Refresh refs after a mutation or sync. Never replay a write that succeeded or whose outcome is uncertain.

## Choose the command

| Intent | IDs | Guide |
| --- | --- | --- |
| Discover | `accounts.list`,`mailboxes.list`,`mailboxes.resolve` | [Read](references/reading.md) |
| Read | `messages.list`,`messages.filter`,`messages.search`,`messages.get`,`messages.raw`,`messages.thread`,`attachments.list`,`attachments.save` | [Read](references/reading.md) |
| Draft | `drafts.create`,`drafts.list`,`drafts.inspect`,`drafts.preview`,`drafts.edit`,`drafts.update`,`drafts.open`,`drafts.adopt`,`drafts.discard`,`drafts.prune`,`messages.reply`,`messages.forward` | [Draft](references/drafts.md) |
| Send | `drafts.send`,`drafts.reconcile` | [Send](references/sending.md) |
| Mutate | `messages.state`,`messages.mark`,`messages.move`,`messages.copy`,`messages.delete`,`sync` | [Mutate](references/mutations.md) |
| Compose | `drafts.handoff`,`drafts.handoff-reconcile` | [Handoff](references/native-handoff.md) |
| Setup | `version`,`update`,`doctor`,`send.setup` | [Setup](references/setup.md) |
| Batch | `batch` | [Recovery](references/output-and-recovery.md) |

## Choose the execution boundary

Never read Mail's private files or databases or script the Mail UI. MailCLI has no index or refresh command.

## Workflows

Placeholders come from earlier output; quote their values:

- REF: `data.page.messages[].ref` from a list; search and filter nest it as `summary.ref`.
- DRAFT: `data.draft.ref` or a `mailcli drafts list` row.
- FILE: a UTF-8 file that holds the body text.
- QUERY: the requested text.
- REV: `data.draft_preview.revision` from the latest preview.
- NEXT: `data.page.next_cursor` from the previous page.

| Workflow | Commands | Read next |
| --- | --- | --- |
| Triage | `mailcli messages list` (unread: `mailcli messages filter --read false`), then `mailcli messages get REF --view plain`. | Metadata and body. |
| Thread | `mailcli messages thread REF`. For older or newer members repeat with `--cursor` set to `data.thread.prev_cursor` or `data.thread.next_cursor` until it is absent. | Members, oldest first. |
| Search | `mailcli messages search --query QUERY`. Repeat with the same query and filters plus `--cursor NEXT` until NEXT is absent. | `data.page.coverage.complete` on the last page. |
| Reply | `mailcli messages reply REF --body-file FILE`, then `mailcli drafts preview DRAFT`. Add `--from ADDRESS` only for a sender the user named. | The preview for the user. |
| Send | `mailcli drafts preview DRAFT`, then, once the user approved it, `mailcli drafts send DRAFT --confirm --expected-revision REV`. | `data.send_result`; reconcile a pending or unknown one with `mailcli drafts reconcile`. |
| Replies | `mailcli messages search --after DATE --with-threading --with-excerpt`, following NEXT until it is absent. | Sent Message-IDs in `in_reply_to[]` or `references[]`; `threading_complete:false` means unknown, a sender domain is only a candidate. |

Follow NEXT on lists and filters too. Review only complete content. Inspect a completed draft instead of recreating it.

Sends and destructive commands need the user's authorization and the published confirmation, valid only while scope and content stay unchanged. Save and export only to new absolute paths, then verify size and hash.

## Error contract

Follow `next.do` with the command and arguments MailCLI emitted. Never invent a replay. See [Recovery](references/output-and-recovery.md).

| `next.do` | Action |
| --- | --- |
| `retry` | Retry after `wait_seconds`, if present. |
| `fix_input` | Correct the named input first. |
| `check_state` | Observe evidence; never repeat the write. |
| `ask_user` | Ask the user for the `next.why` repair. |
| `stop` | Stop; cancellation never authorizes restart. |
