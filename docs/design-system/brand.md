# Brand in product UI

Detent runs coding agents against a team's issues and reports what they did. The Cloud UI is a working tool. Its identity is carried by the mark, the product name and plain, exact language, not by decoration. This document covers how that identity appears in product UI. Marketing and a mark redesign are out of scope.

Statements marked **Proposed** are guidance for review. They are not established rules.

## The mark

The Detent mark is a heavy "D" with a counter and a dot, drawn on a 100×100 viewBox. The glyph occupies x 22–87 and y 15–85, so the artwork already includes 15–22 units of padding inside its box.

| Asset | Fill | Use |
| --- | --- | --- |
| [`docs/brand/detent-mark.svg`](../brand/detent-mark.svg) | Fixed `#3730A3` (indigo) | Documentation and README (88px) |
| [`docs/brand/detent-mark-mono.svg`](../brand/detent-mark-mono.svg) | `currentColor` | Single-colour placements |
| `static/img/detent-mark.svg`, `static/img/detent-mark-mono.svg` | Byte-identical copies of the above | Templ pages: favicon (`internal/web/assets.go`, hosted UI and cloud entry) and the invitation page header |
| [`web/conversation/src/components/DetentWordmark.tsx`](../../web/conversation/src/components/DetentWordmark.tsx) | `currentColor` | The React component for the mark in the Cloud client |
| `web/conversation/public/favicon.svg` | `#3730A3` mark on a white tile with 20-unit corner radius | Cloud client favicon (SVG) |
| `web/conversation/public/favicon-32.png` | 32×32 raster | Cloud client favicon fallback |
| `web/conversation/public/apple-touch-icon.png` | 180×180 raster | iOS home-screen icon |

`index.html` links the three client icons under `/static/app/conversation/`.

Despite its name, `DetentWordmark` renders only the mark. The word "Detent" next to it is live text. Use the existing assets as they are: do not redraw, outline, stretch or recolour the fixed-colour files. For any placement on a themed surface, use `DetentWordmark` or the mono SVG, so the mark follows the surrounding text colour in both themes.

### Where the mark appears

- **Sidebar brand row** (`components/sidebar/SidebarChrome.tsx`, `SidebarBrand`). This row is the Cloud workspace's primary identity. It reads "Detent Cloud": the mark at 16px (`h-4`) in the foreground colour, "Detent" in `text-base font-semibold`, and "Cloud" in the same weight using `text-muted-foreground`. The link's accessible name is "Go to Detent Cloud", and it leads to `/work`. On a stage backdrop the row switches to white, with "Cloud" at 70% white.
- **Work timeline** (`components/chat/MessagesTimeline.tsx`). The mark is the decorative (`aria-hidden`) icon for Detent's own work-log entries.
- **Templ pages** use `static/img/detent-mark.svg` as the favicon and in the invitation header, with `alt=""` beside the visible word "Detent".

### Current inconsistencies (recorded, not changed)

These differences exist today. A change to any of them is a visible UI change and needs a human-authored issue (INV-15).

1. **Three indigo values for one mark.** The asset files use `#3730A3`. The sign-in site header (`app/account/Login.tsx`, `SiteHeader`) inlines its own copy of the SVG path in `#4338ca`, or `#818cf8` in dark mode. None of these is the product primary (`oklch(0.571 0.21 264)`, about `#346bf1`).
2. **A letter tile instead of the mark.** `DetentCloudLogo` (`app/account/Login.tsx`), used on the entry, platform console, support and setup screens, draws a "D" letter in a `bg-primary` tile. It does not use the mark. Its "Cloud" is `font-light`, whereas the sidebar uses `font-semibold`.
3. **Duplicated path data.** The sign-in header duplicates the mark's path rather than importing `DetentWordmark`.
4. **Redundant labelling.** In the sidebar the mark carries `aria-label="Detent"` inside a link that already has an accessible name, and the SVG has no `role="img"`. The mark should be decorative (`aria-hidden`) wherever visible text or the link name already says "Detent".

### Size and clear space (Proposed)

