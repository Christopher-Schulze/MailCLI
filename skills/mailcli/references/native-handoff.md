# Native compose handoff

data.draft_handoff.outcome: handed_off=delegate acceptance only; not_handed_off=pre-dispatch cancellation/confirmed failure; unknown=retained evidence/no replay (also untyped post-dispatch errors). Observe Mail, never guess.

- Handoff: sanitized new compose/To only; Mail default mailto app. Reply/forward stay local. No scripted save (compose_automation_unsupported), send/delivery/window-close proof. Post-dispatch cancel/timeout/unparseable result retains attempt/snapshots, suppresses late success, blocks retry. Observe, then `mailcli drafts handoff-reconcile --ref DRAFT_REF --attempt ATTEMPT_ID --outcome opened|failed --confirm --json`.

Historical save_attempt: `mailcli drafts reconcile --ref DRAFT_REF --json` observes only that save; exact match returns saved_draft/verified cleanup, unknown retains draft/claim. Conflicting/invalid claims fail closed. [Send claims](sending.md) reconcile separately.

| Boundary | Evidence | Result/cleanup |
| --- | --- | --- |
| Prepared | Claim before staging | No dispatch; verified recovery only |
| Pre-dispatch cancel/failure | No dispatch | not_handed_off; snapshots before claim |
| Dispatch | Durable dispatched before invocation | No unresolved replay |
| Delegate | confirmed_opened/confirmed_failed | handed_off/not_handed_off; verified cleanup |
| Unavailable | outcome_unknown + ID/snapshots | Observe then reconcile |
| Reconcile | Exact retained attempt + observation | Confirmed opened/failed; snapshots before claim |

Cleanup refusal retains claims/evidence and the durably recorded acceptance/failure/cancellation. Inspect the attempt, then repeat handoff-reconcile with the same outcome; use failed for canceled_before_dispatch. Never change a recorded outcome or replay the native handoff to repair confirmed cleanup. Snapshot retention can be false while claim cleanup remains pending. Unknown dispatch still requires observation.
