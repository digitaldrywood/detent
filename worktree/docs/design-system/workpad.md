# Design system workpad

Progress and validation record for the design-system build. Dated 2026-10-06.

## Goal

Make the design system the source of truth for the Cloud UI (`web/conversation`): normative rules for every foundation and pattern, tokens generated from the stylesheets, contracts from a validated catalog that covers every shared component, and a gallery that renders all of it in both themes, with or without a hub.

## Worktree and committed state

- Branch `feat/design-system`, in its own Detent worktree, branched from `develop`.
- First pass committed as `23ee16487` ("feat(ui): add the Detent design system"): docs, token and catalog generators, the catalog, the development gallery and twelve new primitives.
- The second pass described below is uncommitted, alongside a concurrent primitive sync in `src/components/ui`, `src/app/global.css` and `src/lib/utils.ts`. Nothing has been pushed or opened as a pull request.
- The gallery runs on the Vite dev server on ephemeral ports. Port 4000 is not used.

## Second pass

### Documentation

- Every hand-written document is normative: it states the rule, the token or component and how to use it. The adoption-gap document was removed, with the defect, inconsistency and open-decision lists that went with it.
- [Foundations](foundations.md): restored the full colour-role table and Tailwind authoring rules; added categorical colours, type roles (the dense sizes `text-2xs` to `text-5xs` are tokens), size preferences, numbers and truncation, icons, layout dimensions, breakpoints, elevation, layers and motion.
- [Patterns](patterns.md): added the page template, keyboard shortcuts, formatting of values and status indicators; corrected control heights; restored the device-tools, managed-account, cloud-connection and authentication recipes, native disabled and loading semantics, the settings-row container rule and the separation from marketing.
- [Accessibility](accessibility.md): right panel at 980px, switch on Enter and Space, the two focus-ring forms, zoom, the inline-link target exception, and the review-criteria framing.
- [Color contrast](color-review.md) states which pairs are safe for text; [brand](brand.md) states the mark, colour, size, clear space and accessible-name rules.
- [README](README.md), [contributing](contributing.md), the client README, `AGENTS.md`, `CLAUDE.md` and the Makefile help say the design checks run in `make check-app`.

### Tooling

- `design-tokens.ts`: the Dark column prints `same` only when both themes resolve to the same value (`themed` now means "differs in dark"); `light-dark()` resolves per theme; elevation, layer and breakpoint groups and their generated sections.
- `design-catalog.ts`: coverage extends to every module under `src/components`; each is part of an entry (`source` or `files`) or listed under `internal` with a reason.
- Tests: themed roles resolve per theme, `light-dark()`, the internal list and coverage counts; the twelve-primitive test file is split by family under `tests/components/ui/`.

### Gallery

- Foundation pages read from `tokens.generated.json` or render the real components and formatters: colour, categorical palette, type, icons, spacing and layout, radius, elevation, layers, motion, breakpoints, status indicators, keyboard shortcuts and formatting.
- Specimens for every newly catalogued shared component, and a nested-overlay specimen (Select and Menu inside a Dialog).
- The five compositions that load from the hub render as the real app route in a frame, themed from the gallery, when the mock hub runs.
- `/design-system` skips the hub bootstrap, so `npm run dev` alone serves it. `npm run build:gallery` writes a static, hash-routed copy to `dist-gallery/` (gitignored).

## Validation

Run in `web/conversation` on 2026-10-06, after the primitive sync and regeneration:

| Gate | Result |
| --- | --- |
| `npx tsc --noEmit` | Pass |
| `npx vitest run` | 2059 of 2060 passed; `tests/reconnect.test.ts` "resumes after server_error…" hit its 15 s timeout while a browser audit loaded the machine, and passed rerun alone (3 of 3) |
| `npx vite build` | Pass; the bundle in `static/app/conversation` contains no gallery code (no gallery test ids, foundation ids or frame routes) |
| `npm run build:gallery` | Pass; `dist-gallery/` with `index.html`, assets and fonts |
| `npm run design:tokens:check` | Pass |
| `npm run design:catalog:check` | Pass: 134 entries (51 primitive, 77 composition, 6 surface) |
| `make check-app` (repository root) | Pass: type check, both design checks, 2060 of 2060 tests, build and licence checks |
| Relative links and anchors in `docs/design-system` | 708 checked, 0 broken |
| Search for provenance names in the design-system docs, sources, scripts and tests | No hits apart from one field name in a git-status fixture |
| Browser click-through | Static gallery: all 262 specimens in Light and Dark at 1100px and 390px rendered without a render failure, an "unknown specimen" or horizontal overflow; the browser console, tracked for about 120 of them (specimens 189–262 in every variant, the first 45 in both themes), showed only the deliberate `RenderErrorBoundary` throw. Dev server: the five app routes render against the mock hub (one live route per page). Two freezes found and fixed: an inline virtualized `Command` specimen and two live app routes on one page. |

Catalog coverage of `src/components`: 147 of 158 modules are part of an entry and 11 are listed as internal, each with a reason ([element catalog](element-catalog.md#internal-modules)).

## Console audit and rebase

- A headless browser loaded all 471 specimen frames of the 134 entries at 1100px and 390px (942 loads) and recorded console errors and warnings, page errors, failed requests, overflow and blank frames. The only findings were the deliberate `RenderErrorBoundary` throw, a transient network failure that did not reproduce, and two mock-hub gaps on the live issue route: a stale mock process (restarted) and a missing pull-request list (the mock now answers an empty list, as the hub does). The remaining 404 on that route is the hub's answer for an issue without a linked conversation, which the page handles.
- Rebased onto `develop`. The sign-in palette moved to `static/css/sign-in.css`, which `index.css` imports, so the token generator now reads it as an owner. The new sidebar workspace picker has a catalog entry and specimens, which brings the catalog to 135 entries.

## Remaining

- `RenderErrorBoundary`'s fallback specimen throws on purpose, so React logs the caught error to the console of that frame.
