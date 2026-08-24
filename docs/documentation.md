# MailCLI Documentation

- [Architecture](#architecture)
- [CLI contract](#cli-contract)
- [Composition](#composition)
- [Data model](#data-model)
- [Local security and permissions](#local-security-and-permissions)
- [Scope](#scope)
- [Search](#search)
- [Setup and usage](#setup-and-usage)
- [Technical baseline](#technical-baseline)

## Architecture

MailCLI is a local Go executable for the accounts already configured in macOS Mail. It never connects directly to Gmail, iCloud, IMAP, SMTP, OAuth, or account-login endpoints.

The implementation has five boundaries:

1. `cmd/mailcli` owns process startup only.
2. `internal/cli` owns command parsing, validation, output selection, exit codes, and confirmation policy.
3. `internal/mail` owns typed use cases, filters, drafts, send claims, and outcome semantics without platform I/O.
4. `internal/mailstore` owns the zero-Apple-Events read path: strict read-only access to Mail's existing Envelope Index, safe mailbox mapping, `.emlx` parsing, attachment extraction, on-demand search, reference revalidation, and store-based write observation.
5. `internal/mailapp` owns the write boundary. It invokes one embedded JavaScript for Automation bridge through `/usr/bin/osascript`, passes one typed JSON request, and decodes one typed JSON response. `internal/mailref` owns opaque store-bound references and cursors shared by the read and write adapters.

Mail.app remains the source of truth. MailCLI owns no mail index, copied corpus, refresh state, daemon, watcher, mailbox cache, or background process. The local Envelope Index is opened with SQLite `mode=ro`, a private connection cache, `query_only=1`, and WAL participation. `immutable`, `nolock`, journal-mode changes, and every SQL write are forbidden. Before any query, MailCLI verifies the store version, framework version, UUID, required tables, columns, indexes, active-account catalog, mailbox cache, and filesystem containment. An unknown profile fails with `unsupported_mail_store_schema`; MailCLI never guesses a changed private schema.

Metadata listing, filtering, search planning, message detail, raw source, attachment listing, and downloaded attachment extraction use no Apple Events on the supported store path. A targeted Apple Events fallback is allowed only for a single already-resolved message whose body or attachment bytes are not locally complete. Mailbox enumeration and cross-mailbox search never fall back to recursive Mail scripting, and Mail `whose` queries are absent.

Every Apple Events caller acquires one context-aware BSD advisory lock at `~/Library/Application Support/MailCLI/mail-access.lock`. Lock acquisition is capped at two seconds; concurrent callers return `mail_busy` before contacting Mail. The gate requires an already-running Mail process and binds the bridge to that exact PID rather than addressing Mail by application name, so MailCLI never starts, activates, quits, kills, or restarts Mail, even if Mail exits between the process check and the Apple Event. Each bridge invocation owns one private `osascript` process group, waits for it, and reaps it before releasing the gate. Context cancellation kills that process group and still waits for process cleanup. A timed-out operation records the exact target Mail PID and later live operations fail with `mail_recovery_required` until that process has been replaced; a replacement Mail PID is never incorrectly marked uncertain. A timeout is never treated as Apple Event cancellation because Mail may continue work after the caller exits. Mutations use direct references, revalidate them immediately before acting, and report observed state rather than assuming success.

The embedded bridge creates one request-local resolution context. Enabled accounts are fetched at most once per invocation, and repeated account or mailbox references reuse the resolved objects. Message listing validates its page limit of `1..25` before accessing Mail. Direct message operations resolve one account, one mailbox path, and one message ID; Mail `whose` queries and mailbox-wide `messages()` reads are forbidden by tests. Send and mutation outcome observation happens outside Mail through exponentially backed-off read checks, retaining immediate verification while avoiding fixed 100/200 ms polling for the full observation window.

## CLI contract

The CLI is optimized for both humans and agents:

- Stable nouns and verbs; no interactive menu.
- JSON is available on every data-bearing command through `--json` and uses `schema_version=1`.
- Standard input accepts structured payloads for long bodies and recipient lists, avoiding shell quoting problems.
- Human output is concise; message bodies and raw MIME are written only when explicitly requested.
- Pagination is explicit through `--limit` and `--cursor`; list, filter, and search accept page sizes from 1 through 25 and always return pages at `data.page`.
- Times are emitted as RFC 3339 with an offset; sizes are bytes.
- Success uses exit code `0`, runtime or Mail.app failure uses `1`, and invalid CLI usage uses `2`.
- Destructive operations require an explicit command and confirmation flag. Sending is draft-first.

Command surface:

| Command | Status | Purpose |
|---|---|---|
| `mailcli doctor` | Implemented | Validate platform, Mail.app, scripting support, permissions, and optional live access |
| `mailcli accounts list` | Implemented | List enabled local Mail accounts and sender identities |
| `mailcli mailboxes list` | Implemented | Recursively list every mailbox with stable account-relative paths |
| `mailcli mailboxes resolve` | Implemented | Resolve an exact account-relative mailbox path to its stable reference |
| `mailcli messages list` | Implemented | Page through a mailbox without loading full bodies |
| `mailcli messages filter` | Implemented | Apply typed filters through the local store and authoritative message sources |
| `mailcli messages search` | Implemented | Run metadata search or bounded on-demand body search across selected scope |
| `mailcli messages get` | Implemented | Read normalized metadata, recipients, body, and attachment metadata |
| `mailcli messages raw` | Implemented | Return the exact raw RFC 5322 source stored locally by Mail.app |
| `mailcli attachments list/save` | Implemented | Inspect or save a received attachment to an explicit non-existing destination |
| `mailcli drafts create/list/inspect/update` | Implemented | Manage private, structured, reviewable local drafts |
| `mailcli drafts save/open` | Implemented | Persist a reviewed draft in Mail.app or inspect an existing Mail.app draft headlessly |
| `mailcli drafts send/discard` | Implemented | Confirm and send through Mail.app or remove only the local draft |
| `mailcli drafts reconcile` | Implemented | Recheck a retained uncertain send against Sent without sending again |
| `mailcli messages reply` | Implemented | Create a local reply or reply-all draft bound to the source message |
| `mailcli messages forward` | Implemented | Create a local forward draft bound to the source message |
| `mailcli messages mark` | Implemented | Change and read back read, flagged, or junk state |
| `mailcli messages move` | Implemented | Move and re-reference a message in a resolved mailbox |
| `mailcli messages copy` | Implemented | Copy and reference a message in a resolved mailbox |
| `mailcli messages delete` | Implemented | Invoke Mail.app's configured deletion behavior after confirmation |
| `mailcli sync` | Implemented | Ask Mail.app to check all mail or synchronize one selected account |

Only commands shown by the installed binary's `mailcli help` are executable.

Machine responses use one envelope:

```json
{
  "schema_version": 1,
  "ok": true,
  "command": "accounts.list",
  "data": {"accounts": []},
  "error": null
}
```

Errors use stable codes and actionable messages. Partial batch results must report each failed item and make the top-level `ok` value false.

## Composition

Draft creation accepts a typed JSON document on standard input with sender identity, To, CC, BCC, subject, body, and attachment paths. Recipient fields are arrays of `{name, address}` objects. Attachment paths must be absolute regular files. MailCLI records their sizes and SHA-256 digests when the draft is created or updated and rejects sending if any attachment changed during review.

Drafts are private JSON files under `~/Library/Application Support/MailCLI/drafts`, not fragile unsaved Mail compose objects. The normal flow is `drafts create` -> `drafts inspect` -> optional `drafts update` -> `drafts send --confirm`. Replies and forwards retain an opaque source reference; at send time Mail creates the native reply or forward so threading, quoted content, and original forward attachments remain under Mail's control. MailCLI prepends the reviewed plain-text body and adds the reviewed recipients and attachments.

Confirmed send is at-most-once. Before contacting Mail, MailCLI captures a Sent-store baseline and persists a durable send claim in the local draft. It then invokes Mail exactly once and observes Sent for up to ten seconds using a unique fingerprint over sender, separate To/CC/BCC sets, subject, normalized body prefix, and attachment name, size, and SHA-256. The result is one of `sent_store_observed`, `accepted_by_mail`, or `outcome_unknown`. Only `sent_store_observed` removes the local draft. Any accepted or uncertain attempt remains claimed and cannot be sent again; repeated `drafts send` calls replay the recorded outcome without Apple Events. `drafts reconcile --ref` performs a store-only observation pass and never invokes send.

`drafts save --ref` explicitly exports a local review draft to Mail's Drafts mailbox. It requires an exact configured `from` identity and refuses with `compose_busy` if Mail already has a compose object, so a user-owned window is never closed or mixed into the operation. MailCLI captures a Drafts-store baseline, invokes one native save, and verifies one unique resulting message from the store. If Mail accepted the save but no unique result appears, it returns `draft_outcome_unknown` and retains the local draft; agents must not retry automatically. Mail 16 can retain the invisible compose backend after saving, so another native save in the same Mail session may return `compose_busy`. MailCLI never restarts Mail to hide this platform behavior. Gmail may rotate temporary identifiers during synchronization; `drafts open` can recover only within the already resolved Drafts mailbox using the bound subject and rejects ambiguity.

`drafts open --message` inspects a resolved Mail.app draft without opening a compose window; body, recipients, headers, and attachment metadata are returned through the normal typed message shape. Editing remains in the deterministic local review lifecycle before `drafts save`. Mail 16 exposes no reliable headless in-place editor for an already persisted draft: its `open` handler opens a compose window. MailCLI does not hide that UI side effect or claim unsupported in-place editing.

Plain text is the guaranteed outbound format because it is portable, readable, inspectable before sending, and fully represented by Mail's scripting interface. New messages preserve the supplied body string; native replies and forwards place the supplied text before Mail's generated quoted content. Outbound attachments use Mail's documented rich-text attachment insertion. Arbitrary HTML is unsupported: Mail 16 declares its scripting `html content` property deprecated and ineffective.

## Data model

Account, mailbox, message, recipient, attachment, draft, and cursor are typed domain values. Message references are opaque and bind the Envelope Index UUID, row identity, message/global identifiers, account, mailbox identity, mailbox path, and subject. The store revalidates physical identity and current mailbox membership before every read or write translation. A changed store, moved message, reused row, or mismatched subject produces a typed stale-reference error instead of accessing a different message.

Mailbox paths are account-relative arrays internally and escaped display strings externally. This prevents collisions between Gmail labels, iCloud folders, and identically named nested mailboxes.

List and search cursors bind the store UUID, query or mailbox fingerprint, sort anchor, and row ID. They cannot be reused after a store replacement or with different filters. Message detail distinguishes normalized plain content, raw source, headers, attachment metadata, `content_source`, `content_complete`, and `missing_parts`. Large bodies, raw MIME, and attachment bytes are never included in list responses.

## Local security and permissions

MailCLI reuses Mail.app's configured accounts and Keychain-backed authentication. It does not request, read, store, log, or transmit account passwords, OAuth tokens, cookies, or app-specific passwords.

macOS may show a one-time Automation consent prompt when `mailcli doctor --live` or the first write sends an Apple Event. The calling host, such as Terminal or Codex, must be allowed to control Mail in System Settings for those operations.

The calling host needs Full Disk Access to read `~/Library/Mail`; this is the one-time permission that enables fast zero-Apple-Events reads. Automation permission is required only for `doctor --live`, targeted incomplete-content fallback, native draft export, send, message mutations, and sync. Accessibility and Screen Recording are never required. When permission is missing, `doctor` returns the exact System Settings remediation and does not weaken the read-only boundary.

Structured output excludes body content unless the command requests it. Diagnostics avoid subjects, bodies, headers, recipient lists, and attachment bytes unless needed to identify the failed operation. MailCLI persists only review drafts and the cross-process access-gate state under `~/Library/Application Support/MailCLI`; it persists no mail corpus or search index. Draft directories use mode `0700` and files use mode `0600`. `drafts discard --confirm` removes only the named local draft.

## Scope

The supported scope is every active account and every mailbox represented consistently by Mail's Envelope Index and mailbox catalog, including Inbox, Sent, Drafts, Archive, Junk, Trash, custom folders, and nested Gmail labels. `mailboxes resolve` accepts one exact `--path` segment per hierarchy level, so localized folders such as `Gesendet` and `Entwürfe` require no guessed identifier. Full and partial local message sources are reported truthfully. A targeted fallback may ask Mail for one uncached body or attachment, but MailCLI never starts Mail to do so.

The implemented surface includes account and mailbox discovery, paginated listing, normalized and raw reading, attachment inspection and saving, cross-mailbox search, draft creation, reply, reply-all, forward, send, copy, move, delete, read/unread, flag/unflag, junk state, and account synchronization.

Account creation, login, password management, direct provider APIs, Mail-store mutation, UI-coordinate automation, Mail rules administration, and permanent trash emptying are out of scope. The private store adapter is strictly read-only and supports only the exact capability-gated profile. Permanent irreversible deletion may be added only as a separate explicitly confirmed capability.

## Search

MailCLI does not execute Mail.app `whose` searches. Those Apple Events can trigger unbounded mailbox work that is not reliably cancelled when the caller exits. Mailbox scoping reduces the amount of work but does not provide a hard execution bound, so the production bridge forbids this search form completely.

`messages filter` and `messages search` without `--query` plan candidates through one parameterized query against the existing Envelope Index. Account, mailbox, sender, recipient, subject, date, read, and flagged constraints are resolved entirely in that metadata query. The `--attachment` constraint additionally inspects each candidate's authoritative MIME source because Mail's attachment catalog can lag behind a fully downloaded message. Positive catalog evidence may prove `--attachment true`; proving `--attachment false` requires a complete source. Partial or missing sources are excluded as unknown and make coverage incomplete. Results use deterministic received-date and row-ID ordering. Query-bound cursors reject filter changes and store replacement.

`messages search --query TEXT` performs an on-demand, stateless MIME scan over only the metadata candidates in scope. Two bounded workers stream each authoritative `.emlx` source, decode text/plain or text/html, discard attachment payloads without hashing them, and retain no corpus after the process exits. `--max-messages` defaults to 50,000 and is capped at 100,000; `--max-bytes` defaults to 4 GiB and is capped at 8 GiB. These limits bound work rather than pretending that arbitrary full-text search is instant.

Every search page includes `data.page.coverage`: backend, candidate messages, scanned messages and bytes, full sources, partial sources, missing sources, and `complete`. Metadata-only results are complete when the store query succeeds. Body results report incomplete coverage when a source is partial, missing, or the requested scan bound was reached. Pagination over body hits also reports page-level incompleteness until the final page, so an agent cannot mistake a page for an exhaustive result. No refresh command exists because MailCLI maintains no index.

## Setup and usage

Requirements are macOS on Apple silicon, Go 1.27 or newer for development, `/System/Applications/Mail.app`, `/usr/bin/osascript`, and at least one account already configured in Mail.app. Grant Full Disk Access to the calling host for reads. Grant Automation access to Mail only when live diagnostics, targeted fallback, or writes are needed.

```bash
./scripts/tests/test.sh
./scripts/build/build.sh
./scripts/tests/test-live-responsiveness.sh
./scripts/build/install-local.sh
command -v mailcli
mailcli doctor --live --json
./bin/mailcli doctor
./bin/mailcli doctor --live
./bin/mailcli version --json
./bin/mailcli accounts list --json
./bin/mailcli mailboxes list --json
./bin/mailcli mailboxes resolve --account ACCOUNT_REF --path Gesendet --json
./bin/mailcli messages list --mailbox MBX_REF --limit 10 --json
./bin/mailcli messages filter --mailbox MBX_REF --read false --attachment true --json
./bin/mailcli messages get --ref MSG_REF --json
./bin/mailcli messages raw --ref MSG_REF
./bin/mailcli attachments list --message MSG_REF --json
./bin/mailcli attachments save --message MSG_REF --attachment ATTACHMENT_ID --output /absolute/path/file.pdf --json
printf '%s' '{"from":"me@example.com","to":[{"address":"recipient@example.com"}],"cc":[],"bcc":[],"subject":"Subject","body":"Readable plain text.\n","attachments":[]}' | ./bin/mailcli drafts create --input - --json
./bin/mailcli drafts inspect --ref DRAFT_REF --json
./bin/mailcli drafts save --ref DRAFT_REF --json
./bin/mailcli drafts open --message MAIL_DRAFT_MESSAGE_REF --json
./bin/mailcli drafts send --ref DRAFT_REF --confirm --json
./bin/mailcli drafts reconcile --ref RETAINED_DRAFT_REF --json
printf '%s' '{"body":"Thanks.\n"}' | ./bin/mailcli messages reply --message MSG_REF --input - --all --json
./bin/mailcli messages mark --ref MSG_REF --read true --flagged false --json
./bin/mailcli messages move --ref MSG_REF --mailbox MBX_REF --json
./bin/mailcli messages delete --ref MSG_REF --confirm --json
./bin/mailcli sync --json
./bin/mailcli messages search --query "project update" --after 2026-08-01 --json
./bin/mailcli messages search --sender example.com --attachment true --json
```

The main test gate validates the syntax and executable bit of every repository shell script before running the Go quality and race gates. Build the binary before invoking `test-live-responsiveness.sh`; that opt-in gate requires an already-running Mail process and Automation permission. `doctor` is non-invasive and checks the read store without Apple Events. `doctor --live` asks an already-running Mail process only for its version and may trigger the one-time Automation prompt; when Mail is closed it returns `mail_not_running` and does not launch it. Body search is bounded per invocation with `--max-messages` and `--max-bytes` and persists nothing. Sending and deletion require `--confirm`; creating, inspecting, and updating a local draft never sends. After `accepted_by_mail` or `outcome_unknown`, never retry send: use `drafts reconcile`. Moving, copying, marking, synchronizing, native draft saving, and confirmed sending or deletion are live Mail operations and should be invoked only when the user requested that mutation.

## Technical baseline

The verified development host is macOS 15.6.1 on `darwin/arm64`, Mail 16.0 build 3826.700.81, and Go 1.27.0. The supported Envelope Index profile is store version `4`, minor version `74003`, framework version `3826.700.81`, WAL journal mode, and a valid store UUID. Any profile drift fails closed before message queries.

Directory enumeration is never authoritative because Mail can retain stale or partial filesystem sources. Store rows select candidates, safe mailbox mapping resolves their sources, and every result reports whether the corresponding local content is complete. Spotlight is not a required dependency; full-text search uses the deterministic on-demand MIME scanner and never creates another persistent index.

Mailbox catalogs are parsed in-process from bounded, symlink-free XML property lists. Only Mail's binary account-ordering preference requires one bounded `plutil` extraction during startup; mailbox enumeration launches no per-account conversion processes.

Release acceptance requires isolated process-inclusive metadata-list p95 below 50 ms and metadata-search p95 below 100 ms on the supported host profile. Direct-store list, filter, and search operations must produce no measurable Mail process CPU increase. On-demand body search is separately bounded by candidate and byte limits because its cost depends on the selected corpus and source completeness.

The implementation uses `github.com/emersion/go-message` for streaming RFC 5322/MIME parsing, `golang.org/x/net/html` for safe text extraction, `golang.org/x/sync/errgroup` for the bounded two-worker body scan, Unicode normalization from `golang.org/x/text`, and `github.com/mattn/go-sqlite3` for one strict read-only SQLite connection. MailCLI binds only explicit writes and targeted incomplete-content fallback to the installed Mail scripting definition. Cross-process serialization uses BSD `flock`; `osascript` runs in an owned process group and is synchronously reaped. No service, daemon, cache, watcher, child process, or goroutine survives a CLI invocation.

The release target is a native `darwin/arm64` binary. Cross-compilation alone is insufficient acceptance evidence because the local store profile, Full Disk Access, Apple Events permission, Mail 16 composition behavior, attachment bytes, send observation, and process non-interference must be exercised on the installed host.
