# Writing instructions for lower token cost

[Back to onboarding](ONBOARDING.md) · [Quick start](getting-started.md)

Instructions determine how much agents read, test, and repeat. The
[September 7–14, 2026 token audit](https://github.com/digitaldrywood/detent/issues/2671)
reported first-call medians of 26k–52k input tokens across projects. Tool output
made up 65–69% of incoming bytes; full validation logs were the largest single
outputs. On one host, 195 of 1,070 sessions ran the full suite ten or more times.
Treat those figures as a baseline from that fleet, not universal model limits.

## Keep the always-loaded budget small

Aim for about **12 KB combined** across `AGENTS.md`, `WORKFLOW.md`, and every
file the workflow requires agents to read. Count mandatory references too:
a short workflow that forces a directory read still has a large effective cost.
Use `wc -c AGENTS.md WORKFLOW.md` plus the explicit required-file paths to
measure bytes; bytes are a size budget, not a token count.

| Location | Put here |
| --- | --- |
| `AGENTS.md` | Short repository conventions, layout, safety constraints, and pointers to relevant skills. |
| `WORKFLOW.md` | Task execution policy and project-specific delivery requirements. |
| Skills | Reference material, specialized recipes, examples, and when to use them. |
| Orchestration config | Model selection, reasoning effort, and runtime policy. |

Do not require reading `CLAUDE.md` or whole directories. Extract the few rules
needed on every task into the canonical instructions; move detailed reference
material into skills with specific descriptions that explain when to invoke them.
Do not turn those pointers into a requirement to read every skill.

The audit's Codex setup loaded repository instructions under a 32 KiB cap and
exposed skill names and descriptions before invocation. A cap is not a budget
target. Once text or tool output enters context, subsequent calls can continue
to carry it; cached reads still have a cost (one tenth of input pricing in the
audited setup). Keep both initial instructions and later output small.

## State one validation rule once

Use one shared rule rather than repeating it in each lane and completion block:

> During implementation, run targeted `go test` commands on touched packages.
> Run the project's configured full validation gate exactly once immediately
> before push. After it passes, do not repeat it unless files change. Capture
> the full log and return a short summary; inspect relevant details on failure.

“Full gate” means the command selected for that project and stage. For this
Detent worker it is `make check-fast`; the heavier merge-queue checks remain
separate. Preserve any explicit safety-critical validation requirements.
Fixes after a failed run need validation again; a green unchanged tree does not.

If a project already provides `make check-summary`, use it after confirming it
runs the required gate and preserves its failure exit status. Detent currently
has no such target. This portable fallback captures the log without losing the
gate's exit code:

```sh
# Use the project's configured gate in place of make check-fast.
gate_log="$(mktemp "${TMPDIR:?Detent temporary directory required}/gate.XXXXXX")"
gate_status=0
make check-fast >"$gate_log" 2>&1 || gate_status=$?
if [ "$gate_status" -eq 0 ]; then
  echo 'Validation passed: make check-fast'
else
  tail -40 "$gate_log"
  echo "Full validation log: $gate_log"
fi
exit "$gate_status"
```

A green response should cost under 1k tokens; one status line suffices. Forty
lines are a useful failure excerpt, not a guaranteed token bound. Use `rg -n`
to locate a failure and read the relevant log slice if the tail omits it. Avoid
plain `make check-fast 2>&1 | tail -40`: without pipeline failure handling, the
successful `tail` can hide a failing gate. Keep logs in the attempt's temporary
directory and record the command and result in the Workpad before cleanup.

## Scope verification and review

Run browser and end-to-end verification only when the diff touches behavior
covered by that suite. This includes server routes or responses used by a UI
journey, not just frontend files. For server-only changes outside that coverage,
record the reason in the Workpad, for example: “Scheduler-only diff; browser
journeys do not cover this path. Focused scheduler tests passed.” Documentation
changes likewise need no browser run unless they change a covered UI artifact.

Configure one automated reviewer per project. Do not repeat a clean review on
the next push; re-check addressed findings after fixes, and review newly changed
scope as needed. Security audits stay per head: a review from an earlier commit
does not substitute for the required current-head security audit. Keep required
CI and its current-head evidence intact.

## Avoid duplicated contracts and repeated reads

Do not copy the Workpad or `detent-status` contract into `WORKFLOW.md`. Detent
appends it; a short reference to the appended handoff is sufficient. Do not pin
model names or reasoning effort in instruction files or skills. Those choices
belong to orchestration configuration, where the operator can change them once.

Read a needed file once. Use targeted lookups such as
`rg -n 'gate|validation' Makefile` afterward instead of rereading entire files.
Do not reread `AGENTS.md` or skill instructions already injected into context.
Read changed sections again when new edits make the earlier view stale.

## Measure before and after

Follow [Diagnosis: authority by question](diagnosis.md#authority-by-question):
use recorded history, not a live state snapshot, to establish a change.

1. Record the instruction revision and effective byte count. Choose comparable
   before/after windows within one project; record model, effort, task mix,
   sample size, and merge outcomes so unrelated changes do not masquerade as
   instruction savings.
2. From each sampled session's rollout, take the **first model call's
   `input_tokens`**, not the final cumulative total. Report median and p90.
   Inspect the backend's rollout schema first: cumulative usage events and
   per-call usage are not interchangeable. Count each session once and report
   missing rollouts rather than treating them as zero.
3. Select PRs actually merged in each window from recorded merge evidence.
   Match their project and issue/PR identities to `usage_events`, and sum
   `total_tokens` across all associated attempts, including retries, reviews,
   and merge work. Include work before the window for those PRs; filtering only
   by usage date undercounts long-running PRs. Deduplicate event IDs and avoid
   counting one event through both issue and PR joins. Some events have no
   `pr_number`; attribute them through the issue identity and report any
   unmatched usage separately.
4. Report total tokens divided by merged PR count, plus median/p90 of the
   per-PR totals. Keep input, output, and cached-input breakdowns separate where
   available; cached input is not an extra amount to add to total tokens.
   Repeat after an instruction change using the same attribution rules.

A smaller first call shows a smaller starting context. Lower tokens per merged
PR also captures fewer repeated gates, verbose logs, and redundant reviews.
Record both; do not claim savings from file size alone.
