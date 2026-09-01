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

MailCLI is a local Go executable for the accounts already configured in macOS Mail. It never connects directly to Gmail, iCloud, IMAP, SMTP, OAuth, or account-login endpoints.

The implementation has five boundaries:

1. `cmd/mailcli` owns process startup only.
2. `internal/cli` owns command parsing, validation, output selection, exit codes, and confirmation policy.
3. `internal/mail` owns typed use cases, filters, drafts, send claims, and outcome semantics without platform I/O.
4. `internal/mailstore` owns the zero-Apple-Events read path: strict read-only access to Mail's existing Envelope Index, safe mailbox mapping, `.emlx` parsing, attachment extraction, on-demand search, reference revalidation, and store-based write observation.
5. `internal/mailapp` owns the write boundary. It invokes one embedded JavaScript for Automation bridge through `/usr/bin/osascript`, passes one typed JSON request, and decodes one typed JSON response. `internal/mailref` owns opaque store-bound references and cursors shared by the read and write adapters.

Mail.app remains the source of truth. MailCLI owns no mail index, copied corpus, refresh state, daemon, watcher, mailbox cache, or background process. The local Envelope Index is opened with SQLite `mode=ro`, a private connection cache, `query_only=1`, and WAL participation. `immutable`, `nolock`, journal-mode changes, and every SQL write are forbidden. One verified Mail-store directory descriptor anchors message, mailbox-cache, and external-attachment opens; macOS `O_NOFOLLOW_ANY` rejects a symlink in any descendant component, and later attachment hashing or copying must reopen the exact selected regular-file identity. Before any query, MailCLI verifies the store version, framework version, UUID, required tables, columns, indexes, active-account catalog, mailbox cache, and filesystem containment. An unknown profile fails with `unsupported_mail_store_schema`; MailCLI never guesses a changed private schema.

Metadata listing, filtering, search planning, message detail, raw source, attachment listing, and downloaded attachment extraction use no Apple Events on the supported store path. A targeted Apple Events fallback is allowed only for a single already-resolved message whose body or attachment bytes are not locally complete. The fallback reads full raw RFC 5322 and passes it through the same MIME parser; Mail scripting content or attachment convenience properties never prove completeness. Mailbox enumeration and cross-mailbox search never fall back to recursive Mail scripting, and Mail `whose` queries are absent.

Every Apple Events caller acquires one context-aware BSD advisory lock at `~/Library/Application Support/MailCLI/mail-access.lock`. Lock acquisition is capped at two seconds; concurrent callers return `mail_busy` before contacting Mail. The gate requires an already-running Mail process, checks its bundle identity as `com.apple.mail`, and binds the bridge to that exact PID rather than addressing Mail by application name. Before a compose or message mutation can invoke `osascript`, the gate writes and synchronizes an exact-PID recovery marker while still holding the lock; a write or sync failure prevents the Apple Event from starting. A definite bridge completion clears and synchronizes that state. An incomplete call or caller crash leaves the already-durable marker, so later live operations fail with `mail_recovery_required` until the affected Mail process has been replaced. MailCLI never starts, activates, quits, kills, or restarts Mail, even if Mail exits between the process check and the Apple Event. Each bridge invocation owns one private `osascript` process group, waits for its leader, terminates any remaining owned group members, and verifies group absence before releasing the gate. SIGINT, SIGTERM, and context cancellation write a separate private bridge marker; the script checks it before mutation and between bounded compose snapshots, requests closure of its owned unsent compose object, and verifies that the object no longer exists. A retained backend returns `compose_cleanup_failed` and leaves recovery latched. Only a bridge that exceeds the 15-second cleanup grace is force-stopped. Incomplete reads, live probes, sync triggers, and Automation denial never create false recovery state. Mutations use store-bound identity, include the locally verified RFC Message-ID when present, and report observed state rather than assuming success.

