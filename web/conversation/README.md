# Detent conversation client

The React application the hub serves at `/chat`. It implements the client half
of [`docs/conversation/decisions.md`](../../docs/conversation/decisions.md) and
follows [`docs/conversation/design-inventory.md`](../../docs/conversation/design-inventory.md)
for its visual system.

Third-party attribution is in [THIRD_PARTY_NOTICES.md](./THIRD_PARTY_NOTICES.md);
the upstream MIT license is in [LICENSE.t3code](./LICENSE.t3code).

## Interface

The conversation client provides Detent's sidebar, issue pages, composer,
command palette, transcript, and workspace panels. Adapters in `src/app/adapters/`
connect the shared UI components to hub payloads and commands.

[`src/app/global.css`](./src/app/global.css) defines the theme,
animations, and markdown styles. [`src/app/index.css`](./src/app/index.css)
sets Detent's fonts and primary color. Component-facing types live in
[`src/contracts/ui.ts`](./src/contracts/ui.ts).

Imported components and runtime packages retain their upstream attribution.
See [third-party notices](./THIRD_PARTY_NOTICES.md) for MIT and Apache-2.0 attribution.

## Build

```sh
make app        # npm ci when needed, then vite build
make app-test   # tsc --noEmit && vitest run
make app-dev    # vite dev server, proxying to a local hub
make check-app  # typecheck, tests, rebuild, bundle drift and attribution
```

`make check-app` is the branch gate and runs inside `make check`
(decisions.md §10.15). After the typecheck and the unit tests it rebuilds the
bundle and fails if `static/app/conversation` changed — the built client is
committed, so a client change that was not rebuilt is drift — and fails if
`app.js` lost its MIT attribution banner. The build is deterministic (fixed
output names, no content hashes, no timestamps), so two builds of the same tree
are byte identical and the drift check means what it says.

`make generate` runs `make app`. The output lands in
`static/app/conversation/` (`index.html`, `app.js`, `app.css`, fixed names, no
content hashes) and is committed, following the precedent of
`static/css/output.css`, so `go build` never needs Node.

`app.js` begins with the MIT notice for the reused T3 Code source
(decisions.md §10.13). It is prepended by the `detent-mit-attribution` plugin
in `vite.config.ts`, which includes the complete license texts from
[`LICENSE.t3code`](./LICENSE.t3code) and
[`LICENSE.pierre-diffs`](./LICENSE.pierre-diffs). Check it with
`head -n 30 ../../static/app/conversation/app.js`.

## Development

```sh
npm run dev:mock   # mock hub + vite together
npm run dev        # vite only; set DETENT_HUB_URL for a real hub
```

The dev server proxies `/api` and `/chat/bootstrap` to
`$DETENT_HUB_URL` (default `http://127.0.0.1:4100`).

`dev/mock-hub.ts` is an in-memory double for the hub: bootstrap, create, list,
snapshot, message pages, an SSE stream with cursors and heartbeats, idempotent
commands, issue linking, and a scripted coordinator that streams a canned reply
in deltas. It also exposes `POST /__mock/*` control endpoints used by the tests
to inject failures (`queue-full`, `unknown-outcome`, `drop-open-streams`,
`expire-cursors`, `revoke-access`, `server-error`, `bad-frame`, `reset`) and to
switch modes (`coordinator`, `account`). It is a development and test double,
not a reference implementation.

It implements the corrections in decisions.md §10: `POST /conversations`
requires a top-level `key` and is idempotent by it, the command envelope
accepts `retry`, the conversation carries `message_count`, the execution
carries `resume`, archive and unarchive filter the list, and `account`
`read_only` reports every project as `can_write: false` and refuses every
mutation with `403 forbidden`.

### Coordinator modes

Every model turn runs on a customer runner, including the coordinator turn that
answers an unlinked chat (decisions.md §1 "Where model turns execute", §9). The
mock hub runs either side of that transition, so the client can be developed
against both without a runner:

| Mode | An unlinked `message` | `capabilities.coordinator` |
|---|---|---|
| `hub` (default) | Answered in place by the scripted hub coordinator; the receipt is `delivered` | true |
| `runner` | `queued`; execution goes `waiting_for_runner` until a runner binds | true |
| `none` | `queued`, and nothing will answer it | false |

Set the mode with `startMockHub({ coordinator })`, the `MOCK_COORDINATOR`
environment variable (`npm run dev:mock` reads it), `POST /__mock/coordinator
{"mode": "runner"}`, or a `?coordinator=runner` query on `/chat/bootstrap`,
which flips a running hub from the browser. The mode is sticky and `reset`
restores the one the hub was started with. `makeHarness({ coordinator })`
passes it through in tests.

In `runner` mode the runner hooks below drive the coordinator attempt on an
unlinked chat instead of an attempt on a linked issue: `start` binds `att_N`
and streams the answer, `complete` closes it, and `complete` also posts the
`propose_issue` card as a `role: system`, `kind: status` message when the reader
asked for an issue — which is what a runner's `item` turn event with
`data.proposal` becomes (decisions.md §9.3, §9.4). `cancel` goes `interrupting`
→ `interrupted`; the client-facing kind stays `cancel` (§9.1).

