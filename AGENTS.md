# AGENTS.md - Detent Agent Notes

- For future requested product or code changes, do not implement directly by default.
- Create a focused GitHub issue in `digitaldrywood/detent`, add `detent:todo`, and let Detent dogfood the work.
- Only make direct code changes when the human explicitly asks for manual implementation, asks to finish an already-started fix, or asks for local review and diagnostics that require edits.

## Issue effort selection

Model and reasoning effort are orchestration settings, not authoring decisions.
They are configured once in the operator's instance config, not in this repo,
and split by stage: Codex Astra (`gpt-6-astra`) plans at `low` effort and
validates at `medium`, and Codex Sol (`gpt-6-sol`) builds (code, rework, merge)
at `high`. `detent.yaml` deliberately carries an empty `agents.model_selection`
block so it inherits that split.

Do not put a `model` in a `detent-agent` block: it overrides every stage,
including Astra planning and validation. Use `effort: high`; each stage clamps
it to its own ceiling, so planning still runs at `low` and validation at
`medium`.

```detent-agent
schema: 1
effort: high
```

Escalation is an operator action: applying the `complexity:very-complex` label
routes the issue to Astra at `medium`. Agents never apply complexity labels and
never assign `xhigh` or `max`.

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
in the PR template.

## Validation

The operator has disabled blocking CI and local validation gates for this
repository. Do not require `make check`, `make check-fast`, a coverage gate,
or a local commit status before pushing or merging. The self-hosted project's
configured `gate.run` is `true`; it runs no validation and publishes no status.
Every `make` test, lint, vet, and build target is still capped by `TEST_PROCS`
(default 4) so worktrees that do run gates share the host (see
[docs/development.md](docs/development.md)).

Add a test only when it asserts a behavior no existing test asserts. Before
writing one, name the failure it would catch; if you cannot, do not write it.
Extend an existing table or fixture with a case instead of adding a sibling
function that rebuilds the same setup. Do not add tests for generated code,
for the text of documentation or configuration, or to move a coverage number.
A test that only re-executes a path another test already asserts is removed in
review (see [docs/test-suite-audit.md](docs/test-suite-audit.md)).

Focused `go test ./<touched-package>/...`, `go vet`, and targeted regressions
are available for diagnostics during edits; they do not become completion
gates. The scheduled full suite validates pinned `develop` commits and tags
only green commits. Every `develop` push still deploys to staging.

Other projects using Detent choose their own validation commands, required
checks, workflow triggers, and release policies. Do not introduce a product-wide
bypass to implement this repository's policy.

Detent workers must use their provided `TMPDIR`, `TMP`, or `TEMP`; never
fall back to host scratch space in a worker. See CLAUDE.md for safety-critical
coverage and fuzz diagnostics.
