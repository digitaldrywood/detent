# Conversation product operations

How to configure, migrate, run, restart and roll back the hosted hub
conversation product. The behavior contracts it operates are in
[`decisions.md`](decisions.md); this document covers only what an operator has
to do and what they will see when something goes wrong.

## 1. What ships

The conversation product is part of the hosted hub (`internal/hubserver`), not
the single-tenant dashboard. The first milestone covers one path:

- A new chat is created inside a project and is private to its creator.
- An ordinary chat is answered by a coordinator turn. The decided design
  (`decisions.md` sections 1 and 9) runs that turn on a customer runner with
  the customer's own provider login: the message opens a coordinator work item
  that only a runner with a live-control backend may claim. A hub that
  configures `conversation.codex` still answers on the hub instead; that path
  is transitional, see the warning in section 2.
- A chat can be linked to a native issue. Linking shares the whole history
  with everyone who can read the project and requires `share_history: true`.
- A linked conversation is bound to the issue's runner when an attempt starts,
  and the operator can steer, interrupt, answer questions and record continue
  intent against that attempt.
- Durable history, receipts and a resumable event stream back all of it, and a
  React client is served at `/chat`.

Since `decisions.md` sections 11 and 12 that React client is the whole hosted
frontend, not only the chat panel. See section 1.1.

Explicitly not in this milestone:

- Attachments beyond text.
- Work board and review dock integration.
- Multiple runners or branches per conversation.
- Cross-host provider thread transfer.
- A production importer for POC data.

The client is dark only. It declares `color-scheme: dark` on its mount root, so
an operator using Detent in light mode gets a deliberately dark panel.

### 1.1 One frontend, and what it replaced

The hub serves the React application shell for every GET that is not an API,
auth, webhook or static path: `/`, `/login`, `/work*`, `/chat*`,
`/organization*`, `/projects*`, `/settings*`, `/fleet*` and `/support`. An
unauthenticated request to any of them except `/login` answers `303` to
`/login`, which renders the shell so the login card can start
`/auth/oidc/start`. A hub whose client bundle is missing answers `503
client_unavailable` instead of an empty page. A successful sign-in lands on
`/work`.

Removed with the Templ pages, so an operator who bookmarked them will notice:

- The rendered pages `/organization`, `/organization/plan`,
  `/organization/billing`, `/projects/:project`, the issue and change pages,
  and `/support`. Every one of those paths now serves the application shell,
  and the client fetches the same data as JSON.
- The form endpoints `POST /projects`, `/organization/create`,
  `/organization/join`, `/organization/switch`, `/organization/invite`,
  `/organization/members/:member/revoke`, `/organization/members/:member/role`,
  `/organization/grants`, `/organization/billing/checkout` and
  `/organization/billing/portal`. Their JSON replacements are below.
- `static/js/hosted-setup.js` and `static/js/hosted-work.js`, and the
  `internal/web/templates/hosted*` files. `static/js/artifacts.js` stays: the
  single-tenant dashboard still uses it.

`POST /logout` answers `204` to a JSON caller (one sending `X-CSRF-Token` or
`Accept: application/json`) and keeps the `303` to `/login` for a form caller.
`/auth/oidc/start`, `/auth/oidc/callback`, `/invite` and `/webhooks/stripe` are
unchanged. Every former HTML error is now the native `{code, message}` shape.

### 1.2 The hosted JSON API

`GET /app/bootstrap` carries what the shell needs to render anything:
organizations, the actor with `can_manage` and `can_manage_runners`, the
readable projects with their workflow states, an open support session, the CSRF
token, capabilities, the turn preference choices, the plan summary and the API
base. `/chat/bootstrap` is kept as an alias answering the same payload.

`preferences` publishes what the composer's model, effort and access pickers
offer:

```json
"preferences": {
  "models":  [{"id": "auto", "label": "Auto", "default": true},
              {"id": "gpt-6-astra", "label": "gpt-6-astra", "default": false}],
  "efforts": [{"id": "auto", "label": "Auto", "default": true},
              {"id": "low", "label": "Low", "default": false},
              {"id": "medium", "label": "Medium", "default": false},
              {"id": "high", "label": "High", "default": false}],
  "access":  [{"id": "auto", "label": "Auto", "default": true},
              {"id": "read_only", "label": "Read only", "default": false},
              {"id": "full", "label": "Full", "default": false}]
}
```

Every list leads with `auto`. The model list is the union of the models the
organization's active runners report in `provider_reports_json` for the
projects the actor can read, plus the hub's own configured coordinator model
when it has one; a hub whose runners have reported nothing, and that configures
no model of its own, answers with `auto` alone. `default` marks the value `auto` resolves to for the project — the
configured coordinator model or reasoning effort — and falls back to `auto`
itself when the hub does not know one. The composer still preselects `auto`,
because that is what a new conversation's preference says.

Under `/api/v2/organizations/:organization`, authenticated by the hosted
session cookie, with `X-CSRF-Token` on every non-GET:

| Method | Path | Who |
| --- | --- | --- |
| GET | `/members` | owner and admin see everyone; anybody else sees themselves |
| POST | `/members/invitations` | owner and admin |
| DELETE | `/members/:member` | owner and admin, never the last owner |
| PUT | `/members/:member/role` | owner and admin |
| PUT | `/members/:member/grants` | owner and admin |
| POST | `/invitations/accept` | any signed-in invitee |
| POST | `/switch` | any session without support impersonation |
| POST | `/support/start` | an authorized staff account |
| GET | `/projects` | any member, with per-project onboarding readiness |
| POST | `/projects` | owner and admin, `grant_access` required |
| GET | `/fleet` | any member |
| GET | `/plan` | owner and admin |
| GET | `/billing` | owner, no impersonation |
| POST | `/billing/checkout` | owner, answers `{url}` |
| POST | `/billing/portal` | owner, answers `{url}` |

A hosted owner or admin also reads and approves a project's policy descriptor
on the generic route, under
`/api/v2/organizations/:organization/projects/:project`:

| Method | Path | Who |
| --- | --- | --- |
| GET | `/policy` | owner and admin by session; worker, operator and admin by bearer token |
| PUT | `/policy` | owner and admin |

`GET` answers the current approval, or `409 policy_mismatch` when the project
has none yet. `PUT` takes the change and answers the approval:

```json
{"expected_policy_id": "", "policy": {"policy_id": "policy_…",
  "source_revision": "…", "source_digest": "…", "config_digest": "…",
  "gates": {"kind": "command", "plan_review": "human",
            "plan_stop_digest": "…", "automated_review": "optional",
            "merge_method": "squash"}}}
```

`expected_policy_id` is the approval being replaced, empty for the first one; a
mismatch answers `409 policy_mismatch`. A member or a viewer gets the same
opaque `404 not_found` every other hosted administration route answers with.
Until this existed, `PUT …/projects/:project/policy` was gated on the instance
administrator and answered `404` for every hosted session, so
`PUT …/projects/:project/onboarding/policy` was the only approval a hosted
owner had. Both routes work; they share one handler.

`POST /api/v2/organizations` creates this hub's first organization for the
reserved bootstrap subject. `:member` is the membership id `GET /members`
returns, not the user id.

`GET /fleet` answers `spend: null`. The hub records allowance consumption per
window, not currency per project, so there is no per-project spend source to
report; the field and its shape exist for the first metering source that has
one.

The project event stream moved to `GET
/api/v2/organizations/:organization/projects/:project/events`;
`GET /projects/:project/events` stays as an alias for one release.

`GET …/work-items` accepts `include=attempts,changes`, which adds
`latest_attempt` and `changes` per item for the board and list cards. Both are
absent without the parameter, so the default response is unchanged, and the
extra reads are bounded to the page the caller already asked for.

**Stored attempt diffs and the pull request panel** (`decisions.md` sections
18.5 and 18.6). Both live on the project base `nativeBase` beside the other
native resources, not under `/conversations`, because an attempt's diff and an
issue's pull requests are project resources the right panel happens to read.
They are mounted whether or not `conversation.enabled` is set.

| Method | Path | Who |
| --- | --- | --- |
| POST | `{nativeBase}/attempts/:attempt/diff` | worker token, fenced by the producer's lease |
| GET | `{nativeBase}/attempts/:attempt/diff` | hosted session or operator token, the issue's read rule |
| GET | `{nativeBase}/work-items/:item/pull-requests` | hosted session or operator token, the issue's read rule |
| POST | `{nativeBase}/work-items/:item/pull-requests/actions` | write on the project |
| POST | `{nativeBase}/work-items/:item/pull-requests/:number/actions` | write on the project |

The diff post carries its producer and generation rather than an idempotency
key: the generation *is* the record.

```json
{
  "producer": {"kind": "attempt", "id": "attempt_…", "lease_id": "…", "fencing_token": 7},
  "generation": {"source": "attempt", "seq": 4},
  "base_sha": "…", "head_sha": "…",
  "files": [{"path": "internal/app/main.go", "old_path": "", "status": "modified",
             "additions": 4, "deletions": 2, "binary": false,
             "patch": "diff --git …", "truncated": false, "denied": false}]
}
```

It answers `202 {accepted, diff_id, generation, file_count, patch_bytes,
truncated}`. The rules the hub enforces:

- **Producer fencing.** While an attempt runs, its own lease is the producer.
  The hub validates the producer the way it validates a run event — the lease is
  current, its pinned policy is approved, and it belongs to the authenticated
  runner — and requires a `running` attempt row bound to that lease and fencing
  token. Workspace sessions (18.1) are the only other legal producer; they
  exist now, but none of them produces a diff, because the live workspace diff
  channel of 18.5 is not built. A workspace producer, a stale fencing token,
  another attempt's id and a released lease are therefore all
  `409 stale_execution`.
- **Generations.** A `seq` at or below the stored one for that source is
  `409 stale_generation`. Generations are keyed by `(attempt, source, source
  id, seq)`, so a workspace's diffs will sit beside an attempt's rather than
  over them.
- **Sizes.** A patch over 1 MB is stored cut at that bound with
  `truncated: true`. A whole diff whose surviving patches exceed 20 MB is
  `413 diff_too_large`; the runner answers that by re-posting the same file
  list with the patches stripped, so the counts survive a change too large to
  store. This one route accepts a body larger than the hub's usual 1 MB.
- **Filter.** The files denylist of 18.4 — `.git/objects`, `.git/config`,
  `node_modules`, `.env*`, `*.pem`, `*.key`, `*.p12`, `id_rsa*` — is applied by
  path string to both `path` and `old_path`, on write. A denied file keeps its
  status and its counts, loses its patch and carries `denied: true`, so a later
  reader is never shown what a live reader was not. The project-level
  `workspaces.files.deny` globs are not applied on this path: the runner that
  posts a stored diff has the project's configuration and the hub path that
  filters it does not, so folding them in here would have the two halves of one
  filter disagree about the same file. The live files surface applies them on
  top of the fixed list above.

`GET …/attempts/:attempt/diff` answers the latest attempt-produced diff,
`?at=<seq>` that generation, and `404` when there is none. `?source=workspace`
is accepted and answers `404` until a workspace can produce one, so the client
can ask for it without the shape changing later. The read resolves the
attempt's work item in the caller's scope first, so a reader who cannot see the
issue cannot see its diff. `include=diff` was deliberately **not** added to the
work item list: `include=` serves the board and list cards, and a diff belongs
to an attempt, not to a card.

The pull request panel answers the 18.6 array. Each row is one of the issue's
change requests joined with the GitHub connector's projection of its pull
request, and carries `connector: {provider, repository, synchronized_at}` or
`connector: null` for a project with no repository bound, which is what lets
the client show T3's unavailable state. `mergeable` is `true`, `false` or the
string `"unknown"`, mapped from the projection's `mergeable_state`: `dirty` is
false, an empty or unknown state is `"unknown"`, anything else is true.
`checks` and `reviews` come from the change request's own evidence;
`review_decision` prefers the connector's decision when it has one.

Two limits of the connector half are worth stating plainly, because the hub
cannot do better today:

- **The hub never calls GitHub on this path.** It holds no per-project GitHub
  credential — a single-tenant hub has one process-wide `gh auth token`, and a
  hosted hub has no GitHub transport at all — so the connector half is the
  `pull_requests` projection the webhook ingest and the reconciler maintain.
  `fetched_at` is that projection's `synchronized_at`. `?refresh=1` therefore
  queues a `pull_request` hydration request for the reconciler rather than
  fetching inline, and is limited to one per organization every ten seconds;
  beyond that it answers `429 refresh_limited`. The assembled view is cached in
  process for sixty seconds.
- **`from_fork` is always false and `labels` is always empty.** The projection
  records neither a head repository nor pull request labels, so a pull request
  the hub can see is always on the bound repository. Both fields are present in
  the shape and will carry real values when the connector projects them.

An action is not executed by the hub. `POST …/pull-requests/actions
{idempotency_key, action: "open", expected_head_sha}` and `POST
…/pull-requests/:number/actions {idempotency_key, action: "update_branch" |
"merge", expected_head_sha}` answer `202 {work_item, action_id}`: they create
one native issue labelled `detent:pull-request-action` in the project's
`Merging` lane when it has one and in its first dispatchable state otherwise,
so the existing merge queue claims it on a runner with a checkout. The label is
reserved — an API caller that sets it is refused — and the association is
recorded in `pull_request_actions`. A head that no longer matches
`expected_head_sha` is `409 head_moved` with both shas in `details`; `merge`
additionally requires the change request to have reached its review policy's
`reviewed` status, and is `409 merge_not_permitted` otherwise. The action is
idempotent by key like every other mutation, so a replay answers with the first
call's work item instead of queueing a second one.

**Workspace sessions and the relay** (`decisions.md` sections 18.1, 18.2, 18.4
and 18.13). They live on `nativeBase` beside the other native resources and are
mounted only when the hub configures `workspaces.enabled`; without it every
route below answers `422 unsupported_control` and the right panel's Files tile
stays disabled with its reason.

| Method | Path | Who |
| --- | --- | --- |
| POST | `{nativeBase}/workspaces` | write on the project; `terminal` in `requires` also needs the grant's runner flag |
| GET | `{nativeBase}/workspaces?work_item=&state=` | hosted session or operator token, the issue's read rule, one row at a time |
| GET | `{nativeBase}/workspaces/:workspace` | the issue's read rule |
| DELETE | `{nativeBase}/workspaces/:workspace` | write on the project |
| POST | `{nativeBase}/workspaces/:workspace/relay-tickets` | write on the project, CSRF header, `Origin` checked |
| GET | `{nativeBase}/workspaces/:workspace/relay?ticket=` | the ticket, redeemed once, bound to the session that minted it |
| GET | `{nativeBase}/work-items/:item/workspace` | worker token, fenced by the workspace tuple |
| GET | `{nativeBase}/workspaces/:workspace/worker/relay` | worker token, fenced by the workspace tuple, headers `X-Detent-Lease` and `X-Detent-Fencing-Token` |
| POST | `{nativeBase}/workspaces/:workspace/worker/bind` | worker token, fenced by the workspace tuple |
| POST | `{nativeBase}/workspaces/:workspace/worker/heartbeat` | worker token, fenced by the workspace tuple |
| POST | `{nativeBase}/workspaces/:workspace/worker/unbind` | worker token, fenced by the workspace tuple |
| GET | `{nativeBase}/workspaces/:workspace/terminal-recordings` | the issue's read rule, then narrowed by the handler: a recording is readable by the person who ran it plus owners and admins, and by owners alone at `user` isolation (§18.3). A recording this actor may not read is absent from the listing rather than refused. |
| GET | `{nativeBase}/workspaces/:workspace/terminal-recordings/:recording` | the same, answering the asciicast v2 document as `application/x-asciicast` |

`GET {nativeBase}/work-items/:item/workspace` is **not** in `decisions.md`
18.1. A claim hands a runner a lease on a work item while every worker endpoint
is addressed by workspace id, and something has to join the two; asking the
runner to parse it out of the issue body would make the dispatch record part of
the wire format. It is fenced by the same tuple `bind` is, so a runner cannot
read the workspace of work it did not claim.

The request body is `{idempotency_key, work_item_id | attempt_id, ref?,
requires?}`. `requires` names any of `files`, `diff`, `terminal`, `preview` and
`git`, and defaults to `["files", "diff"]`; an unknown capability
is `422 invalid_request` rather than a silent drop, because a client that asked
for something the hub does not know must not receive a workspace that cannot
serve it. The refusals an operator will see:

- `409 workspace_exists` with `details.workspace_id` — one workspace per
  worktree at a time. The client's next move is to use the existing one.
- `422 workspace_limit` with `details.scope` (`organization` or `person`) and
  `details.limit` — `plan.workspaces.max_open`, default 20, and the per-person
  cap of 3.
- `403 forbidden` — a terminal was asked for while the subject attempt is still
  running, with `workspaces.terminal.enabled` off, without the project's runner
  grant, on an organization running `workspaces.terminal.isolation: user` by
  somebody who is not an owner or admin, or by a support session at a level
  other than `container`. The message names which (§18.3).
- `409 stale_execution` on a worker endpoint — this runner is no longer the
  workspace's owner and must stop serving it.

**Readiness is a subscription, never a poll.** Every transition emits
`workspace.<state>` on the project event stream with the resource as `data`.
That stream used to carry one integer with no id and no body; it now carries
the typed events beside the original `activity` frame, accepts `?after=<seq>`,
honours `Last-Event-ID`, and answers a cursor it cannot replay with `event:
closed` / `{"reason": "cursor_expired"}`. The typed log is swept after an hour,
which is generous next to the sixty seconds a browser reconnect needs.

**The relay.** A person mints a ticket with a POST that carries the CSRF header
and a checked `Origin`, then opens the socket with `?ticket=`. The ticket is 32
bytes of randomness, single use, valid for 30 seconds and bound to both the
workspace and the session; only its digest is stored. Frames are JSON
`{channel, stream, type, seq, payload}` at most 256 KB; the hub allocates the
stream id as `<connection_id>:<n>` on the first request and stamps
`actor: {principal_id, subject, connection_id, name, email}` on the
runner-bound copy — the name and the email are 18.13's commit author, and they
are the hub's to supply for the same reason the rest of the tuple is. The
caps are fixed rather than configurable, because a client and a runner that
disagreed about them would disagree about when a stream dies: 8 streams per
connection, 32 per workspace, 1 MB of unacknowledged frames per stream before
`overflow`, `workspaces.relay.memory` for the process before `relay_busy`, and
a 60-second window to `resume {stream, last_seq}` from the same principal and
session.

**What the runner serves today.** The files channel, the exec channel and
the git channel. A runner reports `workspace_capabilities` in its heartbeat
and this slice reports `files`, `exec` and `git`, so a workspace that asks
for `diff`, `terminal` or `preview` stays `requested` and fails with
`no_runner` after `workspaces.request_timeout` rather than being claimed by a
runner that would refuse it frame by frame. `watch` on the files channel is
answered `unsupported`, and the client refreshes on focus.

**Project actions and the exec channel** (`decisions.md` section 18.12). An
action is a command stored on the project and run on a workspace session's
worktree. They live on `nativeBase` beside the other native resources.

| Method | Path | Who |
| --- | --- | --- |
| GET | `{nativeBase}/actions` | the project read rule; the whole set, no cursor |
| POST | `{nativeBase}/actions` | write on the project |
| PATCH | `{nativeBase}/actions/:action` | write on the project, `expected_revision` |
| DELETE | `{nativeBase}/actions/:action` | write on the project |
| POST | `{nativeBase}/actions/:action/runs` | write on the project; 202 with `{run_id}` |
| GET | `{nativeBase}/actions/:action/runs?limit=` | the project read rule; newest first |
| GET | `{nativeBase}/actions/:action/runs/:run` | the project read rule |
| GET | `{nativeBase}/actions/:action/runs/:run/output` | the project read rule; `text/plain` |
| POST | `{nativeBase}/workspaces/:workspace/worker/action-runs` | worker token, fenced by the workspace tuple |

The write body is `{idempotency_key, name, command, keybinding?, icon?,
preview_url?, open_preview?, run_on_worktree_creation?}`, and every mutation
carries the hosted boundary's CSRF header like the rest of section 12. The
refusals an operator will see:

- `422 invalid_request` — an empty name or command, a command over 4 KiB, an
  unknown icon, a `preview_url` that is not `http://` or `https://`, a
  keybinding that is not a single chord, a keybinding another action in the
  project already claims, or a 51st action (the cap is 50 per project, because
  the header menu lists every one and a chord is registered per action).
- `409 conflict` with the current revision — a `PATCH` whose
  `expected_revision` is stale.
- `409` on `POST .../runs` — the workspace is unbound, is read-only because its
  attempt is still running, or does not report `capabilities.exec`.

**Runs.** The row is written `queued` before any frame reaches a runner, so a
run that never starts is visible rather than absent. `exit_code` is null until
the process exits, and a run that failed without exiting carries a `reason`
(`lease_lost`, `stream_closed`, `killed`, `workspace_closed`) instead — so
"exited 0" and "never ran" are never the same value. Output is capped at 1 MiB
with `[output truncated at 1 MiB]` appended and `truncated: true` set; the hub
stores it itself, on the `attempt_diffs` precedent, and `output_artifact` names
the `/output` path above. Every transition emits `action_run.<status>` on the
project event stream with the run as `data`, exactly as `workspace.<state>`
does, so a client follows a run it did not start by subscription.

**Run on worktree creation.** An action with the flag runs when a workspace
session reaches `ready` for a *fresh* worktree, in authoring order, one at a
time, reported through the worker endpoint under the workspace lease. A
retained worktree runs nothing, and a setup command that exits non-zero is
recorded as a failed run without stopping the workspace reaching `ready`.

**Not wired.** `preview_url` and `open_preview` are stored and echoed and
nothing acts on them: the Browser surface is snapshot-first and the live
preview is deferred (18.7, 18.9), so the client renders the switch disabled
with the reason rather than hiding the field.


**The git channel** (`decisions.md` 18.13) is what the header's `Commit, push &
PR ▾` group acts through. Three request frames on 18.2's rules, request/response
and stream-reusing exactly like `files`:

| Frame | Answer | Who |
| --- | --- | --- |
| `status` | `status {branch, detached, remote, upstream, ahead, behind, dirty_file_count, head_sha}` | project read |
| `commit {message}` | `committed {commit, branch, files, excluded}` | `write` on the project, re-validated per frame |
| `push` | `pushed {branch, remote, commit}` | `write` on the project, re-validated per frame |

A commit stages every dirty path except the 18.4 denylist ones and reports what
it left out in `excluded`; a silent drop is exactly what a denylist must not
become. A push sends the current branch to the branch's own remote, else
`origin`, else the single remote there is, and never with a force flag of any
kind. The author is the acting person — the hub stamps a `name` and an `email`
onto the `actor` it already stamps, and a frame it cannot name an author for is
refused — and the committer is the runner, which is git's own distinction.

The refusals an operator will see on this channel:

- `error {code: "forbidden"}` — no `write` grant on the project, the runner did
  not report the `git` capability, or the hub could not name the acting person
  (an operator token, or a support session standing in for someone).
- `error {code: "read_only"}` — the subject attempt is still running, so its
  worktree refuses a write. The hub refuses this before the runner sees the
  frame, so it is also the answer when no runner is attached.
- `error {code: "stale_execution"}` — the runner lost its lease between the
  frame arriving and it acting.
- `error {code: "unsupported"}` — this worktree is not a git repository. The
  workspace is reported without the `git` capability rather than failed, so the
  header disables the group with the reason instead of a surface breaking.
- `error {code: "git_failed", stderr}` — git ran and said no. The stderr is
  git's own, verbatim: a person acting on a rejected push needs what git said.

`Create PR` is not on this channel. It is 18.6's `POST
…/work-items/:id/pull-requests/actions {idempotency_key, action: "open",
expected_head_sha}`, above, whose `expected_head_sha` is the sha the last
`status` read after the push.

**Where the worktree is** (18.13). The workspace resource carries `machine_hostname`
and `worktree_path`, both from the runner's heartbeat rather than from the bind,
so a runner that re-prepared a fresh worktree corrects them instead of leaving a
path that no longer exists. The header's `Open ▾` picker hands the operating
system `cursor://file/<path>` or `vscode://file/<path>` only when the reported
host is the machine the browser is on; otherwise every item is disabled naming
the host. A page served over loopback is never taken as proof of being on the
runner's machine, because handing an operating system a path that is not there
is the one failure worth being conservative about.

### 1.3 Turn preferences, references and Settled

**Turn preferences.** A conversation carries
`preferences: {model, reasoning_effort, access}`. Each field is `"auto"` or an
explicit value: a model from the bootstrap's choices, an effort of `low`,
`medium` or `high`, and an access of `read_only` or `full`. `PATCH
…/conversations/:conversation` accepts `preferences` alongside `title`; an
absent field keeps what is stored, and `"auto"` returns a field to the
project's configured default. Anything outside those vocabularies is
`invalid_request`, and a patch that asks for neither a title nor preferences is
refused rather than silently accepted.

Explicit preferences travel to every turn the conversation produces:

- The transitional hub-side coordinator applies the model and the effort to its
  own turn request. `ReadOnly` stays true whatever `access` says — a
  coordinator turn never changes anything.
- A runner-dispatched turn receives them in the `preferences` field of the bind
  response (`POST …/work-items/:item/conversation/bind`). The runner applies an
  explicit model and effort to its turn request, and `access: read_only`
  forbids writes. `full` never re-enables writes the run's own mode forbade.
- A linked conversation's preferences are also written into the issue body as a
  `detent-agent` block, so a runner that never sees the conversation still
  honours them. The block is written when the issue is created and rewritten
  whenever the preferences change; `auto` values are omitted, and returning
  every field to `auto` removes the block. A change that would produce the body
  the issue already has writes nothing, so the issue revision only moves when
  something really changed.

**References.** Message text may name `#123`, `<project>#123`,
`<org>/<project>#123` or a conversation id. The hub extracts them when a user
message is accepted and again when an assistant message reaches a terminal
delivery, resolves each one — a bare `#123` inside the conversation's own
project — and stores only the targets it could resolve. Extraction is bounded
to 20 references per message. The message resource carries them:

```json
"references": [
  {"kind": "issue", "id": "wi_3363", "label": "#3363", "url": "/work/i/wi_3363"},
  {"kind": "conversation", "id": "conv_…", "label": "conv_…", "url": "/chat/c/conv_…"}
]
```