A linked conversation — and, in `runner` mode, an unlinked chat — gets a
scripted runner, driven from the same control surface so every execution status
can be reached without a runner, a lease or a provider:

| Hook | What it does |
|---|---|
| `POST /__mock/runner/<conversation>/start` | Binds a new attempt (`att_N`), moves `starting` → `running`, and streams a reply in deltas into an open assistant message |
| `POST /__mock/runner/<conversation>/question` | Opens a question anchored to a new status message and moves to `waiting_input` |
| `POST /__mock/runner/<conversation>/complete` | Closes the open message and moves to `completed` |
| `POST /__mock/runner/<conversation>/start` with `{"resume": "transcript"}` | Binds as above, reports `resume: "transcript"` and appends the §10.4 status message |
| `POST /__mock/runner/<conversation>/lose` | The worker unbound: every handed-out control becomes `unknown` and the execution with it (§10.3) |

The command endpoint follows the same script: `answer` marks the question
answered and resumes the attempt (a second answer is `409
question_already_answered`), `interrupt` goes `interrupting` → `interrupted`,
`continue` records intent and reports `waiting_for_runner`, and any control
whose `expected.attempt_id` is not the current one is `409 stale_execution`.
Linking emits a status message carrying `data.issue` so the issue result card
is history rather than a client-side memory, and a message containing "create
an issue" makes the coordinator answer with a `data.proposal` status message.

## Layout