The embedded bridge creates one request-local resolution context. Enabled accounts are fetched at most once per invocation, and repeated account or mailbox references reuse the resolved objects. Message listing validates its page limit of `1..25` before accessing Mail. Direct message operations resolve one account, one mailbox path, and one message ID; Mail `whose` queries and mailbox-wide `messages()` reads are forbidden by tests. Production Mail 16 clients reject native compose, draft save, outbound attachment insertion, and send with `compose_automation_unsupported` before acquiring the Apple Events gate. The retained compose implementation is reachable only through injected test clients and remains covered by lifecycle, resource-bound, visibility, cleanup, and at-most-once regression tests. Message-mutation outcome observation happens outside Mail through exponentially backed-off store checks.

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
- Destructive operations require an explicit command and confirmation flag. Native sending is unavailable on Mail 16.
- `mailcli capabilities --json` is the authoritative discovery contract. It reports schema and release identity, every command ID, read/write class, confirmation requirement, Mail-store and Mail.app dependencies, result states, and hard limits without opening the Mail store or contacting Mail.app.
- Capability discovery, local draft create/list/inspect/update/discard, local reply/forward creation, unsupported save/send preflight, and missing or unknown command/subcommand routes bypass Mail-store configuration, SQLite, `plutil`, and Mail.app initialization.

Command surface:

| Command | Status | Purpose |
|---|---|---|
| `mailcli capabilities` | Implemented | Return the versioned machine-readable command, effect, dependency, result, and limitation contract |
| `mailcli update` | Implemented | Check GitHub and install a checksum-verified release plus companion skill with rollback |
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
| `mailcli drafts save` | Blocked on Mail 16 | Return `compose_automation_unsupported` before contacting Mail.app |
| `mailcli drafts open` | Implemented | Inspect an existing Mail.app draft headlessly |
| `mailcli drafts send` | Blocked on Mail 16 | Return `compose_automation_unsupported` before contacting Mail.app |
| `mailcli drafts discard` | Implemented | Remove only the selected local review draft after confirmation |
| `mailcli drafts reconcile` | Implemented | Recheck a retained uncertain send against Sent without sending again |
| `mailcli messages reply` | Implemented | Create a local reply or reply-all draft bound to the source message |
| `mailcli messages forward` | Implemented | Create a local forward draft bound to the source message |
| `mailcli messages mark` | Implemented | Change and read back read, flagged, or junk state |
| `mailcli messages move` | Implemented | Move and re-reference a message in a resolved mailbox |
| `mailcli messages copy` | Implemented | Copy and reference a message in a resolved mailbox |
| `mailcli messages delete` | Implemented | Invoke Mail.app's configured deletion behavior after confirmation |
| `mailcli sync` | Implemented | Ask Mail.app to check all mail or synchronize one selected account |

Agents must discover support from `mailcli capabilities --json`, never by parsing help text. The capability manifest explicitly reports that MailCLI owns no mail index or background process, can read raw MIME, cannot send caller-supplied raw MIME, has `compose_write:false`, `compose_attachment_write:false`, and `send_transport:"none"`, limits pages to 25 messages, raw fallback to 64 MiB, reviewed draft subjects to 64 KiB, bodies to 4 MiB, recipients to 200, attachments to 100, and attachment bytes to 512 MiB. `mailcli help` is a compact human command overview; focused flags remain under `mailcli <command> --help`.

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

Draft creation accepts a typed JSON document on standard input with sender identity, To, CC, BCC, subject, body, and attachment paths. The `body` key is mandatory even when its intentional value is empty. Recipient fields are arrays of `{name, address}` objects; the same normalized address cannot occur more than once across To, CC, and BCC. Hard resource limits are 64 KiB of subject text, 4 MiB of body text, 200 total recipients, 100 attachments, and 512 MiB of attachment bytes. Attachment paths must be absolute regular files. MailCLI rejects an oversized file before hashing and records accepted sizes plus SHA-256 digests in the local review draft.

