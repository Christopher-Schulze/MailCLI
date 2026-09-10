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
- Pages are bounded to 25. Continue until `data.page.next_cursor` is empty. References and cursors are opaque, query/store bound, and short-lived; list or search again after copy, move, delete, or synchronization.
- Message-ID discovery is only candidate discovery. Every candidate is fetched and checked for one exact normalized header. Missing, malformed, substring, or duplicate matches fail closed with `imap_ambiguous_message_id` before hydration or mutation.
- Request `messages get --ref REF --json` for metadata, `--view plain` for normalized text, and `messages raw --ref REF` for exact RFC 5322 source. Check `content_complete`, `missing_parts`, hydration state, and source before claiming completeness. Retained partial content is evidence, not a complete message.
- Search is exhaustive only when `data.page.coverage.complete` is true. Inspect candidate-count exactness, partial/missing-source counts, and scan bounds. Body search is bounded on demand; never invent a larger page or scan limit.
- Attachment IDs are opaque MIME-part paths. Save only to an absolute new path; the command verifies size, SHA-256, ownership, and mode 0600 and never overwrites an existing destination.
- Use `mailcli batch --input - --json` only with explicit refs and unique item IDs. Results are ordered per input; there is no automatic retry. Never replay a successful or uncertain mutation.

## Drafts, compose, and mutations

Create or edit a local draft first, inspect it, and obtain explicit user authorization before sending or destructive mutation.

- `drafts save` is unsupported for new scripted Mail 16 composition and returns `compose_automation_unsupported` before Mail contact. `drafts handoff` opens a visible new compose with sanitized content and To recipients only; Mail.app must be the default `mailto` application. `confirmed_opened` means the sharing-service delegate reported success, never that the draft was saved or sent; `confirmed_failed` means the delegate reported failure. Cancellation before native dispatch returns `canceled_before_dispatch` and cleans staged evidence. Cancellation, timeout, or an unparseable native result after dispatch returns `handoff_outcome_unknown`, retains the attempt ID and attachment snapshots, suppresses late success, and blocks retry. Inspect Mail.app, then run `mailcli drafts handoff-reconcile --ref DRAFT_REF --attempt ATTEMPT_ID --outcome opened|failed --confirm --json` to resolve the retained claim and remove its snapshots. AppKit exposes no supported compose-window cancellation or close operation, so never claim that Mail closed the window. Replies and forwards remain local review drafts.
- `drafts open --message MSG_REF --json` reads a persisted draft without opening an editor. Keep edits in the local draft. Close any editor before moving or deleting a draft and pass `--allow-draft`; delete also requires `--confirm`.
- Lock-owning draft operations pin the leased directory identity for the whole lifecycle. Root replacement cannot redirect draft, claim, spool, receipt, temporary, rename, or cleanup paths; `draft_lock_unsafe` or `draft_lock_changed` is terminal for that operation.
- Mark, move, copy, and delete are direct IMAP mutations. They require current store-bound identity, fresh UIDVALIDITY, and typed `server_truth`. COPY and MOVE unknown outcomes require destination/source reconciliation before another mutation. A local-only mailbox, degraded account, ambiguous mailbox, changed UIDVALIDITY, or duplicate Message-ID stops the operation.
- `mailcli sync --check` compares local and server mailbox identities over IMAP. `sync` without `--check` asks Mail.app to synchronize and needs the live Apple Events preflight.

## Send and reconcile

Sending requires a user request that covers the action plus `--confirm`. It never uses Mail.app.

1. Configure a supported Gmail or iCloud sender with `mailcli send setup --from ALIAS [--account REF]`; the password stays in Keychain and is never displayed or logged. Unsupported providers, stale/ambiguous bindings, missing credentials, or invalid addresses stop before transport.
2. Before provider or credential resolution, composition, send-claim creation, SMTP, or Sent APPEND, `drafts send --ref REF --confirm --json` rejects a historical native `save_attempt` with `draft_save_retry_blocked`; the original draft and save claim stay unchanged for reconcile-only `drafts save` recovery or explicit discard.
3. An idle `drafts send --ref REF --confirm --json` builds RFC 5322/MIME, atomically retains the composed bytes in a private mode-0600 recovery spool with size and SHA-256 metadata, submits over SMTP with STARTTLS, and mirrors to Sent over IMAP. Transfer budgets are size-aware and capped at 15 minutes.
4. Interpret evidence exactly: `sent` means final SMTP 2yz plus exact Sent persistence; `sent_mirror_pending` means SMTP accepted but Sent persistence is incomplete; `outcome_unknown` or `smtp_submission_unknown` keeps the claim and forbids replay. Neither `submission_accepted:true` nor `sent_copy_observed:true` confirms recipient delivery.
5. If submission was accepted and recording crashed, run `drafts reconcile --ref REF --json`. It uses IMAP and the local claim, verifies exactly one Sent candidate against Message-ID, envelope, body, and complete MIME fingerprint, and never resubmits. For a known failed APPEND, it searches and verifies Sent first, then retries with the retained exact MIME spool without reading changed attachment paths. Missing or changed spool bytes block APPEND and retain the draft for explicit resolution; a consumed attempt retires the spool after its immutable receipt is durable. Duplicate, mismatched, unreadable, or absent evidence fails closed with its recovery guidance. A consumed attempt returns its immutable receipt; `drafts inspect --ref REF --json` exposes it.

## Output and errors

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
