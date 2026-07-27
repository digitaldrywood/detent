---
name: activity-based-remote-liveness
description: Prevent healthy paginated remote checks from exceeding an aggregate inactivity timeout while preserving deterministic partial results.
when_to_use: Use when a bounded logical sample still requires a full remote scan, individual requests are healthy, and the aggregate check falsely reaches its liveness deadline.
---

# Track remote activity without masking stalls

- Confirm the timeout wraps a multi-request operation and identify whether its bounded result still requires exhausting remote pages.
- Treat the timeout as an inactivity budget. Renew it only after concrete forward progress, such as fully reading a remote response; never use an unconditional heartbeat.
- Carry the reporter through the request context so only the active operation can renew its deadline and connector factories remain free of mutable callback state.
- Keep liveness pulses separate from result publication. Snapshot completed results and the current stage atomically at named boundaries, then freeze that snapshot before cancellation.
- Reject result publications after freeze so cancellation-aware work cannot move the reported stage or append cancellation artifacts past the timeout boundary.
- Test the two behaviors independently: a multi-response operation whose total runtime exceeds the timeout must complete, while a stalled operation must return completed prior results followed by the synthesized timeout failure.
- Repeat focused tests under the race detector, then run the repository validation gate.
