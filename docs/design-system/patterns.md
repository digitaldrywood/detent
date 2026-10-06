# Patterns

A pattern says which [components](components.md) compose a screen or interaction and how they behave together. It does not take ownership of feature queries, permissions, drafts, mutations or navigation. Token values are in [Foundations](foundations.md).

## Choosing controls

Select an existing variant; do not restyle a primitive with arbitrary radius, fill, shadow or font overrides. A missing reusable appearance is a component-library decision, not a feature-level one.

| Context | Component choice | Nominal geometry |
| --- | --- | --- |
| Main form action | `Button` with default size and variant | 36px narrow, 32px at `sm` |
| Secondary form action | `Button variant="outline"` | Same size as the adjacent action |
| Toolbar or composer action | `Button size="compact"` | 28px, 12px text, 14px icon |
| Compact secondary action | `Button size="compact" variant="outline"` | 28px |
| Subtle toolbar action | `Button variant="ghost"` | Match adjacent controls |
| Destructive confirmation | `Button variant="destructive"` inside `AlertDialog` | Standard form sizing |
| Icon-only action | `Button` with an `icon`, `icon-sm` or `icon-xs` size | Accessible name required |
| Standard field | `Input` default size | 34px narrow, 30px at `sm` |
| Compact field | `Input size="compact"` | 28px |
| Single-choice picker | `Select` | Default min-height 36/32px; compact 28px |
| Searchable choice | `Combobox` or `Autocomplete` | Preserve keyboard and popup behaviour |
| On/off setting | `Switch` | Visible associated label |
| Independent selections | `Checkbox` | Visible associated label |
| Static status | `Badge` with a semantic variant | Badge density, independent of action size |

Default inputs and buttons differ slightly in height. Preserve library geometry and align centres rather than forcing a universal height. On narrow screens editable fields stay at 16px or larger where needed to avoid mobile zoom, while compact controls keep sufficiently large, non-overlapping hit areas.

Menus contain actions; selects contain choices; popovers hold a small contextual interaction; preview cards reveal supplementary details; dialogs hold focused tasks; sheets adapt panels to constrained viewports. Use the primitives' positioning, layering and focus behaviour; never build a tooltip or menu in a feature folder.

Buttons own default, hover, pressed, focus and disabled styling. Loading keeps the action's label and layout, uses `Spinner`, and prevents duplicate submission when the operation requires it. Static information never takes a button appearance.

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
| Desktop | Shared web UI with native inset and drag-region handling |
| Media | Bounded viewers with their own overlay controls |
| Entry and onboarding | Standalone entry surface, wizard and pairing forms |

### Workspace shell

Compose `AppSidebarLayout`, `WorkspacePageHeader`, the main region and the right panel. Use `min-w-0` and `min-h-0` at flexible boundaries so long titles and content do not widen the shell. Native titlebar hit areas and safe areas stay in the shell owner. Do not duplicate the header in a feature.

Non-chat pages put their content in `WorkspacePageContainer` at the `readable`, `wide` or `expanded` width. Header and content share one gutter; patch neither independently.

### Navigation

The thread sidebar uses the Sidebar primitive's semantic tokens and row variants. Active destination, temporary multi-selection, hover, disabled and collapsed are distinct states. Essential actions stay reachable by touch and keyboard; hover-only conveniences need an accessible alternative path. Global search and commands belong to the command palette; page-specific pickers use `Combobox` or `Menu`.

### Conversation and composer

Assistant content belongs to the reading lane; user content uses the message surface. `ChatMarkdown` renders headings, prose, code, tables, citations, links, diagrams and media for every markdown body. Tool activity belongs to the `MessagesTimeline` presentation. The prompt editor, preference controls, primary actions, attachments, context strip, pending input and attached banners stay in the `Composer` composition. Do not build a second composer for an issue discussion or another provider picker; the issue page reuses the composer.

Context references use the file tag chip and its states. A reference chip, a status badge and an action button are different components. Provider marks come from the existing icon mapping, never initials or redrawn logos.

The composer has its own rounded shape and attached-banner geometry; do not apply the control radius or a card to it.

### Settings and forms

One `SettingsRow` per setting: title, optional help and status, and a trailing control, inside a `SettingsSection` on a `SettingsPageContainer`. Rows stack on narrow containers; base the breakpoint on the container, not the screen. Associate labels and help with their inputs and keep errors beside the field they affect. Reuse the existing reset and unavailable patterns where the setting supports them. Settings sections are navigated with `SettingsSidebarNav`, not tabs.

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

Use the page container and `Table`. Left-align labels, right-align comparable values and use tabular numerals. Use quiet dividers and one stable colour per series; label chart meaning directly or through the legend, and keep units, range and the empty state. Usage pages carry product information, not operational diagnostics.

### Entry surfaces and platforms

The Cloud workspace, sign-in and local Templ pages have distinct current contracts (see [Brand](brand.md)). Reuse semantic roles and hierarchy across them without spreading one surface's palette into another. Native mobile interfaces follow their platform's typography and touch geometry.

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
| Validation error | Error foreground and field association | Keep the input and correction visible |
| Unavailable | Unavailable composition or `Alert` | Distinguish lack of capability from a failed attempt |
| Success | Success variant | Pair colour with a label or icon |
| Warning | Warning variant | State the consequence and the corrective action |
| Destructive confirmation | `AlertDialog` | Clear destructive action and cancel path |

Not every queued, idle or unavailable state is an error, and a selected item is not a success. Use a toast for transient feedback and an inline `Alert` when the user must act in place. Keep feedback text short and state the action or result, not the implementation.

```tsx
<div className="flex items-center gap-2">
  <Button size="compact" variant="outline">Cancel</Button>
  <Button size="compact">Save</Button>
</div>

<Badge variant="success">Completed</Badge>
```

## Motion and elevation

[Foundations](foundations.md) owns the motion, elevation and layer values. Reuse the existing transitions, skeleton cycle and status pulse rather than adding another shimmer or pulse, and honour reduced motion and the panel animation settings. Most structure uses dividers and subtle outlines with little or no shadow; floating menus and dialogs use their shared elevation, and the composer its own soft shadow. Glass and grain belong to established surfaces. Portal layering stays inside the primitives; do not scatter z-index values through features.
