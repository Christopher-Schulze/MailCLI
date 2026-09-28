# Output and recovery details

Human output sanitizes control characters and keeps body line feeds and tabs. JSON keeps decoded values; body exports are normalized and raw exports are the exact MIME. Exports go to new absolute 0600 files, are created exclusively, verified for size and SHA-256, and never truncated.

## Batch input

`mailcli batch --input - --json` takes one exact-schema object of at most 16 MiB with refs and unique IDs. Duplicate, aliased, unknown or trailing keys fail before dispatch. Move and copy need a mailbox ref, move and delete accept `allow_draft_mutation`, and only delete accepts and requires `--confirm`. Results keep their order and effects, and a successful or uncertain write is never replayed.

Reads default metadata. An item's `view` or `fields` overrides the read-only `defaults.view` or `defaults.fields`, and no item sets both selectors. The output obeys `--max-bytes`. On overflow `data.required_bytes`, `data.limit_bytes` and `data.measured` report the sizes (`exact` includes the newline, `lower_bound` counts the encoded body and headers or the retained projected outcomes). A read overflow stops admission, cancels active reads, keeps completed outcomes, and counts unstarted `skipped_budget` items as skipped. The response keeps the first 10 item IDs and states and the full counts, so correct the selectors or the budget before any replay. Mutations are unaffected: an overflow keeps their effects and sets `replay_allowed:false`.

```bash
printf '%s' '{"operation":"read","items":[{"id":"meta","ref":"REF"},{"id":"body","ref":"REF","view":"plain"}]}' | mailcli batch --input - --json
```

A partial batch returns `ok:false`, `batch_partial` and exit 1. Inspect every item and its `message_state`, `delete_result` or `saved_attachment`, not only the exit status.

## Recovery details

`error.unclaimed_spool` proves that nothing was submitted and no claim exists. Inspect the draft, and clean up the reported object only with a free lock, no claim and unchanged metadata; for a symlink unlink the link, never the target. Reporting deletes nothing. An accepted-send spool is retained and reconciled, never deleted or resubmitted.

Retry an attachment save only when its output is absent and the error is a typed transient; obey `next.do` and never restart a canceled save automatically. TLS, input, missing, ambiguous or undownloaded errors need correction, and integrity, resource or unknown errors need inspection. A published complete, partial or unknown result forbids replay: keep `saved_attachment` and the item guidance.

Obtain refs from listings and never edit tokens; resolve an ambiguity uniquely and restart stale cursors. For an unsupported version use a compatible binary and keep the catalog; for corruption inspect. A UID or Message-ID needs a verified mapping and UIDVALIDITY. For a partial raw source complete the download or hydrate the missing part.

For a corrupt access gate quit Mail, retry the stopped operation (a verified cleanup returns `mail_not_running` without dispatch), then reopen Mail and retry. A running Mail or a failed lookup preserves the corruption. For an unsafe gate inspect owner, type, link count and identity, and keep the inode. Never delete or replace `mail-access.lock` or expire its ownership by age.

`confirmation_required` means the user has not authorized the action: ask the user before adding `--confirm`. Follow `next.do` and the emitted recovery; only typed transients allow a read retry. Effects, uncertainty and acceptance outrank cleanup and rejection, and never invent absent args or replay an accepted or uncertain write.

A resource overflow reports `error.limit{name,value}` and `observed_at_least`; inspect, and never replay unchanged. Only the leading IMAP response code classifies a rejection: reads may retry after `NO [UNAVAILABLE]` and never automatically after `BAD`. After an accepted mirror failure reconcile.

Follow `recovery.instruction` and never disable TLS verification; retry a FETCH only for a network or truncation cause. For an ambiguous mutation mailbox fix the colliding server names or special-use assignments, refresh the listing, and proceed only when it is unique.

For a read overflow raise the budget, narrow the fields or export the full content. For [Drafts](drafts.md) resize lists, resolve conflicts, and inspect completed mutations without repeating them.

For a busy draft inspect and do not replay; the emitted command needs a ref (state metadata, conflicts/completed full). For a stale search remove the cursor, and after an index change retry with the same filters. A stale binding needs `accounts list` and setup. Mail recovery means quitting and reopening Mail; there is no safe CLI recovery.
