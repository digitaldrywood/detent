# Detent Design System

Use a compact workspace language for Detent: quiet neutral surfaces, semantic colors, small consistent controls, and familiar panels. Consistent output comes from choosing the same components and compositions for the same task. Agents should select from this system before styling a screen.

This is the design reference for Detent’s Cloud UI. Use it to select components, semantic tokens, and screen compositions for authorized UI work. Current implementation details are identified separately from proposed layout baselines and optional components. Product behavior and the repository invariants remain authoritative.

## Scope and implementation

This guide owns shared visual rules and component selection. The [screen inventory](design-inventory.md) owns screen coverage, and [product decisions](decisions.md) own behavior. When their historical styling examples differ from this guide, use this guide for visual rules and the existing implementation for behavior. Existing public sign-in and local Templ surface contracts are retained until a scoped change explicitly adopts shared styling.

The system covers 46 grouped component patterns and the workspace compositions that use them. Detent currently provides 34 of the catalogued primitives; the remaining 12 are proposed options for features that need them. The [component catalog](component-catalog.json) records each primitive’s group, availability, and existing Detent path.

Runtime themes, browser font metrics, opacity, and specialized renderers affect the final appearance. Validate the relevant states against the rendered interface when making visual changes; token values alone do not establish accessibility.

The implementation owners are the [shared stylesheet](../../web/conversation/src/app/global.css), [Detent overrides](../../web/conversation/src/app/index.css), and [component library](../../web/conversation/src/components/ui). Keep reusable appearance in those owners and feature layout in the existing screen compositions.

## Design decisions

1. Reuse an existing Detent primitive first. Its public props own appearance and behavior; callers own placement and layout.
2. Use semantic roles for surfaces, text, actions, feedback, and selection. A brand action and a successful outcome have different roles.
3. Keep workspace chrome compact. Let reading content, forms on phones, and touch hit areas have the space they need.
4. Prefer rows, separators, and existing panels. Use cards for grouped entities or consequential inline requests, rather than wrapping every section in a card.
5. Preserve the whole composition of the composer, sidebar, and right panel. Their attached surfaces and responsive behavior are part of the design.
6. Keep typography, icons, spacing, and state treatments consistent across features. Exceptions belong to named components.
7. Keep useful labels and keyboard focus visible. A tooltip supplements an action; it cannot supply its only accessible name.

The system uses a Zinc-based neutral palette, Lucide icons, Base UI interaction primitives, and Tailwind v4. Use the installed Detent library; adopting these rules does not require regenerating components or upgrading dependencies.

## Color roles

Use Tailwind’s semantic utilities. The foreground of a surface is the text color for that surface. `muted` and `accent` are fills; `muted-foreground` and `accent-foreground` are content colors. `accent` is a subtle interaction surface; `primary` supplies the solid action color.

| Role | Existing token or utility | Use |
| --- | --- | --- |
| Canvas | `background`, `foreground` | Main workspace and default text |
| Grouped surface | `card`, `card-foreground` | Cards and grouped content |
| Floating surface | `popover`, `popover-foreground` | Menus, pickers, floating panels |
| Primary action | `primary`, `primary-foreground` | Main action, send, active switches |
| Secondary action | `secondary`, `secondary-foreground` | Lower emphasis actions |
| Subtle interaction | `accent`, `accent-foreground` | Hover and highlighted items |
| Supporting content | `muted-foreground`, `secondary-label`, `placeholder`, `icon-muted` | Metadata, labels, placeholders, low emphasis icons |
| Boundaries | `border`, `input`, `ring` | Dividers, control outlines, focus |
| Feedback | `success`, `info`, `warning`, `error` with their foregrounds | Outcome, information, caution, failure |
| Destructive action | `destructive`, `destructive-foreground` | Actions that remove or destroy |
| Sidebar | `sidebar-*` | Sidebar fill, labels, hover, active, selected states |
| Conversation | `message-*` | Message fill, content, and message actions |
| Specialized renderer | `code-*`, `terminal-*`, theme adapters | Code, syntax, terminal cursor and selection |

