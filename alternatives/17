---
name: durable-tracker-authorizations
description: Preserve pre-dispatch authorization across mutable tracker state without letting a late agent declaration bypass completion, breaker, or promotion gates.
when_to_use: Use when issue fields, labels, or Workpad declarations opt a Detent attempt out of an ordinary deliverable or approval requirement and the decision must survive restart.
---

# Bind mutable declarations to accepted attempts

1. Identify the ordinary gate being relaxed and preserve it as the fallback for every invalid, missing, or late declaration.
2. Parse the authorization from the issue before dispatch and snapshot only a valid supported value on the running attempt. Never infer pre-authorization from tracker state fetched at completion.
3. At completion, require the dispatch snapshot, the matching current declaration with its evidence, and all work-product invariants. Persist a distinct accepted kind and progress reason only when every condition passes.
4. Treat the durable successful-attempt record as the authority for downstream transitions. Current issue text may remain a required consistency check, but it is never sufficient by itself.
5. Restore accepted state after restart only from terminal attempt metadata that carries the kind, progress reason, and completion evidence timing. Do not reconstruct acceptance from the current issue body or Workpad alone.
6. Audit every consumer of the raw declaration. Transition, gate-wait, revocation, reconciliation, and dashboard paths must either require the accepted-attempt marker or be limited to parsing and diagnostics.
7. Test pre-authorized acceptance, undeclared assertion, authorization added after dispatch, dirty or unpushed repository work, restart restoration, and revocation with current-only tracker state.