| Path | What it is |
|---|---|
| `src/contracts/` | Effect Schema definitions for every wire resource, plus the shared fixtures |
| `src/runtime/connection/` | The T3 connection supervisor and registry, unmodified |
| `src/runtime/rpc/` | Detent adapters: same-origin HTTP, the SSE transport, and the session the supervisor manages |
| `src/runtime/state/` | Conversation list and detail atoms, the pure reducer, and draft storage |
| `src/components/ui/` | T3 Code's primitive set, byte-identical |
| `src/components/` | T3's breadcrumb and composer surface, byte-identical |
| `src/lib/`, `src/hooks/` | T3's `cn`, visible-animation observer and media-query hooks, byte-identical |
| `src/app/` | Router, shell, components, and the two stylesheets (`global.css` copied, `index.css` Detent's overrides) |
| `src/app/account/` | Login, organization, project settings and the first-run wizard, with the account API client and its hooks |
| `src/app/settings/` | T3's settings layout and navigation, and the `/settings` page |
| `src/app/fleet/` | Fleet and spend: the hero number, the chart, the totals and the host cards |
| `src/app/work/` | The Work board, the list, the issue page, the changes list and the review dock |
| `dev/` | The mock hub, and the native work API's own mock |
| `tests/` | Runtime integration tests against the mock hub, and component tests |

`tests/components/app.test.tsx` mounts the whole shell against the mock hub and
carries one chat through handoff, a runner and a question. It is the only test
that exercises the wiring between the router, the atoms and the components;
everything else is either a runtime test with no DOM or a component test with
no runtime.

## The account screens

The React application is the whole hosted frontend (decisions.md §11), so the
screens that were hosted Templ pages are routes here. `routes.account.tsx`
exports `accountRoutes(rootRoute)` — a builder rather than a tree, because a
TanStack route binds to its parent at construction — and `router.tsx` spreads
the result into the root's children.

Since decisions.md §17.3 every one of them except the login card and the wizard
is a section of T3's settings page, and the paths that used to be screens of
their own are redirects into the matching section.

| Route | Screen | Reads |
|---|---|---|
| `/login` | The artifact's wizard card with the two ways in and the invitation token | nothing; it is the one route the hub serves without a session |
| `/settings` | Redirects to `/settings/general` |  |
| `/settings/general` | The organization this session acts on, who the actor is, the plan summary, sign out | the bootstrap |
| `/settings/organization` | Members, roles, project grants, invitations, the switcher and the support banner | `GET /members` |
| `/settings/projects` | The project list and New project | `GET /projects` |
| `/settings/runners` | Provider capacity folded across the fleet, and the host cards | `GET /fleet` |
| `/settings/integrations?project=` | The repository binding, the GitHub integration and the approved policy, one project at a time | `GET {nativeBase}/integration`, `GET {nativeBase}/policy` |
| `/settings/plan` | The effective plan and its allowances | `GET /plan` |
| `/settings/billing` | The subscription, the portal and the configured prices | `GET /billing` |
| `/settings/keybindings` | The shortcuts this client binds | nothing |
| `/settings/about` | The build the hub reports, its API base and the T3 attribution | the bootstrap |
| `/usage` | T3's usage page: Cost / Tokens / Limits / Runners over a 24h, 7, 30 or 90 day window | `GET /usage?range=` |
| `/projects/:project/setup` | The artifact's screen 5 over the four onboarding steps | `GET {nativeBase}/onboarding` |
| `/organization` | Redirects to `/settings/organization` |  |
| `/fleet` | Redirects to `/settings/runners` |  |
| `/projects/:project/settings` | Redirects to `/settings/integrations?project=` |  |

The settings navigation is T3's, in the place T3 puts it: the window's
sidebar. Their `AppSidebarLayout` swaps the thread sidebar for
`components/settings/SettingsSidebarNav.tsx` on a settings route, so the page
itself is one content column under the breadcrumb, the way their
`routes/settings.tsx` draws it. The nav is their whole file — the search row
with its `/` shortcut and its result listbox, the flat icon+label rows with
their active pill, and the footer's "Back" row out of settings — over Detent's
section list.

It carries the five T3 sections Detent Cloud does not serve — Appearance,
SnapShots, Source Control, Connections and Archive — present and disabled
behind a tooltip, because §16 says a T3 feature stays until Michael decides to
pull it out. Plan and billing are the opposite case: they are not rendered at
all for somebody who could only ever be refused them, because a section that
can only say "you cannot see this" is noise.

Three rules run through all of them.

**The bootstrap is read once.** `/app/bootstrap` is a superset of
`/chat/bootstrap` (§12), so `loadBootstrap` fetches the wider path, falls back
to the alias on a 404, and decodes the same payload through both schemas. The
account screens read `client.account`; it is `null` against a hub that has not
moved yet, and every screen treats that as "this hub cannot answer that" rather
than inventing an actor.

**Read-only means no control, not a disabled one.** A viewer sees every value
and none of the affordances that would mutate it (§10.11). Where the hub
refuses anyway — a `403` or a `404` on a project the reader cannot reach — the
screen says so in a sentence rather than showing a stack trace.

**A refused mutation speaks next to the control that asked for it.** The
last-owner rule is the clearest case: the client offers the removal and reports
what the hub said, rather than reimplementing the owner count and disagreeing
with it after a concurrent change.

### Revisions and idempotency

Every non-GET carries an `idempotency_key`. Most screens mint one per intent
(`newKey()`); the first-run wizard persists them, because it is the one screen
where a reader can lose the tab between a request and its answer.
`src/app/account/idempotency.ts` reproduces what `static/js/hosted-setup.js`
did: a key belongs to one `(request path, request body)` pair, lives in
`sessionStorage` under `detent-setup:<path>:<body fingerprint>`, is reused by
every retry of the identical body, and is deleted only once the hub has
answered. Editing a field changes the fingerprint, which is a different command
and a new key. `sessionStorage` rather than `localStorage`: the hub scopes its
replay table by hosted session id, so a key that outlived the tab would replay
against a session that no longer exists.

Saves that carry a revision — the project integration, the onboarding progress,
runner routing — never retry with a fresher number after a `409`. In hosted
mode `nativeAPIError` redacts the current revision from the conflict, so there
is nothing to heal with; the screen re-reads and shows the reader what is
actually stored before they decide again.

### Contract ambiguities resolved

§12 names some payloads by their Go type rather than by their JSON. Where that
left a choice, this is what was chosen and why:

- **`GET /billing` carries `prices`.** The report §12 names
  (`hubserver.hostedBillingReport`) has no price list; the hosted Templ page
  read the configured prices from `HostedBillingConfig` separately. "Checkout
  buttons per configured price" needs them on the payload, so `BillingReport`
  adds `prices: [{id, label}]` and an optional `can_checkout`.
- **Onboarding steps have no ids.** `onboarding.Evaluate` names each step and
  the name is its identity, with exactly two states. The wizard's stepper reads
  `{name, state, detail}` and fills in any step the hub did not send, in
  `Evaluate`'s order.
- **`GET /projects` carries a readiness summary, not the whole onboarding
  project.** The list needs "is this set up" and "what is left"; the wizard
  reads the full `GET {nativeBase}/onboarding` when it opens.
- **Revisions are strings where Go marshals them as strings.** `revision` on
  the integration and on the onboarding progress are JSON strings
  (`json:",string"`); runner routing's `expected_revision` is a number. The
  client echoes each back in the shape it arrived in and never parses one.
- **Member `status` is a string, not an enumeration.** The hub stores an
  `active` flag; anything other than `"active"` renders as disabled rather than
  failing to decode.
- **"Nothing is approved" has two shapes.** `GET {nativeBase}/policy` answers
  `404` where the project has no policy row and `409 policy_mismatch`
  (`policyMismatch` in `internal/hubserver/policy.go`) where the stored
  approval no longer describes what the host resolves. The hosted preview
  answers the second one for a fresh project, so the screen reads both as
  "nothing is approved" and offers the approval rather than an error.
- **`GET {organization}/runners` answers `404` for a reader without the grant.**
  On these routes `nativeNotFound` is the permission status, not `403`, so the
  wizard turns that `404` into the hosted setup page's own sentence — "Runner
  access changed. Reload this project." — rather than "that is not available".
- **`version` is optional in the bootstrap.** The hosted preview does not send
  one, so the About row says the hub does not report a version rather than
  showing an empty field.
- **The invitation form on `/login` needs a session.** §12 puts
  `POST /invitations/accept` under the organization API. With a session the
  card posts it and follows `next`; without one it hands the token to the
  hub's own `/invite` route, which §12 keeps.


## Usage

`/usage` displays one hub usage report. `GET {apiBase}/usage?range=24h|7d|30d|90d&project=` answers everything
the page draws — decisions.md §17.5 — and `src/app/usage/adapter.ts` is the
only Detent code in the path: it fetches, decodes against
`src/contracts/usage.ts`, and reshapes the report into exactly the `merged`
value T3's components already read (`costUsd`, `totalTokens`, `byProvider` as a
`Map`). Nothing in the copied components knows where the numbers came from.

