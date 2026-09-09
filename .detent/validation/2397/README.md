# macOS setup-go cache abort investigation

## Recorded failure and recovery

Original [job 102479919330](https://github.com/digitaldrywood/detent/actions/runs/34355767002/job/102479919330)
and unchanged-head [retry job 102489040487](https://github.com/digitaldrywood/detent/actions/runs/34355767002/job/102489040487)
used setup-go commit `924ae3a1cded613372ab5595356fb5720e22ba16`, Go 1.26.6,
and the same 370,529,182-byte cache. Selected original timestamps are retained
in `failed-timing.txt` and `retry-timing.txt`.

| Operation | Original UTC | Retry UTC |
| --- | --- | --- |
| Setup action starts | 13:14:12.626 | 13:39:30.957 |
| Cache hit | 13:14:33.714 | 13:39:43.508 |
| Restore result | aborted 13:14:57.311 | restored 13:39:57.812 |
| Final go env group ends | 13:15:00.571 | 13:39:59.730 |
| Module download starts | never | 13:39:59.763 |
| Job canceled | 13:24:06.172 | passed |

The original transfer stopped advancing at 264,241,152 bytes (71.3%). The
cache restore returned a miss after warning about the abort. Go itself then
successfully printed its version and environment. No cache extraction command
was logged in the failed attempt.

At the exact action revision, [main.ts lines 70–101](https://github.com/actions/setup-go/blob/924ae3a1cded613372ab5595356fb5720e22ba16/src/main.ts#L70-L101)
awaits restore before printing the version and environment; ending the environment
group is its final operation. [cache-restore.ts lines 36–42](https://github.com/actions/setup-go/blob/924ae3a1cded613372ab5595356fb5720e22ba16/src/cache-restore.ts#L36-L42)
returns after logging the miss. Thus the remaining active step was the setup
Node action waiting to exit, for approximately 545.6 seconds after its final
operation, rather than Go installation, an awaited restore, extraction, or Go
validation. A lingering download socket/timer is plausible but the historical
log has no handle dump, so the exact live handle and abort cause are unproven.
The identical-head retry proceeded from the final group to module download in
32 milliseconds. It establishes intermittent recovery, not a root-cause proof.

## Mitigation

macOS portability runs setup-go with `cache: false`, avoiding both the optional
remote restore and its post-job save. Go toolchain installation remains enabled
and is bounded to three minutes. Setup failure remains fatal; build, vet, and
tests cannot produce a false green after a failed toolchain installation.
Windows retains its existing cache. The ten-minute macOS job limit is unchanged;
fresh CI must demonstrate that the cold module/build path fits that budget.

## Validation

- `actionlint .github/workflows/ci.yml`: passed.
- `git diff --check`: passed.
- `make check CHECK_LOCK_WAIT=60m`: passed; 80.4% coverage and all package/file floors met. Execution took 8m34s after a 33m16s queue wait. An earlier attempt expired at the default 15-minute queue deadline before validation started.
- Fresh PR CI: recorded in the issue Workpad after completion.

No Go behavior changed, so no implementation-mirroring Go test was added for
this declarative action input change. The fresh macOS job exercises the actual
uncached setup and all portability validation steps.
