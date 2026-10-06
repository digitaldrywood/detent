# Worked Multi-Project Configuration

[Back to configuration](../../config.md#choose-a-starting-point)

A Cloud runner keeps machine settings in [`global.yaml`](global.yaml).
Cloud owns its allowed projects, project rank and model selection. The runner
clones each allowed repository into `workspace_root/PROJECT_ID` and loads the
repository's `detent.yaml` and `WORKFLOW.md`.

Register the host with `detent hub runner register`, using the enrollment
command from Cloud. Set the private identity path, organization and workspace
root to real values. Keep this file outside source repositories. The five
host slots bound process concurrency; pressure thresholds and startup pacing
also remain host settings.

The example project definitions are
[`orders-api/detent.yaml`](orders-api/detent.yaml) and
[`storefront/detent.yaml`](storefront/detent.yaml). Check the appropriate file
into each repository, with a prompt-only `WORKFLOW.md` beside it. Adapt its
tracker lanes, instructions and acceptance gates to the selected Cloud project.
Backend commands and optional `detent.local.yaml` overrides belong beside the
repository definition, not in the runner file.

Project concurrency caps remain in project definitions. Cloud ranks eligible
work by issue priority, project rank and creation time; local weights and
priorities do not control runner selection. Running work keeps its capacity.

For one release, old runner `projects`, `weight`, `priority`, `scheduling`,
`fair_share`, `client.native_projects` and `global.agents.model_selection`
values produce warnings and are ignored. Remove them before the next release's
unknown-key rejection. See [Multi-project operation](../../multi-project.md).
