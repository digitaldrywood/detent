# Foundations and token ownership

This document states Detent's foundation rules: colour, type, icons, spacing and layout, breakpoints, radius, elevation, layers and motion. Each rule names the token or utility to use. The tables between `<!-- tokens:… -->` markers are generated from the stylesheets by `npm run design:tokens` (run in `web/conversation`); `npm run design:tokens:check`, which `make check-app` runs, fails when a table or `tokens.generated.json` no longer matches the CSS. Edit the owning stylesheet, never a generated table.

[Color contrast](color-review.md) holds the measured contrast and which pairs are safe for text. [Brand](brand.md) holds the mark and interface voice. [Patterns](patterns.md) holds the page template, keyboard shortcuts, formatting of values and status indicators. The gallery at `/design-system` renders every foundation in both themes.

## Token ownership

`src/app/global.css` owns the base tokens for both themes, the sidebar and contrast roles, and the type, layout, elevation, layer and motion tokens. `src/app/index.css` imports it and owns the fonts and the primary colour. [`static/css/sign-in.css`](../../static/css/sign-in.css) owns the sign-in surface; `index.css` imports it and the server-rendered entry pages load it too.

`index.css` declares its overrides after the import, so for the same selector its later declaration wins. Dark values are declared with `@variant dark`, which compiles to `:root:is(.dark, .dark *)`. That selector is more specific than `:root`, so a `:root` override in `index.css` replaces only the light value; `--primary` in `index.css` changes light mode and leaves the dark value from `global.css` in place.

Tailwind utilities reach the roles through `@theme inline` aliases in `global.css`. Most text roles go through a `--contrast-*` variable, for example `text-muted-foreground` → `--color-muted-foreground` → `--contrast-muted-foreground` → `--muted-foreground`. Each `--contrast-*` variable mixes the role with its surface by `--appearance-contrast-base` and towards `--appearance-contrast-target` by `--appearance-contrast-boost`. With the defaults (100% and 0%) it equals the role; a runtime appearance setting can change those inputs. Every generated value below assumes the defaults. Use the utility rather than reading a role variable directly, so the adjustment keeps working.

Tokens live in three scopes:

