# Non-Code Artifact Workflow Demo

This demo shows Detent running a local content-production workflow with no
GitHub issues, pull requests, branches, CI, or merge train. It uses:

- `tracker.kind: local_sqlite` for work items and status.
- `workspace.kind: filesystem` for task workspaces.
- `deliverable.kind: artifact` for output manifests.
- `gate.kind: artifact` with the `render_status` field as the review gate.

## Run The Demo

From this directory:

```sh
detent --config .detent/demo-global.yaml --host 127.0.0.1 --port 4101
```

Open <http://127.0.0.1:4101/projects/default/kanban>. The project id is
`default` because the missing `.detent/demo-global.yaml` path makes Detent boot
directly from this directory's `WORKFLOW.md` instead of using your normal global
config.

The seeded board shows a content workflow:

```text
Intake -> Research -> Draft -> Review -> Package -> Publish
                    \-> Rework -> Draft
```

The seeded Review cards exercise all artifact-gate outcomes:

- `pending_review` stays in Review.
- `approved` promotes from Review to Package.
- `recut` routes from Review to Rework.

The sample cards set `assigned_to_worker: false` so opening the demo does not
start real content agents. To test agent dispatch later, create an item in an
active state in a workflow that has an agent backend configured for your
environment.

## Add Work Items Through The API

Loopback demo runs do not require an API token. If you configure an
`api_token`, add `-H "Authorization: Bearer $DETENT_API_TOKEN"` to the curl
commands.

```sh
curl -fsS -X POST "http://127.0.0.1:4101/api/v1/projects/default/work-items" \
  -H "Content-Type: application/json" \
  --data @seed/01-research-brief.json

curl -fsS -X POST "http://127.0.0.1:4101/api/v1/projects/default/work-items" \
  -H "Content-Type: application/json" \
  --data @seed/02-review-package.json
```

The response shape is:

```json
{"id":"api-demo-research-001","identifier":"api-demo-research-001","number":7,"url":"http://127.0.0.1:4101/projects/default/kanban"}
```

## Exercise Status Transitions

The artifact gate reads `render_status` from each work item field. Use these
status values:

| Status | Gate action |
| --- | --- |
| `queued` | Wait in the current state. |
| `pending_review` | Wait in Review. |
| `approved` | Promote from Review to Package. |
| `valid` | Promote from Review to Package. |
| `recut` | Route from Review to Rework. |
| `invalid` | Route from Review to Rework. |
| `missing_assets` | Route from Review to Rework. |

A wait value seeded when a work item is created does not park its first
dispatch. Wait statuses take effect after `render_status` is updated later
than the time the item entered its current state.

To flip the waiting Review card to approved:

```sh
sqlite3 .detent/non-code-artifact-demo.db \
  "update detent_work_items set fields_json = json_set(fields_json, '$.render_status', 'approved') where project_id = 'content-production-demo' and id = 'content-demo-003';"

curl -fsS -X POST "http://127.0.0.1:4101/api/v1/refresh"
```

To send the same card back through rework:

```sh
sqlite3 .detent/non-code-artifact-demo.db \
  "update detent_work_items set state = 'Review', fields_json = json_set(fields_json, '$.render_status', 'recut') where project_id = 'content-production-demo' and id = 'content-demo-003';"

curl -fsS -X POST "http://127.0.0.1:4101/api/v1/refresh"
```

No SQLite schema setup is required; the update only changes the configured
status field on an already seeded local work item.

To reset the demo, stop Detent and remove `.detent/non-code-artifact-demo.db`
plus any adjacent `-wal` or `-shm` files.

## Output Manifest Shape

Each artifact card points at an output manifest under `output/`. The sample
manifest in `output/package-approved-newsletter/manifest.json` shows the
expected handoff shape:

```json
{
  "work_item": {
    "id": "content-demo-004",
    "identifier": "content-demo-004",
    "title": "Package approved newsletter"
  },
  "artifact": {
    "kind": "newsletter_package",
    "path": "output/package-approved-newsletter/newsletter-package.md",
    "review_url": "http://127.0.0.1:4101/review/content-demo-004"
  },
  "validation": {
    "status_field": "render_status",
    "status": "approved",
    "notes": "Ready for package handoff."
  }
}
```

## How This Differs From GitHub Issue-To-PR

The default Detent code workflow reads GitHub status, creates a git worktree and
branch, asks an agent to edit code, opens or updates a pull request, waits for
CI and review, and merges through the serialized Merging lane.

This artifact workflow keeps the same board and gate discipline but swaps out
the delivery edge. Work items live in local SQLite, workspaces are plain
filesystem directories, output is an artifact manifest, and the artifact gate
looks at `render_status` instead of PR, CI, and automated review state.
