# Local drafts and reviewed revisions

Review local create/update before authorized send/destruction.

--ref or positional REF; all supplied refs must match, options may surround. Open/adopt: store refs; otherwise local. Scripts/recovery: --ref.

Reply/forward set from to the source account's address when that is unique; else from is empty. Draft views carry from and send_blockers (from_missing, recipients_missing): resolve every blocker (--from ADDRESS on update) before send; blockers are not part of the revision.

- JSON 1 MiB, --max-bytes <=64 MiB. List fields: age_days/created_at/updated_at or all alone; core refs/review/account/format/claims/page stay. Preview never truncates: `drafts inspect --ref REF --view full --export /absolute/new/path --json` for overflow.

- Rich bounds: source/Markdown/HTML/plain each 4 MiB, tree 65536 nodes/512 levels, link labels 16 MiB. Review persisted loss diagnostics/sanitization; rejected/canceled render is not saved.
- List ref-ordered, default 20/limit 1..200; follow page.next_cursor. Changed directory: invalid_cursor/restart. Overflow: no rows/new cursor; emitted recovery halves limit >1, retains incoming cursor/fields/budget; limit 1 requires correction. Inspect state_error; no bodies/private claims. Page revision is never review revision.
- Edit human-only; agents: `drafts update --ref REF --expected-revision REV --input - --json`. Nonterminal refuses before editor/candidate; stdin/stderr require same foreground terminal. JSON editor streams stderr, one stdout envelope; human streams separate; terminal restores. Error/cancel/invalid edit retains error.draft_editor candidate/revision/exit/signal: read current draft/candidate, merge explicitly, newly reviewed update; no blind replay.
- JSON one object <=16 MiB, exact-case unique decoded/nested keys; duplicates/aliases/unknown/trailing input fail before effects. Attachments: path strings. Patch omission keeps, empty clears; attachments:[] clears. Changed body_format needs body; same format preserves it. Never guess rejected input.
- Review complete --view full result or `drafts inspect --ref REF --view full --json`; no duplicate inspect needed. Metadata/list/truncated is not review. data.draft.revision (preview: data.draft_preview.revision) goes in --expected-revision, never editable JSON. Account/recipient role-name-order/subject/body/threading/attachment path-size-hash invalidate; timestamps do not.
- Conflict: read full current/candidate, merge, update with reviewed revision/--input CANDIDATE_PATH, never blindly use error revision. Claims/receipts must match draft_revision; legacy unavailable revision permits inspect/reconcile only.
- Open hydrates store ref, no editor; local draft_* uses inspect/preview. Close editors before mark/move/delete; --allow-draft, delete also --confirm.
- Adopt copies to new local draft_* with managed attachments, original unchanged/create bounds. Empty recipients may adopt, not send. Incomplete source: open Mail draft first; attachment errors keep typed codes. Publication/staging: no replay; list/inspect new ref, preserve staging.
- Completed overflow: retain ref/revision, inspect, never repeat mutation. Only inspect exports local body. Broken stdout: inspect state before retry.
- handoff_attempt ID/outcome/dispatch/snapshot counts survive projections/exports/overflow, without paths/content. Unknown blocks replay; cleanup removes terminal claims. [Handoff](native-handoff.md).
- Prune dry-runs stale never-sent drafts/expired receipts/orphans; --confirm cleanup. Would-sweep differs from swept_artifacts. Partial failure retains effects/failed refs; busy skipped, never swept; no completed-work replay.
- Pinned directory/lock identity covers cleanup; draft_lock_unsafe/changed terminal, never redirect via replacement.
