---
name: mailcli
description: Read, search, draft, reply, forward, send, save received attachments, organize, or synchronize email through the accounts already configured in the local macOS Mail app. Use whenever the user asks an agent to inspect or act on local Mail.app content or to send mail from a configured account; do not use for direct provider login or unrelated prose-only email writing.
---

# MailCLI

Use MailCLI as the single boundary to accounts already configured in macOS Mail. SMTP send and IMAP mailbox mutations share one Keychain credential; never ask the user to paste account passwords, app-specific passwords, OAuth tokens, or any other credentials into chat. Direct the user to `mailcli send setup` once per From address (see Send below). Reads reuse the authentication Mail.app already has.

## Discover capabilities

1. Locate the executable with `command -v mailcli`; in this repository, fall back to `./bin/mailcli` after running `./scripts/build/build.sh`.
2. Run `mailcli capabilities --json` and use only the returned command IDs. Require top-level `ok:true`, envelope `schema_version:1`, `data.capabilities.schema_version:1`, and a compatible release. Never infer capabilities, confirmation, dependencies, or result states from help text. Respect the reported limits, especially `raw_mime_send:true`, `send_transport:"smtp"`, `mutation_transport:"imap"`, scripted `compose_write:false`, `compose_attachment_write:false`, `visible_compose_handoff:true`, and `visible_attachment_handoff:true`.
3. Run `mailcli help` only when human-readable flag usage is needed.
4. Run `mailcli doctor --json` before commands that open the Mail store: accounts, mailboxes, messages list/filter/search/get/raw, attachments, `drafts open`, `drafts reconcile`, and `sync`. Skip it for `send setup`, `drafts send`, and local draft create/list/inspect/preview/edit/update/handoff/discard/prune. It verifies strict read-only store access and reports the exact Full Disk Access remediation when needed. Add `--diagnostics` only when phase timings are needed; timings contain no subjects, bodies, addresses, attachment data, or paths. Use `mailcli doctor --live --json` only before an Apple Events operation; Mail must already be running. The live probe verifies the exact process and reads the Mail version through one Apple Event. It never creates, saves, or sends a message because Mail 16 can retain an invisible outgoing backend even after `close saving no`.
5. Prefer `--json`. Require exit code `0`, top-level `ok:true`, and `schema_version:1`; list, filter, and search results are under `data.page`.

Use `mailcli update` only when the user asks to update MailCLI. Human mode shows progress and either `Updated mailcli...` or `Already up to date...`; `mailcli update --json` emits one non-animated envelope. The command verifies `SHA256SUMS` with the Ed25519 public key pinned in the installed binary before trusting its checksum, then updates both the binary and this skill. If canceled, it lets the installer's rollback trap run and removes its complete owned process group before returning.

Apple Events are used only by `doctor --live`, `drafts open`, `sync` without `--check`, and fallback listing when the store cannot open. SMTP send and IMAP mark/move/copy/delete do not use Apple Events and do not need Mail.app running. For those Apple Events paths, MailCLI serializes calls across local agent processes, verifies the exact Mail process identity, waits for its owned `osascript` leader, terminates residual owned process-group members, and verifies that the group is gone before returning. Never issue parallel raw `osascript` calls. `mail_busy` means the operation did not contact Mail; wait for the active operation and retry once. `mail_not_running` means ask the user to open Mail when the requested action needs Apple Events. `mail_recovery_required` is reserved for an incomplete Apple Events operation that could have mutated Mail; do not retry or send another live command until the user has quit and reopened Mail. Read, probe, sync, and Automation-denial failures do not create that state.

## Read

Resolve account and mailbox identifiers through list commands instead of guessing display names. Traverse every returned mailbox when the user asks for all mail. Use pagination until no cursor remains. Request full bodies, raw MIME, or attachment data only when required; list results intentionally omit large content.

