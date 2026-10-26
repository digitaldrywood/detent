---
name: react-http-test-lifecycle
description: Diagnose delayed React HTTP assertions and cascading failures from fetch spies or held reads.
when_to_use: A routed component test passes intermittently, then later HTTP scenarios hang or recursively call a fetch mock.
---

# Diagnose the first failure and its cleanup separately

1. Preserve the reported request sequence and response assertions. Trace which
   independent read actually supplies the missing UI state; a successful setup
   POST does not mean routing or the page's GET has completed.
2. Reproduce slow delivery at that read, forwarding through the real transport.
   A temporary delay can establish whether the default DOM deadline explains
   the first failure. Remove diagnostic delays from the shipped test.
3. Inspect spies captured as a native transport. If an earlier failure leaves a
   spy installed, another `spyOn` may reuse that same function: capturing it as
   `realFetch` before changing its implementation creates recursion.
4. Restore spies and release promise barriers in teardown, including assertion
   failure paths. Unmount first so late responses cannot update a mounted tree.
5. Await router initialization, use a bounded local integration assertion wait,
   and cover deferred delivery with request acknowledgment and explicit release.
   Assert absence before release and the full behavior afterward; keep unrelated
   reads independent when the scenario requires one of them to remain held.
6. Temporarily inject assertion failures both after installing a spy and while a
   read is held. Verify later scenarios still pass. Restore the source in `finally`,
   then run the final focused file in shuffled orders.

Keep transport/RPC timeout diagnoses separate unless reproduced. A component
deadline and a worker RPC timeout in one log do not establish a shared cause.