- `root`: `:root` (and `@theme`). It applies to the whole workspace and to portaled overlays.
- `sidebar`: `[data-app-sidebar]`. The app sidebar redeclares the neutral roles, so it keeps its own zinc hierarchy in light and near-black in dark.
- `sign-in`: `.detent-sign-in`, the public sign-in palette, a separate contract described in [Brand](brand.md#separate-surface-contracts).

The generated data is [`tokens.generated.json`](../../web/conversation/src/design-system/tokens.generated.json). Code reads it through [`tokens.ts`](../../web/conversation/src/design-system/tokens.ts).

<!-- tokens:summary:start -->
| Scope | Tokens | Different in dark | Colours |
| --- | --- | --- | --- |
| `root` | 150 | 82 | 91 |
| `sidebar` | 37 | 37 | 37 |
| `sign-in` | 14 | 14 | 14 |
<!-- tokens:summary:end -->

### Rules

1. Change a value in its owner: base roles and the type, layout, elevation, layer and motion tokens in `global.css`; fonts and the primary in `index.css`; the sign-in surface in `static/css/sign-in.css`. Record why in the change.
2. Do not restate a token value in a component, a new stylesheet or documentation. Use the semantic utility.
3. A feature does not declare its own palette, size, shadow, z-index or duration. Scoped overrides are limited to the scopes above.
4. Run `npm run design:tokens` after changing either owner, and commit the regenerated JSON and docs with the change.

### Authoring tokens in Tailwind v4

- `@theme` declares a token that generates utilities (`--text-2xs` → `text-2xs`, `--shadow-composer` → `shadow-composer`, `--ease-drawer` → `ease-drawer`).
- `:root` (with `@variant dark` for the dark value) declares a runtime variable that is not a utility, such as `--chat-content-max-width` or `--z-sheet`. Reach it with an arbitrary property, for example `max-w-(--chat-content-max-width)` or `z-(--z-sheet)`.
- `@theme inline` maps a semantic alias to a role (`--color-muted-foreground: var(--contrast-muted-foreground)`), so the utility follows the role at runtime.
- Class names are static strings that Tailwind can find in the source. Never build one by concatenation (`` `text-${size}` ``); map a value to a whole class name instead.
- There is no Tailwind v3 configuration file; configuration lives in CSS.
- `cn()` (`src/lib/utils.ts`) merges classes with `tailwind-merge`, which knows the custom size tokens; pass caller classes through it so a placement class can override a default.

## Color roles

Use the semantic utilities. The foreground of a surface is the text colour for that surface. `muted` and `accent` are fills; `muted-foreground` and `accent-foreground` are content colours.

| Role | Tokens and utilities | Use |
| --- | --- | --- |
| Canvas | `background`, `foreground` | The workspace and its default text |
| Grouped surface | `card`, `card-foreground` | Cards and grouped content |
| Floating surface | `popover`, `popover-foreground` | Menus, pickers, floating panels |
| Primary action | `primary`, `primary-foreground` | The main action, send, an active switch; `ring` follows it |
| Secondary action | `secondary`, `secondary-foreground` | A lower-emphasis action beside a primary one |
| Subtle interaction | `accent`, `accent-foreground` | Hover and highlighted items |
| Supporting content | `muted-foreground`, `secondary-label`, `placeholder`, `icon-muted` | Metadata, secondary labels and shortcut hints, placeholders, low-emphasis icons |
| Boundaries | `border`, `input`, `ring` | `border` divides; `input` outlines a control; `ring` marks focus |
| Feedback | `success`, `info`, `warning`, `error`, `update`, each with `-foreground` and some with `-surface` | Outcome, information, caution, failure, an available update |
| Destructive action | `destructive`, `destructive-foreground` | An action that removes or destroys. It shares its colour with `error`, but `error` reports a failure and `destructive` labels an action. |
| Sidebar | `sidebar`, `sidebar-foreground`, `sidebar-muted-foreground`, `sidebar-accent`, `sidebar-row-hover`, `sidebar-row-active`, `sidebar-row-selected`, `sidebar-border`, `sidebar-ring` | The sidebar's own hierarchy: fill, labels, hover, the current destination (active) and a temporary multi-selection (selected) |
| Conversation | `message-*` | The user's message bubble, its text and its actions |
| Specialized renderer | `code-*`, `terminal-*`, `diff-*` | Code blocks, the terminal and diff additions and deletions |

A brand action and a successful outcome are different roles: never colour a primary action `success`, or a success `primary`.

The tables give each colour token's utility key (`*-muted-foreground` means any colour utility: `text-`, `bg-`, `border-`, `ring-`…), its light and dark values and its owner. A Dark value of `same` means the token resolves to the same colour in both themes; any other value is what it paints in dark mode, whether the token declares its own dark value or inherits one through a themed token it references.

Hex values are approximate sRGB conversions of the OKLCH, `color-mix()`, `light-dark()` and `--alpha()` expressions. Translucent values are `#rrggbbaa`. `†` marks a colour outside the sRGB gamut, such as several Tailwind v4 emerald, amber and blue steps; its hex is clipped, and a wide-gamut display shows it more saturated.

<!-- tokens:colors:start -->
**color-role**

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--appearance-contrast-target` | — | `#000000` | `#ffffff` | `src/app/global.css:93`, dark `src/app/global.css:142` |
| `--color-zinc-25` | `*-zinc-25` | `#fcfcfc` | same | `src/app/global.css:213` |
| `--primary` | `*-primary` | `#346bf1` | same | `src/app/index.css:29`, dark `src/app/global.css:754` |
| `--primary-foreground` | `*-primary-foreground` | `#ffffff` | same | `src/app/global.css:691` |

**surface**

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--accent` | `*-accent` | `#f4f4f5` | `#ffffff0a` | `src/app/global.css:704`, dark `src/app/global.css:759` |
| `--app-chrome-background` | — | `#fcfcfc` | `#0a0a0a` | `src/app/global.css:677` |
| `--app-scrollbar-thumb` | — | `#d9d9d9` | `#ffffff14` | `src/app/global.css:94`, dark `src/app/global.css:143` |
| `--app-scrollbar-thumb-hover` | — | `#bfbfbf` | `#ffffff1f` | `src/app/global.css:95`, dark `src/app/global.css:144` |
| `--background` | `*-background` | `#fcfcfc` | `#0a0a0a` | `src/app/global.css:676`, dark `src/app/global.css:747` |
| `--card` | `*-card` | `#ffffff` | `#111111` | `src/app/global.css:686`, dark `src/app/global.css:750` |
| `--diff-addition` | `*-diff-addition` | `#00bc7d`† | same | `src/app/global.css:1319` |
| `--diff-deletion` | `*-diff-deletion` | `#fb2c36` | `#fb414a` | `src/app/global.css:1320` |
| `--muted` | `*-muted` | `#fafafa` | `#ffffff08` | `src/app/global.css:694`, dark `src/app/global.css:757` |
| `--popover` | `*-popover` | `#ffffff` | `#111111` | `src/app/global.css:688`, dark `src/app/global.css:752` |
| `--secondary` | `*-secondary` | `#fafafa` | `#ffffff08` | `src/app/global.css:692`, dark `src/app/global.css:755` |
| `--surface-raised` | `*-surface-raised` | `#ffffff33` | `#111111` | `src/app/global.css:684`, dark `src/app/global.css:748` |
| `--toolbar-background` | — | `#fcfcfc` | `#0a0a0a` | `src/app/global.css:678` |
| `--toolbar-control` | — | `#ffffff` | `#111111` | `src/app/global.css:681` |
| `--toolbar-control-hover` | — | `#f4f4f5` | `#ffffff0a` | `src/app/global.css:683` |

**text**

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--accent-foreground` | `*-accent-foreground` | `#18181b` | `#f5f5f5` | `src/app/global.css:705`, dark `src/app/global.css:760` |
| `--card-foreground` | `*-card-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:687`, dark `src/app/global.css:751` |
| `--contrast-accent-foreground` | `*-accent-foreground` | `#18181b` | `#f5f5f5` | `src/app/global.css:910` |
| `--contrast-card-foreground` | `*-card-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:900` |
| `--contrast-foreground` | `*-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:875` |
| `--contrast-icon-muted` | `*-icon-muted` | `#71717b` | `#818181` | `src/app/global.css:895` |
| `--contrast-muted-foreground` | `*-muted-foreground` | `#71717b` | `#818181` | `src/app/global.css:880` |
| `--contrast-placeholder` | `*-placeholder` | `#71717b` | `#818181` | `src/app/global.css:885` |
| `--contrast-popover-foreground` | `*-popover-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:905` |
| `--contrast-secondary-foreground` | `*-secondary-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:915` |
| `--contrast-secondary-label` | `*-secondary-label` | `#71717b` | `#818181` | `src/app/global.css:890` |
| `--contrast-toolbar-control-foreground` | — | `#27272a` | `#f5f5f5` | `src/app/global.css:856` |
| `--contrast-toolbar-foreground` | — | `#27272a` | `#f5f5f5` | `src/app/global.css:842` |
| `--diff-addition-foreground` | `*-diff-addition-foreground` | `#009966`† | `#00d492`† | `src/app/global.css:1321` |
| `--diff-deletion-foreground` | `*-diff-deletion-foreground` | `#e7000b`† | `#ff6467`† | `src/app/global.css:1322` |
| `--foreground` | `*-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:685`, dark `src/app/global.css:749` |
| `--icon-muted` | `*-icon-muted` | `#71717b` | `#818181` | `src/app/global.css:698` |
| `--muted-foreground` | `*-muted-foreground` | `#71717b` | `#818181` | `src/app/global.css:695`, dark `src/app/global.css:758` |
| `--placeholder` | `*-placeholder` | `#71717b` | `#818181` | `src/app/global.css:696` |
| `--popover-foreground` | `*-popover-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:689`, dark `src/app/global.css:753` |
| `--secondary-foreground` | `*-secondary-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:693`, dark `src/app/global.css:756` |
| `--secondary-label` | `*-secondary-label` | `#71717b` | `#818181` | `src/app/global.css:697` |
| `--toolbar-control-foreground` | — | `#27272a` | `#f5f5f5` | `src/app/global.css:682` |
| `--toolbar-foreground` | — | `#27272a` | `#f5f5f5` | `src/app/global.css:679` |

**border**

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--border` | `*-border` | `#e4e4e7` | `#ffffff0f` | `src/app/global.css:711`, dark `src/app/global.css:765` |
| `--contrast-border` | `*-border` | `#e4e4e7` | `#ffffff0f` | `src/app/global.css:870` |
| `--contrast-input` | `*-input` | `#d4d4d8` | `#ffffff14` | `src/app/global.css:865` |
| `--contrast-toolbar-border` | — | `#e4e4e7` | `#ffffff0f` | `src/app/global.css:851` |
| `--input` | `*-input` | `#d4d4d8` | `#ffffff14` | `src/app/global.css:712`, dark `src/app/global.css:766` |
| `--ring` | `*-ring` | `#346bf1` | same | `src/app/global.css:713` |
| `--toolbar-border` | — | `#e4e4e7` | `#ffffff0f` | `src/app/global.css:680` |

**feedback**

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--destructive` | `*-destructive` | `#fb2c36` | `#fb414a` | `src/app/global.css:710` |
| `--destructive-foreground` | `*-destructive-foreground` | `#c10007`† | `#ff6467`† | `src/app/global.css:714` |
| `--error` | `*-error` | `#fb2c36` | `#fb414a` | `src/app/global.css:706`, dark `src/app/global.css:761` |
| `--error-foreground` | `*-error-foreground` | `#c10007`† | `#ff6467`† | `src/app/global.css:707`, dark `src/app/global.css:762` |
| `--error-surface` | `*-error-surface` | `#fb2c3614` | `#fb414a29` | `src/app/global.css:709`, dark `src/app/global.css:764` |
| `--info` | `*-info` | `#2b7fff`† | same | `src/app/global.css:715` |
| `--info-foreground` | `*-info-foreground` | `#1447e6` | `#51a2ff`† | `src/app/global.css:716`, dark `src/app/global.css:767` |
| `--success` | `*-success` | `#00bc7d`† | same | `src/app/global.css:717` |
| `--success-foreground` | `*-success-foreground` | `#007a55`† | `#00d492`† | `src/app/global.css:718`, dark `src/app/global.css:768` |
| `--tool-error-icon` | `*-tool-error-icon` | `#fb2c36` | `#fca5a5` | `src/app/global.css:708`, dark `src/app/global.css:763` |
| `--update` | `*-update` | `#346bf1` | same | `src/app/global.css:722` |
| `--update-foreground` | `*-update-foreground` | `#346bf1` | `#51a2ff`† | `src/app/global.css:723`, dark `src/app/global.css:771` |
| `--update-surface` | `*-update-surface` | `#346bf11f` | `#346bf12e` | `src/app/global.css:724`, dark `src/app/global.css:772` |
| `--warning` | `*-warning` | `#fe9a00`† | same | `src/app/global.css:719` |
| `--warning-foreground` | `*-warning-foreground` | `#bb4d00`† | `#ffb900`† | `src/app/global.css:720`, dark `src/app/global.css:769` |
| `--warning-surface` | `*-warning-surface` | `#fe9a0014`† | `#fe9a0029`† | `src/app/global.css:721`, dark `src/app/global.css:770` |

**message**

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--contrast-message-foreground` | `*-message-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:924` |
| `--message-action` | `*-message-action` | `#346bf1` | same | `src/app/global.css:701` |
| `--message-action-foreground` | `*-message-action-foreground` | `#ffffff` | same | `src/app/global.css:702` |
| `--message-action-hover` | `*-message-action-hover` | `#487af2` | `#3061da` | `src/app/global.css:703` |
| `--message-foreground` | `*-message-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:700` |
| `--message-surface` | `*-message` | `#f4f4f5` | `#ffffff0a` | `src/app/global.css:699` |

**code**

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--code-background` | `*-code` | `#ffffff` | `#111111` | `src/app/global.css:736` |
| `--code-foreground` | `*-code-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:737` |

**terminal**

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--terminal-background` | — | `#fcfcfc` | `#0a0a0a` | `src/app/global.css:738` |
| `--terminal-cursor` | — | `#26384e` | `#b4cbff` | `src/app/global.css:740`, dark `src/app/global.css:779` |
| `--terminal-foreground` | — | `#27272a` | `#f5f5f5` | `src/app/global.css:739` |
| `--terminal-selection-background` | — | `#253f6333` | `#b4cbff40` | `src/app/global.css:741`, dark `src/app/global.css:780` |
<!-- tokens:colors:end -->

### Sidebar

<!-- tokens:sidebar:start -->
`:root` sidebar roles (portaled sheets and settings navigation):

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--contrast-sidebar-border` | `*-sidebar-border` | `#e4e4e7` | `#ffffff0f` | `src/app/global.css:947` |
| `--contrast-sidebar-foreground` | `*-sidebar-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:933` |
| `--contrast-sidebar-muted-foreground` | `*-sidebar-muted-foreground` | `#71717b` | `#818181` | `src/app/global.css:938` |
| `--sidebar` | `*-sidebar` | `#fafafa` | `#111111` | `src/app/global.css:727`, dark `src/app/global.css:773` |
| `--sidebar-border` | `*-sidebar-border` | `#e4e4e7` | `#ffffff0f` | `src/app/global.css:734` |
| `--sidebar-control-surface` | `*-sidebar-control-surface` | `#f4f4f5` | `#ffffff08` | `src/app/global.css:730`, dark `src/app/global.css:774` |
| `--sidebar-foreground` | `*-sidebar-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:728` |
| `--sidebar-icon-color` | — | `#a8a8ae` | `#545454` | `src/app/global.css:103` |
| `--sidebar-muted-foreground` | `*-sidebar-muted-foreground` | `#71717b` | `#818181` | `src/app/global.css:729` |
| `--sidebar-row-active` | `*-sidebar-row-active` | `#ffffff` | `#ffffff0a` | `src/app/global.css:732`, dark `src/app/global.css:776` |
| `--sidebar-row-hover` | `*-sidebar-row-hover` | `#fcfcfc` | `#ffffff0a` | `src/app/global.css:731`, dark `src/app/global.css:775` |
| `--sidebar-row-selected` | `*-sidebar-row-selected` | `#ffffff` | `#ffffff08` | `src/app/global.css:733`, dark `src/app/global.css:777` |
| `--sidebar-stage-fade` | — | `#fafafa` | `#111111` | `src/app/global.css:735`, dark `src/app/global.css:778` |

`[data-app-sidebar]` overrides (the app sidebar itself):

| Token | Group | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- | --- |
| `--accent` | surface | `*-accent` | `#f4f4f5` | `#191a1d` | `src/app/global.css:792`, dark `src/app/global.css:813` |
| `--accent-foreground` | text | `*-accent-foreground` | `#18181b` | `#f7f9ff` | `src/app/global.css:793`, dark `src/app/global.css:814` |
| `--background` | surface | `*-background` | `#fcfcfc` | `#000000` | `src/app/global.css:788`, dark `src/app/global.css:809` |
| `--border` | border | `*-border` | `#e4e4e7` | `#ffffff14` | `src/app/global.css:796`, dark `src/app/global.css:817` |
| `--card` | surface | `*-card` | `#ffffff` | `#000000` | `src/app/global.css:790`, dark `src/app/global.css:811` |
| `--card-foreground` | text | `*-card-foreground` | `#27272a` | `#f1f3f7` | `src/app/global.css:791`, dark `src/app/global.css:812` |
| `--contrast-accent-foreground` | text | `*-accent-foreground` | `#18181b` | `#f7f9ff` | `src/app/global.css:910` |
| `--contrast-border` | border | `*-border` | `#e4e4e7` | `#ffffff14` | `src/app/global.css:870` |
| `--contrast-card-foreground` | text | `*-card-foreground` | `#27272a` | `#f1f3f7` | `src/app/global.css:900` |
| `--contrast-foreground` | text | `*-foreground` | `#27272a` | `#f1f3f7` | `src/app/global.css:875` |
| `--contrast-icon-muted` | text | `*-icon-muted` | `#71717b` | `#818181` | `src/app/global.css:895` |
| `--contrast-input` | border | `*-input` | `#d4d4d8` | `#ffffff2e` | `src/app/global.css:865` |
| `--contrast-message-foreground` | message | `*-message-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:924` |
| `--contrast-muted-foreground` | text | `*-muted-foreground` | `#71717b` | `#a3a3a3` | `src/app/global.css:880` |
| `--contrast-placeholder` | text | `*-placeholder` | `#71717b` | `#818181` | `src/app/global.css:885` |
| `--contrast-popover-foreground` | text | `*-popover-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:905` |
| `--contrast-secondary-foreground` | text | `*-secondary-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:915` |
| `--contrast-secondary-label` | text | `*-secondary-label` | `#71717b` | `#818181` | `src/app/global.css:890` |
| `--contrast-sidebar-border` | sidebar | `*-sidebar-border` | `#e4e4e7` | `#ffffff14` | `src/app/global.css:947` |
| `--contrast-sidebar-foreground` | sidebar | `*-sidebar-foreground` | `#27272a` | `#f1f3f7` | `src/app/global.css:933` |
| `--contrast-sidebar-muted-foreground` | sidebar | `*-sidebar-muted-foreground` | `#71717b` | `#a3a3a3` | `src/app/global.css:938` |
| `--contrast-toolbar-border` | border | — | `#e4e4e7` | `#ffffff0f` | `src/app/global.css:851` |
| `--contrast-toolbar-control-foreground` | text | — | `#27272a` | `#f5f5f5` | `src/app/global.css:856` |
| `--contrast-toolbar-foreground` | text | — | `#27272a` | `#f5f5f5` | `src/app/global.css:842` |
| `--foreground` | text | `*-foreground` | `#27272a` | `#f1f3f7` | `src/app/global.css:789`, dark `src/app/global.css:810` |
| `--input` | border | `*-input` | `#d4d4d8` | `#ffffff2e` | `src/app/global.css:797`, dark `src/app/global.css:818` |
| `--muted` | surface | `*-muted` | `#fafafa` | `#0a0a0a` | `src/app/global.css:794`, dark `src/app/global.css:815` |
| `--muted-foreground` | text | `*-muted-foreground` | `#71717b` | `#a3a3a3` | `src/app/global.css:795`, dark `src/app/global.css:816` |
| `--sidebar` | sidebar | `*-sidebar` | `#fafafa` | `#000000` | `src/app/global.css:798`, dark `src/app/global.css:819` |
| `--sidebar-border` | sidebar | `*-sidebar-border` | `#e4e4e7` | `#ffffff14` | `src/app/global.css:805`, dark `src/app/global.css:826` |
| `--sidebar-control-surface` | sidebar | `*-sidebar-control-surface` | `#f4f4f5` | `#0a0a0a` | `src/app/global.css:801`, dark `src/app/global.css:822` |
| `--sidebar-foreground` | sidebar | `*-sidebar-foreground` | `#27272a` | `#f1f3f7` | `src/app/global.css:799`, dark `src/app/global.css:820` |
| `--sidebar-muted-foreground` | sidebar | `*-sidebar-muted-foreground` | `#71717b` | `#a3a3a3` | `src/app/global.css:800`, dark `src/app/global.css:821` |
| `--sidebar-row-active` | sidebar | `*-sidebar-row-active` | `#ffffff` | `#f1f3f71c` | `src/app/global.css:803`, dark `src/app/global.css:824` |
| `--sidebar-row-hover` | sidebar | `*-sidebar-row-hover` | `#fcfcfc` | `#f1f3f714` | `src/app/global.css:802`, dark `src/app/global.css:823` |
| `--sidebar-row-selected` | sidebar | `*-sidebar-row-selected` | `#ffffff` | `#f1f3f712` | `src/app/global.css:804`, dark `src/app/global.css:825` |
| `--sidebar-stage-fade` | sidebar | — | `#fafafa` | `#000000` | `src/app/global.css:806`, dark `src/app/global.css:827` |
<!-- tokens:sidebar:end -->

## Categorical colours

Categorical colours tell things apart; they never report a state. Detent has one categorical set and one chart-series mapping, both separate from the status roles.

- **Identity hues** ([`src/projectIconColors.ts`](../../web/conversation/src/projectIconColors.ts)): 18 Tailwind hues, in this order: gray, red, orange, amber, yellow, lime, green, emerald, teal, cyan, sky, blue, indigo, violet, purple, fuchsia, pink, rose. A swatch uses the 500 step (`bg-red-500`); text and icons use 600 in light and 400 in dark (`text-red-600 dark:text-red-400`). Use them for anything a person picks or recognises by colour: projects, accounts, labels. Read the class names from `PROJECT_ICON_COLORS`; do not write a hue class in a feature.
- **Chart series** ([`src/app/usage/usageProviders.ts`](../../web/conversation/src/app/usage/usageProviders.ts)): one stable colour per provider, declared once in `PROVIDER_PRESENTATION`. Declaration order is the order of every chart, legend, table and hover row. A provider the table does not know takes the neutral foreground mix at 45%.
- **Breakdown segments** (cost and token types within one series) are neutral "ink" mixes of `--contrast-foreground` into `--background` (100%, 72%, 60%, 44%, 30%), never a provider or identity colour.
- **Status is not categorical.** Outcome and state use the semantic roles (`success`, `info`, `warning`, `error`) and the thread status mapping ([patterns](patterns.md#status-indicators)). A project coloured green does not mean success, and a failure is never shown in an identity hue.

Every categorical colour is paired with a label, a legend entry or a mark; colour alone never identifies a series.

## Typography

<!-- tokens:fonts:start -->
| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--font-mono` | `font-mono` | `ui-monospace, "SF Mono", SFMono-Regular, Menlo, Consolas, "Geist Mono", monospace` | same | `src/app/index.css:24` |
| `--font-sans` | `font-sans` | `-apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, "Geist", sans-serif` | same | `src/app/index.css:23` |
| `--font-size-prompt-touch` | — | `max(1rem, 16px)` | same | `src/app/global.css:130` |
| `--text-2xs` | `text-2xs` | `11px` | same | `src/app/global.css:176` |
| `--text-2xs--line-height` | — | `calc(1 / 0.6875)` | same | `src/app/global.css:177` |
| `--text-3xs` | `text-3xs` | `10px` | same | `src/app/global.css:178` |
| `--text-3xs--line-height` | — | `calc(0.875 / 0.625)` | same | `src/app/global.css:179` |
| `--text-4xs` | `text-4xs` | `8px` | same | `src/app/global.css:180` |
| `--text-4xs--line-height` | — | `1` | same | `src/app/global.css:181` |
| `--text-5xs` | `text-5xs` | `7px` | same | `src/app/global.css:182` |
| `--text-5xs--line-height` | — | `1` | same | `src/app/global.css:183` |

| Font face | Owner |
| --- | --- |
| Geist | `src/app/index.css:5` |
| Geist Mono | `src/app/index.css:13` |
<!-- tokens:fonts:end -->

Interface text uses the system stack first, with the embedded Geist faces as fallbacks: `-apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, "Geist", sans-serif`. Monospace text uses `ui-monospace, "SF Mono", SFMono-Regular, Menlo, Consolas, "Geist Mono", monospace`. `body` reads `var(--font-sans)`, so a runtime appearance override of `--font-sans` reaches all interface text. The sign-in and Templ surfaces keep their own Geist-first stacks ([Brand](brand.md#separate-surface-contracts)).

### Type roles

Each role has one treatment. Nominal pixel sizes assume a 16px root.

| Role | Classes | Size |
| --- | --- | --- |
| Page or onboarding headline | `text-2xl font-semibold` (up to `text-3xl` on an entry page) | 24px (30px) |
| Dialog or sheet title | `DialogTitle` / `SheetTitle`: `text-xl font-semibold leading-none` | 20px |
| Dialog or sheet description | `text-sm text-muted-foreground` | 14px |
| Section heading | `text-sm font-medium` | 14px |
| Body, labels, controls | `text-base sm:text-sm` (the primitives apply it) | 16px below `sm`, 14px from `sm` |
| Settings row | title `text-sm font-medium`; description `text-xs text-muted-foreground` | 14px / 12px |
| Compact controls, metadata, tooltips, `Kbd` | `text-xs` | 12px |
| Dense annotation: counters, timestamps in rows, diff stats | `text-2xs`, with `tabular-nums` for numbers | 11px |
| Eyebrow and status label | `text-3xs font-semibold uppercase tracking-widest` | 10px |
| Badge overlaid on an icon | `text-4xs`, `text-5xs` | 8px, 7px |
| Code, paths, SHAs | `font-mono text-xs` (the code renderer uses the code size preference) | 12px |
| Chat Markdown | the `ChatMarkdown` scale: h1 20px, h2 18px, h3 16px, h4–h6 14px, weight 600, line height 1.3; inline code, tables and footnotes 12px | — |

The dense sizes `text-2xs` (11px, 16px line), `text-3xs` (10px, 14px line), `text-4xs` and `text-5xs` (8px and 7px, line height 1) are tokens, declared in `global.css` and registered with `tailwind-merge`. Use them, never an arbitrary `text-[11px]`. `text-4xs` and `text-5xs` exist only for a count or letter overlaid on an icon; important status, notices and form text are never below `text-xs`.

Weights: regular (`font-normal`) for prose; medium (`font-medium`) for labels, actions and controls; semibold (`font-semibold`) for titles and eyebrows. `font-bold` is not used in interface text.

### Numbers, truncation and wrapping

- Use `tabular-nums` for any number that changes in place or aligns in a column: counters, durations, timestamps in rows, costs and table values.
- A single-line identity label in a constrained row (a thread title, a project name, a sidebar row) uses `truncate`, with the full value available as a tooltip or accessible name.
- A path, branch name, worktree or SHA uses `MiddleTruncate`, which keeps the last segment (up to 16 characters, otherwise the last 10) and puts the full value in `title`.
- Descriptions, validation messages, actionable status and prose wrap (`wrap-break-word` for user content that may contain long tokens). Use `line-clamp-2` or `line-clamp-3` only for a preview that links to the full text.

### Size preferences

The root font size is the **interface size** preference (default 16px, 12–20px), so every rem-based dimension scales with it. Build dimensions in rem through the spacing scale so they follow; use px only for hairlines and fixed hit-area minimums. Three other preferences have their own variables:

- **Prompt size** (`--font-size-prompt`, default 14px, 12–20px): the composer's editor. On a coarse pointer below `sm` it uses `--font-size-prompt-touch`, which is at least 16px so mobile browsers do not zoom on focus.
- **Code size** (`--font-size-code`, default 13px, 10–18px): code blocks and diffs. When it is not set, code inherits its context.
- **Terminal size** (default 12px, 8–20px): the terminal renderer.

## Icons

Detent uses [Lucide](https://lucide.dev) (`lucide-react`) for every interface icon. Two modules are the only sources of other glyphs:

- **Brand and provider marks**: [`src/components/Icons.tsx`](../../web/conversation/src/components/Icons.tsx). The provider → icon and provider → colour maps are in `components/chat/ProviderInstanceIcon.tsx`; render a provider through `ProviderInstanceIcon`, never with initials or a redrawn logo.
- **File-type icons**: [`src/pierre-icons.ts`](../../web/conversation/src/pierre-icons.ts), rendered through `PierreEntryIcon`.

An animated icon transition uses `MorphIcon`, which honours reduced motion. Do not inline an SVG path in a feature; add a missing brand glyph to `Icons.tsx` once.

### Size follows the control

A primitive sizes the icons inside it; leave their size unset. Set a size only on an icon that stands outside a control.

| Context | Icon | Icon–label gap |
| --- | --- | --- |
| `Button` default, `sm`, `lg`; `MenuItem`, `SelectTrigger`, `Toggle`, `InputGroup`, `Combobox`, `Autocomplete` | `size-4.5` below `sm`, `size-4` from `sm` (18/16px) | `gap-2` default, `gap-1.5` at `sm` |
| `Button` `xs`, `icon-xs` | `size-4` → `size-3.5` | `gap-1` |
| `Button` `compact`; `Select` compact; `Badge` | `size-3.5` (badge: `size-3.5` → `size-3`) | `gap-1` |
| `Button` `micro`, `icon-micro`, `icon-tiny`; `Kbd` | `size-3` | `gap-1` |
| `Button` `xl`, `icon-xl` | `size-5` → `size-4.5` | — |
| Sidebar menu rows | `size-4`, in `--sidebar-icon-color`, turning `sidebar-foreground` on hover and active | `gap-2` |
| `Alert`, toast | `size-4` | — |
| Standalone inline icon in text | match the text: `size-3` beside `text-2xs`/`text-3xs`, `size-3.5` beside `text-xs`, `size-4` beside `text-sm` | `gap-1` to `gap-1.5` |
| Provider marks | `size-5` default; `size-4` in rows | — |

- **Stroke**: Lucide's default 2px. A 12px glyph that must read beside bold text may use `strokeWidth={2.25}`; no other override.
- **Placement**: the icon leads the label. A trailing chevron (a picker or submenu) is the last child, `size-3.5` or `size-3`, often at reduced opacity. Leading icons in menus, badges, toggles and selects are dimmed (`opacity-80`, `text-muted-foreground`; selects use `text-icon-muted`), and controls give icons `shrink-0` and `pointer-events-none`.
- **Accessibility**: an icon beside a visible label is decorative and carries `aria-hidden`. An icon-only control carries `aria-label` on the control, not on the icon. An icon that alone conveys a state (a status dot, a provider mark without text) is `role="img"` with an `aria-label`.

## Spacing and layout

Spacing is Tailwind's 4px step (`--spacing: 0.25rem`). Choose a step by purpose:

| Purpose | Rule | Value |
| --- | --- | --- |
| Stacked label and value | `gap-0.5` | 2px |
| Icon and label in compact controls | `gap-1` | 4px |
| Icon and label; field label and its input | `gap-1.5` | 6px |
| Control group | `gap-2` | 8px |
| Toolbar or content row | `gap-3` | 12px |
| Section content inset | `p-3` or `p-4` | 12px or 16px |
| Sections of a page | `gap-6` | 24px |
| Major page regions | `gap-8` | 32px |

### Layout dimensions

| Element | Rule |
| --- | --- |
| Workspace gutter | `--workspace-gutter`: 12px, 20px from `sm`, plus the safe-area inset (`--workspace-gutter-start`/`-end`). Header and content share it; recurring pairs are `px-3 sm:px-5` (header) and `px-4 sm:px-5`. |
| Workspace top bar | `--workspace-topbar-height`: 52px, or the window-controls overlay height (at least 40px) in an installed app. Header, sidebar chrome, diff header and right-panel tabs use it. A sub-header is `h-10`. |
| Chat reading lane | `--chat-content-max-width`: 46rem (736px). The chat width preference sets 72rem (wide) or 100% (full). The composer shares the lane. |
| Page containers | `WorkspacePageContainer`: `readable` `max-w-4xl` (896px, default), `wide` `max-w-5xl` (1024px), `expanded` `max-w-6xl` (1152px), with `gap-6`, `px-5 sm:px-6`, `pt-6 pb-12`. |
| Sidebar | 16rem (256px) by default, 3rem collapsed to icons; resizable from 208px to the viewport minus 640px of main content. Below `md` it is a sheet `calc(100vw − 0.75rem)` wide. |
| Right panel | Inline from 981px: preview width 540px by default, 360px minimum, at most 70% of the viewport while the main column keeps 360px. As a sheet: `w-[min(42vw,28rem)] min-w-80`, `min(88vw,24rem)` at 760px and below. Thread details: `--thread-details-panel-width`, 17.5rem. |
| Dialogs | `max-w-lg` by default (alert dialogs always). A dialog that needs more room steps to `max-w-xl`, `max-w-2xl`, `max-w-3xl` or `max-w-4xl`; a simple form may use `max-w-md`. Media viewers use `max-w-[92vw] max-h-[92vh]`. Below `sm` a dialog docks to the bottom at full width. |
| Sheets | `max-w-md`, at most `100% − 3rem` |
| Popovers | `sm` `w-64`, `md` `w-80`, `lg` `w-96`, never wider than `100vw − 2rem`. Menus: at least `min(10rem, 100vw − 2rem)`. |
| Toasts | `max-w-90`, inset 1rem (2rem from `sm`) |
| Settings rows | Stack below a 32rem container width; title and control sit in two columns from it (`@container/settings-row`). |

<!-- tokens:layout:start -->
| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--app-scrollbar-width` | — | `6px` | same | `src/app/global.css:89` |
| `--chat-content-max-width` | — | `736px` | same | `src/app/global.css:127` |
| `--command-content-inset` | — | `16px` | same | `src/app/global.css:110` |
| `--command-shell-inset` | — | `8px` | same | `src/app/global.css:109` |
| `--desktop-window-right-resize-inset` | — | `0px` | same | `src/app/global.css:118` |
| `--floating-content-inset` | — | `12px` | same | `src/app/global.css:111` |
| `--sidebar-content-inset` | — | `8px` | same | `src/app/global.css:101` |
| `--sidebar-control-gap` | — | `8px` | same | `src/app/global.css:102` |
| `--sidebar-row-content-inset` | — | `10px` | same | `src/app/global.css:108` |
| `--surface-grain-size` | — | `256px 256px` | same | `src/app/global.css:987` |
| `--thread-details-panel-width` | — | `280px` | same | `src/app/global.css:128` |
| `--workspace-controls-left` | — | `calc(env(safe-area-inset-left) + 0.75rem)` | same | `src/app/global.css:121` |
| `--workspace-controls-right` | — | `calc(env(safe-area-inset-right) + 0.75rem)` | same | `src/app/global.css:122` |
| `--workspace-controls-top` | — | `0px` | same | `src/app/global.css:120` |
| `--workspace-gutter` | — | `12px` | same | `src/app/global.css:133` |
| `--workspace-gutter-end` | — | `calc(env(safe-area-inset-right) + 0.75rem)` | same | `src/app/global.css:135` |
| `--workspace-gutter-start` | — | `calc(env(safe-area-inset-left) + 0.75rem)` | same | `src/app/global.css:134` |
| `--workspace-native-controls-inset` | — | `0px` | same | `src/app/global.css:123` |
| `--workspace-titlebar-control-gap` | — | `12px` | same | `src/app/global.css:125` |
| `--workspace-titlebar-control-size` | — | `28px` | same | `src/app/global.css:124` |
| `--workspace-titlebar-scroll-fade-height` | — | `24px` | same | `src/app/global.css:126` |
| `--workspace-topbar-height` | — | `52px` | same | `src/app/global.css:119` |
<!-- tokens:layout:end -->

`--workspace-controls-*` and the gutter edges include `env()` insets, so they have no static value.

## Breakpoints

Detent uses Tailwind's breakpoints. `sm` (640px) is the main one: most responsive rules are `sm:` and `max-sm:`.

| Breakpoint | Query | What changes |
| --- | --- | --- |
| `max-[320px]`, `max-[400px]` | up to 320px / 400px | Composer banners and badges take their compact forms |
| `sm` | 640px | Controls step down from 36px and `text-base` to 32px and `text-sm`; gutters widen from 12px to 20px; dialogs stop docking to the bottom |
| `md` | 768px | The sidebar is inline (below `md` it is a sheet, `useIsMobile()`); status labels appear beside their dots |
| `max-[760px]` | up to 760px | The right-panel sheet narrows to `min(88vw, 24rem)` |
| 980px | `(max-width: 980px)` (`RIGHT_PANEL_INLINE_LAYOUT_MEDIA_QUERY`) | At or below: the right panel opens as a sheet; above: inline |
| `lg`, `xl`, `2xl` | 1024, 1280, 1536px | Page layouts may add columns; nothing in the shell changes |
| `3xl`, `4xl` | 1600, 2000px | Defined in `useMediaQuery` for JavaScript checks only; no utility uses them |
| `pointer-coarse` | `(pointer: coarse)` | Hit areas extend to 44×44px; the prompt is at least 16px |

- Use a **viewport** query (`sm:`, `md:`, `useMediaQuery`) for the shell: sidebar, panels, dialogs, gutters and control density.
- Use a **container** query (`@container/<name>` on the parent, `@sm/<name>:` on the child) for a component that lives in a panel of unknown width: settings rows, the composer surface, pull-request rows, file lists. Name every container, and declare it on the element whose width decides the layout.
- In JavaScript, read breakpoints through `useMediaQuery` (`src/hooks/useMediaQuery.ts`), whose maximum queries end 1px below the next breakpoint. Do not compare `window.innerWidth`.

## Radius

<!-- tokens:radius:start -->
| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--control-radius` | — | `8px` | same | `src/app/global.css:100` |
| `--radius` | — | `10px` | same | `src/app/global.css:675` |
| `--radius-2xl` | `rounded-2xl` | `18px` | same | `src/app/global.css:279` |
| `--radius-3xl` | `rounded-3xl` | `22px` | same | `src/app/global.css:280` |
| `--radius-lg` | `rounded-lg` | `10px` | same | `src/app/global.css:277` |
| `--radius-md` | `rounded-md` | `8px` | same | `src/app/global.css:276` |
| `--radius-sm` | `rounded-sm` | `6px` | same | `src/app/global.css:275` |
| `--radius-xl` | `rounded-xl` | `14px` | same | `src/app/global.css:278` |
<!-- tokens:radius:end -->

`--radius` is 10px and the Tailwind radius scale derives from it (`rounded-sm` 6px to `rounded-3xl` 22px). Controls use `--control-radius` (8px) so sidebar, palette, tooltip and toolbar controls align; cards and dialogs use `rounded-xl` and `rounded-2xl`. The composer has its own larger radius and attached-banner geometry; do not apply the control radius or a card radius to it.

## Elevation

Structure is flat: rows, borders (`border-border`) and fills, with little or no shadow. Elevation belongs to the primitive that floats.

| Surface | Elevation |
| --- | --- |
| Rows, sections, panels | None; a border or a fill separates them |
| Inputs and outline buttons | `shadow-xs/5`, with a 1px edge highlight (`--inset-shadow-control-highlight` in light, a white 6% top edge in dark) |
| Primary buttons | `shadow-xs shadow-primary/24` plus the inset highlight; pressed uses `--inset-shadow-control-pressed` |
| Cards | `shadow-xs/5` at most |
| Menus, selects, comboboxes, popovers | `dropdown-glass` with the dropdown shadow (`0 16px 40px -18px` black 55%; deeper in dark) |
| Tooltips | `shadow-md/5`; the glass variant `shadow-xl shadow-black/25` |
| Toasts | `dropdown-glass`, `shadow-xl shadow-black/25` |
| Sheets | `shadow-lg/5` |
| Dialogs | `dialog-glass`, which owns its shadow (`0 24px 64px -24px` black 65% in light; an inset 1px white 4% edge and `0 24px 72px -20px` black 90% in dark) |
| Composer | `shadow-composer` in light, none in dark |

The glass utilities in `global.css` (`surface-glass`, `dialog-glass`, `dropdown-glass`, `alert-glass`, `dialog-backdrop`) blend `--background` or `--popover` at `--glass-opacity` with `--glass-blur` and `--glass-saturation`, and fall back to an opaque surface without `backdrop-filter`. `--surface-grain`, a 256px noise tile at 3.5%, is painted on `body`; chrome surfaces opt in with `surface-grain`. Glass and grain belong to the surfaces above, not to every card.

<!-- tokens:elevation:start -->
| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--inset-shadow-control-highlight` | `inset-shadow-control-highlight` | `0 1px rgb(255 255 255 / 16%)` | same | `src/app/global.css:186` |
| `--inset-shadow-control-pressed` | `inset-shadow-control-pressed` | `0 1px rgb(0 0 0 / 8%)` | same | `src/app/global.css:187` |
| `--shadow-composer` | `shadow-composer` | `0 12px 28px -18px rgb(0 0 0 / 40%)` | same | `src/app/global.css:191` |
| `--shadow-composer-dark` | `shadow-composer-dark` | `0 14px 32px -18px rgb(0 0 0 / 75%)` | same | `src/app/global.css:192` |
<!-- tokens:elevation:end -->

## Layers

Each layer belongs to a primitive. A feature never sets a z-index of its own; if it needs a new layer, it has found a missing primitive.

| Layer | Surface |
| --- | --- |
| `z-10` | In-flow overlays inside a component; the fixed desktop sidebar |
| `z-20` | The sidebar resize rail |
| `z-30` | Marks overlaid on an icon (provider badge, status dot) |
| `z-40` | Drop overlays, the docked composer, the timeline minimap |
| `z-(--z-sheet)` (46) | Sheets: the mobile sidebar and the right-panel sheet |
| `z-50` | Dialog and alert-dialog backdrop and popup; the command palette |
| `z-[60]` | The media viewer, which opens over a dialog |
| `z-100` | Toasts |
| `z-[130]` | Menu, select, combobox, autocomplete and popover positioners |
| `z-[140]` | Tooltip and preview-card positioners |

Popups sit above dialogs, so a select or menu opened inside a dialog shows; tooltips sit above popups. The context-menu fallback and the toast stack compute their own values from these layers inside their owners.

<!-- tokens:layers:start -->
| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--z-sheet` | — | `46` | same | `src/app/global.css:114` |
<!-- tokens:layers:end -->

## Motion

Motion is short, eased out and never the only signal of a change.

| Use | Duration and easing |
| --- | --- |
| Hover and colour changes on controls | `transition-colors duration-150 ease-out` (150ms) |
| Opacity and small transforms | `duration-150` to `duration-200`, `ease-out` |
| Dialog open and close | opacity with `scale-98`, 200ms `ease-in-out`, via `data-starting-style`/`data-ending-style`; a nested dialog offsets `translate-y-8` |
| Sheet open and close | translate by 8 with opacity, 200ms `ease-in-out` |
| Tooltip, preview card, popover | opacity with `scale-98` (popover on enter only) |
| Menus and select popups | No open or close animation |
| Collapsible | `transition-[height]` from `h-0`, 200ms |
| Surfaces that slide in (drawers) | `ease-drawer`, `cubic-bezier(0.32, 0.72, 0, 1)` |
| Toasts | transform 500ms `cubic-bezier(0.22, 1, 0.36, 1)`, opacity 500ms, height 150ms |
| Panels and sidebar | `--panel-animation-duration`, the panel animation preference (default 0: no animation; up to 400ms) |

<!-- tokens:motion:start -->
| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--animate-skeleton` | `animate-skeleton` | `skeleton 2.4s infinite` | same | `src/app/global.css:214` |
| `--animate-status-ping` | `animate-status-ping` | `status-ping 2s infinite` | same | `src/app/global.css:218` |
| `--animate-status-pulse` | `animate-status-pulse` | `status-pulse 2s infinite` | same | `src/app/global.css:217` |
| `--ease-drawer` | `ease-drawer` | `cubic-bezier(0.32, 0.72, 0, 1)` | same | `src/app/global.css:188` |

| Keyframes | Owner |
| --- | --- |
| `mobile-composer-old` | `src/app/global.css:46` |
| `mobile-composer-new` | `src/app/global.css:57` |
| `mobile-draft-headline-exit` | `src/app/global.css:77` |
| `skeleton` | `src/app/global.css:281` |
| `status-pulse` | `src/app/global.css:300` |
| `status-ping` | `src/app/global.css:315` |
| `live-tool-shine` | `src/app/global.css:478` |
| `live-activity-focus` | `src/app/global.css:513` |
| `live-activity-focus-counter` | `src/app/global.css:522` |
| `dc-caret` | `src/app/index.css:58` |

| Where | Property | Value | Owner |
| --- | --- | --- | --- |
| `html[data-mobile-composer-route-transition="true"]::view-transition-old(root), html[dat…` | animation | `none` | `src/app/global.css:21` |
| `html[data-mobile-composer-route-transition="true"]::view-transition-group(mobile-composer)` | animation-duration | `180ms` | `src/app/global.css:25` |
| `html[data-mobile-composer-route-transition="true"]::view-transition-group(mobile-composer)` | animation-timing-function | `cubic-bezier(0.4, 0, 0.2, 1)` | `src/app/global.css:26` |
| `html[data-mobile-composer-route-transition="true"]::view-transition-old(mobile-composer)` | animation | `mobile-composer-old 180ms linear both` | `src/app/global.css:34` |
| `html[data-mobile-composer-route-transition="true"]::view-transition-new(mobile-composer)` | animation | `mobile-composer-new 180ms linear both` | `src/app/global.css:39` |
| `html[data-mobile-composer-route-transition="true"]::view-transition-group(mobile-draft-…` | animation-duration | `130ms` | `src/app/global.css:69` |
| `html[data-mobile-composer-route-transition="true"]::view-transition-old(mobile-draft-he…` | animation | `mobile-draft-headline-exit 130ms cubic-bezier(0.4, 0, 1, 1) both` | `src/app/global.css:73` |
| `@utility live-tool-shine › @media (prefers-reduced-motion: no-preference) and (forced-colors: none)` | animation | `live-tool-shine 2.2s steps(30) infinite` | `src/app/global.css:502` |
| `@utility visible-animate-spin` | animation | `var(--animate-spin)` | `src/app/global.css:508` |
| `@utility live-activity-focus` | animation | `live-activity-focus 2.2s linear infinite` | `src/app/global.css:561` |
| `@utility live-activity-focus › @media (prefers-reduced-motion: reduce), (forced-colors: active)` | animation | `none` | `src/app/global.css:566` |
| `@utility live-activity-focus-counter` | animation | `live-activity-focus-counter 2.2s linear infinite` | `src/app/global.css:574` |
| `@utility live-activity-focus-counter › @media (prefers-reduced-motion: reduce), (forced-colors: active)` | animation | `none` | `src/app/global.css:579` |
| `[data-slot="animated-height"] [data-slot="collapsible-panel"]` | transition | `none !important` | `src/app/global.css:632` |
| `.no-transitions, .no-transitions *, .no-transitions *::before, .no-transitions *::after` | transition-duration | `0s !important` | `src/app/global.css:669` |
| `.no-transitions, .no-transitions *, .no-transitions *::before, .no-transitions *::after` | animation-duration | `0s !important` | `src/app/global.css:670` |
| `@media (prefers-reduced-motion: reduce) › .preview-loading-progress, .preview-loading-progress[data-loading="true"]` | transition | `none` | `src/app/global.css:1306` |
| `@media (prefers-reduced-motion: reduce) › .preview-loading-progress, .preview-loading-progress[data-loading="true"]` | animation | `none` | `src/app/global.css:1307` |
| `@media (prefers-reduced-motion: no-preference) › .dc-caret` | animation | `dc-caret 1.1s steps(2, end) infinite` | `src/app/index.css:81` |

| Reduced-motion query | Inside | Owner |
| --- | --- | --- |
| `(prefers-reduced-motion: no-preference) and (forced-colors: none)` | `@utility live-tool-shine` | `src/app/global.css:492` |
| `(prefers-reduced-motion: reduce), (forced-colors: active)` | `@utility live-activity-focus` | `src/app/global.css:565` |
| `(prefers-reduced-motion: reduce), (forced-colors: active)` | `@utility live-activity-focus-counter` | `src/app/global.css:578` |
| `(prefers-reduced-motion: reduce)` | — | `src/app/global.css:1303` |
| `(prefers-reduced-motion: no-preference)` | — | `src/app/index.css:79` |
<!-- tokens:motion:end -->

Indicator animations are duty-cycled: `skeleton` (2.4s), `status-pulse` (2s, `steps(6)`, opacity 1 to 0.5) and `status-ping` (2s, scale 0.75 to 2) step between a few states, so the compositor paints a handful of frames per cycle. Reuse them; do not add another shimmer or pulse. `live-tool-shine`, `live-activity-focus` and `visible-animate-spin` stay paused until `src/lib/visibleAnimation.ts` sets `--visible-animation-state: running` while the element is visible.

Reduced motion:

- Every animation is gated: `motion-safe:animate-skeleton`, `motion-safe:animate-status-pulse`, `motion-reduce:transition-none` on moving transitions, and `prefers-reduced-motion` rules in the stylesheets for the live activity sweep and the loading bar.
- JavaScript reads `(prefers-reduced-motion: reduce)` before animating panels and the sidebar (`src/panelAnimations.ts`, `src/components/Sidebar.motion.ts`).
- The `.no-transitions` class zeroes every transition and animation while the theme switches.