Use `accounts list --json`, then `mailboxes list --account REF --json` when an account scope is useful. Entries with `state:"degraded"` are listed but unusable for sending or mutations (reason in `degraded_reason`; a degraded account needs `mailcli send setup` or a prior successful send to prove a sender identity) - skip them for those flows instead of treating them as healthy. Resolve localized or nested folders with one exact path segment per flag, for example `mailboxes resolve --account REF --path Gesendet --json` or `--path Projects --path 2026`. Use `messages list --mailbox REF --limit N --cursor CURSOR --json` for mailbox traversal, `messages get --ref REF --json` for normalized plain-text detail, and `messages raw --ref REF` for the unmodified RFC 5322 message.

Treat the CLI as a standards boundary, not a proprietary content pipeline. Structured responses are schema-versioned JSON; exact messages are RFC 5322/MIME; recipient strings follow mailbox-address syntax; timestamps are RFC 3339; attachment metadata exposes MIME media type and decoded size; saved files contain the decoded MIME-part bytes and return SHA-256. Attachment IDs are opaque deterministic MIME-part paths and must be passed back unchanged. If `content_complete` is false, do not substitute Mail scripting convenience text or infer missing bytes; inspect `missing_parts`, use `messages raw` when available, or report the message as incomplete.

List, filter, and search page sizes are `1..25`. Use `--limit 25` for throughput and continue with `data.page.next_cursor`; never invent a larger page size.

Use `messages filter --json` for typed constraints and `messages search --query TEXT --json` only when body terms are required. Both accept `--sender`, `--recipient`, `--subject`, `--after`, `--before`, `--read true|false`, `--flagged true|false`, `--attachment true|false`, `--account REF`, and `--mailbox REF`. Attachment-only searches are catalog-fast: a positive catalog count decides both filter directions without reading the message; catalog-zero candidates still read authoritative MIME because the catalog can lag. A negative result is exhaustive only when `data.page.coverage.complete` is true. Body search is a stateless on-demand scan; narrow its scope and set `--max-messages` or `--max-bytes` when the task is bounded. Continue with `data.page.next_cursor`. Inspect `data.page.coverage.complete`, partial and missing source counts, and scan bounds before claiming exhaust…

MailCLI owns no search index and has no refresh command. Its supported reads already use Mail's local store without Apple Events. Never bypass it with raw SQLite, direct private-file traversal, Mail `whose`, or UI-coordinate automation.

Treat returned message references and cursors as opaque and short-lived. MailCLI binds them to the current store and mailbox membership. After copy, move, delete, or provider synchronization, list or search again for a fresh reference before the next action.

## Compose and mutate

Pass long bodies, recipients, and attachment lists as structured JSON through `--input -` or an input file, or use repeatable `--to`, `--cc`, `--bcc`, and `--attach` flags with `--body` or `--body-file`. Never mix JSON input and terminal-native flags. Always include the JSON `body` key, including when an empty body is intentional. Use `--format plain|markdown|html`; Markdown and HTML are stored with a canonical plain-text representation, and HTML is stripped to a non-active allowlist. Keep To, CC, and BCC distinct and never repeat one address across roles. Use an explicit configured `from` address when account choice matters. Draft attachment paths must be absolute. Respect the local-draft limits: 64 KiB subject, 4 MiB body, 200 total recipients, 100 attachments, and 512 MiB attachment bytes.

Create a local review draft with `mailcli drafts create --to recipient@example.com --subject Subject --body 'Readable plain text.' --json` or structured JSON. Use `drafts preview --ref REF --format plain|source|html`, `drafts edit --ref REF`, `drafts inspect --ref REF --json` (returns raw draft fields), or full-replacement `drafts update`. These operations never contact Mail.app and never send mail. A canceled external editor and all descendants owned by that invocation are terminated before the command returns, and the original draft remains unchanged.

After draft workflows, check `drafts list --json` for leftovers: list entries are summaries (ref, subject, recipients, timestamps, format, attachment count, attempt state; never body content, use `drafts inspect` for content): entries with `ever_sent:false` and a high `age_days` are stale never-sent drafts. Remove them with `mailcli drafts prune` (dry run by default; add `--confirm` to delete exactly the listed drafts, `--older-than <days>` to change the 30-day threshold; values below 1 day are rejected as `invalid_argument`). Prune never touches drafts with send or save attempts; reconcile or discard those explicitly instead.

