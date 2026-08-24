---
name: mailcli
description: Read, search, draft, reply, forward, attach, send, organize, or synchronize email through the accounts already configured in the local macOS Mail app. Use whenever the user asks an agent to inspect or act on local Mail.app content; do not use for direct provider login or unrelated prose-only email writing.
---

# MailCLI

Use MailCLI as the single boundary to accounts already configured in macOS Mail. Never request Gmail, iCloud, IMAP, SMTP, OAuth, or app-specific credentials.

## Discover capabilities

1. Locate the executable with `command -v mailcli`; in this repository, fall back to `./bin/mailcli` after running `./scripts/build/build.sh`.
2. Run `mailcli help` and use only commands the installed binary exposes.
3. Run `mailcli doctor --json` before Mail access. It verifies strict read-only store access and reports the exact Full Disk Access remediation when needed. Use `mailcli doctor --live --json` only before an Apple Events operation; Mail must already be running, the probe never launches it, and macOS may request Automation consent.
4. Prefer `--json`. Require exit code `0`, top-level `ok:true`, and `schema_version:1`; list, filter, and search results are under `data.page`.

MailCLI serializes Apple Events across all local agent processes and fully reaps its owned `osascript` process before returning. Never issue parallel raw `osascript` calls. `mail_busy` means the operation did not contact Mail; wait for the active operation and retry once. `mail_not_running` means ask the user to open Mail when the requested action needs it. `mail_recovery_required` means a previous operation timed out and Mail may still be processing its Apple Event; do not retry, send another live command, or restart Mail automatically.

## Read

Resolve account and mailbox identifiers through list commands instead of guessing display names. Traverse every returned mailbox when the user asks for all mail. Use pagination until no cursor remains. Request full bodies, raw MIME, or attachment data only when required; list results intentionally omit large content.

Use `accounts list --json`, then `mailboxes list --account REF --json` when an account scope is useful. Resolve localized or nested folders with one exact path segment per flag, for example `mailboxes resolve --account REF --path Gesendet --json` or `--path Projects --path 2026`. Use `messages list --mailbox REF --limit N --cursor CURSOR --json` for mailbox traversal, `messages get --ref REF --json` for normalized detail, and `messages raw --ref REF` for exact source.

List, filter, and search page sizes are `1..25`. Use `--limit 25` for throughput and continue with `data.page.next_cursor`; never invent a larger page size.

Use `messages filter --json` for typed constraints and `messages search --query TEXT --json` only when body terms are required. Both accept `--sender`, `--recipient`, `--subject`, `--after`, `--before`, `--read true|false`, `--flagged true|false`, `--attachment true|false`, `--account REF`, and `--mailbox REF`. The attachment filter reads authoritative MIME because Mail's metadata catalog can lag; a negative result is exhaustive only when `data.page.coverage.complete` is true. Body search is a stateless on-demand scan; narrow its scope and set `--max-messages` or `--max-bytes` when the task is bounded. Continue with `data.page.next_cursor`. Inspect `data.page.coverage.complete`, partial and missing source counts, and scan bounds before claiming exhaustive results.

MailCLI owns no search index and has no refresh command. Its supported reads already use Mail's local store without Apple Events. Never bypass it with raw SQLite, direct private-file traversal, Mail `whose`, or UI-coordinate automation.

Treat returned message references and cursors as opaque and short-lived. MailCLI binds them to the current store and mailbox membership. After copy, move, delete, or provider synchronization, list or search again for a fresh reference before the next action.

## Compose and mutate

Pass long bodies, recipients, and attachment lists as structured JSON through `--input -` or an input file. Draft first, inspect the returned draft, update it if needed, and send only after the user explicitly authorizes the exact send. Keep To, CC, and BCC distinct. Use an explicit configured `from` address when account choice matters. Draft attachment paths must be absolute; MailCLI hashes them and refuses to send changed files.

Create a new draft with `printf '%s' '{"from":"me@example.com","to":[{"address":"recipient@example.com"}],"cc":[],"bcc":[],"subject":"Subject","body":"Readable plain text.\n","attachments":[]}' | mailcli drafts create --input - --json`. Inspect it, replace the full input with `drafts update` if needed, then send only with `drafts send --ref DRAFT_REF --confirm --json` after authorization. Interpret `data.send_result.outcome` exactly: `sent_store_observed` is proven and removes the draft; `accepted_by_mail` or `outcome_unknown` retains and permanently blocks resend. For either retained outcome, use `drafts reconcile --ref DRAFT_REF --json`; reconciliation reads Sent and never sends again.

To export the review object to Mail's Drafts mailbox without sending, require an explicit configured `from`, ensure no user-owned compose window is open, then run `drafts save --ref DRAFT_REF --json`. Never retry `draft_outcome_unknown` automatically. MailCLI never quits, relaunches, or activates Mail. Mail 16 can retain an invisible backend after save, so a later save may return `compose_busy`. Inspect an exported draft with `drafts open --message MSG_REF --json`; keep edits in the local review draft because Mail 16 has no reliable headless in-place editor for a persisted draft.

For replies and forwards, create a local draft with `messages reply --message MSG_REF --input - [--all] --json` or `messages forward --message MSG_REF --input - --json`, inspect it, then use the same confirmed `drafts send` command. Mail.app supplies native threading, quoted content, and original forward attachments at send time. MailCLI guarantees reviewed plain text, not arbitrary HTML.

List received attachments with `attachments list --message MSG_REF --json`. Save one with `attachments save --message MSG_REF --attachment ID --output /absolute/non-existing/path --json`; the command never overwrites an existing path and returns byte count plus SHA-256.

Use explicit `messages mark`, `messages copy`, `messages move`, `messages delete --confirm`, and `sync` commands. Do not reinterpret archive as delete or delete as permanent removal. Never send or delete without explicit user authorization. Moving, copying, marking, and synchronization also mutate live Mail state, so require a user request that covers the action.

Never loop live mutations rapidly. Let each MailCLI command finish and inspect its typed result before issuing the next command. The CLI reuses account and mailbox resolution within an invocation, bounds message pages to 25, and backs off outcome checks automatically; agents must not add their own polling or raw Apple Events around it.

## Output handling

Keep message bodies and addresses out of logs and summaries unless the task requires them. Preserve exact raw output when the user asks for RFC 5322 source. For human-facing drafts, favor clear plain text unless the installed command explicitly reports tested rich-text or HTML support; a successful Apple Event alone does not prove HTML fidelity.

Check `content_complete`, `missing_parts`, and search `coverage` before making completeness claims. If the binary lacks a required command or returns an unsupported store profile, stop and report it. Do not bypass MailCLI with direct store access, raw AppleScript/JXA, or UI coordinates.
