# AGENTS.md - Detent Agent Notes

- For future requested product or code changes, do not implement directly by default.
- Create a focused GitHub issue in `digitaldrywood/detent`, add `detent:todo`, and let Detent dogfood the work.
- Only make direct code changes when the human explicitly asks for manual implementation, asks to finish an already-started fix, or asks for local review and diagnostics that require edits.

## Issue effort selection

The default is Codex Sol (`gpt-5.6-sol`) at `high` effort for all work: features,
fixes, tests, reviews, and routine implementation, including cross-component work.

Every issue must include an explicit `detent-agent` block, with `model` unset:

```detent-agent
schema: 1
effort: high
```

- `high` — the default; use it unless the issue states a documented reason not to.
- `low` — an exception that requires a written reason in the issue, and only for
  trivial mechanical edits.
- `medium` — an exception that requires a written reason in the issue.
- `xhigh` and `max` — operator-designated only; never assign automatically.

Concurrency, recovery, routing, multiple files, or a new endpoint alone never
justify changing the effort. Preserve intentional operator exceptions and leave
`model` unset so the issue inherits the fleet-standard model.

## Mechanism moratorium

Effective 2026-09-10 until the operator lifts it. Detent has grown a large set
of interacting self-protection mechanisms (brakes, breakers, leases, parks,
recovery sweeps, revocations, reconcilers). Their interactions are now the main
source of incidents.

- Do not add a new brake, breaker, lease, park, recovery path, revocation,
  reason code, or reconciliation loop.
- A fix for a misbehaving mechanism must remove or consolidate a mechanism, or
  state in the PR why it cannot. "Add a guard for the new case" is not a fix.
- Infrastructure failures (backend startup, protocol errors, workspace hooks)
  are attributed to the instance, never to the issue.
- The orchestrator is the only writer of tracker lane state; workers report
  outcomes and never write lane labels.
- Do not add configuration keys, CLI subcommands, or dashboard surfaces to work
  around a mechanism. Fix the mechanism.
- Machine-filed issues carry an origin stamp and a fingerprint; never file a
  duplicate of an open issue, comment on it instead.

## Repository invariants

Follow [docs/invariants.md](docs/invariants.md). Changes to an invariant or its
enforcement must update that document in the same PR and identify the invariant
in the PR template. Run `make check`, including the existing invariant gate.
