# Validation evidence for #2360

- Original regression: `TestBoardFiguresSchedulerRefusals` reproduced 1 running / 1 ready / 0 waiting instead of 1 / 0 / 1 for dependency and pool refusals with a worker running in Rework.
- Recovery: reviewed every retained source/test/generated file and browser artifact. All belong to this issue. Storage prerequisite #2364 is closed with recorded human completion evidence; generation now succeeds.
- Passed: `make generate`; complete telemetry, tmux, and template tests; focused web API and scheduler refusal/hydrated-dependency tests.
- Final Chrome DevTools verification used an isolated production `BoardPage` renderer with a seeded snapshot and worktree CSS on an ephemeral port. The overlay preview test passed and exited cleanly after browser closure. No live Detent process was modified.
- `browser.json` confirms 1 running / 0 ready / 2 waiting, visible dependency #2064 and pool 1/1 with fleet 1/3 and two idle slots, and no horizontal overflow. `board.png` captures the final Cozy-density view with the running worker in Rework and no In Progress cards.
- The preview has no SSE source, so its reconnecting banner is expected and unrelated to scheduler evidence.
- Focused browser suite passed: sidebar badge/count assertions, board overflow, and dependency waits. Updated the demo header assertion from one to six waiting issues because absent scheduler evidence no longer counts as ready.
- Broader local browser run: 173 passed, 32 skipped, two human-prerequisite signal failures. Preserved the existing Waiting · N blocker count, added table-driven Go coverage, and both formerly failing browser tests passed on rerun. macOS does not perform Linux screenshot comparison; that remains for CI.
- First `make check` exited before validation started after its 15-minute FIFO admission timeout. Retried with `CHECK_LOCK_WAIT=60m`; all gate checks remain enabled.

- Final `make check CHECK_LOCK_WAIT=60m` passed: build, lint (zero issues), vet, NilAway, full race suite, coverage 80.3% against 70%, and all configured package/file floors. The retry waited approximately 36 minutes for admission; validation itself completed successfully.

- Review regressions reproduced and fixed: sampled unsaved refusals no longer suppress a durable skipped fallback after selection; Todo operator-attention and conflict details retain priority over scheduler status. Focused Go tests pass. `review-browser.json` records visible Needs review / Upstream closed alongside scheduler evidence; the isolated preview passed. Follow-up full gate passed on recovery as recorded below.

- Follow-up admission timed out at 60 minutes while first in line; validation had not started. Retried with `CHECK_LOCK_WAIT=240m`, preserving the complete gate and serialization. Queue-loss evidence was added to existing issue #2347.

- Recovery follow-up: all six retained paths belong to the review fixes. The first recovery gate admitted after about 17 minutes and failed perfsprint on a boolean test-name conversion. Replaced it with `strconv.FormatBool`; both focused review regressions passed. `make check CHECK_LOCK_WAIT=240m` then passed after approximately 54 minutes admission and 7 minutes execution: generation, build, zero lint issues, vet, NilAway, full race suite, 80.3% aggregate coverage, and all configured floors.
