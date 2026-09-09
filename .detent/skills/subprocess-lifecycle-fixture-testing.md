---
name: subprocess-lifecycle-fixture-testing
description: Separate subprocess readiness from lifecycle actions and verify that emergency test cleanup cannot hide broken production cleanup.
when_to_use: Use when subprocess cancellation or parent-exit tests fail readiness deadlines, leak helpers after failures, or need to preserve strict child-termination assertions under scheduler pressure.
---

# Verify subprocess lifecycle fixtures

- Retrieve the original failing job log, not only the latest attempt of a rerun. Separate observed errors from hypotheses about scheduling or production cleanup.
- Reproduce startup sensitivity with a controlled delay before readiness publication. Keep the real process-group setup and lifecycle implementation intact. A delayed-start reproduction proves a fixture timing assumption, not the cause of an untraced historical incident.
- Wait for backend startup acknowledgement as well as child readiness before cancellation. Hold parent-exit fixtures on a separate control pipe or stdin command until the test confirms the child is alive, then release the parent. Preserve inherited output descriptors when they are the behavior under test.
- Use bounded OS deadlock guards around readiness and completion; retain exact cancellation, exit-status, and descendant-termination assertions. Do not substitute fake time for external process scheduling.
- Register cancellation and joining before waiting for readiness, with cleanup ordered before temporary directory removal. Once a child PID is known, register independent emergency termination that runs only after the assertions. Isolate coverage output for Go test-binary helpers.
- Use temporary Go overlays to verify negative controls without editing the production worktree: restore the old readiness guard to prove the regression fails; omit production post-exit cleanup to prove the lifecycle assertion still fails. Independently check the recorded child PID after each failing test exits to verify emergency fixture cleanup removed it.
- Repeat focused race and coverage tests for each affected package, run the repository gate, and verify Linux execution with a proper init process or hosted runner. Record what each negative control establishes and leave unsupported historical causes explicitly unresolved.
