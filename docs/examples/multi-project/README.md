# Worked Multi-Project Configuration

[Back to configuration](../../config.md#choose-a-starting-point)

This example shows the same two-layer arrangement Detent uses to orchestrate
its own development: one machine-local `global.yaml` registers every project,
while each repository owns a portable `detent.yaml` and `WORKFLOW.md` contract.
The repositories, paths, host name, and colors here are intentionally
fictitious; no operator identity or private infrastructure is required to
understand the choices.

The example assumes this host layout:

```text
/srv/detent/
├── config/global.yaml
├── repos/
│   ├── orders-api/
│   │   ├── detent.yaml
│   │   └── WORKFLOW.md
│   └── storefront/
│       ├── detent.yaml
│       └── WORKFLOW.md
└── worktrees/
    ├── orders-api/
    └── storefront/
```

The files in this directory map to that layout as follows:

| Example file | Installed location |
| --- | --- |
| [`global.yaml`](global.yaml) | `/srv/detent/config/global.yaml` |
| [`orders-api/detent.yaml`](orders-api/detent.yaml) | `/srv/detent/repos/orders-api/detent.yaml` |
| [`storefront/detent.yaml`](storefront/detent.yaml) | `/srv/detent/repos/storefront/detent.yaml` |

Copy a maintained label-mode
[`WORKFLOW.md`](../../templates/WORKFLOW.label.md) beside each project config,
then adapt its instructions and acceptance gates to that repository. Keep
`global.yaml` outside the source repositories because it contains host-local
paths and credentials. Check each `detent.yaml` into its project repository so
the orchestration policy changes with the code it governs.

## Why the host config looks this way

- `github_token: gh` reuses the authenticated GitHub CLI credential instead of
  storing a token in YAML.
- Five host slots bound total process concurrency. Project caps divide that
  capacity into three API slots and two storefront slots, so either repository
  can stay responsive without exhausting the host.
- Weighted scheduling gives the API twice the dispatch share when both queues
  are ready. Equal priority keeps that weight meaningful; lower numeric
  priority can still be assigned temporarily with `detent promote`.
- Source checkouts and generated worktrees have separate roots. Agents work in
  isolated worktrees and never mutate the long-lived source checkout.
- Each project has its own color for dashboard scanning, but IDs remain the
  primary identifier.

## Why the project configs differ

Both projects use repository labels as their status source, auto-provision the
expected labels, serialize the merge lane, require `make check`, and keep the
dashboard on loopback. The API may run three agents concurrently because its
queue receives the larger scheduling share; the storefront is capped at two.
Everything else stays intentionally aligned so operators can reason about one
workflow across both repositories.

The files include every setting needed for this deployment model. Optional
subsystems such as intake, scheduled routines, model routing, budgets, and
telemetry remain omitted until the operator has a reason to enable them. See
the [complete configuration reference](../../config.md) before adding one.

After replacing the fictitious repositories and service paths, verify the
resolved installation before starting the server:

```sh
detent --config /srv/detent/config/global.yaml doctor --allow-write-probes
```
