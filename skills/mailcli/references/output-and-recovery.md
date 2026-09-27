# Output and recovery details

Store profile: framework-only drift warns unverified with reads available; unsupported version/minor/UUID/schema fails closed without a profile.

Human output sanitizes controls, retaining body LF/TAB. JSON keeps decoded values; body exports normalized, raw exports exact MIME.

Exports: complete exclusive absolute new 0600 files, verified size/SHA-256, never truncated.

## Batch input

`mailcli batch --input - --json`: exact-schema object <=16 MiB, refs/unique IDs. Duplicate/alias/unknown/trailing keys fail before dispatch. Move/copy need mailbox ref; move/delete accept allow_draft_mutation. Only delete accepts/requires --confirm. Results preserve order/effects; no successful/uncertain write replay.

Batch: default 1 MiB, --max-bytes <=64 MiB. Reads default metadata; item view/fields overrides read-only defaults.view/fields, never both selectors. Overflow: data.required_bytes/data.limit_bytes/data.measured; exact includes newline, lower_bound counts encoded body/headers or retained projected outcomes. Read overflow stops admission, cancels/joins active reads and preserves completed outcomes; unstarted skipped_budget items count as skipped. Keeps first 10 item IDs and states and full counts: correct selectors/budget before replay. Mutation execution is unaffected; overflow keeps effects, replay_allowed:false.

```bash
printf '%s' '{"operation":"read","items":[{"id":"meta","ref":"REF"},{"id":"body","ref":"REF","view":"plain"}]}' | mailcli batch --input - --json
```

Partial batch: ok:false/batch_partial/exit 1. Inspect every item plus message_state/delete_result/saved_attachment, not only exit status.

## Recovery details

error.unclaimed_spool proves pre-submit/no claim and reports no-follow path/type/UID/mode. Inspect draft; cleanup only exact object with free lock, no claim, unchanged metadata. Symlink: unlink link, never target. Reporting deletes nothing. Accepted-send spool: retain/reconcile, never delete or resubmit.

Attachment retry: absent output + typed transient; obey next.do, no automatic canceled restart. TLS/input/missing/ambiguous/undownloaded needs correction; integrity/resource/unknown inspection. Published complete/partial/unknown forbids replay; retain saved_attachment/item guidance.

Obtain refs from listings, never edit tokens; uniquely resolve ambiguity, restart stale cursors. Unsupported version: compatible binary/preserve catalog; corrupt: inspect. UID/Message-ID: verified mapping/UIDVALIDITY. Partial raw: complete download/targeted hydration.

Corrupt access gate: quit Mail; retry same operation stopped (verified cleanup returns mail_not_running, no dispatch); reopen/retry. Running Mail/failed lookup preserves corruption. Unsafe gate: inspect owner/type/link count/identity, retain inode. Never delete/replace mail-access.lock or age-expire ownership.

Follow next.do/emitted recovery; only typed transients allow read retry. Effects/uncertainty/acceptance outrank cleanup/rejection; never invent absent args or replay accepted/uncertain writes.

Resource overflow reports error.limit{name,value}/observed_at_least (byte bound >=value+1); inspect, no unchanged replay.

IMAP rejection retains command/status/leading code/sanitized UTF-8 <=512 bytes. Only leading codes classify. NO [UNAVAILABLE] reads may retry, BAD never automatically. Accepted mirror failure: reconcile.

Follow recovery.instruction: Full Disk Access/Automation/config/TLS repair, never disable verification. FETCH retry needs network/truncation cause; deterministic/rejection/unknown failures need inspection.

Mutation mailbox ambiguity: fix colliding server names/special-use assignments, refresh listing, proceed only when unique; inspect retained uncertainty.


Read overflow: raise budget/narrow fields/export full content. [Drafts](drafts.md): resize lists/resolve conflicts/inspect completed mutations, never repeat.

Draft busy: inspect/no replay; emitted command needs ref (state plain, conflicts/completed full). Search stale: remove cursor; changed index: safe retry; keep filters. Stale binding: accounts.list/setup. Mail recovery: quit/reopen, no safe CLI recovery.
