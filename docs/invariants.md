# Repository invariants

These rules apply to all repository changes. Each invariant is a concept, stated in a few sentences and applied at review. Identify affected INV IDs in the Change description; put per-change rationale, evidence and verification there or in issue comments, as required by INV-16.

An invariant is a rule, not a specification and not a test. Invariants are enforced by review, human or AI, and by nothing else. Never write a test, source scanner, baseline file, lint rule or gate step to enforce an invariant, and never name a test in this document. Tests cover the behavior of code; they do not police rules. Only a change to a rule edits this document.

INV numbers are permanent identifiers. A retired invariant keeps its number as
a retired entry; numbers are never reused or renumbered.

## INV-1 — Lane ownership

The Hub is the source of truth for an item’s lane and grants one fenced, renewable lease per item across its phases. The runner holding that lease moves the item through its lanes as phases complete, and the Hub accepts a move only under a valid lease. Agent sessions report outcomes and never write lanes. Lease loss ends the runner’s authority and makes the item eligible for continuation from its last recorded phase commit.

Blocked is reserved for an explicit operator stop or a genuine unresolved decision or permission request. Record the request as a typed `human_action`, surface it durably to the human and release executing capacity. Only authorized human action clears the request. Machine failures use the retry loop.

## INV-2 — Instance-owned infrastructure failures