`GET …/work-items/:item/references` lists the other side: every message that
named the issue, as `{message_id, conversation_id, excerpt, created_at}`,
newest first. The excerpt is bounded to 200 runes, ellipsis included. The
listing needs read access to the issue *and* to the conversation: entries from
private chats the reader does not own are left out, so a shared issue never
leaks a private chat's words.

**Handoff next step.** `POST …/conversations/:conversation/link` accepts
`next: {state, priority, dispatch}`, all optional. `state` must be one of the
project's workflow states. `priority` is the numeric rank 0 to 3, and is absent
by default. `dispatch` is `now` or `later`: `now` creates the issue in the
first dispatchable state, and `later` creates it in `Backlog` when the project
has a non-dispatchable, non-terminal state of that name, whatever its casing,
and in the first dispatchable state otherwise. An explicit `state` wins over
`dispatch`. The response echoes what was applied as
`next: {state, priority, dispatch}`, so the client never has to infer where the
issue landed.

**Settled.** `archived` is gone. A conversation is `active` or `settled`, there
are no archive and unarchive endpoints, and `conversation_archived` is no
longer an error code: a settled conversation accepts commands and unsettles.
Both list endpoints accept `settled=true|false`; without it both groups are
listed, active first. The sidebar groups Settled as T3 does.

A sweep settles conversations on its own. Every five minutes a background pass
looks for active conversations whose execution has ended or never started —
`idle`, `completed`, `interrupted`, `failed` or `unknown` — and whose last
activity is older than `conversation.settle_window` (see section 2, default 24
hours), and settles them with a `conversation.updated` event. A conversation
still waiting for a runner, or running, is never settled. Any accepted command
or new message unsettles, so the wake needs no separate action.

## 2. Configuration

The product is configured by the `conversation:` section of the file named by
`detent hub serve --hosted-config`. There is no separate flag and no
environment variable.

The section is read only when that same file also configures hosted identity
(`internal/cli/hub.go`). A single-tenant hub never mounts the product: the
`/chat` routes require a hosted session and the hosted CSRF scheme, neither of
which a single-tenant hub has. Unknown keys anywhere in the hosted file are
rejected, so a misspelled conversation key fails the whole file at startup.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `enabled` | bool | `false` | Mounts the conversation API, the worker endpoints, the event streams and the `/chat` client. |
| `codex` | mapping | absent | Transitional hub-side coordinator backend for conversations without a linked issue. Self-hosted hubs only; never set on Detent Cloud. See below for what an absent section means. |
| `codex.command` | string | `codex` | Executable used for coordinator turns. |
| `codex.model` | string | empty | Model passed to coordinator turns. Empty leaves the backend default. |
| `codex.reasoning_effort` | string | empty | Reasoning effort passed to coordinator turns. |
| `codex.options` | mapping | empty | The same Codex option shape the runner uses (`internal/config.CodexOptions`): `shell`, `model_provider`, `service_tier`, `approval_policy`, `thread_sandbox`, `turn_sandbox_policy`, `turn_timeout_ms`, `read_timeout_ms`, `stall_timeout_ms`, `deliverable_elicitation_allowlist`. |
| `workspace` | string | none | Directory coordinator turns run in. Required when `codex` is configured, and it must already exist and be a directory. |
| `question_timeout` | duration | `24h` | How long a pending runner question waits for an answer before it expires. Must parse as a positive Go duration. |
| `settle_window` | duration | `24h` | How long a conversation whose execution has ended or never started may sit without activity before the sweep settles it. Must parse as a positive Go duration. |
| `control_queue_size` | int | `64` | Queued controls allowed per conversation before acceptance fails with `queue_full`. |

The `workspaces:` section of the same file configures workspace sessions and
the relay (`decisions.md` 18.1 and 18.2). It is independent of `conversation:`:
a hub may run either without the other, and with the section absent the right
panel's Files tile stays disabled with its reason.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `enabled` | bool | `false` | Mounts the workspace endpoints and the relay. |
| `request_timeout` | duration | `5m` | How long a workspace stays `requested` with no eligible runner before it fails with `no_runner`. |
| `retain_after_run` | duration | `30m` | How long an attempt's worktree stays available after the run. Past it a workspace on that attempt checks out `head_sha` fresh, and any eligible runner may claim it rather than only the one that produced it. `0s` is legal and means every workspace checks out fresh. |
| `idle_timeout` | duration | `30m` | The `idle_timeout_seconds` a workspace reports. Only what a person *does* resets it — a request, an input, a resize — so a busy shell left alone still expires. Runner output does not, and neither does the acknowledgement that carries it: a stream that is only receiving would otherwise keep a workspace alive by admitting it had received something. |
| `max_lifetime` | duration | `4h` | The hard cap no workspace outlives, whatever it is doing. |
| `plan.max_open` | int | `20` | Open workspaces per organization. Beyond it a request is `422 workspace_limit` with `details.scope: "organization"`. |
| `person_max_open` | int | `3` | Open workspaces per person, the cap 18.1 fixes. It exists so an operator can lower it, never so one can be surprised by a higher default. |
| `relay.memory` | byte size | `256MB` | The hub process's whole relay budget. Beyond it new streams fail with `relay_busy`. Accepts a bare byte count or a `KB`/`MB`/`GB` suffix. |
| `files.deny` | list of globs | empty | Extra denylist patterns, applied on top of the fixed list every surface already refuses. Matched against the whole worktree-relative path and each of its segments, so `secrets` denies `secrets/prod/key.txt`. An unparseable glob fails the file at startup, because a pattern that never matches is a hole in the filter. |
| `terminal.enabled` | bool | `false` | Whether a terminal may be requested at all. An owner turns it on. |
| `terminal.isolation` | `container` or `user` | `container` | The level a terminal runs at. `user` is the runner account's own authority handed to a person and is allowed only when an organization sets it explicitly. |
| `terminal.record` | bool | `true` | Whether terminal streams are recorded. |

The terminal keys are live (`decisions.md` §18.3). Three things about them are
worth saying out loud.

`terminal.enabled` defaults **off**: a terminal is the runner account's shell
handed to a person, and section 18.3 is explicit that stripping environment
variables does not confine one. An owner turns it on for the organization.

`terminal.isolation` defaults to `container`, which is the level the setting
recommends and the one a member with `write` and the `runners` grant may use.
**No runner in this build can provide it.** There is no container runtime hook
anywhere in the repository, so a runner reports `user` and a workspace asking
for a terminal on an organization that requires `container` is claimed and then
refused at the channel — which is what section 18.3 says such a runner must do
rather than hand back a plain PTY under the recommended name. An organization
that wants a working terminal today therefore sets `isolation: user`
explicitly, and section 18.3 then allows it to owners and admins only.

`terminal.record` defaults on and cannot distinguish "absent" from "false" in
Go, so the key is a pointer in the configuration reader and the hub defaults a
nil to on. A file has to say `record: false` to turn it off. Recordings are
stored by the hub itself, capped at 1 MiB with a `truncated` flag, and read back
through the two `terminal-recordings` endpoints in section 1.2 — never through
the issue's own read rule, because a recording can carry what the runner
account can see.

Example, added to the hosted configuration described in
[hosted identity](../hosted-identity.md):

```yaml
organization_id: org_example_opaque_id
bootstrap_subject: user_example
public_url: https://organization.example.test
workos:
  client_id: client_example
  api_key_env: WORKOS_API_KEY
conversation:
  enabled: true
  workspace: /var/lib/detent/conversation
  question_timeout: 24h
  settle_window: 24h
  control_queue_size: 64
  codex:
    command: codex
    model: gpt-6-astra
    reasoning_effort: low
    options:
      turn_timeout_ms: 600000
```

### Hub-side coordinator is transitional

Detent does not run model turns on the hub or hold provider credentials; the
customer's runner and the customer's ChatGPT, Claude or API-key login pay for
every turn (`decisions.md` section 1, decided September 10, 2026). The `codex`
and `workspace` keys below exist only because the first slice shipped before
the runner-dispatched coordinator. Set them only on a hub the customer hosts
on their own machine, where the Codex login on that host is theirs. On Detent
Cloud leave `codex` omitted, so ordinary chats report a missing coordinator
rather than being answered on Detent's account. Both keys are removed when the
coordinator moves to runners.

### When `codex` is omitted: the runner-dispatched coordinator

Omitting `codex` is now the default and the supported path, not a degraded
one. The section stays valid, the product still mounts and `workspace` is not
required. What happens instead (`decisions.md` section 9):

- Linked conversations work unchanged. Their turns run on the issue's runner,
  not on the hub, so they need no hub-side backend.
- An ordinary chat message opens a **coordinator work item**: a native issue
  in the conversation's project titled
  `Coordinator turn for conversation <last 8 characters of the id>`, labelled
  `detent:coordinator`, created in the project's first dispatchable workflow
  state, with a body that names the conversation and states that no
  implementation, worktree, branch or pull request is expected. Neither the
  title nor the body repeats anything the user wrote: an issue is readable by
  every project reader while the chat stays private
  (`decisions.md` section 10.1). A row in `coordinator_items` records the
  association. The conversation is **not** linked: its `work_item_id` stays
  empty and it stays private. Creating the item runs the same hosted feature
  and growth checks as `POST /work-items`, so an exhausted plan refuses the
  turn with `allowance_exhausted` and persists nothing.
- `detent:coordinator` is reserved. `POST /work-items` and
  `PATCH /work-items/:item` refuse it with `invalid_request` ("Label
  detent:coordinator is reserved"), whatever the credential, so no tracker
  writer can turn an ordinary issue into a read-only coordinator run. Only the
  hub's own coordinator-item path may set it.
- The message is `queued` and the execution reports `waiting_for_runner` until
  a runner binds. While a coordinator item is open, further messages reuse it
  and reach the bound attempt through the controls poll.
- Only an enrolled runner whose fresh provider report (within
  `providercapacity.MaxAge`) names a live-control backend — today `codex` —
  may claim a coordinator item. The claim loop skips it for every other
  credential, including legacy machine registrations, and moves on to the next
  candidate; a skipped coordinator item never consumes a claim.
- When the bound attempt unbinds, the hub moves the item to the project's
  first terminal state and closes the association. The next message opens a
  new coordinator item. Linking the conversation to a real issue closes the
  open item too: the handoff replaces the coordinator turn.
- `cancel` on such a chat is delivered to the bound attempt as an `interrupt`
  control; the client-facing kind stays `cancel`. With no bound attempt the
  receipt stays `rejected` with `{"code": "no_active_turn"}`.
- `GET /app/bootstrap` reports `capabilities.coordinator: true` when the hub
  has a backend **or** an active runner with a fresh heartbeat, a grant on one
  of the actor's projects and a fresh live-control provider report is
  enrolled.

### Where coordinator items appear

Coordinator items are conversation turns, not project work, so they are hidden
from the places that list work:

- the hosted project page's issue list,
- `GET .../work-items` unless the query carries `include=coordinator`,
- the hub coordinator's `list_attention` scan.

They keep their full native history, attempts, events and audit trail, and
they are still readable one by one through `GET .../work-items/<id>`. Both
open and closed coordinator items are hidden; `include=coordinator` shows both.
To see what a chat dispatched, list with `include=coordinator` or read
`coordinator_items` directly:

```sh
sqlite3 hub.db "SELECT work_item_id, conversation_id, created_at, closed_at FROM coordinator_items ORDER BY created_at DESC LIMIT 20;"
```

### Provider threads follow the runner that made them, and the kind of turn

`conversations.provider_thread_runner_id` records which runner's provider
login produced the conversation's thread; it is empty for the transitional
hub-side coordinator. `conversations.provider_thread_origin` records which
kind of turn produced it: `coordinator` or `worker`. A worker `bind` returns
`resume.thread_id` only when that runner is the one binding **and** the
recorded origin is `worker`; a coordinator `bind` gets it back only when the
origin is `coordinator`. Otherwise the thread is not the right thread for this
turn and the response carries `resume.transcript` instead: the last 20
messages as `{role, kind, text}`, each bounded to 2000 runes, for the runner
to prepend as clearly delimited data. `turn_started` records the thread
together with the reporting runner and the origin.

The origin rule exists because a provider thread carries the instructions and
the permission set of the turn that opened it. A coordinator thread was
started read-only, with no checkout and no authority to change files, run
tests or move issue state; resuming it from a worker attempt hands the worker
those restrictions, and the model says so rather than doing the work — which
is exactly what the third dogfood run saw (section 8). A thread recorded
before the column existed has an empty origin and never resumes. `bind` also
returns `resume.thread_origin`, the origin the binding turn records, so a
different runner reaches the same answer with no thread registry of its own.

To see what a conversation is carrying:

```sh
sqlite3 hub.db "SELECT id, provider_thread_id, provider_thread_runner_id, provider_thread_origin FROM conversations WHERE id = 'conv_…';"
```

The execution resource reports which of the two happened:
`execution.resume` is `"thread"`, `"transcript"` or `""`. A bind that hands
over a transcript also appends a `role: system, kind: status` message reading
"Provider history was not available on this runner; continuing from a
transcript of the last N messages", so the recovery is visible in the history
rather than only in the runner's prompt (`decisions.md` section 10.4).

A runner that supplies `thread_id` on the bind is resuming its own thread and
always gets it back. Otherwise both runner ids must be present and equal: a
legacy worker token has no runner id, so it is never handed a thread the
hub-side coordinator produced, and receives a transcript instead. Coordinator
items are only ever claimed by enrolled runners, so a coordinator bind always
compares two real runner identities.

### Supported providers and backends

The transitional hub-side coordinator backend is Codex only. `codex` is the
sole backend key in the section and it is built through the same Codex
backend constructor the runner uses. The runner-dispatched coordinator will
require the same live-control backend a linked attempt does.

On the runner side, live conversation control requires a backend that
implements `SupportsLiveControl()` and returns true. Only the Codex backend
does. With any other backend the run still executes the issue normally, but no
conversation binding is made: the runner logs `worker_conversation_bind_skipped`
and continues. A conversation never blocks a run.

The API reports that state rather than pretending: after linking, the execution
is `waiting_for_runner` and `execution.capabilities` is
`{"steer": false, "interrupt": false, "answer": false, "continue": false}`.
Capabilities are only ever set from the worker's bind request, so a
conversation whose runner cannot bind never advertises steering, interruption
or answering, and the client hides those controls.

## 3. Storage and migrations

Migration `internal/hubserver/migrations/00022_create_conversations.sql` adds
seven tables and their indexes:

| Table | Holds |
|---|---|
| `conversations` | One row per conversation: audience, status, optional linked work item, revision, provider thread, execution JSON, event head. |
| `conversation_messages` | Ordered transcript with delivery state, attempt and turn attribution, and the accumulated assistant text. |
| `conversation_questions` | Pending and settled runner questions with prompts, answers and expiry. |
| `conversation_commands` | Idempotency records: one receipt per command key. |
| `conversation_events` | The per-conversation event log the stream replays from. |
| `conversation_starts` | One row per attempt that started a conversation turn. |
| `conversation_turn_batches` | One row per applied turn-event batch that carried a `batch_key`, with the event sequence it produced, so a retried batch applies nothing and answers with the first attempt's outcome. |
| `conversation_audience_events` | Audit of private-to-shared transitions. |

The migration is additive. It creates new tables only; no existing table,
index or row is altered. It is reversible in the goose sense: the
`-- +goose Down` section drops all seven tables in reverse dependency order.
See section 5 for what that actually costs.

Migration `internal/hubserver/migrations/00023_coordinator_items.sql` adds the
runner-dispatched coordinator: the table `coordinator_items` (`work_item_id`
primary key, `conversation_id`, `organization_id`, `project_id`, `created_at`,
nullable `closed_at`) with an index on `(conversation_id, closed_at)`, and the
column `conversations.provider_thread_runner_id` (text, default empty). Its
down section drops the table and the column.

Migration `internal/hubserver/migrations/00024_conversation_preferences.sql`
adds turn preferences, message references and Settled (decisions section 14).
It is the one conversation migration that is not additive: the `conversations`
table is rebuilt, because the `status` check constraint has to change from
`('active', 'archived')` to `('active', 'settled')` and SQLite cannot alter a
constraint in place. The rebuild copies every row, maps an `archived` status to
`settled`, renames `archived_at` to `settled_at` and adds `preferences_json`
(default `{}`), then recreates the three indexes 00022 created and adds
`conversations_settle_idx` on `(status, COALESCE(last_message_at, updated_at))`
for the sweep. It also creates `message_references` (`message_id`,
`conversation_id`, `target_kind`, `target_id`, `label`, `created_at`, primary
key `(message_id, target_kind, target_id)`) with an index on
`(target_kind, target_id, created_at, message_id)` for the "referenced from"
listing.

Its down section reverses both: it drops `message_references` and rebuilds
`conversations` with the 00022 shape, mapping `settled` back to `archived`.
Preferences are lost on the way down, because the old table has nowhere to put
them.

Migration `internal/hubserver/migrations/00025_attachments_usage.sql` adds
conversation attachments and per-attempt usage (decisions section 17, items 1
and 5): `conversation_attachments`, `conversation_attachment_blobs` and
`attempt_usage`.

Migration
`internal/hubserver/migrations/00026_attempt_diffs_pull_request_actions.sql`
adds stored attempt diffs and pull request actions (decisions sections 18.5 and
18.6). It is additive; it creates three tables and alters nothing.

| Table | Holds |
|---|---|
| `attempt_diffs` | One row per stored generation of one attempt's worktree: the generation `(source, source_id, seq)`, `base_sha` and `head_sha`, the producer tuple `(kind, id, runner_id, lease_id, fencing_token)`, and sizes (`file_count`, `patch_bytes` stored, `posted_bytes` as sent, `truncated`). `UNIQUE (attempt_id, source, source_id, seq)` is what makes a replayed generation impossible rather than merely refused. |
| `attempt_diff_files` | One row per file of one stored diff, in producer order: `path`, `old_path`, `status`, `additions`, `deletions`, `binary`, `patch`, `truncated`, `denied`. A `CHECK` forbids a denied file from carrying a patch, so the 18.4 filter is enforced by the schema and not only by the code that writes it. |
| `pull_request_actions` | The association between an action's queued native issue and what it was asked to do: `work_item_id`, `subject_work_item_id`, `action`, `number`, `expected_head_sha`, `actor_id`, `created_at`, nullable `closed_at`. |

`attempt_diffs.attempt_id` carries no foreign key, for the same reason
`attempt_usage.attempt_id` does not: a diff is posted *before* the run event
that references it, so it can arrive on a checkpoint the attempt recorder has
not ordered yet. Its down section drops the three tables in reverse dependency
order.

Migration `internal/hubserver/migrations/00027_workspace_sessions.sql` adds
workspace sessions, the relay and the typed project event stream (decisions
sections 18.1, 18.2 and 12). It creates six tables and adds two columns to
`runner_identities`; it alters no existing row.

| Table | Holds |
|---|---|
| `project_events` | The typed per-project event log the stream replays from: `(organization_id, project_id, seq)`, `type`, `subject_id`, `data_json`, `created_at`. Section 12's stream carried one integer with no id and no body, so a client could learn that something changed and nothing about what; 18.1 requires the opposite. Swept after an hour. |
| `workspace_sessions` | One row per workspace: its own owner tuple `(runner_id, machine_id, lease_id, fencing_token)`, the state and reason, `requires` and the runner's reported `capabilities`, `read_only`, the three deadlines (`requested_expires_at`, `expires_at`, `idle_timeout_seconds`), `last_heartbeat_at` and the hub-restart `rebind_deadline`. A partial unique index over open states is what makes a second workspace on one attempt impossible rather than merely refused. |
| `workspace_items` | The association between a workspace and the `detent:workspace` native issue that dispatches it, in the shape `coordinator_items` uses. |
| `workspace_occupancy` | `(workspace_id, runner_id, started_at, ended_at)`, written from the `starting` transition and ended at the closing one. The usage report's runner rows read `workspace_sessions` and `workspace_seconds` from here and never from relay traffic, so double counting is impossible by construction. |
| `workspace_relay_tickets` | Ticket digests, bound to a workspace and a session, with an expiry and a single-use `redeemed_at`. Only the digest is stored, for the same reason an API token's is. |
| `workspace_relay_sessions` | One audit row per person connection: principal, subject, connection, channels, byte counters, close reason and the support reason where there is one. |

`runner_identities` gains `workspace_capabilities_json` and
`workspace_isolation`: the claim gate asks whether a runner can serve a
workspace *and* whether it was saying so recently, and freshness means nothing
unless the answer sits in the same row as `last_heartbeat_at`. The default is
an empty object, so a runner that has never reported serves no surface.
`workspace_sessions.attempt_id` carries no foreign key, for the same reason
`attempt_diffs.attempt_id` does not.

Migration `internal/hubserver/migrations/00028_conversation_thread_origin.sql`
adds the column `conversations.provider_thread_origin` (text, default empty),
which records whether a coordinator turn or a worker attempt produced the
conversation's provider thread (decisions section 9.3). It is additive and
reversible; existing rows keep the empty default, which never resumes, so a
conversation whose thread predates the column continues from a transcript
once and records the origin on its next turn.

Migration `internal/hubserver/migrations/00029_dispatch_requests.sql` stops the
re-dispatch loop in section 7. It is additive and reversible: four columns, no
table rebuilt and no existing row altered.

| Column | Holds |
|---|---|
| `native_attempts.work_item_revision` | The `issues.revision` the attempt was dispatched for, written at `run.started` from the item's own row in the same transaction. Default 0, which is below every real revision, so an attempt recorded before this migration suppresses nothing. |
| `native_attempts.dispatch_generation` | The `issues.dispatch_generation` the attempt was dispatched under, written the same way and for the same reason. |
| `issues.dispatch_generation` | The count of explicit requests for another attempt on this item. A conversation continuation changes nothing about the item, so it cannot be inferred from a revision; it is recorded here instead (decisions sections 9.2.1 and 10.17). |
| `issues.dispatch_requested_at` | When the last such request was accepted. Read by people, not by the claim query, which compares generations. |

A claim candidate is now required to be unanswered as well as dispatchable:
`claimCandidateIDs` excludes an item whose latest attempt — highest
`fencing_token` — has `status = 'succeeded'` at
`work_item_revision >= issues.revision` and
`dispatch_generation >= issues.dispatch_generation`. Its down section drops the
four columns in reverse order; a rollback restores the loop and nothing else,
because no other reader depends on them.

Migration `internal/hubserver/migrations/00030_project_actions.sql` adds
project actions and their runs (decisions section 18.12). It is additive; it
creates two tables and alters nothing.

| Table | Holds |
|---|---|
| `project_actions` | One row per stored action: name, command, chord, icon, the optional preview URL, the two switches, `created_by` and `revision`. Indexed for the per-project listing in authoring order, which is also the order the run-on-worktree-creation set runs in. A partial unique index on a non-empty `keybinding` is what makes two actions claiming one chord impossible under a race rather than merely refused. |
| `project_action_runs` | One row per run: the command as it was when the run started, the status, the exit code (null until the process exits), the failure reason where there is no exit code, the timestamps, the captured `output`, its byte count and the truncation flag. The command is snapshotted rather than read through `action_id` so editing an action does not rewrite the history of what already ran. Runs cascade with their action. |

The output lives in a column rather than in the artifact service, on the
`attempt_diffs` precedent above: a bounded, already-audience-scoped text blob
the hub keeps, where the exec channel's 1 MiB cap is what makes keeping it
safe. A run's output has exactly the read audience the run does, so a blob
store would buy a second audience to keep in step and nothing else.

Migration `internal/hubserver/migrations/00032_action_run_claims.sql` adds
`project_action_runs.claimed_by` and a partial index on the queued rows
(decisions section 18.12). It is additive; the column defaults to the empty
string, so every run already on disk reads as unclaimed, which is what a
finished run is.

`claimed_by` exists because a queued run has two possible executors — the
person who opens the exec channel for it, and the hub handing it to the runner
that already holds the workspace — and they can be asked for the same row at
the same moment. It is written in the same transaction that moves the row out
of `queued`, under the same revision check every other transition uses, so the
first claim wins and the second is refused with `already_running` rather than
starting a second process in one worktree. It also names the executor, which is
what an operator reading a run needs: a person's stream id, `relayhub:N` for
the hub's own dispatch, and `runner:<id>` for a run the runner started itself.
The partial index is what keeps the dispatch's own read an index scan over the
queued rows rather than a scan of every run the project has ever made.

The hub pins the schema it supports. `internal/hubserver/migrate.go` sets
`supportedSchemaVersion = 30`, verifies the embedded migration set reaches
exactly that version, applies any missing migrations when the database is
opened, checks foreign keys afterwards, and fails to start when the database is
already at a higher version. Operators do not run a migration command: starting
the hub is the migration step.

### Retention and size

Every event is retained in this milestone. There is no pruning job, no
retention window and no archival path; `conversation_events` only grows. The
stream code relies on that: a cursor is only rejected as `cursor_expired` when
it is ahead of the conversation head, never because history aged out.

Plan capacity with that in mind:

- Each accepted message writes a message row and a `message.accepted` event.
- Each streamed chunk of assistant text writes a `message.delta` event *and*
  appends the same text to the message row, so a streamed transcript costs
  roughly twice its text plus one row per chunk.
- Every conversation, execution, question and receipt change writes a further
  event row.

Settling a conversation changes its status and groups it under Settled. It is
not read-only, it stays in the default listing (active conversations sort
first), and it deletes nothing.

Message references add one row per resolved reference, bounded to 20 per
message, and re-extraction replaces a message's rows rather than adding to
them.

Migration `internal/hubserver/migrations/00033_workspace_terminal_recordings.sql`
adds one table for §18.3's recordings:

| Table | Holds |
|---|---|
| `workspace_terminal_recordings` | One row per terminal stream: the asciicast v2 document, its size and whether it hit the 1 MiB cap, the relay session and stream it belonged to, who ran it, and the isolation level the PTY actually ran at. That last one is stored rather than re-derived because it decides who may read the row for the life of the row: an organization that changes `workspaces.terminal.isolation` afterwards must not widen the audience of a recording already made. |

It is additive and reversible in the goose sense. The bytes live in the hub
rather than in the artifact service, on the precedent migration 00030 set for
an action run's output and 00026 set for a stored diff: a bounded,
audience-scoped text blob the hub keeps, where the cap is what makes keeping it
safe. What differs is the audience, and that is why it is a table of its own
rather than a column on `workspace_relay_sessions` — a run's output has exactly
the issue's read audience, and a terminal recording's is narrower.

## 4. Local startup

### Fresh checkout

```sh
make setup      # Go tooling, root npm deps, and npm ci in web/conversation
make generate   # go generate, templ, sqlc, Tailwind, and the client build
```

`make generate` runs `make app`, which runs `npm ci` when needed and then the
Vite build, emitting `static/app/conversation/{index.html,app.js,app.css}`.
That output is committed, following the precedent of `static/css/output.css`,
so `go build` never needs Node. Run `make app` on its own when only the client
changed.

Then start a hosted hub whose configuration file carries the section from
section 2:

```sh
DETENT_HUB_ADMIN_TOKEN=... WORKOS_API_KEY=... \
detent hub serve \
  --database ./tmp/hub.db \
  --listen 127.0.0.1:7777 \
  --hosted-config ./tmp/hosted.yaml
```

Migrations run as part of opening the database. Sign in through the hosted
login flow, then open `http://127.0.0.1:7777/chat`. Unauthenticated requests to
`/chat` redirect to `/login` like the other hosted pages. The client resolves
its own routes under that mount: `/chat` (new chat), `/chat/c/:conversation`,
`/chat/p/:project` and `/chat/issues/:workItem`. It reads `/app/bootstrap`
(`/chat/bootstrap` is the same payload under its old name) for the
organization, the actor, the readable projects and their write grants, the CSRF
token and the capability flags. Bundles are served from the embedded filesystem
under `/static/app/conversation/`.

### Client development loop

From the repository root:

```sh
make app-dev                     # vite dev server
```

or from `web/conversation`:

```sh
DETENT_HUB_URL=http://127.0.0.1:7777 npm run dev   # vite against a real hub
npm run dev:mock                                   # mock hub and vite together
```

The dev server proxies `/api`, `/app/bootstrap` and `/chat/bootstrap` to
`$DETENT_HUB_URL`, which
defaults to `http://127.0.0.1:4100`. `dev/mock-hub.ts` listens on
`$MOCK_HUB_PORT`, default `4100`, so `npm run dev:mock` needs no extra
configuration. The mock is an in-memory double with `POST /__mock/*` hooks for
failure injection (`queue-full`, `unknown-outcome`, `drop-open-streams`,
`expire-cursors`, `revoke-access`, `reset`) and a scripted runner. It is a
development and test double, not a reference implementation.

### Tests

```sh
go test ./internal/hubserver -run TestConversation
go test ./internal/conversation/...
go test ./internal/runner ./internal/codex ./internal/hubclient -run Conversation
make app-test    # tsc --noEmit && vitest run in web/conversation
make check       # the local pre-review gate
```

`make check` does not run `make app-test`; run the client tests separately when
the client changed.

### Browser cover for the client

```sh
make app                                             # the spec needs the built client
npx playwright install chromium                      # first run on a new machine
npx playwright test tests/visual/conversation.spec.js
```

`tests/visual/conversation.spec.js` is part of `make visual-e2e`. It does not
use a mock: `tests/visual/hosted-hub.js` runs the Go preview test
`TestHostedBrowserPreview` with `DETENT_HOSTED_BROWSER_PREVIEW=1`, which serves
a hosted hub with `conversation.enabled`, a fake WorkOS provider, a scripted
coordinator backend and one conversation already linked to an issue, and writes
a JSON fixture naming the hub URL, the `/chat` URL, the project id, a login URL
per account and the seeded conversation and work item. The spec signs in as the
owner and, for the read-only cover, as the `viewer` account, drives the client
from the keyboard only and locates every control by its accessible name, scans
five views with `@axe-core/playwright`, and stops the fixture through its
`stop` URL. `go` must be on the PATH; the hub itself binds an ephemeral port.
The whole file is seventeen tests and takes about 45 seconds; the preview hub
holds itself open for five minutes, so a run that outlives that budget fails at
the hub rather than in an assertion.

The `decisions.md` section 10 corrections are covered in the same file, against
the same hub: the idempotent create key (the client's own `POST /conversations`
is captured and replayed byte for byte), the audience preview's `message_count`,
the read-only viewer's missing controls, the sidebar's Settled group, the MIT
banner on the bundle the hub serves, and the hosted "Chat" navigation item. Two of them are not covered end
to end, because the preview fixture cannot reach the states they describe, and
each test says so where it is written:

- Section 10.3, the retry affordance. It appears only on a message whose
  delivery is `unknown`, `failed` or `rejected`. Those are written by the worker
  paths in `conversation_worker.go` and by a coordinator turn that returns an
  error, and the preview's scripted backend (`browserConversationBackend` in
  `internal/hubserver/hosted_browser_test.go`) has no failure route: its only
  candidate is the tool handler, and `coordinatorToolset.handle` turns every
  tool failure into a successful tool result. The browser test therefore checks
  the rule the affordance is bound to — the hub refuses `retry` on a settled
  delivery and duplicates nothing — and that no retry is offered on one.
