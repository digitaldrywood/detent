# Dependency workflows

[Back to README](../README.md#documentation)

Use Todo for genuine software dependency waiting and preserve existing PR phases.

## Human questions

Ask clarification and approval questions in normal comments on the original
issue. Never create synthetic human-prerequisite issues, labels, or dependency
edges. The old `ensure_human_prerequisite` mutation is disabled, including for
existing issues. Intentional standalone human tasks remain non-executable.

Finish independent technical work, stub-testable implementation, and authorized
fallbacks first. Use `ask_human_question` with a stable `key` and a readable
`question`: explain the concrete blocker, researched recommendation, practical
alternatives, and exact decision. Keep the current lane and PR intact and leave
the Workpad `in_progress`. Detent records the question and comment IDs in SQLite
and releases the worker without completing the issue.

An authorized ordinary-language reply resumes interpretation by the worker.
Ambiguous replies require a focused follow-up in the same thread with a new
key. No special command, YAML, completion evidence, or issue closure is required.
A reply is never blanket approval: preserve action-specific authorization for
live sends, publishing, deployment, charges, and destructive operations.

Only one outstanding question can be reserved per issue. Posting intents are
persisted before GitHub writes; retries reconcile a stable comment marker. An
explicitly rejected post releases its unposted reservation so a retry can ask
the question, including after restart. Confirmed comments are never released.
An uncertain post is held for reconciliation rather than blindly reposted and
is reported as a posting error, not as waiting for a human reply. Separate
failure and operational parks are never cleared by the question mechanism.

### Migrate a generated question

After upgrading Detent, use the authenticated project-write endpoint
`POST /api/v1/projects/{project_id}/human-questions/migrate` with a JSON body:

```json
{
  "project_id": "<configured project ID>",
  "dependent": "digitaldrywood/digitaldrywood#647",
  "source": "digitaldrywood/digitaldrywood#685",
  "source_body_sha256": "e09e09f2c8c2548f9efc44bb4ea8dc8d906f481081fa9da1e4997a8307bbbd90",
  "previously_generated_question": true,
  "question": "Newsletter delivery remains deferred and does not block completed #647. For future delivery work, should we use a human-operated Sender step with final content and consent-checked recipient review before each send? The investigation found that the ID-only send endpoint cannot pin the approved payload. I recommend a reviewed manual handoff; the alternative is to keep delivery unavailable until content and audience can be pinned. This decision does not authorize any actual send or reopen #647."
}
```

The hash above selects #685 as inspected on September 9, 2026. Re-read the source
and obtain its exact current body hash if it changes; do not automatically
accept drift. Select only generated clarification/approval issues explicitly
approved for migration, never intentional standalone human work or genuine
software dependencies. Repeat for every original dependent of a selected source.

The concrete #647 issue was subsequently completed with delivery deferred and
the #685 dependency removed. `previously_generated_question` explicitly attests
to that previously generated question when no dependency remains. It defaults
to false; never use it for intentional standalone human tasks. Migration may
post on a closed original issue but never reopens it or creates a new dependency.

The endpoint reserves the internal wait and posts the readable question with
source context before removing the selected body/native dependency and Workpad
blocker. It retains unrelated blockers, PR state, and historical text. The source
is retired as **not planned**, with a superseded explanation, only after no
remaining body/native dependents exist. Its contract and history remain intact;
no completion evidence is invented. Retries reuse the persisted question. A
partial failure is safe to retry with the same request. An uncertain comment
post remains held for reconciliation without generating a second question.

Migration must go through the running service that owns SQLite; do not open or
edit its database from a separate process. The endpoint does not restart Detent.
Existing breaker parks remain subject to their own recovery rules.

## Dependency configuration

Workpad blockers may declare live evidence directly. Each typed blocker names
an `owner`, a `predicate`, and a `recheck_interval` or `expires_at`. Detent
re-evaluates these records on every blocked-lane tick and clears the park when
all predicates are false. The supported predicate types are `issue_state`,
`pull_request_state`, `check_presence`, `budget_capacity`, and
`config_fingerprint`.

```detent-status
schema: 1
status: blocked
blockers:
  - ref: "owner/repo#123"
    reason: "waiting for the dependency to close"
    owner: orchestrator
    predicate:
      type: issue_state
      states: [open]
    recheck_interval: tick
human_action: null
```

Free-text blocker entries remain valid for compatibility, but Detent marks
them unverifiable, surfaces their owner and age, and never clears them
automatically. An expired predicate that still holds is also unverifiable;
orchestrator-owned unverifiable parks are escalated for operator attention.

- **Keep the issue in `Todo`.** Add a machine-readable dependency line such as
  `Depends on: #123`, `Blocked by: owner/repo#123`, or
  `Depends on: https://github.com/owner/repo/issues/123`. Detent keeps the
  issue out of dispatch while any referenced blocker is non-terminal, then
  dispatches it normally after blockers clear. This is the default behavior and
  needs no extra configuration.
- **Legacy dependency waiting in `Blocked`.** Enable `tracker.dependency_auto_unblock` when
  your team wants dependency-waiting issues to sit in a waiting column. Detent
  only moves issues that have explicit `Depends on:` or `Blocked by:` references.
  When all blockers are terminal, closed, or have a merged linked PR under the
  configured `readiness` rule, Detent updates the configured GitHub status
  source to `target_state` and posts an audit comment. Without
  `tracker.dependency_auto_unblock.enabled: true`, a `Blocked` issue is observed
  for display but will not be moved back to `Todo`. Human blockers without
  explicit dependency references stay blocked.
- **Queue fixable blockers automatically.** Enable
  `tracker.blocker_auto_promote` alongside dependency auto-unblock when a
  dependency-waiting issue should pull same-repository blockers out of inactive
  states such as `Backlog`, `Blocked`, or `Human Review`. Detent promotes only
  resolved, same-repository blockers to the configured `target_state`, respects
  current local agent capacity, and posts an audit comment on the promoted
  blocker. Agents that move work to `Blocked` because of another tracked issue
  or pull request must ensure the issue body contains a machine-readable
  `Blocked by:` or `Depends on:` line; Workpad mentions alone are not a durable
  dependency contract.
- **Recover explicit PR-maintenance parks.** Enable `tracker.blocked_recovery`
  only when structured PR-maintenance parks should move work to the configured
  repair lane. The worker that intentionally creates such a park must set
  `reason_code: merge_conflict`, `stale_base`, or `missing_current_head_ci` in
  its blocked `detent-status` block; Detent persists that code on the lane
  entry. Recovery also requires the corresponding PR condition to still hold
  and a new diff-fingerprint/base-OID pair. Issue descriptions, manual status
  moves, and other prose do not authorize recovery. Keep this disabled on
  boards where `Blocked` is also used for deliberate operator parking.

Before you dispatch anything, run **`detent doctor --allow-write-probes`** after
mutation authorization — it checks config
resolution, the database, the `codex` binary, GitHub auth mode, configured
tracker access, repository issue/PR access, required write proofs, rate-limit
visibility, git, and the server port. A clean pre-start `doctor` clears
Detent's direct preflight.
When a running Detent process already owns the configured port,
`detent doctor --port 0 --allow-write-probes` validates the config, database,
tools, token, and write proofs without treating the live listener as a blocker;
pair it with `/health` on the actual service before dispatching more work. Do
not dispatch from a failed doctor run unless the only failure is that expected
live-port collision and `/health` is green. If Detent runs under a systemd user
service, also verify the
service PATH resolves every command used by project hooks and validation gates;
`doctor` checks Detent's direct dependencies, not repo-specific bootstrap tools.
The onboarding runbook includes the service-context check.

Keep systemd's default `KillMode=control-group` for the Detent service. Detent
starts each Codex and Claude worker in its own process group without leaving the
service cgroup, so its persisted worker registry can terminate stale process
groups on shutdown or startup while systemd remains the final cleanup backstop.
Do not wrap worker commands in launchers that double-fork or explicitly move
children into another cgroup.

On shared Linux hosts, add `MemoryHigh` and `MemoryMax` to `detent.service` as
defense in depth. Size both for the aggregate orchestrator and worker footprint,
leave capacity for unrelated workloads, and keep `MemoryMax` above
`MemoryHigh`. For example, a 32 GiB host reserved partly for other work could
use this user-service drop-in as a starting point:

```ini
[Service]
MemoryHigh=24G
MemoryMax=28G
```

Create the drop-in with `systemctl --user edit detent.service`, then run
`systemctl --user daemon-reload` and restart the service during an approved
maintenance window. Verify the effective values with
`systemctl --user show detent.service -p MemoryHigh -p MemoryMax`. These cgroup
limits protect the host if Detent's per-agent RSS ceiling fails; they do not
replace the per-agent ceiling or memory, IO, and CPU pressure admission
controls.
