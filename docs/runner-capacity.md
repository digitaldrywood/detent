# Effective enrolled runner capacity

An operator with current runner administration grants can use
`get_runner_capacity` and `update_runner_capacity`. These tools use the
same application owner as fleet routing capacity edits and the native API:

- `GET /api/v2/organizations/{organization}/runners/{runner}/capacity?backend=codex`
- `PUT /api/v2/organizations/{organization}/runners/{runner}/capacity`

Read first. The update takes `runner_id`, a stable `request_id`, and a
bounded `change` containing `expected_revision`,
`expected_config_revision`, `capacity` (1–10000), and optional `backend`.
The API body contains those change fields plus `idempotency_key`.
MCP mutations retain the existing authenticated approval conversation.
The read returns the configuration revision in `applied.revision`;
absent current evidence requires reconnecting or upgrading the enrolled runner.

The existing heartbeat delivers intent to that runner. Its selected global
configuration owner validates the enrolled identity and file revision, changes
only `global.max_concurrent_agents` and `client.capacity`, and uses the normal
configuration reload. It reports the runtime limit separately until reload
finishes. A repeat of the same request returns its original receipt; use a new
capacity read for current application evidence.

Results separate `desired`, `applied`, `effective`, `status`, and attributed
`limits` with observation times. `effective` is the runner configuration
ceiling, not a promise that a project can launch that many jobs. Provider
availability is reported separately; unknown quota does not become available quota. Project, pool,
lane, policy, provider quota and plan admission still govern real claims.
Unknown or stale evidence yields a null effective value. Select a backend when
multiple providers are reported; multiple account reports for the selected backend
also leave the combined ceiling unknown because their capacities are not interchangeable.

The host ceiling is changed through the existing host control, with its own
revision and authority. Increasing the Cloud runner ceiling does not override it.
Detent currently reads configured external provider-capacity report files and
does not own their producers. A lower provider ceiling therefore produces a
partial result with its backend/account limit and an instruction to update the
producer configuration. Editing a generated report is ineffective and the
capacity operation never does so.

Fresh reports for the same account and model can raise the ceiling while earlier
reservations remain occupied and retain their original snapshots. An unmatched
or unreported account keeps the existing conservative reservation ceiling.
No request cancels leases, rewrites reviewed heads, changes model selection,
expands project access, consumes coding allowance, or pauses the instance.
