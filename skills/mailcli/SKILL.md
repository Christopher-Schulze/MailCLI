---
name: mailcli
description: Read, search, draft, reply, forward, send, save received attachments, organize, or synchronize email through accounts already configured in macOS Mail.app. Use for local MailCLI work; never use it for unrelated prose or direct provider login.
---

# MailCLI

Use MailCLI as the only boundary to the user's configured mail accounts. Reads use Mail's existing local store; direct SMTP/IMAP operations use the Keychain credential selected by the sender binding; never ask the user to paste account passwords, app-specific passwords, OAuth tokens, or cookies. Send users to `mailcli send setup`.

Before running a downloaded release installer, follow the README bootstrap path: use an independently trusted OpenSSL 3 binary to verify the pinned Ed25519 signature over the exact `SHA256SUMS` bytes and the expected archive digest before extraction or `install.sh` execution. Never run a downloaded installer before both checks succeed.

## Start and preflight

1. Resolve one executable: `command -v mailcli`, or `./bin/mailcli` after `./scripts/build/build.sh` in a checkout. Keep that path and its identity for the workflow.
2. Use `mailcli capabilities --json` as the agent contract. Require top-level `ok:true`, `schema_version:1`, `data.capabilities.schema_version:1`, and a compatible release. Use only returned command IDs, schemas, limits, effect classes, confirmations, dependencies, and result states. Help text is for humans.
   Respect the published provider set, `raw_mime_send`, `send_transport`, `mutation_transport`, page/batch limits, and IMAP pool limits. LIST/STATUS/SEARCH/FETCH share the account pool; APPEND and mutations are exclusive per account.
3. To install from a source checkout, run `scripts/build/install-local.sh [BINARY_DESTINATION]`. It builds the current binary and invokes the shared rollback-safe installer, which stages and publishes the matching binary and `skills/mailcli` together. The binary destination defaults to `~/.local/bin/mailcli`; set `MAILCLI_SKILL_DESTINATION` to redirect the skill. After installation, start a new agent session so the skill is discovered, then use `scripts/utils/mailcli-preflight.sh capabilities` for the installed executable. The helper fingerprints the regular executable, keys a mode-0600 cache by binary SHA-256 and schema, and atomically stores only valid `ok:true` JSON. A cache hit avoids another capabilities call. `--refresh` or `invalidate` after installation, binary replacement, schema/contract change, capability failure, or an explicit diagnostic. If the helper is unavailable, run capabilities once for the current binary and retain that result for the current session; do not trust an older result.
4. Check an existing user skill only with `scripts/tests/report-skill-drift.sh`. It is read-only; pass `--repository PATH --installed PATH` for explicit inputs. `status=match` is current, while `missing`, `mismatch`, or `unstable` includes reconciliation guidance and never installs anything.
5. Run `mailcli doctor --json` on the first local-store assessment. In a checkout, `scripts/utils/mailcli-preflight.sh doctor` reuses a healthy result for at most 300 seconds, keyed to the same binary/schema identity. Refresh it after any store, permission, schema, account, or read failure. Never cache `doctor --live`: run it immediately before an Apple Events operation and require Mail.app to already be running.
6. Prefer `--json`. Check exit status, the single envelope, `ok`, and `error.guidance` before acting. The sole successful nonzero result is `sync --check --require-complete` exit 3 with `ok:true` and incomplete coverage.

All installation entrypoints share `~/Library/Application Support/MailCLI/update.lock` through recovery, verification, and cleanup. Direct installers wait at most 30 seconds; self-update uses its command deadline and passes ownership to the child. Self-update ignores `MAILCLI_INSTALL_PACKAGE_ROOT` and uses its authenticated package; source installation retains its explicit checkout override. Never unlink or steal this lock or infer abandonment from age. If identity checks refuse recovery, retain the transaction and replaced artifacts for inspection. Cancel automated installers through their whole process group.

## Choose the execution boundary

