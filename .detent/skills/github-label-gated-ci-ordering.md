---
name: github-label-gated-ci-ordering
description: Diagnose and preserve GitHub Actions check contexts when pull request CI is triggered by newly applied labels.
when_to_use: Use when required checks run on pull_request labeled events, pushes lack fresh checks, or later label-triggered runs replace passing contexts with skipped results.
---

# Preserve label-gated CI ordering

- Inspect the workflow event types and job label conditions before changing orchestration. Confirm which label gates the required checks and whether `synchronize` is intentionally absent.
- Correlate the PR head SHA, force-push events, label remove/add events, workflow runs, and REST check-runs. Treat missing or skipped current-head checks as non-green even when an earlier run passed on the same SHA.
- Reapply the configured gate label after every successful head-changing push. Refresh newly created PR metadata first so the repository, PR number, and current head SHA are exact.
- When reapplying multiple CI lane labels, apply non-gating labels first and the required-check gate label last. A later run can publish skipped check-runs with the same context names and shadow earlier passing results.
- Track the configured gate label specifically. A different `ci-trigger-label` command after the gate label invalidates the ordering evidence and requires the gate label to be reapplied again.
- Preserve both signals when a shell invocation combines `git push` and label reapplication. A failed combined invocation must remain a delivery failure; a successful one only satisfies ordering when the configured gate label runs after the push.
- Before merging, query check-runs for the exact current head and verify the final result for every required context is passing.
