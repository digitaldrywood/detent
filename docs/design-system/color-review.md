# Color review

This review records how Detent's colour tokens perform. It reports findings; it does not change any value. A fix to a finding is a scoped visual decision in the owning stylesheet (see [Foundations](foundations.md#token-ownership)).

## Method

`web/conversation/scripts/design-tokens.ts` resolves each token statically:

1. It substitutes `var()` chains through the owning scope, falling back to `:root` and then to Tailwind's default palette. Tailwind's `--alpha(c / n%)` is expanded to the `color-mix(in oklab, c n%, transparent)` it compiles to.
2. It evaluates `oklch()`, `rgb()`, hex and `color-mix()` in `srgb`, `srgb-linear` and `oklab` with premultiplied alpha (CSS Color 5), using its own conversion code.
3. For each pair below it composites a translucent surface over its backdrop (the scope's `--background` unless the pair says otherwise), then composites translucent text over that surface, in gamma-encoded sRGB as browsers paint.
4. It computes WCAG 2.x contrast from sRGB relative luminance.

Text uses the `--contrast-*` value its Tailwind utility renders, with the default appearance settings. Out-of-gamut colours are clipped to sRGB first. Pairs are rated against 4.5:1 for text ([WCAG 1.4.3](https://www.w3.org/WAI/WCAG22/Understanding/contrast-minimum.html)), with "large only" for 3–4.5:1. Icon pairs are rated against 3:1 ([WCAG 1.4.11](https://www.w3.org/WAI/WCAG22/Understanding/non-text-contrast.html)).

Static values do not prove rendered compliance. Opacity modifiers on consumers, glass surfaces, grain, imported themes and runtime contrast settings all change the painted result. Validate a visual change against the rendered interface.

## Contrast

<!-- tokens:contrast:start -->
Workspace (`:root`):

| Pair | Target | Light colours | Light | Dark colours | Dark |
| --- | --- | --- | --- | --- | --- |
| foreground on background | 4.5:1 | `#27272a` on `#fcfcfc` | 14.56 pass | `#f5f5f5` on `#0a0a0a` | 18.15 pass |
| card-foreground on card | 4.5:1 | `#27272a` on `#ffffff` | 14.89 pass | `#f5f5f5` on `#111111` | 17.26 pass |
| popover-foreground on popover | 4.5:1 | `#27272a` on `#ffffff` | 14.89 pass | `#f5f5f5` on `#111111` | 17.26 pass |
| muted-foreground on background | 4.5:1 | `#71717b` on `#fcfcfc` | 4.72 pass | `#818181` on `#0a0a0a` | 5.09 pass |
| muted-foreground on card | 4.5:1 | `#71717b` on `#ffffff` | 4.83 pass | `#818181` on `#111111` | 4.84 pass |
| muted-foreground on muted | 4.5:1 | `#71717b` on `#fafafa` | 4.62 pass | `#818181` on `#111111` | 4.84 pass |
| placeholder on background | 4.5:1 | `#71717b` on `#fcfcfc` | 4.72 pass | `#818181` on `#0a0a0a` | 5.09 pass |
| secondary-foreground on secondary | 4.5:1 | `#27272a` on `#fafafa` | 14.26 pass | `#f5f5f5` on `#111111` | 17.26 pass |
| accent-foreground on accent | 4.5:1 | `#18181b` on `#f4f4f5` | 16.11 pass | `#f5f5f5` on `#141414` | 16.91 pass |
| primary-foreground on primary | 4.5:1 | `#ffffff` on `#346bf1` | 4.64 pass | `#ffffff` on `#346bf1` | 4.64 pass |
| message-foreground on message | 4.5:1 | `#27272a` on `#f4f4f5` | 13.53 pass | `#f5f5f5` on `#141414` | 16.91 pass |
| message-action-foreground on message-action | 4.5:1 | `#ffffff` on `#346bf1` | 4.64 pass | `#ffffff` on `#346bf1` | 4.64 pass |
| error-foreground on background | 4.5:1 | `#c10007` on `#fcfcfc` | 6.27 pass | `#ff6467` on `#0a0a0a` | 6.84 pass |
| error-foreground on error-surface | 4.5:1 | `#c10007` on `#fcecec` | 5.60 pass | `#ff6467` on `#311314` | 5.90 pass |
| warning-foreground on background | 4.5:1 | `#bb4d00` on `#fcfcfc` | 4.94 pass | `#ffb900` on `#0a0a0a` | 11.53 pass |
| warning-foreground on warning-surface | 4.5:1 | `#bb4d00` on `#fcf4e4` | 4.63 pass | `#ffb900` on `#312100` | 9.07 pass |
| success-foreground on background | 4.5:1 | `#007a55` on `#fcfcfc` | 5.25 pass | `#00d492` on `#0a0a0a` | 10.24 pass |
| info-foreground on background | 4.5:1 | `#1447e6` on `#fcfcfc` | 6.67 pass | `#51a2ff` on `#0a0a0a` | 7.51 pass |
| update-foreground on update-surface | 4.5:1 | `#346bf1` on `#e4ebfb` | 3.89 **large only** | `#51a2ff` on `#121c34` | 6.45 pass |
| code-foreground on code-background | 4.5:1 | `#27272a` on `#ffffff` | 14.86 pass | `#f5f5f5` on `#111111` | 17.36 pass |
| terminal-foreground on terminal-background | 4.5:1 | `#27272a` on `#fcfcfc` | 14.56 pass | `#f5f5f5` on `#0a0a0a` | 18.15 pass |
| sidebar-foreground on sidebar (portaled) | 4.5:1 | `#27272a` on `#fafafa` | 14.26 pass | `#f5f5f5` on `#111111` | 17.26 pass |
| sidebar-muted-foreground on sidebar (portaled) | 4.5:1 | `#71717b` on `#fafafa` | 4.62 pass | `#818181` on `#111111` | 4.84 pass |
| sidebar-icon-color on sidebar (portaled) | 3:1 (UI) | `#a8a8ae` on `#fafafa` | 2.27 **fail** | `#545454` on `#111111` | 2.50 **fail** |

App sidebar (`[data-app-sidebar]`):

| Pair | Target | Light colours | Light | Dark colours | Dark |
| --- | --- | --- | --- | --- | --- |
| sidebar-foreground on sidebar | 4.5:1 | `#27272a` on `#fafafa` | 14.26 pass | `#f1f3f7` on `#000000` | 18.90 pass |
| sidebar-muted-foreground on sidebar | 4.5:1 | `#71717b` on `#fafafa` | 4.62 pass | `#a3a3a3` on `#000000` | 8.33 pass |
| sidebar-foreground on row hover | 4.5:1 | `#27272a` on `#fcfcfc` | 14.56 pass | `#f1f3f7` on `#131314` | 16.66 pass |
| sidebar-foreground on row active | 4.5:1 | `#27272a` on `#ffffff` | 14.89 pass | `#f1f3f7` on `#1b1b1b` | 15.55 pass |
| sidebar-foreground on row selected | 4.5:1 | `#27272a` on `#ffffff` | 14.89 pass | `#f1f3f7` on `#111111` | 17.00 pass |
| sidebar-muted-foreground on row active | 4.5:1 | `#71717b` on `#ffffff` | 4.83 pass | `#a3a3a3` on `#1b1b1b` | 6.85 pass |
| sidebar-icon-color on sidebar | 3:1 (UI) | `#a8a8ae` on `#fafafa` | 2.27 **fail** | `#545454` on `#000000` | 2.79 **fail** |

Public sign-in exception (`.detent-sign-in`):

| Pair | Target | Light colours | Light | Dark colours | Dark |
| --- | --- | --- | --- | --- | --- |
| foreground on background | 4.5:1 | `#1a1f27` on `#f7f8fa` | 15.57 pass | `#edf0f4` on `#0b0d10` | 17.02 pass |
| muted-foreground on card | 4.5:1 | `#5d6b80` on `#ffffff` | 5.41 pass | `#8a93a2` on `#11151b` | 5.91 pass |
| primary-foreground on primary | 4.5:1 | `#ffffff` on `#0f766e` | 5.47 pass | `#0b0d10` on `#2dd4bf` | 10.45 pass |
<!-- tokens:contrast:end -->

## Findings

Every workspace text pair meets 4.5:1 at the token level, with the one exception in finding 1. The findings are listed in order of consequence.

1. **`update-foreground` on `update-surface` is 3.89:1 in light mode, so it fails as normal text.** In light mode both colours derive from `--primary` (`oklch(0.571 0.21 264)`, about `#346bf1`). `--update-foreground` is the primary itself, and `--update-surface` is the primary at 12% over the canvas. No component currently uses the `update` utilities, so the failure is latent. Fix it in the owner before adopting the update role.
2. **Solid actions have little margin.** White on `--primary` measures 4.64:1 in both themes. `--message-action` and `--ring` share the colour. The pair passes, but a hover or pressed fill that lightens it, or a translucent variant such as `bg-primary/90`, can fall below 4.5:1.
3. **`--sidebar-icon-color` is below 3:1 for icons.** It measures 2.27:1 light and 2.50:1 dark on the portaled sidebar, and 2.27:1 light and 2.79:1 dark in the app sidebar. Sidebar menu-button icons (`components/ui/sidebar.tsx`) and the update pill use it. Both are paired with a visible text label, which limits the impact. Because the variable is declared only on `:root`, the app sidebar inherits the root-computed colour rather than mixing against its own surface.
4. **Supporting text has little headroom.** `muted-foreground` measures 4.62:1 on `muted` in light mode and 4.84:1 on `card` in dark mode. Components often lower it further with an opacity modifier. The 2026-10-06 snapshot counts 46 uses of `text-muted-foreground/70` and 21 at other opacities. At 70% the pair on `background` falls to 2.71:1 in light and 3.02:1 in dark, so this treatment suits only non-essential text.
5. **Out-of-gamut colours.** The Tailwind v4 emerald, amber and blue steps used by `success`, `warning`, `info` and the dark `update-foreground` lie outside sRGB. Their hex values are clipped, and wide-gamut displays render them more saturated. The ratios above use the clipped sRGB values.

The public sign-in exception passes on all three of its pairs.

## Values that are not statically resolvable

<!-- tokens:unresolved:start -->
- `--workspace-controls-left (root): calc(env(safe-area-inset-left) + 0.75rem)`
- `--workspace-controls-right (root): calc(env(safe-area-inset-right) + 0.75rem)`
<!-- tokens:unresolved:end -->
