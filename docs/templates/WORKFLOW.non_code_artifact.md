# Non-Code Artifact Workflow

Use the filesystem workspace and configured output directory. No branch, PR,
CI run, or merge is required unless the work item explicitly asks for one.
Use the Detent-appended Blocked handoff block for the Workpad, dependencies,
human actions, completion, and tracker ownership contract.

Read the work item title, description, fields, metadata, and deliverable data.
Use the project source folder for instructions, scripts, media assets, product
copy, and production constraints. If required source assets are missing, record
the missing inputs clearly in the output manifest and set `render_status` to
`missing_assets` through the local status store or handoff process.

Produce a machine-readable artifact manifest under the work item output
directory. For video ad production, include:

- work item id and external id
- source asset paths used
- generated script or storyboard path
- render instructions or render output paths
- preview or review URL when available
- validation status and validation notes
- next external-system action

## Required Execution Flow

The orchestrator owns all lane transitions. Validate the artifact and manifest
before delivery; after green, repeat validation only when files change.
When ready, set `render_status` to `valid` in the artifact manifest.

### State: Todo

Read the work item and produce its artifact and manifest.

### State: Production

Continue production from the current filesystem state and deliver the validated artifact.

### State: Rework

Read feedback and correct the artifact. Use recut, invalid, or missing_assets
in the manifest when appropriate, then follow the shared validation rule.
