# Multi-Project Operation

[Back to README](../README.md#documentation)

A Cloud runner's `global.yaml` holds machine settings only. Cloud supplies
its allowed projects and organization/project model selection. The runner
clones each allowed repository under `workspace_root/PROJECT_ID` and reads
`detent.yaml` and `WORKFLOW.md` from its remote default branch through the
existing repository workflow source. No local project list or
workflow path overrides Cloud routing. An empty allowed-project list runs nothing.

```yaml
apiVersion: detent/v1
kind: GlobalConfig
log_level: info
port: 0
service_name: detent.runner
workspace_root: /absolute/path/to/detent-runner
client:
  hub_url: https://cloud.detent.build/organizations/org_example
  identity_file: /absolute/private/runner/identity.json
  organization_id: org_example
  display_name: Build host
  capacity: 8
  heartbeat_interval_seconds: 30
  lease_ttl_seconds: 90
  provider_capacity_file: /absolute/private/runner/provider-capacity.json
global:
  max_concurrent_agents: 8
  startup:
    jitter_seconds: 10
    max_spawn_per_second: 2
    max_concurrent_starts: 4
  memory:
    max_agent_rss_bytes: 8589934592
    pressure_some_avg60_threshold: 10
    poll_interval_ms: 1000
  io:
    pressure_full_avg10_threshold: 5
    degraded_max_concurrent_agents: 1
    poll_interval_ms: 1000
  cpu:
    pressure_some_avg10_threshold: 80
    degraded_max_concurrent_agents: 1
    poll_interval_ms: 1000
```

`detent hub runner register --workspace-root /absolute/path/to/detent-runner`
writes this machine-only shape and can start the service before checkouts exist.
The runner uses the host's Git authentication to clone linked Cloud repositories.
Allowed-project changes are picked up by the existing configuration reload.
Removed checkouts remain on disk; they no longer run.

Before cloning, the runner discovers existing checkouts under `workspace_root`
by their Git origin. A matching checkout keeps its directory name and runtime
project name, preserving unfinished worktree paths, branches and local attempt
records across the cutover. Set `workspace_root` to the existing checkout parent
when upgrading a host with a custom layout. Multiple matching checkouts require
the operator to select the retained source; the runner preserves them and
reports the project setup failure instead of creating another source checkout.
Missing checkout metadata or a failed clone is an instance-owned setup failure
for that allowed project. Its Cloud grant remains visible in the existing
heartbeat diagnostics; valid projects continue to run.

For one release, runner files containing `projects`, `weight`, `priority`,
`scheduling`, `fair_share`, or `global.agents.model_selection` log a warning
naming each removed key and ignore its value, including malformed legacy
values. `client.native_projects` and other organization/project defaults are
also ignored. Remove these keys before the next release, which will reject
unknown keys at startup. Local-instance settings belong to the local database;
runners do not fall back to file-defined projects or model selection.

Runner owners set the allowed-project list in Cloud; an empty list runs nothing.
Cloud organization settings hold one project rank shared by all runners.
When a slot opens, eligible ready work sorts by issue priority (Urgent, High,
Normal, Low, then unset), project rank, and oldest issue creation time.
Legacy local project weights, priorities, scheduling modes and fair-share
settings do not affect selection. Their configuration compatibility belongs
to the runner configuration migration.

Priority picks the next job (INV-10). Pending requests own no capacity.
Requests that cannot acquire their actual project, lane or host capacity do
not prevent another ready request from using a free slot. Running work is
never cancelled or displaced by priority. A lower-ranked project may wait
indefinitely while higher-ranked ready work exists; dedicated runners limit
competition through their allowed-project lists.

Dispatch
stall age survives restarts and refusal-reason changes by using the later of
the project's last successful selection and its oldest current candidate's
lane-entry time. A newly ready project therefore does not inherit idle time
from an older selection.

Host capacity comes from `client.capacity` and `global.max_concurrent_agents`.
Startup pacing, CPU/memory/IO pressure limits, and provider-capacity reporting
remain machine settings. See [Host pressure admission](config.md#host-pressure-admission)
and [Runner capacity](runner-capacity.md) for their effective limits.

## Running Multiple Instances

Register each runner separately with its own private identity file,
`global.yaml`, workspace root, runtime database, service name and listener
port. Set each runner's allowed projects in Cloud. Cloud owns project rank,
availability and grants; overlapping runners share the same project policy.

## Instance agent defaults and Sol-first selection

Organization settings in Cloud supply the default model selection. Project
settings can override it. The Hub sends the resolved selection to runners;
`global.agents.model_selection` and repository model-selection settings are
ignored with compatibility warnings. Backend commands and routes remain in
the repository's `detent.yaml` and optional `detent.local.yaml`.

The seeded selection uses Codex Sol (`gpt-6.1-sol`) at high effort for normal
and complex implementation, Astra at low for planning and medium for validation,
and Astra at medium for very complex work. Per-issue effort clamps to each
stage's configured ceiling. Configure models in Cloud, not runner files.