- **Minimum size (proposed):** 16px rendered box height for the mark in interface chrome, which matches the sidebar today. The favicon raster minimum is 32px. Smaller placements have not been reviewed.
- **Clear space (proposed):** at least 25% of the mark's rendered box on every side, measured from the box rather than the glyph. The built-in padding (15–22%) is part of that allowance. In the sidebar row, the 6px gap (`gap-1.5`) to the 16px mark is about 38%.
- **Mark with name (proposed):** keep the mark vertically centred on the cap height of the product name. Keep "Detent" and "Cloud" at the same size and weight, separated only by colour, as the sidebar row does.
- **Colour (proposed):** in product UI the mark takes the foreground colour of its surface (`currentColor`). The fixed indigo is reserved for contexts that have no theme: the favicon, README and email.

## Separate surface contracts

Two Detent surfaces do not use the Cloud workspace tokens. They are current contracts in their own right, not drift to be corrected, so record them as exceptions and do not unify them as a side effect of other work.

| Surface | Owner | Fonts | Accent | Notes |
| --- | --- | --- | --- | --- |
| Public sign-in | `.detent-sign-in` in [`index.css`](../../web/conversation/src/app/index.css) (lines 112–146), used by `app/account/Login.tsx` | Geist first (`"Geist", ui-sans-serif, system-ui, sans-serif`), Geist Mono for `.font-mono` | Teal: `#0f766e` light, `#2dd4bf` dark | Scoped palette of 14 tokens; the `--contrast-*` aliases are mapped straight to its roles. Contrast passes; see [color review](color-review.md#contrast). Its header mirrors detent.build. |
| Local Templ dashboard | `static/css/input.css` | Geist first, Geist Mono | Teal `--color-accent` (`#2dd4bf` dark, `#0f766e` light), reserved for interactivity and never used for status | 6px cards (`--radius-card`), 4px chips (`--radius-chip`); separate `--color-ok`/`warn`/`err`/`info` status palette |

The Cloud workspace uses the system font stack with Geist fallbacks and the blue primary ([foundations](foundations.md#typography)). [decisions.md §8](../conversation/decisions.md) records that Templ pages keep their teal accent until the shells are unified. Unifying them is a scoped visual decision for a human-authored issue.

## Interface voice

Detent's users hand work to agents and come back to check it. The interface tells them what happened, what is waiting on them and what they can do next, in the fewest exact words.

- **Sentence case** for titles, labels, buttons, menu items and toasts: "Sign in to Detent", not "Sign In To Detent". Proper nouns keep their case (Detent Cloud, GitHub, Codex).
- **Concrete action labels** that name the result, for example "Create pull request" or "Open in editor". Avoid "OK", "Submit" and "Continue" when a verb and object fit. Use the same verb in the confirmation or toast.
- **Distinguish four kinds of nothing**, and never present one as another:
  - *Empty*: there is nothing yet. Explain what will appear and offer the next action, for example "No conversations yet".
  - *Zero*: a measured count of zero. Show the number with its unit, for example "0 runs this week", not a blank.
  - *Denied*: the content exists but this person cannot see it. Name the missing permission or role.
  - *Unavailable*: the capability does not exist here, or failed to load. Say which. The client already does this for capabilities Cloud lacks: "Not available on Detent Cloud yet." A failed load should offer a retry, not claim the collection is empty.
- **Errors state what failed and how to recover**, from the user's point of view: "Your session expired. Sign in again." and "That sign-in did not complete. Try again." (both from `Login.tsx`).
- **No internal mechanism names in product UI.** Orchestrator internals such as brakes, breakers, leases, parks, recovery sweeps, reason codes and scheduler evidence stay in logs, the API and MCP reads. A status line names the wait in actionable words (INV-13: one line, at most 48 characters). Diagnostic data is never surfaced in UI to make it observable (INV-15).
- **Attribute infrastructure failures to the instance**, not to the user's issue or work: "The runner could not start", not "This issue failed".
- **Keep the agent's work distinguishable from Detent's statements.** Quote agent output as output, and do not restate an inference as a fact.
- **No exclamation marks, emoji or filler** ("Oops", "Great!", "Successfully") in product copy.
