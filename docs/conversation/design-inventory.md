# Conversation product design inventory (D01 and D03)

Prepared September 9, 2026. This document is the acceptance reference for the frontend
work in the Detent conversation product backlog
(`../detent-cloud-concept/IMPLEMENTATION-PLAN.md`). It closes task **D01** (screen
inventory and mapping) and task **D03** (visual system and interaction states).

It records what the supplied mockups actually contain. Where a behaviour is not shown in
any source, it is marked `[inferred]` and stated as a proposal that still needs a
decision, not as an observed fact. Nothing here asserts that a screen exists that was not
read.

## 0. Sources, precedence, and the one honest gap

### 0.1 Sources read in full

| Ref | Source | What it is |
|---|---|---|
| **A** | `~/.claude/projects/-Users-michaelhvisser-Development-digitaldrywood-detent/95cea87b-c676-49de-9226-21fa18c54f68/tool-results/artifact-55db0b4b-1788969374-f3ff.html` | The new Claude mockup artifact. One tabbed page, six tabs: five product screens plus a Notes tab carrying the token list and design rationale. Static markup; the only script switches tabs and opens the project switcher popover. |
| **B** | `../detent-cloud-concept/index.html` | Older local interaction mockup. Interactive prototype: work board, review inbox, projects, and a session layout with a review dock. |
| **C** | `../detent-cloud-concept/conversation-poc/src/app/main.tsx` and `style.css` | Runnable conversation POC. The only source that demonstrates the chat-first flow: centered composer for a new chat, docking after first send, whole-surface dropzone, handoff to an issue, delivery states. |
| **D** | `internal/web/templates/shell.templ`, `chat.templ`, `shell_data.go`, `static/css/input.css` | The existing Detent Templ/HTMX shell and its Tailwind v4 theme. |

### 0.2 The gap, recorded honestly

**Artifact A contains no new-chat screen, no chat list, and no main-chat surface.** Its
five screens are Work board, Issue thread with review dock, Fleet and spend, Project
settings, and First-run wizard. The word "chat" does not appear as a destination anywhere
in its sidebar, its top bars, or its Notes tab. The Notes tab explicitly describes the
sidebar as "issues, not projects" with three groups — Needs you, Running, Waiting — and a
Browse group of cross-cutting pages; conversations are not among them.

Consequences, which the rest of this document acts on:

1. The artifact **cannot** be the visual reference for the chat surfaces. It is the
   reference for the shell (sidebar, project switcher, top bar), for the issue thread,
   and for the review dock.
2. The chat surfaces (new chat, chat list, active conversation) are specified in section
   A.8–A.10 by composing artifact A's shell and message primitives with POC C's chat-first
   behaviour. Every such composition is marked `[inferred]`.
3. The Outcome section of the plan asks for a T3-like experience with "persistent
   conversations in the sidebar" and "one compact compose action". The artifact's sidebar
   has no conversation section. Adding one is a **deliberate extension of the artifact**,
   not a reading of it. It is specified in A.9 and must be reviewed as a change, not
   accepted as already-designed.

### 0.3 Precedence rules

Applied throughout, and repeated at each point of conflict in section A.6:

- **Artifact A wins** for the visual system, the shell, the sidebar model, the top bar,
  the issue thread composition, and the review dock layout. It is the newest artefact,
  it is the only one aligned to the current Detent domain vocabulary (lanes, predicates,
  breakers, attempts, rounds, receipts), and D03 depends on D01 for exactly these.
- **POC C wins** for chat interaction behaviour that artifact A does not show:
  new-chat centering, empty-to-active transition, whole-surface dropzone, per-message
  delivery states, outbox and retry, draft persistence, question cards. C is executable
  and its behaviour is tested; A is static markup.
- **Mockup B is superseded** as a visual reference. It survives only as evidence for
  three review-dock behaviours that A shows statically and B shows working: dock
  close/reopen/expand-restore, Code/Visual/Split switching over one round, and point
  annotation on a visual artefact. Even there, V01 owns the contracts and B's data model
  (whole-document review objects, downloaded JSON payload) is explicitly not the target.
- **Existing Detent D wins** for anything already shipped and authoritative: routes,
  authentication, issue metadata, settings, board semantics. The mounted client renders
  alongside it, it does not replace it.

### 0.4 Reading the control tables

Each control table row is `Control | What it is | Task`. `Task` is a plan task ID
(U01–U08, W01–W03, V01–V05) or `deferred`. `deferred` means one of:

- **deferred (existing surface)** — the control already exists in the Templ application
  and is not part of the conversation frontend backlog.
- **deferred (later milestone)** — real product intent, scheduled after the first
  conversation release.
- **deferred (not modelled)** — the artifact shows it, no Detent capability or contract
  backs it, and no task claims it. It must not be built as decoration.

---

## A. Screen inventory (D01)

### A.1 Work board — artifact screen 1

**Route.** Existing: `GET /` (`s.board`). Project-scoped variant `GET /projects/*`.
No new route. The mounted conversation client does not own this screen.

**Layout regions.**

```
+-- .win (grid: 296px sidebar | 1fr main, height 100vh) --------------+
| .side (bg --card, right hairline)   | .main (column)               |
|  .side-top     52px  brand + icon   |  .top       52px  breadcrumb  |
|  .side-body    flex:1, scrolls      |  .toolbar         filters     |
|    search row                       |  .stats           counters    |
|    project switcher (+ .pop)        |  .body > .scroll > .lanes     |
|    section: Needs you (7)           |    horizontal lane columns    |
|    section: Running (1)             |                               |
|    section: Waiting (3)             |                               |
|    "Show 9 more"                    |                               |
|    section: Browse (6 rows)         |                               |
|  .side-bot     52px  icons + ver    |                               |
+---------------------------------------------------------------------+
```

**Controls.**

| Control | What it is | Task |
|---|---|---|
| Brand row + trailing icon (`.side-top`) | Home link plus one icon, unlabelled in the markup | deferred (existing surface) |
| Search row with `kbd /` | Global search entry, keyboard hint | U01 (chat/issue search); board search deferred (existing surface) |
| Project switcher row | Scope row: project icon, name, chevron, gap, trailing pencil | U01 |
| Pencil on the switcher row | The single create action. Notes tab: "New issue is the pencil on the switcher row, the one create action." | U01 (as New chat in a chat context — see B.13) |
| `.pop` project popover | Search field, "All projects" row, eight project rows each with icon, colour, right-hand status (`4 need you · 0 / 2`, `1 live`, `idle`) or a `paused` pill, and a trailing gear; footer "Add a project" with `⌘⇧N` | U01 |
| Per-project gear in the popover | Opens that project's settings | deferred (existing surface) |
| Section headers (Needs you / Running / Waiting) with rule and count | Group labels, count on the right | U01 |
| Issue rows (`.srow.irow`) | Leading state icon or live dot, project colour dot, `#number`, title, right column carrying exactly one signal: age, `PR`, `held`, elapsed | U01 |
| "Show 9 more" | Pagination inside the section | U01 |
| Browse rows: Pull requests (count 2), Activity, Fleet, Diagnostics (`stalled` warn pill), Reports, Library | Cross-cutting destinations | deferred (existing surface) |
| `.side-bot` three leading icons, version `v0.108.0`, one trailing icon | Unlabelled; version string is live in Detent today | deferred (existing surface) |
| Breadcrumb `Work / All projects` + meta `8 projects · 3 active` | Top-bar context | U01 for the chat contexts; deferred (existing surface) here |
| Live chip: `status-dot ok live` + `Live · data current · 12:18 AM` | Connection and freshness | U05 |
| Split button `Open board` + chevron | Primary destination with a menu | deferred (existing surface) |
| Two ghost icon buttons in the top bar | Unlabelled | deferred (not modelled) |
| Toolbar: `Search issues…` with `kbd /` | Board search | W01 |
| Toolbar: `Filters` | Filter menu | W01 |
| Toolbar: `Sort` showing `Priority` + chevron | Sort menu with current value | W01 |
| Toolbar: warn chip `Loop behind · 26m 20s` | Scheduler health | deferred (existing surface) |
| Segmented `Board` / `List` | View switch | W01 |
| `Lanes 5/9` + chevron | Lane visibility menu | W01 |
| Stats strip: `1 running`, `0 ready`, `3 waiting`, `11 blocked`, `5 completed · 24h`, then mono `1 / 2 slots · 312 tps · $28.77 notional today` | Counters | W01 |
| Lane header: title, count (`err` variant when blocked), optional `1 live`, trailing menu icon | Lane controls | W01 |
| Issue card | Project icon + name + mono `#id`; title; optional worker strip (live dot, model, `medium · full access`, `1.2M tok`); footer with PR chip, `attempt 2`, age, priority pill | W01; the worker strip is U07-adjacent state, read-only here |
| Empty lane message | `Nothing is merging.` + `Last merge parable #2075 · 1h ago` | W01 |

**States present in the markup.** Live (green pulsing dot, `.issue.live` green-tinted
border), waiting with retry (`Waiting · retry 14m`), blocked (`Blocked · 1`), needs review
(`Needs review`), human recovery (`Human recovery`), held with no predicate
(`Held · no predicate`), empty lane, dimmed lane (`Merging`, count 0).

**Unused CSS, recorded so it is not mistaken for a spec.** `.scr-1 .bar` / `.bar i` define
a 3px progress bar with a 62% success fill. No markup uses it. Do not build a per-issue
progress bar on the strength of a dead rule.

### A.2 Issue thread with review dock — artifact screen 2

This is the screen the conversation product must land inside. It is the most important
one in the artifact.

**Route.** Existing: `GET /projects/:project_id/issues/:issue_ref` (`s.issueDetail`).
Proposed conversation-scoped route in the mounted client:
`/chat/i/:project_id/:issue_ref` `[inferred]` — F02 confirms the mount prefix.

**Layout regions.**

```
+-- .win ------------------------------------------------------------------+
| .side (as A.1, project-scoped: no project dots on rows)                   |
|                                                                           |
| .main                                                                     |
|  .top 52px: breadcrumb / Add action / Open+menu / PR chip+menu / 3 icons   |
|  .body (flex row)                                                         |
|    .thread (flex:1, position:relative)                                    |
|      .msgs (scrolls, padding-bottom 190px to clear the composer)          |
|        .col (max-width 760px, 32px side padding)                          |
|          .issuecard                                                       |
|          .agent  (assistant turn: .who, .tool rows, prose, .blocked card) |
|          .bubble (user turn, right-aligned)                               |
|          .agent  (assistant turn, correction round)                       |
|      .composer-wrap (absolute bottom, gradient fade to --bg)              |
|        .settled  (status strip, attached above the box)                   |
|        .box      (the composer)                                           |
|        .checkout (worktree/branch strip, attached below the box)          |
|    aside.dock (flex 0 0 560px, left hairline)                             |
|      .dock-tabs 52px | .dock-sub 40px | .files | .diff (scrolls)          |
+---------------------------------------------------------------------------+
```