- Section 10.4, the transcript-recovery notice. `execution.resume` is written
  when a runner binds an attempt, and the preview enrolls no runner. The browser
  test checks that the field is on the execution resource, that it is empty with
  nothing bound, and that the client does not announce a recovery that did not
  happen.

Closing either one needs failure injection and a runner in the preview fixture,
which is Go work outside the spec.

There is no opt-in live Codex conversation test in the tree. `decisions.md`
section 6 records the intent to keep one behind `DETENT_CONVERSATION_LIVE=1`,
but no test reads that variable today; the only occurrence of the name in the
repository is that sentence. Live provider behavior is therefore unproven by
automated tests — see section 7.

## 5. Restart and rollback

### Restart normalization

The conversation service normalizes durable state before it serves anything,
so no delivery or execution left behind by a previous process is reported as
live:

- Conversations whose execution was `starting`, `running`, `waiting_input` or
  `interrupting` become `unknown`, and their capabilities are cleared.
- Messages whose delivery was `sending`, `sent` or `responding` become
  `unknown`. Queued, saved and terminal deliveries are untouched.
- Questions that were `pending` become `expired`; questions that were `sending`
  become `unknown`.
- Stored command receipts whose status was `sending`, `sent` or `responding`
  become `unknown`.

Nothing is replayed and nothing in-flight is retried. `unknown` means exactly
that: the hub cannot say whether the provider saw the write.

The same rule applies outside a restart. When a worker unbinds or loses its
lease, every control it had been handed becomes `unknown` whatever its kind,
including plain text messages; only a control that was never handed out stays
`queued` for the next attempt. Recovering one is the user's decision, taken
with the `retry` command (`decisions.md` section 10.3):

```json
{"key": "cmd_...", "kind": "retry", "message_id": "msg_..."}
```

`retry` is accepted when the message is a user message in the caller's
conversation whose delivery is `unknown`, `failed` or `rejected`; anything
else is `invalid_request`. It puts that same message id back to `queued` (or
`saved` on the hub-side coordinator path), emits `message.updated` and returns
a receipt for the retry key naming the message. Nothing is duplicated, and the
next bind hands the message out exactly once. A client with no retry
affordance therefore leaves those messages stranded.

After normalization the service re-wakes accepted work. Every active
conversation still holding a user message with delivery `saved` or `queued` is
handed to the coordinator (unlinked) or to the control router (linked). Those
components then re-read durable state; the wake carries no payload.

### Disabling the feature

Set `enabled: false` or remove the `conversation:` section, then restart the
hub. The service is not constructed, so the conversation API and the worker
endpoints return 404 and `/app/bootstrap` reports `feature.conversation:
false`, which is how the client hides the chat screens. The application shell
itself still answers `/chat` and every other client route: since
`decisions.md` section 11 it serves the whole frontend, not only chat.

Stored conversations, messages, questions, receipts and events are preserved
untouched. Re-enabling the section brings the same history back.

A running worker is not stopped by disabling the feature. Its bind attempt
fails and is logged as `worker_conversation_bind_skipped`; the run continues
and completes its issue without live conversation control.

### Rollback constraints

Two constraints make a rollback past this release a restore, not a downgrade.

- **The down migration destroys conversation data.** It drops all seven tables,
  so every conversation, message, question, receipt, event and audience row
  goes with it. There is also no shipped command that runs goose down against
  the hub database: the embedded hub migrations are applied by the server
  itself, and `make db-migrate` targets `internal/store/migrations`, which is a
  different database.
