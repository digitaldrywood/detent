# Brand in product UI

Detent runs coding agents against a team's issues and reports what they did. The Cloud UI is a working tool. Its identity is carried by the mark, the product name and plain, exact language, not by decoration. This document states how that identity appears in product UI. Marketing pages keep their own styling and do not set rules for the workspace; a mark redesign is out of scope.

## The mark

The Detent mark is a heavy "D" with a counter and a dot, drawn on a 100×100 viewBox. The glyph occupies x 22–87 and y 15–85, so the artwork already includes 15–22 units of padding inside its box.

| Asset | Fill | Use |
| --- | --- | --- |
| [`web/conversation/src/components/DetentWordmark.tsx`](../../web/conversation/src/components/DetentWordmark.tsx) | `currentColor` | The mark in the Cloud client. It is the only source of the mark's path in React code. |
| [`docs/brand/detent-mark-mono.svg`](../brand/detent-mark-mono.svg) | `currentColor` | Single-colour placements outside React |
| [`docs/brand/detent-mark.svg`](../brand/detent-mark.svg) | Fixed `#3730A3` (indigo) | Contexts without a theme: documentation, README (88px), email |
| `static/img/detent-mark.svg`, `static/img/detent-mark-mono.svg` | Byte-identical copies of the two files above | Templ pages: favicon and the invitation page header |
| `web/conversation/public/favicon.svg` | `#3730A3` mark on a white tile with a 20-unit corner radius | Cloud client favicon (SVG) |
| `web/conversation/public/favicon-32.png` | 32×32 raster | Cloud client favicon fallback |
| `web/conversation/public/apple-touch-icon.png` | 180×180 raster | iOS home-screen icon |

`index.html` links the three client icons under `/static/app/conversation/`. Despite its name, `DetentWordmark` renders only the mark; the word "Detent" beside it is live text.

### Rules

1. **One source.** Render the mark with `DetentWordmark` in React and with the SVG assets elsewhere. Never copy its path data into another component, redraw it, outline it, stretch it, or replace it with a letter in a tile.
2. **Colour.** In product UI the mark takes the foreground colour of its surface (`currentColor`), so it follows both themes. The fixed indigo `#3730A3` is reserved for contexts that have no theme: the favicon, the README and email. It is not the product primary, and the mark is never painted in `primary` or any other role colour.
3. **Size.** The mark is at least 16px tall (`size-4`) in interface chrome. The favicon raster is at least 32px.
4. **Clear space.** Keep at least 25% of the mark's rendered box clear on every side, measured from the box rather than the glyph; the built-in padding counts towards it. Beside a 16px mark, `gap-1.5` (6px) meets this.
5. **Mark with the product name.** Centre the mark vertically on the name's cap height. Set "Detent" and "Cloud" at the same size and weight (`text-base font-semibold` in the sidebar brand row), separated only by colour: "Detent" in the foreground colour, "Cloud" in `text-muted-foreground`. On a stage backdrop the row is white and "Cloud" is white at 70%.
6. **Accessible name.** Where visible text or the enclosing link already says "Detent", the mark is decorative: `aria-hidden="true"` and no label. Where the mark stands alone, it is `role="img"` with `aria-label="Detent"`.

### Placements

- **Sidebar brand row** (`components/sidebar/SidebarChrome.tsx`, `SidebarBrand`): the Cloud workspace's primary identity. It reads "Detent Cloud" under rule 5, links to `/work`, and the link's accessible name is "Go to Detent Cloud".
- **Entry, sign-in, support and setup screens**: the same mark-and-name lockup as the sidebar, through `DetentWordmark`.
- **Work timeline** (`components/chat/MessagesTimeline.tsx`): the mark is the decorative icon of Detent's own work-log entries.
- **Templ pages**: `static/img/detent-mark.svg` as the favicon and in the invitation header, with `alt=""` beside the visible word "Detent".

## Separate surface contracts

Two Detent surfaces do not use the Cloud workspace tokens. Each is a contract in its own right: keep its palette inside its own scope, do not spread it into the workspace, and do not unify it with the workspace as a side effect of other work.

| Surface | Owner | Fonts | Accent | Notes |
| --- | --- | --- | --- | --- |
| Public sign-in | `.detent-sign-in` in [`static/css/sign-in.css`](../../static/css/sign-in.css), used by `app/account/Login.tsx` and the server-rendered entry pages | Geist first (`"Geist", ui-sans-serif, system-ui, sans-serif`), Geist Mono for `.font-mono` | Teal: `#0f766e` light, `#2dd4bf` dark | Scoped palette of 14 tokens; the `--contrast-*` aliases are mapped straight to its roles. Contrast passes; see [color review](color-review.md#contrast). Its header mirrors detent.build. |
| Local Templ dashboard | `static/css/input.css` | Geist first, Geist Mono | Teal `--color-accent` (`#2dd4bf` dark, `#0f766e` light), reserved for interactivity and never used for status | 6px cards (`--radius-card`), 4px chips (`--radius-chip`); separate `--color-ok`/`warn`/`err`/`info` status palette |

The Cloud workspace uses the system font stack with Geist fallbacks and the blue primary ([foundations](foundations.md#typography)). [decisions.md §8](../conversation/decisions.md) records that Templ pages keep their teal accent until the shells are unified; unifying them is a scoped visual decision for a human-authored issue.

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