**Sidebar difference from A.1.** In the project-scoped state the issue rows carry **no**
project colour dot, the Running section shows the empty row
`Nothing running · 0 / 2 slots`, and counts are project-local. The Notes tab states the
rule: "The dot disappears when a single project is selected."

**Controls — top bar.**

| Control | What it is | Task |
|---|---|---|
| Breadcrumb: project icon + `parable` / `#3363 <title>` | Scope trail, truncating current segment | U06 |
| `Add action` | Adds a scheduled/manual action to the issue | deferred (not modelled) |
| Split `Open` + chevron | Open the issue in the tracker, with a menu | U06 |
| Split `PR #3372 · Blocked` + chevron | PR link carrying live CI/merge state, with a menu | U06 (link), V01 (state semantics) |
| Three ghost icon buttons | Unlabelled | deferred (not modelled) |

**Controls — transcript.**

| Control | What it is | Task |
|---|---|---|
| `.issuecard` h1 | Issue title, 20px/600 | U06 |
| `.issuecard .props` pills | `Blocked`, `High`, `epic #3350`, `effort · medium`, PR pill with icon, `opened Sep 7 by michaelhvisser` | U06 |
| `.issuecard` body and muted acceptance paragraph | Objective and acceptance criteria | U06 |
| `Show full issue` | Expands the truncated body | U06 |
| `.agent .who` | Model identity plus run scope: `gpt-6-astra · attempt 1 · medium · full access`, or `· correction round 2` | U03, U07 |
| `.tool` rows | Work-log lines: icon, `Ran <b>pnpm astro check && pnpm test</b>`, dim detail `· 0 errors · 412 tests`; `Pushed <b>branch</b> → opened <b>PR #3372</b>` | U03 |
| `.tool` row with a status dot | Receipt line: warn dot, `Correction request <b>queued</b> · idempotency key cr_8f3a · waiting for a slot (0 / 2 available)` | U05 (delivery state), V04 (correction receipts) |
| Assistant prose with inline `code` | Markdown body | U03 |
| `.blocked` card | Header `Blocked by a non-terminal dependency`; 150px/1fr key-value grid: `blocked_by`, `predicate` (mono), `owner`, `human_action` | U07 |
| `.blocked .act` buttons: `Ship the index here` (primary), `Wait for #3355`, `Edit predicate` (ghost) | Decision actions on the blocking predicate | U07 |
| `.bubble` | User turn: `.who` line `michaelhvisser · Sep 8, 9:41 PM`, right-aligned, max-width 86% | U03 |

**Controls — composer stack.**

| Control | What it is | Task |
|---|---|---|
| `.settled` strip | Attached above the box: icon, `<b>This issue is blocked</b>`, `Detent recheck runs every tick · next in 4m`, right-aligned `Un-block` action | U07 |
| `.box` placeholder | `Ask for changes, send follow-ups, or attach screenshots`; min-height 44px | U02 |
| Model chip `gpt-6-astra` + chevron | Composer footer chip | U02 — **see the decision in A.7.4** |
| Effort chip `Medium` + chevron | Composer footer chip | U02 — **see the decision in A.7.4** |
| Access chip `Full access` + chevron | Composer footer chip | U02 — **see the decision in A.7.4** |
| Attach icon | File picker entry | U04 |
| Send button | 30px circle, `--primary`, rendered at `opacity:.55` because the draft is empty — this is the disabled treatment | U02 |
| `.checkout` strip | Attached below the box: `Worktree · detent-worktrees/parable/3363`, branch `fix/mailchimp-address-health`, trailing icon | deferred (later milestone, W02) — local-execution concept, must not render in deployment modes that have no worktree |
| `.handle` | 4x28px drag handle at the thread's left edge, CSS-only in this screen | deferred (later milestone) — pane resize |

**Controls — review dock.**

| Control | What it is | Task |
|---|---|---|
| Tabs `Diff (D)`, `Visual (V)`, `State (S)`, `Receipt (R)`, `Activity (A)` with single-key hints | Dock view switch; Diff is the default `on` tab | V02 (Diff), V03 (Visual), W02 (State, Activity), U05/V04 (Receipt) |
| Trailing dock-tabs icon | Unlabelled; close or expand `[inferred]` | V02 |
| `.dock-sub`: `Round 2 · head a41f0c2` | Round and immutable head identity | V01 |
| `.dock-sub` stat: `+2.3k` / `−1` | Diffstat | V02 |
| `.dock-sub` six trailing icons | Unlabelled (unresolved filter, next/previous change, whitespace, wrap, expand — all `[inferred]`) | V02 |
| `.files` rows | Changed-file navigation, mono path, per-file `+N`/`−N`, `on` state for the selected file | V02 |
| `.diff` sticky file header | Path plus `+68` | V02 |
| `.ln` rows | Gutter/gutter/content grid, variants `add`, `del`, `ctx`, `hunk` | V02 |
| `.cmt` inline comment | Numbered annotation badge, `<b>michaelhvisser</b> · line 18 · round 2 · head a41f0c2`, body, actions `Reply` and `Resolve` (ghost) | V02 |

**States present.** Blocked issue with a non-terminal dependency; queued correction
request with a visible idempotency key and a capacity reason; review round 2 anchored to
head `a41f0c2`; one unresolved line comment; dock open at 560px with Diff selected.

**States the artifact does not show for this screen, and which must be designed.**
Streaming assistant output, a running worker in the composer strip, a pending question
awaiting an answer, a disconnected transport, a failed send, an empty thread. Their
treatments are specified in section B and drawn from POC C.

### A.3 Fleet and spend — artifact screen 3

**Route.** Existing: `GET /fleet`, plus `GET /fleet/runners`. No new route.

**Layout regions.** Standard shell; main is a single scrolling column,
`.usage` max-width 1180px, 40px side padding. Blocks in order: hero (340px/1fr grid:
totals and provider rows on the left, daily-spend chart on the right), Totals (five-up
grid), Hosts (two-up cards), Breakdown (heading with a segmented control, then a table).

**Controls.**

| Control | What it is | Task |
|---|---|---|
| Breadcrumb `Fleet / All hosts` + meta `Aug 10 to Sep 8` | Scope and window | deferred (existing surface) |
| Segmented `Spend` / `Tokens` / `Capacity` | Metric switch | deferred (existing surface) |
| Range `Past 24h` / `7 days` / `30 days` / `90 days` | Time window | deferred (existing surface) |
| Ghost icon button | Unlabelled (export `[inferred]`) | deferred (not modelled) |
| Hero: `$118.12`, `79 sessions · notional USD · 56.6M tokens` | Headline totals | deferred (existing surface) |
| Provider rows (Codex, Claude Code) with colour dot, session count, value, share line | Per-provider spend | deferred (existing surface) |
| Daily notional spend chart (inline SVG, gridlines dashed) | Chart | deferred (existing surface) |
| Totals five-up: Processed tokens, Cached input, Uncached input, Output, Cache hit | Stat tiles | deferred (existing surface) |
| Host cards: status dot, name, right meta (`Connected · localhost:4000 · v0.108.0`, `Paused · last seen 2h ago · v0.107.2`), three-up kv grid, capacity meter, second kv grid with GitHub REST/GraphQL quota and loop state | Host health | deferred (existing surface) |
| Breakdown segmented `Project` / `Model` / `Day` | Table grouping | deferred (existing surface) |
| Breakdown table: Project, Spend, Share, Tokens, Sessions, Merged, `$ / merged` | Table | deferred (existing surface) |

**States present.** Connected host with live dot; paused host with a neutral dot, a zero
meter, em-dash quotas, and an `0.109.0 available` update note in `--info`; a loop-behind
value coloured `--warning`; a zero-spend project row rendering `—`.

### A.4 Project settings — artifact screen 4

**Route.** Existing: `GET /settings`. Per-project settings are reached from the switcher
gear. No new route.

**Layout regions.** The sidebar takes a **settings variant**: no project switcher, no
issue sections. Rows are General, Projects (selected) with eight indented `.srow.sub`
project rows (one `on`), Budget & brakes, Capacity & pools, Providers, Trackers, Hosts,
API keys, Appearance, Keybindings, Archive. `.side-bot` becomes a single `Back` row plus
one icon. Main is a 980px column with a warning box then four labelled groups of rows.

**Controls.**

| Control | What it is | Task |
|---|---|---|
| Settings sidebar rows and project sub-rows | Settings navigation | deferred (existing surface) |
| `Back` row in `.side-bot` | Leaves settings | deferred (existing surface) |
| Breadcrumb `Settings / Projects / parable` + meta `getparable/parable · github` | Scope trail | deferred (existing surface) |
| `Restore defaults` (ghost), `View workflow` | Page actions | deferred (existing surface) |
| Warning box: `Dispatch is paused for this project on mac-mini-1.` with reason, auto-reset time, and a `Clear breaker` button | Breaker state and its one action | deferred (existing surface) |
| Dispatch group: `Paused` toggle (off), `Priority` select `2 · High`, `Weight` select `1`, `Pool` select `default · 2 guaranteed, burst to 2`, `Active hours` select `6:00 AM – 11:00 PM · America/Chicago` | Dispatch policy | deferred (existing surface) |
| Lanes & recovery group: `Backlog admission` (off), `Dependency auto-unblock` (on), `Human review required` (on), `Auto-settle merged issues` (on), `Weekly stale sweep` (off) | Lane policy toggles | deferred (existing surface) |
| Budget & brakes group: per-issue ceiling `$3.00`, per-day ceiling `$40.00`, no-progress breaker `3 sessions`, spend-since-progress breaker `4.0M tokens` | Brakes | deferred (existing surface) |
| Worker defaults group: `Model` `gpt-6-astra`, `Effort` `Medium`, `Access` `Full access` | Worker defaults — **the authoritative home of the three composer chips** | deferred (existing surface); see A.7.4 |
| Paths group: Workflow, Workdir, Worktree root with mono values and a copy icon | Paths | deferred (existing surface) |

**States present.** Tripped breaker with a named cause and a reset time; toggles on and
off; a description carrying last-trip evidence (`Last trip: #2093 at $3.03 projected
$2.96`).

### A.5 First-run wizard — artifact screen 5

**Route.** Existing: `GET /onboarding`. No new route.

**Layout regions.** The shell is present but de-emphasised: `.scr-5 .side` renders at
`opacity:.35` with a 1px blur and shows only a search row and the empty state
`No hosts yet / + Add a host`. The top bar is empty. Main centres a 640px `.wiz` card.

**Controls.**

