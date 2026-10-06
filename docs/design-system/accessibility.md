# Accessibility

These are the requirements a Cloud UI change meets before review. They describe what the shared components already provide and what a feature must preserve. They are not a claim that the whole client has passed an accessibility audit.

## Contrast

Ordinary text meets 4.5:1 against the surface it is painted on, and large text (24px, or 18.66px bold) meets 3:1 ([WCAG 1.4.3](https://www.w3.org/WAI/WCAG22/Understanding/contrast-minimum.html)). Icons, focus indicators and control boundaries that carry meaning meet 3:1 ([WCAG 1.4.11](https://www.w3.org/WAI/WCAG22/Understanding/non-text-contrast.html)).

The [color review](color-review.md) measures the token pairs statically. Every workspace text pair passes at the token level except one, and four findings limit how the tokens can be used:

- `update-foreground` on `update-surface` is 3.89:1 in light mode. Do not use the `update` utilities for body text until the owner fixes it ([finding 1](color-review.md#findings)).
- White on `--primary` is 4.64:1. Do not lighten a solid primary or destructive fill further, and do not put text on `bg-primary/90` or lighter tints ([finding 2](color-review.md#findings)).
- `--sidebar-icon-color` is below 3:1 in both themes. A sidebar icon must sit beside a visible label; it cannot be the only cue ([finding 3](color-review.md#findings)).
- `muted-foreground` has little headroom. `text-muted-foreground/70` and lower opacities fall below 4.5:1, so use them only for text a person can ignore without losing information ([finding 4](color-review.md#findings)).

Static values do not prove the painted result. Opacity modifiers, glass surfaces, grain and nested translucent fills change it. When a change composites colours, measure the rendered pixels in both themes.

Colour is never the only signal. Status, selection, errors and diff changes also use text, an icon, a shape or position. The four kinds of nothing (empty, zero, denied, unavailable) are distinguished in words ([brand](brand.md#interface-voice)).

## Focus

Every interactive element shows a visible focus indicator when reached by keyboard. Primitives use `focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1 focus-visible:ring-offset-background`; do not remove it with `outline-none` or override it in a feature. `--ring` is the primary colour, so it shares finding 2's margin.

- Overlays (dialog, alert dialog, sheet, popover, menu) move focus inside when they open, trap it while modal, and return it to the trigger when they close. Base UI provides this; keep the trigger rendered while the overlay is open so focus has somewhere to return.
- Focus order follows reading order. The app shell puts the sidebar before the main region.
- A component that receives focus programmatically (the mobile sidebar sheet, the composer after send) does so because the user just acted, never on a timer.

## Keyboard

Every action is reachable and operable from the keyboard. Each entry in [components.md](components.md) records its keyboard contract; the common ones are:

| Component | Keys |
| --- | --- |
| Button, toggle, checkbox, switch | Tab focuses; Space activates; Enter activates a button |
| Menu, select, combobox, autocomplete, command | Arrow keys move; Enter selects; Escape closes one level; typeahead where the primitive supports it |
| Radio group, toggle group | Tab enters the group; arrow keys move between options |
| Dialog, sheet, popover | Escape closes and returns focus |
| Right panel tabs | Each tab and its close button are separately focusable; surface shortcuts switch tabs |
| Composer | Enter sends, Shift+Enter inserts a newline, `/` opens the slash menu |

A tooltip supplements a control that already has an accessible name. It never holds the only name or the only explanation of an action, because it is unavailable to touch and easy to miss with a keyboard. Icon-only buttons carry `aria-label`.

## Target sizes

Pointer targets meet WCAG 2.2's 24 by 24 CSS pixel minimum ([2.5.8](https://www.w3.org/WAI/WCAG22/Understanding/target-size-minimum.html)), or its spacing exception. On coarse pointers, `Button` and `NumberField` steppers extend their hit area to 44 by 44 pixels with a `pointer-coarse:after` overlay, without changing the visible size. Keep that overlay: do not set `overflow-hidden` on a parent that would clip it, and do not place two such controls so close that their hit areas overlap.

The compact sizes (`icon-micro` at 20px, `micro`) are for dense desktop chrome next to a larger alternative. Do not make one the only way to reach an action on a phone.

## Reduced motion

Animations respect `prefers-reduced-motion`. Spinners and skeletons use `motion-safe:`, and the live activity utilities in `global.css` switch to `animation: none` under `prefers-reduced-motion: reduce` and `forced-colors: active` ([foundations](foundations.md#motion)). A new animation is gated the same way. Nothing essential is communicated only through motion; a spinner always has text or an accessible name nearby.

## Both themes

Light and dark are both supported, and the user can switch between them. Every change is checked in both. Use semantic tokens (`bg-background`, `text-muted-foreground`, `border-border`) rather than literal colours, so a surface follows the theme. The fixed-colour brand assets are for contexts without a theme ([brand](brand.md#the-mark)).

## Narrow layouts

The client works at 390px and 320px wide.

- Text and labels wrap; they are not clipped. Long single tokens such as file paths and branch names use `truncate` or `MiddleTruncate`, and the full value stays readable by assistive technology.
- Overlays stay inside the viewport. Below the `md` breakpoint the sidebar becomes a sheet behind a toggle, and the right panel becomes a sheet.
- Form fields use 16px text on narrow screens so mobile browsers do not zoom on focus.
- Tables keep every column reachable through horizontal scroll inside their own container; the page itself never scrolls horizontally.

## Verifying in the gallery

The gallery at `/design-system` (see [Contributing](contributing.md#run-the-gallery)) renders each specimen in separate Light and Dark documents, so a theme leak shows immediately.

1. Open the component's page. Use **Light + Dark** to compare the themes side by side, or **Light** or **Dark** alone.
2. Switch to **390px** to check the narrow layout. The frames render at that width, so responsive classes respond as they would on a phone.
3. Click into a frame and use Tab, Shift+Tab, the arrow keys, Enter, Space and Escape. Confirm the focus ring is visible in both themes and that focus returns to the trigger when an overlay closes.
4. Open the browser's rendering tools and emulate `prefers-reduced-motion: reduce` and forced colours to check motion and high-contrast behaviour.
5. For contrast, sample the rendered colours in each frame rather than reading token values.

The gallery proves a component's own states with synthetic data. It does not replace checking the real route with real data, permissions and loading behaviour for a product change.
