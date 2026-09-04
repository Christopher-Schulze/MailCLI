# MailCLI Documentation

- [Architecture](#architecture)
- [CLI contract](#cli-contract)
- [Composition](#composition)
- [Data model](#data-model)
- [Local security and permissions](#local-security-and-permissions)
- [Release distribution](#release-distribution)
- [Scope](#scope)
- [Search](#search)
- [Setup and usage](#setup-and-usage)
- [Technical baseline](#technical-baseline)

## Architecture

MailCLI is a local Go executable for the accounts already configured in macOS Mail. Complete local reads never contact Gmail, iCloud, IMAP, SMTP, OAuth, or account-login endpoints; they use Mail's Envelope Index and `.emlx` sources. Incomplete-content hydration, mailbox mutations, `sync --check`, and sending use the provider's IMAP and SMTP endpoints with an app-specific password the user stores once in the macOS Keychain. MailCLI never uses OAuth and never asks for passwords in chat.

The implementation has these package boundaries:

1. `cmd/mailcli` owns process startup only.
2. `internal/cli` owns command parsing, validation, output selection, exit codes, and confirmation policy.
3. `internal/mail` owns typed use cases, filters, drafts, send claims, and outcome semantics without platform I/O.
4. `internal/mailstore` owns the zero-Apple-Events read path: strict read-only access to Mail's existing Envelope Index, safe mailbox mapping, `.emlx` parsing, attachment extraction, on-demand search, reference revalidation, and store-based write observation.
5. `internal/mailapp` owns the optional Mail.app integration: live environment diagnostics (`doctor --live`), legacy draft inspection (`drafts open`), and triggering Mail.app's local sync (`sync` without `--check`).
6. `internal/compose` owns the visible AppKit compose handoff (`drafts handoff`) without Apple Events or sending.
7. `internal/transport` owns network transport: provider endpoint resolution, direct SMTP submission with STARTTLS, IMAP Sent mirroring on its own dedicated connection, pooled IMAP mutations and hydration connections (one authenticated TLS session per account per invocation; repeated operations reuse it, repeated commands on the same mailbox skip the SELECT round trip, IO failures transparently reconnect, and `sync --check` checks every mailbox of an account over that single connection), autonomous IMAP message mutations (STORE flags, COPY, MOVE with COPY+EXPUNGE fallback, DELETE-to-Trash), bounded IMAP hydration (FETCH BODY.PEEK[]), and server delta checks (STATUS).

`internal/mailref` owns opaque store-bound references and cursors shared by the read, store, and transport adapters. `internal/keychain` stores the per-account app-specific password in the macOS Keychain under the `mailcli-smtp` service.

The local Mail.app SQLite Envelope Index remains the source of truth for reads, while mutations and sends execute directly over IMAP and SMTP. MailCLI owns no mail index, copied corpus, refresh state, daemon, watcher, mailbox cache, or background process. The local Envelope Index is opened with SQLite `mode=ro`, a private connection cache, `query_only=1`, and WAL participation. `immutable`, `nolock`, journal-mode changes, and every SQL write are forbidden. One verified Mail-store directory descriptor anchors message, mailbox-cache, and external-attachment opens; macOS `O_NOFOLLOW_ANY` rejects a symlink in any descendant component, and later attachment hashing or copying must reopen the exact selected regular-file identity.

Metadata listing, filtering, search planning, message detail, raw source, attachment listing, and downloaded attachment extraction use no Apple Events. Incomplete-content hydration uses bounded IMAP FETCH over the account's transport connection without launching Mail.app. Attachment saving first copies a matching locally materialized external file; only a missing local attachment falls back to full-message IMAP hydration under the same 64 MiB cap. Mutations (`messages mark`, `messages move`, `messages copy`, `messages delete`) execute over IMAP directly and return typed server-truth evidence. Mailbox enumeration and cross-mailbox search never fall back to recursive Mail scripting, and Mail `whose` queries are absent.

Every Apple Events caller acquires one context-aware BSD advisory lock at `~/Library/Application Support/MailCLI/mail-access.lock`. Lock acquisition is capped at two seconds; concurrent callers return `mail_busy` before contacting Mail. The gate requires an already-running Mail process, checks its bundle identity as `com.apple.mail`, and binds the bridge to that exact PID rather than addressing Mail by application name. Before a compose or sync operation can invoke `osascript`, the gate writes and synchronizes an exact-PID recovery marker while still holding the lock; a write or sync failure prevents the Apple Event from starting. A definite bridge completion clears and synchronizes that state. An incomplete call or caller crash leaves the already-durable marker, so later live operations fail with `mail_recovery_required` until the affected Mail process has been replaced. MailCLI never starts, activates, quits, kills, or restarts Mail, even if Mail exits between the process check and the Apple Event. Each bridge invocation owns one private `osascript` process group, waits for its leader, terminates any remaining owned group members, and verifies group absence before releasing the gate. SIGINT, SIGTERM, and context cancellation write a separate private bridge marker; the script checks it before mutation and between bounded compose snapshots, requests closure of its owned unsent compose object, and verifies that the object no longer exists. A retained backend returns `compose_cleanup_failed` and leaves recovery latched. Only a bridge that exceeds the 15-second cleanup grace is force-stopped. Incomplete reads, live probes, sync triggers, and Automation denial never create false recovery state. Message mutations execute over IMAP and never touch the Apple Events gate; they use store-bound identity, include the locally verified RFC Message-ID when present, and return typed server-truth evidence.

The embedded bridge creates one request-local resolution context. Enabled accounts are fetched at most once per invocation, and repeated account or mailbox references reuse the resolved objects. Message listing validates its page limit of `1..25` before accessing Mail. Direct message operations resolve one account, one mailbox path, and one message ID; Mail `whose` queries and mailbox-wide `messages()` reads are forbidden by tests. Production Mail 16 clients reject scripted draft save and outbound attachment insertion with `compose_automation_unsupported` before acquiring the Apple Events gate; sending uses `internal/transport` and never touches the Apple Events gate. A separate AppKit handoff uses the visible Compose Email sharing service without Apple Events or sending. The retained scripted compose implementation is reachable only through injected test clients and remains covered by lifecycle, resource-bound, visibility, cleanup, and at-most-once regression tests. Message-mutation outcome observation happens outside Mail through exponentially backed-off store checks.

## CLI contract

The CLI is optimized for both humans and agents:

- Stable nouns and verbs; no interactive menu.
- JSON is available on every data-bearing command through `--json` and uses `schema_version=1`.
- Standard input accepts structured payloads for long bodies and recipient lists, avoiding shell quoting problems.
- Human output is concise; message bodies and raw MIME are written only when explicitly requested.
- Focused help accepts `help`, `-h`, and `--help`; options use aligned long names, semantic value placeholders, and readable defaults.
- Pagination is explicit through `--limit` and `--cursor`; list, filter, and search accept page sizes from 1 through 25 and always return pages at `data.page`.
- Times are emitted as RFC 3339 with an offset; sizes are bytes.
- Success uses exit code `0`, runtime or Mail.app failure uses `1`, and invalid CLI usage uses `2`.
- Destructive operations require an explicit command and confirmation flag. `drafts send` requires `--confirm` and delivers over SMTP/IMAP without Mail.app.
- `mailcli capabilities --json` is the authoritative discovery contract. It reports schema and release identity, every command ID, read/write class, confirmation requirement, Mail-store and Mail.app dependencies, result states, and hard limits without opening the Mail store or contacting Mail.app.
- Capability discovery, local draft create/list/inspect/preview/edit/update/discard/prune, sending, credential setup, the unsupported native save preflight, and missing or unknown command/subcommand routes bypass Mail-store configuration, SQLite, `plutil`, and Mail.app initialization. Reply and forward creation read the source message's header block from the Mail store (they require it, like reads) but write only local draft files. Visible handoff reads only the local draft and invokes AppKit. `drafts reconcile` and `drafts open` still need the Mail store.

Command surface:

| Command | Status | Purpose |
|---|---|---|
| `mailcli capabilities` | Implemented | Return the versioned machine-readable command, effect, dependency, result, and limitation contract |
| `mailcli update` | Implemented | Verify a pinned Ed25519 release signature and checksum, then install with rollback |
| `mailcli doctor` | Implemented | Validate platform, Mail.app, scripting support, permissions, and optional live access |
| `mailcli send setup` | Implemented | Store or remove the per-account app-specific SMTP password in the macOS Keychain |
| `mailcli accounts list` | Implemented | List enabled local Mail accounts and sender identities; a single broken account stays listed with `state:"degraded"` plus a `degraded_reason` (`mailbox_cache_unreadable`, `special_use_mailbox_unresolved`, `no_provably_sent_identity`) instead of aborting discovery; hard SQL failures still fail the command |
| `mailcli mailboxes list` | Implemented | Recursively list every mailbox with stable account-relative paths |
| `mailcli mailboxes resolve` | Implemented | Resolve an exact account-relative mailbox path to its stable reference |
| `mailcli messages list` | Implemented | Page through a mailbox without loading full bodies |
| `mailcli messages filter` | Implemented | Apply typed filters through the local store and authoritative message sources |
| `mailcli messages search` | Implemented | Run metadata search or bounded on-demand body search across selected scope |
| `mailcli messages get` | Implemented | Read normalized metadata, recipients, body, and attachment metadata |
| `mailcli messages raw` | Implemented | Return the exact raw RFC 5322 source stored locally by Mail.app |
| `mailcli attachments list/save` | Implemented | Inspect or save a received attachment to an explicit non-existing destination |
| `mailcli drafts create/list/inspect/preview/edit/update/handoff/open/send/reconcile/discard/prune` | Implemented | Manage plain/Markdown/safe-HTML drafts, visibly hand new drafts to Mail.app, inspect persisted drafts, send over SMTP, reconcile claims, discard, and prune stale drafts |
| `mailcli drafts save` | Blocked on Mail 16 | Return `compose_automation_unsupported` before contacting Mail.app |
| `mailcli drafts open` | Implemented | Inspect an existing Mail.app draft headlessly |
| `mailcli drafts send` | Implemented | Submit the reviewed draft over SMTP and mirror it into Sent over IMAP after `--confirm` |
| `mailcli drafts discard` | Implemented | Remove only the selected local review draft after confirmation |
| `mailcli drafts reconcile` | Implemented | Recheck a retained uncertain send against Sent without sending again |
| `mailcli drafts prune` | Implemented | List dry and, with `--confirm`, delete stale never-sent local drafts |
| `mailcli messages reply` | Implemented | Create a local reply or reply-all draft bound to the source message |
| `mailcli messages forward` | Implemented | Create a local forward draft bound to the source message |
| `mailcli messages mark` | Implemented | Change read, flagged, or junk state over IMAP and read back the result |
| `mailcli messages move` | Implemented | Move a message to a resolved mailbox over IMAP |
| `mailcli messages copy` | Implemented | Copy a message to a resolved mailbox over IMAP |
| `mailcli messages delete` | Implemented | Delete a message over IMAP by moving it to the Trash mailbox after confirmation |
| `mailcli sync` | Implemented | `--check` reports server-vs-local deltas over IMAP without Mail.app; skipped mailboxes appear as `failures` entries with `complete:false`; without `--check` asks Mail.app to synchronize |

Agents must discover support from `mailcli capabilities --json`, never by parsing help text. The capability manifest explicitly reports that MailCLI owns no mail index or background process, can read raw MIME and send raw MIME it composes itself (`raw_mime_read:true`, `raw_mime_send:true`), has `compose_write:false`, `compose_attachment_write:false`, and `send_transport:"smtp"`, limits pages to 25 messages, raw fallback to 64 MiB, reviewed draft subjects to 64 KiB, bodies to 4 MiB, recipients to 200, attachments to 100, and attachment bytes to 512 MiB. The 64 MiB raw-source cap binds both raw-source paths: the local store rejects oversized sources and IMAP fetch refuses oversized literals with `raw_source_too_large` before buffering (ordinary content hydration stays uncapped on both paths; attachment hydration uses the same cap). `mailcli help` is a compact human command overview; focused flags remain under `mailcli <command> --help`.

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

Errors use stable codes and actionable messages. Partial batch results must report each failed item and make the top-level `ok` value false. Generic error codes: `unknown_command` (unrecognized command ID), `invalid_argument` (wrong flag or positional argument, exit 2), `invalid_input` (malformed structured input such as stdin JSON, body, or format, exit 2), `missing_required` (a required flag was omitted, exit 2), `confirmation_required` (the action needs `--confirm` after explicit user authorization), `operation_failed` (a runtime failure such as store, IMAP, SMTP, or Mail.app, exit 1). Typed transport and store codes (`smtp_auth_failed`, `imap_connect_failed`, `mail_busy`, `mail_not_running`, `local_only_mailbox`, etc.) surface verbatim in `error.code` or `error.message`; follow their specific remediation rather than retrying with different flags.

## Composition

Draft creation accepts either a typed JSON document or terminal-native `--from`, repeatable recipient/attachment flags, subject, `--body`/`--body-file`, and `--format plain|markdown|html`. JSON and native modes are mutually exclusive; the JSON `body` key is mandatory even when its intentional value is empty. Markdown is rendered with Goldmark. HTML is parsed in-process, reduced to a strict element/link allowlist with all active and remotely loaded content removed, and stored with a canonical plain-text representation. Recipient fields are arrays of `{name, address}` objects; the same normalized address cannot occur more than once across To, CC, and BCC. Hard resource limits are 64 KiB of subject text, 4 MiB of body text, 200 total recipients, 100 attachments, and 512 MiB of attachment bytes. Attachment paths must be absolute regular files. MailCLI rejects an oversized file before hashing and records accepted sizes plus SHA-256 digests in the local review draft.

Reply and forward drafts derive from the source message's stored header block (header-only read, no body scan): subject becomes `Re: <subject>` or `Fwd: <subject>` (stacked existing prefixes collapse to one), reply targets Reply-To falling back to From, and reply-all promotes the other To/CC recipients into CC while excluding the reply target. Explicit input flags win over every derived value, so callers can override recipients or subject. The draft stores the source message ID and thread chain; sending emits `In-Reply-To` and `References` (chain capped at the newest 20 entries, reply only) and forwards carry no threading headers. Reply requires a resolvable reply target; control characters in thread headers are rejected. `messages reply` and `messages forward` require a fresh store-bound message reference and the Mail store.

Drafts are private JSON files under `~/Library/Application Support/MailCLI/drafts`, not fragile unsaved Mail compose objects. Draft creation and updates both write to a temporary file and atomically rename it into place, so a crashed process never leaves a partial `draft_*.json`. `drafts list` returns summaries (ref, subject, recipients, timestamps, format, attachment count, send/save attempt state; never body content) without re-rendering bodies, so a draft with a corrupt body still appears and only `drafts inspect` enforces canonical validation. It skips any unreadable draft file rather than failing the entire listing. `drafts preview` renders plain, source, or sanitized HTML without fetching remote resources. `drafts edit` writes a mode-0600 temporary JSON file, invokes the selected editor directly without a shell and in a private process group, validates the complete result, and only then atomically replaces the draft. Cancellation gracefully terminates that group, force-cleans resistant descendants after a bounded grace period, verifies group absence, and leaves the original draft unchanged. `drafts handoff` verifies attachment fingerprints and invokes `NSSharingServiceNameComposeEmail` on the macOS main thread. It waits for the sharing-service delegate to confirm the handoff instead of treating invocation as success, opens a visible compose, retains the local draft, never sends, and fails unless Mail.app is the current `mailto:` application. Because AppKit exposes recipients as one role-less array and no sender or reply-thread control, handoff supports new drafts with To recipients only and rejects explicit From, CC, BCC, reply, and forward semantics. Replies and forwards remain local review drafts.

Live verification against Mail 16.0 build `3826.700.81` proved that scripted compose setters can return success while body and recipient values are later missing, attachment insertion can fail with Apple Event `-10000`, automatic save can persist only a signature, and `close saving no` can leave an invisible outgoing backend. Visible compose automation also created signature-only phantom drafts. These are data-integrity failures, not cosmetic limitations.

Production `drafts save` therefore returns `compose_automation_unsupported` before baseline capture, claim creation, gate acquisition, or any Apple Event. This also prevents a false `compose_busy` result caused by an unreadable window property. Capabilities report `compose_write:false`, `compose_attachment_write:false`, `raw_mime_send:true`, and `send_transport:"smtp"`. Historical send/save claims remain readable through reconciliation code, but store observation succeeds only with exact final native headers, body, recipient roles, and attachment count; a missing body snapshot or a prefix-only match fails closed. No new Mail 16 compose operation can be created.

`drafts send --ref REF --confirm` bypasses Mail.app entirely. It resolves the provider's SMTP and IMAP endpoints from the From address, loads the app-specific password from the macOS Keychain (`smtp_credentials_missing` with remediation naming `mailcli send setup` when absent), builds the RFC 5322 message with a locally generated Message-ID, submits it over SMTP with STARTTLS, and appends it to the Sent mailbox over IMAP. A confirmed result reports outcome `sent` with the server's final SMTP response and Message-ID as evidence. If SMTP accepted the message but the Sent mirror failed, the outcome is `sent_mirror_pending`: the message was delivered, MailCLI never resends it, and the retained claim stays reconcilable. Typed transport codes surface verbatim: `smtp_auth_failed`, `smtp_rejected`, `smtp_tls_failed`, `smtp_timeout`, `imap_connect_failed`, `imap_auth_failed`, `imap_sent_mailbox_not_found`, `imap_append_failed`, and `imap_timeout`. Sending requires no Full Disk Access or Automation permission; the first keychain read may show one macOS consent prompt.

Crash recovery closes the submit-to-record window: the send claim is written before submission and already carries the generated `message_id` plus an envelope fingerprint (SHA-256 over Message-ID, sender, recipients, subject, and body). If the process dies after SMTP acceptance but before the claim is updated, `drafts reconcile` finds the claim outcome `outcome_unknown` with a Message-ID and verifies it against the Sent mailbox over IMAP: a match upgrades the claim to `sent` (mailbox recorded in the transport evidence), absence returns typed `send_outcome_unverifiable` with Message-ID, attempt timestamp, recipients, and manual remediation, and a fingerprint mismatch returns `send_fingerprint_mismatch`. Absence in Sent never triggers an automatic retry (at-most-once is preserved); legacy claims without a Message-ID stay blocked with a typed `send_reconcile_unavailable` error naming the attempt start.

`drafts open --message` inspects a resolved Mail.app draft without opening a compose window; body, recipients, headers, and attachment metadata are returned through the normal typed message shape. Mail 16 exposes no reliable headless in-place editor for an already persisted draft.

Mark, move, and delete reject messages identified as drafts unless `--allow-draft` is explicit. The caller must first close any Mail editor for that draft because Mail itself can recreate a moved or deleted draft when an open editor later saves. Deletion additionally requires `--confirm`. Copy does not alter the source draft and therefore does not require the draft mutation flag.

## Data model

Account, mailbox, message, recipient, attachment, draft, and cursor are typed domain values. Message references are opaque and bind the Envelope Index UUID, row identity, message/global identifiers, account, mailbox identity, mailbox path, and subject. The store revalidates physical identity and current mailbox membership before every read or write translation. A changed store, moved message, reused row, or mismatched subject produces a typed stale-reference error instead of accessing a different message.

Mailbox paths are account-relative arrays internally and escaped display strings externally. This prevents collisions between Gmail labels, iCloud folders, and identically named nested mailboxes.

List and search cursors bind the store UUID, query or mailbox fingerprint, sort anchor, and row ID. They cannot be reused after a store replacement or with different filters. Message detail distinguishes normalized plain content, raw source, headers, attachment metadata, `content_source`, `content_complete`, and `missing_parts`. MIME parsing selects one representation from each `multipart/alternative`, preserves mixed-part order, and marks malformed recipient or decoding data incomplete. Large bodies, raw MIME, and attachment bytes are never included in list responses.

## Local security and permissions

MailCLI reuses Mail.app's configured accounts and Keychain-backed authentication for reads and mutations. It does not request, read, log, or transmit account passwords, OAuth tokens, or cookies. The one stored secret is the per-account app-specific SMTP password that `mailcli send setup` writes to the macOS Keychain (`mailcli-smtp` service) at the user's no-echo prompt; MailCLI never displays it, includes it in output or logs, or sends it anywhere except the provider's SMTP submission and IMAP endpoints.

macOS may show a one-time Automation consent prompt when `mailcli doctor --live`, targeted fallback listing, `drafts open`, or `sync` (without `--check`) first sends an Apple Event. The calling host, such as Terminal or Codex, must be allowed to control Mail in System Settings for those operations.

The calling host needs Full Disk Access to read `~/Library/Mail`; this is the one-time permission that enables fast zero-Apple-Events reads. Automation permission is required only for `doctor --live`, targeted fallback listing when the store open fails, `drafts open`, and `sync` without `--check`. Message mutations (`mark`, `move`, `copy`, `delete`) and `sync --check` execute over IMAP and need no Automation permission; they load the IMAP credential from the Keychain, so the first keychain read may show one macOS consent prompt. Sending needs no TCC permission: `send setup` and `drafts send` use only the Keychain, and the first keychain read may show one macOS consent prompt. Accessibility and Screen Recording are never required. When permission is missing, `doctor` returns the exact System Settings remediation and does not weaken the read-only boundary.

Structured output excludes body content unless the command requests it. Diagnostics avoid subjects, bodies, headers, recipient lists, and attachment bytes unless needed to identify the failed operation. MailCLI persists only review drafts, historical send/save claims, and cross-process access/update locks under `~/Library/Application Support/MailCLI`; it persists no mail corpus or search index. SMTP credentials live only in the macOS Keychain, never in files. State directories use mode `0700` and files use mode `0600`. `drafts discard --confirm` removes only the named local draft and its claims. `drafts list` reports each draft's age in days and whether any send attempt was ever recorded. `drafts prune` defaults to a dry run that lists never-sent drafts older than 30 days (`--older-than` overrides the threshold); with `--confirm` it deletes exactly those drafts with their claims and lock files, re-verifying age and claim state under the draft lease before each deletion. Drafts with a send or save attempt are reconcilable state and are never pruned.

`doctor --diagnostics` reports only named phase durations in milliseconds. It measures store opening and platform/live probing without including message content, addresses, attachment data, source paths, or draft paths.

## Release distribution

Each release `vX.Y.Z` publishes `mailcli_X.Y.Z_darwin_arm64.tar.gz`, `SHA256SUMS`, and `SHA256SUMS.sig`. The signature covers the exact checksum-manifest bytes with Ed25519; the public key is pinned in source and the updater, while the mode-0600 private key stays outside the repository. The archive contains `bin/mailcli`, the complete `skills/mailcli` package, `install.sh`, `README.md`, and `LICENSE`. The version requested from `build-release.sh`, the CLI's embedded version, archive root, Git tag, and GitHub release title must agree. Builds disable environment-dependent Go VCS stamping, use read-only module resolution, retain `-trimpath`, and strip the linker symbol table plus DWARF debug sections with `-s -w`. Identical source and toolchain inputs therefore produce byte-identical release binaries.

The packaged installer defaults to `~/.local/bin/mailcli` and `~/.agents/skills/mailcli`; `MAILCLI_BINARY_DESTINATION` and `MAILCLI_SKILL_DESTINATION` may select other safe absolute paths. It rejects symbolic-link destinations and pre-existing backup paths. Both payloads are staged and byte-compared before replacement. Existing destinations are moved to dedicated backups, the staged payloads are moved into place, and binary plus skill are verified again. Any failure restores the original destinations; successful verification removes the backups. The installer never changes Full Disk Access, Automation consent, quarantine attributes, or global Gatekeeper settings.

`mailcli update` queries the public latest-release endpoint, compares strict `MAJOR.MINOR.PATCH` versions, requires the exact `darwin/arm64` archive plus `SHA256SUMS` and `SHA256SUMS.sig`, downloads bounded metadata and manifest bodies over HTTPS, and verifies the signature against the pinned Ed25519 public key before downloading or trusting the archive. It then verifies the named SHA-256 digest, rejects traversal, links, unexpected roots, oversized expansion, or excess entries, validates Mach-O architecture, code signature, and embedded version, and invokes the packaged rollback-safe installer for the current binary and standard companion skill. A per-user flock serializes concurrent updaters. Interactive mode renders progress; `--json` emits exactly one non-animated envelope. The installer subprocess strips shell startup and function-injection variables and owns a private process group. Cancellation sends `SIGTERM` so the installer's EXIT rollback receives a five-second grace period, then synchronously force-cleans resistant descendants and verifies group absence before returning.

The native release is linker ad-hoc signed but not Apple-notarized because the release host has no Developer ID signing identity. Browser downloads may therefore require an explicit user-approved Gatekeeper remediation after SHA-256 verification. Notarized distribution requires a future Developer ID certificate and Apple notary credentials; absence of those credentials must never be hidden by automatically clearing quarantine.

## Scope

The supported scope is every active account and every mailbox represented consistently by Mail's Envelope Index and mailbox catalog, including Inbox, Sent, Drafts, Archive, Junk, Trash, custom folders, and nested Gmail labels. `mailboxes resolve` accepts one exact `--path` segment per hierarchy level, so localized folders such as `Gesendet` and `Entwürfe` require no guessed identifier. Full and partial local message sources are reported truthfully. A targeted IMAP FETCH may hydrate one uncached body or a missing attachment without launching Mail.app; attachment hydration uses the capped full-message fallback until a part-scoped fetch is proven safe.

The implemented surface includes account and mailbox discovery, paginated listing, normalized and streamed raw reading, received-attachment inspection and saving, cross-mailbox search, local plain/Markdown/HTML draft creation, preview, editor-based update, visible new-draft handoff, reply, reply-all, forward, native-draft inspection, autonomous SMTP/IMAP draft sending with IMAP Sent mirroring, copy, move, delete, read/unread, flag/unflag, junk state, and account synchronization. Mail 16 scripted draft export remains explicitly unavailable.

Account creation, login, interactive password management beyond the `send setup` keychain credential, direct provider APIs beyond the SMTP submission, IMAP Sent mirroring, IMAP mutations, IMAP hydration, and IMAP status checks used by `drafts send`, `messages mark/move/copy/delete`, hydration, and `sync --check`, Mail-store mutation, UI-coordinate automation, Mail rules administration, and permanent trash emptying are out of scope. The private store adapter is strictly read-only and supports only the exact capability-gated profile. Permanent irreversible deletion may be added only as a separate explicitly confirmed capability.

## Search

MailCLI does not execute Mail.app `whose` searches. Those Apple Events can trigger unbounded mailbox work that is not reliably cancelled when the caller exits. Mailbox scoping reduces the amount of work but does not provide a hard execution bound, so the production bridge forbids this search form completely.

`messages filter` and `messages search` without `--query` plan candidates through one parameterized query against the existing Envelope Index. Account, mailbox, sender, recipient, subject, date, read, and flagged constraints are resolved entirely in that metadata query. The `--attachment` constraint additionally inspects each candidate's authoritative MIME source because Mail's attachment catalog can lag behind a fully downloaded message. Attachment-only queries never open a candidate whose catalog count is positive: the count proves `--attachment true` and disproves `--attachment false` without I/O, reported as catalog-proven coverage; catalog-zero candidates keep the MIME scan either way. Partial or missing sources are excluded as unknown and make coverage incomplete. Results use deterministic received-date and row-ID ordering. Query-bound cursors reject filter changes and store replacem…

`messages search --query TEXT` performs an on-demand, stateless MIME scan over only the metadata candidates in scope. Two bounded workers stream each authoritative `.emlx` source, decode text/plain or text/html, skip decoding non-text part bodies so attachment names stay searchable without paying transfer-decode, and retain no corpus after the process exits. The first scan window is sized to the requested page, doubles only after a window without a match, and never exceeds 64 candidates. Matching text is collapsed into one pre-sized buffer, repeated query terms are deduplicated, nonmatching attachment filters avoid building search text, and the next cursor reuses the matched store row instead of decoding and scanning result references. `--max-messages` defaults to 50,000 and is capped at 100,000; `--max-bytes` defaults to 4 GiB and is capped at 8 GiB. These limits bound work rather than pretending that arbitrary full-text search is instant.

Every search page includes `data.page.coverage`: backend, candidate messages, scanned messages and bytes, full sources, partial sources, missing sources, catalog-proven messages (`catalog_proven_messages`, attachment-only candidates decided by the catalog without opening the source), and `complete`. Metadata-only results are complete when the store query succeeds. Body results are complete when every candidate was scanned or catalog-proven; a source that is partial, missing, or reached a scan bound makes coverage incomplete. Pagination over body hits also reports page-level incompleteness until the final page, so an agent cannot mistake a page for an exhaustive result. No refresh command exists because MailCLI maintains no index.

## Setup and usage

Requirements are macOS on Apple silicon, Go 1.27 or newer for development, `/System/Applications/Mail.app`, `/usr/bin/osascript`, and at least one account already configured in Mail.app. Grant Full Disk Access to the calling host for reads. Grant Automation access to Mail only when live diagnostics, `drafts open`, `sync` without `--check`, or fallback listing is needed.

```bash
./scripts/tests/test.sh
./scripts/build/build.sh
./scripts/release/build-release.sh "${VERSION:?set to the release version}"
./scripts/tests/test-live-responsiveness.sh
./scripts/build/install-local.sh
command -v mailcli
mailcli update
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
./bin/mailcli drafts create --to recipient@example.com --subject Subject --body-file /absolute/path/body.md --format markdown --json
./bin/mailcli drafts inspect --ref DRAFT_REF --json
./bin/mailcli drafts preview --ref DRAFT_REF --format plain
./bin/mailcli send setup --from me@example.com
./bin/mailcli drafts send --ref DRAFT_REF --confirm --json
./bin/mailcli drafts handoff --ref DRAFT_REF
./bin/mailcli drafts open --message MAIL_DRAFT_MESSAGE_REF --json
./bin/mailcli drafts reconcile --ref RETAINED_DRAFT_REF --json
./bin/mailcli drafts prune --json
printf '%s' '{"body":"Thanks.\n"}' | ./bin/mailcli messages reply --message MSG_REF --input - --all --json
printf '%s' '{"to":[{"address":"recipient@example.com"}],"body":"For your review.\n"}' | ./bin/mailcli messages forward --message MSG_REF --input - --json
./bin/mailcli messages mark --ref MSG_REF --read true --flagged false --json
./bin/mailcli messages move --ref MSG_REF --mailbox MBX_REF --json
./bin/mailcli messages copy --ref MSG_REF --mailbox MBX_REF --json
./bin/mailcli messages delete --ref MSG_REF --confirm --json
./bin/mailcli messages delete --ref DRAFT_MSG_REF --confirm --allow-draft --json
./bin/mailcli sync --json
./bin/mailcli messages search --query "project update" --after 2026-08-01 --json
./bin/mailcli messages search --sender example.com --attachment true --json
```

The main test gate (`scripts/tests/test.sh`) checks that every repository shell script is executable and parses, then runs `gofmt`, module verification, Staticcheck, `go vet`, `golangci-lint`, `govulncheck`, race/coverage tests, forbidden-path architecture checks, and isolated release installation tests. It fails if the installed companion skill (`MAILCLI_SKILL_DESTINATION` or `~/.agents/skills/mailcli`) differs from `skills/mailcli`. It defaults to `GOMAXPROCS=4` and two concurrent Go packages; `MAILCLI_TEST_CPUS` and `MAILCLI_TEST_PACKAGES` accept positive-integer overrides. Install the three external tools with `go install honnef.co/go/tools/cmd/staticcheck@v0.8.1`, `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2`, and `go install golang.org/x/vuln/cmd/govulncheck@v1.7.0` (`go.mod` is Go 1.27.0; the gate also accepts the binaries from `$(go env GOPATH)/bin`).

Keychain tests cover the Go wrapper, SecItem status mapping, and CoreFoundation string/data helpers without touching the login keychain. Compose Handoff is tested through a native seam without AppKit. SecItem and AppKit remain live/GUI walls.

`doctor` is non-invasive and checks the read store without Apple Events. `doctor --live` verifies the exact Mail process identity and asks for the Mail version through one read-only Apple Event; it never creates, saves, or sends a message. This is deliberate because Mail 16 can retain an invisible outgoing backend even after `close saving no`. Build the binary before invoking `test-live-responsiveness.sh`; that opt-in gate requires one already-running verified Mail process and Automation permission. The responsiveness gate preserves the exact Mail PID and baseline compose-object count and rejects residual processes or repository handles. Body search is bounded per invocation with `--max-messages`, `--max-bytes`, and a 60-second command deadline. Deletion requires `--confirm`; draft mutations require `--allow-draft`. Native scripted save attempts remain blocked, `drafts send` delivers over SMTP/IMAP without Mail.app, and `drafts reconcile` remains store-only support for retained claims.

## Technical baseline

The verified development host is macOS 15.6.1 on `darwin/arm64`, Mail 16.0 build 3826.700.81, and Go 1.27.0. The supported Envelope Index profile is store version `4`, minor version `74003`, framework version `3826.700.81`, WAL journal mode, and a valid store UUID. Any profile drift fails closed before message queries.

Directory enumeration is never authoritative because Mail can retain stale or partial filesystem sources. Store rows select candidates, safe mailbox mapping resolves their sources, and every result reports whether the corresponding local content is complete. Spotlight is not a required dependency; full-text search uses the deterministic on-demand MIME scanner and never creates another persistent index.

Mailbox catalogs are parsed in-process from bounded, descriptor-opened XML property lists, memoized once per Store lifetime (repeated mailbox-identity checks cost one table scan per process), and every catalog copy serves identity matching only while message membership stays live in SQL. Only Mail's binary account-ordering preference requires one `plutil` extraction, and the store-opening context bounds that child process to 15 seconds. `version`, `update`, capabilities, help, command help, unknown commands and subcommands, local draft create/list/inspect/preview/edit/update/discard/prune, sending, credential setup, visible handoff, and unsupported native save preflight bypass Mail-store configuration, SQLite, and `plutil` initialization entirely. Reply and forward creation need the Mail store for the source header block and write only local draft files. Mailbox enumeration launches no per-account conversion processes. On the supported release host, process-inclusive `drafts list --json` peak RSS fell from 10.13-10.45 MB to 6.59-6.78 MB after the original local-command boundary was enforced. The expanded boundary reduced repeated reply validation, blocked save, and unknown-subcommand paths from about 13 ms to about 5-7 ms per process. These figures are host-specific reference measurements.

Release acceptance requires isolated process-inclusive metadata-list p95 below 50 ms and metadata-search p95 below 100 ms on the supported host profile. Direct-store list, filter, and search operations must produce no measurable Mail process CPU increase. On-demand body search is separately bounded by candidate and byte limits because its cost depends on the selected corpus and source completeness. SMTP submission keeps a 30 s budget per short command and sizes the DATA transfer budget as 30 s plus one second per MiB of payload at a conservative 1 MiB/s floor, capped at 15 min; a live-context socket timeout during DATA reports `smtp_transfer_timeout`, distinct from command timeouts.

The implementation uses `github.com/emersion/go-message` for streaming RFC 5322/MIME parsing, Goldmark for Markdown, `golang.org/x/net/html` for allowlist sanitization and safe text extraction, `golang.org/x/sync/errgroup` for the bounded two-worker body scan, `golang.org/x/sys/unix` for descriptor-relative macOS opens and terminal sizing, Unicode normalization from `golang.org/x/text`, and `github.com/mattn/go-sqlite3` for one strict read-only SQLite connection. Terminal tables use a compact in-process display-cell implementation for CJK, combining characters, emoji modifiers, ZWJ sequences, and flag pairs instead of linking another width database. The visible handoff links AppKit directly into the same binary, so it needs no helper executable, shell, UI coordinates, or Apple Event. MailCLI binds only explicit writes and targeted incomplete-content fallback to the installed Mail scripting definition. Cross-process serialization uses BSD `flock`; each potentially large bridge request is a private temporary file rather than an `ARG_MAX`-bounded process argument, and `osascript`, the update installer, and an external draft editor each run in a private process group that is synchronously reaped and checked. A process-start failure never becomes a send claim. No service, daemon, cache, watcher, child process, or goroutine survives a CLI invocation.

The release target is a native `darwin/arm64` binary packaged with the companion skill and verified installer. The stripped release executable stays below the enforced 11 MiB size budget (checked by `scripts/tests/test-release.sh`) with direct SMTP/IMAP send transport, RFC 5322 MIME composition, and macOS Keychain credentials without external runtime dependencies. The HTML sanitizer deliberately reuses `golang.org/x/net/html` instead of adding a CSS-capable sanitizer dependency, saving about 83 KiB in the final executable and reducing parser surface. More aggressive C optimization, dynamic SQLite linking, executable packing, or delegating HTTPS to external tools is deliberately excluded because a marginal size win is not worth weaker diagnostics, host-dependent behavior, slower startup, or reduced update reliability.