| Need | Path | Mail.app / permission |
| --- | --- | --- |
| Accounts, mailboxes, local messages, raw source, attachments | local store; bounded IMAP hydration only when content is incomplete | Full Disk Access; no Apple Events |
| `sync --check`, mark, move, copy, delete | direct IMAP | no Mail.app or Automation |
| `drafts send`, `drafts reconcile` for a direct claim, setup | direct SMTP/IMAP and Keychain | no Mail.app or Full Disk Access |
| `sync` without `--check`, `doctor --live`, read-only fallback | Mail.app Apple Events | Mail must already run |
| new visible compose | AppKit handoff | proves compose acceptance only; never sending |

Never issue raw SQLite, private-file traversal, AppleScript/JXA, UI-coordinate automation, or parallel `osascript` calls. MailCLI has no daemon, watcher, copied corpus, owned search index, or refresh command.

## Read safely

- Resolve account and mailbox references through list/resolve commands. Preserve the exact server mailbox name and path; never guess localized display names. Traverse every returned mailbox for an all-mail request.
- Inspect account `complete`, `identity_coverage`, and degraded remediation. A degraded account is not a valid send or mutation target; an ambiguous or stale binding must be fixed before transport.
- Message pages are bounded to 25. Continue until `data.page.next_cursor` is empty. References and cursors are opaque, query/store bound, and short-lived; list or search again after copy, move, delete, or synchronization.
- Message-ID discovery is only candidate discovery. Every candidate is fetched and checked for one exact normalized header. Missing, malformed, substring, or duplicate matches fail closed with `imap_ambiguous_message_id` before hydration or mutation.
- Request `messages get --ref REF --json` for metadata, `--view plain` for normalized text, and `messages raw --ref REF` for exact RFC 5322 source. Check `content_complete`, `missing_parts`, hydration state, and source before claiming completeness. Retained partial content is evidence, not a complete message.
- Search is exhaustive only when `data.page.coverage.complete` is true. Inspect candidate-count exactness, partial/missing-source counts, and scan bounds. Body search is bounded on demand; never invent a larger page or scan limit.
- Date filters use received Unix seconds: `--after` is inclusive, `--before` exclusive, and explicit epoch zero remains a real bound. Prefer RFC3339 with `Z` or an offset for a fixed instant; date-only input uses local midnight. Preserve the exact date strings across cursor pages and require after to precede before when both are set.
- Attachment IDs are opaque MIME-part paths. Save only to an absolute new path; the command verifies size, SHA-256, ownership, and mode 0600 and never overwrites an existing destination.
- Embedded RFC messages, including `message/global` and implicit `multipart/digest` entries, appear as attachments even when `name` is empty. Save the selected part to access its complete original embedded MIME bytes; nested content stays inside that object and is not included in outer body/search text. Check `content_complete` and `missing_parts` for decoding, structure or budget failures; a saved-file hash proves bytes, not message validity.
- Use `mailcli batch --input - --json` only with explicit refs and unique item IDs. Results are ordered per input; there is no automatic retry. Never replay a successful or uncertain mutation.

## Drafts, compose, and mutations

Create or edit a local draft first, inspect it, and obtain explicit user authorization before sending or destructive mutation.

