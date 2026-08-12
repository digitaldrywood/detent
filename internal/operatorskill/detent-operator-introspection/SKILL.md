---
name: detent-operator-introspection
description: Answer read-only operator questions about one issue in a running Detent instance, including its lane, latest transition, active work, eligibility, required gate, and evidence freshness. Use when asked why an issue is stuck, whether it is being worked, why it is not advancing, or how trustworthy Detent's current answer is.
---

# Detent operator introspection

Bundle version: 2

Expected response schema: 2

Select the one question class that matches the request. Make its call once. The command returns one JSON object and enforces a request timeout and response-size limit. Keep diagnostics on stderr. Do not add filtering commands before inspecting the complete object.

## Decision tree

### Current lane or latest transition

Use for questions such as "Why is this issue in this lane?" Read `current_lane` and `latest_transition`, including `from`, `to`, `reason`, `at`, and `provenance`.

Call once:

```sh
detent --format json issue '<issue-ref>' --explain --project '<project-id>'
```

Stop: Report the lane and transition with their observation times when the evidence is available or live.

Escalate: Ask the operator for the project ID or an unambiguous issue reference if either is missing. Escalate with the returned source limitations when no transition evidence supports a conclusion.

### Active work

Use for questions such as "Is this issue actually being worked on?" Read `attempt`, `sessions`, and their status, phase, heartbeat, completion, and source fields. Do not infer active work from the lane alone.

Call once:

```sh
detent --format json issue '<issue-ref>' --explain --project '<project-id>'
```

Stop: Report whether the model shows a current attempt or session, and distinguish an absent record from unavailable evidence.

Escalate: Escalate with the exact unavailable or stale source when the returned evidence cannot establish liveness.

### Eligibility or required gate

Use for questions such as "Why is this issue not advancing?" Read `eligibility`, `required_gate`, and their evidence references. Prefer recorded refusal, wait, and gate reasons over inference.

Call once:

```sh
detent --format json issue '<issue-ref>' --explain --project '<project-id>'
```

Stop: Report the recorded eligibility and gate states and their reasons when the model supplies them.

Escalate: Escalate with the unknown or unavailable fields when the model does not record a reason. Do not invent fleet policy to explain the gap.

### Evidence confidence

Use for questions such as "How current or trustworthy is this answer?" Read `observed_at`, `current_lane.freshness`, `current_lane.degraded`, `latest_transition.provenance`, `sources`, and `evidence`.

Call once:

```sh
detent --format json issue '<issue-ref>' --explain --project '<project-id>'
```

Stop: Treat only `live` evidence as current. Treat `available` as readable, then assess its observation, selection, heartbeat, and completion timestamps before describing it as current. Treat transition attribution as established only when `latest_transition.provenance.trustworthy` is true; otherwise report its `trustworthy_since` boundary and preserve the recorded origin as unverified history. Label `last_known`, `expired`, `unavailable`, or `corrupt` evidence explicitly.

Escalate: Escalate when degraded or missing evidence prevents the requested conclusion. A successful response with degraded fields is still a success, not permission to use another data source.

## Interpret command outcomes

- Success: Require `schema` to equal 2. Answer from the returned fields and cite degraded or last-known sources.
- `dashboard_unauthorized` (HTTP 401): A supplied credential is invalid or expired. Stop; do not retry anonymously. Escalate for a valid supported read credential.
- `dashboard_forbidden` (HTTP 403): The request is understood but not allowed. If the diagnostic identifies `api_token_required`, API access requires an operator-configured credential; this is not a missing issue. Otherwise the credential lacks the required read access. Stop and escalate without attempting to recover credential plaintext.
- `ambiguous_reference`: The reference matches more than one issue. Stop and ask for an issue ID, canonical identifier, or full issue URL in the selected project.
- `issue_not_found`: No issue matches the reference in the selected project. Stop and ask the operator to verify both values.
- `runtime_unavailable`: The running service cannot currently build the explanation model. Stop this investigation; retry later, or escalate if an immediate answer is required.
- `dashboard_unreachable` or `dashboard_timeout`: The service is stopped, unreachable, or unresponsive. Stop and escalate with the diagnostic.
- `unsupported_model_version`: The CLI and running service do not share this response model. Stop and escalate for an upgrade or restart.
- `dashboard_request_failed`: The service returned an unclassified failure. Stop and escalate with the diagnostic.

## Boundaries

Use only the read command above. Never substitute raw database access, dashboard HTML scraping, log searches, plaintext credential recovery, mutating commands, or proposal tools. Keep installation outside repository `.detent/skills` directories; those files are worker-task metadata, not ambient operator skills. Follow any fleet-specific process only from the operator's own workflow documentation.
