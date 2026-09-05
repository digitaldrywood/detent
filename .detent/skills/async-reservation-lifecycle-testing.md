---
name: async-reservation-lifecycle-testing
description: Verify bounded scheduler reservations across asynchronous worker completion, remote validation, filtered tracker snapshots, and restart.
when_to_use: Use when a scheduler releases worker capacity during a remote wait but must retain exclusive ownership of a shared resource across retries.
---

# Asynchronous reservation lifecycle testing

- Separate the worker slot and issue claim from the resource reservation. Assert that waits release execution capacity while competing users of the same resource remain deferred.
- Drive a deterministic event sequence with an injected clock: select, mutate the remote head, fetch tracker state, process worker completion, finish validation, complete the item, and fetch again. Include the ordering where the remote mutation becomes visible before worker completion.
- Make test fetches apply the production state filters. Completed or withdrawn items must disappear from the candidate stream; retaining every fixture item can hide a reservation that never releases.
- Use recorded completion state or an explicit terminal cleanup boundary when an owner disappears from fetched candidates. Distinguish that from a temporary missing or degraded tracker response.
- Keep the reservation deadline fixed across retries and worker-owned identity changes. Test expiry at equality, repeated refreshes, external identity changes, and successful admission of the next contender after release.
- Test restart through the actual completion metadata writer and recovery reader. Reject expired, malformed, superseded, or mismatched records; restoration must preserve the original deadline.
- Include independent resources and runnable implementation work in dispatch assertions, and require failed checks or conflicts to prevent the protected action.
- Record both prior and current resource identities with the reason for retaining or releasing ownership so runtime history can distinguish required refreshes from avoidable invalidation.