On the verified Mail 16 host, never invoke `drafts save`; capabilities mark it `unsupported`, and it returns `compose_automation_unsupported` before Apple Events. Mail 16 live tests lost reviewed body and recipients, rejected scripted attachments, and created phantom drafts. For a new local draft with no explicit From, CC, or BCC, `drafts handoff --ref REF` may open Apple's visible Compose Email service. It retains the local draft and never sends. It fails if Mail.app is not the default email application, preventing an accidental browser or third-party mail-client launch. Add sender, CC, or BCC manually in Mail.app when needed. Never use handoff for reply/forward threading, and never bypass the safety gate with raw AppleScript, JXA, or a direct provider API.

For replies and forwards, use a fresh store-bound message reference from list or search, then create a local review draft with `messages reply --message MSG_REF --input - [--all] --json` or `messages forward --message MSG_REF --input - --json`. Derivation is automatic: subject gains `Re:`/`Fwd:`, reply recipients come from the source headers (Reply-To preferred, `--all` promotes the other To/CC recipients), and sent replies carry In-Reply-To/References threading. Explicit input values win over derived ones. A forward input must include at least one explicit To, CC, or BCC recipient. Inspect the result. The sent reply threads at the MIME level, but quoted original content and original attachments are never materialized; only your own body is sent.

Use `drafts open --message MSG_REF --json` to inspect an existing persisted Mail draft without opening an editor. Keep edits in the local review draft because Mail 16 has no reliable headless in-place editor.

List received attachments with `attachments list --message MSG_REF --json`. Save one with `attachments save --message MSG_REF --attachment ID --output /absolute/non-existing/path --json`; the command never overwrites an existing path and returns byte count plus SHA-256.

Use explicit `messages mark`, `messages copy`, `messages move`, `messages delete --confirm`, and `sync` commands. Mark, move, copy, and delete execute over IMAP directly without launching Mail.app or consulting the Apple Events fallback. If the local account catalog is incomplete, they fail with typed `account_catalog_incomplete` and remediation to run `mailcli doctor`. They load the same Keychain credential as send; if it is missing they fail with `smtp_credentials_missing` and remediation naming `mailcli send setup`. Each mutation returns typed `server_truth` evidence (command, server response, mailbox, target mailbox, UID). The local store staleness model: mutations apply immediately on the IMAP server; the local read store updates on Mail.app's next background sync. `mailcli sync --check [--account REF] [--json]` reports per-mailbox server-vs-local message-count deltas and unseen counts over IMAP without launching Mail.app; use it to learn about new server mail. `complete:false` means partial coverage, never verified: the `failures` entries name every skipped account or mailbox with its typed code. `mailcli sync` without `--check` asks Mail.app to synchronize the local store and requires Mail.app running; prefer `--check` when only server deltas are needed. Local-only mailboxes (On My Mac) reject mutations with typed code `local_only_mailbox`. Mark, move, and delete reject draft messages unless the user explicitly authorizes draft mutation, every editor for that draft is closed, and `--allow-draft` is supplied; delete still also requires `--confirm`. A server without UID MOVE uses UID EXPUNGE when available; otherwise MailCLI defers EXPUNGE when other deleted UIDs are present and reports `server_truth.expunge_branch:"deferred"` with `foreign_deleted_count`. Do not reinterpret archive as delete or delete as permanent removal. Never send or delete without explicit user authorization. Moving, copying, marking, and synchronization also mutate live Mail state, so require a user request that covers the action.

Never loop live mutations rapidly. Let each MailCLI command finish and inspect its typed result before issuing the next command. The CLI reuses account and mailbox resolution within an invocation and bounds message pages to 25; agents must not add their own polling or raw Apple Events around it.

## Send

Sending is autonomous and never involves Mail.app: `drafts send --ref REF --confirm --json` submits the reviewed draft over SMTP and mirrors it into the account's Sent mailbox over IMAP. There is no Mail.app launch, no Apple Events, no compose window, and no Automation or Full Disk Access requirement; the first keychain read may show one macOS consent prompt.

