# Design system workpad

Progress and validation record for the design-system build. Dated 2026-10-06.

## Goal

Give the Cloud UI (`web/conversation`) one design reference that is generated from, and checked against, the code: tokens from the stylesheets, contracts from a validated catalog, and a development gallery that renders every catalogued component in both themes. Product adoption is a separate, issue-scoped step.

## Worktree

- Branch `feat/design-system`, in its own Detent worktree.
- The gallery runs on the Vite dev server against the mock hub, on ephemeral ports. Port 4000 is not used.
- Nothing has been committed, pushed or opened as a pull request from this record.

## Done

### Documentation

- [README](README.md): scope, sources of truth, the catalog, design decisions, agent instructions and gallery screenshots.
- [Foundations](foundations.md): colour roles, type, spacing, radius, motion and elevation, with tables generated from the stylesheets.
- [Color review](color-review.md): static contrast for every key pair in both themes, and five findings.
- [Brand](brand.md): the mark, the separate sign-in and Templ surfaces, interface voice and four recorded inconsistencies.
- [Components](components.md) and [element catalog](element-catalog.md): generated from `catalog.json`.
- [Patterns](patterns.md): control selection, screen recipes and interaction states.
- [Accessibility](accessibility.md), [contributing](contributing.md) and [migration](migration.md).

### Tooling

- `scripts/design-tokens.ts` (`npm run design:tokens[:check]`) resolves the tokens in `global.css` and `index.css`, writes `src/design-system/tokens.generated.json`, and regenerates the tables in foundations and the color review.
- `scripts/design-catalog.ts` (`npm run design:catalog[:check]`) validates `src/design-system/catalog.json` against the source (files, exports, `cva` variants, coverage of `src/components/ui`) and regenerates components and the element catalog.
- `make check-app` now runs both `:check` scripts after the type check, so a stale generated file fails the client gate.

### Gallery

- Development-only route at `/design-system`, with per-entry pages and a frame route that renders one specimen in a Light or Dark document.
- Controls for Light + Dark, Light, Dark, full width and 390px.
- 102 of 107 available entries have specimens. Five compositions are excluded with a reason because they load from the hub client on mount ([migration](migration.md#compositions-the-gallery-cannot-render-in-isolation)).

### Primitives

Twelve primitives added to `src/components/ui`, each with a catalog entry, specimens and tests: calendar, color-picker, number-field, radio-group, input-group, draft-input, collapsible-section-header, middle-truncate, discovery-list, standalone-page, wizard and qr-code. None has a feature caller yet.

### Gallery fix

The composer specimen "Attachments (ready, uploading, failed)" showed the uploading image's filename clipped and overlapped by the remove button. Cause: the specimen gave the image `previewUrl: null`. The attachments adapter creates an object URL for every image file, so the real composer shows a thumbnail; the null path is a fallback for when `URL.createObjectURL` is unavailable. The specimen now supplies a synthetic SVG preview. The fallback's layout is recorded as a component defect in [migration](migration.md#component-defects-found-in-review); the component was not changed.

## Validation

Run in `web/conversation` on 2026-10-06:

| Gate | Result |
| --- | --- |
| `npx tsc --noEmit` | Pass |
| `npx vitest run` | 1865 of 1866 passed. `tests/reconnect.test.ts` "resumes after server_error without hiding or duplicating messages" hit its 15 s timeout while other projects' test suites were loading the machine; rerun alone it passed (3 of 3). |
| `npx vite build` | Pass |
| `npm run design:tokens:check` | Pass: tokens current |
| `npm run design:catalog:check` | Pass: 107 entries verified (46 primitive, 55 composition, 6 surface; all available) |
| `make check-app` (repository root) | First run: design checks passed, the suite hit the same reconnect timeout under load. Rerun: pass (type check, both design checks, 1866 of 1866 tests, build and licence checks). |
| Relative links in `docs/design-system/*.md` | All resolve, including heading anchors |
| Case-insensitive search for excluded source names in the design-system docs, sources, scripts and tests | No hits apart from a git-status fixture field |

Browser review on the mock hub: the screenshots in `images/` cover the overview in both themes, the button matrix, an open menu in both themes, the composer with attachments after the fix, the conversation timeline and the app sidebar sheet at 390px.

## Remaining

- Fix the contrast findings in the owning stylesheets, starting with `update-foreground` before the `update` role is adopted.
- Resolve the brand inconsistencies through a human-authored issue.
- Adopt the twelve new primitives where features need them, and work through the primitive gaps in [migration](migration.md#primitive-gaps).
- Separate the five excluded compositions' data hooks from their views so the gallery can render them.
