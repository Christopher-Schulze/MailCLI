# MailCLI Agent Rules

Read the `Development workflow` section of `docs/documentation.md` before changing this repository. If `AGENTS.local.md` exists, read it for owner-specific instructions; that file is ignored and is not part of a fresh clone.

## Task and write authority

- Treat `docs/tasks.md` and `docs/tasks/` as the private local task control plane when present. Do not stage or publish them.
- Use one writer in the primary worktree. Before the first write, acquire an exact TASK/path lease with `scripts/utils/manage-write-lease.sh acquire`; stop on a dirty worktree or existing lease. Never steal or expire a stale lease.
- For tracked work, stage only leased paths, run `review TOKEN` and `gate TOKEN`, then commit the gated patch with a `TASK NNN:` subject and run `release TOKEN`. Mark implementation done only after the commit exists and the full gate passes.
- For an explicitly authorized private-only TASK, verify the deliverable, exact path changes, unchanged HEAD/tracked status, and owner-only task-history snapshot. Run `private-proof TOKEN SNAPSHOT EXACT_CHANGED_PATH...` before `abort TOKEN`; compare their path-change receipts. `abort` alone does not prove completion.
- A lease never authorizes a push, tag, GitHub release, external action, or destructive cleanup.

## Release authority

- The user alone authorizes releases. Never create, push, or delete a tag or GitHub release without an explicit request naming the exact version and action.
- Never change version strings or name a new version without an explicit user instruction. A code commit does not authorize publication.
