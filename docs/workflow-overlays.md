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
