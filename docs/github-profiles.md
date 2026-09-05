# GitHub profiles and native cutover

Hub projects have exactly one authority profile. Existing repository aliases remain
`github_compatible`; newly created native projects use `native`. Migration is opt-in.
Read `/api/v2/organizations/{organization}/projects/{project}/integration` for the
profile, configuration revision, repository binding and field-authority map.

| Field or operation | github_compatible | native |
| --- | --- | --- |
| Issue title, body, labels, assignees, discussion | GitHub | Detent |
| Workflow and priority | GitHub projection with existing managed writes | Detent |
| Dependencies | Existing compatibility graph | Detent graph, initially imported at cutover |
| Original authors and timestamps | Source provenance | Retained source provenance plus authenticated native actors |
| Scheduling, leases, run progress | Detent | Detent |
| Repository execution and merge policy | Trusted repository revision | Same approved policy, retained at cutover |
| GitHub PRs, CI, branch protection and required review | GitHub | GitHub when repository integration is enabled |
| Native approval | Detent only | Detent only; never a required GitHub review |

An administrator can `PUT /integration` with `idempotency_key`,
`expected_revision` (a decimal string), `intake` (`disabled` or `manual`),
`projection` (`disabled` or `summary`) and `repository_enabled` (boolean).
These configure transport capabilities, not competing field owners. Compatibility
projects cannot enable native summary projection. Profile changes only occur
through the cutover operation. Repository integration can be disabled independently
of issue ownership. Disabling it does not authorize bypassing GitHub merge gates.

Use `detent hub serve --github-disabled` for a Hub requiring no GitHub credential.
Existing startup behavior remains the default. Pure native projects need no GitHub
repository. To attach GitHub to an existing native project, an administrator posts
`/integration/repository` with `repository: "owner/name"`, `idempotency_key`, and
`expected_revision`. This verifies repository metadata without fetching issues and
preserves the native project identity. A repository can belong to only one project;
for an existing compatibility binding, import/cut over that project instead.
Bindings are immutable. Enable the desired transport capabilities separately.
Hub's scoped v2 APIs accept its existing operator or enrolled runner
credentials. Configuration and cutover require administrator authority.

## Import and inspect

Enable manual intake on the repository-backed project. All paths below are
relative to its scoped v2 project URL.

1. `POST /imports` with `{"idempotency_key":"import-123","issue_number":123}`.
   The returned `import_id` is durable. Starting the same source issue returns its
   existing job. It does not create a second native identity.
2. `POST /imports/{import_id}/advance` with the returned `expected_revision` string.
   Each call retrieves one page, persists its records and cursor in one transaction,
   and returns the next revision. Use `GET /imports/{import_id}` after a lost reply.
   A repeated stale advance returns a revision conflict rather than duplicating
   events. Network failures retain the cursor and return `status: partial`, an
   error and a retry time. Respect that retry time. Requests share transport backoff.
3. Continue through the issue body, comments, timeline, blocked-by dependencies and
   GraphQL edit histories of the issue and every retrievable comment. REST follows
   next-page links and edit history follows GraphQL cursors. `stage: finished` and
   `status: retrieved` describe traversal of those API surfaces, not complete
   historical truth. Inspect `gaps` even after traversal finishes.
4. Read `/imports/{import_id}/records?limit=100` and follow `next_cursor` for the
   retained source records. Each record includes its source key, original payload,
   author/source timestamps where exposed, and observation time. Comments also
   appear in the native discussion with provenance. Missing source identities use
   a content hash only when GitHub supplies no stable ID. Missing authors and dates
   remain unavailable; they are not invented or attributed to the importing actor.
5. Import referenced blockers before cutover. Unresolved references remain visible
   in the preview. This migration currently requires dependency targets in the same
   repository-backed project; cross-project references prevent cutover and require
   an explicit operator mapping outside this importer.

GitHub does not return deleted comments, inaccessible private activity or redacted
edit diffs to every credential. Accessible GraphQL diffs are retained; missing or
redacted diffs are identified. Source pagination is not an atomic snapshot: quiesce
source edits and repeat traversal before migration. Imports never infer completeness
from issue excerpts or an initial page. Retained records and source IDs deduplicate
comment/timeline overlap and repeated imports.

To reimport, `POST /imports` with a new idempotency key, `restart: true`, and the job's
`expected_revision`. The cursor resets, retained source records remain, and native
body/discussion edits remain authoritative. Reimport adds newly discovered source
events; it does not overwrite native comments, reinstate removed native dependencies
or automatically follow later GitHub workflow changes. For new intake after cutover,
explicitly re-enable manual intake. Newly imported native issues remain ineligible
for claims until traversal finishes and imported dependencies resolve; unrelated
native work continues.

Reimport refreshes the dependency snapshot used by pending intake and cutover.
Removed source edges remain in exported history with `current_dependency: false`;
they do not become native graph edges. Reimport leaves already admitted native
dependencies unchanged, including after source additions or removals.

