# Runner-first Cloud setup (#3244)

Verified with the real generated React bundle and isolated `TestHostedBrowserPreview` Hub fixtures. Each preview used an ephemeral loopback port, its own database/config and synthetic identities. The live dogfood process on port 4000 was untouched. Both previews exited successfully after their stop endpoint was called.

- Desktop: 1280×900. Setup opens on Execution runner and offers enrollment before policy approval. The dialog defaults concurrent work items to 1 and explains separate workspaces/agents and host/provider limits.
- Mobile: 390×844 device viewport. Setup and dialog document width was 390px. The enrollment dialog fit the viewport width; tall setup content scrolls normally.
- Resumed setup: a runner reporting missing checkout opens Local validation, with checkout fix and pending doctor/provider checks. Failed doctor and missing provider sign-in show contextual fixes without any attestation checkboxes.
- Successful reports make the first two steps ready. Reload opens Repository configuration; the runner-reported descriptor can be expanded and approved without pasting JSON. The third step becomes ready only after that explicit approval; artifact selection remains required.
- Final bundle: policy ID column measured 188px client width and 188px scroll width at 390px, with no card overflow.

Screenshots capture first enrollment, desktop/mobile dialog, missing/failed checks, and final policy approval/review. All identities and descriptor contents are test fixtures; no enrollment tokens or provider credentials appear.

Focused Go diagnostics across the touched packages, frontend typecheck and 108 account/setup/dialog tests passed after rebasing. `make generate` passed. The configured repository gate is `true` and publishes no status; full CI, coverage and race gates are disabled for this project.

## Merge-conflict rework (2026-09-30)

Rebased onto `feaa4b57e` (`develop`). Rebuilt the conflicting generated React bundle from the combined source with `make generate`; setup, enrollment, account contract and runner diagnostics behavior are unchanged from the browser-verified source above. Current browser tool discovery has no `mcp__chrome-devtools__navigate_page`; no new browser run is claimed.

Upstream issue archival owns Hub migration 44. Runner local checks now use migration 45 and the supported schema version is 45. Only disposable preview databases used the unmerged runner migration numbered 44. The focused upgrade regression covers persisted schemas 43 and upstream 44, preserves issue/archive state, verifies the archive index and local-check column, and checks foreign keys, integrity and reopening.

Fresh rework diagnostics passed: focused onboarding/runnerauth/Hub client/server/CLI Go tests, TypeScript typecheck, all 108 account/setup/enrollment tests, and the migration-number uniqueness diagnostic. `make generate` took 23.1s; the longest focused Go package was Hub server at 10.8s; Vitest took 4.2s. Prior desktop/mobile screenshots remain acceptance evidence for unchanged setup and enrollment source.
