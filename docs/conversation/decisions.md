# Conversation product: foundation decisions

Recorded September 9, 2026 for the first build milestone: new chat, discussion,
linked issue, continue with that issue's runner. This document settles the
plan tasks D02 (ownership and handoff rules), F02 (client boundary), F03
(contracts) and F04 (spike port plan) of
`detent-cloud-concept/IMPLEMENTATION-PLAN.md`. Later milestones may extend it;
they must not silently contradict it.

## 1. Where the product lives

The conversation product is part of the hosted hub (`internal/hubserver`),
not the single-tenant dashboard (`internal/web`). Reasons:

- Organizations, projects, native issues, attempts, leases, fencing tokens,
  runner identities and hosted sessions already exist only in the hub.
- The design mockups are the Detent Cloud shell.
- The conversation spike proved its ownership checks against the hub's
  native authorization, claims and attempts.

The existing operator chat in `internal/web` and `internal/chat` stays as is.
Its provider abstraction (a Codex `AgentToolBackend` driven with tools) is the
model for the hub's coordinator turns, but hub coordination tools read hub
data (issues, attempts, leases, changes), not the local telemetry snapshot.

A new feature package `internal/conversation` owns the domain vocabulary:
identifiers, delivery and execution states, command envelopes, receipts,
validation and restart normalization. It has no database or HTTP code. The
hub owns storage, HTTP, streaming, runner binding and control delivery.

### Where model turns execute (decided September 10, 2026)

Every model turn, including the coordinator turn that answers an unlinked
chat, executes on a customer runner using that runner's own provider login or
API key. The hub never runs a provider process and never holds provider
credentials. Detent does not resell tokens: the customer's ChatGPT or Claude
subscription, or their own API key, pays for every turn, the same way the
cloud hub RFC already fixes it for issue runs.

This is the model T3 Code uses. Its server runs on the machine that owns the
workspace, spawns the user's own `codex app-server` or `claude` CLI, and its
hosted identity and relay are deliberately outside the model data path.
Detent's hosted hub plays the relay's role: it stores conversations, receipts
and events, and routes turns to a customer runner.

Consequences:

- An unlinked conversation's `message` command creates a coordinator work
  item that is dispatched to an enrolled runner with a live-control backend.
  The runner binds, long-polls controls and posts turn events through the same
  worker endpoints as a linked attempt (section 5). The coordinator prompt
  and hub-reading tool set stay as designed; only the process that runs them
  moves.
- `capabilities.coordinator` in the bootstrap payload means "a runner able to
  take coordinator turns is enrolled for this project", not "the hub has a
  backend configured". With no such runner online an ordinary chat reports
  `waiting_for_runner`, exactly like a linked conversation without an attempt.
- The hub-side Codex coordinator that shipped in the first slice (the
  `conversation.codex` and `conversation.workspace` hosted-config keys, see
  `operations.md`) is transitional. It is acceptable only on a customer
  self-hosted hub, where the hub host is the customer's own machine and the
  login is theirs. It must not be enabled on Detent Cloud, and it is removed
  once the runner-dispatched coordinator lands.
- Two things remain out of scope: any Detent-operated inference or proxy, and
  storing customer provider keys in the hub.

## 2. Identity model

| Identity | Meaning | Where it lives |
|---|---|---|
| Conversation ID (`conv_` + 32 hex) | Durable history and audience. Never changes when an issue is linked or a runner changes. | `conversations.id` |
| Organization, project | Authorization scope. Fixed at creation. | `conversations.organization_id`, `conversations.project_id` |
| Work item (`wi_`) | Optional linked issue. At most one per conversation; at most one canonical conversation per issue. | `conversations.work_item_id`, partial unique index |
| Attempt, run, lease, fencing token | Detent execution ownership generation, from `native_attempts` and `leases`. | Execution owner tuple in `conversations.execution_json` and on every control envelope |
| Runner, machine | Who executes. | Owner tuple |
| Provider thread | Resumable provider context. May outlive an attempt. Never equals an attempt. | `conversations.provider_thread_id`, owner tuple |
| Provider turn | One provider exchange. Live steering appends to the current turn, so one user message is not one turn. | Owner tuple, message columns |
| Message ID (`msg_`) | Accepted user intent or assistant output. Immutable once accepted. | `conversation_messages.id` |
| Question ID (`q_`) | A pending request for user input bound to an owner generation. | `conversation_questions.id` |
| Command key | Client-generated idempotency key, at most 128 bytes. | `conversation_commands.key` |
| Event sequence | Per-conversation monotonic cursor for replay. | `conversation_events.seq` |

The owner tuple is
`{attempt_id, run_id, lease_id, fencing_token, runner_id, machine_id, thread_id, turn_id}`.
Every control carries the expected `attempt_id` (and `turn_id` for steering).
The hub re-validates the tuple at acceptance and again immediately before the
control is handed to the worker, and the worker validates it again against
its own lease before writing to the provider. A mismatch is
`stale_execution`, never a silent redirect to a replacement attempt.

Clients never supply authority by naming an ID. Organization and project come
from the authenticated scope; the actor comes from the hosted session or API
token; lease and fencing token come from the worker's authenticated lease.

## 3. Ownership, audience and handoff (D02)

Actors are hosted members (owner, admin, member, viewer roles with project
grants) or project-scoped API tokens. The actor key is the hub principal
(`api_tokens.id`, which hosted members also have) plus the hosted subject
when present.

Rules:

1. A new chat is private to its creator. It requires a project. Only the
   creator can read it, write to it, subscribe to it, search it or archive
   it. Organization admins do not gain read access to private chats through
   their role; support actors are subject to the same rule.
2. Linking a chat to an issue makes it shared with everyone who can read the
   project, because issues are shared. The link request must carry
   `share_history: true`. The client shows the audience (project members with
   read grants) and the full history that will become visible before the user
   confirms. Without that flag the link request is rejected with
   `share_history_required`. The change is recorded in
   `conversation_audience_events`.
3. Sharing is one way. A shared conversation cannot return to private.
   Selective-context handoff (sharing only part of a history) is not in this
   slice; it would need a separate linked conversation with origin references.
4. One conversation links to at most one issue; one issue has at most one
   canonical conversation. Linking a conversation that is already linked, or
   linking to an issue that already has a canonical conversation, fails with
   `conversation_already_linked`. The failure is reported with the existing
   link so the client can navigate instead of duplicating.
5. A conversation never moves between projects or organizations. A move
   request fails with `conversation_project_fixed`. History is never copied
   or re-parented.
6. Creating an issue from a chat uses the existing native issue creation
   path and workflow state policy. It does not schedule, approve, or grant
   execution permissions. The response reports the issue's lane and whether a
   runner is bound; the client shows "waiting for runner" until an attempt
   binds.
7. Who can control a runner: the actor must have write access to the project
   (`hosted_project_grants.can_write`, or an operator token with a grant) and
   read access to the conversation. Viewers can read shared conversations and
   cannot send commands. Answering a question and interrupting are controls.
8. Membership loss is enforced on every request from live membership. Event
   streams re-authorize on a timer and on every emitted event and close on
   loss. Cached client state is cleared on logout or account change.
9. Archiving marks the conversation read-only and removes it from the
   default list. It never stops or affects a runner; the linked issue and its
   attempts continue under scheduler policy.
10. Ordinary replies never escalate capabilities. A pending question is not an
    approval. Approving a PR, merging, changing tool permissions or moving a
    lane are separate, actor-, action- and execution-scoped operations that
    go through their existing endpoints and policy checks.

Worked examples:

- Private chat linked to a shared issue: link with `share_history: true`;
  all prior messages become readable by project readers; audit row written.
- Non-owner project member opens a private chat: `404 not_found` (opaque),
  same as a cross-project read.
- Member whose grant is revoked while subscribed: the next authorization
  tick closes the stream with `event: closed` and `access_revoked`.
- Linking to an issue that already has a conversation: `409
  conversation_already_linked` with `{existing_conversation_id}`.
- Proposed cross-project move: `422 conversation_project_fixed`.

## 4. Client boundary (F02)

> Superseded on September 10, 2026 by section 11: the React app is the whole
> hosted frontend, not a route that coexists with Templ pages. The build and
> serving notes below still apply; the coexistence language does not.


The conversation client is a React 19 application using Effect for state and
wire validation, reusing the T3 Code client runtime already proven in the
conversation POC (connection supervisor, registry, retention, detail
subscriptions). It is mounted under a same-origin authenticated route in the
hub and coexists with the Templ pages.

- Source: `web/conversation/` (Vite, TypeScript, Vitest). Third-party
  attribution is in `web/conversation/THIRD_PARTY_NOTICES.md`, and the
  MIT license is in `web/conversation/LICENSE.t3code`.
- Build: `make app` runs `npm ci` and `vite build` in `web/conversation/`,
  emitting to `static/app/conversation/`. `make generate` runs it when the
  source directory exists. The built output is committed, following the
  precedent of `static/css/output.css`, so `go build` never needs Node.
- Serving: the hub registers `GET /chat` and `GET /chat/*` which require a
  hosted session (unauthenticated requests follow the existing login
  redirect) and return the application shell with a bootstrap payload
  (organization, projects and grants, actor, CSRF token, capabilities,
  feature flags). Static bundles are served under `/static/app/conversation/`
  from the embedded filesystem.
- Feature flag: `hosted.conversation.enabled` in the hub's hosted config.
  When disabled the routes are not registered and no conversation API is
  mounted; existing pages are unaffected and stored conversations are kept.
- Development: `make app-dev` runs Vite with a proxy to a local hub for the
  API and SSE routes.
- Authentication from the client: same-origin cookie session; mutations send
  `X-CSRF-Token` from the bootstrap payload (the existing hosted CSRF
  scheme). SSE uses the cookie. API tokens are not used by the browser.

Alternative considered: a Templ/HTMX page reusing the existing chat panel.
It would avoid a second build graph, but the plan's product decision is to
reuse the T3 client runtime and chat components, and the required behavior
(cursor-based reconnect without re-downloading, optimistic messages merged
with receipts, per-conversation drafts and upload queues, streaming deltas
without re-rendering the transcript) is exactly what that runtime provides.
The decision can be revisited at review; the API is client-agnostic.

## 5. Contracts (F03)

All conversation endpoints live under the native project base
`/api/v2/organizations/:organization/projects/:project` unless stated. JSON
bodies. Timestamps are RFC 3339 UTC. Errors use the native error shape
`{code, message, details?}` with these distinct codes:

| Status | Code | Meaning |
|---|---|---|
| 404 | `not_found` | No access or no such conversation (opaque) |
| 403 | `forbidden` | Authenticated but the action is not allowed (viewer sending a command) |
| 409 | `idempotency_conflict` | Same key, different payload |
| 409 | `stale_execution` | Expected owner generation no longer current |
| 409 | `conversation_already_linked` | Link constraint violated |
| 409 | `question_already_answered` | Single-use question consumed |
| 422 | `invalid_request` | Validation failure |
| 422 | `unsupported_control` | Backend lacks the capability |
| 422 | `share_history_required` | Link without explicit sharing confirmation |
| 422 | `conversation_project_fixed` | Attempted move |
| 503 | `queue_full` | Bounded control queue full, retry later |
| 200 with `status: unknown` | | Accepted but delivery could not be established |

### Resources

Conversation:

```json
{
  "id": "conv_…", "organization_id": "…", "project_id": "…",
  "title": "…", "visibility": "private|shared",
  "status": "active|archived",
  "work_item_id": "wi_…|null", "linked_at": "…|null",
  "work_item": {"id": "wi_…", "identifier": "<project>#<number>", "title": "…", "lane": "…", "runner_bound": false}|null,
  "owner": {"principal_id": "…", "subject": "…"},
  "execution": {
    "status": "idle|waiting_for_runner|starting|running|waiting_input|interrupting|completed|interrupted|failed|unknown",
    "attempt_id": "…", "run_id": "…", "runner_id": "…", "thread_id": "…", "turn_id": "…",
    "capabilities": {"steer": true, "interrupt": true, "answer": true, "continue": true},
    "error": "…|null", "updated_at": "…"
  },
  "revision": 12, "event_seq": 40,
  "created_at": "…", "updated_at": "…", "last_message_at": "…|null"
}
```

Message:

```json
{
  "id": "msg_…", "conversation_id": "conv_…", "seq": 7,
  "role": "user|assistant|system", "kind": "text|answer|interrupt|continue|tool|status",
  "text": "…", "data": {},
  "delivery": "saved|queued|sending|sent|delivered|responding|completed|interrupted|rejected|failed|unknown",
  "attempt_id": "…|null", "turn_id": "…|null", "provider_item_id": "…|null",
  "actor": {"kind": "human|runner|coordinator", "principal_id": "…"},
  "command_key": "…|null",
  "created_at": "…", "updated_at": "…"
}
```

Question:

```json
{
  "id": "q_…", "conversation_id": "conv_…", "message_id": "msg_…",
  "status": "pending|sending|sent|answered|expired|unknown",
  "owner": {"attempt_id": "…", "turn_id": "…"},
  "questions": [{"id": "…", "header": "…", "question": "…", "options": [{"label": "…", "description": "…"}], "free_text": true, "multiple": false}],
  "answers": {"question_id": ["…"]}, "answered_by": "…|null",
  "expires_at": "…|null", "created_at": "…", "updated_at": "…"
}
```

Receipt:

```json
{"key": "…", "kind": "message", "status": "saved|queued|sending|sent|delivered|rejected|unknown", "message_id": "msg_…|null", "question_id": "q_…|null", "error": {"code": "…", "message": "…"}|null, "updated_at": "…"}
```

### Operator endpoints (hosted session or operator token, project grant)

| Method and path | Body | Result |
|---|---|---|
| `POST /conversations` | `{title?, first_message?: {key, text}}` | 201 conversation (+ receipt when a first message was sent) |
| `GET /conversations?cursor&limit&q&include_archived` | | `{conversations: [...], next_cursor}` visible to the actor, most recent activity first |
| `GET /api/v2/organizations/:organization/conversations?...` | | Same across projects the actor can read |
| `GET /conversations/:conversation` | | `{conversation, messages (latest page), questions, cursor}`; `questions` carries every question still awaiting an answer plus the ones the current attempt already settled, newest 20, so a reloading tab renders a locked card instead of losing it |
| `GET /conversations/:conversation/messages?before=<seq>&limit` | | Older page, ordered by seq |
| `GET /conversations/:conversation/events?after=<seq>` | | SSE stream, see below |
| `POST /conversations/:conversation/commands` | Command envelope | 200 receipt |
| `POST /conversations/:conversation/link` | `{key, share_history, issue: {title, description, state?, labels?, priority?}}` | 200 `{conversation, issue, scheduling: {lane, runner_bound}}` |
| `POST /conversations/:conversation/archive` / `unarchive` | | 200 conversation |
| `PATCH /conversations/:conversation` | `{title}` | 200 conversation |

Command envelope:

```json
{"key": "…", "kind": "message|answer|interrupt|continue|cancel", "text": "…", "attachments": [], "question_id": "q_…", "answers": {"…": ["…"]}, "expected": {"attempt_id": "…|null", "turn_id": "…|null"}}
```

- `message` in an unlinked conversation starts or continues a coordinator
  turn, which executes on a customer runner (section 1). In a linked conversation with a running attempt that supports
  steering it is delivered to the current turn (`delivered` when the
  provider acknowledges). Without a running attempt it is `queued` and the
  execution status reports whether a runner is expected.
- `answer` targets one pending question. Exactly one winner per question.
- `interrupt` and `continue` require `expected.attempt_id` (continue may be
  null when the previous attempt has finished). `continue` follows
  scheduler policy: it records intent and reports `waiting_for_runner`; it
  never starts a runner from the hub.
- `cancel` stops a coordinator turn in an unlinked conversation. Once the
  coordinator is runner-dispatched it is delivered as an interrupt to the
  runner holding the coordinator work item; the client-facing kind stays
  `cancel`.

`issue.priority` on a link request is the native rank 0 to 3, accepted as a
number or its decimal string. The hub does not publish a priority vocabulary
in the bootstrap payload yet, so the client's picker stays hidden and the
field is normally omitted.

On a successful link the hub appends a `role: system`, `kind: status` message
whose `data.issue` is `{id, identifier, title, state, lane, runner_bound}`.
The result of a handoff is therefore history: it survives a reload and
reaches every other tab, not only the response to the link request.

Retrying a command with the same key and byte-identical payload returns the
stored receipt. A different payload is `idempotency_conflict`.

### Event stream

`GET .../events?after=<seq>` responds with `text/event-stream`. Each frame
has `id: <seq>` and `event: <type>` and a JSON `data` body. Types:

| Type | Data |
|---|---|
| `conversation.updated` | Conversation resource |
| `message.accepted` | Message resource |
| `message.delta` | `{message_id, seq, text}` appended text |
| `message.updated` | Message resource (delivery, final text) |
| `question.opened` / `question.updated` | Question resource |
| `execution.updated` | Execution object |
| `command.receipt` | Receipt |
| `heartbeat` | `{seq}` every 15 seconds, no id |
| `closed` | `{reason: access_revoked|archived|server_shutdown}` then the stream ends |

The snapshot from `GET /conversations/:conversation` includes `cursor`; the
client opens the stream with `after=cursor`. Events are committed in the same
transaction as the state they describe, so `snapshot + events after cursor`
is exact. A client that reconnects with a cursor older than the retained
window receives `event: closed` with `reason: cursor_expired` and must
re-snapshot. Deltas are never full documents.

### Worker endpoints (worker token, lease-fenced)

| Method and path | Body | Result |
|---|---|---|
| `POST /work-items/:item/conversation/bind` | `{lease_id, fencing_token, attempt_id, run_id, capabilities: {...}, thread_id?}` | 200 `{conversation_id, resume: {thread_id}, pending_messages: [...]}`; 404 when the issue has no conversation |
| `POST /conversations/:conversation/turn-events` | `{lease_id, fencing_token, attempt_id, events: [...]}` | 202; events: `turn_started`, `delta`, `item`, `question_opened`, `turn_completed`, `control_result`, `execution_status` |
| `GET /conversations/:conversation/controls?after=<n>&wait=<seconds>` | headers carry lease and fencing token | 200 `{controls: [...], cursor}` long-poll, ordered, bounded |
| `POST /work-items/:item/conversation/unbind` | `{lease_id, fencing_token, attempt_id, outcome}` | 200 |

The bound queue holds at most 64 pending controls per conversation. A full
queue rejects acceptance with `queue_full` before anything is persisted.

Delivery ladder for a control: `saved` (committed) → `queued` (waiting for
a bound worker) → `sending` (handed to the worker poll) → `sent` (worker
confirmed receipt, before the provider write) → `delivered` (provider
acknowledged, for steer and interrupt) or `sent` only (answers have no
provider acknowledgement) → `responding` → terminal. Any step that cannot
be established becomes `unknown` and is never retried automatically.

### Bootstrap payload for the client

`GET /chat/bootstrap` (hosted session): `{organization: {id, name}, actor: {principal_id, subject, email, role}, projects: [{id, name, can_write, labels?, priorities?}], csrf_token, capabilities: {coordinator: bool, attachments: bool}, api_base: "/api/v2/organizations/<org>", feature: {conversation: true}}`. A project's `labels` and `priorities` are the vocabularies the handoff form offers; they are absent until the hub publishes them, and the client never invents a taxonomy of its own.

## 6. Spike port plan (F04)

The uncommitted spike in `detent-conversation-spike` is ported deliberately:

Port as is (new files, tests kept):

- `internal/runner/conversation_control.go`: `AgentLiveBackend`,
  `AgentControl`, `AgentInputRequest`, `AgentConversationControl`, and the
  `ConversationControl` field on `AgentTurnRequest`.
- `internal/codex/conversation_control.go` and its tests: the wrapped
  transport, single reader pump, direction-based correlation, turn arming,
  suspended deadline while a question is pending, `Close` joining the pump.
- The transport wrap point in `AppServer.RunTurn` and
  `SupportsLiveControl` on the Codex backend.

Change while porting:

- Collaboration mode becomes an explicit `AgentTurnRequest.CollaborationMode`
  value instead of being implied by the presence of a control object.
- A `request_user_input` for a thread or turn that does not match the binding
  is declined with the existing empty-answer response and logged; it does not
  fail the run.
- Pending questions carry an explicit expiry (`QuestionTimeout`, default 24
  hours, configurable) that ends the wait with `expired` instead of holding
  the turn forever.
- Wrapper state is guarded by a mutex and the single-goroutine contract is
  asserted in tests.
- Status and delivery values are typed constants in `internal/conversation`.

Replace:

- The four runtime-created `spike_*` tables become hub migration
  `00022_create_conversations.sql` with normalized rows and delta events.
- The global spike mutex becomes per-conversation revision checks with
  transactional writes.
- The in-process command channel becomes the worker long-poll and turn-event
  endpoints above; the worker keeps a local bounded queue feeding the
  transport wrapper.
- The `/spike/*` routes are not ported. `run()` is the reference for the
  worker-side turn body, not for how it is triggered: binding happens in the
  native execution path when an attempt starts, and unbinding when it
  finishes or its lease is lost.

Retained tests (ported to the new storage and transport): ownership and
answers, recheck at write, restart uncertainty, revoked access at write,
concurrent answers, terminal crash window, one attempt one start, concurrent
acceptance order. The opt-in live Codex test is kept behind
`DETENT_CONVERSATION_LIVE=1`.

Issue #2416 (human questions on the original issue) shipped in #2428 as an
orchestrator-side flow using tracker comments. Conversation questions use the
same durable-wait principle (persist the question, resume on an authorized
answer, never create synthetic issues). Aligning the two channels so a
question can be answered either in the app or in a tracker comment is R05
work and is not part of the first milestone.

## 7. Out of scope for this milestone

Attachments beyond text (S03/U04 arrive after the runner path is proven),
Work board and review dock integration (W and V tasks), multiple runners or
branches per conversation, cross-host provider thread transfer, and a
production importer for POC data.

## 8. Design decisions resolved from the inventory

`design-inventory.md` lists open decisions. They are settled as follows:

1. Accent: the client uses the artifact's blue primary
   (`oklch(0.571 0.21 264)`) for interactivity; Templ pages keep their teal
   accent until the shell is unified in milestone E.
2. Composer scope chips are read-only indicators of real capabilities; no
   per-turn model, permission, cost or repository overrides.
3. The sidebar gets a Chats section. This is a deliberate extension of the
   artifact required by the plan's outcome (persistent conversations in the
   sidebar) and is reviewed as such.
4. Review split mode is deferred to V03.
5. Typography: Geist and Geist Mono from the embedded fonts on both surfaces.
6. Dark only for the first release; recorded as a limitation for O03.
7. The client renders the whole window on `/chat*`; it links back to the
   Templ pages.
8. Routes: `/chat` (new chat), `/chat/c/:conversation`, `/chat/p/:project`
   (new chat in a project), `/chat/issues/:workItem` (resolve to the linked
   conversation). The bootstrap payload is `/chat/bootstrap`.

## 9. Runner-dispatched coordinator (contract for the next slice)