Drafts are private JSON files under `~/Library/Application Support/MailCLI/drafts`, not fragile unsaved Mail compose objects. The supported flow is `drafts create` -> `drafts inspect` -> optional `drafts update` -> complete the message in Mail's UI. Replies and forwards require a store-bound source reference and create another local review draft. They do not open a compose window, export a native draft, or send mail.

Live verification against Mail 16.0 build `3826.700.81` proved that scripted compose setters can return success while body and recipient values are later missing, attachment insertion can fail with Apple Event `-10000`, automatic save can persist only a signature, and `close saving no` can leave an invisible outgoing backend. Visible compose automation also created signature-only phantom drafts. These are data-integrity failures, not cosmetic limitations.

Production `drafts save` and `drafts send` therefore return `compose_automation_unsupported` before baseline capture, claim creation, gate acquisition, or any Apple Event. This also prevents a false `compose_busy` result caused by an unreadable window property. Capabilities report `compose_write:false`, `compose_attachment_write:false`, and `send_transport:"none"`. Historical send/save claims remain readable through reconciliation code, but store observation succeeds only with exact final native headers, body, recipient roles, and attachment count; a missing body snapshot or a prefix-only match fails closed. No new Mail 16 compose operation can be created.

`drafts open --message` inspects a resolved Mail.app draft without opening a compose window; body, recipients, headers, and attachment metadata are returned through the normal typed message shape. Mail 16 exposes no reliable headless in-place editor for an already persisted draft.

Mark, move, and delete reject messages identified as drafts unless `--allow-draft` is explicit. The caller must first close any Mail editor for that draft because Mail itself can recreate a moved or deleted draft when an open editor later saves. Deletion additionally requires `--confirm`. Copy does not alter the source draft and therefore does not require the draft mutation flag.

## Data model

Account, mailbox, message, recipient, attachment, draft, and cursor are typed domain values. Message references are opaque and bind the Envelope Index UUID, row identity, message/global identifiers, account, mailbox identity, mailbox path, and subject. The store revalidates physical identity and current mailbox membership before every read or write translation. A changed store, moved message, reused row, or mismatched subject produces a typed stale-reference error instead of accessing a different message.

Mailbox paths are account-relative arrays internally and escaped display strings externally. This prevents collisions between Gmail labels, iCloud folders, and identically named nested mailboxes.

List and search cursors bind the store UUID, query or mailbox fingerprint, sort anchor, and row ID. They cannot be reused after a store replacement or with different filters. Message detail distinguishes normalized plain content, raw source, headers, attachment metadata, `content_source`, `content_complete`, and `missing_parts`. MIME parsing selects one representation from each `multipart/alternative`, preserves mixed-part order, and marks malformed recipient or decoding data incomplete. Large bodies, raw MIME, and attachment bytes are never included in list responses.

## Local security and permissions

MailCLI reuses Mail.app's configured accounts and Keychain-backed authentication. It does not request, read, store, log, or transmit account passwords, OAuth tokens, cookies, or app-specific passwords.

macOS may show a one-time Automation consent prompt when `mailcli doctor --live`, targeted fallback, message mutation, or sync first sends an Apple Event. The calling host, such as Terminal or Codex, must be allowed to control Mail in System Settings for those operations.

The calling host needs Full Disk Access to read `~/Library/Mail`; this is the one-time permission that enables fast zero-Apple-Events reads. Automation permission is required only for `doctor --live`, targeted incomplete-content fallback, message mutations, and sync. Accessibility and Screen Recording are never required. When permission is missing, `doctor` returns the exact System Settings remediation and does not weaken the read-only boundary.

Structured output excludes body content unless the command requests it. Diagnostics avoid subjects, bodies, headers, recipient lists, and attachment bytes unless needed to identify the failed operation. MailCLI persists only review drafts, historical send/save claims, and cross-process access/update locks under `~/Library/Application Support/MailCLI`; it persists no mail corpus or search index. State directories use mode `0700` and files use mode `0600`. `drafts discard --confirm` removes only the named local draft and its claims.

