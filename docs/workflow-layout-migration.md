# Workflow Layout Migration

Detent project definitions use four files with separate ownership:

- `detent.yaml` is the checked-in, schema-versioned Detent machine contract.
- `WORKFLOW.md` is checked-in, portable agent instruction prose.
- `detent.local.yaml` is an optional, gitignored machine-local config overlay.
- `WORKFLOW.local.md` is an optional, gitignored machine-local prose overlay.

The configured `projects[].workflow` path remains the project-definition
anchor. Detent resolves the other files beside that path, so an explicitly
configured external definition directory is supported without requiring copies
in the source worktree.

## Diagnose

Run:

```sh
detent doctor --project <project-id> --port 0
```

Doctor reports the active definition layout and revision in human and JSON
output:

- `split`: `detent.yaml` is authoritative and `WORKFLOW.md` is prose-only.
- `legacy`: Detent configuration still lives in `WORKFLOW.md` frontmatter.
- `mixed`: more than one structured source claims authority.
- `incomplete`: prompt prose exists but the machine contract is missing.
- `stale`: the running process does not match the configured layout or source
  hash.

Legacy findings list the structured keys and include a copy-paste migration
command. Mixed, invalid YAML, unsupported schema versions, missing files, and
invalid effective configuration are failures that must be resolved before
dispatch.

## Preview And Apply

Preview the exact file operations and semantic result without writing:

```sh
detent fix workflow-layout --workflow /absolute/definition/root/WORKFLOW.md --dry-run
```

Apply with the default interactive confirmation:

```sh
detent fix workflow-layout --workflow /absolute/definition/root/WORKFLOW.md
```

For an explicitly confirmed non-interactive operation:

```sh
detent fix workflow-layout --workflow /absolute/definition/root/WORKFLOW.md --yes
```

The fixer resolves the global config through the same path rules as normal
startup: `--config`, `CONFIG`, `CONFIG_HOME`, the deprecated `DETENT_CONFIG`
and `DETENT_HOME` fallbacks, then the native user config directory or legacy
`~/.detent/global.yaml`. For a GitHub project that does not declare a usable
project-local token or GitHub App credential, authentication resolves in
runtime order from `GITHUB_TOKEN` and then the top-level `github_token` value.
The `gh`, `gh-auth`, `${gh auth token}`, and `$(gh auth token)` values invoke
`gh auth token` exactly as they do during startup and `detent doctor`.

The repair validates the legacy effective configuration before staging any
write. It moves shared structured configuration into schema version 1
`detent.yaml` and leaves the `WORKFLOW.md` body byte-for-byte unchanged.
Structured local overrides move to `detent.local.yaml` while local prose stays
in `WORKFLOW.local.md`. An empty local override does not create
`detent.local.yaml`.

The migration preserves file permissions and line endings. Files are staged in
the definition directory and installed atomically as one rollback-protected
operation. Existing conflicting destinations, validation failures, concurrent
source edits, or write/rename failures leave the original definition in place.
After installation, Detent reloads and validates the split definition and
proves its effective configuration matches the legacy definition. Repeating the
command after success reports a no-op.

Environment-variable references remain references throughout diagnosis,
preview, migration, and equivalence checking. Detent does not resolve or print
secret values during this workflow. A resolved runtime token is used only to
validate the legacy, proposed, and installed effective configurations; it is
never added to `detent.yaml`, `detent.local.yaml`, or either workflow file.

## Local Overlays

Add both optional local files to `.gitignore`:

```gitignore
/WORKFLOW.local.md
/detent.local.yaml
```

Doctor warns when either local overlay is tracked. A split project that still
contains structured `WORKFLOW.local.md` frontmatter is reported as mixed; run
the surfaced fix command to move only those overrides into
`detent.local.yaml`.

## Git-Ref Definitions

When `projects[].workflow_ref` is configured, Detent reads both
`WORKFLOW.md` and `detent.yaml` from the selected commit and applies local
overlays from the working tree. Migrate the working-tree definition, commit
both shared files, advance the configured ref, and rerun doctor to verify the
new active revision.

Combined `WORKFLOW.md` remains readable during the documented compatibility
window, but new projects and templates should use the split layout.
