# Patterns

A pattern says which [components](components.md) compose a screen or interaction and how they behave together. It does not take ownership of feature queries, permissions, drafts, mutations or navigation. Token values are in [Foundations](foundations.md).

- [Choosing controls](#choosing-controls)
- [Page template](#page-template)
- [Screen recipes](#screen-recipes)
- [Keyboard shortcuts](#keyboard-shortcuts)
- [Formatting values](#formatting-values)
- [Status indicators](#status-indicators)
- [Feedback and interaction states](#feedback-and-interaction-states)

## Choosing controls

Select an existing variant; do not restyle a primitive with arbitrary radius, fill, shadow or font overrides. A missing reusable appearance is a component-library decision, not a feature-level one.

| Context | Component choice | Nominal geometry |
| --- | --- | --- |
| Main form action | `Button` with default size and variant | 36px below `sm`, 32px from `sm` |
| Secondary form action | `Button variant="outline"` | Same size as the adjacent action |
| Toolbar or composer action | `Button size="compact"` | 28px, 12px text, 14px icon |
| Compact secondary action | `Button size="compact" variant="outline"` | 28px |
| Subtle toolbar action | `Button variant="ghost"` | Match adjacent controls |
| Low-emphasis destructive action in a menu or toolbar | `Button variant="ghost-destructive"` | Match adjacent controls |
| Destructive confirmation | `Button variant="destructive"` inside `AlertDialog` | Standard form sizing |
| Icon-only action | `Button` with an `icon`, `icon-sm` or `icon-xs` size | Accessible name required |
| Standard field | `Input` default size | 36px below `sm`, 32px from `sm`, the same as `Button` |
| Compact field | `Input size="compact"` | 30px |
| Single-choice picker | `Select` | Default min-height 36/32px; compact 28px |
| Searchable choice | `Combobox` or `Autocomplete` | Preserve keyboard and popup behaviour |
| On/off setting | `Switch` | Visible associated label |
| Independent selections | `Checkbox` | Visible associated label |
| Static status | `Badge` with a semantic variant | Badge density, independent of action size |

A default `Input` and a default `Button` render at the same height; the compact field is 2px taller than a compact button because its border sits outside the 28px line box. Preserve library geometry and align centres rather than forcing a universal height. Below `sm` editable fields use 16px text so mobile browsers do not zoom on focus, while compact controls keep sufficiently large, non-overlapping hit areas.

Menus contain actions; selects contain choices; popovers hold a small contextual interaction; preview cards reveal supplementary details; dialogs hold focused tasks; sheets adapt panels to constrained viewports. Use the primitives' positioning, layering and focus behaviour; never build a tooltip or menu in a feature folder.

Buttons own default, hover, pressed, focus and disabled styling. Use the native `disabled` attribute (or the primitive's `disabled` prop), never a look-alike class, so the control leaves the tab order and assistive technology reads it as unavailable. Loading keeps the action's label and layout, shows `Spinner` inside the button, sets `aria-busy`, and disables the button when a second submission would repeat the operation. Static information never takes a button appearance.

## Page template

Every non-chat page is the same composition. Build it from these parts and numbers, not from a copied screen.

```
AppSidebarLayout
├─ Sidebar                      16rem; a sheet below md
└─ main (min-w-0, min-h-0)
   ├─ WorkspacePageHeader       52px (--workspace-topbar-height); gutter px-3 → sm:px-5
   └─ scroll region (overflow-y-auto)
      └─ WorkspacePageContainer readable max-w-4xl | wide max-w-5xl | expanded max-w-6xl
         ├─ section             gap-6 between sections; gap-8 between major regions
         └─ section             heading text-sm font-medium; content gap-2 / gap-3
```

- **Gutter.** Header and content share `--workspace-gutter` (12px, 20px from `sm`, plus safe-area insets). Patch neither on its own.
- **Width.** `readable` (896px) is the default for settings, forms and prose; `wide` (1024px) for tables and lists; `expanded` (1152px) for boards and dashboards. Chat uses the reading lane (`--chat-content-max-width`, 46rem) instead.
- **Vertical rhythm.** The container pads `pt-6 pb-12`; sections are `gap-6`; rows inside a section are separated by borders or `gap-2`/`gap-3`, not by cards.
- **Breakpoints.** Below `md` the sidebar is a sheet behind a toggle in the header; at 980px and below the right panel is a sheet. Below `sm` controls step up to 36px and 16px text, and dialogs dock to the bottom.
- **Flexible boundaries.** Put `min-w-0` and `min-h-0` on every flex child that holds long content, so a long title truncates instead of widening the shell.
- **Empty, loading and error.** The page keeps its header and container in every state; only the content region changes (`Skeleton` rows, `Empty`, an inline `Alert`).

## Screen recipes

| Family | Recipe |
| --- | --- |
| Chat | Shared header, reading lane, timeline, composer with attached context and banners |
| Settings | Settings page container; grouped rows of label, description and trailing control; stack on narrow containers |
| Pull requests | Right-panel surface, breadcrumb, compact actions, semantic lifecycle labels, existing detail sections |
| Preview | Preview panel shell, address and actions, bounded viewport, distinct empty and unreachable states |
| Files | Breadcrumbs and compact toolbar above the tree or viewer; consistent unavailable and too-large states |
| Sidebar | Sidebar provider and row geometry; shared hover, active and selected token hierarchy |
| Usage | Page container, compact range control, aligned numbers, consistently labelled charts |
| Diffs | Diff panel shell, file navigation, syntax renderer, existing annotations and review controls |
| Device tools | The specialized device viewport and its rail inside the Preview surface; tool state stays local to that surface |
| Managed account | The account and authentication patterns of the settings screens; provider-specific behaviour stays with the provider |
| Desktop | Shared web UI with native inset and drag-region handling |
| Cloud connection | The existing connection, install and onboarding forms |
| Media | Bounded viewers with their own overlay controls |
| Authentication | `StandalonePage` with the sign-in or pairing form |
| Entry and onboarding | Standalone entry surface, `Wizard` and pairing forms |

### Workspace shell

Compose `AppSidebarLayout`, `WorkspacePageHeader`, the main region and the right panel. Use `min-w-0` and `min-h-0` at flexible boundaries so long titles and content do not widen the shell. Native titlebar hit areas and safe areas stay in the shell owner. Do not duplicate the header in a feature.

Non-chat pages follow the [page template](#page-template).

### Navigation

The thread sidebar uses the Sidebar primitive's semantic tokens and row variants. Active destination, temporary multi-selection, hover, disabled and collapsed are distinct states. Essential actions stay reachable by touch and keyboard; hover-only conveniences need an accessible alternative path. Global search and commands belong to the command palette; page-specific pickers use `Combobox` or `Menu`.

### Conversation and composer

Assistant content belongs to the reading lane; user content uses the message surface. `ChatMarkdown` renders headings, prose, code, tables, citations, links, diagrams and media for every markdown body. Tool activity belongs to the `MessagesTimeline` presentation. The prompt editor, preference controls, primary actions, attachments, context strip, pending input and attached banners stay in the `Composer` composition. Do not build a second composer for an issue discussion or another provider picker; the issue page reuses the composer.

Context references use the file tag chip and its states. A reference chip, a status badge and an action button are different components. Provider marks come from the existing icon mapping, never initials or redrawn logos.

The composer has its own rounded shape and attached-banner geometry; do not apply the control radius or a card to it.

### Settings and forms

One `SettingsRow` per setting: title, optional help and status, and a trailing control, inside a `SettingsSection` on a `SettingsPageContainer`. A row stacks below a 32rem container width and becomes two columns (`minmax(0,1fr)` and `minmax(10rem,auto)`, `gap-8`) from it; the row is a named container (`@container/settings-row`), because a settings panel's width does not follow the screen. Associate labels and help with their inputs and keep errors beside the field they affect. Reuse the existing reset and unavailable patterns where the setting supports them. Settings sections are navigated with `SettingsSidebarNav`, not tabs.

```tsx
<div className="grid gap-1.5">
  <Label htmlFor="project-name">Project name</Label>
  <Input
    id="project-name"
    value={name}
    onChange={(event) => setName(event.target.value)}
    aria-invalid={Boolean(error)}
    aria-describedby={error ? "project-name-error" : "project-name-help"}
  />
  {error ? (
    <p id="project-name-error" role="alert" className="text-xs text-error-foreground">
      {error}
    </p>
  ) : (
    <p id="project-name-help" className="text-xs text-muted-foreground">
      A short name for this project.
    </p>
  )}
</div>
```

### Work board and issues

The board keeps its own workflow semantics. [INV-13](../invariants.md#inv-13--a-board-card-is-a-title-and-one-status-line) limits an issue card to identity, title, at most one actionable status line of 48 Unicode characters, and the existing priority controls, with model and effort visibility as the invariant specifies for each density. This system adds no card metric, banner, badge or diagnostic. The orchestrator owns lane state; the board reads it.

The issue page composes the description, `ActivityFeed`, the shared composer and the properties column, whose pickers open one at a time. Change Request review stays on the linked change page.

### Right-panel surfaces

Diff, Files, Terminal, Output, Preview and pull-request surfaces live in the right panel behind `RightPanelTabs`, framed by their panel shells, and become a `RightPanelSheet` on narrow viewports. File syntax, terminal rendering, diff highlighting, media overlays and device viewports have specialized renderer contracts: adapt semantic colours at those boundaries rather than restyling every token. Terminal controls and cursor state are independent of prose typography. Each surface keeps its loading, waiting (`WorkspaceStatusView`), unavailable, empty and error states.

### Usage and tables

Use the page container and `Table`. Left-align labels, right-align comparable values and use tabular numerals. Use quiet dividers and the [chart series colours](foundations.md#categorical-colours): one stable colour per provider, neutral ink mixes for breakdown segments. Label chart meaning directly or through the legend, and keep units, range and the empty state. Format every value with the [shared formatters](#formatting-values). Usage pages carry product information, not operational diagnostics.

### Entry surfaces and platforms

The Cloud workspace, sign-in and local Templ pages have distinct contracts (see [Brand](brand.md)). Reuse semantic roles and hierarchy across them without spreading one surface's palette into another. Keep workspace styling separate from marketing: the marketing site sets no rule for the workspace, and the workspace borrows no marketing treatment (hero type, gradients, illustrations). Native mobile interfaces follow their platform's typography and touch geometry.

## Keyboard shortcuts

Bindings live in one table, `DEFAULT_BINDINGS` in [`src/app/adapters/keybindings.ts`](../../web/conversation/src/app/adapters/keybindings.ts), with a command id per action (`commandPalette.toggle`, `rightPanel.toggle`, `issue.status`…). A feature adds a command there and reads its label from `shortcutLabelForCommand`; it never listens for a chord of its own or writes a label by hand.

- **Chords.** A chord is `mod` plus a key, with `shift` and `alt` as qualifiers. `mod` is ⌘ on macOS and Ctrl elsewhere. Families share a key: `mod+b` toggles the sidebar, `mod+alt+b` the right panel, `mod+alt+shift+b` maximizes it.
- **Browser-reserved chords.** Detent runs in a browser tab, so it never binds a chord the browser owns: `mod+w`, `mod+t`, `mod+n`, `mod+l`, `mod+r`, `mod+q` and `mod+Tab`. Where a desktop convention uses one, Detent takes the nearest free chord in the same family (closing the right panel is `mod+alt+w`; a new chat is `mod+shift+o`).
- **Destructive actions** take a deliberate chord, never a bare letter (stopping a turn is `mod+shift+.`).
- **Single-key shortcuts** (`s`, `p`, `a`, `l`, `r` on an issue; `c` for a new issue; `/` for search) fire only when the reader is not typing: never from an input, textarea, select, `contenteditable`, or an element with role `textbox`, `searchbox` or `combobox` (`isEditableTarget` in `src/app/lib/shortcuts.ts`), and not while a dialog is open. A single-key shortcut duplicates a visible control; it is never the only way to an action.
- **Display.** `formatShortcutLabel` renders a chord. On macOS: modifier glyphs in the order ⌃ ⌥ ⇧ ⌘, then the key, with no separator (`⌥⌘B`). Elsewhere: words in the order Ctrl, Alt, Shift, Meta, then the key, joined by `+` (`Ctrl+Alt+B`). Letters are upper case; named keys are `Space`, `Esc`, `Up`, `Down`, `Left`, `Right`.
- **Where shortcuts appear.** In a tooltip and in the accessible name of its control as “Label (⌘K)”; at the end of a menu row with `MenuShortcut`; in the command palette with `CommandShortcut`; in prose and help with `Kbd` (`KbdGroup` for a sequence). Shortcut text uses `text-secondary-label`. Advertise a chord with `aria-keyshortcuts` on the control it activates.
- **Thread jump hints** (`mod+1` … `mod+9`) appear beside sidebar rows only while the modifier is held.

## Formatting values

Values are formatted by shared functions, never by hand. Each has one output shape:

| Value | Function | Output |
| --- | --- | --- |
| Clock time | `formatShortTimestamp` (`src/timestampFormat.ts`) | `2:30 PM` or `14:30`, by the timestamp preference (`locale`, `12-hour`, `24-hour`) |
| Time with its day | `formatDayAwareTimestamp` | `2:30 PM`, `yesterday at 2:30 PM`, `8/13 2:30 PM`, with the year when it differs |
| Full timestamp in a tooltip | `formatChatTimestampTooltip` | `14:04, 4th June 2026` |
| Relative time | `formatRelativeTimeLabel` | `just now` under a minute, then `5m ago`, `3h ago`, `2d ago` |
| Run or step duration | `formatDuration` (`src/runtime/support/orchestrationTiming.ts`) | `450ms`, `3.2s`, `42s`, `1m 3s`, `1h 2m 5s`; zero parts dropped |
| Time remaining | `formatDuration` and `formatResetsIn` (`src/app/adapters/usageLimits.ts`) | `12m`, `2h 13m`, `3d 4h`; `resets in 2h 13m` |
| Count | `formatCount` (`src/app/usage/usageFormat.ts`) | `12,345` |
| Tokens | `formatTokens` | three significant figures: `804K`, `76.7M`, `19.9B` |
| Cost | `formatUsd` | `$1,234.50` |
| Share | `formatPercent` | `12.3%`, `<0.1%` |
| Day and hour on a chart axis | `formatDayShort`, `formatDateTimeShort` | `Aug 7`, `Aug 11, 2 PM` |
| Commit SHA | `sha.slice(0, 7)` | `9dd7335`, in `font-mono` |
| Path, branch, worktree | `MiddleTruncate`, `formatWorkspaceRelativePath` | `web/conv…/Gallery.tsx`; `<workspace>/src/app/main.tsx:42` |

- Show the absolute time in a tooltip wherever a relative time is shown.
- Numbers that change in place or align use `tabular-nums`.
- A count carries its unit and agrees in number: `1 run`, `3 runs`, `0 runs` (never a blank for zero; see the four kinds of nothing in [Brand](brand.md#interface-voice)).
- Text truncated with an ellipsis keeps its full value in a tooltip or `title`.

## Status indicators

State is shown by one mapping, `ThreadStatusIndicators` with `resolveThreadStatusPill` (`src/components/Sidebar.logic.ts`); a feature never maps a state to a colour of its own.

| Status | Colour | Motion | Priority |
| --- | --- | --- | --- |
| Pending approval | amber | — | 5 |
| Awaiting input | indigo | — | 4 |
| Working, Connecting | sky | `status-pulse` | 3 |
| Waiting | `sidebar-muted-foreground` | — | 2.5 |
| Plan ready | violet | — | 2 |
| Completed (unseen) | emerald | — | 1 |
| Terminal running | teal | `status-pulse` | — |

- **Colour.** Text uses the 600 step in light and 300 at 90% in dark; the dot uses the 500 step in light. These hues are the status set, distinct from the semantic `success`/`warning` roles and from the categorical identity hues.
- **Anatomy.** The full form is a 6px dot (`size-1.5`) with a `text-3xs` label and `gap-1`; the label shows from `md`. The compact form is the dot alone in a `size-3.5` box. Both are `role="img"` with an `aria-label` and a tooltip carrying the label.
- **Priority.** A row that summarizes several threads (a project) shows its highest-priority status.
- **Connection and outcome dots** use the semantic roles: an 8px dot (`size-2`) in a `size-3` box, `bg-success` connected, `bg-warning` with a `status-ping` halo while connecting, `bg-destructive` failed, `bg-muted-foreground/40` offline. A dot overlaid on an icon is `size-2` with a 2px ring in the background colour.
- **Never colour alone.** A dot always has a label, a tooltip and an accessible name.

## Feedback and interaction states

| State | Presentation | Behaviour |
| --- | --- | --- |
| Rest | Component surface and foreground | Useful label stays visible |
| Hover | Accent or variant hover treatment | No layout shift |
| Pressed or selected | Component's pressed or selected treatment | Expose the accessible state |
| Focus | Ring or outline | Visible with keyboard; not clipped |
| Disabled | Disabled opacity | Prevent action; keep any existing explanation |
| Loading | `Spinner` or `Skeleton` | Preserve layout; expose loading without announcing every frame |
| Empty | `Empty` | Explain absence and offer the relevant action |
| Not set up | Muted text and one Set up action | An optional feature nobody turned on; never counts toward Needs attention |
| Validation error | Error foreground and field association | Keep the input and correction visible |
| Unavailable | Unavailable composition or `Alert` | Distinguish lack of capability from a failed attempt |
| Success | Success variant | Pair colour with a label or icon |
| Warning | Warning variant | A feature is turned on and a required piece is missing or its check failed; say what will not happen and link the fix |
| Error | Error variant | Something configured and working is now broken |
| Destructive confirmation | `AlertDialog` | Clear destructive action and cancel path |

Not every queued, idle or unavailable state is an error, and a selected item is not a success. Use a toast for transient feedback and an inline `Alert` when the user must act in place. Keep feedback text short and state the action or result, not the implementation.

Warning and error states appear only when something is misconfigured or broken, never because an optional feature was not set up. A not-set-up state says it is not set up in muted text and offers one Set up action, with no warning or error tokens and no `role="alert"`. It never counts toward Needs attention. For a Sprite pool, turned on means a ceiling above zero or any member that is not deleted.

```tsx
<div className="flex items-center gap-2">
  <Button size="compact" variant="outline">Cancel</Button>
  <Button size="compact">Save</Button>
</div>

<Badge variant="success">Completed</Badge>
```

## Motion and elevation

[Foundations](foundations.md) owns the motion, elevation and layer values. Reuse the existing transitions, skeleton cycle and status pulse rather than adding another shimmer or pulse, and honour reduced motion and the panel animation settings. Most structure uses dividers and subtle outlines with little or no shadow; floating menus and dialogs use their shared elevation, and the composer its own soft shadow. Glass and grain belong to established surfaces. Portal layering stays inside the primitives; do not scatter z-index values through features.
