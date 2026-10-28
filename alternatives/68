# Dependency workflows

[Back to README](../README.md#documentation)

This page describes how Detent decides that work is waiting on another issue, on a person, or on nothing at all.

## Hub dependency relations

Native Cloud projects record dependencies as Hub relations, created with `detent hub issue dependency`, the `set_dependency` MCP tool, `POST /api/v1/work-items/{id}/dependencies` or the v2 `POST /api/v2/organizations/{org}/projects/{project}/work-items/{item}/dependencies` route. The Hub offers a `Todo` item to runners only when every blocker it depends on is in a terminal workflow state. The item stays in `Todo` while it waits; it does not move to `Blocked`.

Each project has a `require_dependencies` setting that defaults to true. Setting it false disables dependency gating for that project. For native projects the Hub relation is the only dependency source: `Depends on:` or `Blocked by:` lines in an issue body are not read.

## Body dependency lines (GitHub-tracked projects)

For projects with `tracker.kind: github`, Detent also reads a machine-readable dependency line such as `Depends on: #123` or `Blocked by: owner/repo#123` in the issue description and merges it with GitHub's native blocked-by relations. Declarations read only the leading comma/and-separated reference list (legacy semicolon separators also work). The first sentence terminator (`.`, `!`, or `?`), other prose, or the end of the line ends the declaration. Periods inside repository names and URLs remain part of the reference. `none`, `n/a`, and `-` explicitly declare no dependencies, even when issue references follow. For example, `Depends on: none. Order: #2, #3` declares no blockers, while `Depends on: #2 and #3 — see #9 for context` declares only #2 and #3. Fenced examples never add blockers.

## Human action and Blocked

`Blocked` holds only work that waits on a person: a Workpad `detent-status` block with `status: blocked` and a concrete `human_action` (a question or a permission request), or an explicit operator stop. The board shows such a card as "Needs you". Machine failures, refused or unaccepted versions and infrastructure failures never enter `Blocked`; they return to a dispatchable lane through the retry owner with capped backoff, and infrastructure failures stay attributed to the instance.

A human action is cleared by an authorized project member posting a **newer** `## Codex Workpad` comment whose structured block clears `human_action`, for example:

```detent-status
schema: 1
status: in_progress
blockers: []
human_action: null
```

Detent recognizes an authorized Workpad update made after the Blocked entry and returns the issue to its recorded executable lane. An ordinary reply does not clear the Workpad. The orchestrator is the only writer of lane state.

On every recovery read, an item in `Blocked` without a current human action or operator stop is treated as a machine failure and sent back through the retry owner. Historical reason codes, fingerprints and retry counts do not hold it in `Blocked`.

## Typed Workpad blockers

Workpad blockers may declare live evidence. Each typed blocker names an `owner`, a `predicate`, and a `recheck_interval` or `expires_at`. The supported predicate types are `issue_state`, `pull_request_state`, `check_presence`, `budget_capacity`, and `config_fingerprint`.

```detent-status
schema: 1
status: blocked
blockers:
  - ref: "example-org/example-project#123"
    reason: "waiting for the dependency to close"
    owner: orchestrator
    predicate:
      type: issue_state
      states: [open]
    recheck_interval: tick
human_action: null
```

During blocked recovery, orchestrator-owned `issue_state` blockers defer recovery while their predicate holds. Free-text blocker entries remain valid for compatibility but are unverifiable, and an expired predicate that still holds is also unverifiable; neither keeps work in `Blocked` without a human action.

## Tracker dependency settings

These `tracker` settings remain in the configuration schema (see [Configuration](config.md)):

- `tracker.dependency_auto_unblock` moves issues with explicit dependency references out of its `source_states` (default `Blocked`) to `target_state` (default `Todo`) once every blocker satisfies the `readiness` rule, and posts an audit comment. Enabling it requires `Rework` in the active states.
- `tracker.blocker_auto_promote` promotes resolved same-repository blockers of a dependency-waiting issue out of `blocker_states` (default `Backlog`, `Blocked`, `Human Review`) to `target_state` (default `Todo`), within current local agent capacity.
- `tracker.blocked_recovery` routes items carrying a persisted `reason_code` of `merge_conflict`, `stale_base` or `missing_current_head_ci` to its `target_state` (default `Rework`), which must be an active state when enabled. It reads reason codes from older Workpads for compatibility.

## Runner host process limits

On Linux hosts that run the runner as a systemd user service, the service PATH must resolve every command used by project hooks and validation gates; `detent doctor` checks Detent's direct dependencies, not repository-specific bootstrap tools.

Detent starts each Codex and Claude worker in its own process group without leaving the service cgroup, so its persisted worker registry can terminate stale process groups on shutdown or startup while systemd's default `KillMode=control-group` remains the final cleanup backstop. Launchers that double-fork or move children into another cgroup defeat that cleanup.

`MemoryHigh` and `MemoryMax` on `detent.runner.service` add host-level protection on shared machines. Both cover the aggregate orchestrator and worker footprint, and `MemoryMax` must be above `MemoryHigh`. For example, a 32 GiB host reserved partly for other work could use this user-service drop-in:

```ini
[Service]
MemoryHigh=24G
MemoryMax=28G
```

Create the drop-in with `systemctl --user edit detent.runner.service`, run `systemctl --user daemon-reload`, and restart the service. `systemctl --user show detent.runner.service -p MemoryHigh -p MemoryMax` shows the effective values. These cgroup limits do not replace the per-agent RSS ceiling or the memory, IO, and CPU pressure admission controls.
