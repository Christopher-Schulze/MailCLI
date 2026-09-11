# Output and recovery details

Human `messages get` and `drafts open` details replace terminal controls with spaces; body LF/TAB and ordinary Unicode remain readable. JSON retains decoded values, body exports retain normalized content, and `messages raw`/raw exports retain exact MIME bytes. Use those explicit data paths when exact content is required.

Keep bodies, addresses, credentials, and attachment bytes out of logs and summaries unless requested. Detail commands default to metadata; use `--view plain|full`, `--fields`, or `--export /absolute/new/path` only when needed. Exports are complete, exclusive mode-0600 files with verified size and SHA-256; never accept truncation or infer missing bytes.

## Batch input

Use `mailcli batch --input - --json` with explicit refs and unique item IDs. Read the published `batch` schema for allowed operations, item fields, limits, and concurrency. Input is exactly one object up to 16 MiB; duplicate keys, case aliases, unknown fields, and trailing documents fail before dispatch. Results retain input order and per-item evidence. There is no automatic retry; never replay successful or uncertain items.

## Recovery details

Transport codes survive `fmt.Errorf` wrapping and `errors.Join`; an outcome-uncertain code wins over cleanup or rejection codes, so replay stays forbidden while `error.message` retains the joined diagnostics. If SMTP acceptance is followed by a local composed-reader close error, the acceptance evidence and cleanup diagnostic remain visible, the claim stays non-replayable, and no second SMTP submission is attempted.

For `smtp_submission_unknown`, `sent_mirror_pending`, `imap_append_outcome_unknown`, `imap_copy_outcome_unknown`, and `imap_move_outcome_unknown`, retain the operation ID and follow the emitted recovery command. A send attempt points to `drafts reconcile --ref DRAFT_REF --json`; a retained visible-compose attempt points to `drafts handoff-reconcile --ref DRAFT_REF --attempt ATTEMPT_ID --outcome opened|failed --confirm --json`; partial hydration points to `messages get --ref MESSAGE_REF --json` or `drafts open --message MESSAGE_REF --json`. Never blindly retry.
