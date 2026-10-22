# Import a retired local board into a native project

`detent hub issue import-local` reads a copy of the SQLite file formerly
configured by `tracker.local_sqlite.path`. Both `local_sqlite` and `github_local`
use `detent_work_items` and `detent_work_item_events`; the retired connectors
and their configuration are not required. The importer opens the copy read-only,
never migrates it, and sends one item per request through native collaboration
owners. No runner or tracker configuration change is needed to read the copy.

Create a consistent SQLite backup using SQLite's backup command, or copy the
file with its writer stopped. Copying only the main file while WAL writes are
active can lose recent work. Keep the original store until migration is complete.

Choose an existing native target project and an operator credential with write
access to it. The command uses the same `--config`, `--project`, `--hub-url`,
`--organization`, and `--token-env` resolution as other `detent hub issue`
commands. Workers, Reporters, read-only credentials, and credentials without a
project grant cannot import. Project allowances still apply; a rejected item
rolls back completely, including its comments and dependencies.

Choose a stable `--store-id` for the original store and reuse it for every copy
and every run. Source identity combines this value, `--source-project`, tracker
kind, and original item ID. Moving or renaming the copy does not change identity.
Changing any of those identity inputs deliberately starts a different import.
The source project defaults to `default`; specify the old connector's project
ID when it used a different value. Import each source project separately.

Preview the import without writing native items, comments, relationships, or
import receipts:

```sh
detent hub issue import-local ./board-copy.sqlite \
  --project prj_example --source-project default \
  --kind local_sqlite --store-id example-board --dry-run
```

Repeat the same command without `--dry-run` to import. JSON output lists each
source ID and lane, action (`created`, `updated`, or `unchanged`), target lane, comment and
dependency counts, and `unknown_lane: true` when a fallback lane was necessary.
Native work-item IDs are included when they exist. Local SQLite items retain
their source priority, including an unset priority. Errors omit issue titles and
bodies. A failure can leave earlier items imported; replay the same copy and
identity to finish safely. A successful replay creates nothing and preserves
subsequent native edits, lane moves, comments, and relationship changes. This
is a snapshot migration: it does not synchronize later source edits or new
comments on an item already imported.

Lane names match the native workflow case-insensitively. Use repeatable
`--lane-map Source=Target` entries for renamed lanes, using the source spelling
from the store. For example, `--lane-map Todo=Backlog` imports source Todo work
without dispatching it. An unrecognized lane is retained as a reported fallback
to Backlog, then Triage, then another nonterminal, nondispatchable native state.
If no such state exists, import refuses the item. No lane is silently dropped.
Imported history does not supply new human confirmation or waive native claim
and issue-contract checks.

Open items are selected by their source lane. The default terminal lanes are
Done, Cancelled, and Closed; `--terminal-state` replaces that list for a custom
source workflow. Explicit `Depends on:` declarations outside fenced examples
become native dependencies. Referenced terminal prerequisites are included so
those relationships remain usable; unrelated terminal history is omitted.
Missing, cross-project, malformed, or cyclic dependencies refuse the import
before publication. Dependencies are imported first, and an item's content,
current comments, dependencies, and lane commit together before it can dispatch.
Current comments are reconstructed from comment, edit, and delete events;
removed comments stay removed. Source authors and timestamps are retained in
provenance, with the importing operator recorded as the native actor.

For a `github_local` store, first use GitHub onboarding intake for the source
repository in the native target project. Then preview and import with
`--kind github_local`, retaining the same store identity on subsequent runs.
The target's associated repository must match the source identifier's repository.
The importer matches the original GitHub issue number against that project's
onboarding records, independently of the native issue number. Every selected open
item must have exactly one match still in Triage; archived or subsequently moved
native items are refused. A terminal prerequisite can match the same terminal
native lane without changing it. Missing intake matches are never
replaced with duplicate issues. Existing native title, body, and GitHub
provenance remain authoritative; local comments and dependencies are added and
the local lane is restored.

The CLI preflights every item before publishing any. Hosted dry-runs check the
collaboration feature and current allowance capacity against the incoming item
and comment content without consuming mutation or event allowances. These checks
include nonterminal issue, storage, history, mutation, and event limits. Replaying
an already imported item consumes no allowance. Dry-run reports describe the
current target snapshot; cumulative growth across items, serialized history
storage, changed permissions, allowances, or native items can still refuse a
later real run. Publication checks actual growth in each item's transaction.
The authenticated endpoint is
`POST /api/v2/organizations/{organization}/projects/{project}/local-board-import`.
It accepts one `record` and a `dry_run` boolean using the CLI's serialized
request shape. Requests retain the normal 1 MiB limit, at most 1,000 comments
of at most 64 KiB each, and at most 100 dependencies per item. Persistent import
identity and the item are written together in one short transaction. There is
no background import job or local checkpoint to transfer between machines.
