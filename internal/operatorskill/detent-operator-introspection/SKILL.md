---
name: detent-operator-introspection
description: Answer read-only operator questions about one issue in a Detent Hub, including its lane, latest transition, active work, eligibility, required gate, and evidence freshness. Use when asked why an issue is stuck, whether it is being worked, why it is not advancing, or how trustworthy Detent's current answer is.
---

# Detent operator introspection

Bundle version: 3

Expected response schema: 3

Select the one question class that matches the request. Call Hub MCP `explain_item` once with the selected project ID and canonical native work-item ID (`wi_...`). The tool returns a bounded explanation object. Inspect the complete response, including any tool error. Do not add filtering commands before inspecting the complete object.

## Decision tree

### Current lane or latest transition

Use for questions such as "Why is this issue in this lane?" Read `current_lane` and `latest_transition`, including `from`, `to`, `reason`, `at`, and `provenance`.

Call once:

Hub MCP `explain_item` arguments:

```json
{"project_id":"<project-id>","reference":"<work-item-id>"}
```

Stop: Report the lane and transition with their observation times when the evidence is available or live.

Escalate: Ask the operator for the project ID or the canonical native work-item ID if either is missing. Escalate with the returned source limitations when no transition evidence supports a conclusion.

### Active work

Use for questions such as "Is this issue actually being worked on?" Read `attempt`, `sessions`, and their status, phase, heartbeat, completion, and source fields. Do not infer active work from the lane alone.

Call once:

Hub MCP `explain_item` arguments:

```json
{"project_id":"<project-id>","reference":"<work-item-id>"}
```

Stop: Report whether the model shows a current attempt or session, and distinguish an absent record from unavailable evidence.

Escalate: Escalate with the exact unavailable or stale source when the returned evidence cannot establish liveness.

### Eligibility or required gate

Use for questions such as "Why is this issue not advancing?" Read `eligibility`, `required_gate`, and their evidence references. Prefer recorded refusal, wait, and gate reasons over inference.

Call once:

Hub MCP `explain_item` arguments:

```json
{"project_id":"<project-id>","reference":"<work-item-id>"}
```

Stop: Report the recorded eligibility and gate states and their reasons when the model supplies them.

Escalate: Escalate with the unknown or unavailable fields when the model does not record a reason. Do not invent fleet policy to explain the gap.

### Evidence confidence

Use for questions such as "How current or trustworthy is this answer?" Read `observed_at`, `current_lane.freshness`, `current_lane.degraded`, `latest_transition.provenance`, `sources`, and `evidence`.

Call once:

Hub MCP `explain_item` arguments:

```json
{"project_id":"<project-id>","reference":"<work-item-id>"}
```

Stop: Treat only `live` evidence as current. Treat `available` as readable, then assess its observation, selection, heartbeat, and completion timestamps before describing it as current. Treat transition attribution as established only when `latest_transition.provenance.trustworthy` is true; otherwise report its `trustworthy_since` boundary and preserve the recorded origin as unverified history. Label `last_known`, `expired`, `unavailable`, or `corrupt` evidence explicitly.

Escalate: Escalate when degraded or missing evidence prevents the requested conclusion. A successful response with degraded fields is still a success, not permission to use another data source.

## Interpret tool outcomes

- Success: Require `schema` to equal 3. Answer from the returned fields and cite degraded or last-known sources.
- Authentication or authorization failure (HTTP 401 or 403): Stop; do not retry anonymously. If the diagnostic identifies `api_token_required`, access requires an operator-configured credential. Escalate for supported read access without attempting to recover credential plaintext.
- Invalid or missing reference: Verify the project ID and canonical native work-item ID. Do not substitute a bare issue number, title, or URL.
- Unavailable runtime or source: Report the returned limitation. A readable historical record does not establish current activity.
- Unreachable service, timeout, unsupported schema, or another tool error: Stop and escalate with the diagnostic. Do not infer issue state from the failure.

## Boundaries

Use only the Hub MCP read above. Never substitute raw database access, dashboard HTML scraping, log searches, plaintext credential recovery, mutating commands, or proposal tools. Keep installation outside repository `.detent/skills` directories; those files are worker-task metadata, not ambient operator skills. Follow any fleet-specific process only from the operator's own workflow documentation.
