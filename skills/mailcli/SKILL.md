---
name: mailcli
description: Read, search, draft, reply, forward, send, save received attachments, organize, or synchronize email through accounts already configured in macOS Mail.app. Use for local MailCLI work; never use it for unrelated prose or direct provider login.
---

# MailCLI

Use MailCLI as the only boundary to the user's configured mail accounts. Reads use Mail's existing local store; direct SMTP/IMAP operations use the Keychain credential selected by the sender binding; never ask the user to paste account passwords, app-specific passwords, OAuth tokens, or cookies. Send users to `mailcli send setup`.

## Start and preflight

1. Resolve one executable: `command -v mailcli`, or `./bin/mailcli` after `./scripts/build/build.sh` in a checkout. Keep that exact path. Examples below use `mailcli` as shorthand for it.
2. Choose the command from the table below and read its linked guide. Request `mailcli capabilities --command COMMAND_ID --json` once for that command and binary. If the exact command is unknown, use `--family messages` or another known family; use the full catalog only for cross-family discovery. Require `ok:true`, envelope and capability `schema_version:1`, and compatible release identity. Use only returned schemas, limits, effects, confirmations, dependencies, and result states. Help is for humans. Retain inspected contracts for the session, keyed to binary SHA-256; invalidate after replacement, contract change, or capability failure. Never substitute an older binary's contract.
3. Check only the selected command's dependencies. For a local-store operation, run `mailcli doctor --json` on the first store assessment; a checkout's `scripts/utils/mailcli-preflight.sh doctor --binary PATH` reuses a healthy result for at most 300 seconds. Refresh after store, permission, schema, account, or read failure. Direct send and local draft operations do not need an unrelated store check. Never cache `doctor --live`: run it immediately before Apple Events and require Mail.app to already be running.
4. Execute with `--json`, inspect the one envelope and exit status, then follow result evidence or `error.guidance`. Reuse refs, revisions, and cursors exactly; resolve fresh references after mutation or synchronization. Never replay a successful or uncertain write.

## Choose the command

Read only the guide needed for the current action. When a workflow changes action, load the next guide before executing it. All guides ship inside this skill.

| Intent | Command ID for scoped capabilities | Guide |
| --- | --- | --- |
| Find accounts or mailboxes | `accounts.list`, `mailboxes.list`, `mailboxes.resolve` | [Reading](references/reading.md) |
| Find or read messages; save received files | `messages.search`, `messages.get`, `attachments.save` | [Reading](references/reading.md) |
| Create, reply, forward, inspect, or edit a draft | `drafts.create`, `messages.reply`, `messages.forward`, `drafts.inspect`, `drafts.update` | [Drafts](references/drafts.md) |
| Send reviewed content or reconcile a send | `drafts.send`, `drafts.reconcile` | [Sending](references/sending.md) |
| Mark, move, copy, delete, or synchronize | `messages.mark`, `messages.move`, `messages.copy`, `messages.delete`, `sync` | [Mutations](references/mutations.md) |
| Open a visible new compose or resolve its outcome | `drafts.handoff`, `drafts.handoff-reconcile` | [Native handoff](references/native-handoff.md) |
| Install, diagnose, or configure a sender | `doctor`, `send.setup` | [Setup](references/setup.md) |
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

## Execute with minimal output

- List or search metadata first; fetch `messages get --ref REF --view plain --json` only for relevant messages. Use `--fields` for a schema-supported selection or `--export /absolute/new/path` for complete content outside the response. Never substitute a summary for a required full-content review.
- Follow `data.page.next_cursor` until empty for exhaustive requests. Search completeness additionally requires `data.page.coverage.complete`; inspect `content_complete` and `missing_parts` before claiming complete content. `imap_ambiguous_message_id` stops identity-dependent work.
- Use `batch` for supported operations on explicit refs, with unique item IDs and published limits. Inspect every item; a failed or uncertain item never authorizes replay of successful siblings. Read the output guide before batching.
- Draft workflow: create/reply/forward, inspect with `drafts inspect --ref REF --view full --json`, retain `data.draft.revision`, update with that reviewed `--expected-revision`, and review again. Send only when the user's authorization covers the reviewed content, using `--expected-revision REVISION --confirm`. On revision conflict, inspect and merge; never copy a revision out of an error and retry blindly.
- `sent` proves SMTP acceptance plus exact Sent persistence. `sent_mirror_pending` needs `drafts reconcile`; uncertain outcomes prohibit resubmission. Neither `submission_accepted:true` nor `sent_copy_observed:true` proves recipient delivery. `drafts inspect --ref REF --json` exposes retained receipt evidence.
- `mailcli sync --check` observes IMAP state; `sync` without `--check` asks Mail.app to synchronize. New scripted `drafts save` fails with `compose_automation_unsupported`; visible handoff confirms compose acceptance only. Read the native guide for retained claims.
- Keep bodies, addresses, credentials, and attachment bytes out of logs and summaries unless requested. Save/export only to absolute new paths and inspect returned size and SHA-256 evidence.

## Error contract

Every JSON call returns one envelope:

```json
{"schema_version":1,"ok":true,"command":"accounts.list","data":{},"error":null}
```

Check `ok`, `error.code`, `error.message`, and `error.guidance`: `phase`, `effect_certainty`, `retryability`, `replay_allowed`, `recovery`. Only `safe` with replay allowed permits an immediate retry. `observe_required` or `replay_allowed:false` requires observation first; `user_input_required` requires correcting the named input/environment; `terminal` means stop. Follow the emitted recovery command and retain its operation ID. Never invent a retry from prose.

Exit 0 is success, 1 operation failure, and 2 usage failure. Only `sync --check --require-complete` may return exit 3 with `ok:true` for valid incomplete coverage. Read the output guide for teardown failures and retained partial evidence.
