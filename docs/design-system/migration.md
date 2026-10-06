# Migration

This records where the Cloud UI does not yet follow the design system, and the exceptions it keeps on purpose. Each item names its owner so a fix lands once, in the shared component or stylesheet. A gap listed here is not authorization to fix it: a visible change still needs a human-authored issue ([INV-15](../invariants.md#inv-15--visible-ui-changes-require-a-human-authored-issue)).

## Separate surface contracts

Two Detent surfaces do not use the Cloud workspace tokens. They are current contracts, not drift. Do not unify them as a side effect of other work.

| Surface | Owner | Differs from the workspace |
| --- | --- | --- |
| Public sign-in | `.detent-sign-in` in [`src/app/index.css`](../../web/conversation/src/app/index.css), used by `app/account/Login.tsx` | Teal accent (`#0f766e` light, `#2dd4bf` dark), Geist-first fonts, its own 14-token palette and a header that mirrors detent.build |
| Local Templ dashboard | [`static/css/input.css`](../../static/css/input.css) | Teal `--color-accent` reserved for interactivity, Geist-first fonts, 6px cards and 4px chips, a separate `ok`/`warn`/`err`/`info` status palette |

The workspace uses the system font stack with Geist fallbacks and the blue primary. [Brand](brand.md#separate-surface-contracts) has the details. Bringing either surface onto the workspace tokens is one scoped decision for its own issue, made when the shells are unified.

## Compositions the gallery cannot render in isolation

Five available compositions have an `excluded` reason in the gallery registry instead of specimens. Each loads its data through the hub client on mount; the parts it is built from are specimened.

| Composition | Needs | Specimened parts |
| --- | --- | --- |
| `command-palette` | Palette context, new-project and new-issue providers, and issue search through the hub client | Command palette content (`CommandPaletteContent`, `CommandPaletteResults`) |
| `right-panel-workspace` | `useRightPanelWorkspace`, which opens the issue's workspace session against the hub | Right panel tabs, the sheet and every surface |
| `settings-route` | The account API for every section | Settings layout rows and sections |
| `work-board` | A subscription to the project's work items | Board lane, issue card, work toolbar, top bar and list |
| `issue-page` | The issue, its history, attempts, comments and change | Issue properties, activity feed and issue composer |

The way forward is to separate each composition's data hook from its view, so the view takes props and can be specimened with fixtures. Until then, review changes to these compositions on the running app with the mock hub.

## Contrast findings

The [color review](color-review.md#findings) records five findings. None has been fixed; each fix belongs in `src/app/global.css` or `src/app/index.css`, followed by `npm run design:tokens`.

1. `update-foreground` on `update-surface` is 3.89:1 in light mode. Fix before any component adopts the `update` role.
2. White on `--primary` has 0.14 of margin above 4.5:1. Any lighter hover or tint drops below it.
3. `--sidebar-icon-color` is below 3:1 in both themes, and the app sidebar inherits the root-computed value instead of mixing against its own surface.
4. `text-muted-foreground/70` and lower opacities are below 4.5:1. 67 uses at reduced opacity should be reviewed for whether their text is essential.
5. The success, warning, info and dark update colours are outside sRGB and render more saturated on wide-gamut displays.

## Brand findings

[Brand](brand.md#current-inconsistencies-recorded-not-changed) records four inconsistencies, all unchanged:

1. The mark appears in three indigo values (`#3730A3` in the assets, `#4338ca` and `#818cf8` in the sign-in header), none of them the product primary.
2. `DetentCloudLogo` on the entry, platform console, support and setup screens draws a letter "D" in a primary tile rather than the mark, and sets "Cloud" in a lighter weight than the sidebar.
3. The sign-in header duplicates the mark's path data instead of importing `DetentWordmark`.
4. The sidebar mark carries `aria-label="Detent"` inside a link that already has that name; it should be `aria-hidden`.

The proposed size, clear-space and colour rules in brand.md are pending review.

## New primitives without callers

Twelve primitives were added to `src/components/ui` with catalog entries, gallery specimens and tests. No feature uses them yet:

`calendar`, `color-picker`, `number-field`, `radio-group`, `input-group`, `draft-input`, `collapsible-section-header`, `middle-truncate`, `discovery-list`, `standalone-page`, `wizard`, `qr-code`.

Adopt them when a feature needs them, in place of a local implementation. Likely first callers: `radio-group` and `number-field` in settings forms, `middle-truncate` for file paths and branch names, `collapsible-section-header` for the sidebar and settings sections, and `standalone-page` with `wizard` for entry and setup screens. When one is adopted, review its catalog entry against the real use and remove it from this list.

## Primitive gaps

These are proposals for extending existing primitives. Each is a change to the shared owner, made once, with a catalog update and a gallery specimen.

- **Switch mixed state.** `Switch` has no indeterminate state. A switch that controls several items with different values needs a mixed appearance and `aria-checked="mixed"`, as `Checkbox` already supports.
- **Spinner size and tone.** `Spinner` takes only `className`. Add `size` (matching the icon sizes) and `tone` (`muted`, `current`, `primary`) variants, so callers stop setting size and colour by hand.
- **Empty size.** `Empty` has one size. Add a compact size for panels and popovers, where the current spacing is too large.
- **Skeleton shape.** `Skeleton` is a rounded rectangle sized by the caller. Add `shape` variants for text lines, circles (avatars, icons) and blocks, so loading layouts are consistent.
- **Button sizes and variants.** The size scale has gaps callers fill with overrides, and there is no `ghost-destructive` variant for low-emphasis destructive actions in menus and toolbars. Review the scale against real uses before adding sizes.
- **Tooltip scroll dismissal.** A tooltip stays open while its trigger scrolls away. It should close when an ancestor scrolls.
- **Menu mounting and touch density.** The menu popup cannot be kept mounted while closed, which costs a rebuild on every open for large menus; expose a `keepMounted` option. Menu rows are 32px (`min-h-8`) on narrow screens, below the 44px touch target; add a coarse-pointer density.
- **Scroll area class typo.** `scroll-area.tsx` uses `transition-shadows`, which is not a Tailwind utility, so the viewport's shadow transition never applies. It should be `transition-shadow`.
- **Text size scale and class merging.** `global.css` defines one size below `text-xs`: `text-3xs` (10px). Components fill the rest of the dense scale with arbitrary values such as `text-[10px]` and `text-[11px]`. Define the dense sizes (`text-2xs` through `text-5xs`) as tokens and register them with `tailwind-merge` through `extendTailwindMerge` in `src/lib/utils.ts`. Its default classifier happens to treat these names as font sizes today; explicit registration keeps `cn()` from dropping a size if the scale or the library's heuristics change.

## Component defects found in review

- **Image attachment without a preview.** In `src/app/components/Composer.tsx`, an image attachment whose `previewUrl` is null shows its filename centred in the 64px tile, where the remove button (top right) and the "Uploading…" strip (bottom) overlap it. The attachments adapter creates an object URL for every image, so this fallback is reached only when `URL.createObjectURL` is unavailable or throws. The fallback should truncate the name and keep it clear of the overlaid controls. The gallery specimen now supplies a preview, as the adapter does.
