---
name: ready-queue-priority-regressions
description: Diagnose priority inversions hidden by scheduler tests that force acquisition callers to overlap.
when_to_use: A priority scheduler passes concurrent sorting tests but lower-ranked polls take freed capacity ahead of previously ready work.
---

Reproduce with separate submissions: fill real capacity, submit higher and lower
requests sequentially, then release capacity without another poll. Check which
consumer actually starts. Holding a private mutex to batch callers proves the
sort but can hide loss of priority between independent polling loops.

Trace what survives a refused acquisition. A demand snapshot without a consuming
caller cannot dispatch work. Comparing against that snapshot and refusing others
can recreate an idle-capacity reservation. Blocking acquisition inside the owner
event loop can prevent the completions that release capacity.

When the intended contract requires pending executable requests, test the request
lifetime through the real owner loop. Use a long poll interval and observe starts
and completion-driven releases. Verify state requests and shutdown still complete
while work is queued. Pending requests must own no capacity; grants must obey
existing project, lane, and host ceilings before delivery.

Cover snapshot invalidation as well as ordering: retry attempt metadata across
refresh, a removed lane, and a sparse tracker read for a PR-backed candidate.
Fresh tracker fields must remain authoritative; preserve missing PR identity and
use existing hydration to refresh its head/checks. Cancel pending requests before
draining delivered grants so cleanup cannot start another obsolete request.