- **An older binary refuses a migrated database.** `runMigrations` compares the
  database's applied version with the binary's `supportedSchemaVersion` and
  returns `ErrUnsupportedSchema` ("hub database schema is newer than this
  Detent version") when the database is ahead. A binary that predates schema
  version 22 will therefore not start against a database this release has
  migrated.

The supported procedure is to take `detent hub backup --database ... --output
...` on the stopped hub before the upgrade and, if a rollback is needed,
restore that snapshot with the older binary. Verify a snapshot with
`detent hub verify` before relying on it.

## 6. Troubleshooting

| Symptom | Cause | Action |
|---|---|---|
| `GET /chat` returns 503 `client_unavailable` | `app/conversation/index.html` is missing from the embedded static tree, so the binary was built without the client. | Run `make app` (or `make generate`) and rebuild. The built output is committed, so this normally only follows deleting `static/app/conversation/` or building from a tree where it was never generated. |
| Chat message receipt is `queued` and the execution stays `waiting_for_runner` | No hub-side coordinator is configured, so the message opened a coordinator work item and no enrolled runner with a fresh live-control (`codex`) provider report has claimed it yet. | Check `capabilities.coordinator` in the bootstrap payload and the runners' provider reports (`GET .../runners`). A runner whose codex observation is older than `providercapacity.MaxAge`, whose heartbeat is stale, or that holds no grant on the project will never receive the item. |
| A coordinator item is never claimed | The claim loop skips coordinator items for every credential that is not an enrolled runner with a fresh live-control report; legacy machine registrations never receive them. | Enrol the runner, confirm its heartbeat and provider report are fresh, and confirm its grant covers the project. List the items with `include=coordinator`. |
| Command rejected with 503 `queue_full` | More than `control_queue_size` messages (default 64) are queued for one conversation, meaning nothing is draining them. | Check whether an attempt is actually bound: the conversation's execution status and `runner_id` say so. Each refusal logs `conversation.queue_full` with the queue depth and the bound. The client should back off and retry. |
| Command rejected with 409 `stale_execution` | The `expected.attempt_id` or `expected.turn_id` on the control is no longer the current owner generation, or the targeted question is no longer open. The hub never silently redirects a control to a replacement attempt. | Re-read the snapshot and reissue against the current generation. Each rejection logs `conversation.stale_execution` with both attempt identifiers and the path that refused it. |
| Stream ends with `event: closed` and `cursor_expired` | The `after=` cursor is ahead of the conversation's event head. Because every event is retained, this only happens with a cursor from a different or restored database. | Re-snapshot with `GET /conversations/:conversation` and reopen the stream at the returned cursor. |
| Stream ends with `access_revoked` | Membership or project grant was lost, re-checked on a 30 second timer and on every emitted event. Settling never closes a stream: a settled conversation still streams. | Expected; the client re-authenticates or reopens read-only. |
| Stream ends with `server_error` | Re-authorization could not be completed because the hub failed, not because access changed. Each occurrence logs `conversation.stream_reauthorize_failed` with the error. | Transient: the client may reconnect at its cursor. Unlike `access_revoked`, this is not a decision about the actor. |
| Control rejected with 422 `invalid_request` naming `expected.attempt_id` | A `continue` carried a null attempt for a conversation that has or had one. Null is only honest before the first attempt. | Re-read the snapshot and send `expected.attempt_id` for the attempt being continued. |
| `retry` rejected with 422 `invalid_request` | The message is not the caller's, is not a user message, or its delivery is not `unknown`, `failed` or `rejected`. | Only a message the hub could not establish is retryable; a `delivered` or `queued` one is already on its way. |
| A chat moved to Settled on its own | The settle sweep found its execution finished or idle and no activity for `conversation.settle_window`. Each settle logs `conversation.settled` with the last activity, and emits `conversation.updated`. | Expected. Sending anything unsettles it; raise `settle_window` if chats settle sooner than the team works. |
| A preference is refused with 422 `invalid_request` | The model is not in the bootstrap's `preferences.models`, or the effort or access is outside its vocabulary. The choices come from the enrolled runners' provider reports, so a hub whose runners have reported nothing accepts only `auto`. | Check `preferences.models` in the bootstrap and the runners' provider reports. |
| A `#123` in a message did not become a link | The number names no issue in the conversation's project, or the qualified project or organization does not match. Only resolvable targets are stored; the twenty-first reference in one message is dropped. | Expected. Reference the issue by its project (`project#123`) when it lives elsewhere in the same organization. |
| Question shows `expired` | Either the hub restarted while it was pending, or `question_timeout` elapsed. | Answers to an expired question are refused. Ask again in a new turn; raise `question_timeout` if humans routinely need longer. |
| Answer rejected with 409 `question_already_answered` | A question is single use and has exactly one winner. | Reload; the winning answer is in the transcript. |
| Control rejected with 422 `unsupported_control` | `continue` was sent to an unlinked conversation, `cancel` to a linked one, or an unknown kind was sent. | Use `interrupt` on a linked attempt and `cancel` on a coordinator turn. |

### Workspace sessions

A workspace is the object the Files panel attaches to, so every question about
"why is Files still spinning" is a question about which state it is stuck in
and why. The states are `requested → starting → ready ↔ idle → closing →
closed`, with `unreachable` beside `ready`/`idle` and `failed` reachable from
everything. The panel learns each move from the project event stream, so if the
panel is not moving, look at the stream first and the database second.

**Reading the current state.**

```sh
# Every open workspace on the hub, with what is holding it back.
sqlite3 ./tmp/hub.db \
  "SELECT id, state, reason, runner_id, requires_json, last_heartbeat_at
     FROM workspace_sessions
    WHERE state NOT IN ('closed','failed')
    ORDER BY created_at;"

# What the project stream has published, newest last.
sqlite3 ./tmp/hub.db \
  "SELECT seq, type, subject_id, created_at FROM project_events
    WHERE project_id = 'prj_...' ORDER BY seq DESC LIMIT 20;"
```

| Symptom | Cause | Action |
|---|---|---|
| Workspace stays `requested`, then fails with `no_runner` | No runner is eligible. Eligibility is stricter than for ordinary work: every capability in `requires`, reported with a fresh heartbeat, plus an active project grant, plus — when the workspace names an attempt inside `retain_after_run` — the one runner that produced that worktree. | `SELECT id, workspace_capabilities_json, workspace_isolation, last_heartbeat_at FROM runner_identities;`. A runner reporting `{}` has never sent a workspace report; one whose heartbeat is older than the freshness window serves nothing. Remember this slice's runners report `files`, `exec`, `git` and `terminal` and nothing else, so a workspace asking for `diff` or `preview` will never be claimed. |
| A workspace item was run as an ordinary issue | A `detent:workspace` issue reached the issue lane. It cannot any more: the hub's claim candidate query excludes workspace items from every claim that does not declare the `workspace_sessions` capability, which only `hubclient.WorkspaceClaimer` sends. | If you see this on an older hub, the item is almost certainly one whose workspace has already closed. Move the issue to a terminal state by hand; nothing else is waiting on it. |
| Workspace reaches `starting` and stops | The runner bound but its worktree never became usable. It fails with `checkout_failed` or `worktree_missing`. | The runner logs `workspace.bound` then the failure. A `worktree_missing` means something removed the directory under a bound runner. |
| Workspace goes `unreachable` and comes back | Two heartbeats (30 seconds each) were missed and then one arrived. Not terminal. | Expected on a runner restart or a network blip. It closes on its own after `idle_timeout_seconds` if the runner never returns. |
| Workspace fails with `lease_lost` | The workspace lease expired: 90 seconds without a renewal. Occupancy is ended at the expiry rather than at the moment the hub noticed, so a crashed runner does not accrue time. | A failed workspace is never resumed; the person opens a new one. Look for why the runner stopped heartbeating. |
| Every workspace re-requests after a restart | Expected. Hub restart re-marks every open workspace `requested` with `hub_restarted` and gives the original runner 30 seconds to re-bind with its existing tuple. | A runner that re-binds in time returns the workspace to `ready` with no relay sessions carried over. One that does not is failed, and a late bind with the old tuple is refused `stale_execution`. |
| `422 workspace_limit` | `plan.workspaces.max_open` (20) or the per-person cap (3). `details.scope` says which. | Close a workspace, or raise `plan.max_open`. Closed and failed rows do not count. |
| `409 workspace_exists` | One workspace per worktree at a time. | Use the id in `details.workspace_id`. |
| Files panel shows a runner is connected but nothing loads | The workspace is `ready` but the runner's relay socket is not attached, so requests are answered `stale_execution` with "No runner is serving this workspace". | The runner redials its relay on a two-second delay; check the runner for `workspace.relay_dial_failed`. |
| A stream dies with `overflow` | The client stopped acknowledging and its unacknowledged buffer passed 1 MB. | A client bug. The hub will not grow without bound for one reader. |
| A new stream is refused `relay_busy` | The hub process is at `workspaces.relay.memory`. | Raise it, or find the workspace whose streams are not being acknowledged. |
| A connection closes with `revoked` | Authority changed: the session was revoked, the membership deactivated, the role or grant changed, or the periodic 30-second re-check failed. | Expected. The client re-authenticates. |
| A runner connection closes with `superseded` | A second runner connection opened for the same workspace and replaced the first. | Expected during a runner reconnect. Two runners answering one workspace would each be serving a different worktree. |
| A file read answers `denied` | The path matches the fixed denylist (`.git/objects`, `.git/config`, `node_modules`, `.env*`, `*.pem`, `*.key`, `*.p12`, `id_rsa*`) or a `workspaces.files.deny` glob. | Expected and deliberate. The entry is still listed, so the reader learns the file exists and not what is in it. |
| A file read answers `forbidden` for something that plainly exists | It is a symbolic link, a device, a socket, a FIFO, or it sits on a mount point under the worktree. | Expected. Reading a link at all is what a swapped link would exploit, so the rule is the kind, not the target. |
| `watch` answers `unsupported` | No runner implements filesystem notification in this slice. | Expected. The client refreshes on focus. |
| An action run stays `queued` | The row was written but no `run` frame ever reached a runner: the client never opened its exec stream, or the workspace it named stopped being bound first. | `SELECT id, status, reason, workspace_id FROM project_action_runs ORDER BY created_at DESC LIMIT 20;` and check the workspace in the same breath. A queued run is not holding anything; the person runs the action again. |
| An action run ends `failed` with no `exit_code` | The process never exited. `reason` says which teardown got there first: `lease_lost`, `stream_closed`, `killed` or `workspace_closed`. | Expected when a reader closes the tab or a runner loses its lease mid-run; the runner kills the process group, so nothing is left behind. If it is `lease_lost`, look at why the runner stopped heartbeating, as for the workspace row above. |
| An action run sits `running` after a runner crash or a hub restart | It is a run the runner started itself for `run_on_worktree_creation`, so it has no relay stream and none of the stream teardowns reach it. Nothing is executing; only the row is stale. | The workspace sweep fails it once its workspace reaches a terminal state, so it clears on its own within a tick of the workspace closing. It carries the workspace's own reason — `lease_lost` where the hub gave up the worktree's lease, `workspace_closed` otherwise — because the relay's teardown reads that same reason and the two paths must not disagree about why one run died. It is keyed on the workspace's state and never on the run's age, so a slow run on a healthy workspace is never reaped. If the row persists while its workspace is still open, the run really may still be going — check the runner before touching the row. |
| An exec frame answers `forbidden` | The workspace does not report `capabilities.exec`, or it is read-only because its subject attempt is still running. | A command must not write into a worktree a model is editing (§18.12). Wait for the attempt, or open a workspace whose runner serves `exec`. A client that reused a files-only workspace for an action is the other cause, and the client is supposed to refuse that reuse itself. |
| The Terminal card is disabled and says why | The client states §18.3's gate itself, because the hub answers all four refusals with one `forbidden`. Most structural first: no linked issue, a viewer, a grant without `write`, a grant without the `runners` flag, an attempt still running, a runner that reported no terminal. | The sentence names which. A grant is `SELECT can_write, manage_runner FROM hosted_project_grants WHERE user_id = ? AND project_id = ?;`; a runner's report is `workspace_capabilities_json` in `runner_identities`. |
| A terminal `open` answers `forbidden` | One of: `workspaces.terminal.enabled` is off (it defaults off, and an owner turns it on); the grant lacks `write` or the `runners` flag, or the role is viewer; the organization runs `workspaces.terminal.isolation: user` and the person is not an owner or admin; or a support session asked for one at a level other than `container`. | All four are §18.3's own gate, asked per frame rather than cached from the upgrade. The hub logs `workspace.relay_terminal_grant_unreadable` only when the grant could not be read at all. |
| A terminal `open` answers `read_only` | The workspace is on an attempt that is still running (§18.1), so a person cannot type into a worktree the model is editing. | Expected. Wait for the attempt, or open a workspace on the issue rather than on the running attempt. |
| A terminal `open` answers `unsupported` | The runner serves no terminal: the platform has no pseudo-terminal (Windows), the operator's `Support` turns it off, or the worktree could not be opened. | `SELECT workspace_capabilities_json, workspace_isolation FROM runner_identities;`. A runner that reports no `terminal` is never claimed for one, so this is normally reached only by a workspace that reported it and then lost it. |
| A shell disappears about a minute after a tab is closed | Expected: §18.2 parks a stream for 60 seconds and the hub's sweep then sends the runner a `close`, which is what kills the PTY. The runner times its own half the same way for its own socket dropping. | Reconnect inside the window and the client sends `resume` for the same stream and finds the same shell, with the output it missed replayed from the hub's buffer. |
| A terminal ends with `Killed by SIGHUP` | A close, a lost lease or a workspace ending. The runner sends SIGHUP to the shell's process group, the way a real terminal does when its window closes, and SIGKILL five seconds later. | Expected. The group rather than the shell alone, so a build or a server the person started does not outlive the reason it was started. |
| A recording is missing or shorter than the session | `workspaces.terminal.record` is off, or the recording reached its 1 MiB cap and is marked `truncated`. | `SELECT id, cast_bytes, truncated, isolation FROM workspace_terminal_recordings WHERE workspace_id = ?;`. The cap is on the stored copy only: the terminal itself has none, so nothing was cut on the wire. |
| A recording 404s for somebody who can read the issue | Deliberate. §18.3's audience is narrower than the issue's, because a recording can carry what the runner account can see. Not found rather than forbidden, because saying a recording exists is itself a disclosure about who was in the worktree. | Read it as the person who ran it, or as an owner. An admin cannot read a `user`-isolation recording; only an owner can. |
| Output stops at exactly 1 MiB with a marker | The exec channel's cap. The runner keeps draining the pipe past it, so the command still finishes and its exit code still means what it says. | Expected. Narrow the command's output, or read the whole thing from the runner's own logs. |

**Closing a stuck workspace by hand.** `DELETE
{nativeBase}/workspaces/:workspace` is the supported path and takes it through
`closing` to `closed` with `closed_by_actor`, ending its occupancy row and
closing its dispatch item. Editing `workspace_sessions` directly leaves the
dispatch issue open and the runner still holding a lease, so prefer the
endpoint; if the hub itself is down, the workspace expires on its own at
`expires_at` or on the next restart's re-request grace.

### Diagnostics

The hub tags conversation work with `component=conversation` and coordinator
turns with `component=conversation.coordinator`. Workspace work is tagged
`component=workspace`, the relay's own failures ride the same logger, and the
runner tags its half `component=workspace_session` and `component=workspace_lane`.

| Logger | Event | Level | Fields |
|---|---|---|---|
| `workspace` | `workspace.requested` | info | `workspace_id`, `work_item_id`, `attempt_id`, `requires`, `read_only` |
| `workspace` | `workspace.transitioned` | info | `workspace_id`, `from`, `to`, `reason`, `runner_id` |
| `workspace` | `workspace.restart_recovered` | info | `workspaces`, `grace_seconds` |
| `workspace` | `workspace.relay_revoked` | info | `workspace_id`, `connection_id`, `reason` |
| `workspace` | `workspace.sweep_failed` | warn | `error` |
| `workspace` | `workspace.activity_not_recorded` | warn | `workspace_id`, `error` |
| `workspace` | `workspace.relay_audit_not_closed` | warn | `connection_id`, `error` |
| `workspace_session` | `workspace.bound` | info | `workspace_id`, `worktree`, `read_only`, `requires` |
| `workspace_session` | `workspace.heartbeat_failed` | warn | `workspace_id`, `error` |
| `workspace_session` | `workspace.relay_dial_failed` | warn | `workspace_id`, `error` |
| `workspace_session` | `workspace.worktree_not_released` | warn | `workspace_id`, `path`, `error` |
| `workspace_lane` | `workspace.claimed` | info | `workspace_id`, `work_item_id` |
| `workspace_lane` | `workspace.session_failed` | warn | `workspace_id`, `error` |
| `workspace_lane` | `workspace.lease_not_released` | warn | `lease_id`, `reason`, `error` |
 This is the complete set of
conversation log lines the hub emits today, with the fields each one carries.

| Logger | Event | Level | Fields |
|---|---|---|---|
| `conversation` | `conversation.receipt` | info | `conversation_id`, `command_key`, `kind`, `status`, `attempt_id`, and `error_code` when the receipt carries one |
| `conversation` | `conversation.queue_full` | warn | `conversation_id`, `kind`, `queued`, `limit` |
| `conversation` | `conversation.stale_execution` | warn | `conversation_id`, `expected_attempt_id`, `current_attempt_id`, `path` |
| `conversation` | `conversation.restart_normalized` | info | `conversations`, `messages`, `questions`, `receipts` |
| `conversation` | `conversation.wake_pending` | info | `coordinator`, `controls` |
| `conversation` | `conversation execution reconcile failed` | warn | `error` |
| `conversation` | `conversation lease release lookup failed` | warn | `lease`, `error` |
| `conversation` | `conversation lease release settlement failed` | warn | `conversation`, `error` |
| `conversation.coordinator` | `coordinator pass failed` | error | `conversation_id`, `error` |
| `conversation.coordinator` | `coordinator turn failed` | warn | `conversation_id`, `error` |
| `conversation.coordinator` | `coordinator stop timed out waiting for turns` | warn | none |
| `conversation.coordinator` | `coordinator could not persist provider thread` | warn | `conversation_id`, `error` |
| `conversation.coordinator` | `coordinator could not persist delta` | warn | `conversation_id`, `error` |
| `conversation.coordinator` | `coordinator could not persist tool message` | warn | `conversation_id`, `error` |
| `conversation.coordinator` | `coordinator tool failed` | warn | `conversation_id`, `tool`, `error` |
| hub base logger | `conversation client shell could not be read` | error | `error` |

On the runner side the relevant worker events are `worker_conversation_bound`
(work attempt id, `conversation_id`, `thread_id`) and
`worker_conversation_bind_skipped` (work attempt id, `error`).

What the five conversation-product events mean:

- **`conversation.receipt`** is written once per stored receipt, so a command
  key can be followed across `saved`, `queued`, `sending`, `delivered`,
  `failed` and `unknown` from the log alone. `attempt_id` is the execution
  owner the receipt belonged to and is empty when no attempt owns the
  conversation. `error_code` is present only when the receipt carries an
  error; the error message is not logged. The line is emitted inside the
  command's transaction, so a rolled back command can leave a receipt line
  behind for a receipt that was never committed.
- **`conversation.queue_full`** is the `503` the command endpoint returns when
  more than `control_queue_size` controls are already queued. `queued` and
  `limit` say how close to the bound the conversation was.
- **`conversation.stale_execution`** is written once per rejected request, at
  the boundary that answers it, not once per ownership check. `path` is the
  API command kind (`message`, `answer`, `interrupt`, `continue`, `cancel`) or
  the worker endpoint (`bind`, `unbind`, `controls`, `turn_events`). Both
  attempt identifiers are empty when the rejection was not a generation
  comparison — a control aimed at a question that is no longer open, for
  instance.
- **`conversation.restart_normalized`** reports what section 5's normalization
  rewrote in this startup: the number of conversations, messages, questions
  and receipts changed. All four are zero when a restart found nothing
  in flight.
- **`conversation.wake_pending`** reports how many conversations were re-woken
  after that normalization, split by the component that owns the next step:
  `coordinator` for unlinked conversations and `controls` for linked ones.

**No conversation log line carries message text, an assistant answer or an
answer to a runner question.** Every value on the lines above is an identifier,
a status, a count, an error code or an error string, and coordinator error
strings are truncated by `boundRunes`. That is verified by reading every
logging call site in `internal/hubserver/conversation_*.go`, and regression
tested by `internal/hubserver/conversation_logging_test.go`, which drives the
hub with a JSON logger and fails if a distinctive message body reaches any
record.

## 7. Known limitations and open evidence

The remaining plan tasks in the operations track are O01 to O03; the plan file
itself (`detent-cloud-concept/IMPLEMENTATION-PLAN.md`) lives outside this
repository. What is still missing, as a checklist:

- [x] **O01, end-to-end proof.** Recorded September 11, 2026:
      `DETENT_CONVERSATION_LIVE=1 go test ./internal/hubclient -run Live -v`
      against Codex CLI 0.154.0 with the operator's own ChatGPT login passed in
      37 seconds. The run creates a chat, links it to an issue with shared
      history, delivers the queued brief to a real attempt on an enrolled
      runner, receives the model's `request_user_input` question through live
      control, answers it, sees the reply name the chosen colour, completes the
      attempt, continues on a second attempt that resumes the same Codex
      thread (`resume.kind = thread`, same runner identity) and recalls the
      codeword from the first turn. Two things it found, both fixed in the
      same commit: Codex 0.154 lists `request_user_input` in its default
      collaboration mode only behind the `default_mode_request_user_input`
      feature, which the runner now enables on every `codex app-server`
      launch (`withCodexQuestionFeature` in `internal/cli/runner.go`); and a
      worker credential with no runner identity is never handed another
      runner's thread, so the live test binds through an enrolled runner as a
      production runner does. Not yet exercised live: an interrupt mid-turn,
      and a Claude Code runner.
- [ ] **O02, failure injection.** The mock hub exposes queue-full,
      unknown-outcome, dropped streams, expired cursors and revoked access, and
      the Go tests cover restart normalization, lease loss, concurrent
      acceptance and stale generations. None of that has been exercised against
      a live deployment with a real provider and a real lease.
- [x] **O03, accessibility and performance.** Both are now measured, and both
      are regression-tested.
      - Covered: `tests/visual/conversation.spec.js` drives a real hosted hub in
        Chromium from the keyboard only, locating every control by its
        accessible name, across the new-chat route, an active conversation with
        a streamed reply, the sidebar and its search, the linked-issue
        conversation, the handoff form's share-history gate, a reload that
        restores history and the draft, and a 390px viewport with the sidebar
        drawer. Each of those views is scanned with `@axe-core/playwright` and
        the spec fails on any serious or critical violation; there are none
        today. Any console error also fails the spec.
      - Covered: `web/conversation/tests/perf.test.ts` budgets the two hot
        paths — about 7ms to decode and merge a 2,000-message history, under
        1ms to assemble a 500-delta burst, against 500ms and 200ms budgets —
        and asserts that navigating between two conversations three times
        leaves exactly one open detail subscription.
      - Fixed while measuring: the conversation list atom was rebuilt on every
        render of the shell, so a mounted client re-read
        `GET /organizations/:org/conversations` in an unbounded loop (thousands
        of requests within seconds, until the browser refused more sockets).
        It is now memoised per environment like the detail family.
      - Fixed while measuring: the tertiary text token failed WCAG AA at the
        12px it is used at (3.51:1 on the card surface). Both muted foreground
        tokens were raised, which is a deliberate deviation from the exact
        values recorded in `design-inventory.md` B.1.
      - Still open, and deliberately not fixed here because each is a design
        change rather than a correction:
        - The client remains dark only, a recorded product limitation rather
          than an accessibility result.
        - Nothing has been checked against a real screen reader or a real
          touch device; the evidence here is axe plus keyboard emulation.
      - Closed since: `page-has-heading-one` is gated to zero along with the
        serious and critical rules, so a route that loses its `h1` fails the
        spec. The narrow-width drawer implements the focus contract in
        `design-inventory.md` B.14 — opening it moves focus into it, `Escape`
        closes it and returns focus, and a closed drawer carries `inert` so
        Tab never walks an invisible list. `/` focuses search and
        `Mod+Shift+N` starts a new chat; both are advertised through
        `aria-keyshortcuts` and covered by the spec.
- [x] **Live provider test.** `internal/hubclient/conversation_integration_live_test.go`
      is the opt-in test `decisions.md` section 6 promised. It skips unless
      `DETENT_CONVERSATION_LIVE=1` and a logged-in `codex` CLI are present, so
      it never runs in CI; no recorded run against a real provider exists yet,
      which is what O01 below still tracks.
- [ ] **Retention.** All events are retained with no pruning path, so long-lived
      conversations grow without bound. Sizing guidance is in section 3; a
      retention policy is future work.
- [x] **Runner-dispatched coordinator.** A hub with no `conversation.codex`
      backend — the default — dispatches every coordinator turn to a customer
      runner through a coordinator work item (`decisions.md` section 9), and
      `capabilities.coordinator` is true when either a backend is configured or
      a capable runner is enrolled. The hub-side backend remains configurable
      and is transitional; deleting the `codex` and `workspace` config keys is
      what is left.
- [x] **An item in an active state is re-dispatched forever.** Fixed
      September 12, 2026. The September 11 dogfood run claimed one issue four
      times, and the sixth run claimed one four times again, before each was
      moved to Done by hand.

      The diagnosis held in both halves. The hub's claim predicate was a
      function of the item's *lane* alone: in `claimCandidateIDs`
      (`internal/hubserver/api_worker.go`) the eligibility terms were
      `ws.terminal = 0 AND lower(trim(ws.detent_state)) <> 'cancelled' AND
      ws.dispatchable = 1`, with no join to `native_attempts` and no reference
      to `issues.revision`, and the per-candidate loop only skipped an open
      coordinator item, an unexpired lease and a capacity refusal. Nothing on
      the hub side wrote item state when an attempt finished either. The item
      moved lane only because the *orchestrator* moved it, in
      `transitionCompletedActiveIssuesToReview`
      (`internal/orchestrator/completion_transition.go`), whose target was
      `completedActiveReviewTargetState` — always `cfg.SourceState`, defaulting
      to the literal `"Human Review"`. A three-state project (Todo /
      In Progress / Done) has no such lane, so the transition either returned
      no target or failed `updateIssueStateByID`, and either way the item
      stayed In Progress: non-terminal, dispatchable, claimable. With hub
      scheduling the local brake could not help, because `fetchNativeCandidate`
      claims first and the orchestrator releases afterwards with
      `dispatch_deferred`, so every tick that saw the item produced a real hub
      claim and a real Codex turn.

      Three brakes now hold, and the fix needed all three: the completed item
      has to be judged ready to leave its lane, the promotion has to land
      somewhere, and the hub has to stop offering an item that was already
      answered. The first of those is the half the seventh dogfood run found
      still broken (section 8, September 12, 2026 13:19 UTC) and it is brake 0
      below; brakes 1 and 2 landed on September 12 with the sixth run's fix.

      0. **A completed item is judged on facts its tracker can actually
         report.** The readiness rule was a pull request and nothing else:
         `completedActiveReviewTargetState` asks
         `completedActiveIssueReadyForReview(issue,
         gateRequiresPullRequest(cfg.Gate), …)`,
         `gateRequiresPullRequest` is true for every gate kind except
         `artifact` (`internal/orchestrator/autopromote.go`), and that
         predicate answered `false` whenever `issue.PullRequest == nil`. A
         hub-native item never has one — `issueFromNative`
         (`internal/hubclient/native_scheduler.go`, `native_connector.go`) set
         no `PullRequest` field at all — so every completed item on a native
         project was judged not ready, the target was `""`, the loop
         `continue`d in silence, and brake 1's fallback was never even asked
         for. That is the seventh run's "the promotion half **did not fire at
         all**".

         A native item is now judged on the hub's own facts, and the fact that
         decides is **stated rather than inferred from a missing pull
         request**: "this item has no pull request yet" and "no pull request
         can ever mirror this item's change" are different situations and a
         promotion decides them differently. The work item resource carries an
         optional change review surface,
         `GET {nativeBase}/work-items/:item?include=change`
         (`internal/hubserver/native_issue_change.go`), which reports
         `connector: "github" | "none"` from the project's own GitHub
         repository binding — the same binding `GET
         .../work-items/:id/pull-requests` reports as `connector: null` for
         decisions section 18.6's "projects without a GitHub connector show
         the change request alone" — the item's latest change request as a
         pull request (`change_id`, `number`, `state`, `draft`, `url`,
         `head_sha`), joined with the connector's projection whenever one
         mirrors it, and `revision`: the work item revision the recorded
         change covers, which is the `native_attempts.work_item_revision` of
         the latest succeeded attempt that posted a stored diff (section 18.5)
         or published a change version. That is the same number brake 2
         compares, so the two brakes agree on what "the item as it stands"
         means. The surface is absent unless the caller asks for it, so the
         default resource is byte-for-byte what it was and only a promotion
         pays for the three extra reads.

         `issueFromNative` maps it onto `connector.Issue.PullRequest` — the
         change request *is* the native item's pull request — and onto the new
         `connector.Issue.ChangeReview`, which is what the rule reads. The
         readiness rule is then: **a change recorded for the item at its
         current revision is what "ready for review" means, whether or not the
         project has a connector**, and a pull request that was opened for the
         item must be open exactly as before. A connector that reports no
         change review surface at all — every non-hub tracker, and the
         `issueFromWorkItem` GitHub-profile path, where the runner opens the
         pull request itself, both unchanged — leaves the pull request as the
         only authority.

         That last sentence of the rule is the eighth run's amendment
         (section 8, September 12, 2026 16:52 UTC). It first read "with a
         connector, the mirrored pull request must be open", and the eighth
         run proved that is the wrong question. The browser preview fixture
         binds a GitHub repository to the dogfood project, so
         `?include=change` answers `connector: "github"`, and nothing in a
         conversation-driven attempt opens a pull request: opening one is the
         explicit `POST {nativeBase}/work-items/:id/pull-requests/actions
         {action: open}` of decisions section 18.6, executed by the merge lane
         and offered to a person as the header's Create PR button, and the
         model holds no hub credential for it. The rule demanded a pull
         request that would never exist and the item stayed In Progress
         forever. A hosted project may also hold a connector and still run its
         work on a local checkout with no remote; "this project could mirror a
         change" is not "this item has an answer". There is no auto-PR setting
         in the gate configuration (`internal/gate`) to re-arm the requirement
         with; if one is ever added, a native project that sets it is the one
         case that still demands the pull request.

         The surface is read at promotion time through the new optional
         `connector.ChangeReviewHydrator`, implemented by
         `hubclient.NativeConnector.HydrateChangeReview`, because the issue
         the orchestrator holds when it promotes was read when the attempt was
         *dispatched*: the change the attempt produced is newer than it. Only
         the change facts are carried over; the item's lane, title and labels
         stay as the caller read them.

         And the silent `continue` is gone. A completed item that is not ready
         says so once per item at warn — `completed issue not ready for
         promotion` with `missing=pull_request`, `open_pull_request` or
         `change_request`, plus the two revisions the comparison was made on.
         Seven dogfood runs went by with no log line at all for this, which is
         why the run had to read `completedActiveReviewTargetState` to find
         it. Since the eighth run's amendment the three values divide as
         decisions section 9.2.2 states them: `change_request` is a tracker
         that holds no change for the item as it stands, `open_pull_request` a
         pull request the item *has* that merged or closed, and
         `pull_request` a tracker that reviews no change of its own — the
         GitHub-profile path and non-hub trackers. A native item is never held
         for `pull_request` again, which is the whole of the eighth run's
         finding.

      1. **Completion promotes into a lane the project has.** The promotion
         target is resolved against the project's own workflow before it is
         used, by `orchestrator.CompletedReviewTarget`
         (`internal/orchestrator/completion_promotion_target.go`): the
         configured review state when the project has it, otherwise the
         project's own review lane — non-terminal, not dispatchable, not
         operator-only, and named as a review lane, which is what the
         configured state means — otherwise the project's first terminal lane,
         which is the rule `closeCoordinatorItem` and the workspace close
         already apply. A three-state project therefore promotes to `Done`.
         The substitution is logged once per item
         (`completed issue promotion lane substituted`), and a project that
         offers nowhere to promote into says so once at warn and the item is
         left where it is, which is what happened before.

         The orchestrator could not see a lane's meaning at all: `Connector`
         had no way to report a workflow, and both `issueFromNative` and
         `issueFromWorkItem` flatten a hub workflow state to its name. The
         seam is the new optional `connector.WorkflowStateLister`, implemented
         by `hubclient.NativeConnector.ListWorkflowStates` over
         `NativeClient.Project`, which the hub already answers with `terminal`,
         `dispatchable` and `operator_only` per state. It is read once per
         tick, and only for a tick that has something to promote. A connector
         that cannot answer — every non-hub tracker — keeps the configured
         name as the only authority, so nothing else changes behavior.
         Auto-promote's own vocabulary (`PassState`, `ReworkState`) is reached
         only when the configured review lane exists, because a project that
         lacks the review lane does not have the rest of it either; a
         substituted target is applied as a plain transition.

      2. **The hub does not re-offer an item that was already answered.**
         `claimCandidateIDs` now carries `notAlreadyAnsweredClause`: an item is
         not a candidate when its *latest* attempt — highest fencing token, the
         order the hub already pages attempts in — has `status = 'succeeded'`
         at `work_item_revision >= issues.revision` and
         `dispatch_generation >= issues.dispatch_generation`. Migration
         `00029_dispatch_requests.sql` adds
         `native_attempts.work_item_revision` and
         `native_attempts.dispatch_generation`, written at `run.started` from
         the item's own row inside the same transaction, and
         `issues.dispatch_generation` / `issues.dispatch_requested_at`;
         `supportedSchemaVersion` is 29. Both attempt columns default to 0,
         which is below every real revision, so an attempt recorded before the
         migration suppresses nothing.

         The claim-side guard that was built and reverted failed because it
         *inferred* the continuation. A conversation `continue` bumps no
         revision, moves no lane and edits no content, so a revision-only
         guard refused the second attempt
         `TestConversationIntegration/vertical_slice` requires, with
         `candidates = []connector.Issue{}`. The request is written down
         instead of inferred: `requestNativeDispatch`
         (`internal/hubserver/native_dispatch_requests.go`) bumps
         `issues.dispatch_generation` wherever `acceptCommand` accepts a
         command on a **linked** conversation that only a new attempt can
         carry — a `continue`, and a `message` or a `retry` accepted while no
         attempt is live. A steer on a live attempt bumps nothing, because the
         controls poll delivers it; bumping there would have produced exactly
         one extra attempt after every steered turn.

         Every other re-dispatch reason survives untouched, which is what the
         reverted guard could not claim. An attempt that ended `failed`,
         `cancelled` or `interrupted` is retried exactly as before, because
         only `succeeded` suppresses anything. Everything that moves the item
         on — an edit, a comment, a gate rework, a plan-to-implement handover,
         and a person moving the item back into a dispatchable lane — runs
         through `persistNativeIssue`, which bumps `issues.revision`, so the
         succeeded attempt no longer matches the item as it stands. The
         exclusion lives in the candidate query, so the provider candidate
         preview that shares it does not offer one either.

         The exclusion applies to **native** projects only, which is a
         correctness requirement rather than a scoping convenience. On a
         `github_compatible` project the tracker is GitHub and the `issues` row
         is a projection of it: `applyGitHubWebhookIssue` writes title, labels,
         `github_state` and `workflow_state_id` without touching `revision`,
         because `revision` fences *native* edits. An item whose lane moved on
         GitHub would keep the revision its attempt recorded, and the clause
         would strand it forever — the opposite failure, and a worse one. The
         loop the clause exists to stop is a hub-scheduling loop on a native
         project, so that is where it lives.

      Covered: `TestClaimCandidatesRequireUnansweredWorkItem` and
      `TestClaimCandidatesKeepOfferingUnsuccessfulAttempts` in
      `internal/hubserver` walk one item through succeeded → not offered →
      continuation → offered → succeeded → edit → offered → succeeded →
      manual move → offered, and prove a failed, cancelled or interrupted
      attempt is still retried; `TestRecordNativeAttemptCapturesDispatchState`
      pins the two numbers to `run.started`.
      `TestConversationDispatchLoopSettles` in `internal/hubclient` is the
      dogfood loop against a real hub, a real database and the production
      runner turn path: a chat linked to an issue on a three-state project,
      one succeeded attempt whose worktree is a real repository so the
      production diff source posts a real stored diff, a second claim poll
      that returns nothing, readiness that is `(false, "pull_request")` on the
      issue as dispatched and ready once the change review surface is read
      through `HydrateChangeReview`, and a promotion that lands in `Done` with
      exactly one succeeded attempt on the item.
      `TestConversationIntegration/vertical_slice` is unchanged and still gets
      its second attempt. On the orchestrator side
      `TestCompletedActiveReviewFallbackState`,
      `TestResolveCompletedActiveReviewTarget` and
      `TestTransitionCompletedActiveIssuesPromotesIntoTerminalLaneWithoutReview`
      cover the lane table, including that a connector with no workflow to
      report keeps the configured lane;
      `TestCompletedActiveIssueReadyForReview` covers the readiness table
      (native with no connector and a recorded change, native with no
      connector and none, native with a connector and an open pull request,
      native with a connector and a change but no pull request, native with a
      connector and a change below the item's revision, native with a
      connector and a merged or closed pull request, and the GitHub-profile
      rows unchanged), and
      `TestTransitionCompletedActiveIssuesPromotesNativeItemOnItsOwnChange`,
      `TestTransitionCompletedActiveIssuesReportsMissingNativeChange`,
      `TestTransitionCompletedActiveIssuesPromotesConnectedItemOnItsOwnChange`
      and `TestTransitionCompletedActiveIssuesKeepsOpenPullRequestRule`
      drive the whole tick, including that the skip line appears once per item
      across three ticks. `TestIssueFromNativeCarriesChangeReview` in
      `internal/hubclient` pins the mapping,
      `TestConversationDispatchLoopSettlesWithAGitHubConnector` runs the whole
      loop on a project that binds a repository and enables it — the eighth
      run's hub — and
      `TestWorkItemChangeSurfaceIsOptional`,
      `TestWorkItemChangeSurfaceReportsNoConnector`,
      `TestWorkItemChangeSurfaceReportsAConnectorWithNoPullRequest`,
      `TestWorkItemChangeSurfaceFollowsTheAttemptDiff` and
      `TestWorkItemChangeSurfaceReportsTheChangeRequest` in
      `internal/hubserver` pin the resource, including that the default work
      item resource is unchanged and that an edit moves the recorded change
      below the item's revision.

- [ ] **`detent hub issue get` inside the workspace cannot reach the Hub.**
      Open evidence, diagnosed September 12, 2026 and deliberately not fixed:
      it is a contract decision, not a pass-through bug. The first run's item 4
      ("Detent rejects its configured Hub URL"), the fourth run's "Workpad
      update failed because Detent's Hub URL is invalid", the fifth run's
      "Couldn't update the native Workpad: Detent CLI rejected its configured
      Hub URL" and the sixth run's failure to self-promote are all one thing.

      The agent's subprocess inherits the runner's own environment plus a fixed
      override set, and that set is the whole of it: `GH_CONFIG_DIR` and the
      four GitHub token names (`internal/runner/worker_github.go`), `GOCACHE`,
      `GOMODCACHE`, `GOBIN`, `GOLANGCI_LINT_CACHE` (`worker_cache.go`), and
      `TMPDIR`, `TMP`, `TEMP`, `DETENT_WORKER_SCRATCH`
      (`procgroup.SetTempDir`). Nothing carries the Hub URL, the global
      configuration path, or a Hub credential, and there is no `os.Setenv` on
      the path.

      So `detent hub issue get`, with no `--hub-url`, falls back to
      `globalconfig.ResolvePath("")`, which reads the env names `CONFIG`,
      `CONFIG_HOME`, `DETENT_CONFIG` and `DETENT_HOME` — none of them set —
      and lands on `os.UserConfigDir()`. A dogfood runner is started with
      `--config global.yaml` on argv (and a launchd or systemd runner gets
      only `PATH` and `DETENT_SERVICE_MANAGER`, `internal/service/launchd.go`,
      `systemd.go`), so the runner's own configuration is exactly what the
      agent cannot see. The failure is then either
      `globalconfig.MissingFileError` on the default path or, when a default
      file exists with no `client.url`, `hubclient.New`'s "hub URL must be an
      absolute HTTP or HTTPS URL" — which is the sentence the model
      paraphrased.

      It is not a one-line pass-through because there is nowhere to pass it
      from: `runner.Dependencies` (`internal/cli/runner.go`) never receives
      `boot.Global.Path` or the `HubClient` settings, and
      `runner.AgentTurnRequest.Environment` is left zero-valued by every
      construction site except the security audit. Threading the path through
      is three or four lines; deciding **what** to thread is the open question.
      `--token-env DETENT_HUB_TOKEN` is not exported either, and the runner's
      `identity_file` names a runner credential, so handing the agent a usable
      Hub client means handing it a credential that can move issue state —
      which the first run already recorded as deliberately withheld:
      "Completion transitions stayed with the orchestrator, which is the
      intended division". The two honest options are to tell the agent in its
      prompt that it has no Hub CLI and must not attempt one, or to mint a
      narrow read-only project credential for the turn and export it with the
      URL. Neither is taken here.

- [ ] **Automatic replay.** Nothing the hub could not establish is ever retried
      for the user. A control that reached a worker becomes `unknown` on unbind,
      lease loss or restart, and the user retries it explicitly with the
      `retry` command (`decisions.md` section 10.3). A client that offers no
      retry affordance therefore strands those messages.

Product scope left for later milestones is listed in section 1 and in
`decisions.md` section 7.

## 8. Dogfooding on a real project

What it takes to run the conversation product against a real repository with
your own provider login, from this branch, before anything is pushed.

1. Build from the branch: `make app && go build -o tmp/detent ./cmd/detent`.
2. A hosted configuration file with a WorkOS organization you control
   (staging is fine), `conversation: {enabled: true}` and a `usage.prices`
   table (section 2). Hosted mode has no other sign-in path.
3. Start the hub: `DETENT_HUB_ADMIN_TOKEN=... WORKOS_API_KEY=... tmp/detent
   hub serve --database ./tmp/hub.db --listen 127.0.0.1:7777 --hosted-config
   ./tmp/hosted.yaml`, sign in, and create the project under Settings →
   Projects with its GitHub connector. Grant your own account `write` and
   `runners` on it.
4. Enroll a runner from Settings → Providers & runners on a machine with the
   Codex CLI logged in (`codex login`), and start it with the enrollment
   token: `tmp/detent runner --hub http://127.0.0.1:7777 ...`. The runner
   takes both issue runs and coordinator turns; the hub runs no model. The
   runner appends `--enable default_mode_request_user_input` to the Codex
   app-server command it launches, which the model needs to ask questions
   (section 7, O01); if you supply your own `command`, keep it or let the
   runner add it.
5. Open `/chat`, send a message, watch the coordinator answer from the
   runner, create the linked issue from the header's Add action, and follow
   the run on the conversation and the Work board.

Known gaps while dogfooding: the right panel's Browser, Terminal, Files and
Pull request surfaces are disabled cards (decisions section 18 is the
contract, not yet built); a session that expires mid-page shows an inline
"Try again" instead of the sign-in screen; only Cmd+K and Cmd+B are bound.


### As run on September 11, 2026

The first end-to-end run of this branch on a developer laptop: a real hosted
hub, a real enrolled runner, a real Codex login and a real git repository. It
answers "when do we see real data linked back from the backend" with "now, for
issue runs" — and names the two things that stopped it being "now" for
everything.

What was proven: a runner enrolled from Settings → Providers & runners claims a
native issue created from a chat, runs Codex 0.154.0 against a local checkout,
edits the file, runs the tests, posts its messages back into the conversation,
and the issue changes lane. The Providers and Runners sections, the Work board
card and the conversation timeline all showed hub data from that runner, not
fixtures. What was not proven: the runner-dispatched coordinator (section 9 of
`decisions.md`), for the fixture reason recorded at the end.

#### The hub

There is no WorkOS organization to point at on a laptop, so the run used the
browser preview fixture, which serves a real hosted hub with a real database
behind a fake identity provider:

```sh
DETENT_HOSTED_BROWSER_PREVIEW=1 DETENT_HOSTED_BROWSER_PREVIEW_DURATION=12h \
  go test ./internal/hubserver -run TestHostedBrowserPreview -count=1 -timeout 13h -v
```

`DETENT_HOSTED_BROWSER_PREVIEW_COORDINATOR=runner` starts the same preview with
**no hub-side coordinator backend**, which is the only way to exercise the
runner-dispatched coordinator (`decisions.md` section 9) without a real WorkOS
organization: an unlinked chat then creates a `detent:coordinator` work item
and an enrolled runner claims it, instead of the fixture answering with its
scripted sentence. Unset, and for every Playwright spec, the preview keeps the
scripted backend — the specs assert against its canned reply. Anything other
than `runner` is ignored.

It logs `Hosted browser fixture: <path>`; that JSON names the hub URL, the
project ids and one sign-in URL per fixture account. This run got
`http://127.0.0.1:57864`, organization `org_browser_preview` and project
`prj_3cf406d4973448569c59666f3603643e` ("Browser collaboration"). The preview
embeds `static/app/conversation` at compile time, so a client change needs
`make app` **and** a fresh preview before the browser sees it.

Two things the hub then needed, neither of which the product does for you:

1. **The runner grant.** `requireHostedAdministration` gates every runner
   route on the runner grant, which is a permission of its own and separate
   from the role: an owner starts without it, and until it is granted
   `actor.can_manage_runners` is false and the enrollment control is correctly
   absent. Granted with `PUT {base}/members/:member/grants` (`{"project_id":
   …, "write": true, "runner": true, "revoke": false, "idempotency_key": …}`).
   The run had to grant it on *every* project in the organization, because
   `hostedAllRunnerGrants` demanded all of them. It no longer does: the
   boundary asks only whether the member holds the grant somewhere, and
   `createRunnerEnrollment` then requires it on each project the enrollment
   actually names — an owner or an admin may name any project. An enrollment
   naming a project the member was not granted is refused with the same opaque
   `404 not_found` the hosted boundary rewrites every administration error
   into; it does not name the missing project, so grant the projects you intend
   to enrol for before opening the dialog.
2. **An approved policy descriptor.** Without one, `detent doctor` fails the
   project with `Hub request failed with status 409 (policy_mismatch)` and no
   lease is issued. `detent hub policy inspect --config … --project dogfood`
   prints the descriptor; it was approved with
   `PUT {base}/projects/:project/onboarding/policy`
   (`{"expected_policy_id": "", "policy": <descriptor>}`).
   `PUT {base}/projects/:project/policy` — the non-onboarding route — answered
   `404 not_found` for a hosted session during the run, because it was gated on
   the instance administrator and `requireHostedAdministration` did not know
   the path. It is hosted administration now (owner and admin, CSRF and audit
   like every other one), so either route approves the policy; section 1.2 has
   the JSON shape.

Hosted calls from `curl` need the session cookie, `Origin: <hub url>` and
`X-CSRF-Token: sha256("detent-hosted-csrf:" + <cookie value>)`; the same token
is in `GET /app/bootstrap` as `csrf_token`.

#### The repository

A throwaway module outside the repository: `go.mod`, a `main.go` whose
`Greeting` is missing a `"!"`, a table-driven `main_test.go` that fails on it,
an `AGENTS.md` saying to make the test pass and nothing else, and:

```yaml
# detent.yaml
schema: 1
tracker:
    kind: hub_native
    active_states: [Todo, In Progress]
    terminal_states: [Done]
gate:
    kind: command
    run: go test ./...
    automated_review: off
codex:
    approval_policy: never
    thread_sandbox: workspace-write
    turn_sandbox_policy:
        type: workspaceWrite
        networkAccess: true
```

`WORKFLOW.md` next to it is the prompt. No git remote; the hub is the tracker,
so nothing needed pushing.

#### The runner

```sh
go build -o tmp/detent ./cmd/detent
mkdir -m 700 -p <private>/runner-identity
tmp/detent hub runner init --hub-url http://127.0.0.1:57864 \
  --identity-file <private>/runner-identity/identity.json
```

`init` refuses a directory that is not `0700` and any path with a `.git`
anywhere above it. It prints the two host-generated identifiers, which were
pasted into the new **Enroll a runner** dialog in Settings → Providers &
runners with both projects selected. The dialog's one-time token was redeemed
on the host with the command it shows:

```sh
DETENT_RUNNER_ENROLLMENT_TOKEN=… tmp/detent hub runner enroll \
  --organization org_browser_preview --display-name 'Dogfood host' \
  --capacity 2 --identity-file <private>/runner-identity/identity.json
```

There is no `detent runner` command: the runner is the ordinary daemon reading
a global configuration whose `client:` block points at the hub. Redacted, what
this run used:

```yaml
apiVersion: detent/v1
kind: GlobalConfig
env: prod
log_level: debug
instance_name: dogfood-runner
client:
    provider_capacity_file: <private>/provider-capacity.json
    hub_url: http://127.0.0.1:57864
    identity_file: <private>/runner-identity/identity.json
    organization_id: org_browser_preview
    native_projects:
        dogfood: prj_3cf406d4973448569c59666f3603643e
    display_name: Dogfood host
    capacity: 1
    heartbeat_interval_seconds: 15
    lease_ttl_seconds: 90
    # The run needed this: the runner long-polls conversation controls for
    # 25s, and the default 10000 aborted every poll. No longer required —
    # the poll now carries its own deadline. See "What did not work" below.
    request_timeout_ms: 40000
global:
    max_concurrent_agents: 1
    scheduling: weighted
    agents:
        model_selection:
            preset: sol_first
            normal_model: gpt-6-astra
            complex_model: gpt-6-astra
            levels:
                normal: {effort: low}
                complex: {effort: low}
                very_complex: {effort: low}
projects:
    - id: dogfood
      workflow: <checkout>/WORKFLOW.md
      workdir: <checkout>
      weight: 1
      priority: 3
```

Started with `tmp/detent --config <private>/global.yaml --headless --host
127.0.0.1 --port 4321`. Use a port other than 4000 and a configuration
directory of its own: the daemon holds a lock on the `detent.db` beside its
configuration file and will refuse to start beside a running instance.

`provider_capacity_file` is what makes the Providers section say anything, and
it is what section 9.2 gates coordinator claims on. Its `observed_at` has to be
within `providercapacity.MaxAge` (two minutes), so a static file goes stale; a
30-second `sh` loop rewrote it:

```json
[{"provider":"openai","backend":"codex","account_alias":"personal",
  "models":["gpt-6-astra"],"max_concurrent":1,"availability":"available",
  "observed_at":"2026-09-11T21:30:37Z"}]
```

With that in place `GET {base}/fleet` listed the host as `online`, Settings →
Providers & runners drew a real `openai · gpt-6-astra · 0/1 in use` row, and
`capabilities.coordinator` was true.

#### The run, with wall-clock

All times UTC on September 11, 2026. From an empty `/chat` to a passing test
was about ninety seconds.

| Time | What happened |
|---|---|
| 21:47:5x | `/chat`: "Read AGENTS.md and tell me in one sentence what this repository needs". Answered in under a second — by the preview's scripted hub backend, not the runner (see below). |
| 21:48:32 | "Create linked issue" from the header's Add action, title "Make the unit test pass", share-history confirmed, dispatch "Now". |
| 21:48:50 | The runner claimed it. Todo → In Progress, attempt started. 18 seconds after the issue existed. |
| 21:48:55 | `worker_conversation_bound`, `resume: transcript` — §10.4's path, because the chat's provider thread came from the hub-side coordinator and belongs to no runner. |
| 21:49:00 | `codex app-server` launched, `gpt-6-astra`, effort `low`. |
| 21:50:08 | Turn succeeded (68s). |
| 21:50:11 | Attempt succeeded (81s end to end). |

The workspace diff was exactly the intended one-line change, and
`go test ./...` passed in it:

```
 main.go | 2 +-
-	return "Hello, " + name
+	return "Hello, " + name + "!"
```

The conversation showed the runner's own messages, each tagged with its attempt
id: "I'll read the repository instructions, run the test, and make the smallest
required change to `main.go`.", then "Changed only `main.go` to append the
missing `!`. `go test ./...` passes.", with "Worked for 18s" / "Worked for 42s"
timers, tool events for each `commandExecution`, and live Interrupt/Continue
controls. The Work board card showed `gpt-6-astra · codex · runner_46… ·
Running attempt 4`. The issue was moved to Done from the board at the end and
the lane counts settled to `3 completed`.

After raising `request_timeout_ms`, steering was proven too: a message sent
into a running attempt was `delivered` at 21:53:37 and the model's reply
("DOGFOOD-CONTROL-OK") was in the conversation at 21:53:52 — fifteen seconds,
through live control on the runner's own Codex thread.

#### What did not work

1. **The default Codex approval policy is rejected by Codex CLI 0.154.0.**
   *Fixed.* With no `codex:` block in `detent.yaml`, every turn died
   immediately:

   ```
   codex response error: thread/start: Invalid request: unknown variant
   `reject`, expected one of `untrusted`, `on-request`, `granular`, `never`
   ```

   `config.go` defaults `Codex.ApprovalPolicy` to the map
   `{"reject": {"sandbox_approval": true, "rules": true,
   "mcp_elicitations": true}}`, and the current app-server has no `reject`
   variant. Existing projects papered over it because they all set
   `approval_policy: never` themselves; a fresh project set up from the
   documentation did not, and failed on its first turn with a backend error
   rather than a configuration message. The run set `approval_policy: never`
   to get past it.

   Detent now translates its own approval vocabulary when it builds the
   `thread/start`, `thread/resume` and `turn/start` parameters
   (`wireApprovalPolicy` in `internal/codex/approval_policy.go`): `reject`, as
   the word or as the mapping, is sent as `never`, which is the exact
   equivalent — Codex asks nobody and refuses what it cannot run unattended.
   `never`, `untrusted`, `on-request` and `granular` pass through, `on_request`
   canonicalises to `on-request`, and anything else is forwarded unchanged so a
   newer Codex variant needs no Detent release. Existing configuration files
   stay valid and `codex.approval_policy` still reads back what the operator
   wrote. The table is in `docs/config.md`, "Codex approval policy".

2. **No conversation control can reach a bound attempt on the default runner
   configuration.** `internal/hubclient/native_conversation.go` fixes
   `conversationPollWait = 25 * time.Second`, and `newHubScheduling` builds its
   `http.Client` with `Timeout: clientConfig.RequestTimeout()`, whose default
   is `DefaultHubRequestTimeoutMillis = 10000`. Every long poll therefore
   aborts before the hub can answer, once every ten seconds, for the life of
   the attempt:

   ```
   conversation control poll failed: hub unavailable: Get
   ".../conversations/conv_…/controls?after=0&wait=25":
   context deadline exceeded (Client.Timeout exceeded while awaiting headers)
   ```

   Steering, answering a question and interrupting were all silently dead in
   that state — the conversation still showed the runner's output, so nothing
   on screen said the channel one way was broken. `request_timeout_ms: 40000`
   worked around it from configuration.

   *Fixed in code.* The controls poll now carries its own deadline of the wait
   it asked for plus `conversationPollSlack` (10s), through
   `Client.requestWithDeadline`, which also lifts `http.Client.Timeout` for
   that one request — a hard cap a longer context deadline cannot raise on its
   own. Every other Hub call still honours `request_timeout_ms`, so this is a
   per-request deadline and not a global bump, and `request_timeout_ms: 40000`
   is no longer needed for controls to work. A poll that keeps failing now logs
   `conversation control poll failed` at warn **once a minute** with the
   failure and suppressed counts, instead of once per poll.

3. **The runner-dispatched coordinator could not be exercised at all.**
   *Fixed.* The preview fixture hard-coded `Conversation:
   &ConversationConfig{Enabled: true, Backend: &browserConversationBackend{},
   …}`, and a hub with a backend never takes the section 9 path. The chat's
   reply was the fixture's canned sentence; `GET
   …/work-items?include=coordinator` showed no coordinator item was created and
   the runner log showed no claim. The fixture now reads
   `DETENT_HOSTED_BROWSER_PREVIEW_COORDINATOR`; set it to `runner` and the
   preview configures no hub backend, so an unlinked chat dispatches to an
   enrolled runner. The default and every Playwright spec keep the scripted
   backend. See "The hub" above.

4. **The agent could not record its own completion.** Its messages said
   "Detent rejects its configured Hub URL", and `detent hub issue get` failed
   inside the workspace. Completion transitions stayed with the orchestrator,
   which is the intended division, but the agent spent a turn discovering that.

5. **Two local hubs fight over one session cookie.** *Known local-development
   limitation; deliberately not fixed.* Cookies are scoped by host and not by
   port, so a second hub on `127.0.0.1` evicts `detent_hosted_session` for the
   first, and the open tab lands on `/login` on its next navigation with
   `GET /app/bootstrap` answering `401 A hosted session is required`. Anybody
   keeping two local hubs open will read that as the product logging them out.

   Production serves one hub per host, so the scoping is correct there and is
   not changed for a local convenience; naming a port in the cookie would
   weaken it everywhere it matters. The workaround is one hub per browser
   profile — a separate Chrome profile, or one hub in a private window — so
   each hub owns its own cookie jar. Closing the other hub's tabs and signing
   in again also works, for as long as only one hub is open.

Two smaller notes. `detent doctor` against a hub project reports thirteen
warnings that are about the *other*, already-running instance on port 4000
(build drift, remote service health, missing telemetry tables) — pass
`--port 0` and read only the project's own rows. And an issue left in an
active state was re-dispatched forever: this run's issue was claimed four times
before it was moved to Done, because a three-state project has no
`Human Review` for the gate to promote into. *Fixed.* Section 7, "An item in an active
state is re-dispatched forever", has the full code path, why the obvious
claim-side guard was not safe, and the two brakes that replaced it.

#### Second run, September 12, 2026 00:47 UTC

The environment was rebuilt from `0dcc334c` after the fixes above merged, with
a fresh preview hub and a re-enrolled runner. The smoke test passed end to
end: a chat was linked to a new issue, the runner claimed it 46 seconds after
a slot freed, its messages landed in the conversation tagged with the attempt
id, and the workspace commit changed one line of `main.go` with `go test
./...` green. What the first transcript no longer gets right:

1. **`"expected_policy_id": ""` now answers `409 policy_mismatch`.** The
   preview seeds a pull request, and seeding it approves a policy on the
   project first, so the descriptor already has an id. Read it back with
   `GET {nativeBase}/policy` and pass that id as `expected_policy_id`.
   `PUT {nativeBase}/policy` did work for the hosted owner: 200, with
   `approved_by` set to the session's member id.
2. **The enrollment token lives 900 seconds at most.** `ttl_seconds` is capped
   at `MaxEnrollmentTTL`; redeem it late and `detent hub runner enroll` says
   only `401 unauthorized`. Create the enrollment as the last step before the
   runner starts.
3. **Move the fixture's seeded issues to Done before dispatching anything.**
   The preview seeds one issue in Todo and one linked issue; with
   `capacity: 1` the first was claimed, finished and re-claimed in a loop and
   the new issue waited at `waiting_for_runner` for six and a half minutes.
   The same loop then took the new issue: seven attempts by the time it was
   moved to Done by hand, each one a real Codex turn. *Fixed.* Section 7, "An item in
   an active state is re-dispatched forever", is the fix; the seeded issues no
   longer have to be moved to Done before dispatching anything, because a
   succeeded attempt is not re-offered.
4. **`DETENT_HOSTED_BROWSER_PREVIEW_COORDINATOR=runner` did not boot at
   `0dcc334c`.** `seedConversation` (added on another branch the same day)
   waited for the scripted coordinator's canned reply, which a hub with no
   backend never produces, so the preview died in setup after 30 seconds with
   `coordinator reply did not complete`. The wait is now skipped in runner
   mode; the link the fixture makes needs the conversation, not the reply.
   This run therefore used the scripted backend for the unlinked chat; every
   issue-driven step was the real runner.
5. **`request_timeout_ms: 40000` is no longer needed** in the runner config
   for the control poll (item 2 above); the sample keeps it only as an
   example. The runner also logs `WARN chat provider unavailable: no
   compatible Codex backend is configured` at startup, which is harmless: the
   runner's own chat provider is not the hub's coordinator.

#### Third run, September 12, 2026 06:55 UTC

Rebuilt from `c0b6243c` on a fresh preview hub (`http://127.0.0.1:61906`,
project `prj_2638f31342064eb08b10a716b996fdfb`) with a re-enrolled runner.
`DETENT_HOSTED_BROWSER_PREVIEW_COORDINATOR=runner` now boots, so this is the
first run that proved the **runner-dispatched coordinator end to end**: an
unlinked chat created coordinator work item #5, the real runner claimed it
(`worker_conversation_bound … "coordinator": true`), and the answer in the
conversation was the runner's own model, not a canned sentence. What the
earlier transcripts no longer get right:

