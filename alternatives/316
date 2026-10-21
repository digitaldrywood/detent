# Accessibility

These are the accessibility rules for the Cloud UI: what the shared components provide and what a feature must preserve. They are review criteria applied to every UI change, checked in the gallery and on the real route; they are not additional automated gates beyond the design checks `make check-app` already runs.

## Contrast

Ordinary text meets 4.5:1 against the surface it is painted on, and large text (24px, or 18.66px bold) meets 3:1 ([WCAG 1.4.3](https://www.w3.org/WAI/WCAG22/Understanding/contrast-minimum.html)). Icons, focus indicators and control boundaries that carry meaning meet 3:1 ([WCAG 1.4.11](https://www.w3.org/WAI/WCAG22/Understanding/non-text-contrast.html)).

The [color contrast](color-review.md) page measures the token pairs and states [which are safe for text](color-review.md#using-the-pairs). In short: use the role foregrounds on their own surfaces; keep `muted-foreground` at full strength for anything a person must read; use the `update` role for icons and large text only; never put text on a tint of a solid action colour lighter than the button's own hover; and pair every sidebar icon with a visible label.

Static values do not prove the painted result. Opacity modifiers, glass surfaces, grain and nested translucent fills change it. When a change composites colours, measure the rendered pixels in both themes.

Colour is never the only signal. Status, selection, errors and diff changes also use text, an icon, a shape or position. The four kinds of nothing (empty, zero, denied, unavailable) are distinguished in words ([brand](brand.md#interface-voice)).

## Focus

Every interactive element shows a visible focus indicator when reached by keyboard. The primitives own it, in two forms:

- **Buttons, toggles, switches, checkboxes, radios, tabs and rows**: `focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1 focus-visible:ring-offset-background`.
- **Fields** (`Input`, `Textarea`, `Select` trigger, and `NumberField`, `Combobox`, `Autocomplete` and `InputGroup`, which build on `Input`): the border turns `border-ring` and a soft 3px ring appears, `ring-[3px] ring-ring/24` (`has-focus-visible:` on the wrapper); an invalid field uses `border-destructive/64` and `ring-destructive/16`.

Do not remove either with `outline-none` or override it in a feature. `--ring` is the primary colour.

- Overlays (dialog, alert dialog, sheet, popover, menu) move focus inside when they open, trap it while modal, and return it to the trigger when they close. Base UI provides this; keep the trigger rendered while the overlay is open so focus has somewhere to return.
- Focus order follows reading order. The app shell puts the sidebar before the main region.
- A component that receives focus programmatically (the mobile sidebar sheet, the composer after send) does so because the user just acted, never on a timer.

## Keyboard

Every action is reachable and operable from the keyboard. Each entry in [components.md](components.md) records its keyboard contract; the common ones are:

| Component | Keys |
| --- | --- |
| Button, toggle, checkbox | Tab focuses; Space activates; Enter activates a button |
| Switch | Tab focuses; Space or Enter toggles |
| Menu, select, combobox, autocomplete, command | Arrow keys move; Enter selects; Escape closes one level; typeahead where the primitive supports it |
| Radio group, toggle group | Tab enters the group; arrow keys move between options |
| Dialog, sheet, popover | Escape closes and returns focus |
| Right panel tabs | Each tab and its close button are separately focusable; surface shortcuts switch tabs |
| Composer | Enter sends, Shift+Enter inserts a newline, `/` opens the slash menu |

A tooltip supplements a control that already has an accessible name. It never holds the only name or the only explanation of an action, because it is unavailable to touch and easy to miss with a keyboard. Icon-only buttons carry `aria-label`.

## Target sizes

Pointer targets meet WCAG 2.2's 24 by 24 CSS pixel minimum ([2.5.8](https://www.w3.org/WAI/WCAG22/Understanding/target-size-minimum.html)), or its spacing exception. A link inside a sentence is exempt: its size is set by the line of text, and enlarging it would break the prose. On coarse pointers, `Button` and `NumberField` steppers extend their hit area to 44 by 44 pixels with a `pointer-coarse:after` overlay, without changing the visible size. Keep that overlay: do not set `overflow-hidden` on a parent that would clip it, and do not place two such controls so close that their hit areas overlap.

The compact sizes (`icon-micro` at 20px, `micro`) are for dense desktop chrome next to a larger alternative. Do not make one the only way to reach an action on a phone.

## Reduced motion

Animations respect `prefers-reduced-motion`. Spinners and skeletons use `motion-safe:`, and the live activity utilities in `global.css` switch to `animation: none` under `prefers-reduced-motion: reduce` and `forced-colors: active` ([foundations](foundations.md#motion)). A new animation is gated the same way. Nothing essential is communicated only through motion; a spinner always has text or an accessible name nearby.

## Both themes

Light and dark are both supported, and the user can switch between them. Every change is checked in both. Use semantic tokens (`bg-background`, `text-muted-foreground`, `border-border`) rather than literal colours, so a surface follows the theme. The fixed-colour brand assets are for contexts without a theme ([brand](brand.md#the-mark)).

## Narrow layouts

The client works at 390px and 320px wide.

- Text and labels wrap; they are not clipped. Long single tokens such as file paths and branch names use `truncate` or `MiddleTruncate`, and the full value stays readable by assistive technology.
- Overlays stay inside the viewport. Below the `md` breakpoint (768px) the sidebar becomes a sheet behind a toggle. At 980px and below the right panel becomes a sheet (`RIGHT_PANEL_INLINE_LAYOUT_MEDIA_QUERY` in `src/rightPanelLayout.ts`).
- Form fields use 16px text on narrow screens so mobile browsers do not zoom on focus.
- Tables keep every column reachable through horizontal scroll inside their own container; the page itself never scrolls horizontally.

## Zoom

The interface works at 200% browser zoom and with the interface size preference at 20px: content reflows rather than overflowing, nothing is clipped, and no action becomes unreachable. Build dimensions in rem through the spacing scale so they scale with the root, and keep fixed pixel sizes to hairlines and hit-area minimums.

## Verifying in the gallery

The gallery at `/design-system` (see [Contributing](contributing.md#run-the-gallery)) renders each specimen in separate Light and Dark documents, so a theme leak shows immediately.

1. Open the component's page. Use **Light + Dark** to compare the themes side by side, or **Light** or **Dark** alone.
2. Switch to **390px** to check the narrow layout. The frames render at that width, so responsive classes respond as they would on a phone.
3. Click into a frame and use Tab, Shift+Tab, the arrow keys, Enter, Space and Escape. Confirm the focus ring is visible in both themes and that focus returns to the trigger when an overlay closes.
4. Open the browser's rendering tools and emulate `prefers-reduced-motion: reduce` and forced colours to check motion and high-contrast behaviour, and zoom to 200%.
5. For contrast, sample the rendered colours in each frame rather than reading token values.

The gallery proves a component's own states with synthetic data. It does not replace checking the real route with real data, permissions and loading behaviour for a product change.