This section specifies how the decision in section 1 ("Where model turns
execute") is implemented. It coexists with the transitional hub-side
coordinator: when the hub has a `conversation.codex` backend configured the
hub path is used; otherwise the runner path below is used.

### 9.1 Coordinator work items

- When an unlinked, active conversation accepts a `message` command and the
  hub has no coordinator backend, the hub ensures one open coordinator work
  item exists for the conversation: a native issue in the conversation's
  project with title `Coordinator: <conversation title>`, a body that names
  the conversation id and states that no implementation, worktree or pull
  request is expected, the project's first dispatchable workflow state, and
  the label `detent:coordinator`. A row in the new table
  `coordinator_items (work_item_id PK, conversation_id, organization_id,
  project_id, created_at, closed_at NULL)` records the association. The
  conversation's `work_item_id` stays empty; the conversation is not linked
  and stays private.
- If an open (non-terminal, `closed_at IS NULL`) coordinator item already
  exists, no new one is created; the accepted message is `queued` and reaches
  the bound attempt through the controls poll, or the next attempt when the
  item is claimed later. The message receipt is `queued` and the execution
  status is `waiting_for_runner` until a runner binds.
- When the bound attempt unbinds, the hub transitions the coordinator item to
  the project's first terminal state and sets `closed_at`. The next message
  creates a new coordinator item. Each coordinator attempt therefore answers
  the messages queued before and during its turn; follow-ups after completion
  start a new attempt on the same provider thread when the same runner claims
  it.
- `cancel` on an unlinked conversation with a bound coordinator attempt is
  delivered as an `interrupt` control to that attempt; the client-facing kind
  stays `cancel`.

### 9.2 Claim gating and visibility

- A coordinator item is claimable only by an enrolled runner whose fresh
  provider report (within the heartbeat window) lists a backend that
  supports live control (today: `codex`). The hub enforces this in the claim
  loop; runner-supplied label filters remain preferences, never authority.
  Legacy machine registrations never receive coordinator items.
- Coordinator items are excluded from the hosted project issue list, from
  `GET /work-items` unless `include=coordinator` is passed, and from
  attention summaries. They keep normal history and attempts so audit works.
- `capabilities.coordinator` in the bootstrap payload is true when the hub
  has a coordinator backend or at least one active, fresh, live-control
  runner holds a grant for the project.

### 9.2.1 A claim is offered work, not an answered item (added September 12, 2026)

- The hub's claim predicate used to be a function of the item's lane alone, so
  an attempt that succeeded and left the item in a dispatchable lane was
  offered again on the next poll, forever: `operations.md` section 7, "An item
  in an active state is re-dispatched forever". The rule is now that an item
  whose **latest** attempt succeeded against the item as it stands is not a
  claim candidate.
- "As it stands" is two recorded numbers, both captured at `run.started` inside
  the transaction that creates the attempt: `native_attempts.work_item_revision`
  is the `issues.revision` the attempt was dispatched for, and
  `native_attempts.dispatch_generation` is the `issues.dispatch_generation` it
  was dispatched under. The item is offered again as soon as either has moved:
  `issues.revision` past the attempt's, or `issues.dispatch_generation` past
  it.
- **Dispatch is requested explicitly, never inferred.** A revision covers
  everything that changes the item — an edit, a comment, a gate rework, a
  plan-to-implement handover, a person moving it back into a dispatchable lane
  — because all of them run through the same issue write. It does not cover a
  conversation continuation, which changes nothing about the item, so a guard
  that inferred intent from the revision alone refused the second attempt the
  product promises. `issues.dispatch_generation` is therefore bumped by the
  command that wants the attempt, and section 10.17 says which commands those
  are.
- Only a `succeeded` attempt suppresses anything. An attempt that ended
  `failed`, `cancelled` or `interrupted` is retried under the unchanged rules,
  because a failure is not an answer.
- The exclusion lives in the claim candidate query, like the coordinator and
  workspace exclusions, so the provider candidate preview that shares the query
  does not offer an answered item either.
- It applies to **native** projects only. On a `github_compatible` project the
  `issues` row is a projection of GitHub and the projection writes do not touch
  `revision`, which fences native edits, so comparing an attempt's recorded
  revision there would strand an item whose lane moved on GitHub. Stranding is
  the worse failure, and the loop this rule stops cannot happen on a projection
  profile anyway.
- The lane brake and this one are independent and both required. A completed
  item still has to reach a lane that is not dispatchable, and the lane the
  orchestrator promotes into is resolved against the project's own workflow:
  the configured review state when the project has it, otherwise the project's
  own review lane, otherwise its first terminal lane — the rule
  `closeCoordinatorItem` already applies. The hub already reports `terminal`,
  `dispatchable` and `operator_only` per state; the orchestrator reads them
  through the optional `connector.WorkflowStateLister`, and a tracker that
  cannot report a workflow keeps the configured name as the only authority.
- The lane brake has a readiness brake in front of it, and section 9.2.2 is
  that rule. A lane the promotion never reaches is the same as no lane at all,
  which is what the seventh dogfood run found (`operations.md` section 8).

### 9.2.2 A completed item is judged on facts its tracker can report (added September 12, 2026)

- The readiness rule for promoting a completed item out of its active lane was
  an open pull request and nothing else, for every gate kind except `artifact`.
  That is right for a tracker whose changes are reviewed on pull requests and
  wrong for one whose are not: a hub-native item has no pull request, so every
  completed item on a native project was judged not ready, the promotion target
  was empty, the loop continued in silence, and section 9.2.1's lane fallback
  was never asked for.
- **A missing connector is stated, never inferred from a missing pull
  request.** "This item has no pull request yet" and "no pull request can ever
  mirror this item's change" are different facts and they decide differently,
  and only the tracker can tell them apart. The work item resource therefore
  carries an optional change review surface,
  `GET {nativeBase}/work-items/:item?include=change`, whose `connector` is
  `"github"` when the project has a GitHub repository bound and enabled and
  `"none"` when it has none — the same binding section 18.6 reports as a null
  `connector` for "projects without a GitHub connector show the change request
  alone".
- The surface also carries the item's latest change request as a pull request
  (`change_id`, `number`, `state`, `draft`, `url`, `head_sha`), joined with the
  connector's projection whenever one mirrors it, and `revision`: the work item
  revision the recorded change covers, which is the
  `native_attempts.work_item_revision` of the latest succeeded attempt that
  posted a stored diff (section 18.5) or published a change version. That is
  the same number section 9.2.1 compares, so the claim brake and the promotion
  agree on what "the item as it stands" means.
- The rule is then: **a completed attempt is ready for review when the item
  carries a change at its current revision, whether or not the project has a
  connector**, and a pull request that *was* opened for the item must be open —
  a merged or closed one is not ready, exactly as before. A tracker that
  reports no change review surface — every non-hub tracker, and the
  `github_compatible` `issueFromWorkItem` path, where the runner itself opens
  the pull request — leaves the pull request as the only authority, which is
  the behavior every connector had before. (Amended September 12, 2026, after
  the eighth dogfood run; the first version of this rule asked whether the
  project had a connector and demanded the pull request whenever it did.)
- **Why the connector does not decide it.** A hosted project may hold a GitHub
  connector and still have no pull request for an item, because on the hosted
  flow opening one is a separate explicit action — `POST
  {nativeBase}/work-items/:id/pull-requests/actions {action: open}`, section
  18.6, executed by the merge lane and offered to a person as the header's
  Create PR button — which a conversation-driven attempt neither takes nor
  holds a hub credential for. The eighth dogfood run is exactly that shape:
  `change_review_provider=github pull_requests_available=true
  change_revision=2 issue_revision=2 missing=pull_request`, a completed item
  with its diff posted, asked for a pull request that nothing was going to
  open, sitting In Progress forever. "This project could mirror a change" is
  not "this item has an answer", and only the second question decides a
  promotion.
- The gate's pull-request requirement therefore stays in force in two places
  and no others: for a GitHub-profile project, whose runner opens the pull
  request itself and whose items carry no change review surface at all, and
  for an item that has a pull request already, which must still be open. There
  is no "open a pull request automatically on completion" setting in the gate
  configuration (`internal/gate`), so nothing else re-arms the requirement; if
  such a setting is ever added, a native item on a project that sets it is the
  third place.
- The surface is absent unless the caller asks for it, so the default work item
  resource is unchanged, and it is read at promotion time through the optional
  `connector.ChangeReviewHydrator`. The issue the orchestrator holds when it
  promotes was read when the attempt was *dispatched*, so the change the
  attempt produced is newer than it; only the change facts are carried over,
  and the item's lane, title and labels stay as the caller read them.
- A completed item that is not ready is reported once per item at warn —
  `completed issue not ready for promotion`, with the change review provider,
  whether pull requests are available, and the two revisions the comparison was
  made on. The silent skip is what hid this for seven dogfood runs, so the
  absence of a log line is part of the defect and part of the fix. The
  `missing=` vocabulary is the rule's own, and each value means one thing:
  - `change_request` — the tracker reviews changes of its own and holds none
    for the item as it stands: no change is recorded, or the recorded one is
    below the item's current revision. The item has not been answered yet.
  - `open_pull_request` — a pull request was opened for the item and is no
    longer open (merged or closed). This is a judgment about the pull request
    the item has, not about one it might have.
  - `pull_request` — the gate wants a pull request on an item whose tracker
    reviews no change of its own, so the pull request is the only evidence
    that tracker can offer and it has none. After the eighth run's amendment
    this value belongs to the `github_compatible` path and to non-hub
    trackers; a native item is never held for it, because nothing in the
    hosted flow opens a pull request on its own.

### 9.3 Worker binding and provider thread origin

- `POST /work-items/:item/conversation/bind` resolves the conversation
  through `conversations.work_item_id` first and `coordinator_items` second.
  The response gains `coordinator: true` for coordinator items.
- The conversation records where its provider thread was produced:
  `conversations.provider_thread_runner_id` (empty for the hub-side
  coordinator). `bind` returns `resume.thread_id` only when that runner id
  equals the binding runner; otherwise `resume.thread_id` is empty and
  `resume.transcript` carries the last 20 messages as
  `[{role, kind, text}]` (text bounded to 2000 runes) for the runner to
  prepend as clearly delimited data. `turn_started` records the thread id
  together with the reporting runner id.
- `turn-events` `item` gains optional `data` (JSON object, at most 16 KB) so
  a runner can post structured status such as `{"proposal": {...}}`.
- The conversation also records **which kind of turn produced the thread**:
  `conversations.provider_thread_origin` is `coordinator` or `worker`, set
  wherever the thread id is set (`turn_started`, and a `bind` that names a
  thread). A thread carries the instructions and the permission set of the
  turn that opened it — a coordinator thread is read-only with no checkout,
  a worker thread may edit, test and move issue state — so the rule is
  **origin equality**: `bind` returns `resume.thread_id` only when the
  recorded runner is the binding runner **and** the recorded origin matches
  the kind of turn that is binding. A mismatch in either direction, and an
  origin that was never recorded, is handed the transcript instead. `bind`
  also returns `resume.thread_origin`, the origin the binding turn records,
  so a different runner behaves the same way with no registry of its own.
  (Added September 12, 2026, after the third dogfood run: an issue attempt
  resumed the coordinator's thread on the same runner and answered "This
  session's coordinator restrictions prohibit editing files, running tests,
  or changing issue state, so I can't execute the worker assignment".)

### 9.4 Runner behavior

- New run mode `coordinator` and role `coordinator`. The dispatcher selects
  it when the claimed native issue carries the `detent:coordinator` label.
- A coordinator run never creates a git worktree or a deliverable: it uses a
  temporary directory workspace, `ReadOnly: true`, no repository checkout,
  no push, no pull request, and no checkpoint selection tool.
- The prompt is the coordinator instructions (identity, what it can and
  cannot do, concise Markdown answers) plus the bind's pending prompt or
  transcript; the user's messages arrive as controls. Tools:
  `list_attention` (the item's project only, from the worker API issue,
  attempt and question reads), `explain_issue` (issue, attempts, comments)
  and `propose_issue` (posts an `item` event with
  `data.proposal = {project_id, title, objective}`; creates nothing).
- The run ends when the turn completes; the runner unbinds with the outcome
  and releases the lease with `completed`.

## 10. Corrections after the first-slice review (September 10, 2026)

Findings from the spec review of the branch, with the decision taken for
each. These amend earlier sections; where they conflict, this section wins.

1. **Coordinator items carry no user text.** The coordinator issue title is
   `Coordinator turn for conversation <last 8 characters of the id>` and its
   body names the conversation id only. Private chat content never reaches
   project readers through the issue list, `include=coordinator`,
   `GET /work-items/:item` or `explain_issue`.
2. **Creating a conversation is idempotent.** `POST /conversations` requires a
   top-level `key` (client-generated, at most 128 bytes) and runs through the
   native idempotent mutation path keyed by actor and key. A retry returns
   the stored response; a different payload under the same key is
   `idempotency_conflict`. `first_message.key` remains the message command
   key.
3. **No automatic replay, explicit retry.** When a control that was handed to
   a worker (`sending`) can no longer be established because the worker
   unbound, lost its lease or the hub restarted, it becomes `unknown` for
   every kind, including text messages. A `queued` control that was never
   handed out stays `queued`. The user retries with a new command
   `{key, kind: "retry", message_id}`: allowed when that message's delivery
   is `unknown`, `failed` or `rejected` and the message belongs to the
   caller's readable conversation; it sets the delivery back to `queued`
   (or `saved` on the hub-side coordinator path) on the same message id,
   emits `message.updated` and returns a receipt `queued` for the retry key.
   Nothing is duplicated. The client's "retry" affordance sends this command.
4. **Transcript recovery is visible.** When a bind hands a runner a
   transcript instead of a resumable thread, the hub appends a
   `role: system, kind: status` message "Provider history was not available
   on this runner; continuing from a transcript of the last N messages" and
   the execution resource carries `resume: "thread" | "transcript" | ""`.
   Since September 12, 2026 a thread produced by the other kind of turn is
   one of the reasons a bind hands over a transcript, and it carries the same
   notice: section 9.3's origin rule decides, and the reason is deliberately
   not spelled out differently in the history, because from the reader's side
   both cases are the same fact — this turn is continuing from the written
   history rather than from the provider's memory.
5. **Audience preview reports the whole history.** The conversation resource
   carries `message_count`; the share confirmation shows that number.
6. **Instructions precede data.** The coordinator instructions come first in
   the prompt; the transcript and pending follow-ups follow inside delimited
   data blocks whose delimiters are escaped in the content.
7. **Interrupted turns stay interrupted.** A `turn_completed` with status
   `interrupted` or `failed` records that status on the execution and the
   runner maps it to the unbind outcome even when the provider returned
   without an error.
8. **Continue requires the expected attempt.** `continue` must carry
   `expected.attempt_id` when an attempt is or was bound (`null` only when
   the conversation never had one), matching section 5.
9. **Answers must match options.** `ValidateAnswers` rejects a value that is
   not one of the prompt's option labels unless the prompt allows free text.
10. **Hosted plan policy applies to chat-created issues.** Coordinator items
    and linked issues created from a conversation run the same hosted
    feature and growth checks as `POST /work-items`.
11. **Read-only viewers get no controls.** The composer's stop button and the
    execution strip's controls are hidden when the viewer cannot write.
12. **Archive is a client feature.** The client offers archive and unarchive
    with the copy "Archiving hides this chat from your list. It never stops a
    running issue or runner."
13. **Attribution ships with the bundle.** The built client includes the MIT
    notice for the reused T3 Code source as a leading comment.
14. **The client is reachable.** Hosted pages show a "Chat" navigation item
    linking to `/chat` when the conversation product is enabled. The
    single-tenant dashboard's operator chat drawer is a different product
    and stays as is.
15. **Client tests gate the branch.** `make check` runs the client typecheck,
    unit tests and a bundle drift check (`make app` must leave
    `static/app/conversation` unchanged).
16. **Delivered-after-revocation is deliberate.** A control accepted before
    the actor lost access is still delivered to the bound worker; acceptance
    is durable and the audit shows who sent it. The actor's later requests
    and streams are denied. (Amends the spike's write-time refusal for
    operator revocation; worker revocation still refuses the write.)

17. **A continuation is a recorded request, not an inference.** A command
    accepted on a **linked** conversation that only a new attempt can carry
    bumps `issues.dispatch_generation` (and stamps
    `issues.dispatch_requested_at`) on the linked item, which is what makes it
    a claim candidate again under section 9.2.1. Those commands are:
    `continue`, which is only valid when no attempt is live; a `message`
    accepted while no attempt is live, whose `queued` control nothing else can
    deliver; and a `retry` (item 3 above) that puts a message back on the
    ladder as `queued` while no attempt is live. A command handed to a live
    attempt bumps nothing, because the controls poll delivers it — steering a
    running turn must not schedule a turn after it. An unlinked conversation
    bumps nothing either: its coordinator item is created and retired per turn
    (section 9.1), so the next message gets a new item rather than a new
    generation on an old one. (Added September 12, 2026, after the sixth
    dogfood run: the claim-side revision guard built before this rule stopped
    the re-dispatch loop and also stopped
    `TestConversationIntegration/vertical_slice`'s legitimate continuation
    re-claim, because a `continue` bumps no revision.)

18. **A promotion is judged on the tracker's own change, not only on a pull
    request.** A completed item leaves its active lane when its tracker can
    show a change for the item as it stands; on a project with a GitHub
    connector that is still the mirrored pull request being open, and on one
    without a connector it is the hub's own change request or attempt diff
    recorded at the item's current revision. The missing connector is a stated
    fact on the work item resource, not the absence of a pull request, and a
    promotion skipped for lack of readiness is logged once per item naming the
    fact that was missing. Section 9.2.2 is the contract. (Added
    September 12, 2026, after the seventh dogfood run: section 9.2.1's claim
    brake held completely and the promotion half never ran at all, because
    `issueFromNative` sets no `PullRequest` and
    `completedActiveIssueReadyForReview` required one.)

### 10.1 Deferred from the same review

- The hub server now imports the runner package for agent-backend types
  used by the transitional hub-side coordinator. That edge disappears with
  the hub-side coordinator itself; lifting the shared vocabulary into a
  small package is not done in this slice.
- The coordinator instructions and tool scaffolding exist twice (hub-side
  coordinator and runner). The runner copy is authoritative; the hub copy
  goes away with the transitional path. Until then, changes must be made in
  both.
- A cross-process worker test and a recorded run against a real hosted hub
  with a real Codex runner remain open evidence items (see
  `operations.md` section 7).

## 11. One frontend (decided September 10, 2026)

Michael: "everything goes into the react app, we are building a brand new
frontend." The React application under `web/conversation` (T3 Code
components, artifact screens) owns every hosted screen: login, first-run
wizard, organization (members, invitations, switcher), projects, Work board
and list, issue thread with the review dock, fleet and spend, project
settings, plan, billing, support, and chat. The hosted Templ pages are
removed as each React route replaces them; the hub serves the application
shell for every non-API path and JSON for everything the Templ forms did.
The JSON contract for those endpoints is recorded in section 12 once the
inventory is complete. The single-tenant dashboard (`internal/web`) is a
different product and is out of scope.

## 12. Hosted JSON API for the frontend

Everything the hosted Templ pages did through forms becomes JSON so the
React app can own every screen. Conventions: routes live under
`/api/v2/organizations/:organization` (project-scoped ones under the
existing `nativeBase`); the browser authenticates with the hosted session
cookie; every non-GET carries `X-CSRF-Token` from the bootstrap payload and
an `idempotency_key` (at most 128 bytes) in the body; errors use the native
`{code, message, details?}` shape with `401 unauthorized` (no session),
`403 forbidden` (authenticated, not allowed), `404 not_found` (wrong
organization or unknown record), `409` for conflicts, `422 invalid_request`;
mutations keep the hosted write lock and the in-transaction membership
re-check. Existing native endpoints keep their shapes.

### Serving

- `GET /app/bootstrap` replaces `/chat/bootstrap` (kept as an alias):
  `{organization: {id, name}, organizations: [{id, name, public_url,
  current}], actor: {principal_id, subject, email, role, can_manage,
  can_manage_runners}, projects: [{id, name, profile, can_write,
  can_manage_runners, states: [...]}], support: {actor, reason,
  expires_at} | null, csrf_token, capabilities: {coordinator, attachments},
  feature: {conversation}, plan: {id, name, source, window_ends_at} | null,
  api_base}`.
- The hub serves the application shell for every GET that is not an API,
  auth, webhook or static path: `/`, `/login`, `/work*`, `/chat*`,
  `/organization*`, `/projects*`, `/settings*`, `/fleet*`, `/support`.
  Unauthenticated requests to any of them except `/login` redirect to
  `/login`; `/login` renders the shell so the React login card can send the
  user to `/auth/oidc/start`. The Templ hosted pages, their generated code,
  `static/js/hosted-*.js` and their HTML tests are removed.
- `/auth/oidc/start`, `/auth/oidc/callback`, `/invite`, `/webhooks/stripe`
  stay as they are. A successful login redirects to `/work`. `POST /logout`
  answers 204 for JSON callers (form callers keep the redirect).

### Organization

- `GET /members` → `{members: [{id, user_id, email, role, status, grants:
  [{project_id, write, runner}]}], invitations: [{id, email, role,
  created_at, expires_at}]}` (owner/admin; members see only themselves).
- `POST /members/invitations {email, role, idempotency_key}` → 201 invitation.
- `DELETE /members/:member` → 204 (never the last owner).
- `PUT /members/:member/role {role}` → 200 member.
- `PUT /members/:member/grants {project_id, write, runner, revoke}` → 200 member.
- `POST /api/v2/organizations {name, idempotency_key}` for the bootstrap
  subject creating the first organization → 201 `{organization, next:
  "/auth/oidc/start"}`; `POST /invitations/accept {token}` → 200 `{next}`;
  `POST /switch {organization}` → 200 `{next: <public_url>/auth/oidc/start}`.
- `POST /support/start` → 200 `{support}`; support state is also in the
  bootstrap.

### Projects and settings

- `GET /projects` → `[{id, name, profile, states, can_write,
  can_manage_runners, onboarding: {ready, steps}}]`.
- `POST /projects {name, grant_access, idempotency_key}` (owner/admin) → 201
  project (already exists; hosted admins keep access).
- Settings use the existing `GET/PUT {nativeBase}/integration`, `GET
  {nativeBase}/policy`, `PUT {nativeBase}/onboarding/policy`, the
  onboarding endpoints, runner enrollment and routing endpoints. The
  first-run wizard is the artifact's screen 5 over
  `GET {nativeBase}/onboarding` and the same mutations
  `static/js/hosted-setup.js` performs today, with the same idempotency
  keys persisted client-side.

### Fleet and spend

- `GET /fleet` (any member) → `{runners: [{id, display_name, hostname,
  health, state, os, architecture, host_capacity, host_used, capacity_limit,
  reported_capacity, provider_capacity: [...], last_heartbeat_at, leases:
  [{lease_id, work_item_id, title, project_id, expires_at}]}], usage:
  {window_ends_at, allowances: {name: {used, limit}}}, spend: {today,
  window, by_project: [{project_id, amount}]} | null}`. Runner-management
  actions keep their existing endpoints and grants.

### Plan, billing, support

- `GET /plan` (owner/admin) → the plan usage report (`hostedPlanUsage`):
  entitlement, allowances with limits, grants, window.
- `GET /billing` (owner, no impersonation) → the billing report;
  `POST /billing/checkout {price, idempotency_key}` → 200 `{url}`;
  `POST /billing/portal {idempotency_key}` → 200 `{url}`.
- `GET /projects/:project/events` moves to `GET {nativeBase}/events` with
  the same SSE payload; the old path stays as an alias for one release.

### Work

- Board and list read `GET {nativeBase}` (states) and `GET
  {nativeBase}/work-items` (filters `state`, `label`, `assignee`,
  `priority`, cursor). Cards need running attempts and changes: add
  `include=attempts,changes` to the list endpoint returning, per item,
  `latest_attempt: {status, identity, started_at, updated_at} | null` and
  `changes: [{id, title, state, url}]`. Lane moves use `POST
  {nativeBase}/work-items/:item/workflow`.

## 13. Decisions from Michael's assumption review (September 10, 2026)

1. Everything becomes React; Detent becomes an API. The single-tenant Templ
   dashboard (`internal/web`) is replaced by the same React app in a later
   slice; the hosted screens come first.
2. The merged sidebar model (issue lanes plus chats) is good enough for now
   and subject to change.
3. Core layout and core components must be literal copies of T3 Code's,
   including T3's sidebar provider, tooltips, hover preview cards and row
   icons; adaptation is only allowed for data bindings, handlers and
   Detent-only content.
4. The review dock ships as Diff / State / Receipt / Activity first; visual
   review later.
5. Naming stays: Chat, New chat, Needs you.
6. Sharing rules stand (private by default, whole history on link).
7. One conversation per issue and one issue per conversation, but
   conversations and messages can reference other issues and chats the way
   GitHub references work (`#123` links, cross-references shown on the
   target).
8. Creating an issue from chat prompts the user for what to do next (lane,
   priority, dispatch now or later) with Detent's project defaults
   preselected.
9. Settled replaces Archive: finished and idle chats group under Settled as
   in T3; there is no archive action.
10. Explicit retry, no automatic redelivery: confirmed.
11. Model turns run on customer runners: confirmed.
12. Claude Code live control is a tracked todo on the main Detent repository
    for Cory; the frontend is capability-driven so it lights up when the
    adapter lands.
13. Coordinator item lifecycle: confirmed.
14. The composer keeps T3's model, effort and access pickers as real
    controls. "Auto" is the default and means Detent's configured
    preferences; a user override applies to that conversation's turns.
15. Tokens stay the source of truth so themes, colors and light mode can be
    added later; dark only for now.

## 14. Contract additions from the assumption review

- **Turn preferences (13.14).** A conversation carries `preferences:
  {model: "auto" | <model id>, reasoning_effort: "auto" | low | medium |
  high, access: "auto" | read_only | full}`; `PATCH /conversations/:id`
  accepts them. `auto` resolves to the project's configured defaults on the
  runner (the same source as the `detent-agent` override); explicit values
  travel to the coordinator turn and, for linked issues, are written into
  the issue's `detent-agent` block on link and on later change. The
  bootstrap lists the choices: `preferences: {models: [{id, label,
  default}], efforts: [...], access: [...]}` from the enrolled runners'
  provider reports and policy. The composer shows T3's model, effort and
  access pickers with these options and "Auto" preselected.
- **References (13.7).** Message text may reference `#123` (an issue number
  in the same project), `org/project#123`, or a conversation id. The hub
  extracts references on accept into `message_references (message_id,
  target_kind, target_id)`, the message resource carries
  `references: [{kind, id, label, url}]`, and the target's activity shows
  "referenced from" entries; `GET {nativeBase}/work-items/:item/references`
  lists them. The client renders references as links.
- **Handoff next step (13.8).** The link request gains `next: {state,
  priority, dispatch: "now" | "later"}`; the form preselects the project's
  defaults (first dispatchable state, default priority, dispatch later) and
  the user can change them. `dispatch: "now"` places the issue in the first
  dispatchable state; `later` in Backlog when the project has one.
- **Settled (13.9).** Archive endpoints and the `archived` status are
  removed. Conversations are `active` or `settled`; a conversation settles
  automatically when its execution is terminal or idle for the project's
  settle window (default 24 hours) and unsettles on new activity; the list
  endpoint accepts `settled=true|false` and the sidebar groups Settled as T3
  does with "Show N more".

## 15. Port T3's whole main page (September 10, 2026, evening)

Michael: "All of it: incorporate as much as possible from the header to
the surfaces. This whole main page; the chat features down the line use as
much of this app as we can. I'd rather pull things out or modify than have
to ask again for the UI to look and feel the same."

Consequences: T3's ChatHeader (breadcrumb, Add action, Open, Commit-push-PR
split buttons, panel toggles), sidebar shell (provider, rail resize,
animations, tooltips, hover cards), composer, timeline and the right panel
with its surface picker (Browser, Terminal, Files, Diff, Pull request,
Agents) are ported wholesale. Surfaces Detent can feed (Diff, Pull request)
are live; the others stay present but disabled until decided. (Agents was live
too, and is now removed outright rather than disabled — section 18.10, and the
only surface that call applies to.)
Detent's Work issue page uses the same layout. Anything T3-only is removed
or modified afterwards, never omitted up front. Acceptance is a
browser-verified interaction checklist, not screenshots.