### Reference palette

These are baseline reference values before sidebar-specific overrides, imported themes, and contrast adjustments. Detent’s current primary override is described below. The values are not the resolved color of every screen.

| Role | Light | Dark |
| --- | --- | --- |
| Canvas | Zinc 25, `oklch(99.2% 0 0)` | Neutral 950 |
| Foreground | Zinc 800 | Neutral 100 |
| Card and popover | White | Canvas mixed with 3% white |
| Primary | `oklch(0.488 0.217 264)` | `oklch(0.571 0.21 264)` |
| Muted | Zinc 50 | White at 3% alpha |
| Accent | Zinc 100 | White at 4% alpha |
| Border | Zinc 200 | White at 6% alpha |
| Input outline | Zinc 300 | White at 8% alpha |
| Supporting text | Zinc 500 | Neutral 500 mixed with 10% white |
| Error foreground | Red 700 | Red 400 |
| Warning foreground | Amber 700 | Amber 400 |
| Success foreground | Emerald 700 | Emerald 400 |
| Info foreground | Blue 700 | Blue 400 |

Several Tailwind semantic colors map through `--contrast-*` variables in Detent. Preserve that indirection when available. Prefer `text-muted-foreground` to reading `--muted-foreground` directly. Geometry should not depend on a specific palette.

### Detent theme ownership

Proposed rule: Cloud workspace components use the semantic roles already in `global.css`; `index.css` owns Detent-specific overrides. Avoid putting a second palette inside a feature component. Current Detent overrides primary with `oklch(0.571 0.21 264)` at the root. Preserve this value during initial adoption; changing the light primary is a separate visual decision.

The public sign-in surface currently has a scoped teal palette and Geist-first fonts. Local Templ styling in `static/css/input.css` also uses teal, Geist-first fonts, 6px cards, and 4px chips. Keep these as separate current surface contracts. Do not spread those choices into the Cloud workspace or replace them as an incidental part of adoption.

## Typography

Detent’s workspace prefers system sans fonts, with system monospace for code and Geist fallbacks. Interface size can affect rem-based dimensions; nominal pixel values below assume a 16px root. Prompt and code size preferences have separate ownership.

| Purpose | Detent rule | Nominal size |
| --- | --- | --- |
| Body, field labels, ordinary controls | `text-sm`, normal or medium | 14px |
| Compact controls and metadata | `text-xs` | 12px |
| Exceptional dense annotation | An existing 11px treatment; reuse only where available and necessary | 11px |
| Reading text and prompt | Existing renderer and prompt preference | Typically 14–16px by context |
| Section heading | `text-sm font-medium` | 14px |
| Dialog title | Existing `DialogTitle` | `text-xl font-semibold` |
| Page or onboarding headline | Existing page recipe; proposed headline range | 24–30px |
| Code, paths, terminal | `font-mono` and renderer preference | Context-specific |

Ordinary metadata stays at 12px; 11px is a narrow exception, and smaller sizes remain confined to existing specialty components. Do not copy tiny sizes into new forms, notices, or important status text. Do not add typography tokens merely to match these exceptions.

Use regular weight for prose, medium for labels and actions, and existing heading weights for hierarchy. Use tabular numerals for aligned numeric columns, counters, and durations. Use truncation for constrained identity labels; allow descriptions, validation messages, and actionable status to wrap where their container permits it.

## Spacing and geometry

Use the existing Tailwind spacing scale. A 4px base with 2px and 6px intermediate steps matches the observed compact patterns. Choose spacing by purpose rather than the feature author’s preference.

