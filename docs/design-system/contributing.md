# Contributing

This is how to add or change a primitive or composition in the Cloud UI and keep the design system true to the code. Commands run in `web/conversation` unless noted.

A visible UI change still needs a human-authored issue ([INV-15](../invariants.md#inv-15--visible-ui-changes-require-a-human-authored-issue)). This document covers how to make the change once it is in scope, not whether to make it.

## 1. Start from the shared owner

Before writing a component, look for one that already does the job: the [element catalog](element-catalog.md) lists every entry, and [patterns](patterns.md#choosing-controls) maps tasks to components.

- **Change in appearance or behaviour** of an existing component: change the owner (`src/components/ui/*.tsx` for primitives, `src/components/**` or `src/app/**` for compositions). Add a variant or prop when callers need a choice; do not restyle the component from a feature with `className` overrides.
- **New component**: add it once, to `src/components/ui` for a primitive (no app data, no hub client) or `src/components` for a shared composition. Build on Base UI for interaction, `class-variance-authority` for variants, Lucide for icons and semantic tokens for colour.
- **Feature data**: connect it through an adapter in `src/app`. A primitive or shared composition takes plain props and never reads the hub client itself.

Callers own placement and layout (`className` for margin, width, grid position). The component owns everything else.

## 2. Catalogue it

Add or update the entry in [`src/design-system/catalog.json`](../../web/conversation/src/design-system/catalog.json):

- `id`, `name`, `kind` (`primitive`, `composition` or `surface`), `group` and `status` (`available`, `proposed` or `exception`);
- `source` and `exports`, which validation checks against the file;
- `variants`, which must match the `cva` definition exactly;
- `states`, `keyboard`, `use`, `avoid` and `related`.

Write `use` and `avoid` as decisions: when to reach for this component, and what to use instead.

Then run:

```sh
npm run design:catalog
```

This validates the catalog and regenerates [components.md](components.md) and [element-catalog.md](element-catalog.md). It fails when an id repeats, a source or export is missing, a catalogued variant differs from the `cva` definition, or a file in `src/components/ui` has no entry. Commit the regenerated docs with the change; never edit them by hand.

## 3. Add a gallery specimen

Every `available` entry has either specimens or an `excluded` reason in [`src/design-system/gallery/registry.tsx`](../../web/conversation/src/design-system/gallery/registry.tsx). `tests/designSystemGallery.test.tsx` enforces this.

1. Write a `GalleryDoc` in the matching file under `src/design-system/gallery/specimens/` (forms, overlays, feedback, chat, workspace and so on).
2. Give each `Specimen` an `id`, a `title` naming the states it shows, an optional one-line `note`, and a `render` function. Use `minHeight` when an overlay opens inside the frame, and a fixed `height` for compositions that fill the viewport.
3. Render the real component with synthetic data. Shared fixtures live in `src/design-system/gallery/fixtures.ts`. Do not wrap it in a restyled copy, and do not mock away its states: show default, hover or pressed, focus, disabled, loading, empty and error where the component has them. `Matrix`, `Row` and `Cell` in `specimen.tsx` lay out variant-by-state grids.
4. Make the data realistic. Use plausible names and long labels, and give an image a preview, so the specimen exercises the same layout the product does.
5. Register the doc under its catalog id in `registry.tsx`.

A composition that cannot render without a live hub client gets an `excluded` string instead, naming what it needs and which specimens cover its parts. Prefer extracting a prop-driven part that can be specimened over excluding the whole composition.

## 4. Test it

New or changed behaviour gets focused tests in `tests/`, using Vitest and Testing Library:

- primitives in `tests/components/ui/<name>.test.tsx`;
- compositions next to their existing tests under `tests/components/` or at the top of `tests/`;
- the design-system checks in `tests/designCatalog.test.ts`, `tests/designTokens.test.ts` and `tests/designSystemGallery.test.tsx`, which already cover the catalog, tokens and registry; extend them only when you change the scripts or the gallery itself.

Test behaviour a user relies on: roles and accessible names, keyboard operation, state changes and callbacks. Snapshot tests of class strings are not useful.

## 5. Regenerate tokens when a stylesheet changes

When you change `src/app/global.css` or `src/app/index.css`, run:

```sh
npm run design:tokens
```

This rewrites `src/design-system/tokens.generated.json` and the generated tables in [foundations.md](foundations.md) and [color-review.md](color-review.md). If a contrast ratio changes, read the new table and update the color review's findings in the same change. A new token belongs to a role in [foundations](foundations.md#token-ownership); do not add one for a single feature.

## 6. Check it in the gallery

### Run the gallery

```sh
MOCK_HUB_PORT=<port> DETENT_HUB_URL=http://127.0.0.1:<port> npm run dev:mock
```

Choose a free port; do not use 4000. Open `/design-system` on the Vite URL the command prints. The gallery is development-only and is not in the production build.

### Review

- **Both themes**: use **Light + Dark**. Look for literal colours that do not follow the theme, low-contrast text and missing borders.
- **390px**: switch the frame width. Check wrapping, truncation, overlays inside the viewport and touch targets.
- **Keyboard**: tab through the frame and operate every control. Focus is visible and returns from overlays.
- **States**: every state in the catalog entry is shown and looks intentional.

[Accessibility](accessibility.md#verifying-in-the-gallery) has the full checklist.

## 7. Run the gates

```sh
npm run design:tokens:check
npm run design:catalog:check
npx tsc --noEmit
npx vitest run
```

From the repository root, `make check-app` runs the type check, both design checks, the test suite and the production build together. The `:check` scripts fail when a generated file is stale; rerun the generator and commit its output.

## Changing this documentation

The hand-written documents are README, foundations prose, color review findings, brand, patterns, accessibility, contributing and migration. Keep them about Detent's own components and decisions. Record adoption gaps in [migration](migration.md); keep change-specific evidence in the issue or pull request, not here.
