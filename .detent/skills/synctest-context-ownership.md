---
name: synctest-context-ownership
aliases:
  - synctest-mutex-join-testing
description: "Diagnose virtual-time hangs caused by context ownership or mutex contention during cancellation."
when_to_use: "Use when a testing/synctest retry or timeout test stops advancing fake time and a goroutine dump shows a select without the durable-blocking annotation. Also use for synctest mutex join testing."
---

# Keep cancellation inside the virtual-time scope

- Capture the isolated test process's goroutine dump and locate the timer/cancellation select. A plain `select` state inside a synctest bubble, while the test runner waits in `synctest.Run (durable)`, indicates a wait that the fake clock cannot treat as durably blocked.
- Check where every selected channel was created, including the context's `Done` channel. An outer test's `t.Context()` can leave the select dependent on a channel outside the bubble and prevent fake time from advancing.
- Use the inner `*testing.T` passed to `synctest.Test` to create `t.Context()` and any timeout or cancellation context consumed by the timer loop. Keep the application's cancellation behavior intact.
- Keep external fixtures such as SQLite stores under their owning test's cleanup. Pass a context created inside the bubble to the operation whose timers are being tested; do not assume the fixture's setup context is suitable for that operation.
- Verify retry duration and cancellation with fake time, then repeat focused race tests. Test the real persisted state separately so the virtual-time harness does not replace the storage contract.

## Synctest Mutex Join Testing

Use this case when a testing/synctest test waits for quiescence while a shutdown or retirement goroutine is blocked acquiring a mutex held by a deliberately stalled operation.

- Inspect the isolated test's goroutine dump. A test in `synctest.Wait`, a fake operation blocked on a channel, and a retirement goroutine in `sync.Mutex.Lock` identify a harness cycle: mutex contention is not a durable block that lets synctest declare quiescence.
- Keep channel barriers at the actual external operation. Wait for its entry, inspect the observable state, then release it or cancel its context before joining retirement.
- Do not call `synctest.Wait` while intentionally retaining that mutex cycle. Do not replace it with sleeps or change production locking solely to accommodate virtual time.
- For a negative join assertion, check that retirement has not completed while the external operation is held; then release or cancel the operation and await both its result and retirement's completion.
- Verify successful completion and cancellation as separate cases, and run the focused cases under the race detector.