| Purpose | Rule | Nominal value |
| --- | --- | --- |
| Tight icon and label pair | `gap-1` or `gap-1.5` | 4px or 6px |
| Ordinary control group | `gap-2` | 8px |
| Toolbar or content row | `gap-3` | 12px |
| Section content inset | `p-3` or `p-4`, as the recipe specifies | 12px or 16px |
| Section separation | `gap-6` or `gap-8` for reading/settings pages | 24px or 32px |
| Control corner | `--control-radius` | 8px |
| Base radius | `--radius` | 10px |
| Derived radii | `sm`, `md`, `lg`, `xl`, `2xl`, `3xl` | 6, 8, 10, 14, 18, 22px |
| Sidebar insets and control gap | Existing semantic variables | 8px |
| Sidebar row inset | `--sidebar-row-content-inset` | 10px |
| Floating content inset | `--floating-content-inset` | 12px |
| Workspace top bar | `--workspace-topbar-height` | 52px default |
| Sidebar default width | Existing sidebar provider | 16rem; collapsed 3rem |
| Chat content reference | Proposed reading-lane baseline | 46rem |
| Thread details reference | Proposed detail-panel baseline | 17.5rem |

The composer has its own larger rounded shape and attached context-strip geometry. Reuse Detent’s existing composer composition; do not apply the control radius to it. Current Detent uses `max-w-3xl` for the composer and an existing 20px corner treatment. Retain that composition until an authorized visual change adopts another reading width.

Detent’s existing `WorkspacePageHeader` uses a 12px gutter plus safe-area insets. Proposed adoption should choose one shared gutter for header and content rather than independently patching either; 12px on narrow screens and 20px at `sm` is the proposed baseline. Proposed readable, wide, and expanded page containers use `max-w-4xl`, `max-w-5xl`, and `max-w-6xl`.

## Controls

Select an existing variant; do not restyle a primitive with arbitrary radius, fill, shadow, or font overrides. A missing reusable appearance is a component-library decision. Existing Detent props are the implementation authority until an authorized adoption updates them.

| Context | Component choice | Nominal geometry |
| --- | --- | --- |
| Main form action | `Button` with default size and variant | 36px narrow, 32px at `sm` |
| Secondary form action | `Button variant="outline"` | Same size as adjacent action |
| Toolbar or composer action | `Button size="compact"` | 28px, 12px text, 14px icon |
| Compact secondary action | `Button size="compact" variant="outline"` | Same 28px height |
| Subtle toolbar action | Existing ghost variant | Match adjacent controls |
| Destructive confirmation | `Button variant="destructive"` in existing confirmation pattern | Standard form sizing |
| Icon-only action | Existing `icon`, `icon-sm`, or `icon-xs` size | Accessible name required |
| Standard field | `Input` default size | 34px narrow, 30px at `sm` |
| Compact field | `Input size="compact"` | 28px |
| Single-choice picker | Existing `Select` composition | Default min-height 36/32px; compact 28px |
| Searchable choice | Existing `Combobox` or `Autocomplete` | Preserve keyboard and popup behavior |
| On/off setting | Existing `Switch` | Use a visible associated label |
| Independent selections | Existing `Checkbox` | Use a visible associated label |
| Static status | Existing `Badge` with semantic variant | Existing badge density, independent of action size |

Default inputs and buttons differ slightly in source height. Preserve library-defined geometry; align their centers instead of forcing every element to an invented universal height. On narrow screens editable fields must remain at least 16px when needed to avoid mobile browser zoom, while visually compact controls retain sufficiently large, non-overlapping hit areas.

Buttons own default, hover, pressed, focus, and disabled styling. Preserve native disabled behavior, focus rings, accessible labels, and loading semantics. Loading retains the action label and layout, uses the existing spinner, and prevents duplicate submission when the operation requires it. Static informational states should not receive a button appearance.

## Complete primitive catalog

The 46 component patterns are grouped below. Detent already has 34. The 12 marked Proposed are options for a requested feature that needs them; absence is not itself an adoption task.

