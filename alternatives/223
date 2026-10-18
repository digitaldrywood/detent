# Issue #3258 browser evidence

Chrome DevTools verified the actual React setup wizard against an isolated
mock Hub and Vite server, without accessing the live port-4000 instance.
The intake API fixtures were intercepted in this browser only; persistence,
runner authorization, retry, source identity, and landing were checked with
focused Go tests against real isolated Hub services.

- Desktop 1280 x 1100: open-issue preview, expanded source body, labels,
  count/selection and Backlog destination; no horizontal overflow.
- Selecting all matching and changing destination to Todo kept the import
  disabled until its execution checkbox was checked. The submitted body
  contained numbers [12,13,14], destination Todo, and allow_dispatch true.
- Mobile 390 x 1000: completed/skipped/incomplete counts, per-issue rate-limit
  diagnostic and retry deadline remained readable, without horizontal overflow.
- Refresh progress made an explicit read; no recurring intake polling ran.

Screenshots: `intake-desktop.png`, `intake-mobile.png`.