Merged into [INV-3](#inv-3--net-zero-mechanisms).

## INV-3 — Net-zero mechanisms

Use one execution loop and retry owner. Adding or expanding a protection or recovery mechanism requires removing or consolidating one. Reuse valid phase receipts, repair rejected work in place and decide the next phase from current workflow intent, typed dependencies, source, review, access, lease and capacity. Subscription usage never affects dispatch, claim eligibility, capacity, scheduling or retries.

Infrastructure failures belong to the instance. Retry the failed phase with capped jittered backoff and keep valid work and receipts. Machine failures stay dispatchable. Reconcile uncertain publication against the remote ref. A match with the intended SHA confirms publication, an unchanged or absent ref permits a compare-and-swap push, and a moved ref returns the item to Rework.

Lease renewal proves ownership only. For providers with confirmed tool-decision ordering and result coverage, the existing execution owner uses bounded, result-aware repetition evidence to steer or retire a non-progressing provider turn. Silence or activity alone is not a progress verdict. Providers without that coverage retain the existing quiet-session recovery path. One provider-owned deadline remains a transport backstop; command cancellation, resource controls and execution fencing retain their distinct authority. Every retirement feeds the existing end-of-round AI verdict and retry owner under the item lease. That verdict judges progress against the frozen acceptance criteria, verified source and open findings, never by commits or activity alone.

## INV-4 — Native merge queue

Moved to [Delivery policy](../WORKFLOW.md#delivery-policy).

## INV-5 — Local validation and release evidence

Acceptance and release decisions require authentic evidence bound to the exact source and relevant inputs. Reuse it only while those inputs remain valid. [WORKFLOW.md](../WORKFLOW.md#evidence-policy) specifies the commands, independent reviews and release authority.

## INV-6 — Isolated agent home

Workers never inherit the operator's agent instructions or write to the
operator's transcript storage, for any backend. Repository instructions and
the Detent-provided worktree remain authoritative. Codex workers run with an
isolated Codex home; resuming a persisted thread copies only that thread into
the isolated profile and leaves the host copy unchanged.

## INV-7 — Machine issue identity

Machine defects have stable provenance and fingerprints. Update the existing incident only when evidence or disposition changes. Respect human cancellation and allow explicitly authorized, already-deduplicated filing without another confirmation. Retain historical occurrences without per-round issue narration.

## INV-8 — No strict freshness protection

Moved to [Delivery policy](../WORKFLOW.md#delivery-policy).

## INV-9 — Retired mechanisms

Merged into [INV-3](#inv-3--net-zero-mechanisms).

## INV-10 — Priority only picks the next job

Priority chooses the next executable work. Active work keeps its execution slot, and retained item leases reserve no idle execution slots. Enforce configured capacity and access limits and explicit operator stops using current facts.

## INV-11 — Retired

## INV-12 — Native toolchain caches

Use native toolchain caches for speed only, preserving configured environments and active process outputs. Workspace and cache cleanup must preserve live work.

## INV-13 — A board card is a title and one status line

Moved to the [work-board contract](design-system/patterns.md#work-board-and-issues).

## INV-14 — Workers never wait on a human question

Merged into [INV-1](#inv-1--lane-ownership).

## INV-15 — Visible UI changes require a human-authored issue

Every user-facing surface is covered: the Cloud app (`web/conversation`),
Hub server-rendered pages and the local Templ dashboard. A change may add
visible elements or content only when a human-authored issue names the surface
or feature they belong to. A human-authored feature issue authorizes the whole
surface it describes, including the rows, controls, states, empty and error
copy and responsive layout that surface needs; it does not have to enumerate
each element. Machine-filed issues, agent-expanded scope and "while I was
here" additions never qualify.

Removing UI or fixing an existing element in place without adding visible
content does not need that approval. Agent diagnostics, coverage, provenance
and debugging data belong in existing API and MCP reads and logs. Never add
UI to make them observable, including on Diagnostics, behind toggles or debug
flags. Agents describe proposed UI additions in their outcome or file Backlog
work for a human to author or rewrite; they do not build them.

## INV-16 — The Hub carries execution context and Git carries source

<a id="inv-16--the-tracker-database-is-the-only-shared-knowledge-channel"></a>

The Hub carries issue direction, phase receipts, verdicts and findings. Git carries source commits. Agents never carry execution state through repository notes or machine issue-comment streams. Human discussion and final outcomes remain in the tracker.

## INV-17 — Hub-owned assignment and retained item worktrees

<a id="inv-17--runners-are-stateless-like-github-actions-runners"></a>

The Hub alone owns assignment and execution authority through one fenced, renewable item lease. The runner retains the same worktree across the item’s phases and retries. Each completed phase pushes ordinary commits and records commit-bound receipts in the Hub. After lease loss, a new runner resumes from the last pushed phase commit at the first phase without a valid receipt, and uncommitted work may be lost.

Runners use their declared install configuration. Keep unrelated jobs independent of machine-local state. Never open a local application database, discover configuration by scanning the machine, route work by where it previously ran, or use local files or queues as execution or recovery authority.

## INV-18 — Hub writes stay small

A request's write transaction writes only the rows that request owns. It never
rebuilds rollups, recounts totals, cleans up, backfills or reprojects other
rows. Derived data is maintained by background work, one item per short
transaction, and may lag the request that changed it.

Work inside any write transaction is bounded by the size of the request, never
by the size of history. Reads use the reader pool, never the writer
connection. Stored data shape is governed by
[INV-20](#inv-20--sql-never-reads-or-writes-inside-stored-json): new Hub
columns are typed, never JSON.

## INV-19 — No flaky tests

A test that both passes and fails on the same commit is a High defect. Its owner diagnoses it from the failing run's evidence, then fixes the cause or deletes the test. A flake is never accommodated: no added retries, sleeps, longer timeouts, skips, expected-failure marks or quarantine.

## INV-20 — SQL never reads or writes inside stored JSON

Hub and store SQL never use JSON functions or operators over stored data, including building JSON from stored columns. Every field SQL filters, joins, orders, aggregates, projects or updates is a typed column.

Hub rows hold typed columns only. Audit, history and runtime payloads are written to object storage at write time, and the row keeps a typed reference. Typed read-model tables may hold current page content for display, never serialized history. Existing JSON queries and inline payload columns are migration debt that only shrinks: no change adds one, and a change that touches one moves it.

## INV-21 — Designs hold at ten thousand tenants and runners

Every design is judged at 10,000 tenants and 10,000 runners, with up to 1,000 runners, 100 projects and 1,000,000 attempts of history in one tenant. A choice that only works at today's size is a defect.

Request work is bounded by the request and periodic work by a fixed per-tick budget; neither grows with tenants, runners, items or history. Shared locks, connections and transactions are never held across per-item network calls or object walks. Fleet traffic is jittered and admission is limited per tenant and per runner, so one tenant or a synchronized fleet cannot starve the others.

A Change that adds or modifies a hot path, a shared lock or a periodic job states its cost at this scale in its description.

## INV-22 — Tests never use production credentials or systems

Automated tests and development use isolated fixtures and no production credentials or effects. Explicitly authorized operational release checks follow the [release procedure](../WORKFLOW.md#release) and are recorded as production operations.

Code under test that reads a credential from the environment has it cleared in `TestMain` or replaced with a fake. No development, test or CI environment enables a production credential or can open a connection to a production endpoint.

## INV-23 — The tenant is never a process

The tenant is an attribute of a request, a job and a row, never of a process, binary, physical connection pool, timer, idle goroutine or configuration file. One Hub process serves many organizations. The tenant scope is built once from the verified request or the scheduler's selected tenant, passed explicitly and rechecked where writes commit; it is never read from process-wide state. Background work is one shared, bounded scheduler per process. The only dedicated placement is a shard assignment Cory approves, with a recorded reason.

This invariant incorporates [the multi-tenancy architecture](architecture/multi-tenancy.md) as normative; where they differ, this section wins.
