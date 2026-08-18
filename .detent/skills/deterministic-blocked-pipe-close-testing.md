---
name: deterministic-blocked-pipe-close-testing
description: Reproduce and prevent subprocess pipe-close deadlocks when close waits for blocked I/O on only some operating systems.
when_to_use: Use when a transport or subprocess test hangs because one direction is blocked on pipe I/O while shutdown must release another backpressured direction.
---

# Test blocked pipe close deterministically

- Build the wait-for graph from complete goroutine stacks before changing shutdown code. Identify the in-flight I/O, the close waiting for it, and the independent backpressure preventing the peer from exiting.
- Wrap the pipe writer with a test closer that observes write start and waits for that active write to return before delegating close. Use this to model platforms whose file close waits for concurrent pipe I/O.
- Drive each direction into backpressure with observable conditions such as write-start and full-buffer signals. Do not use sleeps to arrange the deadlock.
- Send readiness and completion signals over an independent channel such as captured stderr, then use one raw write larger than the pipe capacity. If helper stacks remain runnable in serialization or compression, replace the workload instead of extending the deadline; repeated encoding measures throughput rather than pipe liveness.
- Start close with a generous context deadline and assert that both the blocked operation and close return. Keep the deadline as a deadlock guard, not a performance assertion.
- Add failure-only recovery that releases publication backpressure, terminates the test subprocess through `internal/procgroup`, and boundedly joins started goroutines before failing. A broken regression must not hold the package until its global timeout.
- Prove the regression red against the old shutdown ordering, then make shutdown release consumers or drainers before closing a resource that may wait for their I/O.
- Repeat the focused test normally and under `-race`, cross-compile the test package for the affected platform, run the repository validation gate, and require real affected-platform CI before completion.
