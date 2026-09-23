You are working on {{ issue.identifier }}: {{ issue.title }}.
Current Detent status: {{ issue.state }}.

Follow repository instructions and keep changes scoped to the issue.
Use the Detent-appended Blocked handoff block for the Workpad, dependencies,
human questions, completion, and tracker ownership contract.

## Project CI Quality Gates

- `<required-stage-category>`: local command `<project-command>`; CI check `<project-check-name>`.

Whenever you touch CI configuration or perform a review, verify every declared
stage exists and passes on the current pull request head. Do not rely on Detent
or `detent doctor` to infer required stages or inspect CI configuration.

## Validation

During implementation, run targeted tests for touched packages. The Detent-appended
Validation gate block is authoritative for the command and full-versus-focused
selection, including Merging. Perform that required validation exactly once,
immediately before push; do not rerun after green unless files changed.
Use a summary target when configured, otherwise pipe output through
`tail -40` with Bash `pipefail` enabled to preserve failure status.
Fix failures and summarize the result; inspect full output only when needed.

## Required Execution Flow

The orchestrator owns all lane transitions. Follow the configured delivery
profile and report outcomes through the appended handoff contract.
Before rebase, preserve the effective diff; after rebase compare with
`git range-diff` and resolve any unexplained loss before pushing.

Commit, push, and open a draft PR referencing the
issue. Review the diff and address actionable feedback before marking ready.
Verify current-head check conclusions and reviews before reporting completion.
Report skipped PR checks as skipped, never as passing tests. When real CI runs
only on merge groups, require successful merge-group checks before merge.

### State: Todo

Read the issue and Workpad, fetch the base branch, and confirm dependencies.
Reproduce the reported bug, implement the smallest complete change, and follow
the shared validation rule.

### State: In Progress

Read the issue, PR, comments, and Workpad. Continue implementation and the
shared delivery steps from the current state.

### State: Rework

Read human, CI, and bot feedback. Fix actionable findings and deliver the
updated PR using the shared validation rule. Verify current-head check conclusions and reviews.

### State: Merging

Rebase onto the current base, follow the shared validation rule, and push.
Wait for the applicable current-head or merge-group CI and address actionable review. Merge using the
configured strategy and exact head SHA, then report the result.
