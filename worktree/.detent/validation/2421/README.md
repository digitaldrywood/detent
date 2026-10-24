# Recovery convergence verification

- SQLite regressions reproduce a Todo issue with four acknowledged parks and a closed dependency, including stale runtime holds and later park generations.
- Public orchestrator recovery API tests submit four concurrent retry requests and assert one worker start. Restart boundaries close and reopen SQLite after intent persistence, tracker mutation, and retry queuing.
- Independent dependency, budget, project-outage, stale-tracker, and invalid-configuration predicates prevent scheduler dispatch. Multiple causes remain visible together.
- Failed journal writes preserve existing holds. A newer attempt supersedes old retry intent. Stale acknowledgement writes cannot lower the durable sequence or replace its timestamp.
- Real runner model-selection tests distinguish unsupported effort from catalog outage. Configuration completion tests cover typed, wrapped, and restored errors; preflight checks avoid repeated launches.
- Focused tests passed for orchestrator, runner, store, and web. Focused recovery race tests passed. `make generate` passed. Full `make check` and current-head CI remain required before handoff.

Browser verification used the real receipt and recovery handlers through a temporary Go overlay and an ephemeral HTTP listener. Chrome showed both blockers, the next recheck, and neutral blocked feedback after Retry fresh. The preview exited successfully after the browser closed. The live process was untouched.

![Recovery receipt and blocked action feedback](recovery.png)

Skill draft: no — existing recovery and isolated-preview skills cover the method.

## Merge-group rework

Merge-group run 34558337339 exhausted Go's default 10-minute package
budget. Its four active tests were only 6–19 seconds old. Hundreds of
subtests waited in `testing.waitParallel`; the active stacks showed a
runnable SQLite migration owner and waiters on `store.migrationMu`, not
an unchanged worker wait. The `runner panic: boom` messages are recovered
panic-fixture outcomes several minutes before the timeout.

After rebasing onto f4e55b02, `go test ./internal/orchestrator -race -count=1`
passed on macOS arm64 in 157.424 seconds. The same full suite passed in an
isolated Linux arm64 container limited to four CPUs in 184.630 seconds.
A second sample using the exact `tools/testgate -race -parallel 4 -timeout 20m`
command passed in 183.210 seconds, including all safety fuzz seeds.
These samples do not reproduce hosted amd64 speed; the recorded hosted
failure remains the evidence for cumulative budget exhaustion.

Both race entry points now use existing `tools/testgate` for orchestrator,
with four parallel tests and a bounded 20-minute package budget. This
provides headroom beyond the observed hosted 10-minute exhaustion without
changing individual deadlines, test selection, assertions, migration work,
or production scheduling. Other package budgets remain unchanged. CI
uploads raw test events and timing summaries; its outer verification job
allows 45 minutes for the existing Hub gate plus the orchestrator gate and
other checks. Local coverage still includes the complete orchestrator suite.