- Rich drafts bound source, rendered Markdown, HTML, and plain text to 4 MiB each, parsed trees to 65,536 nodes and 512 levels, and accumulated link-label text to 16 MiB. On `invalid_argument`, simplify the named content and review its sanitized preview and loss diagnostics; never treat a rejected or canceled rendering as a saved draft.
- `drafts list --limit N [--cursor CURSOR] --json` returns reference-ordered `data.drafts`, defaults to 50 entries, and accepts 1 through 200. Follow `data.page.next_cursor` until absent; a changed draft directory returns `invalid_cursor` and requires restarting without the cursor. Inspect every `state_error`; bodies and confidential claim payloads are omitted. `data.page.revision` describes listing consistency only and cannot be used as `--expected-revision` for update or send.
- `drafts edit --json` sends both editor streams directly to stderr and reserves stdout for one envelope. Interactive editors require stdin and stderr on the same foreground controlling terminal; unsupported routing returns `editor_terminal_unavailable`. On `editor_failed`, `editor_canceled`, or edited-input validation/update failure, inspect `error.draft_editor` for the retained `candidate_path`, original revision, and available process exit/signal status. Review the current draft and candidate before an explicit update with the newly reviewed revision; never blindly replay. Human mode preserves separate stdout/stderr, and terminal ownership/settings are restored after editing.
- Draft and batch JSON accept one object up to 16 MiB. Use each exact-case schema key once, including nested recipient/item keys; duplicates, case aliases, unknown fields, and trailing documents return `invalid_input` before mutation or dispatch. Escaped spellings identify the same decoded key. Draft `attachments` contains path strings. Create, update, reply, forward, and editor output share this validation; preserve intentional empty fields and omission instead of guessing or canonicalizing rejected input.
- Review `drafts inspect --ref REF --view full --json` and retain `data.draft.revision`. Create, update, edit, inspect, and preview expose the opaque revision; metadata-only views and list summaries do not constitute content review. Pass the reviewed value as `--expected-revision REVISION` for update and send, including JSON-input updates. Never place it in the editable input document or infer it from current state. A changed account, recipient role/name/order, subject, body, threading, or attachment path/size/hash invalidates the review; operational timestamps do not.
- On `draft_revision_conflict`, inspect `error.draft_revision_conflict` for expected/current revisions and any retained editor `candidate_path`. Read the current full draft and candidate, explicitly merge when needed, then retry update with the newly reviewed revision and `--input CANDIDATE_PATH`. Never blindly reuse a revision from an error. Editor conflicts preserve both versions. Reviewed send replay must also match the claim/receipt's `draft_revision`; legacy evidence without one returns `draft_revision_unavailable` and is inspect/reconcile-only.
- `drafts save` is unsupported for new scripted Mail 16 composition and returns `compose_automation_unsupported` before Mail contact. `drafts handoff` opens a visible new compose with sanitized content and To recipients only; Mail.app must be the default `mailto` application. `confirmed_opened` means the sharing-service delegate reported success, never that the draft was saved or sent; `confirmed_failed` means the delegate reported failure. Cancellation before native dispatch returns `canceled_before_dispatch` and cleans staged evidence. Cancellation, timeout, or an unparseable native result after dispatch returns `handoff_outcome_unknown`, retains the attempt ID and attachment snapshots, suppresses late success, and blocks retry. Inspect Mail.app, then run `mailcli drafts handoff-reconcile --ref DRAFT_REF --attempt ATTEMPT_ID --outcome opened|failed --confirm --json` to resolve the retained claim and remove its snapshots. AppKit exposes no supported compose-window cancellation or close operation, so never claim that Mail closed the window. Replies and forwards remain local review drafts.
- `drafts open --message MSG_REF --json` reads a persisted draft without opening an editor. Keep edits in the local draft. Close any editor before moving or deleting a draft and pass `--allow-draft`; delete also requires `--confirm`.
- Lock-owning draft operations pin the leased directory identity for the whole lifecycle. Root replacement cannot redirect draft, claim, spool, receipt, temporary, rename, or cleanup paths; `draft_lock_unsafe` or `draft_lock_changed` is terminal for that operation.
- Mark, move, copy, and delete are direct IMAP mutations. They require current store-bound identity, fresh UIDVALIDITY, and typed `server_truth`. COPY and MOVE unknown outcomes require destination/source reconciliation before another mutation. A local-only mailbox, degraded account, ambiguous mailbox, changed UIDVALIDITY, or duplicate Message-ID stops the operation.
- MOVE fallback and delete-to-Trash prove the source Deleted flag before `source_flag` or UID EXPUNGE. On a later flag failure, `imap_move_outcome_unknown` retains partial COPY proof and actual flag state in `data.message_state` or `data.delete_result`; inspect both source and destination before another mutation. Never replay COPY or STORE blindly. Retained source flags describe the pre-expunge observation; summary booleans remain local cached values. Unsupported UID EXPUNGE always defers cleanup and never triggers plain EXPUNGE.
- For mark, require `server_truth.flags_state:"observed"` and inspect `actual_flags` with `flags_source:"STORE"|"FETCH"`; an observed empty set confirms no flags. `imap_flags_mismatch`, post-STORE `imap_message_not_found`, and `imap_flags_outcome_unknown` retain `data.message_state` with `ok:false`. Missing or unverified results label summary booleans as local cached values. Never repeat STORE to recover verification failure; inspect server state first. Batch marks preserve the same evidence and mark missing or unknown outcomes `uncertain`.
- `--junk true` sets `$Junk` and removes `$NotJunk`; false sets `$NotJunk` and removes `$Junk`. MailCLI reads flags first, prunes no-ops, checks PERMANENTFLAGS, and verifies removal before addition. Existing unprefixed `Junk`/`NotJunk` remain unrelated keywords; an observed standardized pair means neither classification. `imap_flags_unsupported` retains state and permission diagnostics; `imap_flags_partial` retains a verified phase after a later rejection. Inspect `server_truth.outcome` and `error.guidance.effect_certainty` to distinguish no write, partial change, and unknown persistence. Known partial or unsupported batch items are `failed`; unknown items are `uncertain`. None permits blind replay.
- `mailcli sync --check` compares local and server mailbox identities over IMAP. `sync` without `--check` asks Mail.app to synchronize and needs the live Apple Events preflight.

