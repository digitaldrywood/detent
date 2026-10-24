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


## Issue-intake integration Rework — attempt 7327, generation 15

Merged target `69874645ab6254f5480990b298e0ce4deeb5e987` while retaining recovered merge `a03aaa99b` and the published PR history. Heartbeat now retains both local-check persistence and incoming issue-intake delivery. Develop migration 51 is preserved; the unlanded local-check migration moves to 52. Focused upgrade cases retain persisted intake/archive data, foreign keys, integrity and reopen. This supersedes the version-51 runner-check statement in the previous section; earlier preview databases were disposable.

Fresh focused Go diagnostics passed (16.8s command), including actual hubclient bounded-intake behavior. TypeScript and 113 account/setup/enrollment/intake tests passed (17.8s). `make generate` rebuilt the combined bundle successfully (18.4s).

Fresh Playwright browser regressions passed at 1280×900 and 390×900 (21.1s command). The real React source with mock API uses ephemeral ports and shuts down automatically. Coverage includes enrollment before the incoming GitHub intake card, runner-first step order, contextual observed checks without attestations, explicit approval help, default capacity 1, independent workspaces/agents, host/provider limits, keyboard/hover/tap, input errors and horizontal bounds. Updated `rework-enrollment-1280.png`, `rework-enrollment-390.png`, `rework-capacity-help-1280.png`, and `rework-capacity-help-390.png`; screenshots show capacity 6 after the test verifies default 1 and exercises editing. Desktop/mobile screenshots were visually inspected. Live port 4000 was untouched and all identities are synthetic.

`mcp__chrome-devtools__navigate_page` is absent from this worker's tool list; no fresh Chrome MCP inspection is claimed. Earlier empty-organization and real-Hub checks/policy screenshots remain historical evidence for their unchanged behavior. This current browser pass covers the combined enrollment/setup source using mock APIs and is not a new real-Hub claim.


## Rework browser integration — attempt 7349, generation 37

Merged develop 81311b8e8857c53ce6da3d57e810368c70e323be while preserving published history. Production source, generated assets and existing screenshots are unchanged. Updated three existing real-Hub browser test assertions/selectors to match the runner-first order and Concurrent work items field. The old first-step assertion reproduced its failure against the real Hub before the correction.

All six focused browser tests passed (25.1s command, 24.5s run): real-Hub first-run wizard, desktop/390px choice layout and accessibility, enrollment/token flow, plus real React/mock-API contextual help and enrollment at 1280px/390px. Default capacity 1, independent workspaces/agents, host/provider limits, no attestations, explicit policy help and enrollment before issue intake remain asserted. New desktop/narrow screenshots were visually inspected; raw screenshots, logs and traces stay in the provided TMPDIR under 3244-browser, rather than becoming additional tracked artifacts. Earlier tracked screenshots remain historical evidence for the unchanged UI.

The existing specs/helper/config were copied under TMPDIR with only the helper output destination redirected, keeping all scratch output there. Isolated preview ports and normal fixture teardown were used. Chrome DevTools navigation is absent in this worker; no fresh MCP inspection is claimed. The live port-4000 process was untouched.


Publication target refresh merged unified sign-in at ade35a4be65891610b8fb890ce1a3ec145374bbe. The conflicted React bundle was regenerated with make generate (19.9s); TypeScript checking passed. An existing resumed checkout component test was corrected to select Repository configuration explicitly; all 113 selected frontend tests passed. The final browser run passed seven tests (38.5s), adding the incoming sign-in flow to the six onboarding/enrollment checks above. Current scratch evidence uses the same 3244-browser directory; the refreshed upstream hosted-Hub helper natively uses provided scratch. No production runner-first behavior changed, no new test was added, and no MCP/current-head CI credit is claimed.


## Recovered merge verification — attempt 7361, generation 1

Completed the recovered merge of develop `11f26bdd299cc6b267ff35bd75f73241d0de5f89` into published PR head `33e03f960ea0f28a3650b22b343ee5e4ff8855e1`. Inspected all 26 recovered paths. Incoming lesson-write removal and settings help match the target; the unfinished runtime notes rewrite was discarded and published notes remain intact. Retained current target invariant documentation, including native dependency ordering. No new runner-first source behavior or tests were added.

`make generate` passed (16.1s) and reproduced the combined tracked bundle. Focused Go diagnostics passed across onboarding, runnerauth, Hub server/client and CLI (15.3s), covering local checks, registration, policy-independent enrollment, migration/archive/intake preservation and heartbeat delivery. Additional focused readiness/startup and incoming runner/orchestrator/project integration checks passed (7.8s). TypeScript checking and 122 selected frontend tests passed (15.6s command, 6.4s Vitest).

All seven existing Playwright regressions passed (25.0s command, 24.4s run), including real-Hub sign-in, first-run runner-first setup, desktop/phone layout and accessibility, enrollment/token flow, and real React/mock-API contextual help at 1280px/390px. Default capacity 1, contextual independent-workspace/agent explanation, policy help, observed checks without attestations, input errors and horizontal bounds remain asserted. Desktop and 390px enrollment screenshots and narrow capacity-help evidence were visually inspected. Screenshots exercise capacity 6 after asserting default 1. All raw evidence remains under provided TMPDIR/3244-browser; fixture servers use ephemeral ports and normal teardown. Chrome DevTools MCP navigation is absent, so no MCP run is claimed. Live port 4000 was untouched.

Migration numbers are unique through 52. Full issue source diff reviewed; no conflict markers or handwritten whitespace errors. The required gate is `true`; no full suite, coverage/race gate or CI waiting was performed. Exact publication evidence is recorded in the canonical issue Workpad.