## 16. Governing rule for the frontend (September 10, 2026)

Michael: "Use the components in their entirety when it comes to the visual
identity, and only when things pull back from the API let's see how we can
move those to Detent functions."

Rule: T3 Code components are copied whole and unmodified for everything
visual and interactive (markup, classes, tokens, icons, animations, hover
and focus states, shortcuts, menus). Detent enters only at the data
boundary: the hooks, stores and clients a component reads from are replaced
by Detent adapters that supply the same shapes. No visual cut, rename or
re-layout without Michael's say-so; T3-only features stay present (live,
or disabled with a tooltip) until he decides to pull them out.

## 17. Usage, attachments, settings and sidebar corrections (September 10, 2026, late)

Michael's review items and the contract for each:

1. **Dropzone.** The whole chat surface is a file dropzone as in T3
   (`workspaceFileDrop.ts`, `ChatComposer` attachment chips); attachments
   are real, not deferred. Contract: `POST {nativeBase}/conversations/:id/
   attachments` (multipart, fields `file`, `idempotency_key`; limits: 20 MB
   per file, 10 per message; images and text types; stored through the
   existing artifact service) → 201 `{id, name, mime, size, url, expires_at}`;
   `DELETE .../attachments/:attachment` while unsent; message commands carry
   `attachments: [id]`; the message resource lists `attachments: [{id, name,
   mime, size, url}]`; reads enforce the conversation's audience; the runner
   receives image and text attachments as provider input.
2. **Brand row.** The sidebar wordmark reads "Detent Cloud" as T3's reads
   "T3 Code"; the organization lives in the switcher row.
3. **Settings.** T3's settings page components (`components/settings/**`:
   layout, icon nav, section headers, item rows, dialogs) are copied whole;
   Detent settings map onto them (General, Organization, Projects,
   Providers/Runners, Integrations, Plan, Billing, Keybindings, About).
4. **Browse group.** Entries duplicated by the footer icons (Pull requests,
   Fleet/Usage, Settings) leave Browse; Browse keeps Activity, Diagnostics,
   Reports, Library.
5. **Usage page.** T3's `UsagePage` (Cost / Tokens / Limits tabs, range
   control, hero total, provider rows, daily chart, totals, breakdown by
   model or day) is copied whole and shown for both tokens and runners.
   Contract: runners report usage per attempt through the run event
   `run.checkpointed`/`run.finished` data (`usage: {provider, model, input,
   cached_input, output, cost_estimate, currency}` accumulated per turn),
   the hub stores it in `attempt_usage (attempt_id, day, provider, model,
   input, cached_input, output, cost_estimate)`, and
   `GET /api/v2/organizations/:org/usage?range=24h|7d|30d|90d&project=` →
   `{range: {from, to}, total: {cost, tokens, sessions}, providers: [{id,
   label, sessions, cost, share, tokens}], daily: [{day, cost, tokens,
   by_provider: {id: cost}}], totals: {processed, cached_input,
   uncached_input, output, cache_savings}, breakdown: {by_model: [{model,
   provider, cost, share, tokens}], by_day: [...]}, limits: {allowances from
   the entitlement with used/limit}, runners: [{id, display_name, sessions,
   tokens, cost, busy_seconds, capacity_used}]}`. Cost is an estimate from a
   per-model price table the hub keeps (`usage.prices` config) and is
   labelled "API estimate" as T3 does.

## 18. Right panel surfaces, workspace sessions and review sandboxes (September 11, 2026)

Michael's direction after the T3 right panel landed: the surfaces are real,
not decoration. "Browser for visual verification, terminal to access the
runners, files to see the project, diff to see the diff, pull requests to see
what's going on there." Agents can wait. He also asked for a sandbox that
runs a pull request in the cloud where the reviewer can "view the current
state" without a live stream. This section is the contract for all of it.
It was reviewed three times by Codex (gpt-6-astra) on September 11, 2026;
the draft below folds in all three, and the third pass reported no
remaining blocker.

Principles, all consequences of section 1:

- Every surface is a runner-side capability streamed through the hub. The
  hub never opens a shell, reads a checkout or runs a preview itself. A
  Detent-hosted review sandbox (18.8) is the one exception, and it runs no
  model.
- Nothing connects inbound to a customer machine. Terminals, files and diffs
  ride the hub's relay the way controls and turn events already do (section
  5, worker endpoints). "SSH into the runner" means a PTY on the runner over
  that relay, not network SSH.
- A surface enables per runner, from the capabilities the runner reports,
  and shows T3's disabled card with the reason otherwise (section 16).
- Everything a person does through a surface is audited and fenced by an
  owner tuple of its own (18.1), re-checked on both sides as section 2
  requires.
- What a surface exposes is bounded by what the person could already read.
  A shell, a file read, a diff, a recording or a screenshot never widens an
  audience; where a surface could leak something the audience may not see,
  the contract says how it is filtered or says plainly that it is not.

### 18.1 Workspace sessions

A surface needs a worktree, and today a worktree exists only while an
attempt runs. A workspace session is the durable object the surfaces attach
to: an attempt's worktree kept alive after the run, or a fresh checkout
opened on purpose ("spin up the worktree and review").

Resource. `{id: ws_<32 hex>, organization_id, project_id, work_item_id,
attempt_id | null, ref, head_sha | null, runner_id | null, machine_id | null,
state, reason | null, requires: [capability], capabilities: {terminal, files,
diff, preview} | null, isolation: "user" | "container" | null,
idle_timeout_seconds, expires_at, opened_at | null, last_activity_at | null,
created_by, relay_sessions: [...] (owners and admins only), revision}`.

- `POST {nativeBase}/workspaces` `{work_item_id | attempt_id | ref,
  runner_id?, requires: [files | terminal | diff | preview]}` → 201. The
  actor needs `write` on the project; `terminal` in `requires` also needs
  the grant's `runners` flag. `requires` is what claim gating matches
  against; a request without it gets `files` and `diff`.
