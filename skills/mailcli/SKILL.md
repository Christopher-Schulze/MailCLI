---
name: mailcli
description: Read, search, draft, reply, forward, send, save received attachments, organize, or synchronize email through accounts already configured in macOS Mail.app. Use for local MailCLI work; never use it for unrelated prose or direct provider login.
---

# MailCLI

Use MailCLI as the only boundary to the user's configured mail accounts. Reads use Mail's existing local store; direct SMTP/IMAP operations use the Keychain credential selected by the sender binding; never ask the user to paste account passwords, app-specific passwords, OAuth tokens, or cookies. Send users to `mailcli send setup`.

## Start and preflight

1. Resolve `command -v mailcli` and keep that exact executable. Checkout/build/cache helpers: [Setup](references/setup.md).
2. Load the action's guide and `mailcli capabilities --for ID,ID --json` for its actual command IDs; use `--for 'NAME.*'` for family discovery. Require `ok:true`, envelope schema 1 and capabilities schema 2 with compatible release identity. Follow exact command schemas, limits, effects, confirmations, result states, `store_dependency` and conditional typed dependencies. Cache contracts by binary SHA-256; invalidate after replacement, contract change or failure.
3. Run `mailcli doctor --json` for the first required store assessment; refresh after store, permission, schema, account or read failures. Local drafts/direct send need no unrelated store check. Never cache `doctor --live`; run immediately before Apple Events with Mail running.
4. Pipes/files default to JSON; terminals to human text. `--json`/`--human` overrides `MAILCLI_OUTPUT=json|human`; conflicts or invalid values fail before initialization. Help remains text. Inspect envelope and exit status; reuse refs/revisions/cursors exactly, refresh refs after mutation/sync, and never replay successful or uncertain writes.

## Choose the command

Read only the guide needed for the current action. When a workflow changes action, load the next guide before executing it. All guides ship inside this skill.

| Intent | Command ID for scoped capabilities | Guide |
| --- | --- | --- |
| Find accounts or mailboxes | `accounts.list`, `mailboxes.list`, `mailboxes.resolve` | [Reading](references/reading.md) |
| Find or read messages; save received files | `messages.list`, `messages.filter`, `messages.search`, `messages.get`, `messages.raw`, `messages.thread`, `attachments.list`, `attachments.save` | [Reading](references/reading.md) |
| Create, reply, forward, inspect, or manage a draft; edit is human-only | `drafts.create`, `drafts.list`, `drafts.inspect`, `drafts.preview`, `drafts.edit`, `drafts.update`, `drafts.open`, `drafts.adopt`, `drafts.discard`, `drafts.prune`, `messages.reply`, `messages.forward` | [Drafts](references/drafts.md) |
| Send reviewed content or reconcile a send | `drafts.send`, `drafts.reconcile` | [Sending](references/sending.md) |
| Verify flags, mark, move, copy, delete, or synchronize | `messages.state`, `messages.mark`, `messages.move`, `messages.copy`, `messages.delete`, `sync` | [Mutations](references/mutations.md) |
| Open a visible new compose or resolve a native claim | `drafts.handoff`, `drafts.handoff-reconcile` | [Native handoff](references/native-handoff.md) |
| Check version, install, diagnose, or configure a sender | `version`, `update`, `doctor`, `send.setup` | [Setup](references/setup.md) |
| Batch, project, export, or recover an error | `batch` or the affected command | [Output and recovery](references/output-and-recovery.md) |

## Choose the execution boundary

| Need | Path | Mail.app / permission |
| --- | --- | --- |
| Accounts, mailboxes, local messages, raw source, attachments | local store; bounded IMAP hydration only when content is incomplete | Full Disk Access; no Apple Events |
| `sync --check`, mark, move, copy, delete | direct IMAP | no Mail.app or Automation |
| `drafts send`, `drafts reconcile` for a direct claim, setup | direct SMTP/IMAP and Keychain | no Mail.app or Full Disk Access |
| `sync` without `--check`, `doctor --live`, read-only fallback | Mail.app Apple Events | Mail must already run |
| new visible compose | AppKit handoff | proves compose acceptance only; never sending |