| Control | What it is | Task |
|---|---|---|
| Step rail `1 Hosts` (on) / `2 Providers` / `3 Projects` | Wizard progress | deferred (existing surface) |
| Two host options with checked checkboxes, name, mono detail line, and a right status (`Connected` with an ok dot; `Paused · seen 2h ago` with a warn dot) | Host selection | deferred (existing surface) |
| `Detent Hub` option: `Shared board, artifacts, and review across hosts`, right action `Sign in` | Hub sign-in | deferred (existing surface) |
| `Add a host` option | Adds a host | deferred (existing surface) |
| `Continue` primary button | Advances the wizard | deferred (existing surface) |

**States present.** Connected, paused, not-signed-in, empty sidebar.

### A.6 Notes tab — artifact screen 6

Not a product screen. It carries the design rationale and the token swatch list, both
used verbatim in section B. Its load-bearing statements:

- The sidebar list is issues, not projects; projects are a scope chosen in the switcher.
- Three groups, one signal each in the right column.
- The project dot appears only in the "All projects" scope.
- **"New issue is the pencil on the switcher row, the one create action."**
- Browse holds the cross-cutting pages.
- "Pause, promote, budget override, and clear breaker are operator controls, not
  navigation. They belong on the project menu and in the command palette."

### A.7 Resolved differences between artifact A and mockup B

#### A.7.1 Navigation model — **artifact A wins**

B's sidebar is `Work / Review inbox / Projects` plus a flat project list. A's sidebar is a
project **scope switcher** plus attention-ordered **issue** groups plus a Browse group.
A wins: it matches the current Detent shell's project rows, it puts the user's actual
question ("what needs me?") in the primary position, and B's separate "Review inbox" nav
item is subsumed by A's `Needs you` group. W03's review destination is therefore a
filtered view reachable from Browse and from `Needs you`, not a peer of Work.

#### A.7.2 Review presentation — **artifact A wins on layout, B contributes behaviour**

B docks review beside a narrow conversation (`minmax(320px,36%) | 1fr` — review dominant)
and offers `Split | Code | Visual` tabs, `Close`, `Expand`/`Restore`, a general-comment
composer, a "Prepare corrections (N)" footer button, and a modal showing the JSON payload.

A docks review at a fixed `flex: 0 0 560px` beside a **conversation-dominant** thread, and
its tabs are `Diff | Visual | State | Receipt | Activity`.

A wins on proportion and tab set: the plan's V02 requires that "conversation and composer
remain available beside the dock", which A satisfies and B does not (B hides the main
header and squeezes the conversation to 36%). B's `Split` mode is retained as a V03
concern; A's tab strip should gain Split as a sixth entry or as a Visual sub-mode — **open
decision, flag for V03**. B's close/reopen/expand-restore behaviour is retained because A
shows only one unlabelled trailing icon; B is the evidence for what it should do
`[inferred]`. B's "Prepare corrections (N)" footer and payload modal are **rejected**: V04
routes correction requests through the authorized worker path, not a download.

#### A.7.3 Comment anchoring — **B's interaction, A's presentation, V01's contract**

B attaches comments by clicking a diff line or a point on an image, with numbered pins.
A shows the resulting artefact: an inline `.cmt` card under the anchored line with a
numbered `.ann` badge, author, `line 18 · round 2 · head a41f0c2`, and `Reply`/`Resolve`.
Take A's rendering and B's placement gesture. Neither source's data model survives: V01
defines file/side/line/range/commit for code and artifact-id/digest/route/viewport/point
for visual.

#### A.7.4 Composer chips — **conflict with the plan; decision required**

A's composer footer carries three chips with chevrons: `gpt-6-astra`, `Medium`,
`Full access`. Plan U02 states: "There are no decorative model, permission, cost, or
repository controls implying unsupported functionality."

These three values are **real** — screen 4 shows them as the project's Worker defaults,
and A's agent header repeats them per attempt (`· attempt 1 · medium · full access`).
The question is only whether a chip in the composer **changes** them for the next turn.

Resolution for U02, to be confirmed in F03 when capabilities are typed:

- Default: render the three as **read-only scope indicators** (no chevron, no menu),
  showing the values in force for the displayed execution. This is honest and needs no
  new capability.
- Only if a backend capability advertises per-turn override may a chip become a menu. In
  that case the chevron returns and the menu lists only values that capability allows.
- In the ordinary chat context (no runner, no issue), the chips do **not** render at all.
  There is no attempt whose model, effort, or access they could describe.

#### A.7.5 Worktree and branch strip — **conditional, not universal**

A's `.checkout` strip below the composer names a worktree path and a branch. This is a
local-execution concept. R01 requires documenting which deployment modes are supported;
the strip must render only where a worktree actually exists and must be absent otherwise
rather than showing a placeholder path. Deferred to W02.

#### A.7.6 Conversation surfaces — **neither A nor B; POC C wins**

B has a session layout with a transcript and a composer, but it is a review prop: its
composer is labelled "Local demo · no worker connected" and its send button only appends
to a local array. A has no chat surface at all. POC C is the only source with a real
chat-first flow and is therefore authoritative for A.8-A.10.

### A.8 New chat — required, missing from the artifact

**Status.** Not present in artifact A. Specified here from POC C plus artifact A's visual
system. Every layout claim below is `[inferred]` unless attributed to C.

**Route (proposed, F02 confirms).** `GET /chat` inside the mounted client. Direct link and
refresh must land here with an empty composer and no conversation created.

**Layout regions.**

```
+-- .win ------------------------------------------------------------------+
| .side  (A.1 shell; Chats section present and selected — see A.9)          |
| .main                                                                     |
|   .top 52px: breadcrumb  <project> / New chat        (no PR chip, no dock)|
|   .body (single column, centred)                                          |
|     flex spacer                                                           |
|     greeting line (single 28px heading, no mark, no body copy)            |
|     .composer  (centred, wider radius, min-height ~90px)                  |
|     hint line: Enter to send · Shift + Enter for a new line               |
|     flex spacer                                                           |
+---------------------------------------------------------------------------+
```

From C: in `.chat-mode.empty-chat` the timeline collapses (`flex: 0; margin-top: auto`),
the welcome mark, body paragraph, and suggestion buttons are hidden, only the 28px
heading remains, and the composer area takes `margin-bottom: auto` — producing a
vertically centred pair of heading and composer. On first send the classes drop and the
same composer docks at the bottom without remounting. **This must be one component whose
position changes, not two components** — U02's acceptance requires that first send does
not lose input.

**Controls.**

