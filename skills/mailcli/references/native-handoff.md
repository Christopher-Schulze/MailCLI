# Native compose handoff

`data.draft_handoff.outcome` has three results: `handed_off` proves delegate acceptance only; `not_handed_off` means pre-dispatch cancellation or confirmed failure; `unknown` retains evidence and forbids replay. An untyped error after dispatch is also unknown. For unknown, inspect Mail.app to determine whether the compose opened, then use the supplied `drafts.handoff-reconcile` recovery with `--outcome opened` or `--outcome failed`; never guess the observation. The stored lifecycle states and reconciliation results described below remain unchanged.

- Scripted native save is unavailable. `drafts handoff` opens a visible new compose with sanitized content and To recipients only; Mail.app must be the default `mailto` application. `confirmed_opened` means the sharing-service delegate reported success, never that the draft was saved or sent; `confirmed_failed` means the delegate reported failure. Cancellation before native dispatch returns `canceled_before_dispatch` and cleans staged evidence. Cancellation, timeout, or an unparseable native result after dispatch returns `handoff_outcome_unknown`, retains the attempt ID and attachment snapshots, suppresses late success, and blocks retry. Inspect Mail.app, then run `mailcli drafts handoff-reconcile --ref DRAFT_REF --attempt ATTEMPT_ID --outcome opened|failed --confirm --json` to resolve the retained claim and remove its snapshots. AppKit exposes no supported compose-window cancellation or close operation, so never claim that Mail closed the window. Replies and forwards remain local review drafts.

For a historical `save_attempt`, use `mailcli drafts reconcile --ref DRAFT_REF --json`. It observes that exact retained native save and never creates a new save. Exact observation returns `data.saved_draft` and cleans verified local state; an unknown result retains the draft and claim, and its recovery guidance names the same reconciliation command. Conflicting or invalid claims fail closed. Send claims continue through send reconciliation; handoff claims require the separate observed-outcome command above.

| Lifecycle boundary | Durable evidence | Public outcome and cleanup |
| --- | --- | --- |
| Prepared | Claim written before staging; dispatch not started | No native effect; verified prepared staging may be recovered before retry |
| Canceled or rejected before dispatch | No native dispatch | `not_handed_off`; remove only verified snapshots, then claim; cleanup refusal retains the claim |
| Dispatch starts | `dispatched` persisted before native invocation | Never replay while the result is unresolved |
| Delegate accepts | Confirmed native completion | `handed_off`; clean verified snapshots and claim; cleanup errors preserve known acceptance and retained evidence |
| Delegate rejects | Confirmed native failure | `not_handed_off`; clean verified snapshots and claim |
| Dispatch result unavailable | `outcome_unknown`, same attempt ID and snapshots | `unknown`; one recovery: inspect Mail.app and reconcile the observed opened/failed result |
| Explicit reconciliation | Exact retained dispatched/unknown attempt and observed resolution | Existing `confirmed_opened`/`confirmed_failed` result; remove verified snapshots before clearing the claim |
