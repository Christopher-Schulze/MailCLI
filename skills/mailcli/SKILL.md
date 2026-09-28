---
name: mailcli
description: Read/search, draft/reply/forward/send, save attachments, organize and sync configured macOS Mail accounts through MailCLI. Not for unrelated prose or provider login.
---

# MailCLI

Use only MailCLI. Keep private content out of logs; never ask for passwords/tokens; Keychain via `mailcli send setup`.

Pin `command -v mailcli`. `mailcli capabilities --for ID --json` (comma IDs/`NAME.*`): require ok, envelope 1, capabilities schema 2, compatible identity. Resolve schema_ref.resolve; obey dependencies. `--outputs` adds output schemas and `error_codes`. Cache per `contract_sha256` (else binary SHA-256); reread on change.

Pipes default JSON, terminals human; `--json` forces JSON. Check envelope/exit; reuse refs/cursors/revisions exactly, refresh refs after mutation/sync. Never replay successful/uncertain writes.

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

Never bypass MailCLI (private files, SQLite, UI/scripts); no index/refresh command.

## Workflows

Quote operands: REF=list `data.page.messages[].ref`, search/filter `.summary.ref`; DRAFT=`data.draft.ref`/draft list; FILE=UTF-8 body file; QUERY=requested text; REV=`data.draft_preview.revision`; NEXT=`data.page.next_cursor`.

| Workflow | Route/result |
| --- | --- |
| Triage | `mailcli messages list` → `mailcli messages get REF --view plain`: metadata/body. Unread: `mailcli messages filter --read false`. |
| Thread | `mailcli messages thread REF`: chronological members. Same REF, `--cursor` from `data.thread.prev_cursor` (older)/`next_cursor` (newer); both until absent. |
| Search | `mailcli messages search --query QUERY`: matches/coverage. Same query/filters, `--cursor NEXT` until absent; inspect coverage. |
| Reply | `mailcli messages reply REF --body-file FILE` → `mailcli drafts preview DRAFT`: local reply/review. Authorized `--from ADDRESS` if needed. |
| Send | `mailcli drafts preview DRAFT` → `mailcli drafts send DRAFT --confirm --expected-revision REV`: authorized full review then submit. Pending/unknown: reconcile. |
| Replies | `mailcli messages search --after DATE --with-threading --with-excerpt` + NEXT: match sent Message-ID in `in_reply_to[]`/`references[]`; domain=candidate only; `threading_complete=false`=unknown. |

List/filter: follow NEXT until absent. Review complete content only; inspect, never recreate, completed drafts.

Send/destructive: authorization/confirmation, reused only for unchanged scope/content. Save/export to absolute new paths; verify size/hash.

## Error contract

Follow `next.do` with emitted command/args; never invent replay. [Recovery](references/output-and-recovery.md).

| `next.do` | Action |
| --- | --- |
| `retry` | Retry after `wait_seconds`, if present. |
| `fix_input` | Correct the named input first. |
| `check_state` | Observe evidence; never repeat the write. |
| `ask_user` | Ask the user for the `next.why` repair. |
| `stop` | Stop; cancellation never authorizes restart. |