## Explicit cutover

Stop new compatibility dispatch and finish active leases. Run a current repository
repair and import every issue in the repository projection; the preview covers the
issues known to Hub. Review source edits and the import gaps before proceeding.

`POST /integration/cutover` first with this shape:

```json
{
  "idempotency_key": "cutover-preview-1",
  "dry_run": true,
  "initial_state": "Todo",
  "closed_state": "Done",
  "states": [
    {"name":"Todo","dispatchable":true,"transitions":["In Progress"]},
    {"name":"In Progress","dispatchable":true,"transitions":["Todo","Done"]},
    {"name":"Done","terminal":true,"transitions":["Todo"]}
  ],
  "accept_partial": false,
  "close_source": false
}
```

Preserve every existing workflow state and repository-specific operator-only stops
in `states`. `closed_state` must name a terminal state when source issues are closed.
The preview returns blockers, incomplete imports, unresolved dependencies, known
history limitations and a checkpoint. It changes no issue ownership. Apply with a
new idempotency key, the same options, `dry_run: false` and that `checkpoint`.
Concurrent source/import/configuration changes invalidate the checkpoint. Active
leases and processing GitHub writes prevent authority changes. Pending legacy
issue writes are superseded transactionally at cutover.

`accept_partial: true` is an explicit acknowledgement of incomplete retrieval;
it does not change the retained import status or erase gaps. Missing issue imports
and unresolved dependency references still prevent cutover. The durable cutover
receipt, readable at `GET /integration/cutover`, records the incompleteness and the administrator actor. Existing approved
repository gate/merge descriptors retain their identity and provenance under the
native project scope. Conflicting native and repository approvals are rejected.

Link-and-close is optional. Set `close_source: true` and an absolute
`destination_url` to enqueue a distinct Detent summary linking to the destination
and identifying the native work item. Closing is retried through the outbox after
the summary is written. Its success is separate from local ownership transfer;
inspect outbox health for pending retries or dead letters.

## Native execution and optional summaries

After cutover, issue creation, edits, comments, state transitions, claims and run
events stay in Hub. Native claims ignore GitHub repository freshness. Issue webhooks
and repair cannot overwrite native fields, and v1 compatibility mutation endpoints
cannot mutate a cut-over issue merely because it retains a GitHub ID.

Workers use `detent hub issue` with their configured Hub identity. The commands
`get`, `create`, `edit`, `comment`, `edit-comment`, `transition`, `dependency`,
`comments` and `history` call the scoped native APIs. Mutations read the existing
v2 JSON schema from stdin, including idempotency keys and expected revisions.
For example, save the mutation to a file and run:

```sh
detent hub issue edit wi_example --project prj_example < issue-edit.json
```

The command uses customer configuration by default. `--config` selects an external
configuration; `--hub-url`, `--organization`, `--project`, and `--identity-file` can
select an explicit enrolled identity. Operators may use `--token-env` with a scoped
Hub credential. Credentials are never printed. Native worker prompts explain the
authority and use `Detent-Work-Item: wi_...` in PRs, avoiding accidental GitHub closing
references for unrelated issues with the same number.

When `projection: summary` is enabled, an operator explicitly posts
`/work-items/{work_item_id}/projection` with `idempotency_key` and `body`. Pending
summaries coalesce per work item and retry idempotently using a distinct comment
marker. They never replace a legacy Workpad. Worker events do not automatically
queue summaries. GitHub PR merges retain fresh head/check/review verification and
GitHub branch protection enforcement; native approvals do not forge reviews.

Repository refresh remains centrally owned by Hub. Webhook hydration is coalesced;
repair runs on configured Hub intervals with a 500-request operation bound,
serialized requests and shared 403/429 backoff. Native issue polling is omitted;
disabling repository integration removes a native project's reconciliation target.
Existing repository freshness and outbox endpoints expose stale/error/retry state.
Administrators can read `/api/v2/github/requests` for process-lifetime request and
error counts by profile and operation family. Adding idle runners does not add Hub
GitHub polling loops. Counters reset when the Hub restarts.

## Rollback and export limitations

There is no automatic reverse authority switch or schema downgrade. A GitHub mirror
cannot faithfully represent native IDs, authenticated actors, discussion revisions,
workflow policy, leases or events. Before cutover, take an owner-coordinated Hub
backup. Restoring that backup discards later native work unless it is exported and
reconciled separately; it does not reopen source issues or undo external comments.

Export issue detail, paginated comments/history, revision endpoints and paginated
import records through v2. Keep the cutover receipt and approved policy descriptors.
These are retention/export surfaces, not a lossless GitHub round-trip importer.
Keep old clients on compatibility projects until their enrolled native project and
policy configuration is ready. Mixed profiles can coexist on one Hub.

References: [GitHub API best practices](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api),
[GitHub timeline API](https://docs.github.com/en/rest/issues/timeline),
[GitHub dependencies API](https://docs.github.com/en/rest/issues/issue-dependencies).
