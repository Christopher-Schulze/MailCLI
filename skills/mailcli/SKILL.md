---
name: mailcli
description: Read/search, draft/reply/forward/send, save attachments, organize and sync configured macOS Mail accounts through MailCLI. Not for unrelated prose or provider login.
---

# MailCLI

Use only MailCLI. Keep private content out of logs; never ask the user to paste account passwords or tokens. Credentials: Keychain via `mailcli send setup`.

Keep `command -v mailcli`'s executable. `mailcli capabilities --for ID --json` (comma IDs/`NAME.*`): require ok, envelope 1, capabilities schema 2, compatible identity. Resolve schema_ref.resolve; obey dependencies. Cache by binary SHA-256; invalidate on replacement/contract change/failure.

Pipes default JSON, terminals human; `--json` forces JSON. Check envelope/exit; reuse refs/cursors/revisions exactly, refresh refs after mutation/sync. Never replay successful/uncertain writes.

## Choose the command

Read the linked guide first.

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

Never bypass MailCLI via private files, SQLite or UI/scripts. No owned index/refresh command. Follow guide boundaries.

## Five workflows

Quote operands: REF=list `data.page.messages[].ref`, search/filter `.summary.ref`; DRAFT=`data.draft.ref`/draft list; FILE=UTF-8 body file; QUERY=requested text; REV=`data.draft_preview.revision`; NEXT=`data.page.next_cursor`.

| Workflow | Route/result |
| --- | --- |
| Triage | `mailcli messages list` → `mailcli messages get REF --view plain`: metadata/body. Unread: `mailcli messages filter --read false`. |
| Thread | `mailcli messages thread REF`: chronological members. Same REF, `--cursor` from `data.thread.prev_cursor` (older)/`next_cursor` (newer); both until absent. |
| Search | `mailcli messages search --query QUERY`: matches/coverage. Same query/filters, `--cursor NEXT` until absent; inspect coverage. |
| Reply | `mailcli messages reply REF --body-file FILE` → `mailcli drafts preview DRAFT`: local reply/review. Authorized `--from ADDRESS` if needed. |
| Send | `mailcli drafts preview DRAFT` → `mailcli drafts send DRAFT --confirm --expected-revision REV`: authorized full review then submit. Pending/unknown: reconcile. |

List/filter follow NEXT until absent. Require complete content, never truncated review. Inspect completed drafts, never recreate.

Send/destructive actions need authorization/confirmation; reuse only for unchanged scope/content. Summaries are not review. Save/export to absolute new paths; verify size/hash.

## Error contract

Follow `next.do` with emitted command/args; never invent replay. [Recovery](references/output-and-recovery.md).

| `next.do` | Action |
| --- | --- |
| `retry` | Retry the original invocation after `wait_seconds`, if present. |
| `fix_input` | Correct the named input first. |
| `check_state` | Observe retained evidence; never repeat the original write. |
| `ask_user` | Request the required environment or permission repair. |
| `stop` | Stop; cancellation never authorizes restart. |
