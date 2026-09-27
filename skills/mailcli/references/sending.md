# Send and reconcile

Send needs authorized full review, --expected-revision REVISION --confirm; no Mail.app. submission_accepted/sent_copy_observed prove SMTP/Sent, never delivery.

Invalid headers stop before SMTP/claim, preserve draft. Correct named value, review again; RFC bounds preserve subject/display-name encoding.

Preserve exact quoted/escaped addresses (e.g. `"A B"@example.com`, escaped in JSON); name never changes identity/deduplication. BCC names validate too; never guess replacements.

ASCII addresses/Unicode display names need no SMTPUTF8. Internationalized envelope/raw headers require SMTPUTF8+8BITMIME after STARTTLS. Unsupported: pre-MAIL/DATA, draft preserved/transient claim cleared. Repair, never transliterate.

1. [Setup](setup.md) resolves exact sender; provider/binding/credential/address failures stop before transport. Historical save_attempt blocks send before setup/composition; reconcile original save or discard, never new save. [Native](native-handoff.md).
2. Account-lock setup precedes Keychain/composition/claim/SMTP. Unavailable: no effect/repair; reads available. MAILCLI_IMAP_MUTATION_LOCK=off permits process-local ordering. Pinned exact 0600 MIME spool carries size/SHA-256; path replacement cannot redirect reads/cleanup. STARTTLS SMTP/IMAP Sent; size-aware deadline <=15 min.
3. Short/oversized/unreadable smtp_source_invalid: `drafts inspect --ref REF --json`, no replay. smtp_data_incomplete proves no terminator attempt: retry only per next.do, never automatic canceled restart. Attempted terminator/final-reply uncertainty: smtp_submission_unknown, retain/reconcile, no resubmit. sent=SMTP 2yz+exact Sent persistence; sent_mirror_pending=acceptance/incomplete mirror. Neither proves delivery.
4. `drafts reconcile --ref REF --json`: local claim/IMAP, never SMTP. One Sent match must prove Message-ID/envelope/body/whole MIME hash; duplicate/mismatch/unreadable/absent fails closed. imap_append_incomplete before CRLF: discard session, search Sent, retry only known-incomplete APPEND/retained bytes. imap_append_outcome_unknown after attempted CRLF: search/verify, no auto-APPEND.
5. Reconcile retained MIME, not changed attachments. Missing/changed spool blocks APPEND/retains draft. Durable immutable receipt precedes spool retirement; inspect exposes it, consumed attempt returns it without submission.