Four tabs. Cost and Tokens are T3's. Limits is T3's tab over Detent's numbers:
T3 shows each connected provider's own rate-limit windows, and what limits a
hosted organization is its entitlement, so the report's `limits` are drawn one
allowance per meter. Runners is the tab §17.5 adds — one row per runner for the
window, with what it was busy doing, how much of its capacity that was, and
what it cost — through the same table component as the Breakdown.

Cost is an estimate from the per-model price table the hub keeps
(`usage.prices`), and the hero says so: "N sessions · API estimate", as T3 says
it.

### Usage ambiguities resolved

- **A 24-hour window's buckets are instants.** §17.5 defines one `daily` array.
  T3 keeps calendar days and rolling hours as two arrays, because its chart and
  its breakdown label them differently. The hub resolves the window, so for
  `range=24h` its `daily` entries carry an ISO instant in `day` and the adapter
  splits them into T3's `hourly`; anything without a `T` is a calendar day.
- **Only cost is per-provider, per period.** `daily[].by_provider` is a cost
  map. The Tokens chart needs a token series per provider, so the adapter
  splits each period's tokens by that period's own cost share — the one
  division that keeps the chart adding up to the Totals grid.
- **`share` is the hub's, `tokenShare` is the client's.** §17.5 sends a
  provider's share of cost. The Tokens tab labels a share of tokens, which the
  adapter computes from the same payload rather than asking for a second field.
- **`limits` is a map, not a list.** §17.5 writes "allowances from the
  entitlement with used/limit", which is the shape `hubserver.HostedEntitlement`
  already has; the client sorts it by name so two reads of one window do not
  reshuffle the rows.
- **`currency` is optional.** §17.5 writes cost as a number. The hub prices in
  one currency at a time, so the field is optional and the client defaults to
  USD.
- **Providers are named by the hub.** T3's presentation table is keyed by its
  own closed union. Detent reads it through `presentationFor`, so a provider id
  this client has never heard of draws a muted row with its id as the label
  rather than crashing the page.

The old `/fleet` screen has been split by this: every number measured over a
window — the spend hero, the daily chart, the totals and the spend-by-project
breakdown — is `/usage`, and what is left (which providers have capacity, which
hosts are enrolled, what they are holding now) is the Providers & runners
section of settings.

## The execution strip on an unlinked chat

The strip above the composer is one component with two vocabularies
(`src/app/lib/execution.ts`). A linked conversation is an issue a runner works
on; an unlinked chat is a question a runner answers, and it gets the strip for
every execution status but `idle` — from the moment a coordinator work item
exists, "is anything happening?" has an answer the reader is owed. The chat
vocabulary never says "issue": waiting names the project a runner has to be
enrolled in, `starting` and `running` both read "A runner is answering this
chat", and the one control is Stop, which sends `cancel` (decisions.md §9.1).

**The strip offers no model, provider, cost or key choice, on either surface.**
Every turn runs on the customer's own runner with that runner's own provider
login or API key; the hub never holds credentials and Detent does not resell
tokens (decisions.md §1). There is nothing here for the reader to pick, and a
picker would promise a choice the product does not have.

A chat that is queued, being answered, stopping or broken also takes the
sidebar lane a linked issue in the same state would (`issueLane`). Lanes are
about execution state, not about whether an issue exists.

## Keyboard, headings and the narrow drawer

Three contracts from the design inventory are implemented in `src/app/lib/` and
covered by both jsdom and browser tests, because each one fails silently:

- **`drawerFocus.ts` — the narrow drawer (B.12, B.14).** Below 768px the
  sidebar is off-canvas. A closed drawer is `inert` *and* `visibility: hidden`,
  not merely `translateX(-100%)`: a transform hides it from the eye and leaves
  every row a tab stop behind the transcript. Opening it moves focus to its
  first focusable element, Tab and Shift+Tab cycle inside it, and `Escape`
  closes it and returns focus to the toggle. `aria-hidden` plus `hidden` is the
  fallback where `inert` is not implemented. The contract is the same with the
  slide animating and with `prefers-reduced-motion: reduce`.
- **`shortcuts.ts` — two shortcuts, no new controls (B.13 rule 4, B.14).** `/`
  focuses the sidebar search unless focus is in an editable field, and
  `Mod+Shift+N` starts a new chat in the current project context by calling the
  same handler the compose pencil calls — B.13 allows a shortcut to duplicate
  the one create action, not a second button. Both are advertised through
  `aria-keyshortcuts` on the controls that own them and spelled out in a
  visually hidden hint (`#dc-shortcut-hint`).
- **`Mod+K` opens T3's command palette.** The binding is `commandPalette.toggle`
  in `@t3tools/contracts`, read through the same keybinding adapter the copied
  `AppSidebarLayout` reads `Mod+B` from, so it is T3's shortcut rather than a
  second table. The palette is mounted above the shell — where T3 mounts theirs,
  in `routes/__root.tsx` — and makes the window behind it `inert` while it is
  open. Escape closes it and returns focus to wherever it came from: the sidebar
  search box, a board row, the composer. Rows that Detent cannot serve stay on
  the list, greyed and `aria-disabled`, rather than disappearing (decisions.md
  §16).