1. **The preview's pre-warmed workspace blocks the policy approval for its
   first 90 seconds.** The fixture now starts an in-process workspace runner
   for the Files surface and pre-warms a workspace, which takes an unreleased
   lease on the project scope at boot. `approvePolicy` refuses to move to a
   *different* descriptor while any lease in the scope is unexpired, so a
   `PUT {nativeBase}/policy` inside that window answers `409 policy_mismatch`
   even with the correct `expected_policy_id` — the message is the same code
   as a stale id, so it reads like item 1 of the second run and is not. The
   lease is never renewed: wait for it to expire (90 s from the line the
   fixture logs as `Preview workspace: … ready on runner …`) and the same
   request answers `200`.
2. **The policy body takes exactly two fields.** `decodeAPIJSON` sets
   `DisallowUnknownFields`, so an `idempotency_key` alongside
   `expected_policy_id` and `policy` is `422 invalid_request`, not a warning.
3. **The pre-warmed workspace item is ordinary dispatchable work, and a real
   runner claims it.** *Fixed; see "What the third run fixed" below.* The fixture's workspace session is a native work item
   labelled `detent:workspace` ("Workspace session ws_…", "No implementation,
   branch or pull request is expected") sitting in Todo. It is **not** in
   `GET {nativeBase}/work-items?include=coordinator`, so the second run's
   "move the seeded issues to Done first" does not catch it. Within twenty
   seconds of starting, this run's runner claimed it, opened a worktree and
   spent a real Codex turn on it, holding the only `capacity: 1` slot; the
   fixture's own workspace runner had already gone `offline` and never took
   it back. Read it by id (`wi_…`, number 4 here) or from the hub database
   and move it to Done with the other seeds before starting the runner.
4. **A coordinator turn on the same runner poisons the next issue run.**
   *Fixed; see "What the third run fixed" below.* The
   chat's coordinator turn and the issue run both land on the one enrolled
   runner, so the coordinator's Codex thread is local and resumable and the
   worker takes `resume: "thread"` on it instead of the second run's
   `resume: "transcript"`. The model stayed under its coordinator
   instructions and refused the assignment — "This session's coordinator
   restrictions prohibit editing files, running tests, or changing issue
   state, so I can't execute the worker assignment" — while the attempt was
   still recorded `worker_attempt_finished … "outcome": "succeeded"` and the
   issue was promoted out of In Progress. The workspace held no change to
   `main.go`. A control issue linked from a chat with no coordinator turn
   took `resume: "transcript"` on the same runner minutes later and made the
   intended one-line edit with `go test ./...` green, which isolates the
   thread reuse as the cause. Until this is fixed, a green attempt on a
   conversation that has already had a coordinator turn is not evidence of
   work; check the workspace diff.
5. **`share_history: false` is refused on link.** `POST
   {nativeBase}/conversations/:conversation/link` answers
   `422 share_history_required`; the first run's "share-history confirmed"
   is the only path.
6. **The owner already holds the runner grant on the seeded project.**
   `GET /app/bootstrap` came back with `actor.can_manage_runners: true` and
   `can_manage_runners` on `prj_2638…`, so the first run's grant step
   (item 1 of "The hub") was not needed. The owner's second, private project
   reports `can_manage_runners: false`, which is correct and harmless as long
   as the enrollment names only the project being dogfooded.

#### What the third run fixed

Items 3 and 4 above are fixed on this branch; this is what the fixes are and
what the run taught about proving them.

1. **The control case is what isolated the first defect.** A second issue,
   linked from a chat that had never taken a coordinator turn, bound with
   `resume: "transcript"` and did its work correctly. Same runner, same
   repository, same prompt: the only difference was whether the conversation
   already carried a coordinator's provider thread. Keep a chat with no
   coordinator turn in any run that touches binding — it is the difference
   between "the runner is broken" and "the runner inherited something".
2. **A `detent.yaml`-only diff still reported `outcome: succeeded`.** The
   attempt that refused the assignment (item 3) changed nothing but the
   configuration file and still unbound as succeeded. The orchestrator does
   have a "no work done" verdict — `store.WorkAttemptTerminalNoProgress` with
   the reason `completed_clean_diff_without_pull_request` in
   `internal/orchestrator/implement_progress.go` — but it keys on an *empty*
   workspace diffstat, and a one-file configuration diff is not empty, so it
   never fired. Teaching that verdict to treat a diff confined to
   configuration as no progress is a change to a safety-critical brake with a
   90% coverage floor and its own fuzz seeds; it is **deliberately left for a
   follow-up** rather than folded into these two fixes.
3. **An issue attempt resumed the coordinator's Codex thread and inherited its
   restrictions.** *Fixed.* The chat's coordinator turn was claimed as a
   `detent:coordinator` item (`coordinator: true`) and opened thread
   `01a0946a-…` on that runner. The chat was linked; the issue attempt on the
   same runner bound with `resume: "thread"`, `coordinator: false` and that
   same `thread_id`, and Codex answered "This session's coordinator
   restrictions prohibit editing files, running tests, or changing issue
   state, so I can't execute the worker assignment."

   The hub resumed any thread the binding runner had produced, and the
   coordinator's thread was one of those. A provider thread carries the
   instructions and the permission set of the turn that opened it, which the
   contract had not said. `decisions.md` section 9.3 now states the rule as
   **origin equality**: the conversation records
   `provider_thread_origin` (`coordinator` or `worker`) wherever it records
   the thread id, and `bind` returns `resume.thread_id` only when the recorded
   runner is the binding runner *and* the recorded origin matches the kind of
   turn that is binding. A mismatch in either direction, and a thread recorded
   before the column existed, is handed the bounded transcript instead, with
   the section 10.4 notice in the history. `bind` also returns
   `resume.thread_origin`, so a different runner reaches the same answer
   without keeping a thread registry of its own. A coordinator's own
   continuation — a second coordinator turn on the same chat — still resumes
   its thread, exactly as before.
4. **The orchestrator claimed `detent:workspace` items as ordinary issues.**
   *Fixed.* The preview fixture pre-warms a workspace session (section 18.1),
   which is a dispatchable `detent:workspace` work item. The real `detent`
   runner claimed it through its normal issue lane within 20 seconds and spent
   a Codex turn on it.

   The hub had a workspace claim gate, but it was a *capability* gate over the
   *open* workspace items only: a runner that failed the capability check was
   skipped, and a workspace item whose session had closed dropped out of the
   gate's view entirely while its issue stayed in a dispatchable workflow
   state. Closing a workspace never moved its issue to a terminal state the
   way closing a coordinator item does, so the issue went on being ordinary
   claimable work forever. Now the exclusion is in the claim candidate query
   itself, like the coordinator's: a claim is offered workspace items only
   when it declares the new `workspace_sessions` claim capability
   (`tracker.NativeWorkspaceCapability`), which is what
   `hubclient.WorkspaceClaimer` — the runner's workspace lane, and nothing
   else — sends. Every other claim, and the provider candidate preview that
   shares the query, never sees one, open or closed. The declared capability
   rather than the label filter is the authority, because a label filter is a
   preference an operator can set on any lane.

   The runner's own workspace lane is also started now: the `detent` runner
   process reports its workspace capabilities on the machine heartbeat and
   runs `internal/workspacerunner.Lane` when it reports `files`. Before this
   it existed only in tests, which is why the fixture's pre-warmed workspace
   had no eligible runner to go to in the first place.

#### Fourth run, September 12, 2026 08:13 UTC

Rebuilt from `bef65adc` on a fresh preview hub (`http://127.0.0.1:63071`,
project `prj_1e6a02e2247544b48ade96f4dbdb5f4d`) with a re-enrolled runner, to
prove the two fixes in "What the third run fixed" against a real Codex runner.
Both held. Nothing else in the transcript above changed; the notes below are
only what this run adds.

Setup was the third run's, in the third run's order, and cost nothing new. The
policy approval answered `200` on the first try because the 90-second pre-warm
lease had already expired by the time the descriptor was read back — the wait
in item 1 of the third run is real, but it is a wait, not a retry loop, and
reading `GET {nativeBase}/policy` for `expected_policy_id` before approving is
enough. The seeded issues (#1, #3) were moved to Done first; the pre-warmed
`detent:workspace` item was **deliberately left in Todo**, which is the whole
experiment. The runner's own `detent.db` was moved aside before the run so no
state from the third run could be mistaken for evidence.

**Fix 1 held: an issue attempt no longer resumes the coordinator's thread.**
The chat took a real runner-dispatched coordinator turn, which opened provider
thread `01a094b5-8198-7252-b1c6-7687a05543d1`:

```
worker_conversation_bound  conversation_id=conv_23d2879c9b4562b9fbbd449f357bfbe1
  thread_id="" resume="" thread_origin="coordinator" coordinator=true
  work_attempt_id=1 issue_id=wi_512b93d2d8f8496db27c7bcc8041208f
```

The same chat was then linked to issue #6 "Make the unit test pass" with
`dispatch: now`, carrying `execution.thread_id:
"01a094b5-8198-7252-b1c6-7687a05543d1"` — the coordinator's thread — into
the link. The issue attempt bound on the **same runner** two minutes later:

```
worker_conversation_bound  conversation_id=conv_23d2879c9b4562b9fbbd449f357bfbe1
  thread_id="" resume="transcript" thread_origin="worker" coordinator=false
  work_attempt_id=2 issue_id=wi_57b8e20b7b9f472bbeceebeebe94af0f
```

`resume: "transcript"`, not the third run's `resume: "thread"`; `thread_id` is
empty rather than the coordinator's; and `thread_origin: "worker"` is the new
field naming the kind of turn that is binding, so the mismatch that forced the
transcript is visible in the log rather than only in the answer the model
gives. The attempt opened a thread of its own —
`01a094b7-5a82-7670-8a6b-580e16697941` on `worker_turn_started`, a different id
from the coordinator's — and the section 10.4 notice reached the conversation
as its own system message: *"Provider history was not available on this runner;
continuing from a transcript of the last 3 messages"*.

The work was real, which is what the third run's green-but-empty attempt was
not. `worker_attempt_finished … "mode": "implement", "outcome": "succeeded"`
41 seconds after the start, and the workspace held exactly the intended change:

```
 main.go | 2 +-
-	return "Hello, " + name
+	return "Hello, " + name + "!"
```

`go test ./...` in that worktree: `ok  example.com/dogfood  0.252s`. The
conversation carried the runner's own messages under the attempt id — "I'll
run the test, make the smallest change to `main.go`, and verify the result.",
then the `commandExecution` events for `go test ./...` before and after the
edit, then "Changed only `main.go` to append `!` to greetings. `go test ./...`
passes." The issue was moved to Done the moment the attempt succeeded,
because section 7's re-dispatch loop was still open at this commit. It is
fixed since; a run from this branch promotes it without a hand edit.

The fourth run's model closed with "Workpad update failed because Detent's Hub
URL is invalid", which is the first run's item 4 unchanged: completion
transitions stay with the orchestrator and the agent still spends a turn
discovering it.

**Fix 2 held: no lane claimed the `detent:workspace` item, and the runner's own
workspace lane is running.** The pre-warmed item was
`wi_2a69852b8736480482f3ed6b6f9e2cda`, number 4, labelled `detent:workspace`,
in `Todo` with `dispatchable = 1` — and its workspace session had already gone
`state=failed, reason=lease_lost` at 08:16:05, so this was the *closed*
workspace case the fix names, the one the old capability gate stopped seeing.
It sat there for the whole run:

* the runner log mentions `wi_2a69852b8736480482f3ed6b6f9e2cda` zero times in
  ten minutes of uptime — the third run's runner claimed it within twenty
  seconds;
* the only two `worker_attempt_started` events in the run are
  `…#5 mode=coordinator` and `…#6 mode=implement`;
* the hub's `leases` table has one lease on that item for the whole run, the
  fixture's own `machine_fe6443…` from boot. The dogfood machine
  `machine_9f3c7686790a48e18dc76765c93fe13b` took exactly two leases, on #5 and
  #6.

The lane is not merely absent, it is running. `runner_identities` carries the
heartbeat's report for the real runner:

```
workspace_capabilities_json={"terminal":false,"files":true,"diff":false,"preview":false}
workspace_isolation=user
```

and the lane polls the hub every five seconds under
`component: workspace_lane`. Its refusals are the clearest proof of what it can
and cannot see:

```
08:22:02–08:24:37  workspace.claim_failed  409 (runner_capacity)
08:24:37–08:26:47  (silence: the hub offers the lane nothing)
08:26:47–08:28:42  workspace.claim_failed  409 (provider_incompatible)
```

`runner_capacity` is the one `capacity: 1` slot being held by the coordinator
and then the issue attempt. The silence that follows is the important window:
the lane was polling, the runner was idle, item #4 was dispatchable — and the
hub offered nothing, because its session was closed. Opening a live workspace
by hand (`POST {nativeBase}/workspaces`, `requires: ["files"]`) at 08:26:44
started the third band within three seconds, so the lane is genuinely being
offered workspace items through the `workspace_sessions` claim capability;
deleting that workspace stopped it again.

Two notes this run adds:

1. **A workspace claim was refused `409 provider_incompatible`.** *Fixed.* The
   live workspace above was never actually claimed: every poll while it was
   open answered `provider_incompatible`, which
   `internal/hubserver/provider_capacity.go` raises as "Work item needs local
   model selection before a provider reservation can be claimed". A workspace
   session serves files and runs no model, so putting its claim through the
   provider reservation asked it for a model selection it has no reason to
   carry. This was **not** one of the two fixes and did not exist as a question
   before them — the lane had to start running before anything could refuse it.

   The claim loop in `internal/hubserver/api_worker.go` walked
   `selectProviderCapacity` for every candidate it reached. That function
   answers early — no reservation, no refusal — when the runner reports no
   provider account at all, which is why every fixture claimed a workspace
   happily and the real Codex runner could not: one heartbeat carrying a
   provider report is the whole difference. With a report present and no local
   model selection on the claim, its last line refuses.

   Now the gate that already decides which workspace items a claim may take
   (`gateWorkspaceClaim` in `workspace_dispatch.go`) also reports *which
   candidates are workspace items at all*, and a candidate in that set is
   claimed without provider requirement matching and without a provider
   reservation. Section 18.1 gives a workspace claim one slot of the runner's
   capacity and a workspace lease with its own fencing token and says nothing
   about a provider reservation, so that is exactly what it now takes: the
   lease response carries no `provider_reservation`, no row is written to
   `provider_reservations`, and a second claim against one remaining slot is
   refused `runner_capacity` and nothing else. The test is keyed on the
   candidate being a workspace item rather than on the lane the claim came
   from, so a workspace lane that dropped its label filter still takes an
   ordinary issue under the ordinary provider rules. The relay fixture used to
   fabricate a model selection to get its workspace claimed; it now claims the
   way `hubclient.WorkspaceClaimer` does, with none.
2. **Closing a workspace left its work item in `Todo`.** *Fixed.* Both #4 and
   the deleted #7 were `detent:workspace` items sitting dispatchable at the end
   of the run. That was exactly the state the third run had to clean up by
   hand, and the third run's fix deliberately did not clean it up: the
   exclusion is in the claim candidate query, so a stranded workspace item was
   inert rather than tidy.

   Inert is enough for the claim path and not enough for a person: a closed
   workspace that leaves its item in `Todo` puts a row on the board that looks
   like outstanding work and is not. A terminal workspace state now also moves
   its dispatch issue to the project's first terminal workflow state and then
   closes the association, the way `closeCoordinatorItem` does for a
   coordinator item. Section 18.1 asks only that closing never delete the
   attempt's artifacts, which this does not touch; a project with no terminal
   state keeps the issue where it is and the association closes regardless.
   Both brakes now hold: the item is terminal, so it is not a candidate, and
   the association is closed, so the gate skips it even if it were.

Environment for a follow-up: `<scratchpad>/dogfood/env4.sh`, hub log `hub4.log`,
runner log `runner4.log`, runner configuration `global.yaml` (kept as
`global4.yaml`), the third run's daemon state moved aside under
`old-61906-state/`.

#### Fifth run, September 12, 2026 09:21 UTC

Rebuilt from `4480e9c3` on a fresh preview hub (`http://127.0.0.1:53261`,
project `prj_e7da81064d514c5cbceebbfdf0f5f1c8`) with a re-enrolled runner, to
prove the two things the fourth run left open: a **workspace claimed by the
real Codex runner** rather than refused `provider_incompatible`, and a
**closed workspace whose dispatch issue reaches a terminal state**. Both held,
and the Files surface answered a real `list` frame from the dogfood worktree.
The ordinary chat → coordinator → link → attempt path ran once more and did not
regress. One new defect turned up, and it is the reason the first relay
connection failed: a workspace lease is never renewed.

**The workspace claim fix held: a real runner claimed a workspace, with no
provider reservation.** A work item was created for the subject
(`wi_0c1e9fe30a67444d8e0fc92ad24a2814`, #5) and a workspace requested against it
as the hosted owner with `POST {nativeBase}/workspaces`
(`{"idempotency_key": …, "work_item_id": …, "requires": ["files"]}`). It
answered `201` with `state: "requested"` and was `ready` seven seconds later:

```
09:27:09Z  POST …/workspaces → 201  ws_b8cfd7f776bde17ac8e8bd4b86a756bb  state=requested
09:27:16Z  GET  …/workspaces/ws_b8cf…  state=ready
           runner_id=runner_ff4a9ae48c744344aca11900f077accf
           machine_id=machine_c02e737c0d934ef898de0f247c001ce3
           capabilities={'terminal': False, 'files': True, 'diff': False, 'preview': False}
```

`runner_ff4a9ae4…` is the enrolled dogfood runner, not the fixture's
`runner_52698a39…`. Its workspace lane says the same thing, and says it with a
bind rather than a refusal:

```
04:27:14 INFO  workspace.claimed  component=workspace_lane
  workspace_id=ws_b8cfd7f776bde17ac8e8bd4b86a756bb work_item_id=wi_b84b237a31324ace93152878c3cbed0f
04:27:14 INFO  workspace.bound    component=workspace_session
  workspace_id=ws_b8cfd7f776bde17ac8e8bd4b86a756bb worktree=fresh read_only=false requires=["files"]
```

`provider_incompatible` appears **zero** times in ten minutes of run-five runner
log; the fourth run's band of it is gone. The only refusals the lane logged are
`409 (runner_capacity)`, once every five seconds while its one `capacity: 1`
slot was held by the workspace it had already claimed, which is what the fix
says the second claim should get and nothing else. The hub agrees on the other
half: `provider_reservations` holds exactly two rows for the whole run, one for
the coordinator lease and one for the issue lease, and none for either workspace
lease.

**The Files surface answered on the real runner.** A relay ticket
(`POST {nativeBase}/workspaces/:workspace/relay-tickets`, `expires_in: 30`,
single use) was redeemed on `GET …/workspaces/:workspace/relay?ticket=…` with
the hosted session cookie, and one files frame with no stream — the hub
allocates one on the first request — came back as a listing of the dogfood
worktree:

```
--> {"channel":"files","payload":{"path":"."},"type":"list"}
<-- {"channel":"files","stream":"relayconn_be994b0a02294c9281b278a7afcbbbca:1","type":"listed","seq":1,
     "payload":{"path":"","entries":[
       {"name":".git","kind":"file","size":224,…},
       {"name":"AGENTS.md","kind":"file","size":341,…},
       {"name":"WORKFLOW.md","kind":"file","size":362,…},
       {"name":"detent.yaml","kind":"file","size":357,…},
       {"name":"go.mod","kind":"file","size":36,…},
       {"name":"main.go","kind":"file","size":189,…},
       {"name":"main_test.go","kind":"file","size":437,…}]}}
```

That is the dogfood repository, read off the runner's own fresh worktree. There
is no `ws` package in `node_modules`, so the client was twenty lines of Go over
`github.com/coder/websocket` in the scratchpad; a browser page would do the same
thing with a native `WebSocket` and the same cookie.

**The terminal-state fix held, on both paths.** Closing a workspace now moves
its dispatch issue to the project's first terminal state and closes the
association, and this run saw it happen three times without a hand edit:

| Workspace | How it ended | Session row | Dispatch issue |
|---|---|---|---|
| `ws_de2843…` (the fixture's pre-warm) | lease lost at 09:23:35 | `failed / lease_lost` | #4 `Done`, `closed_at 09:23:35.45248Z` |
| `ws_b8cfd7…` | lease lost at 09:29:45 | `failed / lease_lost` | #6 `Done`, `closed_at 09:29:45.457069Z` |
| `ws_a45e63…` | `DELETE …/workspaces/:workspace` → `204` at 09:32:16 | `closed / closed_by_actor` | #7 `Done`, `closed_at 09:32:16.677137Z` |

The delete case in full: immediately before the request, #7 was
`detent:workspace`, `Todo`, `terminal = 0`, association open; six seconds after
it, `GET {nativeBase}/work-items/wi_69bd724a6b3b42008453de15e94e3976` answered
`state= Done terminal= True labels= ['detent:workspace']`. The fourth run's
observation 2 — "closing a workspace left its work item in `Todo`" — does not
reproduce. Note the consequence for the setup order: the third run's "move the
pre-warmed `detent:workspace` item to Done by hand before starting the runner"
is **no longer a step**, because the fixture's pre-warm loses its lease at boot
and the hub closes the item itself.

**The ordinary path did not regress.** A chat created at 09:33:16 took a real
runner-dispatched coordinator turn — coordinator work item #8, claimed and
answered in seventeen seconds:

```
worker_conversation_bound  conv=conv_7bab85a41b750d2bf79f71f5c12a40fd
  thread_id='' resume='' thread_origin='coordinator' coordinator=True attempt=1
  issue=wi_0a5d561f54674f9cb24bf2439ad351ce
```

The chat was linked to #9 "Make the unit test pass" with `dispatch: now`,
carrying the coordinator's `execution.thread_id`
`01a094f6-da44-73c2-9100-be64086149b8`. The issue attempt bound on the same
runner two minutes later and did **not** take that thread:

```
worker_conversation_bound  conv=conv_7bab85a41b750d2bf79f71f5c12a40fd
  thread_id='' resume='transcript' thread_origin='worker' coordinator=False attempt=2
```

It opened `01a094f8-b38d-7d33-bbb5-517fc1737e37` of its own, the section 10.4
notice reached the conversation ("Provider history was not available on this
runner; continuing from a transcript of the last 3 messages"), and
`worker_attempt_finished … "mode": "implement", "outcome": "succeeded"` came 34
seconds after the start. The workspace held exactly the intended change and
nothing else:

```
 main.go | 2 +-
-	return "Hello, " + name
+	return "Hello, " + name + "!"
```

`go test ./...` in that worktree: `ok  example.com/dogfood  0.247s`. The issue
was moved to Done the moment the attempt succeeded. The model closed with
"Couldn't update the native Workpad: Detent CLI rejected its configured Hub URL.
Completion transition remains with Detent." — the first run's item 4, unchanged.

**New: a workspace lease is never renewed, so a Files session lives exactly one
`lease_ttl_seconds`.** *Fixed.* This is what broke the first relay attempt, and it is not
a fixture artefact. `ws_b8cfd7…` took lease `33f193f5-…` at 09:27:14 with
`expires_at 09:28:44` (the runner's `lease_ttl_seconds: 90`). The session
heartbeated on schedule — `workspace_sessions.last_heartbeat_at` reached
09:28:14 and no `workspace.heartbeat_failed` was logged — but
`workspaceService.heartbeat` (`internal/hubserver/workspace_worker.go`) writes
only the `workspace_sessions` row, and `Session.renewLease`
(`internal/workspacerunner/session.go`) only advances the runner's own in-memory
`leaseValidUntil`. Nothing on the workspace path calls
`POST /api/v1/leases/:id/renew`, which is the only thing that moves
`leases.expires_at`. At 09:28:44 the row expired and everything downstream
failed at once:

* a person frame sent at 09:28:5x was refused
  `{"channel":"","type":"error","payload":{"code":"revoked","message":"Access to this workspace has changed"}}`
  and the socket was closed — `revalidatePersonLocally` reads the `leases` row
  and returns "the workspace lease is no longer current";
* the runner logged `workspace.worktree_not_released` and
  `workspace.lease_not_released … reason=completed error=Hub request failed with
  status 409 (stale_fencing_token)`;
* the session went `unreachable / runner_restarted` and then
  `failed / lease_lost`;
* the worktree it left behind then failed the orchestrator's own cleanup twice,
  `workspace residual reconciliation failed … workspace retained for recovery
  at …/detent_workspaces/dogfood-wi_0c1e9fe3…-ws-fd5a05e9db36: branch … contains
  commits absent from remote refs`, so a lost workspace lease also leaves a
  worktree nothing reclaims. *Fixed.*

So the Files surface worked for one lease TTL from the claim and then reported
`revoked` to the person, with no warning on the way. A workspace's own
`expires_at` (four hours) and its `idle_timeout_seconds` (1800) are both far
longer, so nothing in the workspace contract predicted this ceiling.

The fix puts the renewal where section 18.1 already said it was — "`POST
.../worker/heartbeat` (renews the lease, carries `state`, `head_sha`,
`capabilities`, `isolation`)". `workspaceService.heartbeat` now writes the
`leases` row in the same transaction as `workspace_sessions`, fenced by the
workspace tuple the endpoint already validates, so there is no second call for
the runner to make and no moment in which the session row and the lease disagree.
The window it writes is the workspace's own `LeaseTTL` rather than whatever the
claim asked for, because that is the window the workspace machine measures
`lease_lost` from; the `released_at IS NULL` guard stays on the statement, so a
heartbeat whose lease has already lapsed is refused with `stale_execution`
instead of resurrecting it. Both deadlines are unchanged: two missed heartbeats
(30 seconds each) still mark `unreachable`, and 90 seconds without one still
fails the workspace with `lease_lost`.

Two consequences were fixed with it. A heartbeat that ends the workspace —
`worktree_missing`, say — now releases the lease rather than renewing it, so a
`closed` or `failed` workspace never leaves a live claim on one of the runner's
capacity slots. And the lane no longer reports a lease the hub has already let
go of as a failure: `ReleaseWorkspaceLease` maps the hub's `stale_fencing_token`
and `lease_not_found` onto the workspace sentinels, and `Lane.release` treats
them as agreement, which is the same rule the unbind already followed. An
orderly close now unbinds and releases under a lease that is still current, so
`workspace.lease_not_released … 409 (stale_fencing_token)` is gone from the
ordinary end of a session.

The last bullet above is fixed too, and separately, because no lease was ever
read on that path: the refusal came from `LocalGit.checkCleanupBranch`
(`internal/workspace/preservation.go`), which retains any `detent/` auto-branch
whose commits are absent from remote refs, and both the runner's
`GitWorktree.Release` (through `CleanupIssue`) and the orchestrator's residual
reconciler run into it, so an orderly close would have left the same orphan.
The rule itself is right for an attempt and wrong for a workspace session: a
fresh session worktree is detached at `head_sha` and commits nothing, so
measuring it against the remotes only reports how far somebody else's commit is
ahead of the remote — in the preview repository, which has no remote, the whole
history.

Section 18.1 already separates the two, so the branch name does now as well. A
session worktree is created on `detent/workspace/<key>` rather than
`detent/<key>` (`Issue.WorkspaceSession`, set by `GitWorktree.issueFor` for
every `worktree: "fresh"` checkout and never for a retained one). Cleanup reads
that namespace back — from the issue, the ownership record, or the branch alone
— and applies the session rule: the worktree goes if it is clean, its branch
holds no commit that lives on no other ref, and it has not committed past the
`head_sha` it was opened on, so the read-only case removes both the worktree
and the branch and leaves nothing for the reconciler to retry. The
attempt rule is untouched: a `detent/<key>` branch with unpushed commits is
still retained, and a `worktree: "retained"` close still removes nothing at all,
because closing a workspace never deletes the attempt's artifacts. The
reconciler also recognises a session worktree that has already detached onto
`head_sha`, which the worktree listing reports with no branch at all, by the
session branch that names its key.

Four smaller notes this run adds, all of them about the curl path rather than
the product:

1. **A work item is moved by `POST {nativeBase}/work-items/:item/workflow`, not
   by `PATCH`.** The body takes `idempotency_key`, `expected_revision` (as a
   JSON **string**, and it must be the item's current revision — `""` or an
   absent field is `422`), `state` and `reason`, where `reason` is one of
   `user_requested`, `worker_progress`, `dependency_ready`. The seeded project's
   states do not allow `Todo → Done` directly: it is two calls, `Todo → In
   Progress → Done`, each with the revision the previous one returned.
2. **The enrollment `operations` vocabulary is `read`, `collaborate`, `claim`,
   `heartbeat`, `events`.** `POST {base}/runner-enrollments` refuses anything
   else with the same opaque `422 invalid_request`, so a guess at
   `["claim","heartbeat","lease"]` reads like a malformed body.
3. **The pre-warm lease window is a real wait, and it was ~100 seconds here.**
   The fixture logged `Preview workspace: … ready` at 09:21:00; `PUT
   {nativeBase}/policy` with the correct `expected_policy_id` answered `409
   policy_mismatch` at 09:21:43, 09:22:01 and 09:22:21, and `200` at 09:22:41.
   Third run item 1 is unchanged; the fourth run simply arrived after it.
4. **The hub reported the runner `offline` for two minutes mid-run**
   (09:29:24–09:31:19, twenty-four `workspace.claim_failed … 409
   (runner_offline)` from the lane) with the daemon healthy and heartbeating
   before and after. It cleared on its own and cost nothing but the delay before
   the second workspace; it is recorded here because a lane that is told
   `runner_offline` looks identical to a lane with nothing to do.

Environment for a follow-up: `<scratchpad>/dogfood/env5.sh`, hub log `hub5.log`,
runner log `runner5.log`, runner configuration `global.yaml` (kept as
`global5.yaml`), the fourth run's daemon state moved aside under
`old-63071-state/`, the Go relay client under `<scratchpad>/relayclient`. Every
work item on the project (#1 to #9) ended `Done`; nothing was left active.

#### Sixth run, September 12, 2026 10:20 UTC

Rebuilt from `0b0f966e` on a fresh preview hub (`http://127.0.0.1:50541`,
project `prj_6ffad8b3f2fa451aaebf619580feaf99`) with a re-enrolled runner, to
prove the one thing the fifth run left open: **a Files workspace that outlives
its lease TTL on the real runner**. It held, by a wide margin — a person relay
sent a files `list` frame every thirty seconds for five minutes and every one
of them answered, on a lease that had by then been alive for nine TTLs. The
ordinary chat → coordinator → link → attempt path ran once more and did the
work. The fix also changes the setup, in a way a follow-up run must know about,
and the run turned up one new defect in the relay's own stream accounting.

**The lease renewal fix held: a workspace lease now advances on every
heartbeat.** A subject item was created (`wi_3a64d0f62a3e406ebe204310854563df`,
#5, moved to `Done` before the request so the issue lane could not claim it) and
a workspace requested against it as the hosted owner with `POST
{nativeBase}/workspaces` (`{"idempotency_key": …, "work_item_id": …,
"requires": ["files"]}`). It answered `201` `state: "requested"` and was `ready`
on the dogfood runner three seconds later:

```
10:31:10Z  POST …/workspaces → 201  ws_11cddb8109256de5e2f764af0d32c147  state=requested
10:31:13Z  GET  …/workspaces/ws_11cd…  state=ready
           runner_id=runner_edc85ac1e67c45cb8eaea158581c57fa
           capabilities={'terminal': False, 'files': True, 'diff': False, 'preview': False}
```

Its lease `c31f5c6c-cef1-40ac-843e-c8f02ac860df` was taken at
`10:31:11.683587Z`, which under the fifth run's behaviour would have killed the
session at `10:32:41`. Sampling the hub's `leases` row every twenty seconds
(`<scratchpad>/dogfood/lease6-samples.txt` and `lease6b-samples.txt`) shows
`expires_at` moving forward on each thirty-second heartbeat and never standing
still:

```
10:32:07Z  renewed=10:31:41.904199Z  expires=10:33:11.904199Z  session ready
10:32:47Z  renewed=10:32:41.906459Z  expires=10:34:11.906459Z  session ready
10:33:47Z  renewed=10:33:41.905291Z  expires=10:35:11.905291Z  session ready
10:34:48Z  renewed=10:34:41.908459Z  expires=10:36:11.908459Z  session ready
10:35:49Z  renewed=10:35:41.912927Z  expires=10:37:11.912927Z  session ready
…
10:43:55Z  renewed=10:43:41.907863Z  expires=10:45:11.907863Z  session idle
10:44:35Z  renewed=10:44:11.906255Z  expires=10:45:41.906255Z  session idle
```

The last renewal before the close was `10:45:41.917737Z`, carrying `expires_at`
to `10:47:11.917737Z` — fourteen minutes and thirty seconds after the claim, on
a ninety-second TTL, with `released_at` empty the whole time. The session never
left `ready`/`idle`: `unreachable`, `lease_lost` and `revoked` appear **zero**
times in the whole run-six runner log, and so does `workspace.heartbeat_failed`.
(The session reports `idle` rather than `ready` from the moment the first relay
connection closes, and stays `idle` while a later connection is actively sending
frames; both are live states, but `ready` does not come back.)

**The Files surface answered across the whole window.** A relay ticket
(`POST …/workspaces/:workspace/relay-tickets`, `expires_in: 30`) was redeemed on
`GET …/workspaces/:workspace/relay?ticket=…` with the hosted session cookie, and
one `list` frame was sent every thirty seconds for eleven rounds — five minutes,
more than three lease TTLs — on a single connection reusing a single stream:

```
10:39:40Z round 1  --> {"channel":"files","payload":{"path":"."},"type":"list"}
10:39:40Z round 1  <-- listed stream=relayconn_5857da5882f44fa49fdf7d5969dae67d:1 seq=1  entries=7
10:40:10Z round 2  --> {"channel":"files","payload":{"path":"."},"stream":"relayconn_5857…:1","type":"list"}
10:40:10Z round 2  <-- listed stream=relayconn_5857…:1 seq=2  entries=7
…
10:44:40Z round 11 --> {"channel":"files","payload":{"path":"."},"stream":"relayconn_5857…:1","type":"list"}
10:44:40Z round 11 <-- listed stream=relayconn_5857…:1 seq=11 entries=7
10:44:40Z done FAILURES=0
```

Every reply was the dogfood worktree — `.git`, `AGENTS.md`, `WORKFLOW.md`,
`detent.yaml`, `go.mod`, `main.go`, `main_test.go` — read off the runner's own
fresh checkout. The fifth run's `{"code":"revoked","message":"Access to this
workspace has changed"}` did not appear once.

**The orderly close is clean, and `lease_not_released` is gone.** `DELETE
{nativeBase}/workspaces/ws_11cd…` at `10:46:04Z` answered `204`; ten seconds
later the session read `closed / closed_by_actor` and its dispatch issue #6
(`wi_28190e7793f3420b813a42c3e38d6fc9`, `detent:workspace`) had gone from `Todo`
to `Done`, `terminal = True`. The lease row shows `released_at
2026-09-12T10:46:04.802078Z` under a still-current expiry, which is what the
fix intends. `workspace.lease_not_released` — the fifth run's `409
(stale_fencing_token)` on every close — appears **zero** times in `runner6.log`,
and no `provider_reservations` row was written against either workspace lease
(`provider_incompatible` is also zero, so the fourth run's fix still holds).

**The orphaned-worktree defect is unchanged, and an orderly close hits it too.**
The close logged the fifth run's last bullet verbatim:

```
10:46:04 WARN workspace.worktree_not_released component=workspace_session
  workspace_id=ws_11cddb8109256de5e2f764af0d32c147
  error=clean up workspace worktree: workspace retained for recovery at
  …/detent_workspaces/dogfood-wi_3a64d0f62a3e406ebe204310854563df-ws-0d21d73bd880:
  branch detent/dogfood-wi_3a64d0f6…-ws-0d21d73bd880 contains commits absent from remote refs
```

and the orchestrator's own reaper picked it up on the next tick, its
`affected_path_count` going from 1 to 2 as this run's worktree joined the set
nothing reclaims (`workspace residual reconciliation failed … removed=0
preserved_skipped=11`). The fifth run predicted exactly this — "the refusal
would happen on an orderly close too" — and it does. It is a separate defect,
and the lease fix does not touch it. Its own fix (`33a4e57c`, the
`detent/workspace/<key>` namespace) landed on the branch seventeen minutes
after this workspace was closed, so this run observed the defect and did not
test the cure; a seventh run should expect `workspace.worktree_not_released` to
be absent from an orderly close and should say so either way.

**New, and since fixed: the pre-warm workspace blocked the policy approval
forever.** The fixture's pre-warmed workspace is now heartbeated by the
fixture's own in-process runner, so its lease is renewed on the same path
everything else is, and `approvePolicy`'s "no unexpired lease in the scope"
condition was never satisfied:

```
10:20:38.797474Z  lease 759e7814-… acquired by the pre-warm workspace
10:22:12Z         renewed=10:21:38.804639Z  expires=10:23:08.804639Z
10:22:28Z         PUT {nativeBase}/policy → 409 policy_mismatch
10:24:40Z         renewed=10:24:38.819014Z  expires=10:26:08.819014Z
10:24:40Z         PUT {nativeBase}/policy → 409 policy_mismatch
```

The third run's item 1 said "wait 90 seconds"; the fourth and fifth runs said
the wait was about a hundred seconds. There was then no wait that ended. This
run worked around it by deleting the pre-warm workspace — `DELETE
{nativeBase}/workspaces/ws_08ba603c3a72f56a573b3fd3411cc172` → `204` at
`10:24:50Z`, which released the lease at `10:24:50.609727Z` — and the same
policy `PUT` answered `200` eight seconds later.

**Fixed.** A workspace session lease is not an execution lease: section 18.1
gives a workspace its own lease so the runner's capacity accounting, renewal
and expiry sweep apply to it with no second mechanism, and nothing runs under
it but a person reading files, a diff or a preview — no model, no gate, no
command — so it holds nothing a policy approval could invalidate. The
approval's unexpired-lease count now excludes leases held on a
`detent:workspace` item (`executingLeaseCountQuery` in
`internal/hubserver/policy.go`) and counts only attempt and claim leases. A
seventh run does **not** need to delete the pre-warm workspace before `PUT
{nativeBase}/policy`, and the third run's item 1 wait is gone with it; a
project with a running attempt still answers `409 policy_mismatch`, which is
the half that has to keep working. Deleting the pre-warm workspace remains
harmless if a run wants the slot back: the fourth run's terminal-state fix
moved that workspace's own item #4 to `Done` in the same breath, so the third
run's "move the pre-warmed `detent:workspace` item to Done by hand" is still
not a step either way.

**New defect, since fixed: a files `list` per frame exhausted the connection's
stream budget after eight frames.** The first attempt at the five-minute proof
sent each `list` with no `stream` field, the way the fifth run's single-shot
client did. The hub allocated a fresh stream for each one and nothing ever
reclaimed it: rounds 1–8 answered, and rounds 9, 10 and 11 came back

```
{"channel":"files","type":"error",
 "payload":{"code":"stream_limit",
            "message":"The stream limit for this connection or workspace has been reached"}}
```

`MaxStreamsPerConnection` is 8 (`internal/workspacesession/frame.go`), so the
ninth allocation was refused, and the connection was then permanently unable to
list anything. The lease was healthy throughout — the refusals are
`stream_limit`, not `revoked`, and the lease row kept advancing — so this is
independent of the lease work, and the whole run of eleven succeeded once the
client reused the stream id the first `listed` frame returned.

**Fixed, in the hub, where a plain client cannot get it wrong.** Section 18.2
now says a stream-less frame on `files`, `diff` or `preview` reuses the stream
that connection already has open on the channel and allocates only when it has
none — those channels are request/response, so a stream-less request is the
same conversation continuing, not a second one. `terminal` is unchanged:
every `open` is its own PTY and its own stream. The second half was the
accounting: `close {stream}` now gives the slot back to the connection
(`closeStream` in `internal/hubserver/workspace_relay.go`), which it never did,
so a connection that closed every stream it opened still ran out of them. A
closed id is never handed out twice — the count that limits streams falls, the
counter that names them does not. A seventh run can send `list` with or
without a `stream` field; both stay on one stream for the life of the
connection. The client was already correct — `workspaceRelay.ts` sends the
first request with no `stream`, holds the second until the hub names one, and
reuses that id — and there is now a test for the nine-round shape that broke
this run.

**The ordinary path did not regress.** A chat created at `10:48:17Z` took a real
runner-dispatched coordinator turn — coordinator item #7
(`wi_dabe3b9ced3f4af2b7ee7ac0cb90623e`), claimed at `10:48:35Z`, finished at
`10:48:44Z`, its answer in the conversation by `10:48:53Z`:

```
worker_conversation_bound  conv=conv_4523c13807f77884d9174e0c3957b48b
  thread_id='' resume='' thread_origin='coordinator' coordinator=True attempt=1
```

The chat was linked to #8 "Make the unit test pass" with `dispatch: now`,
carrying the coordinator's thread `01a0953b-bf23-7af3-8bb3-8e2d34f0932b`. The
issue attempt bound on the same runner two minutes later and did **not** take
that thread — the third run's fix, unchanged:

```
worker_conversation_bound  conv=conv_4523c13807f77884d9174e0c3957b48b
  thread_id='' resume='transcript' thread_origin='worker' coordinator=False attempt=2
```

It opened `01a0953d-9ea5-7fb1-842d-5bb230106d95` of its own, and
`worker_attempt_finished … "mode": "implement", "outcome": "succeeded"` came
thirty seconds after the start. The workspace held exactly the intended change
and nothing else:

```
 main.go | 2 +-
-	return "Hello, " + name
+	return "Hello, " + name + "!"
```

`go test ./...` in that worktree: `ok  example.com/dogfood  0.187s`.

Two differences from the fifth run on this path, both of them section 7's
re-dispatch loop, which was still open at this commit and is fixed since,
rather than anything new. First, the issue was **not** promoted
when the attempt succeeded: the model's own attempt to record completion failed
with the first run's item 4 ("Detent rejects its configured Hub URL"), the issue
stayed `In Progress`, and attempts 3 and 4 were dispatched at `10:52:38Z` and
`10:54:40Z` — both resuming the worker's own thread, both `succeeded`, neither
changing anything, and the fourth one spending its turn opening a question on
the issue. It was moved to `Done` by hand at `11:00:31Z` and the loop stopped.
Second, the coordinator's answer was a refusal to read the repository — "I can't
read `AGENTS.md` in this coordinator run because workspace access is prohibited
and no repository is checked out" — which is the coordinator contract working,
not a failure, but it means the fifth run's "read AGENTS.md" prompt is a poor
smoke prompt for a coordinator turn.

Four smaller notes this run adds, all of them about the curl path:

1. **`POST {base}/runner-enrollments` refuses an `idempotency_key`.** The
   request struct is `runnerauth.EnrollmentRequest` and it has no such field;
   `decodeAPIJSON` sets `DisallowUnknownFields`, so the extra key is
   `422 invalid_request` — the same opaque refusal the fifth run's item 2
   recorded for a wrong `operations` vocabulary. The body is exactly
   `runner_id`, `machine_id`, `project_ids`, `operations`, `ttl_seconds`
   (and optionally `shared_machine`).
2. **`POST {nativeBase}/work-items` needs an explicit `state`.** Omitting it is
   `422 invalid_request`; `"state": "Todo"` creates the item.
3. **`first_message` on `POST {nativeBase}/conversations` takes `key` and
   `text` and nothing else.** A `kind` field alongside them — the shape
   `conversation.Command` uses on the commands route — is `422 invalid_request`.
4. **The hub reported the runner `offline` again for half a minute**
   (`10:46:06Z–10:46:31Z`, eight `workspace.claim_failed … 409 (runner_offline)`
   from the lane) immediately after the workspace was deleted, with the daemon
   healthy and heartbeating on both sides of it. Same shape as the fifth run's
   item 4, shorter, and it cleared on its own.

Environment for a follow-up: `<scratchpad>/dogfood/env6.sh`, hub log `hub6.log`,
runner log `runner6.log` (runner pid in `runner6.pid`), runner configuration
`global.yaml` (kept as `global6.yaml`), the lease samples in
`lease6-samples.txt` and `lease6b-samples.txt`, the two relay transcripts in
`relay6.log` and `relay6b.log`, the fifth run's daemon state moved aside under
`old-run5-state/`, and the looping relay client under `<scratchpad>/relayloop`
(`relayloop <ws-url> <cookie> <rounds> <gap-seconds>`). Every work item on the
project (#1 to #8) ended `Done`; both workspaces are `closed`, every lease is
released, and no attempt is in flight.

#### Screenshots

Written to `<scratchpad>/dogfood/shots/`, in order: `01` Settings → Providers &
runners before enrolment, `02` the Enroll a runner dialog, `03` the one-time
token and the redeem command, `04` the same page with the real runner and its
provider row, `05` the chat's coordinator reply, `06` the Create linked issue
form with the share-history confirmation, `07` the conversation just after
linking, `08` the conversation carrying the runner's messages, `09` and `10`
the steered turn and its reply, `11` the Work board with the running attempt,
`12` the board settled with the issue Done.


#### Seventh run, September 12, 2026 13:19 UTC

Rebuilt from `007ed8d1` on a fresh preview hub (`http://127.0.0.1:60085`,
project `prj_4fe6688481f8493a9d1aae7820fa119d`) with a re-enrolled runner, to
prove the one thing every run so far has had to work around by hand: section
7's **"An item in an active state is re-dispatched forever"**, now ticked. The
run answers that in halves. The hub half holds completely — the loop is gone,
and an item whose latest attempt succeeded is never offered again unless
somebody asks for another attempt, which is exactly what the explicit
continuation does. The promotion half **did not fire at all** on this project,
so the issue ended the run in `In Progress` rather than `Done`: not claimed
again, not re-dispatched, but not promoted either. That half is fixed since —
"Since fixed: a native item is judged on the hub's own facts" below, and
section 7's brake 0. The setup fixes the sixth run reported both held, and the
orphaned-worktree cure landed.

**The loop is gone.** Chat `conv_53b3e2e76ad5170049c738dcef3392b8` was created
at `13:24:55.516403Z`, took a real runner-dispatched coordinator turn on
item #5 (`wi_35343949fcb74e8289c1358b8adf5eaf`, claimed `13:26:03`, finished
`13:26:16`, answer in the conversation at `13:26:13.296099Z`), and was linked
at `13:26:33.030682Z` to #6 `wi_b6975015ea5642cd9d959b6d006ba9f8` "Make the
unit test pass" with `dispatch: now`. The item's own history is the whole
story, and it stops after the run:

```
13:26:33.030682Z  issue.created            revision=1
13:28:08.226373Z  workflow.transitioned    Todo -> In Progress  reason=worker_progress  actor=runner
13:28:09.436971Z  run.started              attempt_658169bbad1e4edd8461a262b2f1a346  fencing_token=9
13:29:00.869500Z  run.finished             outcome=succeeded
                  (aggregate_sequence 6 is the last event on the item)
```

The work was real: `main.go | 2 +-`, `-return "Hello, " + name` /
`+return "Hello, " + name + "!"`, and `go test ./...` in that worktree is
`ok  example.com/dogfood  0.283s`.

From `13:29:00Z` the item was then watched for **fifteen minutes**, sampled
every twenty seconds into `<scratchpad>/dogfood/watch7.txt`, with nothing
touched by hand:

```
13:29:08.188507Z state=In Progress terminal=False rev=2 attempts=1 [attempt_6581:succeeded] exec=completed
13:32:08.421842Z state=In Progress terminal=False rev=2 attempts=1 [attempt_6581:succeeded] exec=completed
13:37:08.666509Z state=In Progress terminal=False rev=2 attempts=1 [attempt_6581:succeeded] exec=completed
13:43:48.875330Z state=In Progress terminal=False rev=2 attempts=1 [attempt_6581:succeeded] exec=completed
```

`GET {nativeBase}/work-items/wi_b697…/attempts` held **one** item for the whole
window, `status: "succeeded"`, and the runner log holds exactly one
`worker_attempt_started` for the item:

```
08:28:08.243 worker_attempt_started wi_b6975015ea5642cd9d959b6d006ba9f8 #6 work_attempt_id 2
08:29:00.871 worker_attempt_finished wi_b6975015ea5642cd9d959b6d006ba9f8 #6 outcome=succeeded
```

The claim polls returned nothing for the whole window — the runner's own
dispatch summary at `13:42:39Z`, fourteen minutes after the success, reads
`candidate_count: 0, eligible_candidate_count: 0, selected_count: 0,
seconds_since_last_selected: 956`, and `last_selected_at` is still the original
`13:28:07Z`. The hub-side reason is the new clause's two columns, exactly as
designed:

```
native_attempts  fencing_token=9  status=succeeded  work_item_revision=2  dispatch_generation=0
issues           wi_b697…         revision=2        dispatch_generation=0
```

The sixth run's second and third claims, at two-minute intervals, have no
counterpart here. That is the loop, closed.

**The explicit continuation still dispatches, exactly once.** A `message` was
posted to the linked conversation at `13:44:12.602Z` while no attempt was live
(`POST {nativeBase}/conversations/:conversation/commands`,
`{"key": …, "kind": "message", "text": …}` → `200 status: "queued"`).
`requestNativeDispatch` wrote the request down rather than inferring it:

```
issues  wi_b697…  revision=2  dispatch_generation=1  dispatch_requested_at=2026-09-12T13:44:12.613262Z
```

and one attempt followed, thirty seconds later, on the worker's own thread:

```
08:44:42.160 worker_attempt_started     wi_b697…  #6
08:44:42.722 worker_conversation_bound  resume='thread' thread_origin='worker' coordinator=False
08:45:20.220 worker_attempt_finished    outcome=succeeded
native_attempts  fencing_token=10  status=succeeded  work_item_revision=2  dispatch_generation=1
```

Its answer is in the conversation at `13:45:16.506737Z` ("`go test ./...`
passes. Changed only `main.go`, adding the missing `!`, and committed as
`8c16011`."), and the total for the run is **three** `worker_attempt_started`
lines — the coordinator turn and one per requested dispatch. Nine minutes after
that attempt finished the item was still unclaimed, `candidate_count: 0`.
The generation guard therefore does both halves of its job: it suppresses the
loop and it honors a continuation that bumps no revision.

**What did not happen: the promotion.** The item never left `In Progress`, and
no promotion was attempted — `completed issue promotion lane substituted`,
`completed issue has no promotion lane`, `completed issue review transition
failed` and `read project workflow states failed` are all **absent** from
`runner7.log`, so `resolveCompletedActiveReviewTarget` was never reached and
the three-state fallback to `Done` was never exercised. The reason is upstream
of the fallback, in `completedActiveReviewTargetState`
(`internal/orchestrator/completion_transition.go`): it asks
`completedActiveIssueReadyForReview(issue, gateRequiresPullRequest(cfg.Gate),
operationalCompletionAccepted)`, `gateRequiresPullRequest` is true for every
gate kind except `artifact` (`internal/orchestrator/autopromote.go`), and that
predicate answers `false` when `issue.PullRequest == nil`. A hub-native item
never has one: `issueFromNative` (`internal/hubclient/native_scheduler.go`)
sets no `PullRequest` field at all. So on a native project with a `command`
gate and no pull request the target is `""`, the loop `continue`s in silence,
and `transitionTimedOutCompletedActiveGateWait` cannot rescue it either —
it needs `autoPromoteActiveGatePendingIssue`, which requires auto-promote
enabled (`auto promote skipped … reason=disabled` is what this project logs).

This is not the fallback failing; the fallback is never asked. Section 7's
brake 1 as written — "the promotion has to land somewhere" — is proven only for
a project whose completed items are ready for review, and a dogfood project
with no remote and no pull request is not one. The practical consequence is
milder than the loop it replaced: the item is inert rather than claimed
forever, so a run no longer burns a Codex turn every two minutes. It is still
a row on the board that looks like outstanding work and is not, which is
exactly the complaint the fourth run's "closing a workspace left its work item
in Todo" fix answered for workspace items. **#6 was deliberately left in
`In Progress` at the end of this run** so the state is inspectable; it is not
re-dispatching.

**Since fixed: a native item is judged on the hub's own facts.** Section 7's
brake 0. The readiness rule no longer demands a pull request from a tracker
that cannot produce one: the work item resource carries an optional change
review surface (`?include=change`) whose `connector` says `none` for a project
with no GitHub repository bound, and whose `revision` is the work item revision
the latest succeeded attempt's stored diff or change version covers. With no
connector, that recorded change is what "ready for review" means and the gate's
pull-request requirement does not apply; with a connector, the mirrored pull
request must still be open. `issueFromNative` carries the change request onto
`connector.Issue.PullRequest`, and the orchestrator reads the surface at
promotion time through `connector.ChangeReviewHydrator`, because the issue it
holds was read when the attempt was dispatched and the attempt's change is
newer than it. On this project's shape the target then resolves through brake
1's fallback and the item reaches `Done`. A completed item that is still not
ready is reported once per item — `completed issue not ready for promotion`,
`missing=pull_request | open_pull_request | change_request` — so the next run
reading `runner*.log` does not have to read
`completedActiveReviewTargetState` to find out why nothing moved. An eighth
run should expect #6's shape to end in `Done` with one attempt, and should
report the skip line either way.

**Since fixed, and confirmed: the pre-warm workspace no longer blocks the
policy approval.** No delete, no wait. The fixture's pre-warm workspace
`ws_3e29d814c4254b2f989fc3ea710128db` took its lease at `13:19:53.943638Z` and
kept it, unreleased, through the approval and for another twenty-eight
minutes (`released_at 13:49:53.970862Z`):

```
13:19:53.943638Z  lease 8a96fb35-… acquired by the pre-warm workspace (never released until 13:49:53)
13:21:42.176934Z  PUT {nativeBase}/policy  sent
13:21:42.253282Z  PUT {nativeBase}/policy  -> 200  approved_by=hosted_935a55b7…  (81 ms, first try)
```

The sixth run's `409 policy_mismatch` forever is gone, and so is the third
run's ninety-second wait.

**Since fixed, and confirmed: an orderly workspace close no longer orphans its
worktree.** A subject item was created and closed first (#7
`wi_825d418e123a470f927bc2ea727ed8e7`, `Todo → In Progress → Done`), and a
files workspace requested against it:

```
13:48:31Z  POST …/workspaces -> 201  ws_1f6828ff26a1dd537b1b7b93b688dd9e  state=requested
13:48:40Z  GET  …/workspaces/ws_1f68…  state=ready  runner=runner_14ef7be2…
           capabilities={'terminal': False, 'files': True, 'diff': False, 'preview': False}
13:50:28.746990Z  DELETE …/workspaces/ws_1f68… -> 204
13:50:40Z  state=closed  reason=closed_by_actor; subject item #7 Done, terminal=True
```

`workspace.worktree_not_released` appears **zero** times in `runner7.log` —
the sixth run asked a seventh run to say so either way, and the
`detent/workspace/<key>` namespace (`33a4e57c`) holds: `git worktree list` in
the dogfood repository gained no entry for `wi_825d418e…`, and the workspace
lease `d363404b-9da7-4037-a494-33de5886a071` was released at
`13:50:31.346265Z` under a still-current expiry. `workspace.lease_not_released`,
`workspace.heartbeat_failed`, `lease_lost`, `unreachable` and
`provider_incompatible` are all zero as well.

**Since fixed, and confirmed: a stream-less files `list` reuses the one
stream.** Eleven `list` frames were sent eight seconds apart, every one of them
with **no `stream` field**, using a copy of the relay client that never sends
one (`<scratchpad>/relayloop7`). All eleven answered on a single stream and
`stream_limit` never appeared:

```
13:48:57Z round 1  --> {"channel":"files","payload":{"path":"."},"type":"list"}
13:48:57Z round 1  <-- listed stream=relayconn_715eb6a9502547d8b329c14187c8f320:1 seq=1  entries=7
13:50:02Z round 9  --> {"channel":"files","payload":{"path":"."},"type":"list"}
13:50:02Z round 9  <-- listed stream=relayconn_715eb6a9…:1 seq=9  entries=7
13:50:18Z round 11 <-- listed stream=relayconn_715eb6a9…:1 seq=11 entries=7
13:50:18Z done FAILURES=0
```

The sixth run's ninth frame died with
`{"code":"stream_limit"}`; this run's ninth frame is `seq=9` on the same
stream. Every reply was the runner's own fresh checkout — `.git`, `AGENTS.md`,
`WORKFLOW.md`, `detent.yaml`, `go.mod`, `main.go`, `main_test.go`.

Five smaller notes this run adds, all of them about the curl path:

1. **`POST {nativeBase}/conversations` takes `key`, not `idempotency_key`, and
   has no `visibility` field.** `conversationCreateRequest` is exactly `key`,
   `title` and `first_message`; `decodeAPIJSON` sets `DisallowUnknownFields`,
   so `{"idempotency_key": …, "visibility": "private"}` is
   `422 invalid_request` with the usual "Request body is invalid". A new
   conversation is private either way. The sixth run's note about
   `first_message` taking only `key` and `text` is unchanged.
2. **`GET {nativeBase}/work-items/:item/attempts` answers `{"items": [...]}`,
   not `{"attempts": [...]}`,** which is easy to read as "no attempts" from a
   script. `?include=attempts` on the work item itself is ignored — attempts
   are only on the subresource.
3. **`POST {nativeBase}/workspaces` answers the workspace with `id`, not
   `workspace_id`;** `work_item_id` is the item. The sixth run's own note said
   `POST work-items` needs an explicit `state`, which is still true (`"state":
   "Todo"`).
4. **The fixture owner already holds the runner grant** on
   `prj_4fe6688481f8493a9d1aae7820fa119d`
   (`GET {base}/members` shows `write: true, runner: true`), so the third run's
   grant step was not needed this run. The other project in the organization
   has `runner: false`, and enrolment naming only the one project is accepted.
5. **The hub reported the runner `offline` again in two short bands**
   (`13:25:48Z–13:26:03Z` and `13:50:48Z–13:50:58Z`, `workspace.claim_failed …
   409 (runner_offline)` from the workspace lane, with
   `409 (host_capacity)` while an attempt held the one slot), with the daemon
   healthy and heartbeating on both sides. Same shape as the fifth and sixth
   runs, shorter, cleared on its own.

One thing unchanged from the first run: the model still cannot record
completion. Its own closing message on the first attempt was "Could not update
the native Workpad: Detent CLI failed", the first run's item 4, and that is
independent of the promotion finding above — the orchestrator's promotion is
what moves the lane, and it never ran.

Environment for a follow-up: `<scratchpad>/dogfood/env7.sh`, hub log
`hub7.log`, runner log `runner7.log` (runner pid in `runner7.pid`), runner
configuration `global.yaml` (kept as `global7.yaml`), the loop samples in
`watch7.txt`, the relay transcript in `relay7.log`, the hub database copies
`hubdb7-a.db` (just after the policy approval) through `hubdb7-final.db`, the
sixth run's daemon state moved aside under `old-run6-state/`, and the
stream-less relay client under `<scratchpad>/relayloop7`. Every work item on
the project ended `Done` except #6, which was left in `In Progress` on purpose
as the promotion evidence; the workspace is `closed`, every lease is released,
and no attempt is in flight.

#### Eighth run, September 12, 2026 16:52 UTC

Rebuilt from `6d736e4e` on a fresh preview hub (`http://127.0.0.1:56060`,
project `prj_94e663045af94c66b80703c6f8612419`) with a re-enrolled runner, to
settle the half the seventh run left open: the promotion. It did **not** fire,
and the reason is not the one the seventh run's "Since fixed" note predicted.
Brake 0 landed and is working — it is the new warn line that names the miss —
but the fact it is judging on this hub is not the one the note expected: the
browser preview fixture **binds a GitHub repository to the dogfood project**,
so `?include=change` answers `connector: "github"`, never `"none"`, and the
rule's no-connector half is unreachable on this fixture. With a connector the
rule keeps demanding a pull request, the item has none, and it stays in
`In Progress`. Everything else the run exercised held: one attempt and no
re-dispatch for fourteen minutes, one continuation attempt on request, a
policy approval that no longer waits on the pre-warm lease, a clean workspace
close, and a project action run end to end on the real runner.

**The chat and the link.** `conv_a1b1f98868e37c8f6a34aa8f2a318d27` was created
at `17:02:29.040Z`, took a real runner-dispatched coordinator turn on item #5
(`wi_d0cf312305c34c89ad002abe15719f89`, claimed `17:03:17.299`, finished
`17:03:37.606`, answer in the conversation at `17:03:31.685Z`), and was linked
at `17:03:52.709681Z` to #6 `wi_5b8c94c3688345f5844d5df82f99b578` "Make the
unit test pass" with `dispatch: now`. The item's own history:

```
17:03:52.709681Z  issue.created          revision=1
17:05:22.837123Z  workflow.transitioned  Todo -> In Progress  reason=worker_progress  actor=runner
17:05:23.833476Z  run.started            attempt_9528911d6be4e6f20ddd66f10b0bbe04  fencing_token=9
17:06:07.665002Z  run.finished           outcome=succeeded
                  (aggregate_sequence 6 is the last event on the item)
```

The work was real: `main.go | 2 +-`, `-return "Hello, " + name` /
`+return "Hello, " + name + "!"`, and `go test ./...` in that worktree is
`ok  example.com/dogfood  0.314s`.

**The promotion did not fire, and this time it says so.** Thirty-nine
milliseconds after `run.finished`, `runner8.log`:

```
2026-09-12T12:06:07.746395-05:00 WARN completed issue not ready for promotion
  issue_id=wi_5b8c94c3688345f5844d5df82f99b578
  identifier=prj_94e663045af94c66b80703c6f8612419#6
  state="In Progress" missing=pull_request
  change_review_provider=github pull_requests_available=true
  change_revision=2 issue_revision=2
```

Once, for the whole run, which is what "once per item" says it should be. The
seventh run had to read `completedActiveReviewTargetState` to learn the same
thing; this run read one log line. That half of brake 0 is proven.

The surface it judged on:

```
GET {nativeBase}/work-items/wi_5b8c94c3688345f5844d5df82f99b578?include=change
  "revision": "2",            <- the item's own revision
  "change": {"connector": "github", "revision": "2"}
```

Two facts in that answer, and the second is the finding:

1. `change.revision` is `2` and the item's revision is `2`, so the recorded
   change does cover the item as it stands. Had the connector been `none`,
   `ChangeAtCurrentRevision` would have been true and the item would have
   promoted through brake 1's fallback into `Done` — the shape the seventh
   run's note predicted.
2. `connector` is `"github"`. The browser preview fixture seeds a repository
   and binds it (`UPDATE projects SET repository_id = ?,
   github_repository_enabled = 1 WHERE id = ?`, `hosted_browser_test.go`) so
   the Pull request surface has the seeded PR #4211 to show. `projectConnector`
   therefore answers a binding, `applyNativeChangeReview` sets
   `PullRequestsAvailable: true`, and `completedActiveIssueReadyForReview`
   skips the change-only branch entirely and requires a pull request.

And the change surface carries no `change_id`: the attempt posted a stored
diff, which is where `change.revision = 2` comes from, but no change request
row exists, so `applyNativeChangeReview` returns before it fills
`issue.PullRequest` and the rule answers `missing=pull_request`.

So **brake 0's native path cannot be exercised on the browser preview fixture
at all**, because the fixture's only runner-granted project has a GitHub
connector bound to it. A ninth run that wants to prove the promotion needs
either a hosted project with `github_repository_enabled = 0` (the fixture's
other project, `prj_6c975016e47f4c209047c4a1a0a68d83`, has no runner grant) or
a rule that asks whether this item's tracker can mirror *this change*, rather
than whether the project has a connector at all. The practical shape is worth
naming: a hosted project may hold a GitHub connector and still run its work on
a local checkout with no remote, and today that combination never promotes.

**Since fixed** (September 12, 2026, the same day): the rule asks the second
question now. A completed attempt is ready for review when the item carries a
change at its current revision, **whether or not the project has a
connector**, and a pull request that was opened for the item must still be
open — a merged or closed one is not ready, exactly as before. The
pull-request requirement survives only where something actually opens one: the
`issueFromWorkItem` GitHub-profile path, where the runner opens it itself and
no change review surface exists, and an item that already has one. The gate
configuration has no auto-PR setting to re-arm it with, so nothing else does
(`internal/gate`). The reason opening a pull request cannot be waited for is
decisions section 18.6: it is the explicit
`POST {nativeBase}/work-items/:id/pull-requests/actions {action: open}`,
executed by the merge lane and offered to a person as the header's Create PR
button, and the model holds no hub credential for it. So the item this run
watched would now promote through brake 1's fallback into `Done` on its
attempt diff alone, with no warn line. Decisions section 9.2.2 carries the
amended rule and the `missing=` vocabulary it logs; section 7's brake 0 above
carries the same amendment. `TestConversationDispatchLoopSettlesWithAGitHubConnector`
(`internal/hubclient`) is this run's hub end to end — a project with
`repository_id` bound and `github_repository_enabled = 1`, an attempt that
posts its diff and opens nothing, and the item reaching `Done` — and it fails
with exactly this run's `readiness = (false, "pull_request")` when the old
rule is put back. **A ninth run can therefore prove the promotion on the
browser preview fixture as it stands**, with no second project and no
connectorless hub.

**One attempt, and no second claim for fourteen minutes.** From `17:06:07Z`
the item was sampled every twenty seconds into `<scratchpad>/dogfood/watch8.txt`
with nothing touched by hand:

```
17:06:25.425225Z state=In Progress terminal=False rev=2 attempts=1 [attempt_9528:succeeded] exec=completed change={"connector": "github", "revision": "2"}
17:13:45.962612Z state=In Progress terminal=False rev=2 attempts=1 [attempt_9528:succeeded] exec=completed change={"connector": "github", "revision": "2"}
17:20:26.305308Z state=In Progress terminal=False rev=2 attempts=1 [attempt_9528:succeeded] exec=completed change={"connector": "github", "revision": "2"}
```

`GET {nativeBase}/work-items/wi_5b8c…/attempts` held **one** item,
`status: "succeeded"`, for the whole window, and `runner8.log` holds exactly
two `worker_attempt_started` lines for the run so far — the coordinator turn
and this one. The seventh run's closed loop stays closed.

**The explicit continuation still dispatches, exactly once.** A `message` was
posted at `17:20:33.781894Z` while no attempt was live
(`POST {nativeBase}/conversations/:conversation/commands`,
`{"key": …, "kind": "message", "text": …}` → `200 status: "queued"`), and one
attempt followed seventy-six seconds later:

```
12:21:49.836 worker_attempt_started   prj_94e663045af94c66b80703c6f8612419#6
12:23:00.416 worker_attempt_finished  outcome=succeeded
attempts: attempt_9528911d6b succeeded fencing=9 | attempt_d1408fc42f succeeded fencing=10
```

Its answer is in the conversation at `17:22:46.757Z` ("`go test ./...` passes.
Exact last line: `ok  example.com/dogfood  0.207s`. Committed as `7039c78`;
worktree is clean."). Three `worker_attempt_started` lines for the whole run —
the coordinator turn and one per requested dispatch. **The §10.17 question the
eighth run was asked to answer — what a message on a *Done* conversation does —
could not be asked**, because the item never reached `Done`. What is proven is
the seventh run's answer again, on an idle item in an active lane: one message,
exactly one new attempt.

**A project action ran end to end on the real runner, but not in two calls.**
A files workspace was opened against #6
(`ws_feafcd18417047e0d6fda4eac38c1fd1`, `requested` at `17:23:31.902024Z`,
`ready` four seconds later on `runner_05996544fbdb49a188dd3f0afb30bd97`, with
`capabilities={'terminal': False, 'files': True, 'diff': False,
'preview': False, 'exec': True}`), then:

```
POST {nativeBase}/actions            -> 201  action_df24a95c79f34b558f0044a42e105e38  {"name":"echo","command":"echo hello"}
POST {nativeBase}/actions/:id/runs   -> 202  {"run_id": "actionrun_052b43e900f947689cce0ecfe81625ec"}
GET  .../runs/:run                   -> 200  status=queued ... for three minutes forty seconds
```

`createProjectActionRun` says why in its own comment: "the row is queued, and
the process starts when a client opens the exec channel for it". The runner
lane only runs a project's actions at worktree creation
(`runCreationActions`); nothing on the runner polls for a queued run. Opening
the exec channel is what starts it — a relay ticket
(`POST .../workspaces/:workspace/relay-tickets`) redeemed on
`GET .../workspaces/:workspace/relay?ticket=…` and one frame:

```
17:27:11Z --> {"channel":"exec","type":"run","payload":{"run_id":"actionrun_052b…","action_id":"action_df24…","command":"echo hello"}}
17:27:11Z <-- {"channel":"exec","stream":"relayconn_db157c5f…:1","type":"output","seq":1,"payload":{"data":"hello\n"}}
17:27:11Z <-- {"channel":"exec","stream":"relayconn_db157c5f…:1","type":"exited","seq":2,"payload":{"code":0}}
```

and the run row then reads `status: "succeeded"`, `exit_code: 0`,
`started_at 17:27:11.944389Z`, `finished_at 17:27:11.950382Z`,
`output_bytes: 6`, with `GET .../runs/:run/output` answering `hello\n`.

**Since fixed: a queued run no longer needs a browser.** The hub now hands a
run still `queued` past a one-second grace period to the runner already holding
its workspace, on its own maintenance tick: it opens a stream of its own on the
runner relay connection and sends the same `run` frame a person would have sent
(`dispatchQueuedActionRuns`, `internal/hubserver/project_action_dispatch.go`).
Nothing on the runner is new — the frame is the frame whoever opened the stream,
so the run gets the same lease validation immediately before the process starts,
the same 1 MiB cap and the same kill-on-lease-loss — and the hub records it from
the frames it relays exactly as it recorded the relayed run above. The two REST
calls that sat here for three minutes and forty seconds are now an action run:
`POST …/actions/:action/runs` → 202, and the row is `succeeded` within a tick.

The two executors cannot collide. `claimed_by` (migration 00032) is written in
the same transaction that moves a run out of `queued`, so the first claim wins
and the second — a person's late `run` frame, or a second dispatch — is refused
with `already_running` rather than starting a second process in one worktree.
The grace period is what keeps a browser the preferred executor for the runs it
starts, because only its own stream shows the output as it is produced.

The client half went with it: the Output surface reads an action's runs and
their recorded output back while it is open, so a run nobody watched is visible
with its output rather than as a status with an empty box.

**Since fixed, and confirmed again: the pre-warm workspace does not block the
policy approval.** The fixture's pre-warm workspace
`ws_b32fcad7163ac286d478b73bd3575681` was created at `16:52:48.828787Z` and
was still open at `16:53:47Z` when `PUT {nativeBase}/onboarding/policy`
answered `200` on the **first** try in **3 ms**. No delete, no wait.

**Since fixed, and confirmed again: an orderly workspace close releases
everything.** `DELETE …/workspaces/ws_feafcd18…` answered `204` at
`17:27:26Z` and the workspace read `closed / closed_by_actor` twelve seconds
later. `workspace.worktree_not_released`, `workspace.lease_not_released`,
`workspace.heartbeat_failed`, `lease_lost` and `provider_incompatible` are all
**zero** in `runner8.log`.

Nine smaller notes this run adds, eight of them corrections to the runbook the
sixth and seventh runs wrote:

1. **The current policy descriptor is `GET {nativeBase}/policy`, not
   `GET {nativeBase}/onboarding/policy`,** which is `404 not_found`. Only the
   approval is on the onboarding path (`PUT
   {nativeBase}/onboarding/policy`), and `expected_policy_id` must be the id
   that `GET …/policy` reports — the seeded `policy_04c9b137…`, not `""`.
   `GET {nativeBase}/onboarding` nests the same descriptor under `policy`.
2. **`POST {base}/runner-enrollments` takes the binding inline.**
   `runnerauth.EnrollmentRequest` embeds `Binding`, so `runner_id` and
   `machine_id` are top-level fields beside `project_ids`, `operations` and
   `ttl_seconds`. Nesting them under `"binding"` is the same opaque
   `422 invalid_request` a wrong operations vocabulary gives.
3. **`detent hub runner init` refuses an identity directory that is not
   `0700`** — "runner identity directory must be private (0700)" — so
   `chmod 700` the directory before the first `init`.
4. **`Todo → Done` in one hop is allowed on the seeded project.** The
   project's own `states[].transitions` lists `Done` under `Todo`, and
   `POST …/work-items/:item/workflow` with `state: "Done"` answers `200`. The
   sixth run's note 1 — "it is two calls" — no longer holds; the rest of that
   note (POST not PATCH, `expected_revision` as a JSON string) does.
5. ~~**A queued action run needs the exec channel.** `POST
   {nativeBase}/actions/:action/runs` answers `202` and the row stays `queued`
   forever on its own; two REST calls are not an action run.~~ **Since fixed:**
   two REST calls are an action run. The hub hands a run still queued past a
   one-second grace period to the runner holding its workspace and records it
   from the frames it relays, so a headless caller polls `GET …/runs/:run` and
   reads `succeeded` within a tick. The exec channel is still how a person
   watches the output arrive; see the action section above for the frame and
   the "Since fixed" note beside it.
6. **The runner needs `--port 0` here.** `127.0.0.1:4000` was occupied, and
   the daemon exits with "bind Detent web listener 127.0.0.1:4000: address
   already in use" rather than falling back.
7. **`GET .../work-items/:item/attempts` items carry no
   `work_item_revision` or `dispatch_generation`.** Those two columns are brake
   2's comparison and are readable only from the hub database; the API's
   attempt resource has `fencing_token`, `status`, `outcome`, `usage` and the
   checkpoint.
8. **A relay ticket's lifetime is the hub's, not the caller's.**
   `POST …/workspaces/:workspace/relay-tickets` with `{"expires_in": 60}`
   answered `{"expires_in": 30}`. Workspace capabilities now also report
   `exec`.
9. **The hub reported the runner `offline` in short bands again**
   (`workspace.claim_failed … 409 (runner_offline)` thirteen times, against
   seventy-six `409 (host_capacity)` while an attempt held the one slot), with
   the daemon healthy and heartbeating. Same shape as the fifth, sixth and
   seventh runs, and it cleared on its own.

One thing unchanged from the first run: the model still cannot record
completion. Its closing message on the first attempt was "Couldn't update the
Detent Workpad: the CLI reports an invalid Hub URL", the first run's item 4,
and it is independent of the promotion finding above — the orchestrator's
promotion is what moves the lane, and it declined to.

Environment for a follow-up: `<scratchpad>/dogfood/env8.sh`, hub log
`hub8.log`, runner log `runner8.log` (runner pid in `runner8.pid`), runner
configuration `global.yaml` (kept as `global8.yaml`), the promotion samples in
`watch8.txt`, the exec transcript in `exec8.log`, the exec relay client under
`<scratchpad>/execclient8`, and the seventh run's daemon state moved aside
under `old-run7-state/`. No hub database copies this run: `sqlite3`'s backup
contends with the live preview hub and hangs, so copy it only after the hub is
stopped. Every work item on the project ended `Done` except #6, which is left
in `In Progress` as the promotion evidence; the workspace is `closed`, every
lease is released, and no attempt is in flight.

#### Ninth run, September 12, 2026 23:19 UTC

Rebuilt from `644b6250` (one formatting commit past `deb28615`) on a fresh
preview hub (`http://127.0.0.1:64550`,
project `prj_b99cb51931ff4f8c870b1b266fa213f4`) with a re-enrolled runner, to
settle the half the eighth run left open — the promotion — and to exercise the
one surface no run had touched: the header's git action group over the real
relay. **Both held.** A completed issue reached `Done` by itself 112
milliseconds after its attempt finished, with no warn line and nothing touched
by hand, and `status` / `commit` / `push` ran on the customer runner's worktree
with a real commit authored by the signed-in person and a real push to the
project's remote. A queued project action run was also dispatched headlessly by
the hub, in two REST calls, exactly as the eighth run's "Since fixed" note
promised.

**The chat and the link.** `conv_c92323f2f63819f9bf55ba431b857cea` was created
at `23:51:13.128681Z`, took a real runner-dispatched coordinator turn on item
#5 (`wi_669653489468425fa7dd012b5ffec370`, started `23:51:56.183`, finished
`23:52:03.015`, answer in the conversation at `23:52:00.763212Z`: "Fix the
failing unit test."), and was linked at `23:52:15.905335Z` to #6
`wi_88d1f1eec39b4b87b917ddd76f43f51d` "Make the unit test pass" with
`dispatch: now`.

**The promotion fired, by itself.** The item's whole history, seven events:

```
23:52:15.905335Z  issue.created          revision=1              actor=human
23:53:56.610875Z  workflow.transitioned  Todo -> In Progress     revision=2  reason=worker_progress  actor=runner
23:53:58.053636Z  run.started            attempt_920042e52bc01e03d06d1923e515732f  fencing_token=10
23:54:39.360632Z  run.finished           outcome=succeeded
23:54:39.472177Z  workflow.transitioned  In Progress -> Done     revision=3  reason=worker_progress  actor=runner
                  (evt_ff4681ccb7874de39257c8602b2ebc5f, aggregate_sequence 7)
```

`run.finished` to `Done` is 111.5 ms. The surface it judged on is the one the
eighth run recorded as unreachable:

```
GET {nativeBase}/work-items/wi_88d1f1eec39b4b87b917ddd76f43f51d?include=change
  "state": "Done", "terminal": true,
  "revision": "3",
  "change": {"connector": "github", "revision": "2"}
```

Still `connector: "github"` — the browser preview fixture still binds a
repository to the dogfood project — and still no pull request anywhere on the
item (`GET …/work-items/:item/pull-requests` → `[]`). The amended rule asks
whether the item carries a change at its current revision rather than whether
the project has a connector, so the attempt's own diff was enough. The runner
log says so in two lines and **no** `completed issue not ready for promotion`
line appears anywhere in the run:

```
2026-09-12T18:54:39.448821-05:00 INFO completed issue promotion lane substituted
  issue_id=wi_88d1f1eec39b4b87b917ddd76f43f51d
  identifier=prj_b99cb51931ff4f8c870b1b266fa213f4#6
  state="In Progress" configured_state="Human Review" target_state="Done"
2026-09-12T18:54:39.488557-05:00 INFO completed issue review transition
  issue_id=wi_88d1f1eec39b4b87b917ddd76f43f51d
  identifier=prj_b99cb51931ff4f8c870b1b266fa213f4#6
  from_state="In Progress" target_state="Done"
```

Brake 1's lane substitution is what names `Done`: the configured target is the
literal `Human Review`, the three-state project has no such lane, and the
substitution reaches the terminal state instead of leaving the item where the
sixth run's four re-dispatches started.

The work was real: `main.go | 1 +, 1 -`, `-return "Hello, " + name` /
`+return "Hello, " + name + "!"`, `patch_bytes: 285`, and the model's closing
message is "Changed only `main.go` to append the missing `!`. `go test ./...`
passes."

**One attempt, and no second claim for eleven minutes.** From `23:54:39Z` the
item was sampled every twenty seconds into `<scratchpad>/dogfood/watch9.txt`
with nothing touched by hand — thirty-three consecutive samples:

```
23:54:50.637813Z state=Done terminal=True rev=3 attempts=1 [attempt_920042e5:succeeded] change={"connector": "github", "revision": "2"}
00:05:31.025918Z state=Done terminal=True rev=3 attempts=1 [attempt_920042e5:succeeded] change={"connector": "github", "revision": "2"}
```

`GET {nativeBase}/work-items/wi_88d1…/attempts` held **one** item,
`status: "succeeded"`, for the whole window, and `runner9.log` holds exactly
**two** `worker_attempt_started` lines for the entire run — the coordinator turn
and this one.

**The git action group, end to end on the real runner.** A workspace was opened
against the now-`Done` #6 with `requires: ["files", "git"]`
(`ws_86592e775afeab69b55a5babfa5715e2`, `requested` `23:55:44.125974Z`,
`ready` `23:55:49.633543Z` — five seconds — on
`runner_8ea65397db1a42cb916dac5afe2cb848`) and the resource carried what
section 18.13 requires of it:

```
"capabilities": {"terminal": false, "files": true, "diff": false,
                 "preview": false, "exec": true, "git": true},
"machine_hostname": "Michaels-MacBook-Pro.local",
"worktree_path": "/private/var/folders/…/detent_workspaces/dogfood-wi_88d1f1ee…-ws-be7c59e8b305"
```

Over the person relay (`POST …/workspaces/:workspace/relay-tickets` redeemed on
`GET …/workspaces/:workspace/relay?ticket=…`), the transcript:

```
23:56:06.007Z --> {"channel":"git","type":"status","payload":{}}
23:56:06.422Z <-- status  branch=detent/workspace/dogfood-wi_88d1f1ee…-ws-be7c59e8b305
                          detached=false remote=origin upstream=true
                          ahead=0 behind=0 dirty_file_count=0 head_sha=122b0ee4…

  (a project action ran here, see below, writing NOTES.md)

23:56:35.473Z <-- status  … ahead=0 dirty_file_count=1 head_sha=122b0ee4…
23:56:35.473Z --> {"channel":"git","type":"commit","payload":{"message":"dogfood: notes"}}
23:56:36.057Z <-- committed {"commit":"4f44f57e7486e17aa19ebaca1b8916eb446c1b29",
                             "branch":"detent/workspace/dogfood-wi_88d1f1ee…-ws-be7c59e8b305",
                             "files":1}
23:56:37.630Z <-- status  … ahead=1 dirty_file_count=0 head_sha=4f44f57e…
23:56:37.935Z --> {"channel":"git","type":"push","payload":{}}
23:56:37.935Z <-- pushed  {"branch":"detent/workspace/dogfood-wi_88d1f1ee…-ws-be7c59e8b305",
                           "remote":"origin","commit":"4f44f57e7486e17aa19ebaca1b8916eb446c1b29"}
```

Every frame reused one stream (`relayconn_9271d81d9fbe4182a716d7fe639ed959:1`,
`seq` 1 through 5) with no `stream` field sent, which is 18.2's stream reuse.
The commit is in the remote and says who decided and who ran it:

```
$ git --git-dir=<remote>.git show -s --format='%H %an <%ae> / %cn <%ce>' 4f44f57e
4f44f57e…  Owner <owner@example.test> / Detent runner (Michaels-MacBook-Pro.local) <runner@detent.invalid>
  NOTES.md | 1 +
```

The author is the signed-in owner's own email, with the name derived from the
local part exactly as 18.13 says (there is no display name in the hosted
schema); the committer is the machine. The push landed
`refs/heads/detent/workspace/dogfood-wi_88d1f1ee…-ws-be7c59e8b305` at
`4f44f57e` on the project's remote.

`GET {nativeBase}/work-items/wi_88d1…/pull-requests` answered `[]`, and the
Create PR half — 18.6's action, not a channel frame — answered `202`:

```
POST {nativeBase}/work-items/wi_88d1…/pull-requests/actions
     {"action": "open", "expected_head_sha": "4f44f57e…", "idempotency_key": …}
202 {"work_item": {… "work_item_id": "wi_ce1fac2e76f345f6ab777b418aeaa3bb",
                   "number": 8, "title": "Pull request open",
                   "labels": ["detent:pull-request-action"], "state": "Todo"},
     "action_id": "pra_c4c6aa00b14d42e395b8f7ee657ad025"}
```

That is a merge-lane work item for a runner with a checkout to execute, which is
18.6's contract; it was parked in `Done` rather than dispatched, because this
project's remote is a local bare repository with no GitHub behind it.

`DELETE …/workspaces/ws_86592e77…` answered `204` and the
workspace read `closed / closed_by_actor` at `23:57:13.868547Z`, a second later.
`workspace.worktree_not_released`, `workspace.lease_not_released`,
`workspace.heartbeat_failed`, `lease_lost` and `provider_incompatible` are all
**zero** in `runner9.log`.

**A queued action run, headless, in two REST calls.** Confirming the eighth
run's "Since fixed" note with no relay client anywhere near it:

```
POST {nativeBase}/actions          -> 201 action_d712ee53d30f409ebd04e67f3c20722c
                                          {"name":"touch","command":"echo dogfood >> NOTES.md"}
POST {nativeBase}/actions/:id/runs -> 202 {"run_id":"actionrun_e1d10f249c614aa68bd3bfbaa016e835"}
GET  .../runs/:run  at +0.0s       -> 200 status=queued
GET  .../runs/:run  at +3.0s       -> 200 status=succeeded exit_code=0
                                          started_at 23:56:26.506169Z
                                          finished_at 23:56:26.533292Z
                                          output_bytes=0
```

The hub's maintenance tick handed the run to the runner already holding the
workspace and recorded it from the frames it relayed; the next `git status`
frame above is the independent proof the process really ran in that worktree
(`dirty_file_count` 0 → 1). `output_bytes: 0` is correct for this command: the
echo is redirected into the file, so the process writes nothing to stdout.

**Since fixed, and confirmed again: the pre-warm workspace does not block the
policy approval.** `PUT {nativeBase}/onboarding/policy` answered `200` on the
first try in **10 ms**, with the fixture's pre-warm workspace
`ws_b7fa217333929cb99321cf823d932e21` open.

Four notes this run adds to the runbook, two of them corrections:

1. **`POST …/work-items/:item/workflow` requires `reason`.** Without it the
   answer is `422 invalid_request` — "The requested operation is unavailable",
   the same opaque sentence a wrong state gives. `{"state": "Done",
   "expected_revision": "1", "reason": "user_requested",
   "idempotency_key": …}` answers `200`. The eighth run's note 4 is otherwise
   intact: POST not PATCH, `expected_revision` as a JSON string, and
   `Todo → Done` in one hop.
2. **`GET {nativeBase}/work-items` returns `{"items": […]}` whose items key the
   id as `work_item_id`, not `id`,** and carry `number` rather than an
   `identifier` string. The same is true of the workflow response. Neighbouring
   listings do not agree on the envelope key either: work items and attempts
   answer `{"items": […]}`, conversation messages answer `{"messages": […]}`
   and workspaces answer `{"workspaces": […]}`.
3. **The dogfood repository had no git remote,** so `push` would have answered
   `git_failed` with "This worktree has no remote to push to" — the
   `ErrNoRemote` refusal, correct but not a proof. A local bare repository
   (`<scratchpad>/dogfood-remote.git`) was added as `origin` before the runner
   started, and every worktree the runner cuts inherits it through the shared
   repository configuration. **`ahead` does not return to 0 after a push**:
   the workspace branch's upstream is `origin/main` (the branch is cut from
   main and never given one of its own), so `ahead=1` after the push is the
   distance from `origin/main`, not a failed push. A header that reads "1
   ahead" off a successful push is reading a true number about a different
   ref, and 18.13's `Upstream` flag does not distinguish the two.
4. **The hub reported the runner `offline` in short bands again**
   (`workspace.claim_failed … 409 (runner_offline)` six times, against
   thirty `409 (runner_capacity)` while an attempt held the one slot; the
   code is `runner_capacity` on this build, not the eighth run's
   `host_capacity`), with the daemon healthy and heartbeating. Same shape as the
   fifth through eighth runs, and it cleared on its own. New this run and
   harmless: `workspace live diff stat failed … unsafe workspace path:
   /…/T/detent-admission-… is not safe under /…/T/detent_workspaces` three
   times during the coordinator turn, which runs in an admission temp directory
   rather than a workspace worktree.

One thing unchanged from the first run: the model still cannot record
completion. Its closing message was "Couldn't update the Workpad: Detent CLI
reports an invalid Hub URL. Detent owns the Done transition." — and this run is
the proof of the second sentence.

Environment for a follow-up: `<scratchpad>/dogfood/env9.sh`, hub log
`hub9.log`, runner log `runner9.log` (runner pid in `runner9.pid`), runner
configuration `global.yaml` (kept as `global9.yaml`), the promotion samples in
`watch9.txt`, the git transcripts in `git9-status1.log` and
`git9-transcript.log`, the relay client under `<scratchpad>/relayloop9` (argv:
`ws_url cookie frame_json…`), and the eighth run's daemon state moved aside
under `old-run8-state/`. Every work item on the project ended `Done`, including
#6; the workspace is `closed`, every lease is released, and no attempt is in
flight.
