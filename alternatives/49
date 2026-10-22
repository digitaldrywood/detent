---
name: trusted-exact-head-audits
description: Build independent model-review gates whose immutable evidence is bound to the exact live pull request base and head without executing audited code.
when_to_use: Use when a Detent completion or merge gate must trust a security, policy, or compliance review that the implementation agent and audited pull request cannot forge or weaken.
---

# Bind trusted reviews to immutable pull request state

- Keep the operative reviewer prompt and its versioned digest in trusted Detent code or another immutable installation source, outside the audited repository head.
- Collect bounded issue and pull request metadata plus GitHub's raw `application/vnd.github.diff` response. Do not rely on per-file `patch` fields because GitHub can silently truncate them.
- Read at most the configured diff limit plus one byte and fail closed on empty or truncated content. Fetch pull request metadata before and after the diff and reject any base or head change.
- Launch a fresh reviewer process in an empty Detent-owned workspace. Verify ChatGPT subscription authentication inside that same process before starting the turn, clear metered API and forge credentials, use a read-only sandbox, expose no dynamic tools, and reject the run if any tool event occurs.
- Persist append-only evidence keyed by project, repository, pull request, base SHA, and head SHA. Include trusted service identity, reviewer version and digest, authentication mode, process and provider identities, exit status, output digest, parsed findings, attempts, and timestamps.
- Treat comments, Workpad text, and agent-authored fields as display-only evidence. Resolve findings only through append-only dispositions attributed to the trusted service identity with concrete supporting evidence.
- Refresh the live pull request base and head independently at completion and immediately before merge. Require an atomic expected-head merge guard; route away from native merge queues that cannot bind enqueue to the audited SHA.
- Test missing, stale, failed, empty, metered-auth, tool-use, malformed-output, identity-missing, oversized-diff, moving-head, forged-comment, forged-disposition, and unresolved-finding cases as fail-closed paths.