The system groups elements into foundations, layout, navigation, actions, forms, overlays, feedback, data display, entry flows, and specialized workspace components. Foundations own color, type, spacing, elevation, and motion. The catalog below groups reusable primitives; the following screen recipes group the larger compositions that use them.

| Family | Primitives | Detent availability |
| --- | --- | --- |
| Actions | `button`, `toggle`, `toggle-group`, `panel-tab-close-button`, `refresh-icon` | Present |
| Basic forms | `input`, `textarea`, `label`, `checkbox`, `switch` | Present |
| Choice and search | `select`, `combobox`, `autocomplete` | Present |
| Extended forms | `radio-group`, `number-field`, `color-picker`, `input-group`, `draft-input`, `calendar` | Proposed |
| Overlays | `dialog`, `alert-dialog`, `sheet`, `popover`, `menu`, `tooltip`, `preview-card` | Present |
| Feedback | `alert`, `badge`, `toast`, `spinner`, `skeleton`, `empty` | Present |
| Navigation | `sidebar`, `command` | Present |
| Structure | `scroll-area`, `separator`, `group`, `collapsible` | Present |
| Extended structure | `collapsible-section-header`, `middle-truncate` | Proposed |
| Data display | `table`, `kbd` | Present |
| Extended data display | `discovery-list` | Proposed |
| Entry and onboarding | `standalone-page`, `wizard`, `qr-code` | Proposed |

Menus contain actions; selects contain choices; popovers hold a small contextual interaction; preview cards reveal supplementary details; dialogs hold focused tasks; sheets adapt panels to constrained viewports. Use the existing library’s positioning and focus behavior. Avoid creating a separate tooltip or menu implementation in a feature folder.

## Screen and component recipes

Workspace compositions are grouped below by responsibility. Existing shells, headers, chips, coordinators, and layout wrappers should remain shared across these groups.

| Family | Recipe for Detent |
| --- | --- |
| Chat | Shared header, reading lane, timeline, composed prompt surface, attached context and banners |
| Settings | Shared page shell; grouped rows with label/description and trailing control; stack on narrow containers |
| Pull requests | Shared panel shell, breadcrumb, compact actions, semantic lifecycle labels, existing detail sections |
| Preview | Shared surface chrome, address/actions, bounded viewport, distinct empty and unreachable states |
| Device tools | Existing specialized viewport and rail; informational tool state remains local to its surface |
| Files | Breadcrumb and compact toolbar above browser or viewer; consistent unavailable/too-large states |
| Managed account integration | Existing account and authentication patterns; retain provider-specific behavior |
| Sidebar | Shared sidebar provider and row geometry; shared hover/active/selected token hierarchy |
| Usage | Shared page container, compact range/tabs, aligned numbers, consistent labeled charts |
| Diffs | Shared diff shell, file navigation, syntax renderer, existing annotations and review controls |
| Desktop | Shared web UI with native inset/drag-region handling |
| Cloud connection | Existing connection, install, and onboarding forms |
| Media | Existing bounded viewers with specialized overlay controls |
| Authentication | Existing standalone entry surface and pairing form |
| Onboarding | Existing wizard and first-run composition |

### Workspace

Compose the existing sidebar, `WorkspacePageHeader`, main area, and existing right panel. Use `min-w-0` and `min-h-0` at flexible layout boundaries so long titles and content do not push the shell wider. Keep native titlebar hit areas and safe areas in the shell owner. Do not duplicate the header in each feature.

### Navigation

Use the sidebar’s semantic tokens and existing row variants. Active destination, temporary multi-selection, hover, disabled, and collapsed modes remain distinct states. Preserve access to essential actions on touch and keyboard; hover-only convenience controls must have an existing accessible alternate path.

### Conversation and composer

