# Thread Resume Spike

Issue: digitaldrywood/detent#859

## Recommendation

Enable the prototype for rework redispatches only when the prior completed session and current issue share the same project, pull request number, head SHA, base SHA, backend, model, and role. Keep fresh fallback semantics for missing or expired provider state. The #852 cache telemetry migration is now part of the store, so `agent.experimental_thread_resume` defaults to enabled while remaining an operator opt-out.

## Backend Support

Codex CLI `codex-cli 0.142.5` exposes `thread/resume` in the experimental app-server protocol. The generated v2 schema accepts `threadId` plus the same execution overrides Detent already uses for fresh threads: `cwd`, model, model provider, service tier, approval policy, and sandbox mode. The prototype sends `thread/resume` when a stored provider thread ID exists, then starts the next turn on the resumed thread.

Claude Code `2.1.199` exposes `-r, --resume [value]` for print-mode sessions. The same help output also exposes `--fork-session` and `--no-session-persistence`; the prototype uses only `--resume <session_id>` so continuation behavior stays aligned with the CLI default.

## Lifetime Limits

Neither verified CLI surface publishes a durable TTL in the command help or generated schema. Codex describes `thread/resume` as loading a thread from disk by `threadId` or rejoining a running thread; Claude describes resuming by session ID. Treat stored provider IDs as opportunistic local cache keys:

- Resume only from Detent sessions that completed successfully.
- Resume automatically only for implement workers dispatched from the configured Rework state with an unchanged pull request head and base.
- Force fresh dispatch when the requested model, backend ID, backend kind, or agent role changes.
- Fall back to a fresh thread only when the resume attempt fails before a new turn starts.
- Keep per-issue handoff notes as the durable cross-machine fallback, because provider thread/session IDs can expire, be pruned, or be unavailable on another host.

## Prototype Shape

The migration adds resume metadata to `codex_sessions`: requested model, backend identity, agent role, provider thread/session IDs, and the Detent session ID used as a resume source. Completed work attempts persist their current pull request head and base SHAs. On a Rework redispatch, the runner looks up the newest completed session whose issue, project, pull request fingerprint, backend, requested model, and role all match, passes an `AgentResume` field into the backend request, and clears the source state before retrying fresh if the resumed attempt fails before a turn starts.

With the flag off, Detent does not look up resume state and does not pass resume arguments to either backend. The schema is still extended so the feature can be enabled without another storage change.

## Measurement

The required #852 cache-read columns were not available in the live dogfood store inspectable from this workspace on 2026-07-02. The local store reported goose versions only through 6 and `usage_events` did not contain `cached_input_tokens`, `reasoning_output_tokens`, or `model_context_window`, so no dogfood cache-read fraction delta could be measured from persisted rows.

The issue body records the prior Prometheus audit estimate of a 50-80% input reduction on continuation sessions, but this branch does not independently validate that estimate. Before broad adoption, rerun one implement/rework or implement/merge dogfood issue with:

1. dogfood Detent migrated through #852 telemetry,
2. `agent.experimental_thread_resume` disabled for the baseline dispatch,
3. the same issue resumed with the flag enabled, and
4. cache-read fraction compared from #852's usage report columns.

Successful selection remains measurable through `codex_sessions.resumed_from_session_id`; cached-input share and per-issue lifetime tokens remain available from the same session rows.

## Validation

The prototype is covered by focused tests for:

- Codex app-server sending `thread/resume` before `turn/start`.
- Claude Code adding `--resume <session_id>`.
- Flag-off runner behavior avoiding resume lookup and backend resume args.
- Resume handshake failure falling back to a fresh backend attempt.
- Resumed turn failure not retrying after the turn has started.
- Automatic lookup restricted to Rework redispatches with complete current pull request identity.
- Store lookup selecting only completed sessions matching project, issue, pull request head and base, backend identity, role, and requested model.
