# Foundations and token ownership

Detent's design tokens live in two stylesheets. This document describes them and is the reference that new UI work builds from. The tables between `<!-- tokens:… -->` markers are generated from those stylesheets by `npm run design:tokens` (run in `web/conversation`). `npm run design:tokens:check` fails when a table, or `tokens.generated.json`, no longer matches the CSS. Edit the owning stylesheet, never a generated table.

This document owns color roles, typography, spacing and geometry, and motion and elevation for the [design system](README.md). [Color review](color-review.md) owns contrast findings. [Brand](brand.md) owns the mark and interface voice.

## Token ownership

| Layer | Owner | Owns |
| --- | --- | --- |
| Tailwind default theme | `web/conversation/node_modules/tailwindcss/theme.css` | The palette (`--color-zinc-500`, `--color-red-700`…), `--spacing: 0.25rem`, default type scale and shadows. Detent does not edit it. |
| Base stylesheet | [`web/conversation/src/app/global.css`](../../web/conversation/src/app/global.css) | Every semantic role in both themes, `--contrast-*` indirection, sidebar overrides, radius, layout variables, glass utilities, keyframes. |
| Overrides | [`web/conversation/src/app/index.css`](../../web/conversation/src/app/index.css) | Geist font faces, the font stacks, the primary colour, and the public sign-in surface (`.detent-sign-in`). |

`index.css` imports `global.css` and then declares its overrides, so for the same selector its later declaration wins. Dark values are declared with `@variant dark`, which compiles to `:root:is(.dark, .dark *)`. That selector is more specific than `:root`, so a `:root` override in `index.css` replaces only the light value. This is why `--primary` in `index.css` changes light mode and leaves the dark value from `global.css` in place (they are the same colour).

Tailwind utilities reach the roles through `@theme inline` aliases in `global.css`. Most text roles go through a `--contrast-*` variable, for example `text-muted-foreground` → `--color-muted-foreground` → `--contrast-muted-foreground` → `--muted-foreground`. Each `--contrast-*` variable mixes the role with its surface by `--appearance-contrast-base` and towards `--appearance-contrast-target` by `--appearance-contrast-boost`. With the defaults (100% and 0%) it equals the role. A runtime appearance setting can change those inputs. Every generated value below assumes the defaults. Prefer the utility to reading a role variable directly, so that adjustment keeps working.

Tokens live in three scopes:

