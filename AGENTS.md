# MailCLI Agent Rules

Read the `Development workflow` section of `docs/documentation.md` before changing this repository. If `AGENTS.local.md` exists, read it for owner-specific instructions; that file is ignored and is not part of a fresh clone.

## Task and write authority

- Treat `docs/tasks.md` and `docs/tasks/` as the private local task control plane when present. Do not stage or publish them.
- Use one writer in the primary worktree. Before tracked or build-asset writes, acquire an exact TASK/path lease with `scripts/utils/manage-write-lease.sh acquire`; stop on a dirty worktree or existing lease. Never steal or expire a stale lease. Plain ignored task edits need no lease or snapshot ritual.
- For tracked work, stage only leased paths, run `review TOKEN` and the relevant registered `gate TOKEN --checks PATH...`, then commit the gated patch with its exact `TASK NNN:` subject and run `release TOKEN`. Mark implementation done only after its acceptance, appropriate checks, documentation and commit exist. Record targeted evidence separately from deferred full verification.
- During the owner-authorized queue execution, run race, vulnerability and the complete suite once at the end of the integrated queue. Never count a targeted receipt as full proof or rerun broad gates per task.
- Close private-only documentation work by verifying its actual deliverable and updating the board/detail; genuinely move completed details to `docs/tasks/done/`. Do not create a second task repository, snapshot, proof receipt or empty product commit. Existing backups remain untouched.
- A lease never authorizes a push, tag, GitHub release, external action, or destructive cleanup.

## Release authority

- The user alone authorizes releases. Never create, push, or delete a tag or GitHub release without an explicit request naming the exact version and action.
- Never change version strings or name a new version without an explicit user instruction. A code commit does not authorize publication.