## Release distribution

Release `v1.0.4` publishes `mailcli_1.0.4_darwin_arm64.tar.gz` and `SHA256SUMS`. The archive contains `bin/mailcli`, the complete `skills/mailcli` package, `install.sh`, `README.md`, and `LICENSE`. The version requested from `build-release.sh`, the CLI's embedded version, archive root, Git tag, and GitHub release title must agree. Builds disable environment-dependent Go VCS stamping, use read-only module resolution, retain `-trimpath`, and strip the linker symbol table plus DWARF debug sections with `-s -w`. Identical source and toolchain inputs therefore produce the same binary independently of the current commit or dirty-worktree state. The builder rejects a non-semantic version, non-absolute output directory, existing output asset, non-ARM64 binary, invalid code signature, VCS-stamped binary, or binary-version mismatch. Release tests additionally reject a DWARF-bearing executable or a binary larger than 10 MiB.

The packaged installer defaults to `~/.local/bin/mailcli` and `~/.agents/skills/mailcli`; `MAILCLI_BINARY_DESTINATION` and `MAILCLI_SKILL_DESTINATION` may select other safe absolute paths. It rejects symbolic-link destinations and pre-existing backup paths. Both payloads are staged and byte-compared before replacement. Existing destinations are moved to dedicated backups, the staged payloads are moved into place, and binary plus skill are verified again. Any failure restores the original destinations; successful verification removes the backups. The installer never changes Full Disk Access, Automation consent, quarantine attributes, or global Gatekeeper settings.

`mailcli update` queries the public latest-release endpoint, compares strict `MAJOR.MINOR.PATCH` versions, selects the exact `darwin/arm64` archive, downloads bounded metadata, checksum, and archive bodies over HTTPS, verifies the named SHA-256 digest, rejects traversal, links, unexpected roots, oversized expansion, or excess entries, validates Mach-O architecture, code signature, and embedded version, then invokes the packaged rollback-safe installer for the current binary and standard companion skill. A per-user flock serializes concurrent updaters. Interactive mode renders progress; `--json` emits exactly one non-animated envelope. The installer subprocess strips shell startup and function-injection variables.

The native release is linker ad-hoc signed but not Apple-notarized because the release host has no Developer ID signing identity. Browser downloads may therefore require an explicit user-approved Gatekeeper remediation after SHA-256 verification. Notarized distribution requires a future Developer ID certificate and Apple notary credentials; absence of those credentials must never be hidden by automatically clearing quarantine.

## Scope

The supported scope is every active account and every mailbox represented consistently by Mail's Envelope Index and mailbox catalog, including Inbox, Sent, Drafts, Archive, Junk, Trash, custom folders, and nested Gmail labels. `mailboxes resolve` accepts one exact `--path` segment per hierarchy level, so localized folders such as `Gesendet` and `Entwürfe` require no guessed identifier. Full and partial local message sources are reported truthfully. A targeted fallback may ask Mail for one uncached body or attachment, but MailCLI never starts Mail to do so.

The implemented surface includes account and mailbox discovery, paginated listing, normalized and raw reading, received-attachment inspection and saving, cross-mailbox search, local draft creation, reply, reply-all, forward, native-draft inspection, copy, move, delete, read/unread, flag/unflag, junk state, and account synchronization. Mail 16 native compose, draft export, outbound attachment insertion, and send are explicitly unavailable.

Account creation, login, password management, direct provider APIs, Mail-store mutation, UI-coordinate automation, Mail rules administration, and permanent trash emptying are out of scope. The private store adapter is strictly read-only and supports only the exact capability-gated profile. Permanent irreversible deletion may be added only as a separate explicitly confirmed capability.

## Search

