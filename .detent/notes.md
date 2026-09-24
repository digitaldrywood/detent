# Issue 3060 handoff

- Root path: `internal/orchestrator/autopromote.go` disabled the security audit gate for a clean Rework head with missing evidence, so Rework promotion never invoked `startSecurityAuditStage`. The existing merge gate still checks trusted evidence.
- Fix removes that bypass. `internal/orchestrator/rework_live_promotion_test.go` covers stale old-head run, pending current-head audit, and promotion after trusted pass. `internal/orchestrator/attempt_allowance_test.go` now expects audit start before Merging.
- `go test ./internal/orchestrator/...` and `go vet ./internal/orchestrator/...` passed. `make check-fast` passed 2026-09-24.
- Live PyroApex #2556 was merged by loganlanou at 10:52 UTC, base `9378fc1f74bfcc5ab8f83b6370d59037b2d24fc4`, head `47461c60b4d7bbeeadd782a6f18974a7bbd277ad`, merge commit `d4b075967ad4715e04b99b49afaae6acf7a8ea31`. Exact-head `detent audit evidence` still returns no trusted evidence. Port 4000 was unreachable; no audit run ID can be reported or triggered through the live service.