## Send and reconcile

Sending requires a user request that covers the reviewed content plus `--expected-revision REVISION --confirm`. It never uses Mail.app.

Composition encodes long subjects and display names losslessly within RFC header limits. A header-specific `invalid_argument` stops before SMTP and leaves the reviewed draft unchanged with no new send claim; correct the named value, then review the updated draft before sending.

Keep valid mailbox syntax in recipient `address` fields, including required quotes and escapes such as `"A B"@example.com`; JSON must escape the quotes. A separate `name` never changes mailbox identity or duplicate detection. Preserve quoted addresses returned by reply extraction or account discovery; never repair a rejected unquoted value by guessing a different identity. BCC names receive the same control/UTF-8 validation before SMTP.

An encoded Unicode display name with ASCII addresses does not require SMTPUTF8. Internationalized envelope addresses or raw MIME headers require SMTPUTF8 and 8BITMIME after STARTTLS. `smtp_utf8_unsupported` stops before MAIL/DATA, retains the unchanged draft, and clears the transient send claim; its guidance requires correction before retry. Never transliterate or replace an address to bypass this requirement.

1. Configure a supported Gmail or iCloud sender with `mailcli send setup --from ALIAS [--account REF]`; the password stays in Keychain and is never displayed or logged. Unsupported providers, stale/ambiguous bindings, missing credentials, or invalid addresses stop before transport.
2. Before provider or credential resolution, composition, send-claim creation, SMTP, or Sent APPEND, `drafts send --ref REF --expected-revision REVISION --confirm --json` rejects a historical native `save_attempt` with `draft_save_retry_blocked`; the original draft and save claim stay unchanged for reconcile-only `drafts save` recovery or explicit discard.
3. An idle `drafts send --ref REF --expected-revision REVISION --confirm --json` builds RFC 5322/MIME, keeps the composed spool descriptor pinned through every independent bounded SMTP/APPEND read and identity-checked cleanup, atomically retains those bytes in a private mode-0600 recovery spool with size and SHA-256 metadata, submits over SMTP with STARTTLS, and mirrors to Sent over IMAP. A replaced transient pathname cannot redirect reads or delete the replacement. Transfer budgets are size-aware and capped at 15 minutes.
4. Interpret evidence exactly: `sent` means final SMTP 2yz plus exact Sent persistence; `sent_mirror_pending` means SMTP accepted but Sent persistence is incomplete; `outcome_unknown` or `smtp_submission_unknown` keeps the claim and forbids replay. Neither `submission_accepted:true` nor `sent_copy_observed:true` confirms recipient delivery.
5. If submission was accepted and recording crashed, run `drafts reconcile --ref REF --json`. It uses IMAP and the local claim, verifies exactly one Sent candidate against Message-ID, envelope, body, and complete MIME fingerprint, and never resubmits. For a known failed APPEND, it searches and verifies Sent first, then retries with the retained exact MIME spool without reading changed attachment paths; the retention copy was streamed from the pinned composed descriptor and each replay uses an independent bounded view. Missing or changed spool bytes block APPEND and retain the draft for explicit resolution; a consumed attempt retires the spool after its immutable receipt is durable. Duplicate, mismatched, unreadable, or absent evidence fails closed with its recovery guidance. A consumed attempt returns its immutable receipt; `drafts inspect --ref REF --json` exposes it.