- `root`: `:root` (and `@theme`). It applies to the whole workspace and to portaled overlays.
- `sidebar`: `[data-app-sidebar]`. The app sidebar redeclares the neutral roles, so it stays on its own zinc hierarchy in light and near-black in dark.
- `sign-in`: `.detent-sign-in`. This is the public sign-in palette. It is a separate current contract, described in [Brand](brand.md#separate-surface-contracts).

The generated data is [`tokens.generated.json`](../../web/conversation/src/design-system/tokens.generated.json). Code reads it through [`tokens.ts`](../../web/conversation/src/design-system/tokens.ts).

<!-- tokens:summary:start -->
| Scope | Tokens | With a dark value | Colours |
| --- | --- | --- | --- |
| `root` | 126 | 39 | 87 |
| `sidebar` | 37 | 19 | 37 |
| `sign-in` | 14 | 10 | 14 |
<!-- tokens:summary:end -->

### Rules

1. Change a value in its owner: base roles in `global.css`; fonts, the primary and the sign-in surface in `index.css`. Record why in the change.
2. Do not restate a token value in a component, a new stylesheet or this documentation. Use the semantic utility.
3. A feature does not declare its own palette. Scoped overrides are limited to the existing scopes above.
4. Run `npm run design:tokens` after changing either owner, and commit the regenerated JSON and docs together with the change.

## Color roles

The utility column lists the Tailwind colour key. `*-muted-foreground` means any colour utility, such as `text-`, `bg-`, `border-` or `ring-`. A value of `same` means the token has no dark declaration of its own. It can still render differently in dark mode if it references a themed token, and the generated hex shows that result.

Hex values are approximate sRGB conversions, written by the generator from the OKLCH, `color-mix()` and `--alpha()` expressions. Translucent values are shown as `#rrggbbaa`. `†` marks a colour outside the sRGB gamut, such as several Tailwind v4 emerald, amber and blue steps. Its hex is clipped, and a wide-gamut display shows it more saturated.

Use the roles by meaning:

- `background`/`foreground` is the canvas and its default text. `card` holds grouped content and `popover` holds floating surfaces.
- `muted` and `accent` are subtle fills. `muted-foreground` is supporting text. It is not a fill.
- `primary` is the solid action colour. `ring` follows it.
- `error`, `warning`, `success`, `info` and `update` are outcome colours. Each has a `-foreground` text colour, and some have a `-surface` tint. `destructive` is an alias of `error`.
- `message-*` styles the user's message bubble and its action. `code-*` and `terminal-*` belong to specialised renderers.

<!-- tokens:colors:start -->
**color-role**

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--appearance-contrast-target` | — | `#000000` | `#ffffff` | `src/app/global.css:82`, dark `src/app/global.css:115` |
| `--color-zinc-25` | `*-zinc-25` | `#fcfcfc` | same | `src/app/global.css:130` |
| `--primary` | `*-primary` | `#346bf1` | `#346bf1` | `src/app/index.css:28`, dark `src/app/global.css:665` |
| `--primary-foreground` | `*-primary-foreground` | `#ffffff` | same | `src/app/global.css:602` |

**surface**

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--accent` | `*-accent` | `#f4f4f5` | `#ffffff0a` | `src/app/global.css:615`, dark `src/app/global.css:670` |
| `--app-chrome-background` | — | `#fcfcfc` | same | `src/app/global.css:588` |
| `--app-scrollbar-thumb` | — | `#d9d9d9` | `#ffffff14` | `src/app/global.css:83`, dark `src/app/global.css:116` |
| `--app-scrollbar-thumb-hover` | — | `#bfbfbf` | `#ffffff1f` | `src/app/global.css:84`, dark `src/app/global.css:117` |
| `--background` | `*-background` | `#fcfcfc` | `#0a0a0a` | `src/app/global.css:587`, dark `src/app/global.css:658` |
| `--card` | `*-card` | `#ffffff` | `#111111` | `src/app/global.css:597`, dark `src/app/global.css:661` |
| `--muted` | `*-muted` | `#fafafa` | `#ffffff08` | `src/app/global.css:605`, dark `src/app/global.css:668` |
| `--popover` | `*-popover` | `#ffffff` | `#111111` | `src/app/global.css:599`, dark `src/app/global.css:663` |
| `--secondary` | `*-secondary` | `#fafafa` | `#ffffff08` | `src/app/global.css:603`, dark `src/app/global.css:666` |
| `--surface-raised` | `*-surface-raised` | `#ffffff33` | `#111111` | `src/app/global.css:595`, dark `src/app/global.css:659` |
| `--toolbar-background` | — | `#fcfcfc` | same | `src/app/global.css:589` |
| `--toolbar-control` | — | `#ffffff` | same | `src/app/global.css:592` |
| `--toolbar-control-hover` | — | `#f4f4f5` | same | `src/app/global.css:594` |

**text**

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--accent-foreground` | `*-accent-foreground` | `#18181b` | `#f5f5f5` | `src/app/global.css:616`, dark `src/app/global.css:671` |
| `--card-foreground` | `*-card-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:598`, dark `src/app/global.css:662` |
| `--contrast-accent-foreground` | `*-accent-foreground` | `#18181b` | same | `src/app/global.css:821` |
| `--contrast-card-foreground` | `*-card-foreground` | `#27272a` | same | `src/app/global.css:811` |
| `--contrast-foreground` | `*-foreground` | `#27272a` | same | `src/app/global.css:786` |
| `--contrast-icon-muted` | `*-icon-muted` | `#71717b` | same | `src/app/global.css:806` |
| `--contrast-muted-foreground` | `*-muted-foreground` | `#71717b` | same | `src/app/global.css:791` |
| `--contrast-placeholder` | `*-placeholder` | `#71717b` | same | `src/app/global.css:796` |
| `--contrast-popover-foreground` | `*-popover-foreground` | `#27272a` | same | `src/app/global.css:816` |
| `--contrast-secondary-foreground` | `*-secondary-foreground` | `#27272a` | same | `src/app/global.css:826` |
| `--contrast-secondary-label` | `*-secondary-label` | `#71717b` | same | `src/app/global.css:801` |
| `--contrast-toolbar-control-foreground` | — | `#27272a` | same | `src/app/global.css:767` |
| `--contrast-toolbar-foreground` | — | `#27272a` | same | `src/app/global.css:753` |
| `--foreground` | `*-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:596`, dark `src/app/global.css:660` |
| `--icon-muted` | `*-icon-muted` | `#71717b` | same | `src/app/global.css:609` |
| `--muted-foreground` | `*-muted-foreground` | `#71717b` | `#818181` | `src/app/global.css:606`, dark `src/app/global.css:669` |
| `--placeholder` | `*-placeholder` | `#71717b` | same | `src/app/global.css:607` |
| `--popover-foreground` | `*-popover-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:600`, dark `src/app/global.css:664` |
| `--secondary-foreground` | `*-secondary-foreground` | `#27272a` | `#f5f5f5` | `src/app/global.css:604`, dark `src/app/global.css:667` |
| `--secondary-label` | `*-secondary-label` | `#71717b` | same | `src/app/global.css:608` |
| `--toolbar-control-foreground` | — | `#27272a` | same | `src/app/global.css:593` |
| `--toolbar-foreground` | — | `#27272a` | same | `src/app/global.css:590` |

**border**

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--border` | `*-border` | `#e4e4e7` | `#ffffff0f` | `src/app/global.css:622`, dark `src/app/global.css:676` |
| `--contrast-border` | `*-border` | `#e4e4e7` | same | `src/app/global.css:781` |
| `--contrast-input` | `*-input` | `#d4d4d8` | same | `src/app/global.css:776` |
| `--contrast-toolbar-border` | — | `#e4e4e7` | same | `src/app/global.css:762` |
| `--input` | `*-input` | `#d4d4d8` | `#ffffff14` | `src/app/global.css:623`, dark `src/app/global.css:677` |
| `--ring` | `*-ring` | `#346bf1` | same | `src/app/global.css:624` |
| `--toolbar-border` | — | `#e4e4e7` | same | `src/app/global.css:591` |

**feedback**

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--destructive` | `*-destructive` | `#fb2c36` | same | `src/app/global.css:621` |
| `--destructive-foreground` | `*-destructive-foreground` | `#c10007`† | same | `src/app/global.css:625` |
| `--error` | `*-error` | `#fb2c36` | `#fb414a` | `src/app/global.css:617`, dark `src/app/global.css:672` |
| `--error-foreground` | `*-error-foreground` | `#c10007`† | `#ff6467`† | `src/app/global.css:618`, dark `src/app/global.css:673` |
| `--error-surface` | `*-error-surface` | `#fb2c3614` | `#fb414a29` | `src/app/global.css:620`, dark `src/app/global.css:675` |
| `--info` | `*-info` | `#2b7fff`† | same | `src/app/global.css:626` |
| `--info-foreground` | `*-info-foreground` | `#1447e6` | `#51a2ff`† | `src/app/global.css:627`, dark `src/app/global.css:678` |
| `--success` | `*-success` | `#00bc7d`† | same | `src/app/global.css:628` |
| `--success-foreground` | `*-success-foreground` | `#007a55`† | `#00d492`† | `src/app/global.css:629`, dark `src/app/global.css:679` |
| `--tool-error-icon` | `*-tool-error-icon` | `#fb2c36` | `#fca5a5` | `src/app/global.css:619`, dark `src/app/global.css:674` |
| `--update` | `*-update` | `#346bf1` | same | `src/app/global.css:633` |
| `--update-foreground` | `*-update-foreground` | `#346bf1` | `#51a2ff`† | `src/app/global.css:634`, dark `src/app/global.css:682` |
| `--update-surface` | `*-update-surface` | `#346bf11f` | `#346bf12e` | `src/app/global.css:635`, dark `src/app/global.css:683` |
| `--warning` | `*-warning` | `#fe9a00`† | same | `src/app/global.css:630` |
| `--warning-foreground` | `*-warning-foreground` | `#bb4d00`† | `#ffb900`† | `src/app/global.css:631`, dark `src/app/global.css:680` |
| `--warning-surface` | `*-warning-surface` | `#fe9a0014`† | `#fe9a0029`† | `src/app/global.css:632`, dark `src/app/global.css:681` |

**message**

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--contrast-message-foreground` | `*-message-foreground` | `#27272a` | same | `src/app/global.css:835` |
| `--message-action` | `*-message-action` | `#346bf1` | same | `src/app/global.css:612` |
| `--message-action-foreground` | `*-message-action-foreground` | `#ffffff` | same | `src/app/global.css:613` |
| `--message-action-hover` | `*-message-action-hover` | `#487af2` | same | `src/app/global.css:614` |
| `--message-foreground` | `*-message-foreground` | `#27272a` | same | `src/app/global.css:611` |
| `--message-surface` | `*-message` | `#f4f4f5` | same | `src/app/global.css:610` |

**code**

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--code-background` | — | `#ffffff` | same | `src/app/global.css:647` |
| `--code-foreground` | — | `#27272a` | same | `src/app/global.css:648` |

**terminal**

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--terminal-background` | — | `#fcfcfc` | same | `src/app/global.css:649` |
| `--terminal-cursor` | — | `#26384e` | `#b4cbff` | `src/app/global.css:651`, dark `src/app/global.css:690` |
| `--terminal-foreground` | — | `#27272a` | same | `src/app/global.css:650` |
| `--terminal-selection-background` | — | `#253f6333` | `#b4cbff40` | `src/app/global.css:652`, dark `src/app/global.css:691` |
<!-- tokens:colors:end -->

### Sidebar

<!-- tokens:sidebar:start -->
`:root` sidebar roles (portaled sheets and settings navigation):

| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--contrast-sidebar-border` | `*-sidebar-border` | `#e4e4e7` | same | `src/app/global.css:858` |
| `--contrast-sidebar-foreground` | `*-sidebar-foreground` | `#27272a` | same | `src/app/global.css:844` |
| `--contrast-sidebar-muted-foreground` | `*-sidebar-muted-foreground` | `#71717b` | same | `src/app/global.css:849` |
| `--sidebar` | `*-sidebar` | `#fafafa` | `#111111` | `src/app/global.css:638`, dark `src/app/global.css:684` |
| `--sidebar-border` | `*-sidebar-border` | `#e4e4e7` | same | `src/app/global.css:645` |
| `--sidebar-control-surface` | `*-sidebar-control-surface` | `#f4f4f5` | `#ffffff08` | `src/app/global.css:641`, dark `src/app/global.css:685` |
| `--sidebar-foreground` | `*-sidebar-foreground` | `#27272a` | same | `src/app/global.css:639` |
| `--sidebar-icon-color` | — | `#a8a8ae` | same | `src/app/global.css:92` |
| `--sidebar-muted-foreground` | `*-sidebar-muted-foreground` | `#71717b` | same | `src/app/global.css:640` |
| `--sidebar-row-active` | `*-sidebar-row-active` | `#ffffff` | `#ffffff0a` | `src/app/global.css:643`, dark `src/app/global.css:687` |
| `--sidebar-row-hover` | `*-sidebar-row-hover` | `#fcfcfc` | `#ffffff0a` | `src/app/global.css:642`, dark `src/app/global.css:686` |
| `--sidebar-row-selected` | `*-sidebar-row-selected` | `#ffffff` | `#ffffff08` | `src/app/global.css:644`, dark `src/app/global.css:688` |
| `--sidebar-stage-fade` | — | `#fafafa` | `#111111` | `src/app/global.css:646`, dark `src/app/global.css:689` |

`[data-app-sidebar]` overrides (the app sidebar itself):

| Token | Group | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- | --- |
| `--accent` | surface | `*-accent` | `#f4f4f5` | `#191a1d` | `src/app/global.css:703`, dark `src/app/global.css:724` |
| `--accent-foreground` | text | `*-accent-foreground` | `#18181b` | `#f7f9ff` | `src/app/global.css:704`, dark `src/app/global.css:725` |
| `--background` | surface | `*-background` | `#fcfcfc` | `#000000` | `src/app/global.css:699`, dark `src/app/global.css:720` |
| `--border` | border | `*-border` | `#e4e4e7` | `#ffffff14` | `src/app/global.css:707`, dark `src/app/global.css:728` |
| `--card` | surface | `*-card` | `#ffffff` | `#000000` | `src/app/global.css:701`, dark `src/app/global.css:722` |
| `--card-foreground` | text | `*-card-foreground` | `#27272a` | `#f1f3f7` | `src/app/global.css:702`, dark `src/app/global.css:723` |
| `--contrast-accent-foreground` | text | `*-accent-foreground` | `#18181b` | same | `src/app/global.css:821` |
| `--contrast-border` | border | `*-border` | `#e4e4e7` | same | `src/app/global.css:781` |
| `--contrast-card-foreground` | text | `*-card-foreground` | `#27272a` | same | `src/app/global.css:811` |
| `--contrast-foreground` | text | `*-foreground` | `#27272a` | same | `src/app/global.css:786` |
| `--contrast-icon-muted` | text | `*-icon-muted` | `#71717b` | same | `src/app/global.css:806` |
| `--contrast-input` | border | `*-input` | `#d4d4d8` | same | `src/app/global.css:776` |
| `--contrast-message-foreground` | message | `*-message-foreground` | `#27272a` | same | `src/app/global.css:835` |
| `--contrast-muted-foreground` | text | `*-muted-foreground` | `#71717b` | same | `src/app/global.css:791` |
| `--contrast-placeholder` | text | `*-placeholder` | `#71717b` | same | `src/app/global.css:796` |
| `--contrast-popover-foreground` | text | `*-popover-foreground` | `#27272a` | same | `src/app/global.css:816` |
| `--contrast-secondary-foreground` | text | `*-secondary-foreground` | `#27272a` | same | `src/app/global.css:826` |
| `--contrast-secondary-label` | text | `*-secondary-label` | `#71717b` | same | `src/app/global.css:801` |
| `--contrast-sidebar-border` | sidebar | `*-sidebar-border` | `#e4e4e7` | same | `src/app/global.css:858` |
| `--contrast-sidebar-foreground` | sidebar | `*-sidebar-foreground` | `#27272a` | same | `src/app/global.css:844` |
| `--contrast-sidebar-muted-foreground` | sidebar | `*-sidebar-muted-foreground` | `#71717b` | same | `src/app/global.css:849` |
| `--contrast-toolbar-border` | border | — | `#e4e4e7` | same | `src/app/global.css:762` |
| `--contrast-toolbar-control-foreground` | text | — | `#27272a` | same | `src/app/global.css:767` |
| `--contrast-toolbar-foreground` | text | — | `#27272a` | same | `src/app/global.css:753` |
| `--foreground` | text | `*-foreground` | `#27272a` | `#f1f3f7` | `src/app/global.css:700`, dark `src/app/global.css:721` |
| `--input` | border | `*-input` | `#d4d4d8` | `#ffffff2e` | `src/app/global.css:708`, dark `src/app/global.css:729` |
| `--muted` | surface | `*-muted` | `#fafafa` | `#0a0a0a` | `src/app/global.css:705`, dark `src/app/global.css:726` |
| `--muted-foreground` | text | `*-muted-foreground` | `#71717b` | `#a3a3a3` | `src/app/global.css:706`, dark `src/app/global.css:727` |
| `--sidebar` | sidebar | `*-sidebar` | `#fafafa` | `#000000` | `src/app/global.css:709`, dark `src/app/global.css:730` |
| `--sidebar-border` | sidebar | `*-sidebar-border` | `#e4e4e7` | `#ffffff14` | `src/app/global.css:716`, dark `src/app/global.css:737` |
| `--sidebar-control-surface` | sidebar | `*-sidebar-control-surface` | `#f4f4f5` | `#0a0a0a` | `src/app/global.css:712`, dark `src/app/global.css:733` |
| `--sidebar-foreground` | sidebar | `*-sidebar-foreground` | `#27272a` | `#f1f3f7` | `src/app/global.css:710`, dark `src/app/global.css:731` |
| `--sidebar-muted-foreground` | sidebar | `*-sidebar-muted-foreground` | `#71717b` | `#a3a3a3` | `src/app/global.css:711`, dark `src/app/global.css:732` |
| `--sidebar-row-active` | sidebar | `*-sidebar-row-active` | `#ffffff` | `#f1f3f71c` | `src/app/global.css:714`, dark `src/app/global.css:735` |
| `--sidebar-row-hover` | sidebar | `*-sidebar-row-hover` | `#fcfcfc` | `#f1f3f714` | `src/app/global.css:713`, dark `src/app/global.css:734` |
| `--sidebar-row-selected` | sidebar | `*-sidebar-row-selected` | `#ffffff` | `#f1f3f712` | `src/app/global.css:715`, dark `src/app/global.css:736` |
| `--sidebar-stage-fade` | sidebar | — | `#fafafa` | `#000000` | `src/app/global.css:717`, dark `src/app/global.css:738` |
<!-- tokens:sidebar:end -->

## Typography

<!-- tokens:fonts:start -->
| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--font-mono` | `font-mono` | `ui-monospace, "SF Mono", SFMono-Regular, Menlo, Consolas, "Geist Mono", monospace` | same | `src/app/index.css:23` |
| `--font-sans` | `font-sans` | `-apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, "Geist", sans-serif` | same | `src/app/index.css:22` |
| `--text-3xs` | `text-3xs` | `10px` | same | `src/app/global.css:1233` |
| `--text-3xs--line-height` | — | `calc(0.875 / 0.625)` | same | `src/app/global.css:1234` |

| Font face | Owner |
| --- | --- |
| Geist | `src/app/index.css:4` |
| Geist Mono | `src/app/index.css:12` |
<!-- tokens:fonts:end -->

The client uses the system font stack first, with the embedded Geist faces as fallbacks. This follows the decision recorded on 2026-09-10: `-apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, "Geist", sans-serif` for interface text. Monospace text uses `ui-monospace, "SF Mono", SFMono-Regular, Menlo, Consolas, "Geist Mono", monospace`. `index.css` matches both stacks exactly. The older wording in [decisions.md §8](../conversation/decisions.md) ("Geist and Geist Mono … on both surfaces") describes the Templ and sign-in surfaces. It no longer describes the client.

`body` reads `var(--font-sans)` rather than a literal stack, so a runtime appearance override of `--font-sans` reaches all interface text. Chat code blocks take their size from the runtime `--font-size-code` preference. That variable is not declared in CSS, so it falls back to `inherit`.

The type scale is Tailwind's, plus any dense sizes declared in `global.css` (listed in the table above). The sizes in use below come from a snapshot of `src/**/*.tsx` on 2026-10-06. It is not generated, so treat the counts as an indication.

| Size | Utility | Uses | Typical use |
| --- | --- | --- | --- |
| 12px | `text-xs` | 309 | Compact controls, metadata, badges |
| 14px | `text-sm` | 284 | Body, labels, ordinary controls, section headings with `font-medium` |
| 13px | `text-[13px]` | 49 | Dense rows and lists |
| 11px | `text-[11px]` | 44 | Dense annotation |
| 16px | `text-base` | 28 | Sidebar brand row, reading text |
| 10px | `text-[10px]`, `text-3xs` | 20 | Counters and keyboard hints |
| 18–36px | `text-lg` to `text-4xl` | 23 | Dialog titles (`text-xl font-semibold`) and page headlines |
| 7–22px | other arbitrary values | 19 | Provider badges and specialised renderers |

Chat Markdown has its own scale in `global.css` (lines 939–1087). Headings are 1.25rem, 1.125rem and 1rem, and `h4`–`h6` are 0.875rem, all at weight 600 with line height 1.3. Inline code, tables and footnotes use 0.75rem, and footnote references use 0.6875rem. The weights in use are `font-medium` (178), `font-semibold` (39), `font-normal` (30) and `font-bold` (1). Use regular weight for prose, medium for labels and actions, and semibold for headings.

## Spacing, radius and layout

Spacing is Tailwind's default 4px step (`--spacing: 0.25rem`), with no Detent override. Compact patterns use `gap-1`/`gap-1.5` for an icon and its label, `gap-2` for control groups, `gap-3` for toolbar rows and `p-3`/`p-4` for section insets.

<!-- tokens:radius:start -->
| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--control-radius` | — | `8px` | same | `src/app/global.css:89` |
| `--radius` | — | `10px` | same | `src/app/global.css:586` |
| `--radius-2xl` | `rounded-2xl` | `18px` | same | `src/app/global.css:190` |
| `--radius-3xl` | `rounded-3xl` | `22px` | same | `src/app/global.css:191` |
| `--radius-lg` | `rounded-lg` | `10px` | same | `src/app/global.css:188` |
| `--radius-md` | `rounded-md` | `8px` | same | `src/app/global.css:187` |
| `--radius-sm` | `rounded-sm` | `6px` | same | `src/app/global.css:186` |
| `--radius-xl` | `rounded-xl` | `14px` | same | `src/app/global.css:189` |
<!-- tokens:radius:end -->

`--radius` is 10px, and the Tailwind radius scale is derived from it (`rounded-sm` 6px to `rounded-3xl` 22px). `--control-radius` (8px) keeps sidebar, palette, tooltip and toolbar controls aligned. The composer uses its own larger radius and is not a consumer of these.

<!-- tokens:layout:start -->
| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--app-scrollbar-width` | — | `6px` | same | `src/app/global.css:78` |
| `--command-content-inset` | — | `16px` | same | `src/app/global.css:99` |
| `--command-shell-inset` | — | `8px` | same | `src/app/global.css:98` |
| `--desktop-window-right-resize-inset` | — | `0px` | same | `src/app/global.css:104` |
| `--floating-content-inset` | — | `12px` | same | `src/app/global.css:100` |
| `--sidebar-content-inset` | — | `8px` | same | `src/app/global.css:90` |
| `--sidebar-control-gap` | — | `8px` | same | `src/app/global.css:91` |
| `--sidebar-row-content-inset` | — | `10px` | same | `src/app/global.css:97` |
| `--surface-grain-size` | — | `256px 256px` | same | `src/app/global.css:898` |
| `--workspace-controls-left` | — | `calc(env(safe-area-inset-left) + 0.75rem)` | same | `src/app/global.css:107` |
| `--workspace-controls-right` | — | `calc(env(safe-area-inset-right) + 0.75rem)` | same | `src/app/global.css:108` |
| `--workspace-controls-top` | — | `0px` | same | `src/app/global.css:106` |
| `--workspace-native-controls-inset` | — | `0px` | same | `src/app/global.css:109` |
| `--workspace-titlebar-control-gap` | — | `12px` | same | `src/app/global.css:111` |
| `--workspace-titlebar-control-size` | — | `28px` | same | `src/app/global.css:110` |
| `--workspace-titlebar-scroll-fade-height` | — | `24px` | same | `src/app/global.css:112` |
| `--workspace-topbar-height` | — | `52px` | same | `src/app/global.css:105` |
<!-- tokens:layout:end -->

`--workspace-controls-left` and `--workspace-controls-right` add `env(safe-area-inset-*)`, so they have no static value. The sidebar width (16rem expanded, 3rem collapsed) is owned by the sidebar provider in `components/ui/sidebar.tsx`, not by a CSS token. Detent declares no chat-lane width, detail-panel width or page-gutter token; those widths are set by their compositions.

## Elevation and surfaces

Detent declares no shadow tokens. Shadow values appear inline in components, for example `shadow-[0_12px_28px_-18px_rgb(0_0_0/40%)]`. Elevation comes from:

- Hairline highlights on controls. These are inline `shadow-[0_1px_--theme(--color-black/4%)]` in light and `shadow-[0_-1px_--theme(--color-white/6%)]` in dark, together with Tailwind's `shadow-xs/5` and `shadow-sm/5`.
- The glass utilities in `global.css` (lines 243–327). `surface-glass`, `dialog-glass`, `dropdown-glass`, `alert-glass` and `dialog-backdrop` blend `--background` or `--popover` at `--glass-opacity` (80%) with `--glass-blur` (12px light, 16px dark) and `--glass-saturation`. Without backdrop-filter support they fall back to an opaque surface. `dialog-glass` owns the dialog's drop shadow, `0 24px 64px -24px rgb(0 0 0 / 65%)` in light and a deeper dark stack.
- `--surface-grain`, a 256px noise tile at 3.5% opacity painted on `body`. Chrome surfaces opt in with the `surface-grain` utility.

Most structure uses borders (`border-border`) and fills rather than shadows. Use a shadow only through an existing primitive or glass utility.

## Layers

Detent declares no z-index tokens. The layer literals live in the primitives and should stay there:

| Layer | z-index | Owners |
| --- | --- | --- |
| Local stacking | `z-0`–`z-20` | Inside a component |
| In-pane overlays | `z-40` | Chat drop target, timeline minimap |
| Modal and fixed chrome | `z-50` | `dialog`, `alert-dialog`, `sheet`, `command`, fixed workspace controls (`AppSidebarLayout`, `RightPanel`) |
| Toasts | `z-100` | `toast` viewport; stacked toasts use `z-[calc(9999-var(--toast-index))]` |
| Popups | `z-[130]` | `menu`, `popover`, `select`, `combobox`, `autocomplete` |
| Hints | `z-[140]` | `tooltip`, `preview-card` |

Popups sit above dialogs, so a menu opened inside a dialog shows. Tooltips sit above menus. A feature that needs a new layer has found a missing primitive.

## Motion

<!-- tokens:motion:start -->
| Token | Utility | Light | Dark | Owner |
| --- | --- | --- | --- | --- |
| `--animate-skeleton` | `animate-skeleton` | `skeleton 2.4s infinite` | same | `src/app/global.css:131` |
| `--animate-status-ping` | `animate-status-ping` | `status-ping 2s infinite` | same | `src/app/global.css:135` |
| `--animate-status-pulse` | `animate-status-pulse` | `status-pulse 2s infinite` | same | `src/app/global.css:134` |

| Keyframes | Owner |
| --- | --- |
| `mobile-composer-old` | `src/app/global.css:35` |
| `mobile-composer-new` | `src/app/global.css:46` |
| `mobile-draft-headline-exit` | `src/app/global.css:66` |
| `skeleton` | `src/app/global.css:192` |
| `status-pulse` | `src/app/global.css:211` |
| `status-ping` | `src/app/global.css:226` |
| `live-tool-shine` | `src/app/global.css:389` |
| `live-activity-focus` | `src/app/global.css:424` |
| `live-activity-focus-counter` | `src/app/global.css:433` |
| `dc-caret` | `src/app/index.css:57` |

| Where | Property | Value | Owner |
| --- | --- | --- | --- |
| `html[data-mobile-composer-route-transition="true"]::view-transition-old(root), html[dat…` | animation | `none` | `src/app/global.css:10` |
| `html[data-mobile-composer-route-transition="true"]::view-transition-group(mobile-composer)` | animation-duration | `180ms` | `src/app/global.css:14` |
| `html[data-mobile-composer-route-transition="true"]::view-transition-group(mobile-composer)` | animation-timing-function | `cubic-bezier(0.4, 0, 0.2, 1)` | `src/app/global.css:15` |
| `html[data-mobile-composer-route-transition="true"]::view-transition-old(mobile-composer)` | animation | `mobile-composer-old 180ms linear both` | `src/app/global.css:23` |
| `html[data-mobile-composer-route-transition="true"]::view-transition-new(mobile-composer)` | animation | `mobile-composer-new 180ms linear both` | `src/app/global.css:28` |
| `html[data-mobile-composer-route-transition="true"]::view-transition-group(mobile-draft-…` | animation-duration | `130ms` | `src/app/global.css:58` |
| `html[data-mobile-composer-route-transition="true"]::view-transition-old(mobile-draft-he…` | animation | `mobile-draft-headline-exit 130ms cubic-bezier(0.4, 0, 1, 1) both` | `src/app/global.css:62` |
| `@utility live-tool-shine › @media (prefers-reduced-motion: no-preference) and (forced-colors: none)` | animation | `live-tool-shine 2.2s steps(30) infinite` | `src/app/global.css:413` |
| `@utility visible-animate-spin` | animation | `var(--animate-spin)` | `src/app/global.css:419` |
| `@utility live-activity-focus` | animation | `live-activity-focus 2.2s linear infinite` | `src/app/global.css:472` |
| `@utility live-activity-focus › @media (prefers-reduced-motion: reduce), (forced-colors: active)` | animation | `none` | `src/app/global.css:477` |
| `@utility live-activity-focus-counter` | animation | `live-activity-focus-counter 2.2s linear infinite` | `src/app/global.css:485` |
| `@utility live-activity-focus-counter › @media (prefers-reduced-motion: reduce), (forced-colors: active)` | animation | `none` | `src/app/global.css:490` |
| `[data-slot="animated-height"] [data-slot="collapsible-panel"]` | transition | `none !important` | `src/app/global.css:543` |
| `.no-transitions, .no-transitions *, .no-transitions *::before, .no-transitions *::after` | transition-duration | `0s !important` | `src/app/global.css:580` |
| `.no-transitions, .no-transitions *, .no-transitions *::before, .no-transitions *::after` | animation-duration | `0s !important` | `src/app/global.css:581` |
| `@media (prefers-reduced-motion: reduce) › .preview-loading-progress, .preview-loading-progress[data-loading="true"]` | transition | `none` | `src/app/global.css:1217` |
| `@media (prefers-reduced-motion: reduce) › .preview-loading-progress, .preview-loading-progress[data-loading="true"]` | animation | `none` | `src/app/global.css:1218` |
| `@media (prefers-reduced-motion: no-preference) › .dc-caret` | animation | `dc-caret 1.1s steps(2, end) infinite` | `src/app/index.css:80` |

| Reduced-motion query | Inside | Owner |
| --- | --- | --- |
| `(prefers-reduced-motion: no-preference) and (forced-colors: none)` | `@utility live-tool-shine` | `src/app/global.css:403` |
| `(prefers-reduced-motion: reduce), (forced-colors: active)` | `@utility live-activity-focus` | `src/app/global.css:476` |
| `(prefers-reduced-motion: reduce), (forced-colors: active)` | `@utility live-activity-focus-counter` | `src/app/global.css:489` |
| `(prefers-reduced-motion: reduce)` | — | `src/app/global.css:1214` |
| `(prefers-reduced-motion: no-preference)` | — | `src/app/index.css:78` |
<!-- tokens:motion:end -->

Indicator animations are duty-cycled. `skeleton`, `status-pulse` and `status-ping` hold their states and step between them with `steps()`, so the compositor paints a handful of frames per cycle rather than one per display refresh. Reuse them rather than adding another shimmer or pulse. `live-tool-shine`, `live-activity-focus` and `visible-animate-spin` stay paused until `src/lib/visibleAnimation.ts` sets `--visible-animation-state: running` on the element while it is visible.

Component transitions use Tailwind durations: `duration-150` (14 uses) and `duration-200` (12) are the norm, and `duration-180` and `duration-220` cover panel and composer motion. `ease-out` is the usual curve. Surfaces that slide or pop in use the drawer curve `cubic-bezier(0.32, 0.72, 0, 1)` inline; there is no easing token. These counts come from the same 2026-10-06 snapshot.

Reduced motion is handled in three places:

- CSS: the `prefers-reduced-motion` rules listed above disable the live activity sweep and the preview loading bar (that `global.css` rule currently has no consumer). The streaming caret in `index.css` animates only under `no-preference`.
- JavaScript: `src/panelAnimations.ts` and `src/components/Sidebar.motion.ts` read `(prefers-reduced-motion: reduce)` and skip panel and sidebar animation.
- Theme changes: the `.no-transitions` class (`global.css` line 576) zeroes every transition and animation while the theme switches.

Do not use motion as the only signal of a state change.