MailCLI does not execute Mail.app `whose` searches. Those Apple Events can trigger unbounded mailbox work that is not reliably cancelled when the caller exits. Mailbox scoping reduces the amount of work but does not provide a hard execution bound, so the production bridge forbids this search form completely.

`messages filter` and `messages search` without `--query` plan candidates through one parameterized query against the existing Envelope Index. Account, mailbox, sender, recipient, subject, date, read, and flagged constraints are resolved entirely in that metadata query. The `--attachment` constraint additionally inspects each candidate's authoritative MIME source because Mail's attachment catalog can lag behind a fully downloaded message. Positive catalog evidence may prove `--attachment true`; proving `--attachment false` requires a complete source. Partial or missing sources are excluded as unknown and make coverage incomplete. Results use deterministic received-date and row-ID ordering. Query-bound cursors reject filter changes and store replacement.

`messages search --query TEXT` performs an on-demand, stateless MIME scan over only the metadata candidates in scope. Two bounded workers stream each authoritative `.emlx` source, decode text/plain or text/html, discard attachment payloads without hashing them, and retain no corpus after the process exits. The first scan window is sized to the requested page, doubles only after a window without a match, and never exceeds 64 candidates. Matching text is collapsed into one pre-sized buffer, repeated query terms are deduplicated, nonmatching attachment filters avoid building search text, and the next cursor reuses the matched store row instead of decoding and scanning result references. `--max-messages` defaults to 50,000 and is capped at 100,000; `--max-bytes` defaults to 4 GiB and is capped at 8 GiB. These limits bound work rather than pretending that arbitrary full-text search is instant.

Every search page includes `data.page.coverage`: backend, candidate messages, scanned messages and bytes, full sources, partial sources, missing sources, and `complete`. Metadata-only results are complete when the store query succeeds. Body results report incomplete coverage when a source is partial, missing, or the requested scan bound was reached. Pagination over body hits also reports page-level incompleteness until the final page, so an agent cannot mistake a page for an exhaustive result. No refresh command exists because MailCLI maintains no index.

## Setup and usage

Requirements are macOS on Apple silicon, Go 1.27 or newer for development, `/System/Applications/Mail.app`, `/usr/bin/osascript`, and at least one account already configured in Mail.app. Grant Full Disk Access to the calling host for reads. Grant Automation access to Mail only when live diagnostics, targeted fallback, or writes are needed.

```bash
./scripts/tests/test.sh
./scripts/build/build.sh
./scripts/release/build-release.sh 1.0.5
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
./bin/mailcli drafts inspect --ref DRAFT_REF --json
./bin/mailcli drafts open --message MAIL_DRAFT_MESSAGE_REF --json
./bin/mailcli drafts reconcile --ref RETAINED_DRAFT_REF --json
printf '%s' '{"body":"Thanks.\n"}' | ./bin/mailcli messages reply --message MSG_REF --input - --all --json
printf '%s' '{"to":[{"address":"recipient@example.com"}],"body":"For your review.\n"}' | ./bin/mailcli messages forward --message MSG_REF --input - --json
./bin/mailcli messages mark --ref MSG_REF --read true --flagged false --json
./bin/mailcli messages move --ref MSG_REF --mailbox MBX_REF --json
./bin/mailcli messages delete --ref MSG_REF --confirm --json
./bin/mailcli messages delete --ref DRAFT_MSG_REF --confirm --allow-draft --json
./bin/mailcli sync --json
./bin/mailcli messages search --query "project update" --after 2026-08-01 --json
./bin/mailcli messages search --sender example.com --attachment true --json
```