| Control | What it is | Task |
|---|---|---|
| Project scope in the breadcrumb | The chat's project context. Plan decision 2: chats start in an explicit project context | U01, U06 |
| Greeting heading | One line, e.g. `What should we work on?` (C's copy) | U02 |
| Composer textarea | Placeholder `Message Detent…` (C); Enter sends, Shift+Enter newlines, IME composition respected | U02 |
| Attach button | Opens the file picker; keyboard-reachable | U04 |
| Send button | Disabled while the draft is empty and no attachment is staged | U02 |
| Attachment chips | Staged files with a per-file remove control | U04 |
| Whole-surface dropzone | The entire conversation surface accepts a file drop, with depth-counted enter/leave so nested targets do not flicker (C) | U04 |
| Hint line | `Enter to send · Shift + Enter for a new line` (C) | U02 |

**Rejected from C.** The suggestion buttons (`What needs me?`, `Start a new issue`) are
hidden by C's own empty-chat rules and are **not** part of this screen. The `＋ New issue`
button in C's header is rejected: it duplicates the one create action (see B.13).

**States.**

- **new-chat** — no conversation record exists yet. Composer centred, send enabled only
  when the draft is non-empty or an attachment is staged.
- **sending first message** — send disabled, the draft is preserved verbatim. On
  acknowledgement the client navigates to `/chat/c/:id` **without** clearing and
  re-entering the text. C does this via `execute()` returning an ack carrying the new id.
- **failed first send** — the user's text and staged attachments survive; an actionable
  error appears; the outbox holds the original idempotency key (C persists
  `outbox:<id>` in `localStorage`). U05.

### A.9 Chat list — required, missing from the artifact

**Status.** Not present in artifact A. Artifact A's sidebar has no conversation section.
Adding one is a deliberate extension driven by the plan's Outcome ("persistent
conversations in the sidebar"). Everything here is `[inferred]`.

**Placement.** A new sidebar section between the project switcher and the `Needs you`
section, using the existing `.sec` header and `.srow` row primitives so it is visually
indistinguishable from the issue sections:

```
  search row
  project switcher row                <- the pencil is the one create action
  .sec  Chats            (count)      <- NEW
    .srow  conversation rows           <- NEW, capped, then "Show N more"
  .sec  Needs you        (7)
  .sec  Running          (1)
  .sec  Waiting          (3)
  .sec  Browse
```

**Route.** The sidebar section needs no route. A full-page list is still required for
narrow widths and for search results: proposed `GET /chat/list` `[inferred]`.

**Controls.**

| Control | What it is | Task |
|---|---|---|
| `Chats` section header with count | Group label, matching `.sec` | U01 |
| Conversation row | Leading state indicator, title (truncated with ellipsis), right column carrying exactly one signal: relative time, or a live dot while streaming, or an attention marker | U01, U08 |
| Selected row | `.srow.sel` treatment; must track the route so browser back/forward is correct | U01 |
| `Show N more` | Section pagination | U01 |
| Search (the existing sidebar search row) | Real server-side search across conversations, authorization-enforced | U01 |
| Archive affordance | Explicitly defined; archiving a chat must not stop a running issue | U01 |
| Attention state on a row | Set from canonical events, not from a client guess | U08 |

**The single signal rule.** The Notes tab is explicit that each sidebar group's right
column carries exactly one signal. Chat rows obey it: relative time by default, replaced
by a live dot while a turn streams, replaced by an attention marker when the conversation
has an unanswered question. Never two at once.

**States.** Empty (`.srow.empty`, e.g. `No chats yet`), populated, one row selected, a
row streaming, a row needing attention, more-available, search-results, and
search-returns-nothing.

### A.10 Active conversation — composed, partly missing from the artifact

**Status.** Artifact A shows this only in its **linked-issue** form (A.2). The ordinary
chat form is composed from A's message primitives plus C's behaviour. `[inferred]` where
noted.

**Route (proposed).** `GET /chat/c/:conversation_id` for an ordinary chat;
`GET /chat/i/:project_id/:issue_ref` once the conversation is linked to an issue. Plan
decision 1 requires one canonical conversation ID across both; the issue relationship is
modelled separately, so the two routes resolve to the same conversation record.

**Layout regions.** Identical to A.2 minus the review dock and minus the issue card, until
an issue is linked:

```
.main
  .top 52px  breadcrumb: <project> / <chat title>
  .body
    .thread
      .msgs > .col (760px)
        [assistant/user turns, tool rows, issue result card]
      .composer-wrap  (docked, gradient fade)
        [.settled strip only when a scope status exists]
        .box
```

**Controls beyond A.2 and A.8.**

| Control | What it is | Task |
|---|---|---|
| `Load earlier messages (N)` | Server-paginated history; must not jump the scroll position (C sets `follow.current = false` before loading) | U03, S02 |
| Return-to-latest affordance | Required when the user has scrolled away from the bottom during streaming; C tracks a 100px threshold but shows no button — **the button is new** `[inferred]` | U03 |
| Issue result card | After handoff: title plus `Open issue` action, rendered inside the assistant turn that created it (C renders `.issue-card` from `m.links`) | U06, U08 |
| `Create linked issue` action | Opens the handoff form: project, title, objective, audience and history preview, confirmation | U06 |
| Handoff form fields | Title (required, max 180), objective (required), project (fixed, displayed) | U06 |
| Question card | Pending-input panel: one fieldset per question, option buttons, a free-text input, a single `Send answer` submit disabled until every question is answered (C) | U07 |
| Execution control strip | The A.2 `.settled` slot generalised: lane name, an explanatory sentence, and exactly one primary action — `Recover history` when delivery is unknown, `Interrupt` while active, `Continue` / `Start runner` when idle with a saved message (C) | U07 |
| Retry panel | Appears when an outbox entry survives; `Retry same command` reuses the original idempotency key, `Dismiss retry` clears it (C) | U05 |
| Attempt boundary divider | A rule with `Attempt N` between turns from different attempts (C) | U03, U07 |

**Copy rule taken from C, and it matters.** When delivery is uncertain the transcript
renders `Delivery is uncertain. Recover history before deciding whether to send again.`
It never says "failed, retry". U05's acceptance criterion depends on this distinction.

### A.11 Cross-screen state matrix

Every state D01 requires, where it renders, and what it looks like. `—` means the state
does not apply to that surface.

| State | New chat (A.8) | Chat list (A.9) | Conversation (A.10) | Issue thread (A.2) |
|---|---|---|---|---|
| **new-chat** | Composer centred, heading only, no transcript, no scope chips | No row yet; the section shows its existing rows | — | — |
| **active / streaming** | — | Row's right column becomes a pulsing `--success` dot | Assistant turn grows; tool rows append; a caret marks the streaming tail; send becomes stop | Same, plus the agent `.who` line naming attempt and model |
| **idle** | — | Right column shows relative time | Composer enabled, no status strip, no spinner | `.settled` strip absent unless the lane has a status |
| **waiting-for-input** | — | Row shows the attention marker | Question card pinned after the last turn; composer stays usable; answering is a distinct action from approving | Same |
| **failed** | Draft and attachments preserved; actionable error; outbox holds the key | Row unchanged (failure is per-message, not per-conversation) | Per-message error state plus the retry panel | Same |
| **disconnected** | Composer stays editable; send is disabled with a stated reason | Section dims; counts freeze with an "as of" time | Top-bar Live chip degrades to a warn dot with `Reconnecting… data as of HH:MM`; the transcript is not cleared | Same, and dock content is marked as of the last known head |
| **linked-issue** | — | Row carries the issue reference in the title line | Issue result card in the transcript; breadcrumb gains the issue segment; scope indicator becomes the issue | This is the default state of A.2 |
| **waiting-for-runner** | — | Right column shows the queue signal | `.settled` strip: distinct copy from a working runner, e.g. `Waiting for a runner · 0 / 2 slots` | The artifact's own example: `Correction request queued · waiting for a slot (0 / 2 available)` |
| **review dock** | — | — | Not available (no PR) | Right pane 560px, Diff default, round and head named in `.dock-sub` |

`waiting-for-runner` and `active` must never share a treatment. B05's acceptance requires
scheduling state to be reported accurately including "waiting for runner", and U06's
requires that "waiting for a runner is distinct from a working runner". A pulsing green
dot means a worker is producing output; a queue signal means nothing is running.

### A.12 Deferred controls, collected

Everything the artifact renders that no task claims. Building any of these without a
backing capability violates U02's acceptance criteria.

| Control | Screen | Reason |
|---|---|---|
| `Add action` | A.2 top bar | No modelled action-creation capability |
| Three unlabelled ghost icons | A.2 top bar | Purpose not shown |
| Two unlabelled ghost icons | A.1 top bar | Purpose not shown |
| Trailing icon in `.side-top` | all | Purpose not shown (sidebar collapse `[inferred]`) |
| Three leading icons in `.side-bot` | all | Purpose not shown |
| Six trailing icons in `.dock-sub` | A.2 dock | Purpose not shown |
| `.scr-1 .bar` progress bar | A.1 | Defined in CSS, never rendered |
| `.handle` resize grip | A.2 | Defined in CSS, no behaviour shown |
| `.checkout` worktree strip | A.2 | Deployment-mode dependent (A.7.5) |
| Command palette | Notes tab | Named in the rationale, no UI shown |
| `⌘⇧N` Add a project | switcher popover | Existing surface |

---

## B. Visual system (D03)

All values below are read directly from artifact A unless attributed otherwise.

### B.1 Colour tokens (exact values)

Declared on `:root` in artifact A, with `color-scheme: dark`.

| Token | Value | Use |
|---|---|---|
| `--bg` | `#0a0a0a` | Canvas |
| `--card` | `#121212` | Sidebar, cards, composer box, popover surfaces |
| `--popover` | `#121212` | Alias of `--card` |
| `--raised` | `#121212` | Alias of `--card` |
| `--fg` | `#f5f5f5` | Primary text |
| `--muted-fg` | `#8a8a8a` | Secondary text, icon default |
| `--muted-fg-2` | `#6b6b6b` | Tertiary text, gutters, captions |
| `--muted` | `rgba(255,255,255,.03)` | Lane background, selected row, inset strips |
| `--accent` | `rgba(255,255,255,.04)` | Hover fill, user bubble, inline code background |
| `--accent-2` | `rgba(255,255,255,.06)` | Pressed/open fill, segmented selection, count chip |
| `--border` | `rgba(255,255,255,.06)` | Hairline |
| `--border-2` | `rgba(255,255,255,.1)` | Stronger hairline: popover, composer box, kbd |
| `--input` | `rgba(255,255,255,.08)` | Control border |
| `--primary` | `#3f6af5`, then overridden by `oklch(0.571 0.21 264)` | The single accent. The hex is the fallback declaration; the oklch value wins in supporting browsers. Keep both lines in this order. |
| `--primary-fg` | `#fff` | Text on primary |
| `--success` | `#34d399` | Live, additions, ok |
| `--warning` | `#fbbf24` | Behind, queued, paused-with-cause |
| `--error` | `#f87171` | Blocked, deletions, needs review |
| `--info` | `#60a5fa` | Neutral notice, hunk headers, update available |
| `--violet` | `#a78bfa` | Declared, unused in markup |
| `--orange` | `#fb923c` | Provider identity (Claude Code row) |
| `--success-s` | `rgba(52,211,153,.14)` | Success pill fill |
| `--warning-s` | `rgba(251,191,36,.14)` | Warning pill fill |
| `--error-s` | `rgba(248,113,113,.14)` | Error pill and blocked-card fill |
| `--info-s` | `rgba(96,165,250,.14)` | Info pill and hunk fill |

Derived values used inline and worth naming as tokens:

| Purpose | Value |
|---|---|
| Primary pill fill / text | `rgba(63,106,245,.18)` / `#9db4ff` |
| Diff add row / text | `rgba(52,211,153,.09)` / `#8ee6c0` |
| Diff del row / text | `rgba(248,113,113,.09)` / `#f4a0a0` |
| Diff context text | `#c9c9c9` |
| Diff hunk row | `rgba(96,165,250,.06)` on `--info-s` text |
| Live issue card border | `rgba(52,211,153,.35)` with `0 0 0 1px rgba(52,211,153,.08)` |
| Blocked card border | `rgba(248,113,113,.3)` |
| Warning box border | `rgba(251,191,36,.3)` |
| Scrollbar thumb | `rgba(255,255,255,.08)` |
| Popover shadow | `0 16px 48px rgba(0,0,0,.65), 0 0 0 1px rgba(0,0,0,.4)` |
| Composer shadow | `0 8px 30px rgba(0,0,0,.45)` |
| Wizard shadow | `0 30px 80px rgba(0,0,0,.6)` |

**Project identity colours**, used for the row dot, the sidebar icon, and the breakdown
table dot. The Notes tab requires each project to keep one colour everywhere it appears.

`church-kit #8bc34a` · `client-portals #3aa0e6` · `michaelhvisser-web #e08bd6` ·
`parable #7b8cff` · `parable-marketing #d98a3a` · `port-app #5cc8c0` ·
`port-marketing #ef7d3a` · `threefold-marketing #f25c5c`

These are per-project data, not theme tokens. They must be assigned by the server from
project identity and passed to the client, not hard-coded in CSS.

**Recorded deviation: the two muted steps are raised for contrast.** The mounted client
(`web/conversation/src/app/style.css`) ships `--muted-fg: #9b9b9b` and
`--muted-fg-2: #828282`, not the artifact's `#8a8a8a` / `#6b6b6b`. Both muted steps are
used at 12px — section labels, counts, captions, the composer hint, the chip detail
string — which is ordinary text for WCAG, so both need 4.5:1. The artifact's tertiary
value measures **3.51:1 on `--card`** and **3.71:1 on `--bg`**, and axe reports it as a
serious `color-contrast` violation on every route. Raising only the tertiary step would
have collapsed it into the secondary one, so both moved together and the two-level
hierarchy survives: the shipped values measure **6.7:1** and **4.9:1** on `--card`.

**Do not revert these two tokens to the artifact values.** The ratios above are the
reason, `tests/visual/conversation.spec.js` is the guard — it fails the client on any
serious or critical axe violation — and the same numbers are recorded in a comment above
the declarations in `style.css`. Anything that reads these tokens at 12px inherits the
same obligation.

**Semantic discipline.** The Notes tab states the rule: one blue accent, semantic colour
reserved for state. `--primary` never signals status; `--success`/`--warning`/`--error`
never signal interactivity. This matches the existing Detent `input.css` comment
("interactivity ONLY … Never status"), so the rule carries across both surfaces.

### B.2 Radius

| Token | Value | Applied to |
|---|---|---|
| `--radius` | `10px` | Cards, issue cards, blocked card, `.settled`/`.checkout` strips, comment card |
| `--cr` | `8px` | Controls: buttons, segmented container, sidebar rows, search field, selects |
| — | `6px` | Small chips: pills, counts, kbd, file rows, inline code (`5px`), brand mark |
| — | `7px` | `.chip` |
| — | `12px` | Popover, lane column, settings row group, wizard option |
| — | `14px` | Issue card in the thread |
| — | `16px` | User bubble, composer box, wizard card |
| — | `999px` | Caption pill |
| — | `50%` | Status dots, send button, annotation badge, toggle knob |

Two families, as the Notes tab summarises them: **radius 10px, controls 8px**. Everything
else is a documented exception.

### B.3 Spacing

The artifact uses a **2px grid with 4px and 8px dominant**: observed gaps and paddings are
2, 4, 5, 6, 8, 10, 12, 14, 16, 18, 20, 22, 26, 28, 32, 40. The odd steps (6, 10, 14, 18,
22) are load-bearing — the sidebar row gap is 10px, the top-bar gap is 8px with 22px left
padding, the message column is 32px.

The existing Detent theme declares `--spacing: 4px` as a strict grid. **These are
incompatible.** Resolution: the mounted client keeps its own 2px-based scale as explicit
custom properties; it does not adopt Tailwind's spacing scale. See section C.3.

Fixed heights, which are the real layout contract:

| Element | Height |
|---|---|
| Sidebar width | `296px` (`--sidebar-w`) |
| `.side-top`, `.side-bot`, `.top`, `.dock-tabs` | `52px` |
| `.dock-sub` | `40px` |
| `.srow` | `36px`; `.srow.sub` and `.srow.irow` `32px`; `.srow.empty`/`.srow.more` `30px` |
| `.prow` (popover row) | `34px`; `.pop-search` `36px` |
| `.btn` | `30px`; `.btn.sm` `26px`; `.btn.icon` `30x30` |
| `.chip` | `26px`; `.seg button` `24px` |
| `.pill`, `.count`, `kbd` | `20px` |
| `.frow` (dock file row) | `28px` |
| Send button | `30px` circle |
| Toggle | `34x20` with a `16px` knob |
| Review dock | `flex: 0 0 560px` |
| Board lane | `flex: 0 0 300px` |
| Message column | `max-width: 760px`, `padding: 0 32px` |
| Transcript bottom padding | `190px` (clears the absolutely positioned composer) |

### B.4 Typography

Families:

```
--font: -apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif;
--mono: ui-monospace, "SF Mono", SFMono-Regular, Menlo, Consolas, monospace;
```

The artifact uses the **system font**, not Geist. The existing Detent shell loads Geist and
Geist Mono as variable web fonts. See section C.4 for the resolution.

Base: `14px / 1.5`, `-webkit-font-smoothing: antialiased`.

| Role | Size / line-height / weight |
|---|---|
| Assistant prose | `14.5px / 1.62` |
| User bubble | `14px / 1.55` |
| Sidebar row | `14px`; issue row `13px`; sub row `13px` |
| Section label | `12px`, `letter-spacing: .02em`, `--muted-fg` |
| Breadcrumb | `15px`; current segment weight `500`; meta `13px` |
| Issue title (thread) | `20px / 1.3`, weight `600`, `letter-spacing: -.01em`, `text-wrap: balance` |
| Button | `13px` weight `500`; small `12px` |
| Pill | `11px` weight `500`, line-height `1` |
| Count, kbd | `11px` |
| Mono (`.mono`) | `12px` |
| Diff and file rows | `11.5px / 1.7` mono |
| Inline code | `12.5px` mono on `--accent` with a `--border` hairline, `5px` radius |
| Tool/work-log row | `12.5px`, `--muted-fg`, with `<b>` at `--fg` weight `500` |
| Dock sub-header | `12.5px`; mono stat `11.5px` |
| Table | header `12px` weight `400` `--muted-fg`; cell `13px` |
| Hero number | `40px` weight `600`, `letter-spacing: -.02em` |
| Wizard heading | `24px` weight `600`, `letter-spacing: -.02em` |

Numeric discipline: `font-variant-numeric: tabular-nums` on every count, age, token
figure, currency value, and diff gutter (`.num`, `.srow .r`, `.pill` counts, table `.r`
cells). Identifiers (`#3363`, `a41f0c2`, file paths, predicates, idempotency keys) render
in `--mono`.

### B.5 Borders and elevation

One hairline system, no drop shadows for depth except on floating layers.

- Structural separation: `1px solid var(--border)`.
- Emphasised container: `1px solid var(--border-2)` — popover, composer box, comment card,
  wizard, `kbd`.
- Control outline: `1px solid var(--input)`.
- Selected sidebar row: `background: var(--muted)` plus `box-shadow: inset 0 0 0 1px
  var(--border)` — an inset ring, not an outline, so the row does not shift.
- Buttons carry `box-shadow: inset 0 -1px rgba(255,255,255,.06)`; the primary button
  carries `inset 0 1px rgba(255,255,255,.16), 0 1px 2px rgba(63,106,245,.25)`.
- Shadows appear only on floating layers: popover, composer box, wizard (values in B.1).

### B.6 Focus ring

**The artifact does not specify a product focus treatment.** Its only `:focus-visible`
rule is on the mockup's own tab switcher:

```css
.tabs .tab:focus-visible { outline: 2px solid var(--primary); outline-offset: 1px }
```

The other sources: mockup B uses `2px solid var(--accent)` at `offset: 2px`; POC C uses
`2px solid #c6b1e7` at `offset: 3px`; Detent `input.css` uses `2px solid
var(--color-accent)` at `offset: 1px`.

**Decision.** Adopt the artifact's own values for the mounted client:

```css
:focus-visible { outline: 2px solid var(--primary); outline-offset: 1px; }
```

This matches Detent's existing offset exactly and uses the artifact's accent. Every
interactive element in the artifact is currently a `<div>` or `<span>` styled as a
control; **all of them become real `<button>`, `<a>`, `<input>`, or `<textarea>` elements
in the implementation**, because the artifact's markup gives no focusable target at all.
This is a correctness requirement, not a preference: O03 gates on keyboard accessibility.

### B.7 Component and state sheet — navigation

#### B.7.1 Sidebar rows

| State | Treatment |
|---|---|
| Rest | `36px`, `10px` gap, `8px` radius, `--fg` text, icons `--muted-fg` |
| Hover | `background: var(--accent)`; icons brighten to `--fg` |
| Selected | `background: var(--muted)` + inset `1px --border` ring; icons `--fg` |
| Issue row | `32px`, `13px`; leading state icon or live dot; project dot only in the "All projects" scope; right column `11.5px` `--muted-fg` tabular |
| Empty | `30px`, `--muted-fg-2`, `12px`, `pointer-events: none` |
| More | `30px`, `--muted-fg`, `13px`, clickable |
| Sub row (settings) | `32px`, `padding-left: 40px`, `--muted-fg`; `on` promotes to `--fg` |
| Disabled | Not shown in the artifact `[inferred]`: `opacity: .5`, no hover, `aria-disabled` |

Row title truncates with `text-overflow: ellipsis` on a `min-width: 0` flex child. The
right column never wraps and never carries two signals (B.13).

#### B.7.2 Sidebar sections

`.sec`: `14px` top / `4px` bottom padding, `12px` `--muted-fg` label, a `1px --border`
rule filling the remaining width, and an optional tabular count in `--muted-fg-2`. This
one primitive serves Needs you, Running, Waiting, Browse, and the new Chats section.

#### B.7.3 Project switcher

| State | Treatment |
|---|---|
| Closed | A `.srow` carrying the project icon in its own colour, the name, a chevron, a `6px` gap, and the trailing create icon |
| Open | Row gains `background: var(--accent-2)`; `.pop` opens below at `top: calc(100% + 4px)`, full row width, `#161616`, `--border-2` hairline, `12px` radius, popover shadow, `6px` padding |
| Popover search | `36px` row with a `1.5px solid var(--primary)` bottom border — the artifact's only text-input-in-focus treatment |
| Popover row | `34px`, `8px` radius; hover `--accent`; current `--accent-2`; right column is either a status string (`4 need you · 0 / 2`, `1 live`, `idle`) or a `paused` pill; trailing gear |
| Popover footer | `Add a project` with `⌘⇧N`, separated by a top hairline, `38px` |
| Dismiss | Click outside closes it (the artifact's script does exactly this); Escape must also close and return focus to the trigger `[inferred]` |

#### B.7.4 Breadcrumb and top bar

`52px`, `8px` gap, `22px` left / `14px` right padding, `border-bottom: 1px solid
transparent` — the top bar has **no visible rule** in this design; separation comes from
the content below it.

Breadcrumb: `15px`, `flex: 1`, `min-width: 0`. Leading context segments render in
`--muted-fg` (`.ctx`), separators `/` in `--muted-fg-2`, the current segment in `--fg`
weight `500`, truncating. An optional `13px` `--muted-fg` meta string follows.

Trailing cluster, right to left: page actions, then split buttons, then ghost icon
buttons. The Live chip sits between the breadcrumb and the actions.

#### B.7.5 Buttons

| Variant | Treatment |
|---|---|
| Default | `30px`, `--input` border, `rgba(255,255,255,.03)` fill, `13px/500`, inset top-light |
| Hover | `rgba(255,255,255,.06)` |
| Primary | `--primary` fill and border, `#fff` text, inset highlight plus a blue-tinted drop shadow |
| Ghost | Transparent border and fill, `--muted-fg` text; hover fills `--accent` and promotes text to `--fg` |
| Icon | `30x30`, centred |
| Small | `26px`, `12px`, `8px` padding |
| Split | Two buttons sharing a radius; the second is `6px` wide padding with no left border |
| Disabled | Not shown for buttons `[inferred]`: `opacity: .5`, `cursor: default`, `aria-disabled="true"` |

Segmented control (`.seg`): `--muted` fill, `--border` hairline, `8px` radius, `2px`
padding; children `24px` at `13px` `--muted-fg`; the selected child takes `--accent-2` and
`--fg`.

### B.8 Component and state sheet — messages

#### B.8.1 User message

`.bubble`: `--accent` fill, `16px` radius, `12px 16px` padding, `margin: 0 0 22px auto`
(right-aligned), `max-width: 86%`, `14px / 1.55`. A `.who` line above the text at `11px`
`--muted-fg` carries author and timestamp.

POC C reaches the same shape by different means (`max-width: 80%`, `18px` radius,
avatar and meta hidden in chat mode). **Use the artifact's values.**

States: sent (as above); sending `[inferred]` — same bubble at `opacity: .6` with the
delivery chip reading `sending`; failed — bubble unchanged, an error line and the retry
panel below; uncertain — bubble unchanged, the uncertain-delivery sentence below.

#### B.8.2 Assistant message

`.agent`: no bubble, no background. `14.5px / 1.62`, paragraphs `margin-bottom: 12px`,
lists `padding-left: 22px`, `22px` bottom margin between turns. A `.who` header line at
`12px` `--muted-fg`: an icon, the model name, then a dim run scope
(`· attempt 1 · medium · full access`, or `· correction round 2`).

The asymmetry is deliberate and load-bearing: **the user is a bubble, the assistant is the
page**. Do not give assistant turns a surface.

States: streaming `[inferred]` — a caret at the tail (POC C uses a 5x12px block in the
accent colour); complete; failed `[inferred]` — the partial text is retained, with an
error row beneath in `--error`; empty `[inferred]` — no bubble is drawn until the first
token or tool row arrives.

#### B.8.3 System / status messages

The artifact has no separate system-message component. Status is delivered by three
existing primitives, and no fourth should be invented:

1. `.tool` rows inside an assistant turn (work log, receipts).
2. The `.settled` strip above the composer (scope-level status).
3. The `.blocked` card (a decision the user must make).

#### B.8.4 Tool and work-log rows

`.tool`: a flex row at `12.5px` `--muted-fg`, `6px` vertical padding, `12px` left padding
behind a `2px solid var(--border)` left rule, `6px` bottom margin. Leading `13px` icon or
status dot; the operative noun in `<b>` at `--fg` weight `500`; trailing detail in
`--muted-fg-2`.

Observed variants:
- Command: icon + `Ran <b>pnpm astro check && pnpm test</b> · 0 errors · 412 tests`.
- Push: icon + `Pushed <b>branch</b> → opened <b>PR #3372</b>`.
- Receipt: warn status dot + `Correction request <b>queued</b> · idempotency key cr_8f3a ·
  waiting for a slot (0 / 2 available)`.

The receipt variant is the model for every delivery state: **status dot + verb in bold +
the reason in dim text**. Reuse it for queued, sent, acknowledged, completed, rejected,
and unknown (R04's vocabulary). The dot colour carries the state; the text carries the
explanation.

#### B.8.5 Blocked and question cards

**Blocked card** (`.blocked`): `--error-s` fill, `rgba(248,113,113,.3)` border, `10px`
radius, `12px 14px` padding, `14px` vertical margin. Header at `13px` weight `600` in
`--error` with a leading icon. Body is a `150px / 1fr` grid at `13px` with keys in
`--muted-fg`; the predicate value renders in `--mono`. A `.act` row of small buttons
follows: one primary, one default, one ghost.

**Question card** `[inferred]` — not in the artifact; specified from POC C. Use the blocked
card's geometry with `--info` semantics instead of `--error`:

- Header: an eyebrow such as `RUNNER NEEDS YOUR INPUT`, `--info`.
- One fieldset per question with the question as its legend.
- Option buttons; the chosen one takes the selected treatment.
- A free-text input labelled by the question, for answers outside the options.
- One primary `Send answer` submit, disabled until every question has an answer.
- After answering: the card stays in place showing the answer and its delivery state, so a
  second tab sees the resolution without answering again (U07).

The two cards must not look alike at a glance. Red means the work is stopped and a
decision is needed; blue means the runner is waiting on you and is still alive.

#### B.8.6 Issue card

Two distinct components share the name; keep them separate.

**Board issue card** (A.1): `--card` fill, `--border` hairline, `10px` radius, `10px 12px`
padding, `6px` gap, `0 1px 2px rgba(0,0,0,.3)`. Hover promotes the border to `--border-2`.
The `live` variant takes a `rgba(52,211,153,.35)` border plus a green glow ring. Header
row: project icon, project name, mono `#id` pushed right. Title at `13px / 1.4`. Optional
worker strip: `--muted` fill, `--border` hairline, `8px` radius, `8px 10px` padding, live
dot, model name in `--fg`, dim scope, token count right-aligned tabular. Footer at `11px`:
status pill, PR chip, attempt, age, priority pill.

**Thread issue card** (A.2): `--card` fill, `14px` radius, `18px 20px` padding, `22px`
bottom margin. `h1` at `20px/600`. A wrapping row of property pills. Body paragraphs at
`14px`, acceptance in `--muted-fg`. A right-aligned `Show full issue` link at `13px`.

**Issue result card** (A.10, after handoff) `[inferred]` — use the thread issue card's
geometry at reduced scale inside the assistant turn: title, one status pill, and an
`Open issue` action.

### B.9 Composer

#### B.9.1 Docked (default, artifact A)

`.composer-wrap` is absolutely positioned across the bottom of the thread with
`padding: 0 32px 16px` and a `linear-gradient(to top, var(--bg) 70%, transparent)` fade so
the transcript dissolves behind it. `.composer` is `max-width: 760px`, centred — the same
column as the messages.

The stack is three attached pieces:

1. `.settled` (optional, above): `34px`, `0 12px` inset margin, `--muted` fill, hairline
   on three sides, `10px 10px 0 0` radius, `12.5px` `--muted-fg`. Content: icon, a bold
   statement, an explanation, then one right-aligned action.
2. `.box`: `--card` fill, `--border-2` hairline, `16px` radius, `14px 16px 12px` padding,
   composer shadow. Placeholder at `14px` `--muted-fg` over a `44px` minimum. Footer row:
   scope chips separated by `.vline` dividers, a flexible spacer, the attach icon, and the
   send button.
3. `.checkout` (optional, below): `32px`, mirrored radius `0 0 10px 10px`, `12px`
   `--muted-fg`.

The inset margins (`0 12px`) make the attached strips narrower than the box, so the box
reads as the primary object.

#### B.9.2 Centred (new chat, POC C)

Same `.box` component, repositioned. From C's `.chat-mode.empty-chat` rules:

- The transcript collapses (`flex: 0; margin-top: auto`), the composer area takes
  `margin-bottom: auto`, and the pair sits vertically centred.
- Only the greeting heading renders above it, at `28px` weight `500`,
  `letter-spacing: -0.7px`.
- The textarea's minimum height grows (C: `58px` docked, `90px` centred) and the type size
  grows (C: `12px` to `15px`). Applied to the artifact's scale: `44px` to `88px`, and
  `14px` to `16px`.
- The gradient fade and the absolute positioning are dropped; the composer is in flow.
- `.settled` and `.checkout` do not render — there is no scope to describe.

**One component, one mount.** The transition is a class change on an ancestor, never an
unmount. C proves this is achievable and U02's acceptance depends on it.

#### B.9.3 Composer states

| State | Treatment |
|---|---|
| Empty | Placeholder visible; send at `opacity: .55` (the artifact's own disabled treatment), `disabled`, `aria-disabled="true"` |
| Draft | Placeholder gone; send at full opacity and enabled |
| Focused | Box border promotes toward `--primary` (C: `border-color` change on `:focus-within`). The artifact's only in-focus input treatment is the popover search's `1.5px solid var(--primary)` bottom border; apply the same colour as a full border here `[inferred]` |
| Sending | Send disabled; the draft is **not** cleared until the acknowledgement lands |
| Streaming | Send becomes **stop**. Same 30px circle, a square glyph, `--error` fill `[inferred]` — the artifact shows no stop control |
| Disabled by connection | Textarea stays editable so the user can keep drafting; send disabled with a stated reason in the `.settled` strip |
| Blocked scope | `.settled` renders the artifact's exact pattern: bold statement, recheck explanation, one action (`Un-block`) |
| Attachment staged | Chips render above the textarea inside the box |
| Attachment uploading | Chip shows progress and a cancel control |
| Attachment failed | Chip takes `--error` treatment with a retry control; the message must not send as if the file arrived |

#### B.9.4 Attachment chips

Not in the artifact; specified from POC C, restyled to the artifact's tokens.

`26px`+ high, `--muted` fill, `--border-2` hairline, `10px` radius, `6px 9px` padding,
`max-width: 260px`, `12px` label truncating with ellipsis. Image attachments show a
`48x48` `object-fit: cover` thumbnail at `5px` radius. A trailing remove button labelled
`Remove <name>`. Chips wrap in a `10px`-gapped row above the textarea.

Limits are the POC's and must be re-set by S03 against real Detent storage: 4 files,
1.5 MB each, 2 MB total, PNG/JPEG/WebP plus text types.

#### B.9.5 Whole-surface dropzone

From POC C, which handles the hard parts correctly and should be ported rather than
rewritten:

- The drop target is the **entire conversation surface**, not the composer.
- `dragenter`/`dragleave` are depth-counted with a ref so nested children do not flicker
  the overlay.
- Only `dataTransfer.types.includes("Files")` activates it, so dragging text or an element
  inside the page is not mistaken for an upload.
- The overlay is `position: absolute; inset: 12px`, `pointer-events: none`, a 2px dashed
  border in the accent colour, a `20px` radius, a near-opaque canvas wash, and two lines
  of copy: a `24px` heading and a dim supporting line.
- `paste` with `clipboardData.files` is handled on the same surface.
- A keyboard-equivalent path exists through the attach button and a hidden file input.

Restyled to the artifact: dashed border `--primary`, wash
`rgba(10,10,10,.94)`, radius `--radius`.

### B.10 Connection and delivery status

Two different things. Do not merge them.

**Connection (transport-level), top bar.** `.chip` at `26px`, `7px` radius, hover
`--accent`. Contents: a `status-dot` then a label then a dim detail.

| State | Dot | Copy |
|---|---|---|
| Live | `--success` with the `live` ping ring | `Live · data current · 12:18 AM` |
| Degraded | `--warning` | `Loop behind · 26m 20s` (the artifact's own example) |
| Reconnecting `[inferred]` | `--warning` | `Reconnecting… data as of HH:MM` — matches the existing Detent shell's degrade copy |
| Offline `[inferred]` | `--error` | `Offline · data as of HH:MM` |

The `live` ping is `::before` at `inset: -4px`, `--success` at `.35` opacity, a 2s
8-step animation. `@media (prefers-reduced-motion: reduce)` disables every animation in
the artifact; keep that rule.

**Delivery (per-message), transcript.** Use the `.tool` receipt row (B.8.4). One row per
state transition, or one row updated in place. Vocabulary from R04: `queued`, `sent`,
`acknowledged`, `completed`, `rejected`, `unknown`.

| Delivery state | Dot | Rule |
|---|---|---|
| queued | `--warning` | Name the reason (`waiting for a slot (0 / 2 available)`) |
| sent | `--muted-fg` | Only when the write actually left; not a synonym for acknowledged |
| acknowledged | `--info` | Only where the provider genuinely acknowledges |
| completed | `--success` | Terminal success |
| rejected | `--error` | Terminal failure with a stated cause and a retry path |
| unknown | `--warning` | Never rendered as "failed, retry". Render the consequence sentence and offer `Recover history` |

**Pill vocabulary** for statuses that belong on cards and property rows, all `20px` at
`11px/500`, `6px` radius: `mute` (`--muted` on `--border`), `ok`, `warn`, `err`, `info`,
`pri`. Observed labels: `Waiting · retry 14m`, `Waiting · 1`, `Blocked · 1`,
`Needs review`, `Human recovery`, `Held · no predicate`, `CI pass`, `High`, `Normal`,
`Low`, `paused`, `stalled`, `effort · medium`.

**Counts** (`.count`): `20px` minimum width, `6px` radius, `--accent-2` fill,
`--muted-fg` text, tabular. The `err` variant swaps to `--error-s`/`--error` (the Blocked
lane's count of 11 uses it). The `pri` variant is solid `--primary` on white.

### B.11 Empty, error, and loading states

**Empty.** Three treatments, chosen by container:

| Container | Treatment | Example |
|---|---|---|
| Sidebar section | `.srow.empty` — `30px`, `12px`, `--muted-fg-2`, non-interactive | `Nothing running · 0 / 2 slots` |
| Lane / panel | `.scr-1 .empty` — `22px 12px`, centred, `12px`, `--muted-fg-2`, and a second dim line carrying the most recent relevant fact | `Nothing is merging.` / `Last merge parable #2075 · 1h ago` |
| Full surface | Centred block: `13px` `--muted-fg-2` statement plus one `--muted-fg` action | `No hosts yet` / `+ Add a host` |

The artifact's rule is worth stating: an empty state carries **a fact, not an
illustration**. `Nothing is merging` is followed by when something last merged. Apply the
same to chat: `No chats yet` should be followed by the create action, not a graphic.

**Error.** Two treatments:

- Inline notice (`.warnbox`): `12px 14px`, `10px` radius, semantic tinted fill and a
  30%-alpha border of the same hue, a leading icon nudged `2px` down, `13px` text with the
  headline in `<b>`, a spacer, then **one** small button that resolves it (`Clear
  breaker`). Use `--error-s` for failures and `--warning-s` for degradations.
- In-transcript failure: the `.tool` receipt row in its `rejected` form, plus the retry
  panel from POC C when an outbox entry survives.

**Loading.** The artifact shows no skeleton or spinner. The existing Detent `input.css`
defines a `dt-skeleton` keyframe over `--color-elev`. Decision `[inferred]`: reuse that
approach in the mounted client — a `--muted`-based shimmer on the shape being loaded, no
spinners, no full-page blocking overlay. Streaming text needs no skeleton; the caret is
the indicator. POC C's fallback (`Opening conversation…` with a back link) is the correct
treatment for a conversation that has not resolved yet.

### B.12 Narrow widths (< 768px)

The artifact declares itself desktop-first ("best at 1440px+") and contains **no**
responsive rules for the product screens. Its only media query disables animation for
reduced motion. This section is therefore `[inferred]` throughout, built from the existing
Detent shell's behaviour (which already implements an off-canvas sidebar at the `md`
breakpoint) and POC C's breakpoints.

Breakpoint: `768px`, matching Tailwind's `md` and therefore the existing Templ shell.

**Sidebar.**

- Below 768px the 296px sidebar leaves the flow and becomes an **off-canvas drawer**:
  fixed, full height, translated off-screen, revealed by a hamburger button in the top
  bar, with a scrim that closes it. This is exactly what `shell.templ` already does with
  `data-mobile-open`; reuse the pattern so both surfaces behave identically.
- The drawer keeps its full 296px width and its full content: search, project switcher,
  Chats, the three issue sections, Browse. Nothing is dropped.
- Opening the drawer moves focus into it; Escape and the scrim close it and return focus
  to the trigger.
- Selecting any row closes the drawer.
- The desktop icon-rail collapse (the existing shell's `data-rail`) does **not** apply
  below 768px; a drawer and a rail are not both needed.

**Top bar.**

- Gains a leading hamburger at `44x44` minimum.
- The breadcrumb keeps only the current segment; leading context segments collapse.
- Page actions collapse behind one overflow button, matching the existing shell's
  `data-mobile-topbar-controls` popover.
- The Live chip keeps its dot but drops the dim detail string.

**Transcript.**

- `.col` side padding drops from `32px` to `16px`; `max-width` stops applying.
- The user bubble's `max-width` grows from `86%` to `92%` (C uses `90%`).
- Assistant type stays at `14.5px`; do not shrink body text.

**Composer.**

- `.composer-wrap` padding drops to `0 12px 12px`.
- The three scope chips (if present as read-only indicators) collapse to a single chip
  showing the model, with the rest available on tap `[inferred]`.
- The hint line's second half (the contextual sentence) is hidden; `Enter to send ·
  Shift + Enter for a new line` remains.
- The centred new-chat composer stays centred, at `min-height: 72px` and `15px` text.
- The `.checkout` strip does not render below 768px.

**Review dock.**

- The 560px dock cannot sit beside a conversation below 768px. It becomes a **full-screen
  overlay** over the thread, opened from the PR chip in the top bar and closed by an
  explicit close control. Mockup B stacked them at `40vh / 60vh`; that leaves both
  unusable and is rejected.
- V02's requirement that "conversation and composer remain available beside the dock"
  cannot hold at this width; the honest behaviour is one at a time with a fast switch.

**Touch targets.** Every interactive row and button reaches at least `44x44` below 768px.
The artifact's `30px` buttons and `32px` rows do not, and must grow.

### B.13 The one-new-chat-action rule

**Rule: one new-chat action per navigation context. Never two.**

This is the artifact's own principle, stated in its Notes tab for issues — "New issue is
the pencil on the switcher row, the one create action" — carried to conversations.

Concretely:

1. The **only** create affordance in the shell is the trailing icon on the project
   switcher row. In a chat navigation context it is a compose-pencil that starts a new
   chat in the current project scope. In an issue navigation context it creates an issue.
2. There is **no** "New chat" button in the top bar, **no** "New chat" row at the top of
   the Chats section, and **no** "＋ New issue" button in the conversation header. POC C
   renders that last one; it is rejected here.
3. The plan's D03 acceptance criterion — "no redundant Main chat button doing the same
   thing" — is satisfied by there being no persistent "Main chat" destination at all. An
   ordinary chat with no linked issue *is* the main chat. Navigating to `/chat` from the
   compose action is the only way to reach an empty composer.
4. A keyboard shortcut may duplicate the action (it is not a second visible control). If
   one is added it should follow the artifact's own `⌘⇧N` convention for creation.
5. U08's acceptance criterion — "Do not render a second duplicate summary outside the
   transcript as in the original POC" — is the same principle applied to output: one
   canonical rendering per thing.

### B.14 Keyboard and focus

- Every control is a real focusable element (B.6).
- Visible focus is `2px solid var(--primary)` at `1px` offset, on every interactive
  element, never removed.
- Tab order follows the visual order: sidebar, then top bar, then transcript, then
  composer, then dock.
- The composer textarea is the default focus target on `/chat` and after navigating into
  a conversation.
- `Enter` sends, `Shift+Enter` inserts a newline, and IME composition suppresses both
  (C implements the first two; composition handling is required by U02).
- `Escape` closes the project switcher popover, the mobile drawer, the handoff form, and
  the mobile dock overlay, each returning focus to its trigger.
- `/` focuses search (the artifact renders the hint as a `kbd`).
- The dock tabs advertise single-key shortcuts `D`, `V`, `S`, `R`, `A` in the artifact;
  these must not fire while focus is inside a text field.
- The streaming transcript is an `aria-live="polite"` region announcing completed turns,
  not every token.
- Pending questions receive focus when they appear, since they block progress.
- The transcript's return-to-latest control is reachable by keyboard.

---

## C. Implementation notes for the mounted client

The conversation client is a **React/Effect application reusing the T3 client runtime from
the POC, mounted under a same-origin authenticated route inside Detent**, coexisting with
the existing Templ/HTMX shell. It is not a Templ port. F02 confirms the route and build
ownership; this section records what the design side of that decision requires.

### C.1 Where the tokens live

The React application's own stylesheet defines the artifact's tokens as CSS custom
properties on the mount root, using **the artifact's exact names** so that copied markup
and future artifact revisions map one-to-one:

```css
/* conversation client stylesheet — scope to the mount root, not :root,
   so the surrounding Templ page keeps its own theme. */
[data-detent-conversation-root] {
  --bg: #0a0a0a;
  --card: #121212;
  --popover: #121212;
  --raised: #121212;

  --fg: #f5f5f5;
  --muted-fg: #8a8a8a;
  --muted-fg-2: #6b6b6b;

  --muted: rgba(255, 255, 255, 0.03);
  --accent: rgba(255, 255, 255, 0.04);
  --accent-2: rgba(255, 255, 255, 0.06);

  --border: rgba(255, 255, 255, 0.06);
  --border-2: rgba(255, 255, 255, 0.1);
  --input: rgba(255, 255, 255, 0.08);

  --primary: #3f6af5;                 /* fallback, keep first */
  --primary: oklch(0.571 0.21 264);   /* wins where supported */
  --primary-fg: #fff;

  --success: #34d399;
  --warning: #fbbf24;
  --error: #f87171;
  --info: #60a5fa;
  --violet: #a78bfa;
  --orange: #fb923c;

  --success-s: rgba(52, 211, 153, 0.14);
  --warning-s: rgba(251, 191, 36, 0.14);
  --error-s: rgba(248, 113, 113, 0.14);
  --info-s: rgba(96, 165, 250, 0.14);

  --radius: 10px;
  --cr: 8px;
  --sidebar-w: 296px;

  --font: -apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif;
  --mono: ui-monospace, "SF Mono", SFMono-Regular, Menlo, Consolas, monospace;

  color-scheme: dark;
}
```

Additions the artifact uses inline and which should become named properties rather than
repeated literals:

```css
  --primary-s: rgba(63, 106, 245, 0.18);   /* primary pill fill */
  --primary-fg-soft: #9db4ff;              /* primary pill text, active step number */

  --diff-add-bg: rgba(52, 211, 153, 0.09);
  --diff-add-fg: #8ee6c0;
  --diff-del-bg: rgba(248, 113, 113, 0.09);
  --diff-del-fg: #f4a0a0;
  --diff-ctx-fg: #c9c9c9;
  --diff-hunk-bg: rgba(96, 165, 250, 0.06);

  --live-ring: rgba(52, 211, 153, 0.35);
  --scroll-thumb: rgba(255, 255, 255, 0.08);

  --shadow-pop: 0 16px 48px rgba(0, 0, 0, 0.65), 0 0 0 1px rgba(0, 0, 0, 0.4);
  --shadow-composer: 0 8px 30px rgba(0, 0, 0, 0.45);
  --shadow-card: 0 1px 2px rgba(0, 0, 0, 0.3);

  --focus-ring: 2px solid var(--primary);
  --focus-offset: 1px;

  --h-bar: 52px;      /* side-top, side-bot, top, dock-tabs */
  --h-row: 36px;      /* .srow */
  --h-row-sm: 32px;   /* .srow.irow, .srow.sub */
  --h-ctl: 30px;      /* .btn */
  --h-ctl-sm: 26px;   /* .btn.sm, .chip */
  --h-chip: 20px;     /* .pill, .count, kbd */
  --w-dock: 560px;
  --w-col: 760px;     /* message column */
```

Project identity colours are **not** tokens. They arrive per project from the server and
are applied inline, exactly as the artifact does
(`style="color:#7b8cff"` on the project icon, `background` on the dot).

### C.2 Which tokens to mirror into `static/css/input.css`

The two surfaces must look like one product where a user crosses between them. Mirror the
values that are visible at the seam — the sidebar, the top bar, the canvas, the status
colours — into the Templ theme's `@theme` block, keeping Detent's existing
`--color-*` naming so no Templ class has to change:

| Detent `@theme` variable | Current value | Mirror to | Reason |
|---|---|---|---|
| `--color-page` | `#0b0d10` | `#0a0a0a` | Canvas must match across the seam |
| `--color-surface` | `#14171c` | `#121212` | Sidebar and card fill |
| `--color-elev` | `#1c2027` | `rgba(255,255,255,.06)` over page (`#1a1a1a` opaque equivalent) | Hover/raised fill |
| `--color-line` | `#262b33` | `rgba(255,255,255,.06)` (`#1c1c1c` opaque equivalent) | Hairline weight |
| `--color-text` | `#edf0f4` | `#f5f5f5` | Primary text |
| `--color-sec` | `#8a93a2` | `#8a8a8a` | Secondary text |
| `--color-dim` | `#808a97` | `#6b6b6b` | Tertiary text — **verify contrast before adopting**; the current value carries a comment saying it was brightened to hold WCAG AA on 11px footnotes, so this mirror may have to keep Detent's value |
| `--color-ok` | `#34d399` | unchanged | Already identical |
| `--color-warn` | `#fbbf24` | unchanged | Already identical |
| `--color-err` | `#f87171` | unchanged | Already identical |
| `--color-info` | `#60a5fa` | unchanged | Already identical |
| `--color-accent` | `#2dd4bf` (teal) | `oklch(0.571 0.21 264)` (blue) | **The one real conflict.** Detent's interactivity colour is teal; the artifact's is blue. They cannot both be the accent. |
| `--radius-card` | `6px` | `10px` | Artifact's card radius |
| `--radius-chip` | `4px` | `6px` | Artifact's chip radius |
| `--font-sans` | Geist | see C.4 | |
| `--font-mono` | Geist Mono | see C.4 | |

Two mirrors need a decision rather than an edit:

- **Accent.** Recommendation: adopt the artifact's blue as `--color-accent` across both
  surfaces, because the artifact is the newer direction and the accent appears on every
  focus ring, link, and selected state — a split would be visible immediately. The four
  semantic colours are already identical, so this is the only colour change with reach.
  Note that `--color-accent` currently doubles as the focus ring colour in `input.css`;
  changing it changes every focus ring, which is the intent.
- **`--color-dim`.** The existing value is documented as contrast-tuned. Re-run the
  contrast check in `docs/redesign-contrast.md` before lowering it to `#6b6b6b`; if it
  fails, keep Detent's value in the Templ theme and let the mounted client use the
  artifact's, since the client uses `--muted-fg-2` mostly for 11–12px non-essential text.

Do **not** mirror the spacing scale. Detent's `--spacing: 4px` strict grid and the
artifact's 2px grid are incompatible (B.3), and the Templ pages are built against the
4px grid today. The mounted client keeps its own scale.

Do **not** mirror the light theme. The artifact defines no light palette. Until one is
designed the mounted client is dark-only and should declare `color-scheme: dark` on its
root, so a Detent user in light mode gets a deliberately dark panel rather than a broken
one. Record this as a known limitation for O03.

### C.3 Scoping and isolation

- Define the tokens on the mount root (`[data-detent-conversation-root]`), not on
  `:root`. The Templ page around the mount keeps its own `--color-*` values, and neither
  stylesheet leaks into the other.
- The mounted client's stylesheet must not define bare element selectors that escape the
  mount (`body`, `html`, `a`, `button` at top level). The artifact's reset does exactly
  that; scope it.
- Tailwind's `@source` globs in `input.css` cover `internal/web/templates/**` and
  `internal/web/ui/**` only. The React application's files are outside them, so no
  Tailwind class scanning happens against the client — correct, and it should stay that
  way. The client ships its own CSS.

### C.4 Typography across the seam

The artifact uses the system font stack. The Templ shell preloads and uses Geist and
Geist Mono, already served from `/static/fonts/`.

Recommendation: **the mounted client uses Geist and Geist Mono**, with the artifact's
stacks as fallbacks:

```css
  --font: "Geist", -apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif;
  --mono: "Geist Mono", ui-monospace, "SF Mono", SFMono-Regular, Menlo, Consolas, monospace;
```

Reasons: the fonts are already served same-origin and preloaded by the shell, so there is
no additional cost; a font change at the seam is more visible than any of the colour
deltas; and Geist is metrically close enough to the system stack that the artifact's sizes
and line-heights transfer without retuning. If a review prefers the artifact's exact
system-font look, the change is one line and affects only the client — but then the shell
should be changed too, not left split.

### C.5 What to keep from the existing Detent shell

Keep, and do not reimplement in the client:

| Element | Where | Why |
|---|---|---|
| Authentication, session, and CSRF | `internal/web/server.go`, hosted auth | B02 requires reuse; the client mounts behind the same middleware |
| Routes `/`, `/fleet`, `/diagnostics`, `/health/ui`, `/reports`, `/library`, `/analytics`, `/api-keys`, `/settings`, `/projects/...` | `server.go` | All remain; the client adds routes, it removes none |
| `AppShell` topbar Live indicator and its degrade behaviour | `shell.templ`, `liveBehaviorScript` | The artifact's Live chip is the same component with different styling; the degrade copy ("Reconnecting… data as of HH:MM") is already correct and should be matched, not re-invented |
| Off-canvas sidebar at `md`, `data-mobile-open`, scrim, focus return | `shell.templ` | The client's narrow-width drawer (B.12) should behave identically |
| `StatusDot` primitive and its kinds | `internal/web/ui/primitives` | Same semantics as the artifact's `.status-dot` variants |
| The four semantic colours and the "accent is interactivity only" rule | `input.css` | Already aligned with the artifact's Notes tab |
| `dt-skeleton`, `dt-pulse`, `dt-tick` keyframes and the reduced-motion posture | `input.css` | Reuse for loading and live-tick treatments |
| Version string, build-update notice, connection notice | `shell.templ` | Global, must keep working while the client is mounted |
| The `#snapshot` morph rule | `CLAUDE.md`, board pages | Untouched. The client is a separate mount and must never be placed inside `#snapshot` or swapped by idiomorph |

Replace or retire:

| Element | Action |
|---|---|
| `ChatPanelHost` / `ChatConversation` / `ChatAction` in `chat.templ` — the 28rem right-hand operator chat drawer, its in-memory history, and its `POST /api/v1/chat/messages` form | **Superseded** by the mounted client. B03 states the goal explicitly: replace the in-memory/buffered limitation rather than create a second disconnected operator service. Keep it running behind the feature flag until the client reaches parity, then retire it. Its confirmation-gated action cards (`ChatAction`) carry real product logic worth porting to U07's approval treatment. |
| The topbar `Chat` toggle button (`data-chat-toggle`) | Becomes a **link to `/chat`** rather than a drawer toggle, once the client is enabled. |

### C.6 How a user gets from Board to Chat and back

The two surfaces are separate documents; navigation between them is ordinary page
navigation, and it must feel like one application.

**Templ shell to client.** Add `Chats` to `appShellNavGroups` in
`internal/web/templates/shell_data.go`, in the `primary` group beside `Work`:

```go
{
    ID: "primary",
    Items: []appNavItem{
        {ID: "board", Label: "Work",  Href: "/",     Icon: "kanban",         Active: active == "board"},
        {ID: "chat",  Label: "Chats", Href: "/chat", Icon: "message-circle", Active: active == "chat"},
    },
},
```

`appShellActiveNav` gains `"chat"` to its passthrough case. Behind the feature flag the
item is simply absent, satisfying F02's requirement that the feature can be disabled
without losing existing UI access.

**Client to Templ shell.** The client renders the artifact's full sidebar, including the
`Browse` group. Those rows are plain `<a href>` links to the existing Templ routes —
`Pull requests`, `Activity`, `Fleet` (`/fleet`), `Diagnostics` (`/diagnostics`),
`Reports` (`/reports`), `Library` (`/library`) — and a full page load back into the Templ
application is the correct and expected behaviour. The client must not try to render those
pages.

The client's `Work` entry links to `/`. Its project switcher's per-project gear links to
the existing settings route. Its issue-scoped conversation links to the existing issue
detail route where the user wants the full metadata surface.

**Server-side rendering of the shell, to avoid a flash.** The client's sidebar and top bar
duplicate the Templ shell's structure. Two options, and the choice belongs to F02:

- (a) The Templ shell renders around the mount, and the client renders only the main
  column. Cheapest, guarantees zero seam, but couples the client's layout to Templ and
  makes the transcript's absolutely-positioned composer harder to place.
- (b) The client renders the whole window including the sidebar, and the Templ shell is
  not rendered on `/chat*` routes at all. Matches the artifact's structure exactly and is
  what the POC does. Requires the client to reproduce the Browse rows, the version string,
  and the Live chip — all cheap — and requires the token mirroring in C.2 to be exact.

Recommendation: **(b)**, with C.2's mirroring making the seam invisible. It keeps the
client's layout self-contained, which every one of U01–U08's acceptance criteria assumes.

**Back navigation.** Browser back and forward must work across the seam without a special
case: the client uses real routes and real history entries (the POC already uses TanStack
Router with `/`, `/chats/$id`, `/issues/$id`), and links out of the client are ordinary
navigations. U01 and U06 both gate on this.

---

## Open decisions this document surfaces

These need an answer before the corresponding task can be marked done. None of them can be
resolved from the mockups alone.

1. **Accent colour** — teal (`#2dd4bf`, Detent today) or blue (`oklch(0.571 0.21 264)`,
   the artifact). Recommendation: blue, applied to both surfaces (C.2).
2. **Composer scope chips** — read-only indicators, or real per-turn overrides. Default is
   read-only until a capability advertises otherwise (A.7.4). Confirm in F03.
3. **Sidebar Chats section** — the artifact has none; adding one is an extension of the
   artifact and needs sign-off (A.9, 0.2).
4. **`Split` review mode** — the artifact dropped B's `Split` tab. Restore it as a sixth
   dock tab, as a Visual sub-mode, or leave it out (A.7.2). Decide in V03.
5. **Font at the seam** — Geist everywhere (recommended) or the artifact's system stack
   everywhere (C.4).
6. **`--color-dim` mirror** — needs a contrast re-check before adoption (C.2).
7. **Light theme** — the artifact defines none. Dark-only is the honest first release;
   record it as a limitation for O03 (C.2).
8. **Shell ownership on `/chat*`** — Templ shell around the mount, or client renders the
   whole window. Recommendation: the client renders the whole window (C.6). F02 decides.
9. **Mount prefix and route shapes** — `/chat`, `/chat/c/:id`, `/chat/i/:project/:issue`,
   `/chat/list` are proposals. F02 decides.