- **One `h1` per route.** On a conversation route the breadcrumb's current
  segment is the heading; on the new-chat route the hero headline is, and the
  breadcrumb carries an `h2` instead — which is T3's own arrangement, their
  chat header title being an `h2` under the hero's `h1`. Sidebar sections and
  in-transcript cards are level two, and Markdown headings in a reply start at
  level two, so a message never contends with the route for the page's name.

The composer's prompt is a Lexical editor, so it is a `contenteditable` with
`role="textbox"` rather than a `<textarea>`: it is reached by its accessible
name ("Message"), it reports text through `innerText` rather than `value`, and
"disabled" is `contenteditable="false"`. `tests/components/composerInput.ts`
is the seam the jsdom tests type through.

## Browser cover: keyboard, screen-reader names and axe

`tests/visual/conversation.spec.js` (in the repository root, run by
`make visual-e2e`) drives this client in Chromium against a **real hosted hub**,
not the mock: `tests/visual/hosted-hub.js` starts the Go preview test
`TestHostedBrowserPreview` with `DETENT_HOSTED_BROWSER_PREVIEW=1`, waits for the
`Hosted browser fixture: <path>` line, reads the JSON it writes (hub URL, the
`/chat` URL, one per-account login URL, and the id of a conversation already
linked to an issue), signs in as the owner and stops the fixture through its
`stop` URL at the end. Everything is driven from the keyboard and located by
accessible name — Tab, Enter, Shift+Enter, Space, `getByRole` — so a control a
keyboard or a screen reader cannot reach fails the test. Each view is also
scanned with `@axe-core/playwright` and any serious or critical violation fails,
as does `page-has-heading-one` at any impact.
Run it with `make app` first (the spec needs the built client) and then
`npx playwright test tests/visual/conversation.spec.js`; add
`npx playwright install chromium` on a machine that has no browser yet.

## Bundle size

| File | Size | Gzip |
|---|---:|---:|
| `app.js` | 2,833 kB | 890 kB |
| `app.css` | 329 kB | 45 kB |
| `chunks/*.js` | 134 files, ~6.5 MB | fetched on use |

Porting the interface roughly doubled the JavaScript — it was 562 kB (178 kB
gzipped) when the UI was hand-written — and took the CSS from 16.7 kB to
168 kB. The JavaScript growth is Lexical, Base UI, `react-markdown` with its
unified and remark graph, and the lucide icons; the CSS is T3's whole token
system and prose scope, which is plain CSS and so is not tree-shaken. Both are
the price of the port being a port.

Mounting T3's whole timeline and markdown took `app.js` from 1,032 kB to
2,833 kB. That is `@legendapp/list`, `@pierre/diffs` and Shiki's core — real
diff rendering and real syntax highlighting, where before there was neither.
The grammars are not in that number: they are 134 chunks, fetched the first
time a code block in that language is rendered, and the entry only carries the
map. Shiki uses its web bundle.

Nothing here is on a first-paint path the hub blocks on, and 45 kB of gzipped
CSS is not worth cherry-picking by hand — cherry-picking is exactly what would
make it drift from upstream.

## Performance budgets

`tests/perf.test.ts` builds a 2,000-message history through the mock hub and
times the two hot paths with `performance.now()`. Measured on an Apple silicon
laptop: decoding the snapshot and the full older page through the Effect Schema
and merging them into one `ConversationDetail` takes **about 7ms** for 2,000
messages, and assembling a 500-delta streaming burst takes **under 1ms**. The
committed budgets are 500ms and 200ms — they exist to catch an accidental
quadratic, not to police milliseconds. The same file asserts that navigating
between two conversations three times leaves exactly one open detail
subscription, so a leak shows up as a failing test rather than as a slow tab.

## Shared contract fixtures

`src/contracts/fixtures/*.json` holds one realistic payload per resource in
decisions.md §5. `src/contracts/fixtures.ts` binds each file to its schema and
`src/contracts/fixtures.test.ts` fails if a file has no binding. **The Go
implementation is expected to decode the same files**, so this directory is a
stable cross-language contract surface: keep the path, and add a fixture rather
than changing one when the shape grows.

### Fixture shapes, and one that is not the wire shape

Every fixture is the exact JSON body of its endpoint, with one deliberate
exception: `event-*.json` files are `{"type": "...", "data": {...}}`. On the
wire a frame carries the type in the SSE `event:` field and only `data` in the
body, so the Go side should compare its event payload against the fixture's
`data` member and its event name against `type`. The wrapper exists because the
client's `ConversationEvent` schema is a discriminated union and needs the
discriminator inside the value it decodes.

### The account fixtures

`account-*.json` covers §12: the extended bootstrap in both its plain and its
support-session shape, the members and invitations, the project list, the
integration, the policy approval, the onboarding project and its progress
response, the runner enrollment, the fleet in both its spending and its
no-spend shape, the plan report and the billing report. The mock hub serves
these shapes, so the runtime tests do not wait on the hub.

### The usage fixtures

`usage-30d.json`, `usage-24h.json` and `usage-empty.json` cover §17.5: a
realistic 30 calendar days across two providers and six models, the same
generator's 24 rolling hours, and the organization that has run nothing. They
are generated by `usageReport()` in `dev/mock-hub.ts` from a fixed clock and a
seeded jitter, so the fixtures, the mock hub and a screenshot cannot disagree;
regenerate them by calling that function rather than editing the JSON.
`POST /__mock/usage {"usage": null}` switches the mock hub to the empty
report.


