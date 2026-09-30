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

## Current-target merge Rework (attempt 7174)

Recovered merge 6bcebe6a6 moves the unlanded local-check migration to 49, following develop's hosted Blocked-lane migration 48. SQL is unchanged. Current upgrade/archive/integrity/reopen and 47→48 lane diagnostics passed, along with runner-first readiness, sanitized observations, registration ordering and policy-independent enrollment.

Merged develop e109b2f8e contextual onboarding help with runner-first ordering and observed checks; help no longer describes browser attestations. `make generate` passed (19.1s). TypeScript checking and 109 focused account/setup/dialog tests passed (Vitest 3.27s). Focused Go diagnostics took 13.8s; Hub client only compiled with no matching tests. Migration versions are unique through 49.

Fresh isolated Playwright verification passed at 1280×900 and 390×900 (17.2s). It verifies runner-first step order, absence of attestation checkboxes, local-check help, explicit approval help, default capacity 1, separate workspace/agent explanation, keyboard/hover/tap interaction, unchanged choices, input errors and horizontal bounds. The initial run exposed a test locator ambiguity with two runners; the help interaction is now scoped to the first runner. New `rework-*.png` screenshots record the merged enrollment and capacity help. This fixture uses the real React source with the existing mock API on ephemeral ports; the earlier screenshots cover real isolated Hub runtime behavior. No chrome-devtools MCP was available and no fresh MCP session is claimed. Port 4000 untouched.


## Rework verification — attempt 7299, generation 8

Merged develop `3ce76fc185cf1d090eacaa27779ac77cfc84587c` into published head `af21bc23c67275f267696a4f352180b370a85e00`. Only notes conflicted; production/frontend source merged unchanged. Migration 51 is unique, supported version and upgrade expectations agree, and current archive/integrity/reopen diagnostics pass. Earlier version-45/49 records above are historical.

Focused Go diagnostics passed across Hub server (2.449s), onboarding (0.885s), runnerauth (1.099s) and CLI (9.637s); command wall time 23.5s. Hub client compiled with no matching tests, without behavioral test credit. TypeScript checking and 109 selected account/setup/dialog tests passed (16.8s command, Vitest 6.18s). `make generate` passed in 20.1s and reproduced all tracked generated assets exactly; existing build warnings remain.

Fresh Chrome DevTools inspection used real React source with the existing mock API in isolated fixtures, independent Chrome context, and local preview ports 5173/5175. Empty-organization first-run setup opened Execution runner and offered enrollment before policy approval. Resumed setup opened observed Local validation with contextual hints and no attestation checkboxes. Enrollment defaulted to 1 and its help described independent workspaces/agents, host resources and provider limits. Desktop enrollment was inspected at 1280×900; device emulation verified 390×900 setup/dialog/help and document width 390px, with dialog width 390px and no horizontal overflow. No console errors were observed. All current screenshots contain synthetic fixture data. Both previews exited 0 after browser closure; live port 4000 was untouched.

Fresh screenshot evidence: `current-enroll-desktop.png`, `current-enroll-390.png`, `current-capacity-help-390.png`, `current-first-setup-390.png`, `current-resumed-checks-390.png`. Earlier real Hub screenshots remain historical acceptance evidence; this fresh browser pass uses the mock API and makes no new real-Hub claim.