## Output and errors

Human `messages get` and `drafts open` details replace terminal controls with spaces; body LF/TAB and ordinary Unicode remain readable. JSON retains decoded values, body exports retain normalized content, and `messages raw`/raw exports retain exact MIME bytes. Use those explicit data paths when exact content is required.

Keep bodies, addresses, credentials, and attachment bytes out of logs and summaries unless requested. Detail commands default to metadata; use `--view plain|full`, `--fields`, or `--export /absolute/new/path` only when needed. Exports are complete, exclusive mode-0600 files with verified size and SHA-256; never accept truncation or infer missing bytes.

## Error contract

Every JSON call returns one envelope:

```json
{"schema_version":1,"ok":true,"command":"accounts.list","data":{},"error":null}
```

On failure, inspect `error.code`, `error.message`, and finite `error.guidance` fields: `phase`, `effect_certainty`, `retryability`, `replay_allowed`, and `recovery`. `safe` is the only immediate retry policy. `observe_required` or `replay_allowed:false` forbids replay until the named state is checked. `user_input_required` means correct the named input or environment; `terminal` means stop. Exit 1 is runtime/operation failure, 2 is usage failure, and 3 is only the valid incomplete sync result. Transport codes survive `fmt.Errorf` wrapping and `errors.Join`; an outcome-uncertain code wins over cleanup or rejection codes, so replay stays forbidden while `error.message` retains the joined diagnostics. If SMTP acceptance is followed by a local composed-reader close error, the acceptance evidence and cleanup diagnostic remain visible, the claim stays non-replayable, and no second SMTP submission is attempted.

For `smtp_submission_unknown`, `sent_mirror_pending`, `imap_append_outcome_unknown`, `imap_copy_outcome_unknown`, and `imap_move_outcome_unknown`, retain the operation ID and follow the emitted recovery command. A send attempt points to `drafts.reconcile --ref DRAFT_REF --json`; a retained visible-compose attempt points to `drafts handoff-reconcile --ref DRAFT_REF --attempt ATTEMPT_ID --outcome opened|failed --confirm --json`; partial hydration points to `messages get --ref MESSAGE_REF --json` or `drafts open --message MESSAGE_REF --json`. Never blindly retry.

## Progressive local references

Load the detailed contract only when the task needs it:

- [Architecture and transport](../../docs/documentation.md#architecture)
- [CLI schemas and projections](../../docs/documentation.md#cli-contract)
- [Composition and handoff](../../docs/documentation.md#composition)
- [Data model and references](../../docs/documentation.md#data-model)
- [Search coverage and pagination](../../docs/documentation.md#search)
- [Setup, bindings, and permissions](../../docs/documentation.md#setup-and-usage)
- [Development gates and release boundaries](../../docs/documentation.md#development-workflow)

The repository README gives user-facing command examples; capabilities JSON remains the executable authority.