## The Work board, the list and the issue page

`src/app/work/` implements artifact screens 1 and 2 (design inventory A.1 and
A.2) against the hub's native work API. Four routes:

| Route | What it is |
|---|---|
| `/work` | Every project the bootstrap payload lists, as one board with the project dot on each card |
| `/work/p/:projectId` | One project's board |
| `/work/i/:workItemId` | One issue: its description, its resources, its activity, its composer and its properties — and its conversation, in the right panel |
| `/work/changes` | The Browse group's Pull requests destination |

### The issue is the page (decisions.md §19)

The issue page is where a linked conversation now lives. Three routes say so:

| Path | Where it lands |
|---|---|
| `/chat/issues/:workItem` | `/work/i/:workItem` — a route-level redirect; the id is in the path, so nothing has to be read to resolve it |
| `/chat/c/:conversation`, linked | `/work/i/:workItem?panel=conversation` — the panel opens on arrival |
| `/chat/c/:conversation`, unlinked | stays a chat |

The sidebar's thread rows and the command palette's conversation rows link
straight to the destination rather than bouncing through the redirect;
`src/app/lib/conversationDestination.ts` is the one place that rule is written,
and the redirect is what catches a pasted link.

**The page.** The main column is the identifier and the title, the description
through the app's markdown component, "Add sub-issues" (disabled with its
reason: the hub has no parent-child relation), the Resources list, the Activity
feed and a composer. The properties sidebar is on the right at 300px and
**yields its width to the right panel**: while the conversation is open the
column narrows to 640px and the properties move to a disclosure at the top of
it, so the issue stays readable beside a 540px chat.

**The Activity feed** is merged client-side in `src/app/work/lib/activity.ts`,
because the hub serves no merged projection. Four sources:

| Source | What it contributes |
|---|---|
| `GET .../work-items/:item/history` | created, edited, moved, dependency changed, change opened and published, and the log's own run rows |
| `GET .../work-items/:item/attempts` | one "claimed the issue and started attempt N" row per attempt, and one ending row per attempt that ended — the attempt record is where the status, the outcome and the runner live, and the log's run rows fold behind the disclosure as "the same thing, in the log's words" |
| `GET .../work-items/:item/comments` | comment cards, each with an inline reply that posts one more comment (the hub's comments have no parent, so the footer does not pretend to a thread) |
| the linked conversation | questions asked and answered, steering sent to an open turn, interrupts, and attachments |

Runs of low-value rows fold into "Show N events…" as Linear does, from two
rows up: a disclosure over one line is worse than the line. A `comment.created`
event whose comment is already drawn as a card is dropped rather than folded —
the card is directly below it.

**The live row** is the conversation, as one row. While an attempt runs it
wears T3's `animate-status-pulse` dot, the runner's current sentence (the
latest assistant text, or the execution copy when there is none), the elapsed
time and Interrupt; when nothing runs it is the last turn's summary. Either
way "View conversation" opens the `conversation` surface in the right panel,
and so do the panel toggle and `rightPanel.toggle` — on a page that has a
conversation the toggle opens *it* rather than T3's empty surface launcher.

**The bottom composer** has two modes. Comment (the default) posts a work item
comment; Send to runner sends a `message` to the current turn with `expected`,
exactly as the chat composer does, and is offered only while an attempt is
actually running — there is no turn to steer otherwise, and the panel's own
composer is where a queued follow-up belongs. Attachments are runner-mode only:
a comment carries no files.

The view lives in the query string, not in component state:
`?view=board|list`, `q`, `state`, `label`, `assignee`, `priority`, `sort` and
`lanes`. A filtered board is therefore a link, the back button undoes a filter,
and a reload lands where it left. `localStorage` remembers the last view **per
project**, and only fills in when the reader arrives with no query string at
all — a shared link always wins over a remembered preference
(`src/app/work/lib/viewState.ts`).

Moving a card is optimistic and conflict-aware. The card lands in the new lane
immediately; the transition goes to
`POST .../work-items/:item/workflow` with a fresh idempotency key and the
revision the card was rendered from; a `409 revision_conflict` puts the card
back, reloads the board and says so in a notice. **A hosted `409` carries no
`current_revision`** — `nativeAPIError` strips it and replaces the message — so
re-reading is the only correct recovery and this client does not try to guess.
Drag is a pointer gesture, so every card also carries the same move as a menu
(`LaneMenu`), reachable and driveable from the keyboard alone.

### What the hub does not serve, and what this client does instead

Every gap below is a real absence in the native API today, not a shortcut. Each
is named at the code that works around it.

| What the artifact shows | What the API has | What this client renders |
|---|---|---|
| A per-issue progress bar | Nothing. `NativeAttempt` has no progress field | No bar. The design inventory already records the artifact's own `.bar` rule as dead CSS (A.1); with no number behind it, a bar would be decoration |
| `1.2M tok` on the worker strip | No token usage anywhere on the work API | Omitted. The strip names the model, the backend, the runner and the elapsed time it computes from `updated_at - started_at` |
| `medium · full access` on the worker strip | `identity` carries `role`, `backend` and `model` only | Omitted for the same reason |
| `1 / 2 slots · 312 tps · $28.77 notional today` | Fleet figures; no work endpoint serves them | Replaced by what the client does know: how much of the board it read, and how many issues it could ask about a worker and a change |
| A changed-file list and a unified diff | No file list, no hunks, no per-file counts. A version's code is one opaque artifact (`uri`, `sha256`, `availability`), and `viewed-files` stores only digests, so the hub never learns a file name | The Diff tab shows the round, the head, the base, the merge base, the repository, the artifact and its digest, links out to the pull request, and says plainly that the hub serves no file list |
| A project-wide review queue | `GET .../work-items/:item/changes` is per work item; there is no project-scoped changes endpoint | `/work/changes` is assembled from the issues the board read, and says so on the page |
| Multi-select filters | `validateNativeQuery` rejects a repeated query parameter with `422` | One value per field goes to the hub; any others are applied to the result, and the Filters menu says which |
| Sort and text search | `GET .../work-items` is hard-coded `ORDER BY number` with no `q` | Both are applied to the loaded board, and the search field's title says so |
| "Who is working on this" on a list row | Nothing. Attempts and changes are per work item | A bounded enrichment pass: the 24 most recently updated unfinished issues get their attempts and changes, six requests at a time. The stats strip says how many |

### What the hub gained for the property pickers

Three of the gaps above were closed in the hub rather than worked around,
because a picker that cannot express the thing it offers is not a picker.

- **`GET {nativeBase}/labels`** (`internal/hubserver/native_labels.go`). The
  hub has no label table: labels are a JSON array on each work item, so the
  catalogue is the union of what the project's items carry, with a `count`
  per label and a `color` derived from the name by an FNV-1a hash into a
  fixed palette. Deriving the colour is what lets every client draw the same
  dot for the same label without a column to store it in. A managed prefix —
  `priority:`, `effort:`, `detent:` — never appears: those are fields the hub
  owns and shows in rows of their own, and `detent:` is reserved on writes.
  Creating a label is therefore just attaching a name.
- **A priority that can be removed.** `PATCH .../work-items/:item` used to
  take `priority` as a plain number, and an omitted member means "leave
  alone", so there was no way to say "no priority" — the picker had to
  explain that a priority could not be removed once set. `tracker.PriorityPatch`
  gives the member three states instead of two: absent leaves it, a number
  sets it, and `"none"` (or a JSON `null`) clears it. The client sends the
  word, because a reader of a request log can tell it apart from an omission.
  The removal records the same `issue.edited` history event any other field
  change does, so the activity feed sees it. The v1 operator endpoint
  (`POST /api/v1/work-items/:id/priority`) learned the same word: it clears
  the queue entry's override and takes the mirrored `priority:` label off the
  GitHub issue rather than replacing it.
- **Assignees through the same patch.** `assignees` was already accepted;
  what was missing was a list of people to choose from. The assignee picker
  reads `GET /api/v2/organizations/:org/members`, which any session may call
  — a plain member sees only themselves, an owner or admin sees everyone —
  and assigns by email, which is the one human-readable identity the hosted
  membership serves. A hub that does not answer leaves the picker with "No
  assignee" and the reader's own row rather than taking the page down.

### The activity stream

`GET /projects/:project/events` is a hosted **page** route, not an API route.
It emits one event name, `activity`, whose data is a **bare decimal integer**
— the maximum `event_sequence` across the project's issues — with no `id:`
field. It cannot say what changed and cannot be resumed, so the only honest
response to an increase is to read the board again; the client coalesces a
burst of ticks into one reload.

### Cover

- `tests/work.test.ts` drives the work API client against the mock hub: cursor
  pagination, the single-value filter rule (proved against the hub's own
  `422`), the string revision, an allowed and a disallowed transition, a stale
  revision, a scripted conflict and its recovery, and the activity stream.
- `tests/components/work.test.tsx` covers the card, the lane, the list row, the
  stats strip and the review dock's four tabs against the shared fixtures.
- `tests/components/activity.test.tsx` covers the feed: one sentence per
  history type, the merge of all four sources, the fold rule, the comment card
  and its reply, and the live row in both of its states.
- `tests/components/issuePickers.test.ts` covers what the five property
  pickers offer: the workflow order and the unreachable state that stays on
  the list with its reason, "No priority" first and the removal it asks for,
  the assignee groups and the invitation row that is listed either way, the
  label suggestions and the offer to create a name the catalogue has never
  seen, and the related candidates that exclude this issue and its existing
  relations.
- `tests/visual/work.spec.js` (repository root) drives the real hosted hub in
  Chromium: the lanes, the keyboard move and its persistence across a reload,
  the view switch, the URL state, the issue page and its properties sidebar,
  the merged activity feed, the lane change and the row it records, the live
  row opening the conversation panel and sending from it, a posted comment
  appearing as a card, the five property pickers opening from their own
  letters (and not opening from a composer being typed into), a priority set
  and then removed, a label created, attached and taken off, a member
  assigned and unassigned, a relation added and removed, and the
  `/chat/issues/:id` redirect — each scanned with axe.

`dev/mock-work.ts` is the work half of the development double. It reproduces
the rules that are easy to get wrong: string revisions, `422` on a repeated
query parameter, `422` on a disallowed transition, a hosted-shaped `409` with
no `current_revision`, a bare-array changes list, and a bare-integer activity
stream. Its controls are `POST /__mock/work/conflict` (arm one scripted
conflict), `/__mock/work/activity` (bump the sequence) and `/__mock/work/reset`.

## What this milestone does not do

Attachments, multiple runners per conversation, and any light theme. The client
is dark-only and declares `color-scheme: dark` on its mount root; a Detent user
in light mode gets a deliberately dark panel rather than a broken one.

## Corrections from the first-slice review (decisions.md §10)

**Creating a conversation is idempotent (§10.2).** `POST /conversations`
carries a top-level `key`, distinct from `first_message.key`. The new-chat
surface writes that key to the outbox *before* the request and clears it only
when the hub answers, so a reload in the middle of a create reuses it and the
retry returns the stored conversation instead of opening a second one.

**Retry re-queues, it never re-sends (§10.3).** A message whose delivery is
`unknown`, `failed` or `rejected` gets a Retry affordance and one sentence:
"Retry re-queues this message once; nothing is duplicated." Pressing it sends
`{key, kind: "retry", message_id}` with a **new** key; the hub puts that same
message back to `queued` (or `saved`) and emits `message.updated`. The
affordance is on both halves of the transcript — the client's own outbox
entries and messages the hub persisted and a snapshot loaded.

There is one narrow fallback. An optimistic entry only learns its message id
from a receipt, and a send that produced no receipt at all (a transport error,
a `503`) left no message to name. There, and only there, the client re-sends
the byte-identical `message` command under its original key — `expected`
included, because a payload that differs under the same key is
`idempotency_conflict`. `PendingMessage.messageId` is what tells the two apart.

**Transcript recovery is visible (§10.4).** `execution.resume` is `"thread"`,
`"transcript"` or `""`. On `transcript` the strip says "This runner continued
from a transcript of recent messages; the provider's earlier context was not
available." The hub also appends a `role: system, kind: status` message saying
the same thing in the history, which the transcript renders as a plain status
row.

**The audience preview counts the whole history (§10.5).** The handoff form
reads `conversation.message_count`, not the loaded page: "All N messages in
this chat become readable by everyone who can read project P."

**Read-only viewers (§10.11).** Where the bootstrap says `can_write: false`
the composer is disabled and says "You can read this chat but not send
messages", and the stop button, the execution strip's controls, the handoff
action, the retry affordances and the conversation menu are not rendered at
all. A disabled control reads as "not yet"; this reader is never going to press
it. Drive it in the mock with `POST /__mock/account {"mode": "read_only"}` or
`/chat/bootstrap?account=read_only`.

**Archive and unarchive (§10.12).** The top bar carries a conversation menu —
a button with `aria-haspopup="menu"`, Enter or Space to open, focus on the
first item, Escape to close and return — offering "Archive chat" or "Unarchive
chat" against `POST .../archive|unarchive`. An archived chat leaves the sidebar
until "Show archived" is ticked, which re-reads the list with
`include_archived=true`; its transcript keeps a banner ("This chat is archived.
Archiving hides this chat from your list. It never stops a running issue or
runner.") and a disabled composer.

## What the client does when the hub misbehaves

Three rules, each of which was a defect first:

- **The stream and the command response never contradict each other.** Both
  report on the same command and only the stream is ordered. A message the
  stream already delivered is never turned back into an unconfirmed bubble by a
  slow failure, and a receipt is never applied when it is behind the ladder
  position the entry already reached (`deliveryRank` in
  `runtime/state/conversationState.ts`) — a late `queued` cannot unlock a
  question panel the runner has already been answered on.
- **A frame the client cannot decode is skipped, not fatal.** It advances the
  cursor and is logged. Failing the stream over it produced an endless
  reconnect from an unchanged cursor that re-read the same frame every second.
- **Reconnects are bounded.** One second, doubling, capped at thirty, reset by
  any frame that arrives, and the transcript says "Reconnecting in Ns" while it
  waits. A `closed` frame with `reason: server_error` says nothing about the
  conversation, so the client keeps the transcript and comes back on the same
  backoff; `access_revoked` and `archived` still end the subscription for good.

Outbox controls carry their own payload (`expected`, and the answers for an
`answer`), so Retry still works after the reader navigated away and back — the
payload used to live in a component ref that the remount threw away.

## Browser cover and the hosted hub

`tests/visual/account.spec.js` covers the account screens against the same real
hosted hub, as `owner` and as `viewer`: the login card without a session, the
organization page's role gating, the settings navigation in the sidebar, the
project settings, the wizard's four steps and the fleet. It probes the hub once
in `beforeAll` and skips any test whose §12 endpoint is not served yet, naming
the endpoint in the skip message, so each one starts passing with the hub
change that lands it rather than needing an edit here.

`tests/visual/conversation.spec.js` runs against a real hosted hub. The §10
assertions above are present but `test.skip`ped, each with a TODO naming the
item, because the hosted preview fixture does not serve them yet. The contract
they check is covered against the mock hub by `tests/corrections.test.ts` and
by the component tests; unskip each one with the hub change that lands it, not
on its own.