- `GET {nativeBase}/workspaces?work_item=&state=` and `GET
  {nativeBase}/workspaces/:id` follow the issue's read rule. `DELETE
  {nativeBase}/workspaces/:id` (write) closes it. Every transition emits
  `workspace.<state>` on the project event stream (section 12) with the
  resource as `data`, so a client observes readiness by subscription and
  never by polling.
- States and their only legal transitions:
  `requested → starting → ready ↔ idle → closing → closed`,
  `requested | starting | ready | idle | unreachable → failed {reason}`,
  `ready | idle → unreachable → ready` (runner heartbeat lost then
  regained), `unreachable → closed` after `idle_timeout_seconds`, and
  `starting | ready | idle | unreachable → requested {hub_restarted}` on
  hub restart only, `requested → ready` when the original runner re-binds
  within the restart grace, and `requested | starting → closing` on
  `DELETE` or on `expires_at`.
  Reasons: `no_runner`, `checkout_failed`, `worktree_missing`,
  `runner_restarted`, `hub_restarted`, `lease_lost`, `capacity`,
  `closed_by_actor`, `expired`.
- Dispatch. A workspace is its own work item kind, `detent:workspace`,
  distinct from the coordinator kind in section 9: it may check out the
  repository, it does not end when a turn ends, and it stays claimed until
  the workspace closes. A runner may claim it only if it reports every
  capability in `requires` with a fresh heartbeat (section 9.2 freshness),
  holds an active grant for the project, and, when `attempt_id` names a
  retained worktree, is the runner that ran that attempt. With no eligible
  runner the workspace stays `requested` for `workspaces.request_timeout`
  (default 5 minutes) and then fails with `no_runner`.
- Ownership generation. Claiming creates a workspace lease with its own
  fencing token. The workspace owner tuple is `{workspace_id, runner_id,
  machine_id, lease_id, fencing_token}`. It is not the attempt's tuple: a
  finished attempt's lease is released and never revived. Nor is it an
  execution lease: no model, gate or command runs beneath it, so it holds
  nothing a policy approval could invalidate and it is not one of the "active
  leases" that refuse a policy change. It has to be excluded explicitly, not
  left to time: a workspace renews its lease for as long as it is open, so
  counting it would block every policy change in the project with no wait that
  ends. Worker endpoints mirror the conversation worker set in section 5: `POST
  {nativeBase}/workspaces/:id/worker/bind` (claim, returns the tuple and the
  checkout instructions), `POST .../worker/heartbeat` (renews the lease,
  carries `state`, `head_sha`, `capabilities`, `isolation`), `POST
  .../worker/unbind {reason}`. Two missed heartbeats (30 seconds each) mark
  the workspace `unreachable`; lease expiry (90 seconds) fails it with
  `lease_lost`.
- Worktree rules. One workspace per worktree at a time; a second request on
  the same attempt while one is open returns 409 `workspace_exists` with the
  existing id. A workspace on an attempt that is still running is allowed
  with `files` and `diff` only, read-only, and refuses `terminal` until the
  attempt finishes, so a person cannot type into a worktree the model is
  editing. `workspaces.retain_after_run` (default 30 minutes) keeps an
  attempt's worktree available after the run; after that a workspace on the
  attempt checks out `head_sha` fresh and reports `attempt_id` with
  `worktree: "fresh"`.
- Lifetime. `idle_timeout_seconds` from `workspaces.idle_timeout` (default
  30 minutes) and a hard cap `workspaces.max_lifetime` (default 4 hours).
  Only person-originated frames reset idle (input, requests, resizes);
  runner output and watches do not, so a busy shell left alone still
  expires. Closing a workspace never deletes the attempt's artifacts.
- Recovery. Hub restart: every open workspace is re-marked `requested`
  with `reason: hub_restarted` and the original runner has 30 seconds to
  re-bind with its existing tuple, which returns the state to `ready` with
  no relay sessions carried over. After 30 seconds the hub revokes that
  lease and re-dispatches under a new generation; a late bind with the old
  tuple is refused with `stale_execution`. There is no state in which two
  runners hold the workspace. Runner restart: the runner unbinds every
  workspace it held with `runner_restarted`; the workspace fails. Deleted worktree: the runner reports `worktree_missing` on
  heartbeat and the workspace fails. A failed workspace is never resumed;
  the person opens a new one.
- Capacity and accounting. A workspace reserves one slot of the runner's
  capacity at claim, when the bind succeeds and the state becomes
  `starting`, so two claims cannot both be admitted against one remaining
  slot; the reservation is released at `closed` or `failed`. The hub writes
  `workspace_occupancy (workspace_id, runner_id, started_at, ended_at)`
  from `starting` and from the closing event; an `unreachable` workspace
  whose lease expires is ended at lease expiry, so a crashed runner does
  not accrue time. The usage report's runner rows (section 17.5) add
  `workspace_sessions` and `workspace_seconds` from that table, never from
  relay traffic, so double counting is impossible by construction.
- Limits. Per organization, `plan.workspaces.max_open` (default 20) open
  workspaces; per person, 3 at a time. Beyond either the request fails with
  422 `workspace_limit`. Allowance exhaustion on a plan that meters
  workspaces answers 402 `allowance_exhausted` with the allowance named.

### 18.2 The relay

The hub forwards frames between a person's connection and the runner's
connection for one workspace, with its own checks in the middle. There may
be many person connections (tabs, people) and exactly one runner connection
per workspace.

- Person side. Browsers cannot set headers on a WebSocket upgrade, so the
  CSRF rule of section 12 is met with a ticket: `POST
  {nativeBase}/workspaces/:id/relay-tickets` (hosted session, CSRF header,
  `Origin` checked against the hub's public URL) → 201 `{ticket, expires_in:
  30}`; the client then opens `GET {nativeBase}/workspaces/:id/relay?ticket=`
  and the hub redeems the ticket once, binds the connection to the session
  that minted it, and rejects any other `Origin`. A ticket is 32 bytes of
  randomness, single use, and bound to the workspace and the session.
- Runner side. `GET {nativeBase}/workspaces/:id/worker/relay` under the
  worker token, fenced by the workspace tuple; a second runner connection
  for the same workspace replaces the first (the old one closes with
  `superseded`).
- Frames are JSON `{channel, stream, type, seq, payload}`. `channel` is one
  of `terminal | files | diff | preview`; `type` is from the table for that
  channel (18.3 to 18.7) and anything else is answered with `{type: "error",
  code: "unknown_frame"}` and dropped. A frame is at most 256 KB; larger
  payloads are sent as `chunk {part, parts, data}` frames the receiver
  reassembles, up to the channel's read cap (18.4, 18.5). Binary data is
  base64 in `payload.data` with `payload.encoding: "base64"`.
- Streams. The hub allocates stream ids as `<connection_id>:<n>` when a
  person opens one (`open` on `terminal`, or the first request on `files`,
  `diff` or `preview`); a stream belongs to that person connection and only
  it sees the stream's frames. The runner sees, on every person-originated
  frame, `actor: {principal_id, subject, connection_id}` stamped by the hub
  and never supplied by the client. Terminal streams are never shared
  between connections; a second tab that wants the same shell opens its own
  stream and gets its own PTY. Per workspace at most 8 streams per
  connection and 32 in total; beyond that `open` fails with
  `stream_limit`.
  On `files`, `diff` and `preview` a stream-less frame after the first is the
  same conversation continuing, not a second one: the hub reuses the stream
  that connection already has open on the channel and allocates only when it
  has none. Those channels are request/response, so allocating per request
  would let an ordinary client spend its whole budget on one directory listed
  eight times and then be refused `stream_limit` on a connection holding eight
  streams nobody is using. `terminal` is the exception in both directions:
  every `open` allocates, because every open is its own PTY.
  A person ends a stream with `close {stream}`, which the hub answers with
  `closed`, forwards to the runner, and counts back against both limits; the
  answer does not depend on a runner being attached, because a person must be
  able to give up a stream whatever the runner is doing. A closed id is never
  handed out again: the count that limits streams falls, the counter that
  names them does not. A stream also returns to the budget when the runner
  closes it and when its connection ends.
- Sequencing and delivery. `seq` is per stream per direction, starting at
  1. Each side acknowledges with `ack {through}` at least every 32 frames
  or 1 second. The hub buffers at most 1 MB per stream per direction of
  unacknowledged frames and closes the stream with `overflow` beyond that;
  total relay memory is capped by `workspaces.relay.memory` (default 256 MB
  per hub process), beyond which new streams fail with `relay_busy`. On
  reconnect a person mints a new ticket from the same hosted session and
  sends `resume {stream, last_seq}` on the new connection; a stream may be
  resumed only by a connection whose principal and session match the ones
  that opened it, and only within 60 seconds of the old connection closing,
  which is also how long the hub keeps the replay buffer and how long the
  runner keeps a disconnected PTY alive (18.3). Anything else answers
  `resume_failed` and the stream closes.
  Person-originated frames are never replayed by the hub or the runner:
  a terminal `input` whose acknowledgement never came is reported to the
  person as `unknown_delivery {seq}` and dropped, the same rule section 5
  applies to controls whose outcome cannot be established.
- Authority checks. On every person-originated frame the hub re-validates
  the workspace tuple against the current lease and the hosted session's
  validity. The person's authority is the full list: session not expired,
  membership active, project read for any stream, `write` for a terminal
  or a capture request, the grant's `runners` flag for a terminal, owner or
  admin role for a `user`-isolation terminal, `workspaces.terminal.enabled`
  still on, and a support session still within its window. The hub
  evaluates that list at connect and again immediately whenever any of its
  inputs changes: membership, role, grant, session and setting mutations
  emit `authority.changed {principal_id | organization_id}` internally, and
  the relay closes every affected connection with `revoked` on that event.
  A 30-second periodic re-check is the fallback for anything the event
  misses. On `revoked` the runner receives `close` for each of that
  connection's streams. The runner validates its own lease
  against the tuple immediately before it acts on a frame (writes to the
  PTY, opens a file, runs git); a frame that arrives after lease loss is
  dropped with `stale_execution`, never executed.
- Audit. Every person connection writes `workspace_relay_sessions (id,
  workspace_id, principal_id, subject, connection_id, channels, opened_at,
  closed_at, close_reason, bytes_in, bytes_out, recording_artifact)`.
  Support actors' rows carry the support reason.
- Backpressure never crosses into control delivery: the runner keeps the
  relay and the control long-poll on separate connections and goroutines.

### 18.3 Terminal

The T3 terminal drawer (`ThreadTerminalDrawer`, `xterm`) over a PTY on the
runner.

What it is. A terminal is the runner account's authority, handed to a
person. Stripping environment variables does not confine a shell: the
process can read the runner's provider login files, its credential store,
other checkouts and anything else that user can read. The contract does not
pretend otherwise. It offers two isolation levels the runner declares in
`isolation` and the organization chooses between:

- `user`: the PTY runs as the runner's user in the worktree. Provider
  credentials and Detent tokens are removed from the environment as a
  courtesy, not a boundary. Allowed only when the organization has set
  `workspaces.terminal.isolation: user` explicitly and the person is an
  owner or admin with the `runners` grant. The setting page says in words
  what this level exposes.
- `container`: the runner starts the PTY inside a container (or equivalent
  the runner implements) that mounts only the worktree, has no access to
  the runner's home directory, credential files or sockets, and runs as an
  unprivileged user. Members with `write` and the `runners` grant may use
  it. This is the default the setting offers and the only level a viewer of
  the setting page sees as recommended.

A runner that cannot provide `container` reports `isolation: "user"` and
the terminal card stays disabled for organizations that require
`container`, with the reason.

- Frames. Person → runner: `open {cols, rows, cwd?}` (cwd relative to the
  worktree; shell is the runner's choice), `input {data}`, `resize {cols,
  rows}`, `close`. Runner → person: `opened {pid, isolation}`, `output
  {data}`, `exit {code, signal}`.
- Gating, in order: workspace readable; `write` on the project; the grant's
  `runners` flag; `workspaces.terminal.enabled` (default off, an owner turns
  it on); the isolation rule above; and the workspace not being on a running
  attempt (18.1). Viewers never get a terminal. A support actor gets one
  only with a support reason recorded and only at `container`.
- Recording. `workspaces.terminal.record` (default on) stores each stream
  as an asciicast v2 artifact referenced from the relay session row. Its
  audience is the person who ran the session plus owners and admins, never
  the issue's readers: a recording can carry what the runner account can
  see, which is wider than the issue. A `user`-isolation recording is
  readable by owners only. Recording cannot remove
  what the person typed; the setting page says so. Known secret values from
  the project's secret store (18.7) are scrubbed from recordings on write,
  best effort.
- Runner side. The PTY is killed on `close`, when the person's connection
  has been gone for 60 seconds without a resume (18.2), when the lease is
  lost, and when the workspace closes. Idle in the terminal does not keep the workspace alive; a person's
  input does (18.1).

### 18.4 Files

T3's file picker and file view (`files/ProjectFilePicker`,
`components/files/**`) over a read-only file service on the runner.

- Frames. `list {path, cursor?}` → `listed {path, entries: [{name, kind:
  file | dir | symlink, size, modified_at, ignored, denied}], next_cursor |
  null}` (500 entries per page); `stat {path}` → `stat {path, kind, size,
  modified_at, mime, ignored, denied}`; `read {path, offset?, length?}` →
  `content {path, mime, size, offset, data, truncated}` (chunked per 18.2);
  `watch {path}` → `changed {path, kind}` or `unsupported` (the client then
  refreshes on focus). Errors: `not_found`, `forbidden`, `too_large`,
  `denied`.
- Containment is enforced at open time, not on the request string. The
  runner opens with `O_NOFOLLOW` on the final component, resolves the
  canonical path of what it opened, and refuses anything whose canonical
  path is outside the worktree's canonical path, anything that is not a
  regular file or directory (devices, sockets, FIFOs), and any mount point
  under the worktree. A symlink is listed as `symlink` with no target and
  reading it answers `forbidden`. This closes symlink swaps and links to
  in-tree secrets.
- Limits. 2 MB per read (T3's "too large" state beyond that), directory
  listings paged at 500 entries, at most 4 reads in flight per stream.
- Denylist, applied to the canonical path: `.git/objects`, `.git/config`,
  `node_modules`, and files matching the project's secret patterns
  (`.env*`, `*.pem`, `*.key`, `*.p12`, `id_rsa*`, and `workspaces.files.deny`
  globs). Denied entries are listed with `denied: true` and never read; the
  client shows T3's redacted state. `.gitignore` is honoured for listing
  unless the reader toggles "show ignored".
- Writes are out of scope for this slice. Editing happens through the
  model or the reader's own checkout.

### 18.5 Diff

Two sources, one surface, one filter.

- Producer identity. Every diff write names its producer and is fenced by
  the producer's lease, not the subject attempt's: `producer: {kind:
  attempt | workspace, id, runner_id, lease_id, fencing_token}`. While an
  attempt is running its own lease is the producer; after it finishes only
  a workspace lease on that attempt may write to it. The hub validates the
  producer the way `appendNativeRunEvent` validates a run event today
  (current lease, approved policy, authenticated runner) and rejects
  anything else with `stale_execution`.
- Stored attempt diff. At every `run.checkpointed` and at `run.finished`
  the runner posts `POST {nativeBase}/attempts/:id/diff` (worker token)
  `{producer, generation: {source: attempt, seq}, base_sha, head_sha, files:
  [{path, old_path?, status: added | modified | deleted | renamed,
  additions, deletions, binary, patch, truncated, denied}]}`. For an
  attempt producer `seq` is the run event sequence the diff belongs to; the
  runner posts the diff before the event that references it, while it still
  holds the lease, and the final diff is posted before `run.finished`. A
  `seq` at or below the stored one is rejected with `stale_generation`. A
  workspace producer writes `generation: {source: workspace, id, seq}` with
  its own monotonic counter; those diffs are stored beside the attempt's,
  never over them, and `GET .../diff?source=workspace` reads the latest of
  them while the default read stays the attempt's own final diff. A patch over 1 MB
  is stored truncated with `truncated: true`; a diff over 20 MB is rejected
  with `diff_too_large` and the runner posts the file list without patches.
  `GET {nativeBase}/attempts/:id/diff` returns the latest under the issue's
  read rule; `?at=<seq>` returns that attempt-produced one.
- Issue-addressed read (added September 12, 2026).
  `GET {nativeBase}/work-items/:item/diff` → 200 `{diff: AttemptDiff | null}`:
  the latest diff stored against the issue, whichever attempt produced it,
  under the same read rule and with the same `?source=` parameter. The
  attempt-addressed read above answers 404 for an attempt that posted no diff,
  which is right for a caller that named one attempt and wrong for the panel,
  whose question is "does this issue have a diff at all". Asking per attempt
  meant walking the attempt list and taking a 404 — and a browser console
  error — for every attempt that never ran long enough to checkpoint, on every
  issue open rather than only when the surface is on screen. One request with
  a nullable answer says the same thing and says it once; migration 00026's
  `attempt_diffs_item_idx (organization_id, project_id, work_item_id,
  created_at)` is the index it was already given. Ordering across attempts is
  by `created_at`, because `seq` is monotonic within one attempt's generations
  and means nothing between them.
- Workspace diff, live. Relay channel `diff`: `request {base?}` → `diff
  {...same shape...}` chunked per 18.2, computed by the runner against
  `base` (default the attempt's `base_sha`, else the project default
  branch), at most one in flight per stream.
- Filter. The files denylist of 18.4 applies to diffs by path string, on
  both `path` and `old_path`, because a deleted or renamed file has no
  current canonical target: a file whose `path` or `old_path` matches a
  denied pattern appears with `denied: true`, its `patch` empty and its
  counts intact, whatever its status. The same rule applies to the stored diff, so a
  later reader is not shown what the live reader was not.
- Renderer. Detent Cloud uses `@pierre/diffs` under Apache-2.0. See
  [third-party notices](../../web/conversation/THIRD_PARTY_NOTICES.md).
  The Diff surface renders the stored diff with Shiki syntax highlighting,
  `chat/ChangedFilesTree.tsx` for the file list, and `FileDiff` for the hunks.
  The change version card is the fallback when no attempt has posted a diff.
- Hunk comments. `POST {nativeBase}/attempts/:id/diff/comments`
  `{idempotency_key, sha, path, side: old | new, line, body}` (write on the
  project). If the sha is not the stored diff's `head_sha` the hub answers
  409 `stale_diff` with the current sha. With a pull request on the issue
  the comment is posted as a review comment through the connector and the
  response carries its URL; otherwise it becomes a conversation message
  with a reference of the new kind `file` (`{kind: "file", path, line, sha}`
  added to section 14's reference kinds).

### 18.6 Pull requests

T3's `PullRequestDetailPanel`, ghosts and empty states whole, over the hub's
change requests joined with the GitHub connector.

- `GET {nativeBase}/work-items/:id/pull-requests` → `[{id, number, title,
  state: open | closed | merged, draft, url, head: {ref, sha, repository},
  base: {ref}, author, from_fork, mergeable: true | false | unknown, checks:
  [{name, status, conclusion, url, completed_at}], reviews: [{author,
  state, submitted_at}], review_decision, labels, updated_at, fetched_at}]`.
  The hub caches the connector view for 60 seconds and refreshes on the
  connector's webhook when one is configured; `?refresh=1` forces a fetch,
  limited to one per 10 seconds per organization.
- Projects without a GitHub connector show the change request alone with
  T3's "unavailable" state naming what is missing.
- Actions. Opening a pull request has no number yet, so it is addressed by
  the issue: `POST {nativeBase}/work-items/:id/pull-requests/actions`
  `{idempotency_key, action: open, expected_head_sha}`. Actions on an
  existing pull request are `POST
  {nativeBase}/work-items/:id/pull-requests/:number/actions`
  `{idempotency_key, action: update_branch | merge, expected_head_sha}`
  (write; `merge` additionally follows the change-request merge policy).
  Both answer 202 `{work_item, action_id}`. The action is executed by the existing merge
  queue on a runner with a checkout, never by the hub against GitHub
  directly; a head that moved since `expected_head_sha` fails the action
  with `head_moved` and the panel offers to retry. The header's git action
  group enables per action when the policy allows it and stays disabled
  with the reason otherwise (section 16). Section 18.13 is that group: what
  Commit and Push act on, and how `open` is reached from it.

### 18.7 Browser preview: snapshot first

The Browser surface shows the current state of the application under
review as captures, not a live stream. Captures are cheap, need no tunnel,
carry no live credentials, and work on a phone.

- Project configuration, in project settings and stored hub-side:
  `preview: {command, url, ready_timeout_seconds, routes: [{path, name,
  viewport?}], env_from_secrets: [names], allowed_hosts: [], on_pull_request:
  false, secrets_policy: trusted_only | never}`. `url` must be `http://` on
  a loopback address with an explicit port; the runner starts `command`,
  and readiness means the process it started is the one listening on that
  port and `url` answers 200 within `ready_timeout_seconds`. Where the
  preview runs: a capture that receives secrets runs from a clean, detached
  checkout of the trusted `sha` in a scratch directory the runner creates
  for the generation and deletes after it (`git worktree add --detach`),
  never from the workspace's worktree, which a terminal may have edited;
  the runner verifies `HEAD == sha` and a clean status before injecting
  anything. A capture of the workspace worktree itself is allowed only
  without secrets and is labelled `source: {kind: worktree, sha, dirty}`.
  Execution boundary: the preview process on a runner runs inside the same
  `container` isolation as a terminal (18.3), with the capture network
  policy enforced by that container's network namespace, not only by the
  browser. A runner that reports `isolation: user` may run previews only
  for trusted refs, only when the organization has opted into
  `workspaces.terminal.isolation: user`, and never for a sandbox of an
  untrusted ref. A port already
  bound before the start fails the capture with `port_in_use` rather than
  photographing an unrelated service. At most 20 routes.
- Capture network policy. The capture browser may navigate only to `url`'s
  origin and hosts in `allowed_hosts`; every other request, including
  redirects, subresources, loopback ports other than `url`'s, private
  ranges and cloud metadata addresses, is blocked at the browser's request
  layer and counted in the capture's `blocked_requests`.
- Limits. 60 seconds per route, one route at a time, PNG at most 10 MB, DOM
  at most 5 MB, console at most 1 MB (truncated with a marker). The preview
  process runs under the runner's resource limits and its whole process
  tree is killed on completion, timeout or cancel.
- Generations. A capture request creates a generation `{id: cg_<32 hex>,
  source: {kind: checkout | worktree, sha, dirty}, requested_at,
  requested_by | null}`; every capture in it carries the generation, so a
  screenshot attests to a commit only when `source.kind` is `checkout`, and
  the panel labels a worktree capture "uncommitted" and refuses to compare
  it against a base commit. "Latest" is the newest
  generation that completed, per route; a capture posted for a generation
  older than the stored latest is kept as history, never shown as current.
- Writes (worker token, fenced by the producer as in 18.5): `POST
  {nativeBase}/attempts/:id/captures` `{producer, generation, route,
  taken_at, viewport, screenshot: artifact_id, dom: artifact_id, console:
  artifact_id, blocked_requests, status: ok | timeout | error | port_in_use,
  error?}`.
- Reads (issue read rule): `GET {nativeBase}/attempts/:id/captures` lists
  the latest per route with `generation` and `history` counts; `?route=` and
  `?before=` page history. `POST {nativeBase}/attempts/:id/captures/refresh`
  `{routes?}` (write) creates a generation and delivers a `capture` control
  to the workspace bound to that attempt over its control channel, exactly
  as a conversation control reaches a bound worker (section 5): no second
  lease, no second capacity slot, the workspace lease is the producer. The
  workspace must report `capabilities.preview`; one opened without it
  answers 409 `capability_missing` and the client offers to open a preview
  workspace. With no workspace open the hub opens one with `requires:
  [preview]` and queues the control for it. A workspace on a running
  attempt answers 409 `attempt_running`. The response is 202 with the
  generation and the workspace. `GET .../captures/:id/compare?against=<capture id>` returns a
  pixel-diff artifact the hub computes lazily.
- Artifact safety. Screenshots, DOM snapshots and console logs are
  artifacts with the issue's read audience and `workspaces.artifact_retention`
  (default 30 days). The DOM snapshot is sanitized by the hub when stored: `script`, `iframe`,
  `object`, `embed`, `link`, `meta http-equiv` and event handler attributes
  are removed, and every `src`, `href`, `srcset`, `poster` and CSS `url()`
  that is not a `data:` URL is rewritten to an empty value, so the stored
  document cannot make a request. It is served as `text/plain` for download
  and rendered in the panel in a sandboxed iframe under
  `Content-Security-Policy: default-src 'none'; img-src data:; style-src
  'unsafe-inline'`. Console output has
  the project's known secret values scrubbed on write, best effort, and the
  contract says plainly that an application which logs its own secrets
  will leak them to the issue's readers; the secret store's names and the
  `env_from_secrets` list are the operator's tool for keeping that small.
- Secrets. Per project, managed by owners and admins: `PUT
  {nativeBase}/secrets/:name {value}` (write-only; the hub stores a new
  version encrypted with its data key and never returns a value), `DELETE
  .../secrets/:name`, `GET .../secrets` → names, versions and updated_at
  only. Names match `[A-Z][A-Z0-9_]{0,63}`; names that look like provider
  or Detent credentials (`DETENT_*`, `OPENAI_*`, `ANTHROPIC_*`, `CODEX_*`,
  `CLAUDE_*`, `*_API_KEY` for a known provider) are rejected with
  `reserved_secret_name`, because section 1 forbids the hub holding them.
  The idempotency store records a hash of a secret request body, never the
  body. Audit rows carry names only. A runner fetches values with `GET
  {nativeBase}/workspaces/:id/worker/secrets` under its workspace lease, once
  per workspace, only the names in `env_from_secrets`, only when the
  workspace's `ref` is trusted (18.8), and never when
  `preview.secrets_policy` is `never`, which applies to both backends; they are injected into the preview
  process environment and nowhere else.
- Surface. The Browser card is enabled when stored captures exist for the
  attempt or sandbox (they are readable whatever runs today), or when the
  project declares `preview` and either a Detent sandbox is available on
  the plan or a capable runner is online (the workspace's own runner once
  one exists, else a fresh-heartbeat runner with an active grant). With
  captures but no way to make new ones, the panel shows them with the
  refresh control disabled and the reason. The panel
  shows the latest capture per route with its timestamp and sha, a route
  picker, T3's refresh control wired to the refresh endpoint, and a
  before/after toggle against the base branch's latest capture when one
  exists. Relay channel `preview` carries progress only: `refresh {routes}`
  → `capture_started {generation}`, `capture_done {capture_id, route}`,
  `capture_failed {route, status}`.
- A live, tunnelled preview URL is deferred (18.9). When it comes it is an
  upgrade on the same surface, not a replacement.

### 18.8 Review sandboxes

The same capture contract, run on Detent's own compute for customers who
want it without a runner, or for a pull request that has no attempt.

Trust first. Code from a pull request is untrusted until a person says
otherwise, and secrets never meet untrusted code:

- Trust is decided per `sha`. A sha is `trusted` when the connector
  reports its pusher and that pusher's GitHub login is linked to an active
  organization member (`members.github_login`, set from the member's own
  connected account, never typed by an admin), or when an owner or admin
  has approved that exact sha through `POST .../sandboxes/:id/approve
  {sha}`. A sha from a fork, or one whose pusher is unlinked or not a
  member, is `untrusted`. A new push is a new sha and is decided afresh by
  the same rule: a member's push is trusted on its own, an approval never
  carries over. A running sandbox keeps the sha it started with;
  `on_pull_request` starts a new sandbox for the new sha and closes the old
  one once the new one is `ready`.
- An untrusted sandbox runs with no secrets, `egress: deny`, and only the
  routes that need none of them; with `preview.secrets_policy: never` no
  sandbox ever receives secrets. Automatic creation on pull request
  (`preview.on_pull_request`) applies to trusted refs only; an untrusted
  pull request shows T3's PR state with "awaiting approval to preview" and
  an approve control for owners and admins.
- MicroVM isolation protects the host, not the secrets handed to the
  process. The setting page says so beside `env_from_secrets`.

Resource and API.

- `POST /api/v2/organizations/:org/sandboxes` `{project_id, pull_request?:
  number, ref?, backend: detent | runner}` (write on the project; `detent`
  additionally needs the plan's `sandboxes` entitlement) → 201 `{id: sb_<32
  hex>, project_id, backend, state, reason | null, ref, head_sha, trust:
  trusted | untrusted, approved_by | null, expires_at, captures_url,
  created_by}`. `GET .../sandboxes?project=`, `GET .../sandboxes/:id` (issue
  read rule of the linked work item, else project read), `DELETE` (write),
  `POST .../sandboxes/:id/approve {sha}` (owner or admin). Events
  `sandbox.<state>` on the project stream. States: `requested → starting →
  ready → capturing → idle → closing → closed`, `→ failed {reason}`; reasons
  `untrusted`, `clone_failed`, `command_failed`, `timeout`, `quota`,
  `closed_by_actor`, `pull_request_closed`, `expired`.
- `backend: runner` dispatches a `detent:workspace` item with
  `requires: [preview]` bound to the sandbox rather than to an attempt;
  nothing leaves the customer's network. That workspace's lease is the
  producer for the sandbox's captures: `POST .../sandboxes/:id/captures`
  accepts `producer.kind: workspace` when the workspace is the one the
  sandbox dispatched, and `producer.kind: sandbox` from the supervisor's
  token otherwise. `backend: detent` runs an isolated microVM
  (Firecracker-class; Fly Machines is the first provider) that clones
  `head_sha` with an ephemeral read-only deploy key the hub creates through
  the connector at start and removes at close, runs the preview command,
  and captures. The microVM itself holds nothing: a supervisor outside the VM, run by
  Detent, clones `head_sha` onto the VM's disk before boot, injects
  secrets (trusted only) into the VM's environment at boot, collects the
  capture output volume after, and is the only party that talks to the
  hub. It does so with a sandbox token (`api_tokens` scope `sandbox`, bound
  to the sandbox id and to a `generation` the hub increments on every start
  or replacement) whose only authorities are `POST .../sandboxes/:id/
  captures`, `POST .../sandboxes/:id/heartbeat` and `GET
  .../sandboxes/:id/secrets`. The hub revokes the token the moment the
  sandbox leaves `ready | capturing | idle` for any reason, on replacement,
  and on any trust change for its sha; a request with a stale generation is
  refused with `stale_execution`. Inside the VM the application has no
  Detent session, no provider credentials, no model and, for an untrusted
  sha, no network at all; the supervisor's clone and upload are how an
  `egress: deny` sandbox still works. Section 1 stands.
- Egress: `sandboxes.egress: allow | deny | allowlist` per project, default
  `allowlist` with the project's package registries when secrets are
  injected and `allow` otherwise; always `deny` for untrusted refs. Idle
  timeout 30 minutes, hard cap 2 hours, no persistent disk. The hub closes
  a sandbox when its pull request closes or merges.
- Captures from a sandbox are the same resource as an attempt's captures,
  keyed by sandbox: `POST .../sandboxes/:id/captures`, `GET
  .../sandboxes/:id/captures`, `POST .../sandboxes/:id/captures/refresh
  {routes?}` (write; delivers the `capture` control to the sandbox's
  workspace, or to the supervisor for `backend: detent`) and `GET
  .../sandboxes/:id/captures/:capture/compare?against=`, with the same
  generations, limits, network policy and artifact rules as 18.7.
- Teardown. Two minutes without a supervisor heartbeat fails the sandbox
  with `heartbeat_lost`: the hub revokes the token, removes the deploy key
  through the connector, and asks the provider to destroy the machine;
  a provider that cannot confirm destruction is retried and the sandbox is
  listed under `sandboxes?state=failed&cleanup=pending` until it does.
  `closed`, `failed` and replacement all run the same teardown.
- Metering. `sandbox_usage (sandbox_id, organization_id, project_id, day,
  backend, seconds, captures)` is written from `sandbox.ready` to
  `closed | failed`, with heartbeat loss ending the interval at the last
  heartbeat. It is shown on the Usage page's Runners tab as a "Detent
  sandboxes" row and priced by a plan line `sandbox_minutes`, with the
  allowance on the Limits tab. Concurrency is `plan.sandboxes.max_open`
  (default 2); beyond it creation answers 422 `sandbox_limit`, and beyond
  the allowance 402 `allowance_exhausted`.

### 18.9 Deferred

- A live tunnelled preview URL with an interactive iframe.
- Terminal split, vertical split and multiple terminals per stream.
- Annotating a capture and posting the annotation to the pull request or
  the conversation. This is the review loop that makes captures Detent's
  rather than a screenshot service, and it is the first thing to build
  after captures exist.
- Writes through the files surface.

  (The Agents surface was listed here and then shipped. It is now removed —
  section 18.10 — so it is neither deferred nor live: it is not carried.)

### 18.10 Capabilities and keybindings

- **No Agents surface (September 12, 2026).** Michael, reviewing the right
  panel: "I think for our purposes we don't need an agents tab." T3's `agents`
  surface is removed from the panel — the kind and its union member, the
  launcher card, the add-surface menu entry, the title and icon cases, the two
  unavailability strings and the `A` letter shortcut — and `AgentsPanel.tsx`
  goes with it. It is the one T3 surface this port drops rather than keeps
  present-and-disabled, which section 16 requires be his call and is. It is not
  a loss of information: section 19.3's issue page already carries the running
  attempt as a live activity row that opens the conversation, so the roster was
  a second reading of the same fact. The live count survives on T3's own panel
  toggle ("2 agents working"), which is a whole copy and is about the thread
  rather than about a tab. With `A` freed, the issue page's Assignee picker owns
  that letter outright; the letter-yield guard of section 19.1 stays for `P`,
  unchanged, because it was written against whatever letters a properties
  column claims rather than against a fixed pair.
- Runners report `capabilities: {terminal, files, diff, preview, exec,
  git}` and `isolation` in the heartbeat next to the provider reports
  (section 9.2). `exec` is section 18.12's: running one project action
  non-interactively. It is separate from `terminal` because a runner may
  serve one and not the other, and because the two are gated differently.
  `git` is section 18.13's. The other four are this section's own.
  The bootstrap payload (section 12) gains `projects[].capabilities` with
  the same keys, true only when a runner with a fresh heartbeat and an
  active grant for that project reports them; the single top-level
  `capabilities` object keeps its current meaning. Once a workspace exists
  the panel reads the workspace's own `capabilities`, because only that
  runner can serve it.
- The T3 keybinding commands bound in the client: `commandPalette.toggle`,
  `chat.new`, `sidebar.toggle`, `rightPanel.toggle`, `rightPanel.close`,
  `rightPanel.toggleMaximized`, `diff.toggle`, `terminal.toggle`,
  `terminal.new`, `terminal.close`, `filePicker.toggle`,
  `modelPicker.toggle`, `projectSearch.toggle`, `thread.next`,
  `thread.previous`, `thread.stop`, `thread.settle`,
  `thread.copyReference`, `preview.toggle`, `preview.refresh`. None of them
  names the Agents surface, so dropping it above cost no binding; the only
  keyboard change is the launcher's bare `A`, which is now the Assignee
  picker's alone. The rest
  (`chat.newLocal`, `composer.stash`, `editor.openFavorite`,
  `preview.zoom*`, `preview.focusUrl`, `terminal.split*`,
  `themeEditor.toggle`, `thread.pin`) appear in the Keybindings settings
  page disabled with the reason, per section 16.

  **The terminal's three commands are bound (September 12, 2026).**
  `terminal.toggle` takes T3's own `mod+j`, unchanged. `terminal.new` and
  `terminal.close` are upstream's `mod+t` and `mod+w`, and both are chords a
  browser tab keeps for itself, so they move onto the `alt` the panel family
  already uses (`alt+mod+t`, `shift+alt+mod+w`) and keep upstream's letters ---
  the fifth deviation of the same kind, and for the same reason, as
  `rightPanel.close` and `chat.new`. `terminal.split` and `terminal.splitVertical`
  stay disabled with a reason of their own: T3's surface union carries a
  `splitDirection` and Detent's Terminal surface draws one shell at a time
  behind a picker, so there is no split for a chord to toggle. The launcher's
  bare `T` is live on the same condition the card is.

### 18.11 Sequencing

1. ~~Pull requests and stored attempt diffs with producer fencing: hub and
   connector only, no relay. Enables the PR and Diff surfaces with real
   data.~~ **Shipped.** Migration 00026 stores `attempt_diffs` and
   `attempt_diff_files` keyed by generation and fenced by the producer's
   lease, with the 18.4 denylist and the 1 MB / 20 MB caps applied on write;
   the runner posts its worktree diff before every `run.checkpointed` and
   before `run.finished`; `GET {nativeBase}/work-items/:item/pull-requests`
   joins change requests with the GitHub projection the reconciler maintains
   (the hub holds no GitHub credential of its own, so `?refresh=1` queues a
   hydration request rather than fetching inline), and the three action
   endpoints queue one merge-lane work item each.
2. ~~Workspace sessions, their lease and worker endpoints, and the relay with
   tickets, streams and acknowledgements; files first, then terminal at
   `container` isolation.~~ **Shipped, except the terminal.** Migration 00027
   stores `workspace_sessions` with its own owner tuple, the `workspace_items`
   association that dispatches it as the `detent:workspace` kind, and
   `workspace_occupancy`; the state machine, its reasons and both limits live
   in `internal/workspacesession` with every legal transition and every refusal
   under test. The claim gate is stricter than the coordinator's — every
   capability in `requires` reported with a fresh heartbeat, an active project
   grant, and the one runner that holds a retained worktree — so runners now
   report `workspace_capabilities` and `isolation` beside their provider
   reports. The relay is a WebSocket on `github.com/coder/websocket` with
   single-use tickets, hub-allocated streams, hub-stamped actors, the 18.2
   caps, a 60-second resume window and `workspace_relay_sessions` audit rows.
   The runner claims workspace items in a lane of its own, keeps or checks out
   the worktree, heartbeats, and serves the files channel with containment
   enforced at open time (`os.Root`, `O_NOFOLLOW`, an lstat that refuses a
   symlink, and a device check for mount points) and the denylist delegated to
   the one `tracker.DiffPathDenied` the stored diff already uses.

   **Corrected September 12, 2026.** Two halves of "the runner claims workspace
   items in a lane of its own" were only true in tests. The `detent` runner
   process never constructed that lane and never reported
   `workspace_capabilities` on its heartbeat, so no real runner was eligible
   for a workspace; and the hub's ordinary claim, whose workspace gate was a
   capability filter over *open* items only, handed a closed workspace item's
   still-dispatchable issue to the issue lane as ordinary work — which the
   third dogfood run caught it doing. The runner now reports the report and
   starts the lane when it serves `files`, and a claim is offered workspace
   items only when it declares the `workspace_sessions` claim capability, in
   the candidate query rather than in a skip set. Closing a workspace still
   does not move its issue to a terminal state the way closing a coordinator
   item does; nothing claims it either way now, so that difference is left
   alone.

   Six things turned out to need a decision this section did not make.
   **The project event stream had no typed events**: section 12's stream
   carried one integer with no id and no body, which cannot satisfy "a client
   observes readiness by subscription and never by polling", so 00027 adds a
   durable `project_events` log on the conversation stream's design and the
   stream now carries typed frames beside the original `activity` one.
   **A claim names a work item and the worker endpoints name a workspace**, so
   `GET {nativeBase}/work-items/:item/workspace` joins them, fenced by the same
   tuple `bind` is. **A fresh workspace takes a worktree of its own** rather
   than the issue's, because the backend keys a worktree's path and branch on
   the issue identifier and a workspace reusing it would hold the branch an
   ordinary run of the same issue needs. **The per-frame authority re-check is
   the hub-local half only** — session, membership, grant, workspace and lease,
   all answerable from the hub's own database — with the provider-backed
   membership read left to the 30-second re-check and to `authority.changed`,
   because a network call per keystroke would itself become the failure mode.
   **An `ack` does not reset the idle clock.** 18.2 routes it like any
   person-originated frame and 18.1 resets idle on "person-originated frames
   (input, requests, resizes)"; read together, a stream that was only receiving
   output would keep its workspace alive by admitting it had received
   something, which is the "busy shell left alone" the timeout exists to close.
   Acks therefore reset nothing and are not re-authorized. **`relay_sessions`
   is owners and admins, not the creator.** The field's parenthetical is taken
   literally: 18.3 widens the equivalent audience for a terminal recording to
   "the person who ran the session" in so many words, and the absence of that
   clause here is the contract rather than an omission.

   Two smaller divergences, both deliberate. Allowance exhaustion answers
   **429**, not the 402 this section names, because `allowance_exhausted`
   already means 429 everywhere else in the hub and one code with two statuses
   is worse than one status the section did not expect; nothing meters
   workspaces yet, so the refusal that actually fires is `workspace_limit`.
   And `closing` is never observed: `DELETE` and every sweep walk through it to
   a terminal state in one transaction, because nothing yet waits for a runner
   to acknowledge a close. The state and its event exist; no workspace rests
   there.

   **The terminal shipped September 12, 2026.** A runner now reports `files`,
   `exec` (18.12), `git` (18.13) and `terminal` (18.3); `internal/workspaceterminal`
   opens a PTY on the project's configured shell in the worktree, the relay
   routes the channel with 18.3's authority asked on every frame, and migration
   00033 records each stream as an asciicast whose audience is 18.3's own rather
   than the issue's. Every terminal `open` allocates a stream and a PTY of its
   own (18.2), a dropped connection has the section's sixty seconds to resume
   before the shell is killed, and the client's Terminal tab is `@xterm/xterm`
   inside T3's `DiffPanelShell`.

   Four things needed a decision this section did not make.
   **Container isolation is refused rather than approximated.** This repository
   ships no container runtime hook of any kind, so a runner reports `user` and
   `workspaceterminal.New` returns `ErrContainerIsolation` for the other level.
   That is what 18.3 says such a runner must do, and the alternative --- handing
   back a plain PTY under the recommended name --- would be the one failure this
   surface must not have.
   **A recording is a table rather than a column on the relay session row.**
   18.3 says "each stream", and one connection may open several, so one column
   cannot name several recordings; `recording_artifact` names the listing
   filtered to that connection instead. The audience is what makes it a table:
   an action run's output has exactly the issue's read audience (18.12), and a
   terminal recording's is narrower --- the person who ran it plus owners and
   admins, owners alone at `user` isolation --- so it is stored with the
   isolation level it actually ran at and gated on read.
   **The recording is capped at 1 MiB and marked truncated.** 18.3 sets no
   bound, and a terminal must have none on the wire: a shell that stopped
   producing after a megabyte would be a terminal that stopped working, which is
   the whole difference from the exec channel's bounded log. So the cap lives on
   the stored copy alone.
   **Secret scrubbing is not done, with a reason.** 18.3 asks that "known secret
   values from the project's secret store (18.7) are scrubbed from recordings on
   write, best effort", and 18.7's secret store is deferred to step 3 below.
   There is no set of known values in this build to scrub against; the scrub
   goes in on write, where the contract puts it, when the store does.

   One divergence from the section's own words. **A token principal is not
   refused per frame.** 18.3 says a terminal requires a hosted session, and the
   creation gate enforces exactly that: a worker or operator token asking for
   `requires: ["terminal"]` is refused, so no token can open a workspace with a
   terminal on it. Per frame the token has no membership role and no project
   grant row, so none of the section's remaining checks can be asked of it, and
   its authority is the token the middleware already proved on the upgrade ---
   which is the same reasoning 18.13's `refuseGitWrite` applies to a commit, the
   other frame on this relay that changes a worktree.

   Not shipped: the live workspace diff channel (18.5) and `watch` on the files
   channel, which is answered `unsupported` as 18.4 allows. A workspace asking
   for `diff` or `preview` stays `requested` and fails with `no_runner` rather
   than being claimed by a runner that would refuse it frame by frame.

   Shipped out of this order: the header's git action group and its `git`
   channel (18.13). It arrived before the terminal because it is step 2's
   relay and step 1's pull request action rather than a new mechanism, and
   because the person side of it was two header controls that were already
   drawn and disabled.
3. Captures on the runner after a run and on request; the Browser surface
   over them; the secret store.
4. Detent-hosted sandboxes on the same capture contract, with trust,
   metering and the sandbox token.
5. Annotations, then the live tunnel.

### 18.12 Project actions and the exec channel (September 12, 2026)

T3's header carries an "Add action" control (`ProjectScriptsControl.tsx`) and a
dialog behind it: a name with a glyph, a keybinding, a command, an optional
preview URL, and two switches — run automatically on worktree creation, open
preview automatically when this action runs. Detent copied the control's
third branch, the bare button, and cut everything else, because a hosted
project has no checkout on the reader's machine and there was nothing for a
command to run on. Workspace sessions (18.1) removed that reason: there is now
a worktree, on a runner, that a person can reach. This section is the contract
for making the dialog real.

Michael's framing, which is section 16 applied: use the components in their
entirety for the visual identity, and move only what pulls back from the API to
Detent functions. So the dialog, the menu, the icon picker, the chord capture
and the switches are T3's, whole; what is Detent's is where the actions are
stored, where they run, and what a run leaves behind.

**What an action is.** A command the project wrote down. Stored per project,
not per issue and not per workspace: that is what makes it worth writing once,
because every conversation in the project offers the same actions and a
worktree opened tomorrow runs the same setup as one opened today.

Resource: `{id, name, command, keybinding, icon, preview_url,
run_on_worktree_creation, open_preview, created_by, revision, created_at,
updated_at}`.

- `GET {nativeBase}/actions` follows the project read rule and returns the
  whole set in authoring order. There is no cursor: the set is capped at 50 per
  project, because the header menu lists every one of them and a keybinding is
  registered per action, so an unbounded set is an unbounded menu and an
  unbounded chord table.
- `POST {nativeBase}/actions` (write on the project) → 201.
  `PATCH {nativeBase}/actions/:id` (write, `expected_revision`) → 200.
  `DELETE {nativeBase}/actions/:id` (write) → 204, and its runs go with it.
  Every mutation is idempotent by key and carries the hosted boundary's CSRF
  header, like every other mutation in section 12.
- Authoring order is the order the run-on-worktree-creation set runs in. An
  author who wants install before build writes install first, and nothing else
  in the contract would say so.
- A chord resolves to one command, so two actions in a project may not claim
  the same keybinding. Without that rule the client would have to pick a
  winner, and which one it picked would depend on listing order.
- The hub stores the chord and never resolves it. A keystroke is resolved in
  the browser against the platform it is running on, and the hub has no way to
  know whether `mod` means Command or Control for the person pressing it.
- The command is not validated beyond its bounds. It runs through a shell, so
  any shell syntax is legal by construction, and a hub that tried to decide
  which commands are safe would be pretending to a boundary it does not have —
  the same admission 18.3 makes about a terminal. What bounds the damage is who
  may write an action (`write` on the project) and where it runs (the worktree,
  under the workspace's isolation level), not a pattern match on the string.

**Why `exec` is its own channel and not a terminal with a command typed into
it.** A terminal (18.3) is interactive, it is a PTY, it is recorded as an
asciicast whose audience is narrower than the issue's, its lifetime is a
person's attention span, and it is gated behind the grant's `runners` flag and
an owner-only isolation choice because it hands the runner account's shell to a
person. An action is the opposite of all five: the command is the project's,
written down in advance and reviewable; it produces one bounded output with the
issue's own audience; it ends on its own; and it may run with nobody watching
at all. A channel of its own is what lets the gate, the recording and the
lifetime differ without either surface having to ask which of the two it is. A
runner may serve one and not the other, so `exec` is a capability of its own
beside `terminal`, `files`, `diff` and `preview` (18.10), and `requires:
["files", "exec"]` is what a client asks for when running an action is the
reason it wants a worktree.

A consequence worth writing down, because it was latent until now: reusing an
open workspace is only correct when that workspace's reported capabilities
satisfy what the new caller asked for. While every workspace was a files
workspace the check was invisible; with two capabilities a client that reuses
a files-only workspace for an action gets frames refused by the channel gate
and a reader who sees an action that never finishes. The client applies the
same `Satisfies` rule the claim gate does, on both the reuse path and the 409
`workspace_exists` adoption path.

**The channel.** Non-interactive, one run per stream.

- Frames. Person → runner: `run {run_id, action_id, command}`. Runner →
  person: `output {data, encoding?, truncated?}` repeatedly, then `exited
  {code, signal?}`, then `closed`. `run_id` is in the frame because the run
  record exists before the first frame does, and it is what lets the hub record
  status from frames it is only relaying; `command` is carried so the runner
  runs the action it was handed rather than one a client composed.
- Every `run` allocates its own stream, as every terminal `open` allocates its
  own PTY (18.2). Two concurrent runs sharing one stream would share one
  sequence space and one cancellation.
- The runner follows `exited` with `closed`, because the hub releases a stream
  slot on `close` or `closed` and a runner that stopped at `exited` would leak
  the slot for the life of the connection.
- Gating, in order: the workspace is readable; `write` on the project; the
  workspace reports `capabilities.exec`; and the workspace is not read-only. A
  workspace on a running attempt is read-only (18.1), and a command must not
  write into a worktree a model is editing. The terminal's extra authority —
  the `runners` grant, `workspaces.terminal.enabled`, the isolation rule — does
  not apply, for the reasons above.
- Runner side. The command runs under the project's configured shell with the
  worktree as its working directory, at the workspace's isolation level (18.3).
  The runner validates its lease immediately before starting the process, not
  once per session, and keeps checking while it runs. It kills the process
  group — not just the shell, or a command that spawned children would leave
  them behind — on stream close, on lease loss, when the socket dies and when
  the workspace ends. Provider and Detent credentials are removed from the
  child environment as a courtesy, not a boundary, in the same words 18.3 uses.
- Output is capped at 1 MiB. Past the cap the runner stops forwarding, emits
  `[output truncated at 1 MiB]` once as its own span, and keeps draining the
  pipe so the process is not blocked writing into a full one and its exit code
  still means what it says. A marker rather than a silent stop, because a
  reader who cannot tell a finished log from a cut one will read the cut one as
  finished.

**Runs are recorded.** A run is a record, not just a stream, so a run nobody
watched is still visible and a run whose watcher closed the tab still has an
outcome.

- `POST {nativeBase}/actions/:id/runs {idempotency_key, workspace_id}` → 202
  `{run_id}`. The row is written `queued` before any frame reaches a runner, so
  a run that never starts is visible as queued rather than absent.

- `GET {nativeBase}/actions/:id/runs/:run_id` → `{status: queued | running |
  succeeded | failed, exit_code, started_at, finished_at, output_artifact,
  reason, truncated, ...}`, and `GET {nativeBase}/actions/:id/runs` lists an
  action's runs newest first. The list exists because a run-on-worktree-creation
  run has no client that saw a 202, so listing is the only way to find it.
  `exit_code` is null until the process exits, so
  "exited 0" and "never exited" are different facts rather than the same zero;
  a run that failed without exiting says why in `reason` (`lease_lost`,
  `stream_closed`, `killed`, `workspace_closed`).
- `succeeded` is exit 0 and `failed` is everything else, including a run whose
  outcome cannot be established. That is section 5's rule for a control whose
  outcome is unknown, applied to a command: a run left `running` forever is the
  bug the rule exists to prevent, so every teardown path writes a terminal
  status.
- "Every teardown path" is two different sets, and saying so is the difference
  between the rule holding and only appearing to. A run the relay started has a
  stream, so the stream ending is what ends it: the person's socket closing,
  the runner's closing or being superseded, the stream closing or overflowing,
  a detached stream being swept, the workspace going terminal, or the hub
  shutting down each take the run and write a terminal row with whatever output
  it had. A run the runner started itself has no stream and no relay state, so
  none of that reaches it, and a crashed runner or a hub restart would leave
  the row `running` with nothing to end it. The workspace sweep closes that:
  every `queued` or `running` run whose workspace has reached a terminal state
  is failed, carrying that workspace's own end reason. It keys on the
  workspace's state and never on the run's age, which is what makes it exact
  rather than a reaper — a terminal workspace means no runner holds that
  worktree, so no legitimate report can still arrive, while a slow test on a
  healthy workspace is indistinguishable from a stuck row by age alone.
- The sweep and the relay's teardown read the reason from the same place, and
  that is load-bearing rather than tidy. A workspace that ends while a run is
  still on it can be reached by either path, and whichever commits first wins;
  if the sweep stamped a blanket reason the two would disagree about why one
  run died, and which explanation an operator saw would depend on scheduling.
  A wrong but confident explanation is harder to debug than a missing one.
- The guarantee that follows, in the terms an operator needs: a
  runner-originated run is recorded by the runner's reports while its workspace
  is alive, and if those reports stop for any reason — the runner crashed, the
  hub restarted, a report was refused and not retried — the run is failed
  within one maintenance tick of its workspace reaching `closed` or `failed`,
  carrying that workspace's own reason: `lease_lost` where the hub gave up the
  worktree's lease, and `workspace_closed` otherwise. No run stays `queued` or
  `running` once its workspace has ended, and a run on a workspace that is
  still open is never touched however long it has been going.
- The hub records status from the frames it relays. It is forwarding them
  already; it now also reads them, marking `running` on the `run` frame it
  relays and terminal on the `exited` frame. Output accumulates per stream in
  memory and is written once on completion, not a database write per frame.
- `output_artifact` names `GET {nativeBase}/actions/:id/runs/:run_id/output`,
  which serves the bytes as `text/plain`. The hub stores this itself rather
  than through the artifact service, on the precedent of `attempt_diffs`
  (migration 00026): a bounded, already-audience-scoped text blob the hub
  keeps, where the 1 MiB cap is what makes keeping it safe. Its audience is the
  issue's, unlike a terminal recording's.
- Every transition emits `action_run.<status>` on the project event stream with
  the run as `data`, the way `workspace.<state>` rides it (18.1). That is how a
  client observes a run it did not start — a run-on-worktree-creation run, or
  one another tab began — by the same subscription and never by polling.

**Who executes a queued run** (September 12, 2026, after the eighth dogfood
run). A queued row was only ever turned into a process by a person opening the
exec channel for it, and nothing on the runner polls: its lane runs a project's
actions when a worktree is created and never again. So the two REST calls above
were not an action run at all — the eighth run made them, got its 202, and
watched the row sit `queued` for three minutes and forty seconds. That is a trap
for the API and for every headless caller, and the contract above is what
promised them otherwise.

A queued run therefore has two possible executors, and the hub hands it to
exactly one.

- **The person who opens the exec channel for it**, which is the one the
  contract prefers: only their own stream shows them the output as it is
  produced. A client that queues a run and opens the channel for it in the same
  turn — which is what the browser does — is always that executor.
- **The hub itself**, for the run nobody came back for. One maintenance tick
  after a short grace period, the hub opens a stream of its own on the
  workspace's runner connection and sends the same `run` frame a person would
  have sent. Nothing on the runner is new: the frame is the frame whoever opened
  the stream, so the run gets the same lease validation immediately before the
  process starts, the same 1 MiB cap, the same kill-on-lease-loss and the same
  process-group teardown, and the hub records it from the frames it relays
  exactly as it records a person's. The grace period is what separates the two
  cases, because the honest discriminator is time: a browser's round trip is
  far inside it and a headless caller has no round trip to make.
- The hub's stream is owned by the hub rather than by a connection, so no person
  can address, resume or inherit it, and runner frames on it are recorded rather
  than buffered for a replay nobody can ask for — a stream with no reader behind
  it must not be able to overflow. It counts against the workspace's stream
  budget like any other, because the runner serves them all from one socket.
- The race is settled on the row and never by ordering. `claimed_by` (migration
  00032) is written in the same transaction that moves the run out of `queued`,
  under the same revision check every other transition uses, so the first claim
  wins and the second is refused with `already_running` — a code of its own
  rather than `forbidden`, because the run *is* going to run and the answer is
  to read it back through the row rather than to ask again. One row is never
  two processes in one worktree, whichever order the two claimants arrived in.
- A run the hub dispatched is not delivered to anybody live, which is why the
  recorded half above is not optional: the Output surface reads an action's runs
  and their output back on load, so a run nobody watched is visible with its
  output rather than as a status with an empty box.

**Run automatically on worktree creation.** When a workspace session reaches
`ready` for a *fresh* worktree, the runner runs every action carrying the flag,
in authoring order, one at a time, and reports each as a run through `POST
{nativeBase}/workspaces/:id/worker/action-runs` under the workspace lease. It
is sequential because the order is the contract; running them concurrently
would make writing install before build meaningless. A retained worktree runs
nothing: the setup already ran when the worktree was created, and running it
again would re-do work on a tree someone may be reading. A setup command that
fails does not stop the workspace reaching `ready` — a failed setup is
information, not a reason to deny the reader their worktree.

The worker endpoint exists because this run has no person connection to relay
through, and inventing a runner-originated relay stream so the hub could read
its own frames back would be a second mechanism for the same fact. It is
fenced by the workspace tuple exactly as `bind` and `heartbeat` are.

**The preview URL stays and does nothing yet.** The Browser surface is
snapshot-first and the live preview is deferred (18.7, 18.9), so there is
nothing for an action to open. Section 16 says a T3-only feature stays present
— live, or disabled with a tooltip — until Michael decides to pull it out, so
the field is stored and echoed and the "Open preview automatically when this
action runs" switch is rendered disabled with the reason "Browser preview is
not available on Detent Cloud yet." An action authored today does not have to
be re-authored when the surface lands.

**Where the output goes.** There is no terminal view in the client, so a run's
output is a right-panel Output surface built from the markdown renderer's own
code-block chrome (`MarkdownCodeBlock`, with its wrap and copy controls). That
is section 16's rule rather than a shortcut: reusing the chrome the client
already draws code in keeps one visual answer to "here is some monospaced
output" instead of two.

The surface reads recorded runs back, and not only the ones the tab is
streaming. While it is open it lists the project's actions, lists each one's
runs, and reads the output of every terminal run that has bytes to read; a run
it hears about on the project event stream reads its own output back the moment
it finishes. Without that half the surface only ever held runs *this tab*
started over the exec channel, so a run the hub dispatched — the headless
caller's run, and every run-on-worktree-creation run — was invisible here
however completely the hub had recorded it. `output_bytes` is what decides
whether the read is made rather than the status alone, because a command that
printed nothing and exited zero has nothing to fetch.

**Detent's "Create linked issue" moves.** It had been occupying T3's
Add-action slot — the header button was theirs, class for class, with Detent's
label on it — because there were no actions to put there. Now there are, so it
moves into the control's menu beside them, in its own group, and both exist.

### 18.13 The git action group (September 12, 2026)

T3's header carries two controls this port had left present and disabled: the
`Commit, push & PR ▾` split button (`GitActionsControl.tsx`, 1,993 lines over a
417-line logic module) and the `Open ▾` picker (`chat/OpenInPicker.tsx`).
Michael sent both as the target: "a split button whose menu lists Commit, Push,
Create PR, and Open listing Cursor (⌘O), VS Code, Finder with their marks."
Section 16 says a T3 feature stays present until he decides to pull it out; this
is the other half of that bargain, which is that a control kept present
eventually has to work.

Both act on the workspace session's worktree (18.1), so neither exists on a
conversation with no linked issue: the group is disabled with **"Link an
issue to get a worktree."** and that is the same sentence the Open picker
gives, because it is the same fact.

- **The channel.** A fifth relay channel, `git`, on 18.2's frame rules —
  JSON `{channel, stream, type, seq, payload}`, request/response like
  `files`, and 18.2's stream reuse, so a second stream-less request
  continues the conversation rather than spending another stream.
  - `status` → `status {branch, detached, remote, upstream, ahead, behind,
    dirty_file_count, head_sha}`. It carries no file list on purpose:
    naming the files would make a header control a second Files surface,
    and 18.5 already has one.
  - `commit {message}` → `committed {commit, branch, files, excluded}`. It
    stages everything dirty except the 18.4 denylist paths, and
    `excluded` names what it left out — a person who expected a file in the
    commit has to be able to see why it is not there, and a silent drop is
    exactly the failure a denylist must not become.
  - `push` → `pushed {branch, remote, commit}`. The current branch to the
    project's remote, resolved as `branch.<name>.remote`, else `origin`,
    else the single remote there is; **never** with any force flag, and
    never `--force-with-lease`, because the header cannot know what it
    would be overwriting.
  - Failure is `error {code: "git_failed", message, stderr}`. The stderr is
    git's own, verbatim: a person acting on a rejected push needs what git
    said, not a paraphrase of it.
- **Authority.** `status` needs project read, which the connection already
  proved. `commit` and `push` need `write` on the project, and the hub
  re-validates it per frame the way 18.2 already re-validates everything
  else — a socket that lives for an hour would otherwise outlive the grant
  that opened it. The hub also refuses a write on a read-only workspace
  before the runner sees the frame, which is the only refusal a reader gets
  when no runner is attached.
- **The runner.** It validates its lease immediately before acting, as
  18.2 requires, and answers `stale_execution` to anything that arrives
  after lease loss. It refuses a write on a read-only workspace (the
  subject attempt is still running) and on a retained attempt worktree
  while that attempt runs. The two conditions are written out separately
  even though today one implies the other: a future checkout that separates
  them must not silently start allowing a write into a tree the model is
  editing.
- **Who commits.** The author is the acting person and the committer is the
  runner, which is git's own distinction and the honest one — the person
  decided, the machine ran it. The hub stamps the author's name and email
  onto 18.2's `actor`, alongside the principal and connection it already
  stamps, for the same reason the rest of the tuple is hub-supplied: a
  client that could name its own author could attribute a commit to
  anybody, and a commit's author line is permanent in a way an audit row is
  not. There is no display name anywhere in the hosted schema, so the name
  is derived from the email's local part rather than fabricated. A frame
  with no author email — an operator token, or a support session standing
  in for someone — is refused: a commit that names nobody who could answer
  for it is worse than no commit.
- **Capability.** `git` joins `{terminal, files, diff, preview}` in
  `requires`, in the runner's heartbeat report and on the workspace
  resource. The claim gate matches it like any other, so a runner without a
  git binary or with a worktree that is not a repository is never handed the
  work; a runner whose worktree turns out not to be one reports the
  workspace without the capability rather than failing it, and the header
  then disables the group with that reason. The header asks for
  `requires: ["files", "git"]`, and when no workspace is open for the issue
  the action requests one and runs once readiness arrives on the project
  event stream — never by polling (18.1).
- **Create PR** is not on the channel. It is 18.6's existing action,
  `POST {nativeBase}/work-items/:id/pull-requests/actions {idempotency_key,
  action: "open", expected_head_sha}`, which queues a merge-lane item; the
  runner with a checkout does the work, never the hub against GitHub. The
  `expected_head_sha` is the sha the last `status` read *after* the push, so
  it is what the remote actually has. A project with no GitHub connector
  disables the item with **"No GitHub connector on this project."**
- **The primary** runs Commit → Push → Create PR in sequence and stops at
  the first failure, with the reason in T3's own toast. A step with nothing
  to do is skipped rather than failed — no dirty files is no commit, a
  branch already in step with its upstream is no push — and status is
  re-read between the commit and the push so the push decision is made on
  what the commit produced. Which steps the primary runs is T3's own
  `resolveQuickAction`, and the per-item enablement and its sentences are
  T3's `buildMenuItems` and `getMenuActionDisabledReason`; 18.6's rule that
  the group "enables per action when the policy allows it and stays
  disabled with the reason otherwise" is a policy layer *in front* of that
  table, never a replacement for it. Detent's reasons are the structural
  ones — no issue, no write grant, a read-only or failed workspace, a
  runner that does not serve git — and they win, because a reader must be
  told the structural reason before the state one.
- **Open ▾ opens on the runner's machine or nowhere.** The worktree is on
  the customer's runner, and `cursor://file/<path>` only reaches a worktree
  the operating system can see. So the workspace resource gains
  `machine_hostname` and `worktree_path`, both from the runner's heartbeat
  rather than from the bind, so a runner that re-prepared a fresh worktree
  corrects the resource instead of leaving a path that no longer exists.
  Where the reported host is this machine, Cursor and VS Code hand the
  operating system their scheme; where it is not, every item is disabled
  with **"This worktree is on <hostname>."** Finder is present and always
  disabled with **"Available on the runner's machine only."** — no scheme
  is registered for revealing a path from a browser, and a `detent://`
  handler is a desktop app Detent does not ship. The comparison is
  deliberately conservative: a page served over loopback is never taken as
  proof of being on the runner's machine, because handing the operating
  system a path that is not there is the one failure worth being careful
  about.

## 19. The issue is the page; the conversation is a surface (September 11, 2026)

After the first real runner run Michael reviewed the linked-issue view and
said the chat-shaped page was too much: "things need to be more linear
focused, issue just showing the current state, the work that happened, etc."
and "you can always jump in to seeing and directing what is happening in the
runner but that needs to just be one item, like an activity that shows live
(a glowing dot) that when you click you can view the whole chat." He chose
the right panel for that chat from the mockup (`Detent Issue Page` canvas,
September 11): "let's go with the sidebar for the chat when the runner is
running so we can steer it."

Decided:

1. **One issue page**, at `/work/i/:workItem`, in the Linear shape and on
   T3's components: title and identifier; the description; "Add
   sub-issues"; a Resources list (the attempt diff, the pull request, the
   conversation); an Activity feed; a comment composer at the bottom. A
   properties sidebar on the right: status, priority, assignee (the runner
   while one holds the issue), labels, project, effort and model, attempts,
   pull request, related issues and the originating chat.
2. **The Activity feed merges** the work item's history (created, moved,
   labels, relations), attempts (claimed, started, checkpointed, finished,
   interrupted, failed), the linked conversation's events (question asked
   and answered, steering delivered, diff posted) and comments. Comments are
   cards with an inline reply; everything else is one line. Runs of low
   value events fold into "Show N events…" as Linear does.
3. **The conversation is one row in that feed while it is live**: a pulsing
   dot, the runner's current sentence, elapsed time, Interrupt, and "View
   conversation". When no attempt is running the row is the last turn's
   summary with the same link. Clicking the row opens the whole conversation
   in T3's right panel as a `conversation` surface, with T3's composer,
   Interrupt and Continue; the issue stays on the left and the properties
   sidebar yields its width to the panel. The panel toggle and
   `rightPanel.toggle` open the same surface.
4. **Routes.** `/chat/issues/:workItem` redirects to the issue page.
   `/chat/c/:conversation` for a linked conversation redirects to its issue
   page with the panel open; unlinked chats keep the chat page. The Work
   board's issue open goes to the issue page.
5. **The bottom composer posts comments, and only comments.** A comment is a
   work item comment, not a conversation message: readable by everyone who can
   read the project, and it survives the conversation. The runner's own
   composer is the conversation surface in the right panel, which the Activity
   feed's live row opens with "View conversation" (point 3).

   It briefly carried a second "Send to runner" mode as well. Michael removed
   it on September 12: "we can remove this since we will have the activity of
   the current runner; when the user clicks into the activity it will open the
   sidebar with the runner and the separate chat window." Two composers on one
   page offering the same send, on a page that already shows where that send
   goes, asked the reader to choose between things that were not alternatives.

   So this card is Linear's comment box: a prompt and a send. Gone with the
   mode: the three turn pickers (a comment has no model, no reasoning effort
   and no runtime access to choose), the scope sentence beside the toggle, the
   `/comment` and `/runner` slash commands, and the paperclip — the hub's
   comment endpoint takes a body and nothing else, so a clip there, even a
   disabled one, would promise a feature that is not coming. `/clear` and
   `/shortcuts` are what the menu has left.
6. Not in this slice: annotations on captures, sub-issue creation, editing
   the description in place.

## 20. Composer review (September 11, 2026)

Michael reviewed the composer against Codex's desktop app and T3 Code and
asked for four things.

1. **Proper picker options.** "When selecting a model we probably need to
   make sure we know the thinking levels."

   The hub knew a model's identifier and nothing else, so the effort picker
   offered one fixed vocabulary for every model. The provider capacity report
   now carries per-model detail — `model_details: [{id, label, provider,
   default, reasoning_efforts, default_reasoning_effort, legacy}]`, optional,
   describing only models `models` already advertises — and the runner fills
   it from the agent backends it dispatches on, which is the same catalogue
   automatic model selection reads (`model/list` on Codex's app-server). The
   read is cached and runs off the claim path: a heartbeat never waits for a
   provider process, and a runner that has not completed one publishes exactly
   what it published before.

   The bootstrap republishes it on `preferences.models[]`, each choice gaining
   `efforts`, `default_effort`, `provider` and `legacy`; "auto" stays first.
   `ValidatePreferences` accepts an effort a model itself publishes, because a
   hub that offered a level and then refused it would be lying to the picker.

   The composer's model chip becomes T3's command list inside their popover:
   a search field, a provider rail (favourites first, then one button per
   provider present in the data), rows carrying the label and provider with a
   ⌘1…⌘9 gutter that selects, a favourite star persisted per viewer in
   `localStorage`, and a collapsed "Legacy models" shelf for models whose
   provider has named a successor. Effort and access stay T3's `Select`:
   effort under a "Reasoning" group scoped to the selected model, with a
   Default badge on that model's own default and "Auto" still first (§14);
   access with T3's words and the lock — "Full access", "Read only" — and a
   line each.

   Selecting a model whose levels do not include the current one resets the
   effort to that model's default, and the picker says so on one line.

   **Service Tier is out, not disabled.** T3 prices a request against a hosted
   account; every Detent turn runs on the customer's own runner with that
   runner's own login (§1), so this is not a T3 feature Detent has no source
   for yet (§16) — it is one Detent will never have.

2. **The hint line is gone.** "Dont need 'Enter to send · Shift + Enter for a
   new line'." What it carried besides the two keystrokes — which act the send
   is (U07) — moves into the composer footer's leading slot, where the issue
   page already said it. `describedBy` therefore defaults to none.

3. **The context strip carries the pull request.** T3's strip is the checkout
   on the left and the pull request and branch on the right. Detent's left
   control already named the conversation's scope; the right one now carries
   the linked issue's pull request as a `#<number>` chip linking to the host's
   page for it, and the branch beside it, read from
   `GET {nativeBase}/work-items/:item/pull-requests` (§18.6). With a pull
   request but no branch, or a branch but no pull request, it shows what it
   has; with neither it falls back to the issue's lane. **A draft says nothing
   on the right**: "No linked issue" is true of every new chat and tells the
   reader nothing. A chat that has been sent and is still unlinked keeps
   saying it, because there the absence is a fact about that chat.

4. **The execution strip is one line.** "To tall fix the css, maybe omit 'The
   last attempt finished.'" The sentence sat beside the attempt id and wrapped.
   Every row truncates now, and a sentence is printed only where the label does
   not already say it: queued, waiting for you, unknown, and a failure, which
   prints the reason the hub gave rather than "The last attempt failed." The
   sentences themselves are unchanged — A.10's explanatory sentence is still
   what `executionCopy` returns, and the issue page still prints it in full.

### 20.1 Two corrections from the preview (September 11, 2026)

1. **Order is the runner's, not the alphabet's.** The hub sorted the model
   list by identifier, which put `gpt-5.6-sol` above `gpt-6-astra` — the
   backend's own default. It no longer sorts: `conversationModelChoices`
   returns the runners' order, deduplicated by first sighting, with the
   configured coordinator model last when no runner reported it. A provider
   lists its catalogue in the order it wants read.

   The one move the client makes is to lead with the model the backend
   defaults to, which the report already knows (`model_details[].default`) and
   the bootstrap now publishes as `backend_default` on the choice. That is a
   different fact from the choice's `default`, which says what "auto" resolves
   to for this hub: one is the provider's answer and the other is the
   operator's. The ⌘1…⌘9 numbers follow the order, so ⌘1 is the default model.

2. **The provider rail wears T3's marks.** It drew two letters of the
   lowercased provider slug — "OP" for `openai` — which reads as a placeholder
   nobody finished. T3 have the marks (`components/Icons.tsx`) and the table
   that picks one (`components/chat/providerIconUtils.ts`), keyed by their
   `ProviderDriverKind`; a capacity report names the vendor instead, so
   `app/adapters/providerMark.tsx` translates the one into the other and
   nothing else. A provider they have no mark for gets a neutral glyph and its
   name on T3's tooltip — never initials, which is what their
   `ProviderInstanceIcon` falls back to and what this was trying to be.

## 21. Client review (September 12, 2026)

Michael's fourth pass over the React client. Four items, and what each one
settled.

1. **The execution strip is the height of the context strip.** "Look to make
   this shorter more like the height of the bottom." The band above the
   composer card was half again as tall as the band under it, which frames the
   same card: the strip's row was `min-h-7` where T3's
   `ComposerSurface.ContextStrip` row is `min-h-7 sm:min-h-6`, and the banner
   `Body` it sits in padded it again on both sides.

   The strip now carries T3's context-strip metrics, class for class —
   `min-h-7 sm:min-h-6`, `ps-1 pe-2`, `gap-1`, and
   `font-normal text-muted-foreground/70 text-xs` — and the `Body` adds no
   padding of its own, so the only vertical space left is T3's own banner
   padding. Still one line, still truncating (§20.4).

2. **The slash list is part of the composer.** "The `/` commands need to be
   under the chat like the example." It was T3's floating popover, anchored to
   the card with a gap above it and a border and shadow of its own, which
   reads as a window over the transcript. Codex's desktop app renders the list
   inside the composer's own container, directly above the editor, at the same
   width, flush, with plain rows and no surface of its own, so the card simply
   grows taller with the list at its top.

   That is what it does now: mounted inside `ComposerSurface.Main` above the
   prompt editor, with the row markup, the keyboard behaviour and the `listbox`
   / `option` / `aria-activedescendant` wiring unchanged. The runner skills and
   prompts notice stays (§16) as a quiet last line rather than a boxed footer:
   the list has no border of its own any more, so a rule inside it would be
   the only one on the card.

3. **The issue composer posts comments only.** Recorded in §19.5, which this
   review rewrote.

4. **One connection chip, and both chips say what they mean.** The Work board's
   toolbar read "1 project · ● Not streaming · data as of 9:02 AM · Reload"
   while the sidebar footer read "● Live", and Michael asked what Reload does
   and what Live represents.

   Three faults, all in `app/work/lib/useWork.ts`:

   - the all-projects board — the default `/work` route — opened **no stream at
     all**. The effect began `if (projectId === null) return`, so the chip
     stayed on its initial `false` however healthy the client was. It now
     opens one stream per project in scope, and is live only when every one of
     them is connected: on an all-projects board a single project whose stream
     is down is activity the reader is not being told about;
   - the board's own load published `live: false` in its final `setState`, so
     every reload dropped the chip — including the reload the stream itself
     had just asked for. `live` and `sequence` belong to the stream and are
     carried across a load untouched;
   - a stream the browser had **closed** rather than merely lost was never
     reopened, so one refused request left the board dark until someone
     pressed Reload. A closed source is now reconnected on a delay.

   The chip is then one decision in one place (`app/work/lib/freshness.ts`),
   fed by the app's own connection state — the same value the sidebar footer
   shows. While the client is not live the board borrows the sidebar's word
   (Reconnecting, Offline, Connecting), because the board cannot be fresher
   than the client it is inside; with the client live the only question left is
   this board's own streams, and "Not streaming" with a Reload is shown exactly
   when they are down and never otherwise.

   Both chips carry one sentence saying what the label means and what the
   action does, on T3's `Tooltip`, from one module
   (`app/lib/connectionTooltips.ts`) so one client is never explained two
   ways: "Live: updates arrive over the project event stream." and
   "Not streaming: showing data from 9:02 AM, Reload fetches the board again."
