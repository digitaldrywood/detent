# Linked GitHub issue intake verification

Browser evidence used the real hosted Hub and built React client, with an isolated
database, fake identity provider, and random loopback port. GitHub transport on
Hub remained disabled. All fixture scripts, overlays, logs and scratch databases
lived under the worker-provided TMPDIR. The live port-4000 process was untouched.
Chrome DevTools tools were absent from the worker inventory; the installed
Playwright Chromium browser supplied the verification.

- `pending.png`: historical GitHub source link and pending intake before dispatch.
- A New issue dialog submission with only a GitHub URL returned the same native
  issue successfully, without copying title or body.
- `complete.png`: completed intake and expandable retained source title/body,
  with the observed timestamp. The browser's completed state was seeded into the
  isolated fixture; atomic intake itself was verified through the actual API tests.
- `mobile.png`: the same completed state at 390 × 844, with no horizontal overflow.

Focused Go tests cover URL/repository validation, duplicate linking, authenticated
GraphQL pagination, authorization and rate-limit failures, incomplete fetches,
claim-fenced atomic persistence, concurrent retries, native edits, instance
failure attribution, credential isolation, and native landing to Done without
further GitHub issue reads. Focused React tests cover URL-only submission and
existing creation/board behavior. The configured handoff gate is `true`.