Assistant content belongs to the reading lane; user content uses the existing message surface. Reuse the existing Markdown renderer for headings, prose, code, tables, citations, links, diagrams, and media. Tool activity belongs to the existing timeline presentation. Keep the prompt surface, preference controls, primary action, attachments, context strip, pending input, and attached banners in their existing composition. Do not build a second composer for an issue discussion or invent another provider picker.

Context references use the existing chip component and its interaction states. A reference chip, a status badge, and an action button are different components. Provider marks should use the existing icon library and established provider mapping rather than initials or newly drawn logos.

### Settings and forms

Use one grouped row for each setting: label, optional help/status, and its control. The proposed layout changes from a stack to two columns at a 32rem container width. Apply that pattern through the existing Detent setting layout when adopted; do not assume screen width predicts available panel width. Associate labels and help with inputs, and keep errors beside the field they affect. Reuse existing inheritance and reset patterns where the setting already supports them.

### Work board and issues

Keep the board’s own workflow semantics. INV-13 requires identity, title, at most one actionable status line of 48 Unicode characters, and existing priority controls. Retain model and effort visibility as the invariant specifies for each density. This system adds no card metric, banner, badge, or diagnostic content. Issue detail can use the existing detail and timeline composition, within the authorized UI scope.

### Files, diffs, terminal, preview, and media

Use the existing surface shell, compact toolbar, breadcrumbs, and tabs. File syntax, terminal rendering, diff highlighting, media overlays, and device viewports have specialized renderer contracts. Adapt semantic colors at those boundaries; do not replace them with generic cards or manually style every token. Keep terminal controls and cursor state independent of prose typography. Retain existing loading, unavailable, empty, and error states for each surface.

### Usage and tables

Use the existing page and table components. Left-align labels, right-align comparable values, and use tabular numerals. Use quiet dividers and one stable semantic color mapping per series. Label chart meaning directly or through the existing legend; preserve units, time range, and the current data’s empty state. Chart and table changes must not add operational diagnostics to product UI.

### Entry surfaces and platform adaptation

Cloud workspace, sign-in, and local Templ pages have distinct current contracts. Keep workspace styling separate from marketing. Reuse semantic roles and hierarchy across surfaces; native mobile interfaces should follow their platform’s typography and touch geometry. Specialized overlays should consume the same semantic theme roles as the surrounding workspace.

## Feedback and interaction states

| State | Presentation | Behavior |
| --- | --- | --- |
| Rest | Existing component surface and foreground | Useful label remains visible |
| Hover | Existing accent or variant hover treatment | No layout shift |
| Pressed or selected | Component’s pressed/selected treatment | Expose the appropriate accessible state |
| Focus | Existing ring or outline | Visible with keyboard; not clipped |
| Disabled | Existing opacity and disabled treatment | Prevent action; preserve an existing explanation where provided |
| Loading | Existing spinner or skeleton | Preserve layout; expose loading without announcing every frame |
| Empty | Existing `Empty` composition | Explain absence and offer an existing relevant action |
| Validation error | Error foreground and field association | Keep input and correction visible |
| Unavailable | Existing unavailable composition | Distinguish lack of capability from a failed attempt |
| Success | Success foreground/variant | Use a label or icon with color |
| Warning | Warning foreground/surface | State the consequence and existing corrective action |
| Destructive confirmation | Existing alert-dialog pattern | Clear destructive action and cancel path |

Do not treat every queued, idle, or unavailable state as an error. A selected item is not automatically a successful outcome. Use toasts for transient feedback and an existing inline state when the user must act in that location. Keep feedback text concise and state the action or result rather than the internal implementation.

## Motion and elevation

Use the existing 150–200ms control transitions. The existing skeleton has a 2.4s stepped opacity cycle; status pulse and ping use 2s cycles to reduce compositor work. Reuse these animations rather than adding a different shimmer or pulse. Honor reduced-motion preferences and existing panel animation settings.