One-time setup per From address: the user runs `mailcli send setup --from user@example.com` and enters an app-specific password at the no-echo prompt. That credential is used for SMTP send and for IMAP mark/move/copy/delete. MailCLI stores it in the macOS Keychain and never displays, logs, or returns it. `mailcli send setup --from user@example.com --remove` deletes the stored credential. Never ask the user to paste a password into chat or pass one on the command line; direct them to the setup command.

The send flow resolves provider endpoints from the From address, loads the keychain credential, builds the RFC 5322 message with a locally generated Message-ID, submits it over SMTP with STARTTLS, and appends it to the Sent mailbox over IMAP. A confirmed result reports outcome `sent` with the server's final response and Message-ID as evidence. If SMTP accepted the message but the Sent mirror failed, the outcome is `sent_mirror_pending`: the message was delivered, so never resend it; the retained claim stays reconcilable through `drafts reconcile`. Missing credentials fail with `smtp_credentials_missing` and remediation naming `mailcli send setup`. An unsupported or unresolvable From address fails with `transport_unsupported_provider` or `invalid_address`: direct send currently supports Gmail (including googlemail.com) and iCloud (including me.com and mac.com) sender addresses only. Stop and tell the user; do not invent endpoints or edit MailCLI source. Other typed codes surface verbatim: `smtp_auth_failed`, `smtp_rejected`, `smtp_tls_failed`, `smtp_timeout`, `imap_connect_failed`, `imap_auth_failed`, `imap_sent_mailbox_not_found`, `imap_append_failed`, and `imap_timeout`. Sending requires explicit user authorization and `--confirm`; never send without a user request that covers the action.

If a send crashes after SMTP acceptance, the retained claim carries a Message-ID and envelope fingerprint, so `drafts reconcile --ref REF --json` resolves it without resending: a Sent-mailbox match over IMAP upgrades the claim to `sent`, absence returns `send_outcome_unverifiable` with the Message-ID and manual remediation, and retries stay blocked either way (absence in Sent does not prove non-delivery). Legacy claims without a Message-ID stay blocked with `send_reconcile_unavailable`; discard them explicitly after manual verification.

## Output handling

Keep message bodies and addresses out of logs and summaries unless the task requires them. Preserve exact raw output when the user asks for RFC 5322 source. For human-facing drafts, use `drafts preview` and the stored canonical representations. Visible handoff uses sanitized rich content when present; a successful handoff proves only that macOS accepted the visible compose request, never that the message was sent.

Check `content_complete`, `missing_parts`, and search `coverage` before making completeness claims. Targeted fallback content hydrates over IMAP (`content_source: "imap_raw"`) without launching Mail.app or sending Apple Events. If the binary lacks a required command or returns an unsupported store profile, stop and report it. Do not bypass MailCLI with direct store access, raw AppleScript/JXA, or UI coordinates.

## Error contract

Every `--json` response is a single envelope:

```json
{"schema_version":1,"ok":true,"command":"accounts.list","data":{...},"error":null}
```

On failure:

```json
{"schema_version":1,"ok":false,"command":"accounts.list","data":{},"error":{"code":"operation_failed","message":"..."}}
```

Exit codes: `0` = success, `1` = runtime/operation error, `2` = usage/argument error. Always check `ok` and `error.code`, not just the exit code.

Generic error codes and remediation:

- `unknown_command` — the command ID is not recognized; run `mailcli capabilities --json` for the authoritative list.
- `invalid_argument` — a flag or positional argument is wrong (exit 2); the message names the problem.
- `invalid_input` — structured input (stdin/file JSON, body, format) is malformed; the message names the field and constraint.
- `missing_required` — a required flag was omitted; the message names the flag (e.g., `missing required --ref`).
- `confirmation_required` — the action needs `--confirm` after explicit user authorization.
- `operation_failed` — a runtime failure (store, IMAP, SMTP, Mail.app); inspect `error.message` and any typed code in the message. Do not blindly retry; diagnose the cause first.

Typed transport and store codes (`smtp_auth_failed`, `imap_connect_failed`, `mail_busy`, `mail_not_running`, `local_only_mailbox`, etc.) are documented in the relevant sections above and surface verbatim in `error.message` or `error.code`. When a typed code appears, follow its specific remediation rather than retrying with different flags.
