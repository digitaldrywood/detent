# Machine-local workflow overlays

[Back to README](../README.md#documentation)

Keep `detent.yaml` and `WORKFLOW.md` checked in as separate shared contracts.
For settings that apply on one machine, create schema-versioned
`detent.local.yaml`. For machine-local agent direction, create prose-only
`WORKFLOW.local.md`. Add both local files to the repository's `.gitignore`.
Local configuration wins per structured leaf key; mapping siblings that are
not mentioned remain shared, while local lists and scalar values replace their
shared values. Local Markdown is appended after the shared direction under a
visible machine-local heading.

Detent loads all present definition files at startup. Its watcher reloads
edits, creation, and deletion without a restart, and periodic
reconciliation covers missed filesystem events. `detent doctor` reports an
active overlay, lists its structured override keys, and warns if Git tracks the
local file.

If a changed definition cannot be loaded or validated, Detent keeps the last
good workflow active and marks the project degraded in `/health`. The workflow
source entry records `last_reload_error` and `reload_failed_at`, and
`detent doctor` reports that the project is pinned to its last-good revision.
A later successful reload clears the degraded state.

For example:

```yaml
# detent.local.yaml
schema: 1
tracker:
  assignee: local-operator
polling:
  interval_ms: 90000
```

```markdown
<!-- WORKFLOW.local.md -->
Use the tools installed on this build host.
```

## Adopt an approved policy on an enrolled runner

Cloud approval records an exact descriptor. An existing supplied definition
remains owned by its configured local source; approval does not replace files,
remove overlays, or change runner routing.

1. Pause the project persistently and set the runner to draining. Let active
   sessions, deferred completions and Cloud leases finish through their current
   owners. Preserve the runtime database and unpublished Changes.
2. Place the intended shared `detent.yaml`, `WORKFLOW.md` and applicable
   `AGENTS.md` in the configured definition source. Keep `detent.local.yaml`
   and `WORKFLOW.local.md` in place. Retain host-only settings in the shared
   files as well if they have not been moved to the local overlay. For a local
   source, the workflow path is absolute or home-relative and no Git commit is
   required. For `workflow_ref`, commit the shared files to that configured
   Git ref; local overlay files remain outside Git.
3. Read `local_project_configuration` for that project and runner. This is
   the preview: `selected_policy` identifies the configured candidate,
   `effective_policy` identifies the running definition, and `config_revision`
   identifies the current source including its overlays. Inspect the candidate
   using the official policy CLI and approve that exact descriptor in Cloud.
   A descriptor prepared from a different directory is not a source binding.
   Its identity must match the configured candidate before applying it.
4. Call `apply_local_project_policy` with the current runner revision,
   configuration revision, effective policy ID, approved policy ID and source
   revision. The source revision is the authored definition digest, not a Git
   commit or a value to put in `workflow_ref`. Host-only overlays are supported;
   policy-bearing overlay changes still require matching approval.
5. Read `local_project_configuration` again after processing. `applied` and
   the effective policy identity confirm the runtime result. `saved` is false
   for this operation because the configured source is already durable and
   the owner does not rewrite it. Reloading or restarting uses those same
   files. The project remains paused and runner routing remains draining until
   the operator resumes them explicitly.

Readback includes `last_operation`, the most recent completed configuration
command's receipt from Cloud's existing command store. It retains the request
ID, operation, observed configuration revision, effective policy ID, applied
and saved flags, constraint and observation time across later heartbeats.
Current configuration fields remain current; a historical refusal does not
prevent a new request. Read the receipt's revision and time when the source
has changed since that operation.

If the selected identity differs from the approval, update the configured
shared source or approve the intended configured candidate, then read a fresh
preview. Never delete private overlays, reset enrollment, clear holds, edit
runtime databases or discard Changes to make policy adoption succeed.
