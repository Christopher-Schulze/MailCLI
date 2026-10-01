# MailCLI Documentation

This manual is the complete reference for MailCLI.
The first chapters cover installation and daily use; the later chapters specify each subsystem's contract.
`mailcli capabilities --json` remains the machine-readable authority for commands, schemas, limits and error codes.

- [Overview](#overview)
- [For agents](#for-agents)
- [Install and update](#install-and-update)
- [Setup](#setup)
- [Commands](#commands)
- [Workflows](#workflows)
- [Output contract](#output-contract)
- [Errors and recovery](#errors-and-recovery)
- [Limits](#limits)
- [Reading and search](#reading-and-search)
- [Drafts and composition](#drafts-and-composition)
- [Sending](#sending)
- [Visible handoff](#visible-handoff)
- [Mailbox mutations and batch](#mailbox-mutations-and-batch)
- [Accounts and bindings](#accounts-and-bindings)
- [Mail.app integration](#mailapp-integration)
- [Security](#security)
- [Platform and compatibility](#platform-and-compatibility)
- [Release and distribution](#release-and-distribution)
- [Architecture](#architecture)
- [Development](#development)

## Overview

MailCLI is a local Go executable for the accounts already configured in Apple Mail on macOS.
It reads Mail's local store, creates structured review drafts, and performs explicit SMTP and IMAP operations.

- Reads, searches, raw source and attachments come from Mail's Envelope Index and `.emlx` sources without Apple Events.
  Complete local reads need no Mail.app process, no network and no provider credentials.
- Missing content can require targeted IMAP hydration; local inbox coverage does not prove remote freshness.
- Mark, move, copy and delete run over IMAP and return typed server evidence.
- Reviewed drafts are sent over SMTP with a Sent copy over IMAP; an accepted submission is never repeated.
- MailCLI keeps no separate mail corpus, search index, daemon, watcher or background process, and never writes Mail's Envelope Index; its only mail-derived cache holds IMAP-fetched excerpts of at most 1,000 runes (see [Commands](#commands)).
- Every data-bearing command returns one versioned JSON envelope with typed errors and one recommended `next` action.

JSON is the default for pipes and files, human text for terminals.
Explicit `--json` or `--human` overrides `MAILCLI_OUTPUT=json|human`.
Help remains human text; machines discover the contract through `capabilities`.

## For agents

1. `mailcli capabilities --for <id> --json` returns the effects, confirmations, dependencies, result states and limits of the commands a workflow needs.
2. `mailcli capabilities --for <id> --output-schema --json` adds full parameter schemas, `schema.output` trees and their reachable definitions for parsing data. Use `--outputs` for the additional envelope/error definitions and command error catalog.
3. The error catalog `data.capabilities.error_codes` (published with `--outputs`) gives each code's meaning, emitting commands and guidance: `phase`, `effect_certainty`, `retryability`, `replay_allowed` and the recommended `next` action.
   A live envelope's `error.guidance` and `next` stay authoritative.
4. Cache the contract per `data.capabilities.contract_sha256`.
   `mailcli version --json` returns the same digest without the contract, so a cache check costs one small read; reread the contract when it differs.
   `mailcli capabilities --outputs --json` is the complete export of every command.
5. Reply matching: `mailcli messages search --after 2026-09-01 --with-threading --with-excerpt --json`, follow `data.page.next_cursor`, and match each sent Message-ID against `summary.in_reply_to[]` and `summary.references[]`.
   The sender domain is only a candidate; `threading_complete:false` means unknown.

Never invent refs, attachment IDs, cursors or reviewed revisions; obtain them from MailCLI output.
Follow `schema_ref.resolve` with the same executable for exact parameter contracts, or add `--schemas` to inline them.
The companion skill in `skills/mailcli` routes intents to commands and guides; see [Agent skill](#agent-skill).

## Install and update

Install the latest release with the HTTPS bootstrap in the repository README, or build a verified source checkout with `./scripts/build/install-local.sh`.
Both paths install the matching binary and companion skill together; the binary defaults to `~/.local/bin/mailcli` and the skill to `~/.agents/skills/mailcli`.
An agent host must discover that skill directory or a supported link to it.
`mailcli update` verifies and installs the latest signed release with rollback and refreshes the canonical skill installation.
`mailcli update --check` fetches only the release metadata and reports `update_available`; it never downloads, locks, installs or changes the contract identity.
Check an installation with `mailcli version --json` and `mailcli doctor --json`.
[Release and distribution](#release-and-distribution) specifies signing, verification, rollback and destination rules.

### Agent skill

The skill uses a compact `skills/mailcli/SKILL.md` entrypoint and seven operational guides under `skills/mailcli/references/`.
Agents read the guide for the current action and request `capabilities --for COMMAND_ID --json` for one command or `capabilities --for ID,ID,... --json` for a known multi-command workflow; family discovery is for an unknown command set.
Source and release installations copy the entire skill tree; skill discovery is host-specific, see the [agent skill installation paths](../README.md#agent-skill).
Self-update ignores skill-destination overrides and refreshes the canonical installation: a host-supported link follows that target, while a separate copy requires its original installer and destination override.
`./scripts/tests/report-skill-drift.sh --repository PATH --installed PATH` compares an installed skill read-only and reports match, missing, mismatch or unstable state with reconciliation guidance.

## Setup

Requirements are macOS on Apple silicon, `/System/Applications/Mail.app`, `/usr/bin/osascript` and at least one account already configured in Mail.app.

The installer creates missing binary and skill directories, including `~/.agents/skills`; it installs no SQL database and does not configure Mail accounts or grant permissions. On first use, the bundled skill directs the agent to its setup guide and `mailcli doctor --json`. An absent Mail directory or a readable directory without a generation returns `mail_store_not_initialized` with `next.do:ask_user`: ask the user to open Mail.app, configure the intended account and finish its initial download, then rerun doctor. Inaccessible directories, incomplete stores and unsupported generations keep their distinct failure evidence. Let the user choose among discovered accounts when their intent is ambiguous; never create Apple's store or guess account settings. Existing working installations need no setup rerun.

1. Grant Full Disk Access to the process hosting MailCLI (Terminal, an IDE or an agent host) so it can read `~/Library/Mail`.
   This one permission enables all zero-Apple-Events reads; IMAP mutations and `sync --check` also need it for local identity and comparison data.
2. Run `mailcli accounts list --json`, then `mailcli mailboxes list --account ACCOUNT_REF --json` to discover opaque refs.
3. For direct sending, IMAP mutations and hydration, run `mailcli send setup --from me@example.com` locally once.
   It stores an app-specific password in the macOS Keychain at a no-echo prompt; MailCLI never asks for passwords in chat.
   For an alias use `--account ACCOUNT_REF --from ALIAS` and, when needed, `--credential-account LOGIN`.
   Gmail and iCloud domains have built-in endpoints; other domains need an account binding with validated explicit SMTP and IMAP endpoints (see [Accounts and bindings](#accounts-and-bindings)).
   Unsupported providers fail before network access or credential storage.
4. Automation permission (control Mail in System Settings) is needed only for `doctor --live`, targeted fallback listing when the store cannot open, and `sync` without `--check`.
   macOS may show a one-time consent prompt when one of them first sends an Apple Event.
5. Visible handoff needs Mail.app as the default email application and a human check of the resulting compose window; it never sends.

Sending and direct transport-claim reconciliation need no Mail-store or Automation permission; the first Keychain read may show one macOS consent prompt.
Accessibility and Screen Recording are never required.
When a permission is missing, `doctor` returns the exact System Settings remediation.

## Commands

Examples use discovered refs and absolute paths chosen by the caller.
They illustrate syntax, not authorization to send, delete, install or change mail.
Each listed output key is under `data`; conditional evidence can also appear on a failed envelope.
All commands share the [Output contract](#output-contract); the selected capability schema lists every flag, field registry and incompatible combination.

### Discovery and maintenance

- `capabilities`: discover contracts; main flags `--for`, `--schemas`, `--output-schema`, `--outputs`, `--errors`, `--limits`.
  `--output-schema` requires `--for` and returns inline parameters, output/field variants and only reachable data definitions; it excludes `--outputs`, `--errors` and `--limits`.
  `--outputs` adds every selected command's `schema.output` tree, shared `$defs` and the error catalog.
  `--errors CODE[,CODE]` returns only the catalog entries of those codes (about 1 KB per code instead of the roughly 60 KB of `--outputs` for one command); `--for` restricts the commands inside each entry, and an unknown code, or one the selected commands cannot emit, is an `invalid_argument` error naming it.
  Example: `mailcli capabilities --for messages.get --json`.
  Output: `capabilities`.
- `version`: inspect installed identity; main flag `--json`.
  Example: `mailcli version --json`. Output: `name`, `version`, `contract_sha256`.
- `doctor`: inspect platform, Mail store, permissions and optional live Mail access; main flags `--live`, `--diagnostics`.
  Example: `mailcli doctor --json`. Output: `checks`, `timings`.
- `update`: verify a pinned Ed25519 release signature and checksum, then install binary and skill with rollback; main flags `--check` (read-only: report `update_available`, `updated` stays false), `--json`.
  Example: `mailcli update --check --json`. Output: `update_result`.
- `sync`: without `--check` ask Mail.app to synchronize; with `--check` compare local and server mailbox identities and counts over IMAP without Mail.app; main flags `--account`, `--check`, `--require-complete`.
  Example: `mailcli sync --check --account ACCOUNT_REF --json`.
  Output: `sync_check` or `sync_result`.
- `send.setup`: store or remove the per-account app-specific password in the Keychain and create or update account bindings; main flags `--from`, `--account`, `--credential-account`, `--remove`; explicit endpoints use `--smtp-host`, `--smtp-port`, `--imap-host` and `--imap-port`.
  Example: `mailcli send setup --from me@example.com`.
  Output: `send_setup`, conditional `partial_effects`.

### Account and mailbox discovery

- `accounts.list`: list enabled accounts with sender identities from a bounded Sent-history scan, `identity_coverage`, and `direct_ops_supported` with `direct_ops_reason`; main flags `--limit`, `--cursor`, `--max-bytes`.
  Example: `mailcli accounts list --json`.
  Output: `accounts`, `complete`, `identity_coverage_complete`, `page`.
- `mailboxes.list`: recursively list mailboxes with stable account-relative paths in account-reference then path order; main flags `--account`, `--limit`, `--cursor`, `--max-bytes`.
  Example: `mailcli mailboxes list --account ACCOUNT_REF --json`.
  Output: `mailboxes`, `page`.
- `mailboxes.resolve`: resolve an exact account-relative path, one `--path` per hierarchy level; main flags `--account`, `--path`.
  Example: `mailcli mailboxes resolve --account ACCOUNT_REF --path Projects --json`.
  Output: `mailbox`.

### Message reading and search

- `messages.list`: page the unified inbox or a selected mailbox without loading bodies; main flags `--account`, `--mailbox`, `--limit`, `--cursor`, `--fields`.
  Example: `mailcli messages list --limit 20 --json`. Output: `page`.
- `messages.filter`: apply typed metadata filters and bounded attachment scans through the local store; main flags `--mailbox`, `--sender`, `--subject`, `--read`, `--attachment`, `--max-messages`, `--max-scan-bytes`, `--exact-count`, `--cursor`.
  Example: `mailcli messages filter --subject invoice --json`. Output: `page`.
- `messages.search`: search metadata and bounded local bodies across the selected scope; main flags `--query`, `--max-messages`, `--max-scan-bytes`, `--exact-count`, `--cursor`.
  Example: `mailcli messages search --query invoice --json`. Output: `page`.
- Reply metadata on list, filter and search pages is opt-in and read-only.
  `--with-threading` fills `summary.in_reply_to[]`, `summary.references[]` (bracketed msg-ids in header order), `summary.from{name,address}` and `summary.threading_complete` from the bounded header block (local or IMAP `BODY.PEEK[HEADER]`).
  `--with-excerpt` fills `summary.excerpt`, `summary.excerpt_complete` and `summary.excerpt_source` (`local`, `imap-partial`, `unavailable`); `--excerpt-length` defaults to 240 runes (1 to 1000).
  The reply-metadata and excerpt keys appear only when their flag (or `--fields`) asked for them; without the flag they are absent, not empty. `message_id` is omitted while unknown.
- Excerpts prefer text/plain over HTML text, drop `>` quote lines and a signature after an exact `-- ` line, collapse whitespace and cut at a rune boundary.
  Each reads 256 KiB of local RFC source or less; only when the local source is partial or missing does it fetch over IMAP the `MIME-Version`, `Content-Type` and `Content-Transfer-Encoding` header fields plus a 64 KiB `BODY.PEEK[TEXT]` prefix, never the full body. Larger sources report `excerpt_complete:false`.
  A page selects each account mailbox once and fetches all of its IMAP excerpts with one `UID FETCH`; a message the server does not return gets `enrichment_error:"imap_message_not_found"`.
  Excerpts use the server UID from the local Mail store without a server search; when the local mailbox has no UIDVALIDITY, the fetched `Message-ID` must equal the local one, otherwise the row keeps its local result with `enrichment_error:"imap_message_uid_mismatch"`. The account's mailbox list is loaded once per page.
  A successful IMAP excerpt is cached for 30 days under `~/Library/Caches/MailCLI/excerpts` (owner-only versioned files, keyed by local store identity, server UID and UIDVALIDITY when known, at most 1,000 runes each; entries without the current version and typed excerpt/complete fields, or exceeding the rune bound, are ignored and overwritten); later pages serve it with `excerpt_source:"imap-partial"` without contacting the server; the cache is asked as soon as the local source is known to be partial and before any of its bytes are read, and a complete local source never uses it.
  Cache reads accept only a bounded regular file containing one complete JSON document with the current version; trailing whitespace is allowed. Symlinks, FIFOs, expired or corrupt entries are cache misses, and FIFO opens never wait for a writer.
  Unrequested keys are empty; empty or false means unknown, not absence.
- A malformed `In-Reply-To` or `References` value keeps every valid msg-id in source order but sets `threading_complete:false`; discarded text before, between or after IDs, malformed brackets/comments and invalid tokens cannot establish completeness. Whitespace, comma separators and balanced escaped/nested comments remain supported. A malformed present From, multiple senders or incompatible duplicate From fields also set false while retaining available valid sender evidence; absent optional fields do not imply malformed input.
  Present empty or comment-only threading fields are malformed and cannot establish completeness; absent optional fields remain valid.
  When a requested read or IMAP fetch fails, the row names the first failure code in `summary.enrichment_error` (for example `imap_timeout` or `raw_source_partial`) and the page still succeeds.
  When the base list/filter/search page succeeded but optional enrichment reaches its operation deadline while the caller remains live, the page keeps its refs, order, coverage and cursor. Completed metadata remains; unfinished requests carry `operation_timeout`, incomplete evidence and an unavailable excerpt source when no source exists. No later chunk, credential lookup, remote call or cache publication starts after expiry; owned in-flight work is drained. Parent cancellation/deadline, base-read failure and invalid partial result mapping still fail the operation. Deadline partials name every requested ref in order and return the canonical deadline sentinel; unrelated or combined errors are not accepted as deadline-only evidence.
  A page enriches at most four messages at a time in row order and charges its 8 MiB excerpt budget with the bytes actually read (every local source byte read, also when the row then goes to IMAP or fails, plus the 64 KiB bound of a planned IMAP prefix fetch; a cache hit that skipped the local read costs nothing), so a page reads up to 8 MiB plus one chunk; once the budget is used up, later rows keep their reply metadata and get `enrichment_error:"enrichment_page_budget_exhausted"` for the excerpt and can be fetched on a smaller page.
- `messages.get`: read projected metadata, recipients, body and attachment metadata; main flags `--ref`, `--view`, `--fields`, `--export`, `--max-bytes`, `--excerpt-length`.
  Its summary carries the threading fields (`get` always reads the header block); `--fields excerpt` and `--fields header_fields` (ordered, unfolded `[{name, value}]`) are opt-in, and `--view full` returns `headers` but not `header_fields`.
  Example: `mailcli messages get --ref MESSAGE_REF --view plain --json`.
  Output: `message`, conditional `content_export`.
- `messages.raw`: return or exclusively export the exact RFC 5322 source stored by Mail.app; main flags `--ref`, `--export`, `--max-bytes`.
  Example: `mailcli messages raw --ref MESSAGE_REF --json`.
  Output: `raw_source`, conditional `content_export`.
  In human mode a complete local `.emlx` source streams directly to stdout without a second in-memory copy.
- `messages.state`: read server flags over IMAP `UID FETCH` and compare them with the local index; main flag `--ref`.
  Example: `mailcli messages state --ref MESSAGE_REF --json`. Output: `state`.
- `messages.thread`: list a message's conversation members chronologically from the local grouping; main flags `--ref`, `--limit`, `--cursor`, `--fields`, `--max-bytes`.
  Example: `mailcli messages thread --ref MESSAGE_REF --json`. Output: `thread`.
- `messages.new`: compare the newest server messages of a mailbox with the local store over IMAP and list the ones the store lacks; main flags `--account`, `--mailbox` (default `inbox`), `--limit` (1 to 50, default 20), `--require-complete` (exit 3 for incomplete comparison coverage).
  Example: `mailcli messages new --account ACCOUNT_REF --json`. Output: `new_messages`.
- `attachments.list`: inspect received attachment metadata; main flags `--ref`, `--limit`, `--cursor`, `--fields`, `--max-bytes`.
  Example: `mailcli attachments list --ref MESSAGE_REF --json`.
  Output: `attachments`, `content_complete`, `content_source`, `missing_parts`, `page`.
- `attachments.save`: export one decoded received attachment to a new absolute path; main flags `--ref`, `--attachment`, `--output`.
  Example: `mailcli attachments save --ref MESSAGE_REF --attachment ATTACHMENT_ID --output /absolute/new/file.pdf --json`.
  Output: `saved_attachment` (`attachment_id`, `path`, `size`, `sha256`).
  Attachment IDs are deterministic MIME-part paths, also during targeted hydration; saved files hold the decoded part bytes, and `attachments.list` reports media types.

### Local drafts and review

- `drafts.create`: create a local plain, Markdown or safe-HTML review draft; main flags `--input`, `--from`, `--to`, `--body-file`, `--format`, `--attach`.
  Example: `mailcli drafts create --to recipient@example.com --body-file /absolute/message.txt --json`.
  Output: `draft`.
- `drafts.list`: page local draft summaries; main flags `--limit`, `--cursor`, `--fields`, `--max-bytes`.
  Example: `mailcli drafts list --json`. Output: `drafts`, `page`.
- `drafts.inspect`: read a local draft; main flags `--ref`, `--view`, `--fields`, `--export`, `--max-bytes`.
  Example: `mailcli drafts inspect --ref DRAFT_REF --view full --json`.
  Output: `draft`, conditional `content_export`.
- `drafts.preview`: render the reviewed composition without fetching remote resources; main flags `--ref`, `--preview-format`, `--max-bytes`.
  Example: `mailcli drafts preview --ref DRAFT_REF --json`. Output: `draft_preview`.
- `drafts.update`: patch a reviewed local draft; main flags `--ref`, `--expected-revision`, `--input`, `--body-file`.
  Example: `mailcli drafts update --ref DRAFT_REF --expected-revision REVIEWED_REVISION --body-file /absolute/message.txt --json`.
  Output: `draft`.
- `drafts.edit`: open a local draft in a human terminal editor; main flags `--ref`, `--editor`, `--editor-arg`.
  Agents use inspect and update instead.
  Example: `mailcli drafts edit --ref DRAFT_REF`. Output: `draft` in JSON mode.
- `drafts.open`: read a store message or Mail.app draft through the same retrieval and hydration path as `messages get`, not a local `draft_*` ref; main flags `--ref`, `--view`, `--fields`, `--max-bytes`.
  Example: `mailcli drafts open --ref MESSAGE_REF --view plain --json`.
  Output: `message`.
- `drafts.adopt`: copy a Mail.app store draft into a new local review draft; main flags `--ref`, `--view`, `--fields`, `--max-bytes`.
  Example: `mailcli drafts adopt --ref MESSAGE_REF --json`. Output: `draft`.
- `drafts.discard`: remove only the selected local draft and its claims after confirmation; main flags `--ref`, `--confirm`.
  Example: `mailcli drafts discard --ref DRAFT_REF --confirm --json`.
  Output: no command-specific data field.
- `drafts.prune`: list stale never-sent drafts, expired send receipts and orphaned claim, spool and snapshot artifacts, then optionally delete them; main flags `--older-than`, `--confirm`.
  Example: `mailcli drafts prune --older-than 30 --json`. Output: `prune`.

### Delivery and visible handoff

- `drafts.send`: submit the exact reviewed revision over SMTP and mirror it to Sent over IMAP; main flags `--ref`, `--expected-revision`, `--confirm`.
  Example: `mailcli drafts send --ref DRAFT_REF --expected-revision REVIEWED_REVISION --confirm --json`.
  Output: `send_receipt`, `send_result`.
- `drafts.reconcile`: recover retained transport claims over IMAP, recheck legacy send claims against Sent, or observe a historical native save claim, never sending again; main flag `--ref`.
  Example: `mailcli drafts reconcile --ref DRAFT_REF --json`.
  Output: `send_receipt`, `send_result`, or `saved_draft`.
- `drafts.handoff`: open a visible new-message compose window without sending; main flag `--ref`.
  Example: `mailcli drafts handoff --ref DRAFT_REF --json`. Output: `draft_handoff`.
- `drafts.handoff-reconcile`: record the human-observed outcome of a retained handoff attempt; main flags `--ref`, `--attempt`, `--outcome`, `--confirm`.
  Example: `mailcli drafts handoff-reconcile --ref DRAFT_REF --attempt ATTEMPT_ID --outcome opened --confirm --json`.
  Output: `handoff_reconcile`.

### Reply, organization and batching

- `messages.reply`: create a local reply or reply-all draft bound to the source message; main flags `--ref`, `--input`, `--all`, `--body-file`.
  Example: `mailcli messages reply --ref MESSAGE_REF --body-file /absolute/reply.txt --json`.
  Output: `draft`.
- `messages.forward`: create a local forward draft bound to the source message; main flags `--ref`, `--input`, `--to`, `--body-file`.
  Example: `mailcli messages forward --ref MESSAGE_REF --to recipient@example.com --body-file /absolute/forward.txt --json`.
  Output: `draft`.
- `messages.mark`: change read, flagged or junk state over IMAP and read back the result; main flags `--ref`, `--read`, `--flagged`, `--junk`, `--allow-draft`.
  Example: `mailcli messages mark --ref MESSAGE_REF --read true --json`.
  Output: `message_state`.
- `messages.move`: move to a resolved destination over IMAP; main flags `--ref`, `--mailbox`, `--allow-draft`.
  Example: `mailcli messages move --ref MESSAGE_REF --mailbox MAILBOX_REF --json`.
  Output: `message_state`.
- `messages.copy`: copy to a resolved destination over IMAP; main flags `--ref`, `--mailbox`.
  Example: `mailcli messages copy --ref MESSAGE_REF --mailbox MAILBOX_REF --json`.
  Output: `message_state`.
- `messages.delete`: move to Trash over IMAP after authorization; main flags `--ref`, `--confirm`, `--allow-draft`.
  Example: `mailcli messages delete --ref MESSAGE_REF --confirm --json`.
  Output: `delete_result`.
- `batch`: run an explicit bounded read, attachment-save, mark, move, copy or delete request with ordered item outcomes; main flags `--input`, `--concurrency`, `--max-bytes`, `--confirm`.
  Example: `mailcli batch --input /absolute/request.json --json`.
  Output: `batch_result`. Delete batches also require `--confirm`.

## Workflows

Discover refs before every workflow.
Evaluate only the selected command's conditional dependencies, keep page scope unchanged, and inspect completeness and recovery evidence before assuming success.

1. Triage: list inbox metadata, follow `data.page.next_cursor`, then get `--view plain` for selected refs.
   Do not load every body to select a message; `messages filter --read false` without `--mailbox` lists unread mail across the local store.
2. Conversation: thread around a discovered seed, follow either thread cursor, then read only members needed for the answer.
   The initial page contains the seed.
3. Attachment: list attachments, select a returned ID, save to a new absolute path, and retain byte and hash proof.
   Incomplete discovery is not complete content.
4. Reply: create a reply draft, inspect the full result and its revision, apply authorized updates, then send that reviewed revision only after approval.
5. Send: set up credentials locally, create and fully review a draft, retain `data.draft.revision`, then send with `--expected-revision` and `--confirm`.
   Reconcile uncertain outcomes instead of submitting again.
6. Replies: search `--after DATE --with-threading --with-excerpt`, follow `data.page.next_cursor`, and match each stored sent Message-ID against both `summary.in_reply_to[]` and `summary.references[]`.
   Treat the domain of `summary.from.address` only as a candidate and `threading_complete:false` as unknown.
   All steps are read-only.

The packaged skill's guides provide the same workflows for agent hosts.

## Output contract

### Envelope and exit codes

Every data-bearing command returns one envelope:

```json
{
  "schema_version": 1,
  "ok": true,
  "command": "accounts.list",
  "data": {"accounts": [], "complete": true, "identity_coverage_complete": true},
  "error": null
}
```

`data` is one flat field union; each field belongs to one command, and the emitted key order is stable.
Times are RFC 3339 with an offset; sizes are bytes.
Envelope Index summaries keep received and sent date presence independently: non-NULL Unix zero is `1970-01-01T00:00:00Z`, negative and positive timestamps render in UTC, and SQL NULL remains an empty string in list, filter, search, detail and observation results.

| Exit | Meaning |
| --- | --- |
| `0` | Success. |
| `1` | Runtime, configuration, Mail.app or operation failure; also missing `--confirm`, failures whose recovery asks for correction, and partial or uncertain effects. |
| `2` | Invalid invocation or caller input rejected during validation, including malformed references and cursors. |
| `3` | An otherwise successful `sync --check --require-complete` or `messages new --require-complete` whose comparison `complete` is false; its JSON keeps `ok:true`, `error:null` and normal result evidence. |

A partial batch returns `ok:false`, `error.code:"batch_partial"` and exit `1` while preserving every item's evidence.

### Output mode and arguments

Output mode is resolved before initialization: explicit global `--json` or `--human` overrides `MAILCLI_OUTPUT=json|human`; otherwise pipes and files receive JSON and terminals receive human text.
Global flags work before or after command words and options; option values and operands after `--` remain data.
Boolean `=false` selects the opposite mode, repeated matching modes are accepted, and conflicting modes or a nonempty invalid environment value return `invalid_argument` with exit `2` (conflicts in a JSON envelope, other mode errors in the selected mode).
Help (`help`, `-h`, `--help`) stays human text with aligned long options, semantic placeholders and readable defaults.

Each single-reference command accepts canonical `--ref REF` or one positional `REF` before or after options.
Repeated `--ref` flags and a positional ref must be identical; missing, empty, conflicting or excess references return `invalid_argument` before dispatch, and `--` ends option parsing.
Message and local-draft references keep their distinct semantics; `messages.search` keeps its separate query operand.
Standard input accepts structured JSON payloads for long bodies and recipient lists, avoiding shell quoting problems.
Draft JSON, draft body files and batch input honor caller cancellation before dispatch. Standard input waits without an added input timeout; cancellation is checked while polling readiness, without closing stdin, changing its descriptor flags or leaving a background reader. Explicit input paths must resolve to regular files, including regular-file symlinks; use `-` for piped streams. Local reads check cancellation between bounded chunks.
Draft and batch JSON require one object of at most 16 MiB with unique, exact-case schema keys.
Duplicate keys (also escaped duplicates, even with identical values), case aliases, unknown fields, invalid value shapes and trailing documents return `invalid_input` (exit `2`) before draft mutation or batch dispatch; diagnostics name the field or container and byte location without echoing values.
The same boundary validates create, update, reply, forward and editor output; omission and intentional empty fields keep their distinct meaning, and draft attachments are path strings, not objects.

In JSON mode a missing subcommand for `accounts`, `mailboxes`, `messages`, `attachments`, `drafts` or `send` returns `invalid_argument`, and an unknown subcommand returns `unknown_command`.
Both use exit `2`, keep usage text off stderr and include `error.valid_subcommands` in stable command order; unknown subcommands name the family in the envelope `command` and add `error.data` with `requested` and the same `choices`.

Human output is concise; bodies and raw MIME appear only when requested.
`messages get` and `drafts open` replace terminal controls (C0/C1, DEL, ESC, BEL, CR) and Unicode line and paragraph separators with spaces in every human detail field and diagnostic; body text keeps LF and TAB.
This presentation policy never modifies stored mail, JSON values, normalized exports or exact raw output.

### Projections and output budget

`messages get` defaults to the `metadata` view, which returns `summary` (with `attachment_count`), `to`, `cc`, `bcc` and `reply_to` from the header block only.
Attachments and content state (`attachments`, `content_source`, `content_complete`, `missing_parts`, `hydration`) come with `--view plain`, `--view full` or `--fields`.
`--links full|host|none` (batch read items and `defaults` accept `links` too) reduces the URLs in the returned `content`: `host` replaces every HTTP or HTTPS URL of 40 or more characters by `<host>`, `none` removes all HTTP and HTTPS URLs (the scheme in any letter case; `mailto:`, `ftp:` and bare `www.` addresses stay) together with the ` (url)` suffix of a link label, and both collapse runs of blank lines; the default `full` and every `--export` keep the complete body.
`drafts inspect` defaults to its `metadata` view, which keeps identity and operation-state evidence while omitting bodies.
`drafts create`, `drafts update`, `drafts edit`, `messages reply` and `messages forward` default to the canonical plain draft body; `--view plain` omits draft source and HTML variants, and `--view full` includes every stored representation.
`attachments list` defaults to attachment metadata; `messages raw` has only its `full` view.
Raw JSON strings require valid UTF-8 and round-trip the exact RFC source bytes, including literal U+FFFD. Invalid encoding returns `raw_source_invalid_utf8` without repaired source data; use `messages raw --export /absolute/new/path --json` or stream without `--json` for arbitrary bytes. Local and server refs follow the same rule.
`--fields` selects exact JSON field names; `all` selects the complete target registry and stands alone.
Combining `--fields` with `--view`, an unknown field, or a view unsupported by the target fails with `invalid_argument` before retrieval or mutation.
Hydration recovery for `messages get` and `drafts open` retains the validated invocation arguments, including projection, link, excerpt, budget and export options; recovery guidance never executes the retry itself.
Seven target-specific field registries drive validation, embedded schemas and `data.capabilities.limits.output_projection`, including `draft_list_fields`, `list_page_fields` and `search_page_fields`; batch read input uses the message registry.

`messages.list`, `messages.thread`, `messages.filter` and `messages.search` accept `--fields` for `sender`, `subject`, `date_received`, `date_sent`, `message_id`, `read`, `flagged`, `junk`, `deleted`, `flags_state`, `size` and `attachment_count`; filter and search also accept `snippet`, and pages can select `conversation_id`, `server_truth`, `staleness_note`, `mailbox_ref`, `account` (list and thread only) and the reply-metadata and excerpt keys.
Every projected message keeps `ref` and the named fields (`mailbox_ref` and `account` only when named; reply metadata and excerpts also when `--with-threading` or `--with-excerpt` was given). A nonempty `enrichment_error` remains an evidence peer in narrowed pages. A nonempty `flags_state` accompanies selected `read`, `flagged` or `deleted` as their evidence peer; local-only summaries omit it. Filter and search keep the complete coverage object, and `next_cursor` appears only when continuation is available.
Thread projection selects fields within `data.thread.messages` while retaining the seed `ref`, `conversation_id`, `truncated` and both available cursors. For example, `messages thread --ref MSG_REF --fields ref,subject --json` returns compact members; omitting `--fields` or selecting `all` keeps complete summaries. Thread field selection adds no header reads, excerpts or RFC threading enrichment.
Draft JSON lists (`content_diagnostics`, recipients, attachments) and `missing_parts` are `[]` when empty, never `null`.
Draft-list `--fields` accepts combinations of `age_days`, `created_at` and `updated_at`; the fixed core stays in every healthy summary, `state_error` appears only for corrupt summaries, and capabilities list `draft_list_core_fields` and `draft_list_optional_fields`.

For `messages get --json` and `drafts open --json`, an explicit `--fields` request with only `summary` and header fields reads Envelope Index values plus only the bounded RFC header block; a missing local source with IMAP configured fetches only `BODY.PEEK[HEADER]`.
The default `metadata` view reads only the header block, one bounded `UID FETCH BODY.PEEK[HEADER]` for a message that is not downloaded; `--fields attachments`, `content_source`, `content_complete`, `missing_parts` or `hydration` use the MIME metadata parser without retaining body text or HTML; `content` keeps the full body path.
Human draft opening keeps the full read, and every draft-open projection continues to reject server references before remote reads.

`--max-bytes` defaults to 1 MiB and accepts values through the 64 MiB maximum published in capability JSON.
MailCLI measures the complete encoded envelope before writing it; an oversized response returns exit `1` with `error.code:"output_too_large"` and never truncates content.
The same budget applies to list pages, `drafts list`, `drafts preview` and to `batch --json` as a whole envelope.
Every `output_too_large` envelope carries `data.required_bytes`, `data.limit_bytes` and `data.measured`: `exact` is the complete requested envelope length including its newline, while early batch-read overflow reports `lower_bound`.
An oversized list page returns byte evidence and correction guidance without rows or a continuation cursor; an oversized draft list offers a page at half the requested limit that keeps the cursor, fields and any explicit byte budget (at limit 1 no smaller page is offered); an oversized preview offers `drafts inspect --ref REF --view full --export /absolute/new/path --json`.
JSON output uses one encoder with HTML escaping disabled; JSON control characters stay escaped.
The budget applies to JSON only, not human summaries, and is not a whole-process memory limit.

`messages get`, `messages raw` and `drafts inspect` accept `--export /absolute/new/path`.
The exporter creates a mode-0600 file exclusively, writes the complete normalized body or raw RFC 5322 bytes, verifies path identity, size and SHA-256, and reports only `data.content_export` metadata.
Publication syncs the file and pinned containing directory; copying and digest verification observe caller cancellation between IO steps. A completed export keeps its metadata and `effect_certainty:complete`, `observe_required`, `replay_allowed:false` when output or finalization fails. Failed cleanup that cannot prove removal reports an unknown effect and forbids replay; inspect the destination without deleting a replacement.
A relative, existing or symlinked destination or a non-directory parent fails before retrieval, incomplete normalized content fails without creating an export, and failed hydration keeps `data.message.hydration` while retaining recovered content only when it was not redirected to an export.

### Capability discovery

`mailcli capabilities --json` is the authoritative discovery contract and opens neither the Mail store nor Mail.app.
The outer envelope stays at schema version 1; the nested manifest has `data.capabilities.schema_version:2`.
It reports release identity and, per command, `audience` (`human` for `drafts.edit`, `agent` otherwise), effect class, confirmation requirement, local `store_dependency`, typed `dependencies`, result states and `limit_refs`.
Each `dependencies` entry is `{kind,target,condition}`: `kind` is `network`, `credential` or `app`; `target` is `imap`, `smtp`, `github-release`, `keychain`, `mail-app`, `editor` or `system-compose-service`; `condition` is `always`, `if-local-source-incomplete`, `if-enrichment-source-incomplete` (list, filter and search with `--with-threading` or `--with-excerpt`), `if-local-attachment-bytes-unavailable`, `if-local-store-unavailable`, `if-batch-item-requires-imap`, `if-sync-check`, `if-sync-default`, `if-doctor-live`, `if-send`, `if-smtp-accepted` or `if-transport-claim-needs-imap-reconciliation`.
An empty array means no external dependency.

Each `commands[].schema` publishes a stable ID and version (`@v1` except `batch@v2`) with flags, value types, defaults, bounds, enums, required markers, positional arguments and incompatible-input constraints; draft and batch schemas also describe their JSON input and item fields, and the batch schema publishes request-level `defaults` with `view` and `fields`.
Discovery uses one `--for` selector: an exact command ID, comma-separated IDs or a family wildcard such as `--for 'messages.*'`, expanded in canonical order; empty, unknown, repeated and overlapping entries return `invalid_argument`.
`--limits` returns the complete limit set without commands and excludes `--for`.
Scoped output keeps effects, dependencies, confirmations and result states, includes `sync_check_policy` only with `sync` and `draft_save_policy` only with `drafts.*`, and limits `limits` to the keys in the selected commands' `limit_refs`; only a single-command selection returns `data.capabilities.scope`.
Scoped discovery publishes `schema_ref.resolve` argv instead of inline parameter schemas; run it with the same binary, or request `--for IDS --schemas` to inline them (`--schemas` requires `--for`; unscoped discovery keeps all inline schemas).
`--outputs`, with or without `--for`, adds each command's `schema.output` tree with `$ref` pointers into `data.capabilities.$defs` (including `$defs.error` and `$defs.envelope`) and the error catalog.
`--for IDS --output-schema` includes the same inline parameter contracts, data output trees and projection variants with only their transitively reachable `$defs`. It omits the command error catalog and unconditional shared envelope/error roots; referenced error, finalization and partial-result evidence types remain. `--for` is required; `--outputs`, `--errors` and `--limits` are incompatible, while `--schemas` is redundant. Recursive output types remain supported, and the full-contract digest is the same as in every other view.

The error catalog `data.capabilities.error_codes` has one entry per code with `code`, `meaning`, `commands` and `guidance` groups (`commands`, `phase`, `effect_certainty`, `retryability`, `replay_allowed`, `next`); with `--for` it is restricted to the selected commands.
Catalog guidance is the runtime classification of the bare code; a live envelope's `error.guidance` and `next` stay authoritative because retained evidence can refine them.
Every capabilities view carries the same `data.capabilities.contract_sha256`: SHA-256 over MailCLI's canonical JSON of the complete contract (all commands with parameter and output schemas, shared `$defs`, error catalog and limits; version and digest cleared); `mailcli version --json` reports the same value.
Shared `$defs` keys are snake_case names of the published types, such as `message_summary`.
Agents discover support from capabilities, never by parsing help text.

### Next action and guidance

The schema-1 `next` object appears on every failure and on a successful result with an outstanding claim, incomplete content, degraded account catalog, submitted synchronization, incomplete synchronization coverage or differing synchronization counts; completed results and normal pagination omit it.
Its fields are `do`, `why` (one sentence of 120 Unicode characters or fewer; for `ask_user` it names the concrete user action), optional `command` (a published command ID), optional `args` and optional `wait_seconds`.
Commands and arguments come only from supported recovery builders; absent arguments mean the caller keeps the original invocation or inspects the evidence, never invents a command.

| Evidence, in precedence order | `next.do` |
| --- | --- |
| Cleanup failure, or complete/partial/unknown operation effects | `check_state` |
| Caller cancellation with proven no effect | `stop` |
| Terminal failure with proven no effect | `stop` |
| Environment, credentials, account configuration or permission repair with no effect | `ask_user` |
| `confirmation_required` (the action waits for the user's authorization; exit `1`) | `ask_user` |
| Observation required, missing guidance or an unclassified failure | `check_state` |
| Safe and replay allowed, with no effect | `retry` |
| Input correction required, with no effect | `fix_input` |

Transient or busy safe retries carry `wait_seconds:1`; `search_index_changed` can retry immediately.
Cancellation never produces an automatic retry, and unknown or partial writes always require observation before repair or replay.
A pending send uses the retained `drafts.reconcile` route; a native handoff first uses `drafts.inspect` because its reconciliation needs a human-observed outcome; interactive update recovery needs reviewed stdin and is never emitted as an executable command.
A batch's next action never permits replaying successful or uncertain siblings.

Failed envelopes add `error.guidance` with `phase`, `effect_certainty`, `retryability`, `replay_allowed` and `recovery`.
`retryability` is `safe`, `observe_required`, `user_input_required` or `terminal`; `observe_required` and `replay_allowed:false` forbid replay until the retained evidence is checked.
`recovery.action` is `retry`, `observe`, `reconcile`, `correct`, `inspect` or `none`; a recovery `command` and `args` appear only for a supported command schema, and `operation_id` carries durable mutation or send identity.
Detailed guidance, item outcomes, operation IDs and partial-effect evidence stay authoritative beside `next`.

### Finalization

JSON invocations finalize initialization, execution and resource teardown before writing stdout.
A configuration failure before service creation emits one `ok:false` envelope with `error.code:"initialization_failed"` (or a preserved typed initialization code) and exit `1`.
Each invocation owns its direct transport graph; the IMAP pool closes after execution and before the local store.
If teardown fails, `data.finalization` reports `state:"failed"` with `finalization_failed`: a successful command becomes `ok:false` with exit `1`, while an existing command failure stays the top-level error and operation evidence stays in `data`.
Finalization retains every original data value and projection omission; it adds cleanup evidence without rebuilding projected messages, drafts, batches or lists. Typed payload validation still runs before finalization.
Human mode writes cleanup failures to stderr.
stdout write failures stay exit `1` and never fabricate success; an invalid payload is replaced with `serialization_failed`.

## Errors and recovery

Inspect `ok`, `error.code`, `error.guidance`, retained `data` and every batch item.
Follow `next`: a completed, partial or unknown effect requires checking state and never permits blind replay; only explicitly transient no-effect failures permit unchanged replay.
Codes surface verbatim in `error.code`; follow their specific remediation rather than retrying with different flags.
Typed store and transport codes such as `mail_store_unavailable`, `unsupported_mail_store_schema`, `unsafe_message_source`, `local_only_mailbox` and `message_already_trashed` name their condition precisely.
The error catalog lists every code with its meaning and guidance; a group's `recovery` names the observation command the runtime always has for it (`REF` marks the draft reference it fills in), and `mail_access_gate_failed`, `mail_automation_unavailable`, `bridge_cleanup_failed`, `account_binding_changed` and `special_use_mailbox_unresolved` report `ask_user` because only the user can repair them.
Codes that stay `check_state` without a command are generic or evidence-only: `operation_failed`, `store_profile_unverified`, an untyped `hydration_failed` whose origin stays unknown, and `draft_busy` on commands without a draft reference.

Generic codes: `unknown_command` (unrecognized command), `invalid_argument` (wrong flag or operand, exit `2`), `invalid_input` (malformed structured input, exit `2`), `missing_required` (omitted required flag, exit `2`), `confirmation_required` (the action needs `--confirm` after explicit user authorization) and `operation_failed` (runtime failure in store, IMAP, SMTP or Mail.app, exit `1`).
`draft_operation_canceled` means the caller canceled a draft command before completion; `draft_operation_timeout` means its operation deadline expired. Both preserve their context cause, and specific transport or outcome errors keep their original classification. A timeout never claims that the caller canceled, and uncertain writes still require observation before replay.

Pre-effect failures:
identity, binding, store, credential, editor and precondition failures that stop before any external effect (for example `account_disabled`, `keychain_load_failed`, `draft_mutation_confirmation_required`, `message_already_trashed`, `account_binding_stale`) report `effect_certainty:"none"` with `correct` or `inspect` recovery for every command, writes included.
Outcome-uncertain codes keep observation-first guidance.
Transient contention and network failures before any effect (`account_binding_busy`, `prune_state_changed`, `update_busy`, `update_check_failed`, `update_download_failed`) permit an unchanged retry.
Deterministic refusals before any effect (untrusted or invalid release artifacts, unsupported platforms or capabilities, and state files that fail an integrity check) are terminal: `next.do` is `stop`.
`check_state` with no effect is reserved for cases that need evidence first, such as a revision conflict or a busy draft.

Reads:
read failures are replayable only for explicitly classified transient errors; unknown read failures use `retryability:"observe_required"` with `recovery.action:"inspect"`.
Known Mail-store, account and Mail.app permission or readiness failures set `recovery.action:"correct"` and name the action in `recovery.instruction` (Full Disk Access, account or binding correction, Automation permission, safe Mail.app recovery); no instruction ever directs deleting or replacing the MailCLI access-gate or account-binding file.
Draft-state and mailbox-cache permission causes require correcting file access with no replay; malformed state is terminal.
IMAP certificate and hostname failures keep `imap_connect_failed` but require correcting TLS configuration, never disabling verification; connection refusal, disconnect and timeout stay transient.
`imap_fetch_failed` permits replay only with a preserved network or truncated-connection cause.
`mail_busy`, `mail_automation_timeout` and `mail_process_changed` prove a read was not dispatched or was a read-only probe, so retry is safe.
Origin-ambiguous read errors such as `imap_mutation_failed`, `imap_sent_mailbox_not_found`, `mail_error`, `bridge_cleanup_failed`, `mail_automation_failed`, other doctor or probe failures and internal `invalid_request` require inspection without replay.
`mail_store_path_mismatch` and `content_export_changed` require terminal inspection of the store or export identity.
Retained submission, mutation, APPEND or partial-effect evidence always takes precedence over read classifications.

References and cursors:
`invalid_reference` means a malformed opaque ref; obtain a current ref from the matching listing and never edit tokens.
Message reads decode the ref before reporting an unavailable store, so a malformed ref returns `invalid_reference` with exit `2` and `fix_input`, not a Full Disk Access request.
`next.why` uses `Fix the input.` for `fix_input`; `error.message` retains the detailed diagnostic. Default `stop` quotes `error.message`, selected terminal codes name their outcome (for example `message_already_trashed`: nothing to do), and `check_state` with a recovery command says `Do not replay. Inspect the state with next.command and next.args.`
Unknown flags name the flag as written and list the command's valid flags; canceled and timed-out operations without a more specific code are `operation_canceled` and `operation_timeout`, and validation failures carry no `data.store_profile`.
`ambiguous_reference` means an account or mailbox path did not resolve uniquely; refresh the listing first.
`stale_cursor` means a Mail.app page boundary changed; restart that listing without the cursor.
`account_reference_version_unsupported` needs a compatible MailCLI build; `account_reference_corrupt` and mixed `account_reference_invalid` are terminal catalog-integrity failures.
`imap_message_uid_unknown` requires refreshing the local catalog or a fresh Message-ID-backed ref; `imap_ambiguous_message_id` can stop read hydration before FETCH until one verified UID and UIDVALIDITY resolve.
`raw_source_partial` means no complete raw source was available; finish the download in Mail.app or use targeted IMAP, then retry with a verified complete source.
Definite `messages.get` `not_found` requires a fresh valid ref and supplies no guessed lookup command.
`search_cursor_stale` requires a fresh search without guessed filters or cursor repair; `search_index_changed` is safe to retry; neither retains the original filters for a complete command.
Never treat a missing page or cursor as a successful empty result.

Output size:
`output_too_large` can follow `messages.get`, `messages.raw`, `attachments.list`, `drafts.open`, `drafts.inspect`, a projected draft response or `batch`.
Narrow fields, export complete content, or raise the budget as the guidance says.
When create, update, edit, reply or forward succeeded but the JSON response is too large, the failure reports `effect_certainty:"complete"`, `replay_allowed:false` and `recovery.action:"inspect"` with the saved ref and revision; run the supplied `drafts.inspect` instead of repeating the mutation.
Read-only output limits and rejected writes never gain completion merely from a known ref, and export is suggested only where supported.

Drafts:
a completed draft mutation with a known ref emits `drafts.inspect --ref REF --view full --json`, and `draft_revision_conflict` from `drafts.update`, `drafts.send` or `drafts.reconcile` uses that full view when the ref is retained.
State-only draft recovery uses the default metadata view with `--ref REF --json`.
`draft_busy` proves this invocation made no change and forbids replay; with a retained ref it pairs `retryability:observe_required` with `recovery.action:inspect` and a runnable `drafts.inspect --ref REF --json` that reads without taking the competing lease, otherwise it keeps `recovery.action:observe` without a command.
Revision conflicts require reviewing and merging the competing versions, never copying a revision from an error.

Accounts and Mail.app:
`account_binding_stale` requires inspecting `accounts.list --json` and correcting the binding to an enabled account through `send setup` before retry.
`mail_recovery_required` from Mail.app-gated operations such as default `sync` requires the user to quit and reopen Mail.app; MailCLI has no restart command.

Delivery:
after SMTP acceptance use `drafts reconcile` and never resend to repair Sent.
An unknown handoff requires inspecting Mail.app and explicitly reconciling its attempt.
MailCLI cannot guarantee recipient delivery or close an external window.
[Sending](#sending), [Mailbox mutations and batch](#mailbox-mutations-and-batch) and [Visible handoff](#visible-handoff) specify their phase-aware codes.

## Limits

`mailcli capabilities --limits --json` returns the current machine contract.

| Area | Bound |
| --- | --- |
| Pages | List commands default to 20 items and accept `--limit` values from 1 through 200; JSON responses default to a 1 MiB envelope, `--max-bytes` up to 64 MiB. |
| Search and filter | `--max-messages` defaults to 50,000 candidates, `--max-scan-bytes` to 4 GiB; see [Search](#search) for caps. |
| Drafts | 64 KiB subject, 4 MiB reviewed body, 200 recipients, 100 attachments, 512 MiB attachment bytes. |
| Structured input | Draft and batch JSON one object of 16 MiB or less; batch 100 items. |
| Raw source | 64 MiB per message, local or hydrated. |
| Recovery spool | 1 GiB per accepted message. |
| Sender identity scan | 2,000 Sent messages by default, 10,000 at most. |

The capability manifest limits list pages to 200 items, raw fallback to 64 MiB, reviewed draft subjects to 64 KiB, bodies to 4 MiB, recipients to 200, attachments to 100, and attachment bytes to 512 MiB.
Wire, discovery, state, transfer and cleanup bounds are specified in their chapters; timeouts are collected in [Timeouts and budgets](#timeouts-and-budgets).

## Reading and search

### Store access

Mail's local SQLite Envelope Index is the source of truth for reads; MailCLI owns no mail index, copied corpus, refresh state or process-global mailbox cache.
Before SQLite is opened, numeric Mail generation directories are inventoried.
A sole supported `V10` generation is selected; beside a newer generation, `V10` is accepted only when `PersistenceInfo.plist` identifies it as active.
Newer or ambiguous layouts fail with `unsupported_mail_store_schema` or `ambiguous_mail_store_generation`; for the latter, inspect `PersistenceInfo.plist` and pin an exact validated `Config.MailStorePath` only when the intended directory is known.
Library callers may pin `Config.MailStorePath` to an exact `V10` directory, which is still validated for regular `MailData/Envelope Index` files.

The Envelope Index is opened with SQLite `mode=ro`, a private connection cache, `query_only=1` and WAL participation; `immutable`, `nolock`, journal-mode changes and every SQL write are forbidden.
The parent directory is pinned through a descriptor-backed root; parent and database identities are recorded before connecting and compared with `PRAGMA database_list` and device/inode identities after opening.
One verified Mail-store directory descriptor anchors message, mailbox-cache and external-attachment opens, and macOS `O_NOFOLLOW_ANY` rejects a symlink in any descendant component. Nonblocking opens reject FIFOs before reading, without waiting for a writer.
Supported non-Darwin builds validate every component and report root replacement as `store_changed`; `js/wasm` and Plan 9 reject secure Mail-store opens with `unsupported_platform`.

Mailbox catalogs are parsed in-process from bounded, descriptor-opened XML property lists and serve identity matching only while message membership stays live in SQL.
The per-client mailbox catalog cache is bounded to five minutes, clones returned values, and is invalidated on close or configuration changes; malformed or oversized cache files keep typed diagnostics.
EMLX frames require a bounded XML plist trailer with one `plist` root, one `dict`, strict nesting and no trailing content; root version `1.0` is checked when present.
Only Mail's binary account-ordering preference needs one `plutil` extraction, and the store-opening context bounds that child process to 15 seconds.
Directory enumeration is never authoritative because Mail can keep stale or partial files: store rows select candidates, safe mailbox mapping resolves sources, and every result reports whether its local content is complete.
Spotlight is not required.

The supported Envelope Index profile is store version `4`, minor version `74003`, WAL journal mode and a valid store UUID, with `3826.700.81` as the verified `last_write_framework_version`.
Required schema properties must be unique and readable, and every required table column and index is capability-checked before the profile is accepted; duplicate, malformed, missing or unsupported values fail closed with `unsupported_mail_store_schema` before message queries, while unknown property keys are ignored.
Only a differing framework stamp degrades instead of failing: reads continue with the profile marked `unverified`.
Whenever a store profile is open, every JSON envelope carries `data.store_profile` with `state`, `framework_version` and `supported_framework_version`; the unverified state adds `code:"store_profile_unverified"`, human output prints one warning, and `doctor` reports the `mail-store-profile` check.
The degraded state grants no write access, no mutation relaxation and no trust in unverified layout details.

Capability discovery, `version`, `update`, help, unknown commands and subcommands, local draft create/list/inspect/preview/edit/update/discard/prune, sending, credential setup, direct transport-claim reconciliation and visible handoff bypass Mail-store configuration, SQLite, `plutil` and Mail.app initialization.
Reply and forward creation read the source header block from the Mail store and write only local draft files; `drafts open` and `drafts adopt` use the local store with targeted IMAP hydration and no Mail Automation; legacy baseline reconciliation uses the Mail store while direct claims do not.

### References and cursors

Account, mailbox, message, recipient, attachment, draft and cursor are typed values.
Message references are opaque and bind the Envelope Index UUID, row identity, message and global identifiers, account, mailbox identity, mailbox path and a short subject fingerprint; the subject text is not part of the ref.
The store revalidates physical identity and mailbox membership before every read or write translation; a changed store, moved message, reused row or mismatched subject returns a typed stale-reference error instead of touching a different message.
The `acct_`, `mbx_`, `msg_` and `cur_` reference prefixes and the `lcur_` store list-cursor prefix are stable; new `acct_`, `mbx_` and `msg_` refs use a short bounded binary payload, refs issued by earlier versions still decode, and malformed or noncanonical payloads return typed reference-validation errors.
`messages new --account` and `sync --check --account` select by decoded account ID across supported legacy, compact and binary encodings, with case-insensitive account matching; returned catalog refs remain canonical. Corrupt or unsupported account encodings retain `account_reference_corrupt` or `account_reference_version_unsupported` before credentials or network access.

Mailbox paths are account-relative arrays internally and escaped display strings externally, which keeps Gmail labels, iCloud folders and identically named nested mailboxes apart.

Message-list cursors remain store/mailbox-bound, filter and search cursors remain store/query-bound, draft cursors remain directory-revision-bound, and thread cursors retain their conversation binding under `data.thread`.
The account, mailbox, and attachment catalog cursors are versioned and bind the command, scope, and ordered stable identities.
Human account and mailbox lists show available continuation as `Next cursor:` in terminal tables or a `next_cursor` tab-separated line in pipes; use its exact value with `--cursor`. Terminal pages omit it.
List and search cursors bind the store UUID, query or mailbox fingerprint, sort anchor, row ID and nullable received-date state; page size is excluded, so a continuation may use a different `--limit`.
Cursors cannot be reused after a store replacement or with different filters.
Pagination is best-effort keyset pagination across invocations, not a persistent snapshot.

### Listing

`messages list --json` defaults to the unified inbox across active accounts.
One SQL statement selects physical and label membership, deduplicates rows and orders received dates descending with row ID as tie breaker; NULL dates come last.
Each item carries its account ref as `account` and its resolved `mailbox_ref`, also under projections.
`--account REF` narrows the inbox or mailbox selection.
`--mailbox` accepts an opaque ref, a case-insensitive role (`inbox`, `sent`, `drafts`, `trash`, `junk`, `archive`) or an exact slash-separated path; roles use proven Sent/Drafts cache attributes and localized role names, never guessed attribute bits, and ambiguity returns `ambiguous_mailbox` with candidate refs.
`messages filter` and `messages search` accept the same selectors; without `--account` a role or path covers the matching mailbox of every account (`filter --mailbox inbox --read false` is the unread inbox of all accounts), with `--account` it covers that account, and two matches inside one account stay ambiguous.
Inbox cursors bind the store UUID, account scope, resolved inbox set and date/row boundary.
Unified inbox and role/path selection require the supported local store and never trigger a global Apple Events scan; an explicit ref keeps the legacy fallback.

### Message detail

Message detail distinguishes normalized plain content, raw source, headers, attachment metadata, `content_source`, `content_complete` and `missing_parts`.
Large bodies, raw MIME and attachment bytes are never included in list responses.
MIME parsing selects one representation from each `multipart/alternative`, keeps mixed-part order, and marks malformed recipient or decoding data incomplete.
Text in legacy charsets (ISO-8859-x, Windows-125x, Shift_JIS, GB18030, EUC-KR, Big5 and the other encodings known to `golang.org/x/text`) is decoded to UTF-8 for reads, excerpts and body search; an unknown charset keeps its raw bytes and adds `mime-decoding` to `missing_parts`.
Attachment transfer-decoding errors and declared `X-Apple-Content-Length` shortfalls mark the message incomplete and list the MIME part path in `missing_parts`; recovered bytes and healthy attachment size and hash evidence remain available.

Parsing uses one aggregate budget per message: 32 MiB of decoded text, 4,096 visited entities, 64 nesting levels, 8 MiB of retained part and header metadata, and 128 MiB of source bytes.
A budget stop keeps validated content, sets `content_complete:false` and records `mime:budget:<resource>` in `missing_parts` (`text_bytes`, `parts`, `depth`, `metadata_bytes` or `raw_bytes`).
Cancellation returns the validated document with `mime:canceled` and preserves the cancellation error.
Raw headers are bounded to 1 MiB.

Received HTML has a 16 MiB source limit, a 262,144-token lexical preflight for dense input, and a post-parse limit of 262,144 nodes and 512 levels.
These are not an exact process-memory cap.
A failed conversion returns no truncated HTML text and adds `mime:html:<stage>` (`source`, `tokens`, `parse`, `tree` or `render`) to `missing_parts`; other valid parts, including a later plain alternative, remain available.
Incoming `text/html` uses the same semantic conversion as drafts and keeps image `alt` text.

Embedded `message/rfc822` and `message/global` bodies appear in the attachment list even without a filename or disposition; the MIME type and opaque part ID identify them, and `multipart/digest` children without Content-Type use `message/rfc822` ([RFC 2046 section 5.1.5](https://www.rfc-editor.org/rfc/rfc2046#section-5.1.5), [RFC 6532 section 3.7](https://www.rfc-editor.org/rfc/rfc6532#section-3.7)).
Saving such a part keeps the complete encapsulated message after decoding only its enclosing transfer encoding; nested headers and text never merge into the outer identity, body, attachment count or search text.
Embedded structure is validated recursively within the shared budgets, including during search; malformed or missing embedded headers, unsupported nested subtypes, externalized bytes and truncation mark the containing part and message incomplete.
A saved part's size and SHA-256 prove the extracted bytes, not structural completeness; inspect `content_complete` and `missing_parts`.
Multipart containers are never selected in place of leaf part `1`.

### Hydration

Complete local reads never contact Gmail, iCloud, IMAP, SMTP, OAuth or account-login endpoints.
Incomplete content is hydrated with bounded IMAP `FETCH BODY.PEEK[]` over the account's transport without launching Mail.app.
The shared 64 MiB message cap binds local raw-source reads and full-message hydration for reads and attachment saving: oversized local sources and remote literals fail with `raw_source_too_large` before buffering. Sent reconciliation uses the separate composed-message limits described below.
FETCH parses complete logical responses across literal boundaries, accepts UID and BODY in either order, ignores unrelated flag updates, and fails closed on duplicate or contradictory BODY values; an authoritative UIDVALIDITY change during FETCH invalidates the result.
The returned BODY section must match the requested full source, headers or header fields after case and PEEK normalization; unexpected partial offsets fail closed. Excerpts require the requested header fields and one non-NIL text prefix at offset zero, rejecting conflicting or duplicate sections.
FETCH, flag verification and UTF8 ENABLE share case-insensitive OK/NO/BAD completion parsing. Missing or unknown status atoms invalidate the session with `imap_response_malformed`; definite FETCH rejections retain `imap_fetch_failed` and their typed server rejection evidence. Optional UTF8 refusal retains modified UTF-7 fallback; UTF8=ONLY requires confirmed UTF8 enablement.
Source/excerpt FETCH, SEARCH, STATUS and ENABLE allow at most 1,024 logical responses and 4 MiB of aggregate protocol metadata per command, including ignored responses and literal framing. FETCH literal payloads have a separate aggregate allowance: the source cap or twice the excerpt prefix cap per requested UID. STATUS literals count as metadata. Exhaustion returns `imap_resource_limit_exceeded` and discards the session; post-dispatch APPEND uncertainty remains intact.
Large literals spill to private unlinked temporary files instead of the heap, which can add disk I/O latency.

A missing source is recovered through the reference's catalog-to-server UID mapping when it can be checked against the live row and mailbox-local `Info.plist` UIDVALIDITY.
Without that evidence, resolution requires an independent source-derived or reference-bound Message-ID and uses the exact mailbox-scoped search below; subject and sender never establish identity, even for a unique candidate.
Absent both supported identities, `imap_message_uid_unknown` stops before remote dispatch and asks for Mail.app synchronization and a fresh reference. Ambiguity returns `imap_ambiguous_message_id`, stale evidence `stale_reference`, and a renamed server mailbox stays `imap_mailbox_not_found`.
Successful hydration updates the returned reference with the verified UID and UIDVALIDITY.

Message-ID resolution treats UID SEARCH as candidate discovery, then fetches each candidate's `BODY.PEEK[HEADER.FIELDS (MESSAGE-ID)]` and counts only exact normalized headers.
Substring candidates are discarded; missing, duplicate or malformed Message-ID headers fail closed, and duplicate matches return `imap_ambiguous_message_id` before any UID becomes a target.
Malformed, zero, overflowing or duplicate UIDs, or more than one SEARCH response, return `imap_response_malformed` and discard the session.
Message-ID inputs are normalized once: a bare identifier receives angle brackets, while partially bracketed, empty, whitespace-containing, control-containing or unbalanced input returns `invalid_imap_value` before connecting.

When hydration fails, reads keep parseable local content.
`messages.get` and `drafts.open` return `ok:false`, exit `1`, a typed remote error and the retained partial `data.message`, whose `hydration` object carries `state` (`failed` or `canceled`), `attempted_source`, separate sanitized `local` and `remote` causes and `remediation`.
Human mode prints the retained content and the same diagnostics before exiting `1`.
A canceled hydration stays an error and is never a complete read; `hydration` is omitted after a complete read or successful fallback.

### Attachments

Attachment saving first copies a matching locally materialized external file; only a missing local attachment falls back to full-message IMAP hydration under the same 64 MiB cap.
External attachment discovery is bounded per directory to 10,000 entries, 128 hashed ambiguity candidates, and 1 GiB cumulative hash input.
A limit returns `attachment_resource_limit`; no partial scan reports an attachment as complete or downloaded.
External files take precedence over complete inline MIME data because they can change reported metadata and saved bytes.

The writer pins the destination parent directory, rejects symlinked or replaced parents, creates the output exclusively and publishes it with mode `0600`.
A parent swapped mid-write fails as `store_changed` and the owned file is removed; cleanup removes a name only while it still has the identity this operation created, and `attachment_changed` preserves replacements.
Verified size, SHA-256 and file identity from the authoritative copy pass are returned without a second content scan.
If an error follows verified publication, the error keeps `saved_attachment`, reports `effect_certainty:"complete"` and requires inspection before replay; if identity, size, mode or modification time no longer match, the evidence is omitted and the effect is unknown.
The final identity check does not rehash the file or prove immunity to same-user content changes.

Replay of an attachment save is allowed only when the output is proven absent and the cause is explicitly transient: cancellation or deadline, a Mail readiness failure proven before dispatch, or IMAP connection, cancellation, disconnect, timeout and FETCH failures with a preserved network or truncation cause.
TLS verification, input or configuration errors, missing or ambiguous attachments and attachments not yet downloaded require correction; resource-limit, integrity and deterministic-source failures require inspection without replay; unknown causes use `observe_required` with `recovery.action:inspect`.
Complete, partial or unknown publication outcomes always forbid replay while keeping verified `saved_attachment` evidence.

### State and threads

`mailcli messages state --ref MSG_REF` closes the verify-after-write gap without mutating anything.
It resolves the reference through the same identity, credential, mailbox and UIDVALIDITY path as mutations, then issues one bounded `UID FETCH <uid> (UID FLAGS)` on a shared read session.
`data.state` separates `server_flags` and `server_state` (`observed` or `missing`) from `local_index_flags` (which may lag until Mail.app syncs) and reports `flags_agree` plus a `staleness_note`.
A missing target is `server_state:"missing"` without an error; stale references, unsupported providers without binding endpoints and transports lacking flag reads fail before network access.

`mailcli messages thread --ref MSG_REF` lists local conversation members without network access.
`conversation_id` is Mail.app's opaque grouping key, not RFC References threading; `data.thread` includes the seed `ref`, `conversation_id` (zero when ungrouped), `messages`, `truncated` and available `next_cursor`/`prev_cursor`.
Without `--cursor`, the chronological page contains the seed, balanced around it where possible; NULL dates precede non-NULL dates and ROWID breaks ties.
`--limit` accepts 1 through 200.
Pass either cursor with the same `--ref`: next continues toward newer members, previous toward older members; `truncated` means more visible members exist in either direction.
Deleted and deactivated-account members are excluded.
Thread cursors bind the Mail store, seed and conversation identity and the ordering keyset, not the write generation, so paging continues while Mail syncs; legacy v1 cursors continue forward.
Inserts behind an already traversed boundary may need a new traversal; ungrouped messages return the seed alone without cursors.

### Search

MailCLI never executes Mail.app `whose` searches, which can trigger unbounded mailbox work that is not reliably cancelled.
`messages filter` and `messages search` without `--query` plan candidates through one parameterized Envelope Index query that resolves account, mailbox, sender, recipient, subject, date, read and flagged constraints.
`--attachment` also inspects each candidate's MIME source because Mail's attachment catalog can lag; a positive catalog count proves `--attachment true` and disproves `--attachment false` without I/O (reported as catalog-proven coverage), while catalog-zero candidates keep the MIME scan.
Partial or missing sources are excluded as unknown and make coverage incomplete.
Results use deterministic received-date and row-ID ordering.

Matching uses NFC normalization followed by Unicode simple lowercasing for queries, MIME text, attachment names, snippets and metadata SQL candidates; SQLite applies the same policy through the registered `mailcli_search_fold` function, and full case-fold expansions are excluded.
Date filters compare received timestamps in whole Unix seconds: `--after` is inclusive and `--before` exclusive.
Omitted bounds are unbounded; `1970-01-01T00:00:00Z` is a real boundary and negative timestamps work.
RFC 3339 offsets select that instant; `YYYY-MM-DD` selects local midnight.
With both bounds, after must be strictly earlier than before; NULL received dates match only without date bounds.
Keep the same date strings when continuing with a cursor.

`messages search --query TEXT` runs an on-demand, stateless MIME scan over the metadata candidates in scope and keeps no corpus after exit.
Two bounded workers stream each `.emlx` source, decode text/plain or text/html, and skip decoding non-text bodies so attachment names stay searchable.
The first scan window matches the requested page, doubles only after a window without a match, and never exceeds 64 candidates.
A full result page stops loading candidates; the cursor stays anchored at the last classified candidate.
Both search and filter accept these scan budgets; `--max-bytes` independently bounds JSON output.
`--max-messages` defaults to 50,000 and is capped at 100,000; it bounds scan candidates and the exact-count probe for metadata filters.
`--max-scan-bytes` defaults to 4 GiB and is capped at 8 GiB for body or attachment scanning.
These limits bound work rather than pretending that full-text search is instant; narrow account, mailbox, sender, date or subject scope for large stores.

Metadata-only pages fetch the requested page plus one continuation candidate and run no count query by default: `candidate_messages` is that observed lower bound and `candidate_messages_exact` is true only when no continuation remains.
`--exact-count` adds one bounded count probe after the current cursor (body probes cover `max-messages + 1` rows); a larger set fails with `search_count_limit_exceeded` before body scanning.

Every search page includes `data.page.coverage` with backend, candidate messages, candidate-count exactness, scanned messages and bytes, full, partial and missing sources, `catalog_proven_messages`, `sources_complete` and `complete`.
`sources_complete` is page-local: every processed candidate had conclusive source or catalog evidence for the requested query. Missing, partial or unparseable relevant source makes it false, including incomplete MIME parsing of full RFC bytes; `full_sources` alone is not proof. Empty and metadata SQL pages are true, as are attachment-only candidates conclusively decided by catalog evidence. An unprocessed continuation candidate or a result/message/byte bound alone does not make it false.
Body page `complete` still requires an exact candidate total, every candidate classified and no source loss; reaching a result, message or byte bound keeps it incomplete and continuation resumable. Complete search coverage requires a terminal `complete:true` page and the retained AND of `sources_complete` from every page of the same query/scan. A clean final page cannot erase earlier source loss. Following `next_cursor` continues the chain, including budget changes; restart or a changed query/index revision starts a new chain. This evidence retains best-effort consistency and does not promise a cross-process snapshot.
Known source problems (`message_source_missing`, invalid EMLX framing, an `ENOENT`-class open error) degrade only that candidate; unexpected open failures abort the search.
Pagination over body hits reports page-level incompleteness until the final page, so a page is never mistaken for an exhaustive result.
No refresh command exists because MailCLI maintains no index.

An incomplete body search returns `next_cursor` after the last fully classified candidate when later candidates remain; a candidate blocked after earlier progress gets an inclusive cursor and is retried with the next page's byte budget.
If no candidate was classified and the next source cannot fit, the search returns `search_budget_too_small` with `error.required_bytes` (rounded up to a binary MiB) and no cursor.
Recovery keeps the original search/filter command, query, filters including explicit false values, projection, incoming cursor, exact-count setting, candidate limit, requested threading/excerpt and excerpt length, output mode and `--max-bytes`. Only `--max-scan-bytes` increases.
If the required budget is invalid, unavailable or above 8 GiB, terminal guidance supplies no retry command; narrow the query or inspect the source before a new search.
Page size, `--max-scan-bytes` and `--max-messages` may change between pages; filters and `--exact-count` stay bound to the cursor.

Search pagination is best-effort because a SQLite snapshot cannot survive separate processes.
Every page exposes `coverage.consistency:"best_effort"` and a compact `coverage.index_revision` from Mail's `WriteTransactionGeneration` or a bounded Envelope Index/WAL metadata token.
Cursor v4 binds the query, store UUID, keyset position and that revision; supported v3 cursors still decode.
Insert, delete, move or store replacement invalidates continuation with `search_cursor_stale` or `invalid_cursor`, and a revision change during one page returns `search_index_changed`; restart without a cursor after either.
`.emlx` replacement is outside the revision, but framing and identity revalidation mark the candidate missing or incomplete.
This is detectable invalidation, not snapshot isolation.

### Sync check

`sync --check` compares the complete union of local and server mailbox identities over IMAP STATUS without Mail.app.
Each STATUS response must identify the requested mailbox and provide exactly one valid `MESSAGES`, `UNSEEN`, `UIDNEXT` and `UIDVALIDITY`; anything else is a per-mailbox `imap_response_malformed` failure.
Each mailbox result reports `state` (`matched`, `local_only`, `server_only`, `inaccessible` or `unresolved`), local and server count availability and the exact `server_name` when known.
Matched entries carry both counts only when the local count is available; a zero local count never stands in for a missing local mailbox.
A failed server LIST or an empty selectable server catalog makes coverage incomplete; `complete:false` and typed `failures` identify every uncovered identity.
Failure entries distinguish caller cancellation (`operation_canceled`) from elapsed comparison deadlines (`sync_check_timeout`); the caller's cause takes precedence over a stale server error.
Nothing to compare is not a failure: a local account without an address (`local_account`) and a mailbox with no local count that is empty on the server (`empty_without_local_count`) are listed under `skipped` and keep `complete:true`; a mailbox with no local count but server messages stays an `unresolved` failure.
`counts_match` is true when the check is complete and every compared mailbox has equal local and server counts, and `mismatched_mailboxes` counts the others (equal counts do not prove equal mail); when counts differ on a complete check `next` is `check_state` without a command, because the differences close once Mail.app has synced.
`sync_check_policy` reports that incomplete checks are successful results with exit `0` by default; automation that requires exhaustive coverage uses `--require-complete` (complete exit `0`, incomplete exit `3` with the same `ok:true` payload, runtime failure exit `1`, invalid flags exit `2`).
`--require-complete` without `--check` is invalid.
The check reports counts without downloading content or refreshing Mail's index.

### New mail on the server

`messages new` answers "is there mail the local store does not have yet?" without waiting for Mail.app.
It is opt-in and read-only: no other read command contacts IMAP for this, and nothing on the server changes (`BODY.PEEK` header fields only, no flag is set).
For each selected account (all with stored credentials, or `--account`) and mailbox (`--mailbox`, default the inbox), one `SELECT` and one sequence-number `FETCH` read the newest 100 server messages (`UID`, `\Seen` and the `From`, `Subject`, `Date` and `Message-ID` header fields); verified mailbox-local UIDs or local message headers decide which of them are new.
SELECT must establish the mailbox count; an explicitly empty mailbox needs no FETCH. Every requested sequence must return exactly one nonzero unique UID, explicit FLAGS (an empty list is valid), and the exact non-NIL requested header section before tagged OK can establish coverage. Omitted/duplicate/out-of-window rows, missing FLAGS, wrong sections, EXPUNGE or changed UIDVALIDITY invalidate the comparison instead of implying zero new messages.
Recent FETCH bounds each header to 16 KiB and each logical response to 1 MiB plus twice that header limit; the aggregate wire limit is the actual requested message count times that per-response limit plus 1 MiB of protocol overhead. At most 1024 ignored unsolicited responses, including FLAGS-only updates, are accepted; they never fill a requested row. Coverage and limit failures discard the session and appear under `failures` with `complete:false`.
A mailbox the local store keeps as physical rows (`matched_by:"uid"`) is compared by server UID only when local and server UIDVALIDITY match. A mailbox it keeps as labels of other rows (Gmail: every message lives once in All Mail and the local rows carry All Mail UIDs, so INBOX UIDs are not comparable) narrows local candidates by sender address, sent time and normalized subject (lower case, single spaces, without reply and forward prefixes such as `Re:`, `AW:` and `Fwd:`), then verifies their RFC Message-ID from the local source (`matched_by:"headers"`). A server message without a sender or `Date` counts as new because it cannot match; missing Message-ID or unavailable local source on a candidate makes comparison incomplete instead of silently equating different messages.
Local membership includes only non-deleted physical rows or label links to non-deleted rows; deleted and dangling label links cannot hide new mail or select a comparison mode. Visible physical candidates require matching local UIDVALIDITY, including mixed physical/label membership. With no visible members, every verified recent server row is missing locally: `matched_by:"uid"` denotes the empty-set comparison and asserts no local UID or generation equality. No local UIDVALIDITY is needed in that case, and local-reference lookup returns no counterpart; a positive label match still requires the exact RFC Message-ID.
Subjects and sender names are decoded from RFC 2047 words in every charset MailCLI reads (for example Windows-1252 and ISO-8859-15), so a row shows the text a mail client shows.
Each mailbox result carries `matched_by`, `server_messages`, `scanned_messages` (validated recent rows, at most 100), `window_limited` (server count exceeds scanned rows), `new_count` (missing only within that scanned window), `truncated` (missing scanned rows exceed `--limit`, or all scanned rows are missing while older server messages remain outside the window) and `state`: `checked`, `unresolved` (no local mailbox record, no server UIDVALIDITY or no local UIDVALIDITY for visible physical candidates, with `reason`) or `uidvalidity_changed` (local and server UIDVALIDITY differ for physical candidates, so no row is returned). Either unverified state makes the overall `complete` false. Validated window evidence remains available even when local comparison is unresolved; a failed FETCH appears only as a failure, not as a successfully scanned empty mailbox.
`window_limited:true` alone does not make account/mailbox coverage incomplete, and `new_count` never counts historical missing mail outside the window. The default returns exit 0 for partial comparison results; `--require-complete` returns exit 3 for `complete:false` with the same `ok:true` result and discovered rows. Runtime and invalid-input failures retain exits 1 and 2. Human output includes `scanned_messages` and `window_limited` beside the server count.
Caller cancellation, including a wrapped cancellation cause, stops discovery with `operation_canceled`, exit 1 and `next.do:stop`; no later account, credential lookup or mailbox read starts. An elapsed comparison deadline remains a partial result with `operation_timeout` failure entries, preserving unrelated typed server failures when the caller remains live.
Rows are newest first and carry `server_ref`, decoded single-line `subject` and `sender`, `date_sent` (UTC), `message_id` and `unseen`.
They are server evidence, not local messages: a `server_ref` (`srv_` prefix) is opaque and binds account, mailbox path, UIDVALIDITY and UID.
`messages get` (`metadata`, `plain` and `full` views, `--links`, `--fields` and `--export` as for local refs), `messages raw`, `attachments list` and `attachments save` accept it and read the message over IMAP with `BODY.PEEK`, so nothing is marked as read; the same size limits, `content_complete`, `missing_parts` and typed IMAP failures (UIDVALIDITY changed, message not found, credentials, timeout) apply.
The result qualifies `summary.read`, `summary.flagged` and `summary.deleted` with `summary.flags_state`: `observed` means FLAGS was fetched in the referenced UID generation (an empty FLAGS list verifies false); `missing` means the UID was absent; `unverified` means optional flag access, identity resolution or transport failed. Missing and unverified retain useful content, but their default false booleans do not prove unset flags. The existing `staleness_note` remains; this evidence does not verify `junk`, and local-only summaries omit `flags_state`. Detail and batch-read summaries retain the qualification under every projection. Detail subject/sender values use the complete bounded decoded headers with safe single-line presentation; discovery triage keeps its 200-rune limit.
Caller cancellation or deadline expiry during optional flag observation returns an error with the already-read message evidence for every server read intent. An optional flag timeout while the caller remains live still yields useful content with unverified flags.
`summary.local_ref` appears once the local counterpart is verified; from then on the local ref gives the full command set. A local ref shown through a label resolves its IMAP UID in that label's mailbox by Message-ID, never by the physical All Mail UID.
Batch `read` and `attachment_save` items accept a server ref like the commands they mirror; every other command (`mark`, `move`, `copy`, `delete`, `reply`, `forward`, `state`, `thread`, drafts and batch mutation items) rejects a server ref with `invalid_reference`, and nothing converts a server ref into a local one.
An account without credentials, without network or in a degraded state is listed under `failures` and keeps the call successful with `complete:false`; local accounts are listed under `skipped`.
Incomplete comparison takes precedence in `next`: `ask_user` points to failures and unresolved mailbox reasons even when valid new messages were found. On a complete comparison with new messages, `next` is `check_state` without a command; `sync` asks Mail.app to fetch them into the local store and does not repair inaccessible accounts.

### Mailbox resolution

The supported scope is every active account and every mailbox represented consistently by the Envelope Index and mailbox catalog, including Inbox, Sent, Drafts, Archive, Junk, Trash, custom folders and nested Gmail labels.
`mailboxes resolve` accepts one `--path` segment per server-provided hierarchy level, so localized folders such as `Gesendet` and `Entwürfe` need no guessed identifier.
IMAP LIST keeps each exact wire name separately from its decoded display name, display path, hierarchy delimiter, special-use flags and negotiated Modified UTF-7 or UTF-8 encoding; NIL delimiters stay flat.
Resolution prefers byte-identical display paths, then compares each segment under NFC canonical equivalence without compatibility folding; ambiguous normalized matches fail closed, and the case-insensitive INBOX identity is unchanged.
Multiple special-use candidates fail with `imap_ambiguous_mailbox` and list their exact wire names; correct the colliding names or special-use assignments, refresh the list, and rerun only once the mailbox resolves uniquely.
Per-client LIST results are cached by host, port, username and account identity without password material, and entries expire after five minutes.

LIST uses a 32 MiB cumulative wire-response limit, 10,000 untagged logical response lines (including ignored untagged replies), and 10,000 mailboxes per LIST operation.
The 1 MiB physical-line, 8 MiB logical-response, 1 MiB per-literal and 128-literal ceilings also apply.
Exceeding an aggregate ceiling returns `imap_resource_limit_exceeded` with `limit:{name,value}` and `observed_at_least`, a proven lower bound (at least `limit.value + 1` bytes when the exact size is unavailable), and discards the session.
Standalone overflow is terminal with no effect; existing partial-effect or outcome-uncertain guidance takes precedence.

## Drafts and composition

Local structured drafts are the review boundary.
Drafts are private JSON files under `~/Library/Application Support/MailCLI/drafts`, not fragile unsaved Mail compose objects; creating, editing or updating one never sends mail.
Creation and updates write a temporary file and atomically rename it into place, so a crash never leaves a partial `draft_*.json`.
Scripted Mail compose is disabled on Mail 16 (see [Native compose boundary](#native-compose-boundary)); delivery uses direct SMTP and IMAP, and visible handoff is the only native compose path.
Direct transport covers Gmail and iCloud domains; other domains fail with `transport_unsupported_provider` unless an account binding pins explicit endpoints.

### Create and update

Draft creation accepts either a typed JSON document or terminal-native `--from`, repeatable recipient and attachment flags, subject, `--body`/`--body-file` and `--format plain|markdown|html`.
JSON and native modes are mutually exclusive; the JSON `body` key is mandatory even when intentionally empty.
Draft and batch JSON must use valid UTF-8, with every escaped high Unicode surrogate immediately paired with a low surrogate and no isolated low surrogate. Invalid encoding returns `invalid_input` before publication or dispatch; valid pairs, literal U+FFFD and escaped literal backslash-u text keep their decoded meaning. Editor JSON uses the same boundary, and diagnostics never echo rejected values.
Recipient fields are arrays of `{name, address}` objects, and the same normalized address cannot occur more than once across To, CC and BCC.
`address` values must be valid mailbox syntax, including required local-part quoting such as `"A B"@example.com`; `name` affects presentation only.
Mailbox identity and case survive composition, SMTP envelopes, reply targets, sender identities and bindings; malformed unquoted values are rejected rather than repaired, and every role, including BCC, rejects prohibited controls and invalid UTF-8 before SMTP.
Attachment paths must be absolute regular files; MailCLI rejects an oversized file before hashing and records accepted sizes and SHA-256 digests in the draft.

Every created, inspected, previewed, edited or updated draft exposes an opaque `revision` computed from its validated canonical send state.
It covers reference and kind, account and sender, ordered To/CC/BCC names and addresses, subject, body format, source, plain and HTML, source and thread identity, and ordered attachment path, size and SHA-256.
Timestamps, attachment mtime, diagnostics and operational claims do not change it, and stored revision text is never trusted.
Review the complete create or update result requested with `--view full`, or fetch it with `drafts inspect --ref REF --view full --json`, before sending, and retain `data.draft.revision`.
A metadata-only response, truncated output or a returned revision alone does not prove the content was reviewed; list summaries are discovery, not review snapshots.

`drafts update` applies patch semantics: omitted editable fields keep their values, while explicitly supplied fields, including intentional empty strings or arrays, replace them.
Omitted attachments keep their paths; clearing them requires an explicit empty `attachments` JSON array.
Changing `body_format` requires a new `body`; repeating the stored format keeps the body.
`drafts update` and `drafts send` require `--expected-revision REVISION`, also with `--input FILE|-`; the revision is never an editable JSON field and omission never selects the current draft.
Both compare it under the draft lease before replacing content, consuming send evidence, creating a claim or contacting transport.
A mismatch returns exit `1` with `draft_revision_conflict` and `error.draft_revision_conflict` (`ref`, `expected_revision`, `current_revision`); review the current full content before an explicit retry.
New send claims and receipts retain `draft_revision`; legacy claims or receipts without it return `draft_revision_unavailable` from send and remain readable through `drafts inspect` and `drafts reconcile`.

### Rich content

Markdown is rendered with Goldmark, including GFM tables, strikethrough and autolinks; task-list markers stay literal.
The plain-text part separates paragraphs, headings, quotes, top-level lists and tables by one blank line.
Drafts stored by an earlier version keep their original rendering and stay valid.
HTML is parsed in-process, reduced to a strict element and link allowlist with all active and remote content removed, and stored with a canonical plain-text representation.

| Scope | Supported contract |
| --- | --- |
| Elements | `a`, `b`, `blockquote`, `br`, `code`, `del`, `em`, `h1`-`h6`, `hr`, `i`, `li`, `ol`, `p`, `pre`, `s`, `strong`, `table`, `tbody`, `td`, `tfoot`, `th`, `thead`, `tr`, `u`, `ul`; other wrappers are unwrapped when their children are safe. |
| Attributes | `a[href]` accepts absolute `http`, `https` or `mailto` URLs; `a[title]` is kept. Every other attribute is removed and every link receives `rel="nofollow noreferrer"`. |
| Presentation | Semantic typography, headings, lists, preformatted and code whitespace, and table rows and cells. CSS and `style` attributes are never kept. |
| Resources | Images, forms, media, frames, scripts, SVG, MathML, templates and remote resources are removed. An image `alt` value becomes plain text; images are not embedded from local paths or `cid:` references. |
| Diagnostics | Deduplicated first-seen records with codes `removed_element`, `removed_attribute`, `unsafe_attribute_removed`, `unsafe_url_removed`, `unsafe_style_removed` or `remote_resource_removed`, holding only the element and optional attribute name. |

The plain alternative keeps non-redundant absolute link targets as `(URL)`, preserves `<pre>` and `<code>` whitespace, emits indented list markers and pipe-delimited table rows, and omits unsafe links and active subtrees.
Create, update, reply and forward return and persist value-free `content_diagnostics` with the canonical body: lossless rich content stores `[]`, plain content `null`.
Mutation gates (update, send, handoff and historical save reconciliation) re-render the stored source and require stored diagnostics to match exactly, otherwise they fail with `draft_state_error`.
Inspection reads (`drafts inspect`, `drafts preview`) validate structure without re-rendering; omitted or null legacy diagnostics are reconstructed without rewriting the file or changing its revision.

Source, rendered Markdown, serialized HTML and plaintext are each bounded at 4 MiB.
The parsed tree is checked for at most 65,536 nodes and 512 levels, and link-label capture is limited to 16 MiB across a rich rendering.
Limit failures return `invalid_argument`, never truncated content.
Structures that need HTML5 repair after filtering, such as removed table captions or nested anchors, keep a bounded normalization tree so canonical plaintext keeps its meaning.

### Replies and forwards

Reply and forward drafts derive from the source message's stored header block without a body scan. A safely missing local source uses the existing targeted IMAP header fetch with verified UID and UIDVALIDITY; a bound Message-ID must match. Unsafe or stale sources fail closed, and an operator without header-only fetching never falls back to a full-body fetch for derivation.
The subject becomes `Re: <subject>` or `Fwd: <subject>` with stacked common localized and numbered prefixes collapsed; leading list tags and display case are preserved.
For automatic reply targets, every valid Reply-To address becomes a recipient in header order, and From is used only when Reply-To is absent; a malformed, empty, repeated or partially parsed Reply-To fails with `invalid_message_source`. Explicit To recipients and forwards do not require unused From or Reply-To fields to be valid. Automatic reply-all CC still requires complete source To/CC evidence and excludes final To recipients and verified own identities.
Reply-all moves the other To and CC recipients into CC, excluding reply targets, any final To address and the usable source account's email addresses, configured aliases and discovered sender identities. Own identities are excluded only from automatic CC, never from explicit recipient input. Malformed source To or CC fails automatic reply-all with `invalid_message_source` before a draft is created; ordinary reply and an explicit CC override remain usable.
Explicit input fields win over derived values, including an intentionally empty `subject`, `to` or `cc`; native recipient flags accept an empty value for that case.
Without `--from` and `--account`, the sender is inferred from the account that holds the source message: exactly one distinct own identity matching source To or CC, otherwise its only identity when no match exists. Multiple matches, malformed To/CC, an unavailable or degraded account leave automatic `from` and account selection unset; recipient order never breaks a tie.
`drafts create`, `inspect`, `preview` and the reply and forward responses carry `from` (empty when unset) and `send_blockers` (`from_missing`, `recipients_missing`); the blockers are review evidence and never part of the revision.
`send_blockers` covers only these two content checks, which need no contact with any server: an empty list does not mean the draft can be sent, because `drafts send` still validates stored limits, claims, the thread source, addresses, sender identity and transport, and its errors are authoritative.
Reply-all CC is deduplicated against final To before validation. Recipient comparison parses mailbox syntax and folds only domain case; local-part case and required quoting are preserved, so `User@example.com` and `user@example.com` remain distinct.
Automatic reply targeting requires a single unambiguous From author when no Reply-To list is available. Multiple authors, malformed From fields and incompatible duplicate From fields fail source validation; a valid Reply-To list or explicit To recipients remain authoritative.
The draft stores a canonical source Message-ID and thread chain: valid entries in first-seen order without duplicates, the direct parent once at the end, capped at the newest 20 entries. When References is absent and the source has exactly one valid In-Reply-To ID, that ancestor precedes the direct parent. Present References take precedence; multiple In-Reply-To IDs supply no fallback chain, and a malformed relevant header fails source validation.
Sending emits that `In-Reply-To` and `References` chain; metadata and composition share the same msg-id parser, including comments, adjacent IDs, legacy comma separators and UTF-8 IDs. Malformed atoms/domains, incomplete chains and raw CR/LF are rejected before a draft is created ([RFC 5322 section 3.6.4](https://www.rfc-editor.org/rfc/rfc5322.html#section-3.6.4), [RFC 6532 section 3.2](https://www.rfc-editor.org/rfc/rfc6532.html#section-3.2)).
Both commands require a fresh store-bound message ref and the Mail store.
Sending a forward needs at least one explicit recipient, and SMTP success does not guarantee conversation grouping.

### Editor

`drafts edit` is human-facing (`audience:human`) and needs stdin and the selected output (stderr in JSON mode) on the same foreground terminal; otherwise it returns `editor_terminal_unavailable` before launch.
Nonterminal stdin returns `interactive_required` with `drafts.update` recovery carrying the real ref and revision with `--input -`.
It writes a mode-0600 temporary JSON file in a mode-0700 private directory, runs the editor directly without a shell in its own foreground process group, validates the complete result, and replaces the draft only if the revision captured before launch still matches under the draft lock.
`drafts edit --json` reserves stdout for one envelope; editor output goes to stderr.
Terminal settings and the prior foreground group are restored after exit, failure or cancellation.
Editor failure returns `editor_failed`; cancellation or SIGINT/SIGTERM returns `editor_canceled`.
Unsuccessful candidates stay in their private directory, and errors carry `error.draft_editor` with `ref`, `expected_revision`, `candidate_path` and, when available, `exit_code` and terminating `signal`.
A concurrent change keeps the newer stored draft and reports `candidate_path` with the conflict; merge explicitly and save with `drafts update --ref REF --expected-revision REVIEWED_REVISION --input CANDIDATE_PATH --json`.
Successful updates remove the temporary directory; never blindly replay the editor.

### Listing and preview

`drafts list --limit N [--cursor CURSOR]` defaults to 20 summaries and accepts 1 through 200.
JSON keeps summaries at `data.drafts` and exposes `limit`, `revision` and optional `next_cursor` at `data.page`; records use ascending reference order, and `--limit 0` is invalid.
Summaries hold ref, subject, recipients, timestamps, format, attachment count, age in days, whether a send attempt was recorded, and redacted send, save and handoff attempt metadata, never bodies, HTML, raw MIME or attachment bytes.
A corrupt draft appears as a minimal `state_error` entry; each file is bounded to 20 MiB and retained summary metadata to 1 MiB.
The listing revision binds the root, directory identity and modification time; changes during or between pages return `invalid_cursor`, requiring a restart without `--cursor`.
It is change detection, not a content copy, and it never authorizes update or send.
`drafts inspect` fails closed on structurally invalid state with the file and remediation, and `drafts preview` renders plain, source or sanitized HTML without fetching remote resources or truncating bodies.

### Open and adopt

`drafts open --ref` reads a store message or Mail.app draft through the same retrieval and targeted hydration path as `messages get` without opening a compose window.
Local `draft_*` refs are not message refs; review them with `drafts inspect` or `drafts preview`.
Mail 16 exposes no reliable headless editor for a persisted draft.

`drafts adopt --ref MSG_REF` copies a Mail.app store draft into a new local draft; the original store draft is never modified.
Sender, recipients, subject and plain body map onto draft fields, and attachment bytes are saved into a draft-owned `<REF>.attachments` directory so the draft pins them by path and fingerprint.
Limits match `drafts create`; a draft without recipients is adopted as-is, but sending still requires a recipient.
Attachments download into an owned mode-0700 sibling staging directory `.mailcli-adopt-<REF>` on the same filesystem without holding a draft lease during network I/O, then publish under the preassigned ref lease.
An incompletely materialized source fails with `adopt_source_incomplete`; per-attachment failures keep codes such as `attachment_not_downloaded`.
Once publication has started or staging remains, the result is unknown and replay is forbidden: inspect `drafts list --json`, then the new ref if it exists.
`drafts discard` removes the adopted attachment directory with the draft.

### Discard and prune

`drafts discard --confirm` removes only the named local draft and its claims.
`drafts prune` is a dry run by default.
It lists never-sent drafts older than 30 days (`--older-than` accepts 1 through 106,751 days), expired terminal send receipts with fixed 30-day retention, draft refs whose claim, spool or handoff snapshot survives without its draft file, and eligible unpublished temporaries.
With `--confirm` it deletes exactly those drafts with their claims and lock files, re-verifying age and claim state under each draft lease, removes only receipts without a draft or unresolved claim, and sweeps verified inactive temporaries and orphan claims and spools.
Drafts with a send or save attempt are reconcilable state and are never pruned; busy refs are skipped.
Handoff snapshots and their claim are swept only when a matching prepared claim proves dispatch never started; dispatched, ambiguous, changed or non-empty unclaimed evidence is kept and reported as a prune failure, while verified empty private snapshot parents can be removed.
Temporaries matching `.<REF>.<suffix>.mailcli-<24 lowercase hex>` for the known `json`, `send-spool`, `send-claim`, `save-claim` and `handoff-claim` writers are eligible only when older than ten minutes; unknown suffixes are kept and counted with up to 20 diagnostic names.
Prune classifies the root in one bounded streaming pass without decoding bodies; a 64 MiB classification bound returns `prune_candidate_limit_exceeded` before any deletion.
Dry-run JSON reports the captured `revision` and whether the directory stayed unchanged (`stable`); unrelated modification-time drift returns `stable:false`, while a vanished or replaced directory is an error; confirmed prune rechecks that revision before cleanup and returns `prune_state_changed` without deleting when it changed.
Human output lists `would sweep artifacts` and `swept_artifacts`; partial failures keep completed effects and per-ref failure rows with a nonzero exit, matching JSON.
These checks protect supported operations but are not an atomic compare-and-unlink against another same-user process.

### Draft locks

Every lock-owning draft command takes a two-second BSD `flock` on a per-draft lock opened relative to the pinned private draft directory; the command keeps its own deadline.
A busy lock's own two-second budget returns `draft_busy`; caller cancellation returns `draft_operation_canceled`, while expiry of the outer operation deadline returns `draft_operation_timeout`. A waiting command never unlocks another process's lease.
Each operation keeps a descriptor-backed root tied to the lock's parent identity and performs every draft, claim, spool, receipt, temporary and cleanup operation through it, so renaming or replacing the configured root cannot redirect the operation; a changed identity fails as `draft_lock_unsafe` or `draft_lock_changed`.
Draft JSON publication holds the exclusive ref lease from before temporary creation until publication or verified cleanup.
Mark, move and delete reject messages identified as drafts unless `--allow-draft` is explicit; close any Mail editor for that draft first, because Mail can recreate it when the editor later saves.
Copy leaves the source draft unchanged and needs no draft flag.

### Native compose boundary

Live verification against Mail 16.0 build `3826.700.81` showed that scripted compose setters can report success while body and recipients are later missing, attachment insertion can fail with Apple Event `-10000`, automatic save can persist only a signature, and `close saving no` can leave an invisible outgoing backend.
These are data-integrity failures.
Scripted native composition remains disabled before baseline capture, claim creation, gate acquisition, or any Apple Event.
Capabilities report `compose_write:false`, `compose_attachment_write:false`, `raw_mime_send:true` and `send_transport:"smtp"`.
The unsupported `drafts save` command is absent from dispatch, schemas and capabilities; invoking it returns the normal drafts unknown-command error.

Historical send and save claims stay readable through reconciliation.
Historical draft-save recovery uses `mailcli drafts reconcile --ref <DRAFT_REF> --json`: it revalidates the claim under the exclusive draft lease and never starts a new save.
Exact observation returns `data.saved_draft` and permits verified cleanup; an unknown result returns `draft_save_outcome_unknown` and keeps the claim and draft.
Store observation requires exact final native headers, recipient roles and attachment count plus a body matching the materialized snapshot after canonicalization (line separators fold to LF, Mail placeholders and `> ` prefixes unquote, whitespace collapses, and bounded send-time signature and quoted-reply tails strip).
Signatures without a delimiter, re-wrapped text and HTML differences can remain `send_outcome_unverifiable` and need manual reconciliation.

## Sending

`drafts send --ref REF --expected-revision REVISION --confirm` bypasses Mail.app entirely.
It needs no Full Disk Access or Automation permission; the first Keychain read may show one macOS consent prompt.

1. Before provider or credential resolution, composition, claim creation or any network contact, a historical `save_attempt` returns `draft_save_retry_blocked`; the draft and save claim stay byte-identical for `drafts reconcile` or explicit discard.
2. Endpoints resolve from the sender's account binding first: explicit binding hosts take precedence over the provider table for Gmail (`gmail.com`, `googlemail.com`) and iCloud (`icloud.com`, `me.com`, `mac.com`).
   Other domains fail with `transport_unsupported_provider` before Keychain access or network connection unless both legs resolve to explicit binding hosts.
3. The app-specific password loads from the Keychain; only a missing item or an empty successfully loaded password returns `smtp_credentials_missing` naming `mailcli send setup`. Other credential read failures retain their code and cause and stop before submission or mirroring.
4. MailCLI builds the RFC 5322 message with a locally generated Message-ID and atomically retains the composed bytes in a private mode-0600 `<REF>.send-spool`; the send claim records the Message-ID, envelope and versioned MIME fingerprints and the spool's size and SHA-256.
   Before SMTP submission, `send_attempt.recovery_identity` pins the resolved IMAP account ID (when bound), host, port and credential username; it contains no password.
   Each attachment is read once through SHA-256 during composition; a fingerprint mismatch names the attachment and aborts before SMTP.
5. The bytes are submitted over SMTP with STARTTLS and appended to the Sent mailbox over IMAP, which always uses implicit TLS on the bound port (993 for Gmail and iCloud); an IMAP server that offers only STARTTLS is unsupported; each consumer reads an independent view pinned to the spool's identity, so path replacement cannot redirect bytes or cleanup.

Cancellation stops new hashing, spooling and transport work and releases the draft lease; a send claim is kept whenever SMTP or its final outcome is unknown, so cancellation never causes an automatic resend.
External attachment discovery, copy and output verification, plus recovery-spool copy and verification, observe cancellation between bounded reads. Canceled verification preserves the cancellation cause and retained evidence; terminal cleanup still completes under its owning draft lease. Native filesystem Sync calls retain their existing semantics.
Each accepted-message recovery spool is bounded to 1 GiB and is removed after durable terminal send evidence; unresolved or corrupt evidence stays for explicit recovery.

### SMTP rules

SMTP DATA reads the declared size plus one probe byte.
A short, oversized or unreadable source returns `smtp_source_invalid` before the terminator with `effect_certainty:none`, `retryability:observe_required`, `replay_allowed:false` and recovery `drafts inspect --ref REF --json`.
Write, deadline and transfer failures before the `.\r\n` terminator is attempted return `smtp_data_incomplete`; the transient claim is cleared and an explicit send retry is safe.
Once the terminator write starts, a failed write or unreadable final reply is `smtp_submission_unknown`: keep the claim, reconcile, and never replay while acceptance is unknown.
Final SMTP replies are bounded to 128 lines and 64 KiB including CRLF. Multiline overflow is a protocol failure after DATA and remains `smtp_submission_unknown`, including replies that begin with a negative status; it never proves acceptance or rejection.
Greeting, EHLO/HELO, AUTH, MAIL, each RCPT and DATA acceptance additionally cap received wire bytes at 128 KiB per command; STARTTLS, its verified TLS handshake and automatic post-TLS EHLO share a 1 MiB inbound budget. TLS framing is counted, ordinary fragmented replies work, and overflowing early replies stop before message data or its terminator is sent. Existing TLS message limits and final-reply limits remain enforced.
After DATA, a complete SMTP 4xx or 5xx final reply is a definitive `smtp_rejected` whose `error.message` keeps the full reply and enhanced status; 4xx may be retried by a later explicit send once the server condition is resolved, 5xx needs corrected content or recipients.
If SMTP accepted the message but closing the composed reader fails, MailCLI keeps the acceptance evidence and diagnostic and never submits again.

SMTPUTF8 is decided from actual envelope addresses and raw headers at every MIME level; encoded display names with ASCII addresses and UTF-8 bodies alone do not need it.
When the envelope does not already require it, the client inspects the replayable source before MAIL with bounded MIME traversal (8 MiB aggregate header accounting, 4096 entities, nesting depth 64) without rewriting bytes; an unclassifiable structure fails with `smtp_rejected`.
Internationalized envelopes or headers require SMTPUTF8 and 8BITMIME after STARTTLS; otherwise `smtp_utf8_unsupported` stops before AUTH, MAIL, RCPT or DATA, clears the transient claim and keeps the unchanged draft.
Address identity and case are preserved with SMTP quoting restored; there is no transliteration ([RFC 6531](https://www.rfc-editor.org/rfc/rfc6531#section-3.2), [RFC 6532](https://www.rfc-editor.org/rfc/rfc6532#section-3.7)).

MIME composition keeps decoded subjects and display names using bounded UTF-8 encoded words only when needed.
Every generated header line is 998 bytes or shorter excluding CRLF; fields with encoded words use a 76-byte limit and each word stays within 75 bytes without splitting a UTF-8 character.
Message-ID, thread identifiers, address specifications and MIME parameters keep their structured syntax and are never encoded words.
Invalid UTF-8, prohibited controls or a structured token that cannot fit a legal line fail before SMTP with a header-specific `invalid_argument` and no send claim; thread-source validation keeps `invalid_message_source` ([RFC 5322](https://www.rfc-editor.org/rfc/rfc5322#section-2.1.1), [RFC 2047](https://www.rfc-editor.org/rfc/rfc2047#section-2)).
Encoded bodies and attachments stream into the private spool; no complete encoded body is accumulated in memory.

### Sent mirror and evidence

A confirmed result keeps compatibility outcome `sent` and exposes `submission_accepted:true` only for the final SMTP 2yz reply after DATA, plus `sent_copy_observed:true` only for exact Sent persistence; neither confirms recipient delivery.

| Compatibility outcome | `submission_accepted` | `sent_copy_observed` | Meaning |
| --- | --- | --- | --- |
| `sent` | `true` | `true` | Final SMTP 2yz response and exact Sent persistence are observed; recipient delivery is unconfirmed. |
| `sent_mirror_pending` | `true` | `false` | SMTP submission is accepted, Sent persistence is incomplete, and recipient delivery is unverified; do not resubmit. |
| `outcome_unknown` | `false` | `false` | Submission or recording ended before a deterministic boundary; the retained claim blocks replay. |
| explicit SMTP rejection | `false` | `false` | The server rejected the submission; no Sent-copy or recipient-delivery claim is made. |

If SMTP accepted the message but the Sent mirror failed, the outcome is `sent_mirror_pending`; MailCLI never resubmits, and `drafts reconcile` searches and verifies Sent before retrying a known failed APPEND from the retained exact bytes, even when the original attachment paths changed or vanished.
Every Sent APPEND is preceded by a durable mirror-attempt marker with a unique attempt ID; a known pre-APPEND failure may retry only after a fresh Sent search and a new marker.
Only one complete SEARCH response with unique positive 32-bit message identities proves absence or candidates; missing, repeated or malformed SEARCH evidence blocks APPEND. Malformed post-APPEND evidence retains `imap_append_outcome_unknown` and its underlying cause, never authorization to repeat the write.
A literal copy, source-length, deadline or flush failure before the terminating CRLF returns `imap_append_incomplete` and discards the session; reconcile searches Sent, then retries only that APPEND.
After the terminating CRLF, a failed write or unreadable reply returns `imap_append_outcome_unknown`; reconciliation searches and verifies Sent but never retries automatically, and a duplicate or unprovable result stays `send_mirror_outcome_unknown`.
Missing or changed recovery-spool bytes block APPEND and keep the draft.
Recovery compares the current target with the retained original identity before credential lookup or IMAP contact. Account, endpoint or username changes return `send_identity_unverifiable`, retaining the claim and spool; inspect `send_attempt.recovery_identity` and restore the original target before reconciliation. Password rotation for an unchanged target remains supported.
After SMTP acceptance every mirror rejection requires `drafts.reconcile`; never rerun `drafts.send`.

After terminal evidence, MailCLI writes a private `<REF>.send-receipt` before removing the draft, claim and spool.
The receipt keeps the reviewed `draft_revision`, attempt and completion times, final outcome, `submission_accepted`, `sent_copy_observed`, SMTP response, Message-ID, Sent mailbox, optional UIDVALIDITY and UID, and append state.
Repeating `drafts send` with the same revision, or `drafts reconcile`, on a consumed ref returns that receipt without network writes, and `drafts inspect` shows it under `data.send_receipt`.

Crash recovery closes the submit-to-record window.
If the process dies after SMTP acceptance but before the claim update, `drafts reconcile` finds `outcome_unknown` with a Message-ID and verifies it against Sent: exactly one match is fetched and checked against Message-ID, sender, recipients, subject, body and complete MIME fingerprint before the claim becomes `sent`, proving a Sent copy, not delivery.
Sent verification uses an owned streaming FETCH source bounded by the 1 GiB composed spool limit, hashes decoded attachments incrementally within the 512 MiB aggregate and 100-attachment limits, and keeps the existing 16 MiB text-part and 64 MiB header bounds. The source must match its declared length and close successfully before adoption. Legacy byte-only fetchers retain the 64 MiB source cap and fail closed for larger candidates.
Duplicate matches, an identity mismatch or an unreadable candidate fail closed; absence returns `send_outcome_unverifiable` with manual remediation and never triggers an automatic retry, and a fingerprint mismatch returns `send_fingerprint_mismatch`.
Legacy claims without a Message-ID stay blocked with `send_reconcile_unavailable`, and claims without a versioned MIME fingerprint or original IMAP recovery identity with `send_identity_unverifiable`. The original target is never guessed from current configuration; unresolved legacy claims need manual Sent verification. Historical terminal receipts and store-baseline reconciliation remain readable.

If the process stopped after publishing a spool but before creating its claim, the next send holds the exclusive draft lease, verifies that no claim exists and that the exact spool is a bounded regular mode-0600 file with unchanged identity, removes it and composes afresh.
An ambiguous or foreign spool is kept and `drafts send` refuses before SMTP with `send_recovery_spool_changed` (`effect_certainty:none`, `retryability:user_input_required`, recovery `drafts.inspect --ref REF --json`).
Its `error.unclaimed_spool` reports only no-follow metadata (absolute path, object type, owner UID, four-digit octal mode) after the lease confirms no claim exists.
Manual removal is safe only after confirming no retained claim, a free draft lock and unchanged metadata; remove only that object, and for a symlink only the link.
The same code from accepted-send reconciliation carries no spool evidence, keeps `partial`/`observe_required` guidance and never recommends deletion or SMTP replay.

Typed transport codes surface verbatim: `smtp_auth_failed`, `smtp_rejected`, `smtp_tls_failed`, `smtp_timeout`, `smtp_transfer_timeout`, `smtp_data_incomplete`, `smtp_source_invalid`, `smtp_submission_unknown`, `imap_connect_failed`, `imap_auth_failed`, `imap_sent_mailbox_not_found`, `imap_ambiguous_mailbox`, `imap_append_failed`, `imap_append_incomplete`, `imap_append_outcome_unknown`, `imap_ambiguous_message_id` and `imap_timeout`.
Lookup traverses wrapped and joined causes, and an outcome-uncertain code stays public even when cleanup failures are joined.
A controlled live send on 2026-09-08 (iCloud to Gmail with BCC, HTML and plain alternatives, reply threading and an attachment) returned `sent` with both evidence booleans, kept the attachment byte-identical and no BCC header, and refused the consumed ref with `not_found`; this is dated evidence, not a test of the current source.
Never infer recipient failure from an empty local search right after SMTP acceptance; verify the recipient side with the exact Message-ID once available.

## Visible handoff

`drafts handoff` opens a visible new-message compose window through the macOS Compose Email sharing service, linked into the binary without Apple Events, a helper executable or UI coordinates, and never sends.
It requires Mail.app as the current `mailto:` application and supports new drafts with To recipients only; explicit From, CC, BCC, reply and forward semantics are rejected because AppKit exposes one role-less recipient array.
Every attachment is rechecked against its recorded size and SHA-256 and copied into a mode-0700 private staging directory with one subdirectory per attachment and mode-0400 files; only those snapshots reach the sharing service.
Same-size replacements, symlinks, deletions, inode changes and fingerprint mismatches fail closed with `handoff_attachment_missing`, `handoff_attachment_unreadable` or `handoff_attachment_changed` before dispatch.
The command waits for the sharing-service delegate instead of treating invocation as success; the dispatch phase has a fixed 10-second deadline.

`data.draft_handoff.outcome` is `handed_off`, `not_handed_off` or `unknown`.
`handed_off` proves only native delegate acceptance, never saving, sending, delivery or window closure.
Before native dispatch, cancellation returns error code `handoff_canceled_before_dispatch` with public outcome `not_handed_off` and cleans the attempt and staged evidence.
After dispatch, cancellation, timeout, an untyped failure or an unparseable native result returns `handoff_outcome_unknown`, suppresses a late success, keeps the attempt ID and snapshots, and blocks retry with `handoff_retry_blocked`.
Inspect Mail.app and run `drafts handoff-reconcile --ref DRAFT_REF --attempt ATTEMPT_ID --outcome opened|failed --confirm --json` to record the observed outcome and remove the claim and snapshots.
The attempt lifecycle uses `prepared`, `dispatched`, `outcome_unknown`, `confirmed_opened` and `confirmed_failed`; the confirmed states describe only the sharing-service delegate result.
AppKit has no supported compose-window close operation, so MailCLI never claims a window was closed.
Cleanup failures return `handoff_attachment_cleanup_failed` or `handoff_claim_cleanup_failed`.

Retained attempts appear as `handoff_attempt` recovery metadata in every draft detail view, field selection, output-size fallback and export response: attempt ID, timestamps, outcome, dispatch state, snapshot retention, count and byte total, without names, paths or contents.
A prepared retry rechecks the exact attempt ID and requires `prepared` state with dispatch not started before cleaning staging.
A 2026-09-07 Mail 16 probe opened the expected visible compose without sending and left no residual processes.

## Mailbox mutations and batch

Mutations (`messages mark`, `messages move`, `messages copy`, `messages delete`) execute over IMAP directly and return typed server-truth evidence; they never touch the Apple Events gate.
They use store-bound identity with the locally verified RFC Message-ID when present, and server writes do not update Mail's local index until it syncs.
Mutation identity resolution loads the referenced active account with the same bounded Sent-history evidence as account listing, falls back to the complete catalog for inactive-account errors, shares one binding snapshot per invocation and reloads credentials per attempt.
Resolution errors are `account_reference_corrupt`, `account_reference_version_unsupported`, mixed `account_reference_invalid`, `account_disabled`, `account_identity_missing` and `account_degraded`; messages expose only short SHA-256 fingerprints of invalid references.

### Mark

`messages.mark` keeps `data.message_state.server_truth` on success and post-dispatch errors: operation ID, UID, mailbox, UIDVALIDITY, `flags_source` (`STORE` or `FETCH`), `flags_state` (`observed`, `missing` or `unverified`) and nonempty `actual_flags`.
Observed empty flags are a confirmed empty set, and summary booleans derive from that observation; missing or unverified state keeps labeled local cached booleans.
`UID STORE` responses are consumed through tagged completion by UID attribute; each performed phase is verified, and insufficient proof triggers one targeted `UID FETCH <uid> (UID FLAGS)` on the same exclusive session, because tagged OK alone ignores nonexistent UIDs ([RFC 3501 section 6.4.8](https://www.rfc-editor.org/rfc/rfc3501.html#section-6.4.8)).
Each response sequence is bounded to 1,024 logical responses and 4 MiB; a UIDVALIDITY change after dispatch invalidates the evidence without replaying STORE.
Contradictory flags return `imap_flags_mismatch`, a vanished target `imap_message_not_found`, and unavailable verification `imap_flags_outcome_unknown`, all `ok:false` with exit `1` and no blind replay.

`--junk true` removes `$NotJunk` and adds `$Junk`; `--junk false` does the reverse.
A preflight FLAGS read drops no-op changes and checks every needed change against the mailbox's PERMANENTFLAGS before any write; removals precede additions and unrelated flags are kept.
`\*` permits keywords but never unlisted system flags, and missing PERMANENTFLAGS permits permanent changes ([RFC 9051 section 6.3.2](https://www.rfc-editor.org/rfc/rfc9051#section-6.3.2)); a verified no-op succeeds without STORE.
Both standardized junk keywords present means no classification and `junk:false` ([RFC 9051 section 2.3.2](https://www.rfc-editor.org/rfc/rfc9051#section-2.3.2)); unprefixed `Junk` and `NotJunk` are deprecated ([IANA keyword registrations](https://www.iana.org/assignments/imap-keywords/imap-keywords.xhtml)) and kept as unrelated keywords.
Unsupported required changes return `imap_flags_unsupported` with the observation and no STORE; a rejected later phase returns `imap_flags_partial` with `outcome:partial`, and a permission withdrawal during a STORE can leave `outcome:unknown`.
Guidance reports `effect_certainty:none` before STORE or after a first definitive rejection, `partial` after a verified phase, and `unknown` when the outcome is unavailable; every failure requires inspecting state and permissions before another mutation.
A successful observation describes the state at completion, not a guarantee against later changes by other clients.

### Copy, move and delete

Transfers supplement a verified local UID with the Message-ID from the bounded local header block even when the reference omits it. If that source is unavailable, a supported bounded IMAP header read can supply it without fetching the body. Flag-only mutations retain their direct UID path. A genuinely absent Message-ID still cannot guard COPY or the MOVE fallback; native MOVE without one continues to require verified COPYUID evidence.

COPY records a deterministic operation identity before dispatch with the source account, source and destination UIDVALIDITY and UID, and any strict single-UID `COPYUID` mapping.
The destination is searched by exact Message-ID on the mutation session while the account's cross-process mutation lock is held. MailCLI peers serialize this check and dispatch; a verified matching destination prevents another COPY and returns `imap_copy_outcome_unknown` with the destination UID, and a failed check also stops dispatch.
A source without a Message-ID cannot be checked and is refused; with `MAILCLI_IMAP_MUTATION_LOCK=off` only process-local ordering remains.
A lost or incomplete final response returns `imap_copy_outcome_unknown` with the evidence so far and `replay_allowed:false`; observe the destination by exact Message-ID instead of repeating the write. Unresolved `copyAttempts` live only in one Client's memory; a subsequent process cannot inherit them. Destination observation can be delayed, unavailable or ambiguous, and an absent immediate match does not prove no COPY occurred. The destination guard provides no cross-process exactly-once guarantee.
Before a COPY or MOVE is written, validation, cancellation or deadline failures keep `not_started` evidence with the source UIDVALIDITY and stop without destination reconciliation.
Proven no-effect cancellation and transient failures permit safe replay (`retryability:safe`, `replay_allowed:true`, `recovery.action:retry`); validation and configuration failures require correction (`user_input_required`, `recovery.action:correct`).
A partial command write or missing final response stays `unknown`, and retained partial effects always forbid safe replay.

MOVE and DELETE keep source observation, exact Message-ID destination guard, dispatch and verification inside the existing account mutation lock. An existing or ambiguous destination stops without transferring or deleting the source; the lock orders MailCLI peers, not unrelated mail clients.
Authenticated capabilities choose native MOVE or the COPY fallback; capabilities are validated once per session, and a greeting alone never establishes support. Unknown or rejected discovery stops without mutation; NO/BAD from supported MOVE never authorizes COPY.
The source UID must exist in the selected UIDVALIDITY before dispatch. Native completion requires source absence plus a verified destination UID/generation from strict COPYUID or one exact independent Message-ID; tagged OK alone is insufficient. Generation-bound native operations without Message-ID require usable COPYUID and verified effects. Rejected or unprovable outcomes return `imap_move_outcome_unknown` with retained mapping and proven effects, without replay.
Fallback requires an independent Message-ID and checks every needed source deletion flag before COPY; unsupported changes stop with zero COPY, and an already-set flag needs no unsupported STORE. Missing COPYUID requires a unique destination observation before source STORE. Successful UID EXPUNGE must also prove source absence.
The fallback records `source_flag` only after bounded STORE/FETCH observation proves `\Deleted` for the selected source UID and UIDVALIDITY, and dispatches `UID EXPUNGE` only after that proof; without UID EXPUNGE it defers cleanup and counts foreign deleted UIDs without a plain EXPUNGE.
`server_truth.expunge_branch`, `foreign_deleted_count` and `completed_effects` keep the proven MOVE phases (`copy`, `source_flag`, `uid_expunge`, `cleanup_deferred`).
A later flag failure returns `imap_move_outcome_unknown` with partial COPY evidence and actual flag state under `data.message_state` or `data.delete_result`; neither COPY nor STORE is replayed automatically.
`messages delete` moves to the account's Trash after `--confirm` and returns `delete_result`; a message already in Trash returns `message_already_trashed`.
Permanent irreversible deletion is not offered.

### Batch

`mailcli batch --input FILE|-` accepts one JSON object with an `operation` (`read`, `attachment_save`, `mark`, `move`, `copy` or `delete`) and an ordered `items` array.
Each item has a unique trimmed `id` and a store-bound `ref`; attachment saves also need `attachment_id` and an absolute `output_path`, marks at least one of `read`, `flagged` or `junk`, and moves and copies a `mailbox` ref.
`move` and `delete` items accept `allow_draft_mutation`; `copy` rejects it.
A `delete` batch requires the invocation's `--confirm`, which every other operation rejects, and `--concurrency` overrides the request's worker limit.
The request is capped at 16 MiB, 100 items, and eight workers (default two).
Capabilities publish these bounds as `limits.batch_operations`, `default_batch_concurrency`, `maximum_batch_concurrency`, `maximum_batch_items` and `maximum_batch_input_bytes`.
Before dispatch, every item, duplicate ID, attachment destination and operation-specific ref is validated: mark, move and delete need unique sources compared by decoded message identity (account, store UUID, store mailbox ID and message or library ID), copy may repeat a source only across distinct destinations compared by account and NFC-normalized path, and conflict errors name item IDs without ref values.
Items then run on the invocation's single store and transport graph through the same single-operation paths.

Results keep input order under `data.batch_result`; each item has `state` (`completed`, `failed`, `skipped`, `skipped_budget` or `uncertain`) and, when applicable, `error.code`, `error.message`, `retryable` and the same guidance, and mutation items carry the same `message_state` or `delete_result` evidence as single commands.
`batch_canceled` marks an item that never started. An active canceled mutation is `failed` when transport evidence proves `not_started` or `rejected` with no completed effects; missing, partial or unknown dispatch evidence remains `uncertain` and must not be replayed. Mutation aliases use the same effect guidance as their single-command counterparts.
There is no automatic retry: submit again only failed items whose error says `retryable:true`, never successful or uncertain mutations.
A failed attachment save keeps verified `saved_attachment` evidence, and its `retryable` value follows `error.guidance.replay_allowed`.

Read items default to the `metadata` projection like `messages get`; request-level `defaults.view` or `defaults.fields` is valid only for `operation:"read"`, item-level selectors override them, and `view` and `fields` are mutually exclusive at each level.
Concurrent reads account for requested bodies and headers before retaining results, and completed items pass exact projected-envelope admission.
Read overflow stops new reads, cancels and joins in-flight reads, keeps completed outcomes and marks never-started items `skipped_budget`.
It returns one bounded `output_too_large` envelope with at most the first 10 item IDs and states plus full counts, proves `effect_certainty:none`, and sets `retryability:user_input_required` and `replay_allowed:true` so the caller can narrow `view` or `fields` or raise `--max-bytes` and replay the same read batch.
Mutation batches do not use this cancellation policy; their overflow keeps effect-aware, non-replayable guidance and completed-effect evidence.

## Accounts and bindings

### Account catalog

The account catalog separates the transport `type`, stable `display_name`, `discovered_sender_identities` and `configured_sender_aliases`; `email_addresses` remains their compatibility union.
Account identity resolution is local-store-only.
Sender identities come from the newest bounded Sent-history window, defaulting to 2,000 messages; supported configuration can raise that default of 2,000 only up to a maximum of 10,000.
Each account carries `identity_coverage` with `source`, `state`, `observed_messages`, `limit` and `more_available`.
States are `complete`, `bounded` (usable identities, more Sent history outside the bound), `not_observed` (no sender inside the bound), `no_valid_sender` (history exhausted without a valid sender), `no_sent_mailbox`, `unavailable`, `not_applicable`, and `configured` with `source:"account_binding"` for a bound account without Sent history.
The capability manifest publishes this bound: the default limit is 2,000 and the hard 10,000 maximum applies.

Account listing may return a partial catalog with `data.complete:false`; `data.identity_coverage_complete:false` separately reports bounded or unavailable sender history.
Account-local cache, special-use or sender-data failures keep the account listed with `state:"degraded"`, a stable `degraded_reason` and `degraded_remediation`; unreadable or malformed mailbox caches use `degraded_reason:"mailbox_cache_unreadable"`, while parser-level `mailbox_cache_malformed` stays an internal diagnostic.
A mutation targeting a degraded account returns `account_degraded`, while a global SQL or schema failure returns `account_catalog_incomplete` with `mailcli doctor` remediation instead of invoking Apple Events.
A disabled account returns `account_disabled`; an account without a provable credential-backed sender returns `account_identity_missing`.
`accounts list` reports per account `direct_ops_supported` and `direct_ops_reason` (`provider_supported`, `binding_hosts` or `unsupported_provider`) from the same endpoint resolution that sends and mutations use; the manifest publishes the vocabulary under `limits.direct_ops_support_reasons`.

### Bindings and send setup

Explicit bindings live in a private versioned JSON file in the MailCLI application-support directory and hold only the stable account ID, permitted sender aliases, the credential lookup identity and optional explicit SMTP and IMAP host-port endpoints; passwords stay in the Keychain.

Configure a supported account with `mailcli send setup --account ACCOUNT_REF --from ALIAS [--credential-account LOGIN]`.
The command checks that all aliases and the credential identity resolve to the same supported provider, stores the password under the credential identity, and atomically upserts the alias binding.
Accounts on other domains add `--smtp-host HOST --smtp-port PORT --imap-host HOST --imap-port PORT` together with `--account`.
Each host-port pair must be complete, ports must be in 1–65535, and hosts must be fully qualified public DNS names or public IP literals; loopback, private, link-local, multicast, unspecified addresses and reserved names (`localhost`, `.localhost`, `.local`, `.internal`, `.home.arpa`, `.lan`, `.corp`) fail with `account_binding_host_invalid` before the password prompt.
Explicit endpoints skip the provider check per leg: a fully explicit binding covers both SMTP and IMAP, while a single explicit leg falls back to the provider table for the other.
Rerunning setup for a binding keeps its endpoints and credential identity unless host flags or `--credential-account` are supplied again; any host flag replaces all four fields together, and host flags without `--account` are rejected.
`--remove` deletes the selected Keychain credential and keeps the binding for reconfiguration.
Before prompting or storing a password, setup validates the complete planned binding against the observed document, including provider-family, alias and binding-count limits. The final locked merge still checks concurrent changes. Password input is one line bounded to 4096 bytes including CRLF; EOF with data is allowed, other read failures abort without exposing or storing partial input. SIGINT/SIGTERM and caller cancellation stop input waits and prevent new Keychain writes; terminal settings are restored on controlled exits and restoration failures are reported. Native Keychain calls keep their synchronous behavior once started.
Send-time validation skips the Sent-history scan for bound accounts because permitted senders come from the configured aliases.
An alias shared by several bindings returns `account_binding_ambiguous` until an explicit account ref is supplied; a binding whose account is no longer enabled returns `account_binding_stale`.

Updates serialize read, merge and atomic publication under cross-process locks in stable order: the legacy binding lock first, then `account-bindings.lock` for the standard `account-bindings.json` (custom basenames use `account-bindings-<full lowercase SHA256 of basename>.lock`).
Lock inodes are never unlinked; contention returns `account_binding_busy` after two seconds or earlier on cancellation.
Publication uses a pinned parent directory, verifies the written file after rename and directory sync, and sweeps only its own basename-hashed temporaries older than ten minutes; ordinary reads never repair permissions.
Credential caches are invalidated right after a successful Keychain write.
A later failure returns `data.partial_effects` in order: `keychain_store:complete`, then `binding_publish:none|unknown|complete`, then `lock_release:failed` when a lock release fails.
`none` means the rename did not happen, `unknown` that the rename completed but directory sync or identity verification failed, and `complete` that publication succeeded even if a lock release failed later.
The locked merge compares implicit credential and endpoint fields with the pre-prompt snapshot; divergence returns `account_binding_changed` with `binding_publish:none` and no replay.
Recovery is read-only `mailcli accounts list --json` followed by an explicit setup decision; MailCLI never retries or rolls back the Keychain credential automatically.

## Mail.app integration

Mail.app is involved only in `sync` without `--check`, `doctor --live`, targeted fallback listing when the store cannot open, and visible handoff.
Every Apple Events caller acquires one BSD advisory lock at `~/Library/Application Support/MailCLI/mail-access.lock`; acquisition is capped at two seconds and concurrent callers return `mail_busy` before contacting Mail.
The gate requires an already-running Mail process with bundle identity `com.apple.mail` and binds the bridge to that exact PID.
MailCLI never starts, activates, quits, kills or restarts Mail.
Before a potentially mutating Apple Event, the gate writes and synchronizes an exact-PID recovery marker under the lock; a failed write prevents the event, a definite completion clears it, and an incomplete call leaves it so later live operations fail with `mail_recovery_required` until that Mail process has been replaced.
Each bridge call owns one private `osascript` process group, terminates remaining members and verifies group absence before releasing the gate; large requests pass through a private temporary file rather than process arguments.
SIGINT, SIGTERM and cancellation write a private bridge marker that the script checks before mutation; a retained compose backend returns `compose_cleanup_failed`, and only a bridge exceeding the 15-second cleanup grace is force-stopped.
Incomplete reads, live probes, sync triggers and Automation denial never create false recovery state.

Gate state is opened relative to pinned no-follow directory descriptors; the lock must be a current-user-owned regular file with one link.
Symlinks, hardlinks, foreign owners, non-regular files or identity replacements fail before dispatch with `mail_access_gate_unsafe`, which requires inspection and a verified recovery plan.
A corrupt marker stays untouched while Mail runs and returns `mail_access_gate_corrupt` (`mail_busy` when process lookup times out); quit Mail.app and retry, and only a verified stopped-process check lets MailCLI clear the file contents and return `mail_not_running`.
Never remove or replace `mail-access.lock`.

The bridge resolves accounts at most once per invocation, validates the fallback listing page limit of `1..25` before contacting Mail, and resolves one account, mailbox path and message ID per direct operation; Mail `whose` queries and mailbox-wide `messages()` reads are forbidden.
The fallback gateway is read-only (account listing, message listing, probing and sync).
Production clients reject scripted draft save and outbound attachment insertion with `compose_automation_unsupported` before acquiring the gate.
`sync` without `--check` triggers Mail's refresh; it does not prove completion.
`doctor` checks the read store without Apple Events; `doctor --live` verifies the Mail process identity and asks for the Mail version through one read-only Apple Event and never creates, saves or sends a message.
`doctor --diagnostics` reports only named phase durations in milliseconds, without message content, addresses, attachment data or paths.

## Security

MailCLI reuses Mail.app's configured accounts and local files; complete local reads use no provider credentials.
Direct SMTP and IMAP use separately provisioned app-specific credentials; MailCLI never extracts Mail.app's passwords, OAuth tokens or cookies.
The one stored secret is the per-account app-specific password that `mailcli send setup` writes to the macOS Keychain (`mailcli-smtp` service) at a no-echo prompt; it never appears in output, logs, chat, argv or state files and goes only to the provider's SMTP and IMAP endpoints.
Keychain bridge buffers are zeroed before release and the IMAP LOGIN command is wiped after writing; platform boundaries (a Go string password, CoreFoundation-owned `CFData`, garbage-collected copies) mean wiping bounds exposure time rather than guaranteeing erasure.
Account identifiers with an embedded NUL byte fail with `keychain_invalid_identifier` before any Keychain call.
Targeted IMAP, discovery and sync-check resolution validate endpoints before credential access and carry the selected password directly into the configuration, with one Load for a successful identity. Only a missing Keychain item or empty stored password permits another eligible alias; exhausted/missing credentials use `imap_credentials_missing` and `send setup` guidance. Other Keychain read failures retain their typed cause and stop alias/network work; unlock or permit Keychain access for `keychain_load_failed` instead of replacing the password. Direct send, delivery and Sent reconciliation likewise preserve credential read failures; only actual missing/empty passwords use `smtp_credentials_missing`. Reconciliation retains its existing send claim and never resubmits SMTP after a credential failure. Credentials are not cached by this resolution path.

Keychain items carry no per-binary access control: `kSecAttrAccessControl` needs entitlements an ad-hoc signed binary lacks (`errSecMissingEntitlement`), partition-list controls are iOS-only, and the deprecated `SecAccess` API cannot express a stronger identity.
As measured, an item created by the ad-hoc signed CLI is readable without a prompt by other unsigned same-user processes, while Apple-signed tools trigger the consent dialog.
The residual risk is that a same-user process can read the app-specific passwords; effective mitigations are the credential's narrow, revocable scope, optional Keychain auto-lock, and removal through `send setup --remove`.
A per-binary ACL becomes feasible only with a Developer-ID signing identity with Keychain entitlements.

MailCLI persists only review drafts, historical send and save claims, terminal send receipts, accepted-message recovery spools, account bindings and cross-process locks under `~/Library/Application Support/MailCLI`, plus the IMAP excerpt cache under `~/Library/Caches/MailCLI/excerpts`; it persists no mail corpus or search index.
State directories use mode `0700`; drafts, claims, receipts and spools use mode `0600`.
To remove all local state, delete `~/Library/Application Support/MailCLI` (its `drafts` folder holds unsent review drafts) and `~/Library/Caches/MailCLI/excerpts` (safe at any time), remove each stored password with `mailcli send setup --account ACCOUNT_REF --from ADDRESS --remove`, and delete the binary, the skill directory and any skill link.
Structured output excludes body content unless requested, and diagnostics avoid subjects, bodies, headers, recipient lists and attachment bytes unless needed to identify the failed operation.
Received attachment and content exports require new absolute destinations and refuse overwrites and unsafe path substitution.
Mutations require explicit intent; send and destructive operations require confirmation and reviewed-state checks; handoff opens UI without sending.
HTML keeps a bounded semantic allowlist, removes active and remote resources and reports value-free diagnostics (see [Rich content](#rich-content)).

## Platform and compatibility

The supported product target is macOS on Apple silicon (`darwin/arm64`).
The verified development host is macOS 15.6.1 with Mail 16.0 build 3826.700.81 and the exact Go version pinned in `go.mod`.
SMTP and IMAP are independent of macOS, but there is no Linux or Windows credential backend, account configuration, message-discovery adapter or installer.
The Keychain requires Darwin with CGO; other builds return `keychain_unsupported` for credential operations.

| Operation | Mail.app process required | Remaining platform/data dependency |
| --- | --- | --- |
| List/search and complete local message or attachment reads | No | Supported Apple Mail Envelope Index, .emlx sources and Full Disk Access; results reflect the local store |
| New-message discovery in local list/search | No | Mail.app must update its local store; complete local coverage does not certify server freshness |
| `messages new` server view | No | Keychain credentials and IMAP; compares the newest 100 server headers without updating the local store |
| Local drafts and direct SMTP send/Sent reconciliation | No | Local draft state plus macOS Keychain for direct SMTP/IMAP credentials; no Mail-store startup for the direct path |
| IMAP mark/move/copy/delete and sync --check | No | Keychain credentials and store-bound account/message identity or local counts; server writes do not update the local read index |
| Targeted IMAP hydration | No | A locally resolved message reference and Keychain credentials; fetches missing content rather than discovering an unindexed inbox |
| sync without --check, doctor --live, supported read fallback | Yes | Exact already-running Mail process and Automation permission; sync triggers refresh, it does not prove completion |
| Visible new-compose handoff | Native Mail UI | AppKit and Mail.app as the default email application; acceptance never proves sending, saving or closing |

If Mail.app is closed, complete downloaded content stays readable, but MailCLI does not advance the local index itself.
A new macOS or Mail release may need an adapter update; unsupported store versions fail closed instead of guessing.

### Providers and authentication

Direct SMTP and IMAP support Gmail (`gmail.com`, `googlemail.com`) and iCloud (`icloud.com`, `me.com`, `mac.com`) by default; other domains need an account binding with explicit validated endpoints.
The capability manifest reports `supported_providers` and `unsupported_provider_code:"transport_unsupported_provider"`, and a new provider needs one entry in the closed exact-domain table so capabilities, help and validation change together.
MailCLI implements the app-specific-password path with SMTP `AUTH PLAIN` and IMAP `LOGIN`; it does not implement OAuth/XOAUTH2 or provider account authorization.

Provider policy is distinct from that path.
Google ended username-and-password-only access from third-party apps for Google Workspace accounts beginning in January 2025, not app passwords for every Google Account; app passwords need 2-Step Verification and may be unavailable for organization-managed or Advanced Protection accounts ([Google less-secure app guidance](https://support.google.com/accounts/answer/6010255), [Google app-password guidance](https://support.google.com/mail/answer/185833)).
Apple documents Apple Account authorization for supported third-party apps and app-specific passwords otherwise ([Apple Account authorization](https://support.apple.com/en-us/121539), [Apple app-specific passwords](https://support.apple.com/en-us/102654)).

### Scope

The implemented surface covers account and mailbox discovery, paginated listing, normalized and streamed raw reading, received-attachment inspection and saving, cross-mailbox search, local plain, Markdown and HTML drafts with preview and editor update, visible new-draft handoff, reply, reply-all, forward, native-draft inspection and adoption, direct SMTP sending with IMAP Sent mirroring, copy, move, delete to Trash, read, flag and junk state, and synchronization.
The capability manifest reports that MailCLI owns no mail index or background process and reports `raw_mime_read:true` and `raw_mime_send:true`.
Out of scope: account creation and login, password management beyond the `send setup` credential, provider APIs beyond the SMTP and IMAP operations above, Mail-store writes, UI-coordinate automation, Mail rules administration, permanent trash emptying and Mail 16 scripted draft export.
The store adapter is strictly read-only; permanent deletion may be added only as a separate explicitly confirmed capability.

## Release and distribution

### Release artifacts

Each release `vX.Y.Z` publishes `mailcli_X.Y.Z_darwin_arm64.tar.gz`, `SHA256SUMS` and `SHA256SUMS.sig`.
The signature covers the exact checksum-manifest bytes with Ed25519; the public key is pinned in source and the updater, and the mode-0600 private key stays outside the repository.
`mailcli-release-sign` opens that key through a pinned parent descriptor, rejects symlinks, replacements, non-regular files and group- or world-readable permissions, and keeps decoded key material only for the signing operation.
The archive contains `bin/mailcli`, the complete `skills/mailcli` package, `install.sh`, `README.md` and `LICENSE`.
The version requested from `build-release.sh`, the embedded CLI version, archive root, Git tag and GitHub release title must agree.

Builds disable Go VCS stamping, use read-only module resolution, keep `-trimpath`, strip symbols and DWARF with `-s -w`, and dead-strip native stubs with `-extldflags=-dead_strip`, so identical source and toolchain produce byte-identical binaries.
Stripping keeps the Go-derived Mach-O `LC_UUID` that macOS 26 needs to launch the binary.
Normal compiler inlining stays enabled for hot paths; the release gate reports `release_binary_bytes` without a cap.
Dynamic SQLite linking, executable packing, more aggressive C optimization and delegating HTTPS to external tools are deliberately excluded because the size gain does not justify weaker diagnostics, host-dependent behaviour or less reliable updates.
The binary is ad-hoc signed but not notarized, because the release host has no Developer ID identity; browser downloads may need an explicit user-approved Gatekeeper step after SHA-256 verification, and quarantine is never cleared automatically.

`build-release.sh` builds and verifies archive, checksum, signature and a signed staging manifest inside mode-0700 staging on the destination filesystem, then publishes each final path by a no-overwrite hard link.
The staging manifest records the full source commit, actual Go version, binary SHA-256 and asset digests; resuming requires the same clean checkout and toolchain, and legacy manifests without source binding are refused.
An interrupted publication keeps the authenticated staging and reports matching, missing and divergent final paths; a rerun verifies staged bytes, byte-compares existing final assets and publishes only missing paths.
The three final paths do not form one filesystem-atomic transaction.
`--discard-stale-staging` rebuilds only verified owner-only staging when no final assets exist; conflicting final assets need a different empty absolute `MAILCLI_RELEASE_DIRECTORY`, and all three final paths are checked before building.

Releases are published only on an explicit owner instruction naming the exact version and action.
Committing, building, packaging into an isolated directory, installing into an isolated home and reading release state never authorize a version change, push, tag, GitHub release or asset upload.
The repository has no publication script or release workflow.

### Installer

The first-install bootstrap in the README needs only macOS system tools. It trusts GitHub HTTPS for the script, manifest and archive, and does not independently verify the Ed25519 signature. The `SHA256SUMS` check detects a mismatched download but cannot authenticate the publisher when both files come from the same host. The bootstrap checks the one expected archive entry, safe layout and embedded binary version before running `install.sh`; subsequent `mailcli update` calls verify the signature in Go.

The packaged installer defaults to `~/.local/bin/mailcli` and `~/.agents/skills/mailcli`; `MAILCLI_BINARY_DESTINATION` and `MAILCLI_SKILL_DESTINATION` select other safe absolute paths.
It rejects unsafe parent or destination symlinks and pre-existing backup paths.
Both payloads are staged in their target parents, snapshotted, byte-compared and recorded in a mode-0600 transaction manifest before any live rename.
A restart rolls back an incomplete transaction through snapshot- and identity-checked paths; a committed transaction removes only its own backups after both artifacts verify, and replaced artifacts are preserved.
The installer never executes staged content, never deletes unrelated paths and never changes Full Disk Access, Automation consent, quarantine attributes or Gatekeeper settings.

Self-update, release installation and local source installation share the mode-0600 lock file `~/Library/Application Support/MailCLI/update.lock`.
Every installer holds it from transaction scanning through verification, rollback and cleanup, and a live transaction is never treated as abandoned by age or PID.
Direct installers use the macOS `lockf` interface and wait at most 30 seconds; the updater waits within its command context and passes its owned descriptor to the installer child.
Lock files are never unlinked or stolen; invalid types, symlinks, extra hard links and replaced identities fail closed with recovery evidence kept.
Ctrl-C targets the foreground process group; automation should signal the whole installer group.

`scripts/build/install-local.sh [BINARY_DESTINATION]` builds the checkout and runs the same installer with that checkout as package root, publishing binary and skill together with the same lock, identity checks, rollback and recovery.
The optional argument changes only the binary destination; `MAILCLI_SKILL_DESTINATION` redirects the skill.
Source builds are ad-hoc signed, so macOS asks again for Keychain access after every rebuild. `scripts/utils/create-local-signing-identity.sh` creates, once, a self-signed identity in its own password-less keychain `~/Library/Keychains/mailcli-local-signing.keychain-db` and adds it to the user keychain search list; `install-local.sh` then signs every build with it (`MAILCLI_SIGNING_KEYCHAIN` selects another keychain, empty disables signing), so one "Always Allow" persists across rebuilds.

### Self-update

`mailcli update` queries the public latest-release endpoint, compares strict `MAJOR.MINOR.PATCH` versions, requires the exact `darwin/arm64` archive plus `SHA256SUMS` and `SHA256SUMS.sig`, and verifies the signature before downloading or trusting the archive.
Version components contain only ASCII decimal digits, with no signs or leading zeros except `0`; the optional `v` prefix and surrounding-whitespace normalization remain supported.
Malformed or incomplete release metadata, invalid installed/release versions and oversized resources return terminal `update_package_invalid` rather than an unchanged retry. Transient pre-install metadata/download failures retain `update_check_failed`/`update_download_failed`; sanitized errors preserve cancellation/deadline causes without exposing request URLs or tokens. Caller cancellation stops automatic retry, while an attempted installation still requires observing `version --json`.
Metadata, asset and redirect URLs must use exact trusted GitHub hosts (`api.github.com`, `github.com`, `objects.githubusercontent.com`, `release-assets.githubusercontent.com`), HTTPS on the default port and at most 10 redirects.
Credentials, fragments, malformed URLs, untrusted hosts, alternate ports and HTTP downgrades fail with `update_host_untrusted`, `update_url_invalid`, `update_url_invalid_port`, `update_url_insecure`, `update_redirect_invalid` or `update_redirect_limit`, which never echo rejected URLs, credentials or paths.
The archive is authenticated in memory before the shared lock; after acquiring it, the updater reads the installed version and reports an equal or newer installation without reinstalling, and no network request happens while the lock is held.
Extraction rejects traversal, links, unexpected roots, oversized expansion and excess entries, and validates Mach-O architecture, code signature and embedded version.
`codesign --verify --strict` only proves a kernel-acceptable arm64 signature; byte authenticity comes from the Ed25519 and `SHA256SUMS` chain.
The installer pins the binary's SHA-256 and the skill tree digest once and compares every staged and installed artifact against them.
Its subprocess strips shell startup and function-injection variables and ambient `MAILCLI_INSTALL_PACKAGE_ROOT`, runs in a private process group, and on cancellation receives `SIGTERM` with a five-second grace before descendants are force-cleaned.
Interactive human output shows a compact MailCLI heading, indented progress and a version outcome with restrained status colors. `NO_COLOR` disables color; `TERM=dumb` disables animation and styling. Redirected text retains plain progress/outcome lines; `--json` emits exactly one envelope without presentation text.

After an installation attempt, errors keep `data.update_result` with `binary_path`, `latest_version`, `updated` and `failed_phase` (`installer`, `installed_binary_verification`, `package_cleanup`, `update_lock_validation` or `update_lock_close`).
`updated:true` and `effect_certainty:"complete"` require a verified installed version; an installer or verification error without it reports `unknown`, while cleanup or lock errors after verification report `complete`.
Recovery inspects `mailcli version --json` and never reruns the installer or assumes rollback.

Installer, updater, preflight and release-state version probes set `MAILCLI_OUTPUT=human` locally, so plain `mailcli MAJOR.MINOR.PATCH` checks work across old and new binaries.
To upgrade through an older updater or installer, run `MAILCLI_OUTPUT=human mailcli update` or `MAILCLI_OUTPUT=human /absolute/path/install.sh`; an old updater without that setting may reject the new piped JSON version output.

A shared version string does not establish artifact identity: HEAD, `origin/main`, local `dist/`, the checkout binary, the installed binary and both skills can differ.
`scripts/tests/report-release-state.sh` compares the source version, HEAD, local and optional origin tag, current-version `dist/` assets, checkout and installed binary, installed skill and optional GitHub release independently and read-only; `--strict` makes drift fail and `--remote-required` includes the live GitHub release.

## Architecture

### Package boundaries

1. `cmd/mailcli` owns process startup, resource finalization and process-level output routing.
   It creates one invocation-owned direct transport graph, injects it into the mail service and local store client, and closes the IMAP pool before the local store.
2. `internal/cli` owns command parsing, validation, output selection, exit codes and confirmation policy.
3. `internal/mail` owns typed use cases, filters, drafts, send claims, outcome semantics, and local filesystem I/O for drafts, claims, attachments, account bindings and advisory locks.
   Mail-store access and SMTP/IMAP operations go through the injected `Gateway` and `SendTransport` dependencies, whose adapters stay in `internal/mailstore` and `internal/transport`.
   It consumes the failure-classification predicates in `internal/transport/classification.go` and the transport-neutral `ServerMutationOutcome` vocabulary on `ServerMutationEvidence` instead of comparing transport codes; `internal/mailstore` maps wire results to that vocabulary.
4. `internal/mailstore` owns the zero-Apple-Events read path: read-only Envelope Index access, safe mailbox mapping, `.emlx` parsing, attachment extraction, on-demand search, reference revalidation and store-based mutation observation.
5. `internal/mailapp` owns the optional Mail.app integration: `doctor --live` and Mail's local sync; its fallback gateway is read-only.
6. `internal/compose` owns the visible AppKit handoff without Apple Events or sending.
7. `internal/transport` owns provider endpoint resolution, SMTP submission with STARTTLS, IMAP Sent mirroring, IMAP mutations (STORE, COPY, MOVE with COPY plus EXPUNGE fallback, delete to Trash), bounded hydration, STATUS checks and LIST discovery.
8. `internal/mailref` owns opaque store-bound references and cursors; `internal/keychain` stores the per-account password under the `mailcli-smtp` service.

Within `internal/mail`, draft use cases own service-level create, read, update, discard and handoff operations.
Their supporting responsibilities are bounded reference-ordered pagination and directory revision checks; streaming metadata reads and skipped-string validation; pure input, content, address, and resource validation; bounded JSON state files and atomic publication; references, leases, and mutation cleanup; send/save claim encoding, validation, and transitions; attachment fingerprints and snapshot checks; envelope and Sent-message identity fingerprints; replayable MIME spools and transport adapters; direct SMTP/IMAP delivery helpers and recipient normalization; send and reconciliation orchestration; retained historical native-save reconciliation; stale-draft, orphan-lock, and orphan-claim/spool/snapshot cleanup; and shared cancellation and lock-wait classification.
Service entrypoints depend on these helpers, and those helpers do not call back into CLI, store, or one another through new package cycles; each responsibility has one implementation.

Main libraries are `github.com/emersion/go-message` for streaming MIME, Goldmark for Markdown, `golang.org/x/net/html` for sanitization and text extraction, and `github.com/mattn/go-sqlite3` for one strict read-only connection.
No service, daemon, process-global cache, watcher, child process or goroutine survives an invocation; `osascript`, the update installer and an external editor each run in a private process group that is reaped and checked, and a process-start failure never becomes a send claim.

### IMAP transport

IMAP sessions use a bounded pool keyed by exact host, port and username: the CLI default is two authenticated TLS connections per account, while `imapclient.NewWithOptions` accepts one through eight; the CLI has no flag to change it.
Reads may overlap on separate sessions; APPEND and every mutation take the full account gate and exclude reads and each other.
Repeated selected-state reads can reuse SELECT on an idle session, while mutations and FETCH re-SELECT and verify UIDVALIDITY; I/O failures discard the session, and waits observe cancellation.
`Close` makes pending waiters return `imap_timeout`, waits for owned operations, then logs out and closes every session.
Credential changes use `InvalidateCredentials`: idle sessions close, in-flight operations drain, and the next operation authenticates with current credentials; passwords never enter session identity, logs or state.
`imapclient.OperationContracts` is the package source of truth, and `mailcli capabilities --json` publishes the same contract.

| IMAP operation | Class | Concurrency | Session ownership | Mailbox selection | UIDVALIDITY |
|---|---|---|---|---|---|
| LIST | read | shared_account | pooled | none | not_used |
| STATUS | status | shared_account | pooled | none | observed |
| SEARCH | read | shared_account | pooled | reuse_or_select | observed |
| FETCH | fetch | shared_account | pooled | fresh_select | required_match |
| APPEND | mutation | exclusive_account | dedicated | fresh_select | not_used |
| STORE, COPY, MOVE, DELETE | mutation | exclusive_account | pooled | fresh_select | required_match |
| CLOSE | close | exclusive_client | all_pooled | none | not_used |

The CLI requires a cross-process mutation lock per credential-free host, port and username identity (`imap-mutations-<hash>.lock` in the MailCLI state directory).
A mutation waits up to 30 seconds under contention and reports `imap_account_busy` (transient, no effect, retry allowed) only when that bound expires.
Directory, lock-client or lock-file setup failures return `imap_mutation_lock_unavailable` before dispatch with `phase:validation`, `retryability:user_input_required` and `recovery.action:correct`, naming the directory when known; read-only commands stay usable.
`drafts send` preflights lock setup before SMTP with one nonblocking attempt that counts a current holder as valid and reserves nothing.
Reads and hydration never take the mutation lock.
`MAILCLI_IMAP_MUTATION_LOCK=off` (or `0`, `false`, `no`) is the explicit process-local opt-out; library callers opt in through `imapclient.ClientOptions.MutationLockDir`, and platforms without advisory `flock` return the same unavailable error.

Tagged `NO` or `BAD` replies from LOGIN, LIST, SEARCH, UID SEARCH, STATUS, SELECT and APPEND expose `error.imap_rejection` with the command, status, leading response-code atom and the server text (512 UTF-8 bytes or less after control-character removal); response codes later in prose are not interpreted.
`imap_command_rejected` is the default; LOGIN `NO` keeps `imap_auth_failed`, and only `NO [NONEXISTENT]` on a target-mailbox command and `NO [OVERQUOTA]` on APPEND map to `imap_mailbox_not_found` and `imap_quota_exceeded`.
Only `NO [UNAVAILABLE]` permits retry for reads, and `BAD` is never automatically retryable.
Cancellation, timeout, connection loss and malformed tagged status use `imap_canceled`, `imap_timeout`, `imap_disconnected` and `imap_response_malformed`; reads may retry these, while a possibly dispatched mutation stays observation-required.
Dial, pool and command failures preserve both the I/O cause and the caller's cancellation/deadline sentinel. A canceled caller with proven no effect receives `next.do:stop`; an unknown write outcome still requires observation before replay. Client Close interrupts pending acquisition as cancellation, not timeout.
APPEND consumes 100 untagged responses or fewer before continuation within a cumulative 64 KiB wire budget; exceeding either returns `imap_resource_limit_exceeded` without sending message data.
Generic tagged-completion readers share the flag-response limits: at most 1,024 physical lines and 4 MiB including ignored responses and CRLF. Exhaustion returns `imap_resource_limit_exceeded` and discards the session; retained COPYUID evidence stays bounded and comes only from a leading response code on an untagged status or the matching completion tag, never prose or FETCH text. An invalid tagged status is rejected before completion is reported; APPEND after its terminator and dispatched COPY/MOVE without a valid final reply remain outcome-unknown.
Local command-line validation writes no bytes; interrupted I/O discards the session.

### Timeouts and budgets

| Operation | Budget |
| --- | --- |
| Local message, raw-source and attachment reads | 60-second read budget, plus a separate 60 seconds for reference resolution |
| IMAP hydration | 30-second setup budget plus the transfer budget; at the 64 MiB raw-source bound and a 1 MiB/s floor the maximum computes to a 94-second FETCH budget, 124 seconds with setup |
| CLI hydration window | The outer CLI hydration command window is 254 seconds: 60 local read, 60 resolution, 124 hydration and a 10-second parse margin |
| SMTP and IMAP commands | Short protocol commands and final replies use a 30-second budget |
| SMTP DATA and IMAP APPEND | 30 seconds plus one second per MiB at a 1 MiB/s throughput floor, capped at 15 minutes |
| Draft commands | 15 minutes for `drafts send` and `drafts reconcile`, two minutes for `drafts prune`, 15 seconds for `drafts edit` and `drafts discard` |
| Draft update and handoff staging | 30 seconds plus one second per attachment MiB at the same floor, with the same 15-minute cap |
| Handoff dispatch | fixed 10-second deadline |
| Draft lock | two-second BSD `flock` per command and per prune candidate |
| Mail.app access gate | acquisition capped at two seconds; 15-second cleanup grace before force-stop |
| IMAP mutation lock | waits up to 30 seconds |
| Installer lock | 30 seconds for direct installers |
| Account-binding lock | two seconds |
| Body search | 60-second command deadline |
| Caches | mailbox catalog and IMAP LIST results for five minutes |

Caller cancellation and earlier parent deadlines always take precedence, and timeout and cancellation errors state that no external mutation was attempted where that is proven.

## Development

Build with `./scripts/build/build.sh`; it writes `bin/mailcli` unless `MAILCLI_BUILD_OUTPUT` names another destination.
`./scripts/tests/test.sh` checks the exact staged change: changed-source formatting and shell syntax, configured lint, changed Go packages and their transitive reverse dependencies, applicable documentation contracts, and registered shell checks; it omits vulnerability scanning.
`./scripts/tests/test.sh --full` is the integrated non-live suite: shell script checks, `gofmt`, module verification, one configured `golangci-lint` pass (errcheck, ineffassign, staticcheck and unused; vet runs in no standard flow), blocking `govulncheck`, coverage tests, forbidden-path architecture checks, commit and release authority checks, and isolated release, source-installation and skill-validation tests.
`--push-check` validates a matching full proof without publishing.
It runs four Go packages concurrently with `GOMAXPROCS=4` and runs the shell regressions in one ordered lane beside the Go checks; `MAILCLI_TEST_CPUS` and `MAILCLI_TEST_PACKAGES` accept positive-integer overrides.
No gate, CI run or full proof uses the race detector; run `scripts/tests/run-race-tests.sh [PACKAGE...]` manually when a race check is wanted.
Install the tools with `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2` and `go install golang.org/x/vuln/cmd/govulncheck@v1.7.0`; binaries in `$(go env GOPATH)/bin` are accepted.
`go.mod` is the version source of truth: entry points select its exact version with invocation-scoped `GOTOOLCHAIN`, check `GOVERSION` and reject conflicting overrides.
Vulnerability findings fail, and offline or unavailable scanning is incomplete, never a successful full proof.

Live stages run only with `--full-checks` and explicit flags: `MAILCLI_LIVE_TESTS=1` (live Mail-store tests), `MAILCLI_KEYCHAIN_LIVE=1` (live Keychain round trip) and `MAILCLI_LIVE_RESPONSIVENESS=1` (a fresh temporary binary passed as `MAILCLI_BINARY` to `test-live-responsiveness.sh` against the running Mail process with Automation permission); each unset flag prints a skip line. The background shell-regression lane clears these flags without changing the parent's selected live stages.
Ordinary verification contacts no user Keychain, native compose UI, Mail store or installed skill directory, and does not verify live provider authentication, delivery, Mail.app scripting or visible AppKit handoff.
Keychain tests run the real SecItem calls against an isolated temporary keychain, and handoff is tested through a native seam without AppKit.
The documentation contract test pins every shared operational bound (page limits, search caps, byte budgets, timeouts, pool sizes) to one value wherever `README.md`, this document, the skill guides and the implementing scripts or constants state it.

`scripts/tests/test-release.sh` is the repeatable `darwin/arm64` release harness: exact Go pin, required tools, module integrity, native packaging, generated-key signing, checksum verification, archive contents, installation and SIGKILL rollback recovery in a temporary directory and home; `--staging-only` runs the staging regressions alone.
`scripts/tests/test-install-local.sh` checks the source wrapper against isolated destinations, `scripts/tests/test-skill-drift.sh` checks skill packaging without the user's skill directory, and `scripts/tests/test-bootstrap.sh` checks generated release-signature fixtures, archive rejection and the HTTPS bootstrap's required guards.
`scripts/tests/test-release-authority.sh` rejects publication commands in normal scripts and workflows, `scripts/tests/test-commit-authority.sh` rejects staging and commit commands outside its fixtures, and the full suite snapshots branch, remote-tracking and tag refs around release verification.
The CI workflow (`.github/workflows/ci.yml`) is optional and starts only through manual `workflow_dispatch`, with no automatic push or pull-request runs.
Its one ARM64 job on `macos-26` asserts Darwin/arm64, runs `go build ./...`, and invokes the shared full gate `scripts/tests/test.sh --full`.
Actions use immutable commit SHAs, the exact `go.mod` toolchain is restored and verified, and the job holds only `contents: read` with no push, tag, release or GitHub API step.
Use focused checks during development; the final integrated local full proof runs when the owner requests it, and online CI is requested explicitly for an independent environment check.

`scripts/utils/mailcli-preflight.sh capabilities` caches a valid `capabilities` envelope in an owner-only cache keyed by binary SHA-256, schema and selected command set; `invalidate` removes the entries for the current binary.
`doctor` results use the same identity with a five-minute freshness bound for healthy checks, failed envelopes are never cached, and `doctor --live` always runs immediately before an Apple Events operation.

`scripts/benchmarks/run-performance-evidence.sh` runs the reproducible performance matrix: MIME composition, attachment verification and streaming, search parsing, generated-store list, filter, search and lifecycle, mailbox-list query plans, rich-content preparation, draft pagination, raw-source building, mutation-account resolution and loopback IMAP STATUS/FETCH concurrency.
It fixes `GOMAXPROCS=4`, input shapes and repetitions, prints the environment and raw `ns/op`, `B/op` and `allocs/op`, and reports p50 and p95; `--group NAME` runs one group.
Every path uses generated fixtures or loopback fake servers and never opens Mail.app, reads the user's store, loads credentials or contacts a provider.
Release acceptance targets isolated process-inclusive metadata-list p95 below 50 ms and metadata-search p95 below 100 ms on the supported host, with no measurable Mail process CPU increase from direct-store operations.
