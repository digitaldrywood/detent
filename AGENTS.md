# AGENTS.md - Detent Agent Notes

- For future requested product or code changes, do not implement directly by default.
- Create a focused GitHub issue in `digitaldrywood/detent`, add `detent:todo`, and let Detent dogfood the work.
- Only make direct code changes when the human explicitly asks for manual implementation, asks to finish an already-started fix, or asks for local review and diagnostics that require edits.

## Issue effort selection

Use Codex Astra (`gpt-6-astra`) at low effort by default for features, fixes,
tests, reviews, and routine implementation, including cross-component work.

Every issue must include an explicit override, with model unset:

```detent-agent
schema: 1
effort: low
```

- `low` — the default for all work without a specific documented reason to escalate.
- `medium` — an exception for a concrete reasoning difficulty or evidence that low was insufficient; explain the reason in the issue.
- `high` — rare, significant research or architecture work with a written justification.
- `xhigh` and `max` — operator-designated only; never assign automatically.

Concurrency, recovery, routing, multiple files, or a new endpoint alone do not
justify higher effort. Preserve intentional operator exceptions. Configured
complexity levels default to low; verify any approved exception against the
runtime effort ceiling rather than raising broad defaults.

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