Never issue raw SQLite, private-file traversal, AppleScript/JXA, UI-coordinate automation, or parallel `osascript` calls. MailCLI has no daemon, watcher, copied corpus, owned search index, or refresh command.

## Five workflows

Pipe output for default JSON or use `--json`. Quote each substituted operand. `REF` is `data.page.messages[].ref` from list, or `data.page.messages[].summary.ref` from search/filter; `DRAFT` is `data.draft.ref` or a `drafts list` ref; `FILE` is an existing UTF-8 body file; `QUERY` is the requested text. Read each linked guide before its operation.

| Workflow | Commands and result |
| --- | --- |
| Triage | `mailcli messages list`, then `mailcli messages get REF --view plain`: metadata followed by explicit content. For unread triage use `mailcli messages filter --read false`. [Reading](references/reading.md) |
| Thread | `mailcli messages thread REF`: chronological members around the seed. Repeat with the same REF and `--cursor CURSOR` from `data.thread.prev_cursor` for older members or `data.thread.next_cursor` for newer ones; traverse each direction until its cursor is absent. [Reading](references/reading.md) |
| Search | `mailcli messages search --query QUERY`: matches and coverage. Repeat with the same query/filters and `--cursor NEXT`, where NEXT is `data.page.next_cursor`, until absent; require `data.page.coverage.complete` for exhaustive claims. [Reading](references/reading.md) |
| Reply | `mailcli messages reply REF --body-file FILE`, then `mailcli drafts preview DRAFT`: creates one local reply and returns its complete review. Select an authorized sender with `--from ADDRESS` on reply when needed; never infer it from the recipient. [Drafts](references/drafts.md) |
| Send | `mailcli drafts preview DRAFT`, then `mailcli drafts send DRAFT --confirm --expected-revision REV`: REV is `data.draft_preview.revision` from the complete reviewed preview. Require authorization for that content and sender. `sent` proves submission and Sent persistence, not delivery; pending or unknown outcomes require observation/reconciliation, never resubmission. [Sending](references/sending.md) |

For list/filter continuation, repeat the same selection with `--cursor NEXT` until `data.page.next_cursor` is absent. Check `content_complete` and `missing_parts` before claiming complete bodies. On `search_budget_too_small`, preserve the incoming cursor/query/filters and use `error.required_bytes` plus emitted recovery args to raise `--max-scan-bytes`. Output overflow never authorizes truncated review: follow the recovery guide to raise a read budget or export to an absolute new path; inspect an already-completed draft mutation instead of creating it again.

## Execute with minimal output

Require explicit user authorization for sending or destructive changes and pass the advertised confirmation flags. Reuse existing authorization while its scope and reviewed content remain unchanged; do not ask for the same confirmation again.

- Use schema-supported `--fields` to trim metadata, retaining identity, cursors and coverage. Never substitute summaries for required content review. `imap_ambiguous_message_id` stops identity-dependent work.
- Before batching read [Output and recovery](references/output-and-recovery.md) for selectors, limits and confirmation. Use explicit refs and unique item IDs. Reads default to metadata. Inspect every partial result; failed or uncertain items never authorize replay of successful siblings.
- Update only changed draft fields with the reviewed `data.draft.revision`; review the updated content. On conflict, inspect and merge, never blindly reuse a revision from an error. `drafts inspect --ref REF --json` exposes retained receipt evidence; `sent_mirror_pending` needs `drafts reconcile`.
- `mailcli sync --check` observes IMAP state; `sync` without `--check` asks Mail.app to synchronize. Scripted native save is unavailable; visible handoff confirms compose acceptance only. Read the native guide for retained claims.
- Keep bodies, addresses, credentials, and attachment bytes out of logs and summaries unless requested. Save/export only to absolute new paths and inspect returned size and SHA-256 evidence.

## Error contract

Follow `next.do` on failures and pending results; use emitted command/args, never invent replay. Details: [recovery](references/output-and-recovery.md).

| `next.do` | Action |
| --- | --- |
| `retry` | Retry the original invocation after `wait_seconds`, if present. |
| `fix_input` | Correct the named input first. |
| `check_state` | Observe retained evidence; never repeat the original write. |
| `ask_user` | Request the required environment or permission repair. |
| `stop` | Stop; cancellation never authorizes restart. |