The main test gate validates the syntax and executable bit of every repository shell script before running `gofmt`, module verification, Staticcheck, `go vet`, `golangci-lint`, `govulncheck`, race/coverage tests, and isolated release installation gates. Build the binary before invoking `test-live-responsiveness.sh`; that opt-in gate requires one already-running verified Mail process and Automation permission. `doctor` is non-invasive and checks the read store without Apple Events. `doctor --live` verifies the exact Mail process identity and asks for the Mail version through one read-only Apple Event; it never creates, saves, or sends a message. This is deliberate because Mail 16 can retain an invisible outgoing backend even after `close saving no`. The responsiveness gate preserves the exact Mail PID and baseline compose-object count and rejects residual processes or repository handles. Body search is bounded per invocation with `--max-messages`, `--max-bytes`, and a 60-second command deadline. Deletion requires `--confirm`; draft mutations require `--allow-draft`. New native send/save attempts are blocked, while `drafts reconcile` remains store-only support for historical retained claims.

## Technical baseline

The verified development host is macOS 15.6.1 on `darwin/arm64`, Mail 16.0 build 3826.700.81, and Go 1.27.0. The supported Envelope Index profile is store version `4`, minor version `74003`, framework version `3826.700.81`, WAL journal mode, and a valid store UUID. Any profile drift fails closed before message queries.

Directory enumeration is never authoritative because Mail can retain stale or partial filesystem sources. Store rows select candidates, safe mailbox mapping resolves their sources, and every result reports whether the corresponding local content is complete. Spotlight is not a required dependency; full-text search uses the deterministic on-demand MIME scanner and never creates another persistent index.

Mailbox catalogs are parsed in-process from bounded, descriptor-opened XML property lists. Only Mail's binary account-ordering preference requires one `plutil` extraction, and the store-opening context bounds that child process to 15 seconds. `version`, `update`, capabilities, help, command help, unknown commands and subcommands, local draft create/list/inspect/update/discard, local reply/forward creation, and unsupported native save/send preflight bypass Mail-store configuration, SQLite, and `plutil` initialization entirely. Mailbox enumeration launches no per-account conversion processes. On the supported release host, process-inclusive `drafts list --json` peak RSS fell from 10.13-10.45 MB to 6.59-6.78 MB after the original local-command boundary was enforced. The expanded boundary reduced repeated reply validation, blocked save, and unknown-subcommand paths from about 13 ms to about 5-7 ms per process. These figures are host-specific reference measurements.

Release acceptance requires isolated process-inclusive metadata-list p95 below 50 ms and metadata-search p95 below 100 ms on the supported host profile. Direct-store list, filter, and search operations must produce no measurable Mail process CPU increase. On-demand body search is separately bounded by candidate and byte limits because its cost depends on the selected corpus and source completeness.

The implementation uses `github.com/emersion/go-message` for streaming RFC 5322/MIME parsing, `golang.org/x/net/html` for safe text extraction, `golang.org/x/sync/errgroup` for the bounded two-worker body scan, `golang.org/x/sys/unix` for descriptor-relative macOS opens, Unicode normalization from `golang.org/x/text`, and `github.com/mattn/go-sqlite3` for one strict read-only SQLite connection. MailCLI binds only explicit writes and targeted incomplete-content fallback to the installed Mail scripting definition. Cross-process serialization uses BSD `flock`; each potentially large bridge request is a private temporary file rather than an `ARG_MAX`-bounded process argument, and `osascript` runs in an owned process group and is synchronously reaped. A process-start failure never becomes a send claim. No service, daemon, cache, watcher, child process, or goroutine survives a CLI invocation.

The release target is a native `darwin/arm64` binary packaged with the companion skill and verified installer. Standard Go linker stripping reduced the verified executable from 15,496,930 bytes to 9,431,826 bytes, a 39.14 percent reduction; warm local-command latency and process RSS remained within measurement noise. More aggressive C optimization, dynamic SQLite linking, executable packing, or delegating HTTPS to external tools is deliberately excluded because a marginal size win is not worth weaker diagnostics, host-dependent behavior, slower startup, or reduced update reliability. Cross-compilation alone is insufficient acceptance evidence because the local store profile, Full Disk Access, Apple Events permission, Mail 16 composition behavior, attachment bytes, send observation, installation rollback, skill discovery, and process non-interference must be exercised on the installed host.
