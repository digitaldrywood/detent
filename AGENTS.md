# AGENTS.md - Detent Agent Notes

- For future requested product or code changes, do not implement directly by default.
- Create a focused GitHub issue in `digitaldrywood/detent`, add `detent:todo`, and let Detent dogfood the work.
- Only make direct code changes when the human explicitly asks for manual implementation, asks to finish an already-started fix, or asks for local review and diagnostics that require edits.

## Issue effort selection

Model and reasoning effort are orchestration settings, not authoring decisions.
The fleet default is Codex Astra (`gpt-6-astra`) at `low` effort — equivalent in
capability to Sol at `high` — and it is configured once in the operator's
instance config, not in this repo. `detent.yaml` deliberately carries an empty
`agents.model_selection` block so it inherits that default.

Do not put a `model` in a `detent-agent` block. If you include one for `effort`,
use `low`; the configured policy ceiling clamps anything higher, so a raised
effort in an issue body has no effect.

```detent-agent
schema: 1
effort: low
```

Escalation above the default is an operator action: applying the
`complexity:very-complex` label routes the issue to the high-effort level.
Agents never apply complexity labels and never assign `xhigh` or `max`.

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

Do not re-read AGENTS.md, CLAUDE.md, or an injected skill when its contents are already in the prompt.

During the edit loop, run `go test ./<touched-package>/...` and `go vet` on
those same packages; use focused regressions while iterating. After all edits
and review fixes, run the configured `gate.run` exactly once immediately before
pushing, following the command and scope in WORKFLOW.md’s Detent Protocol
section, including the existing invariant checks. Never repeat a green gate unless
files changed; after a failure, fix the cause before retrying.

Keep gate output out of the conversation except for a short summary. Use Bash
with `pipefail` so truncation cannot hide a failing exit status. Set and export
`gate_command` to the configured `gate.run` command before using this wrapper:

```bash
bash -o pipefail -c '
  : "${gate_command:?Set gate_command to the configured gate.run}"
  log=$(mktemp "${TMPDIR:-${TMP:-${TEMP:-/tmp}}}/detent-check.XXXXXX") || exit
  echo "Gate log: $log"
  bash -o pipefail -c "$gate_command" 2>&1 | tee "$log" | tail -40
'
```

Detent workers must use their provided `TMPDIR`, `TMP`, or `TEMP`; never
fall back to host scratch space in a worker. For standalone contributors without
these variables, the example uses `/tmp`. The command retains the full log.
On failure, inspect only the failing package or tool diagnostics from that log;
do not rerun the gate just to recover output. A green result needs only the
success status in the handoff, not the log. See CLAUDE.md for safety-critical
coverage and fuzz requirements.