Most structure uses dividers, subtle outlines, and little or no shadow. Floating menus and dialogs use their shared elevation. The composer uses a specialized soft shadow and joined-surface treatment. Glass blur and grain belong to established surface components; avoid adding them to every card. Keep portal layer ownership inside the primitives rather than scattering new z-index values through features.

## Accessibility rules

Text should meet 4.5:1 contrast for ordinary text and 3:1 for qualifying large text, measured against the resolved surface. Copied token values do not prove compliance, especially with opacity, glass, imported themes, or small labels. See [W3C contrast guidance](https://www.w3.org/WAI/WCAG22/Understanding/contrast-minimum.html).

WCAG 2.2’s minimum target criterion uses 24 by 24 CSS pixels or qualifying spacing/exceptions. Proposed Detent touch controls aim for 44 by 44 effective pixels; do not let expanded hit areas overlap. Inline prose links have different constraints. See [W3C target guidance](https://www.w3.org/WAI/WCAG22/Understanding/target-size-minimum.html).

Preserve semantic controls, associated labels, visible focus, keyboard navigation, focus return from overlays, text/icon alternatives to color, and non-hover access to essential actions. Check zoom, long labels, reduced motion, both themes, and narrow panels during a focused visual review. These are review criteria, not new blocking validation gates.

## Implementation examples

These examples use components and variants already present in Detent. Imports are intentionally omitted because relative paths depend on the caller; use the repository’s existing import pattern. Apply them only to UI already authorized for the change.

```tsx
// A compact toolbar uses one size family.
<div className="flex items-center gap-2">
  <Button size="compact" variant="outline">Cancel</Button>
  <Button size="compact">Save</Button>
</div>

// A field owns its label, help, and validation association.
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

// Color follows the meaning of the existing status.
<Badge variant="success">Completed</Badge>
```

Use `@theme` for tokens that generate utilities, `:root` for ordinary runtime variables, and existing `@theme inline` mappings for semantic aliases. Keep class names statically discoverable. Do not introduce a Tailwind v3 configuration file. See [Tailwind theme variables](https://tailwindcss.com/docs/theme).

## Agent instructions

Use the following with an authorized Detent UI task:

```text
Use the Detent Design System and the existing component library.
First read AGENTS.md, relevant invariants, the target component, and its closest
existing screen. Identify the authorized UI scope, the page recipe, semantic
color roles, component variants, and required interaction states.

Reuse web/conversation/src/components/ui primitives and existing shell,
composer, settings, and surface compositions. Use props for primitive
appearance; use className for placement and layout. Keep compact toolbar
controls in one size family. Use semantic text and surface utilities, system
font tokens, existing icon mappings, and shared header/sidebar geometry.

Do not invent a new palette, tooltip, menu, composer, card layout, or status
mapping for one feature. Do not import every upstream primitive or upgrade
dependencies just to achieve consistency. Existing specialized renderer
contracts and Detent-specific data/interaction behavior remain authoritative.

Implement only the UI named by a human-authored issue where INV-15 requires it.
Honor INV-13 card content and INV-16 publication ownership. File product work
through the supported selected tracker unless manual implementation is
explicitly authorized. Do not add enforcement mechanisms or validation gates.

Review both themes, narrow containers, long labels, keyboard focus, touch hit
areas, and existing loading/empty/error/disabled states. Report the actual
components reused, source references, and any unresolved visual exception in
the authorized Change or issue outcome.
```

## Applying the system

For a requested UI change, identify its existing screen recipe and choose the smallest set of available components that expresses it. Use the component catalog to distinguish implemented primitives from optional patterns. Preserve existing behavior, permission checks, and renderer contracts while aligning appearance through the shared owners.

A proposed width, layout, or missing component is not permission to add a surface or perform a broad redesign. Scope product changes through the selected tracker and follow INV-13, INV-15, and INV-16. Keep change-specific evidence and unresolved exceptions in the authorized Change or issue outcome, rather than appending them to this guide.
