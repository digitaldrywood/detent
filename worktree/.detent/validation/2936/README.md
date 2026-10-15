# Native command wait tuning — issue 2936

The local Detent database was queried read-only for completed `detent` project
sessions starting from September 15 through September 22, 2026 (UTC). The latest
record in the fixed window started at 19:37:24 UTC on September 22. There were
332 sessions, averaging 19.71 minutes, with a 14.70-minute median and 44.62-minute
p90. These are whole-session durations, not gate durations.

Provider thread IDs joined those records to 298 available local Codex transcripts.
Consecutive identical `CommandExecution` records were grouped within a transcript;
the interval extends from the first start to the last completion. Groups whose
command contains `make check` and whose elapsed interval is at least 30 seconds
provide 285 substantive gate-command timing proxies:

| Statistic | Minutes |
| --- | ---: |
| Mean | 13.64 |
| Median | 9.06 |
| p90 | 29.23 |
| p95 | 42.87 |
| p99 | 69.85 |
| Maximum | 75.98 |

The filter excludes short probes and immediate failures. Command text matching
can include compound commands, polling gaps count toward elapsed time, and
consecutive identical launches can be conflated. Missing transcripts and still
running sessions are excluded. This is a local-host tuning sample, not a claim
about fleet-wide cost or exact gate runtime. Aggregate results are in
[timing-summary.json](timing-summary.json); no transcript content is published.

## Decision

Use a 50-minute native empty `write_stdin` wait, with a 51-minute enclosing
code-mode wait. Both return early on completion; these are maximum waits, not
mandatory sleeps. Fifty minutes clears the observed p95 while leaving headroom
below the existing default one-hour backend stream timeout. Longer commands
reuse the same session when a wait reaches its cap. Initial exec retains the
backend's 30-second cap; only subsequent empty waits receive the longer window.

Remove the ordinary worker's competing five-minute inactivity timeout. Keep
operator-only tool sessions on their configured inactivity timeout. Preserve
configured stream timeouts, cancellation, and the runner's existing wall-clock
turn/session deadlines. On the measured instance those wall-clock limits are
four and five hours; the one-hour backend timeout only bounds gaps between
messages and is not a total runtime limit. No live settings were changed.

## Evidence and verification

The installed Codex CLI 0.155.1 accepted the existing
`background_terminal_max_timeout` setting through `config/read`. Its matching
source applies that limit to empty polls, and both thread start and resume accept
configuration overrides. The implementation sets this native key per worker
thread rather than adding a Detent configuration setting.

- [Official configuration reference](https://developers.openai.com/codex/config-reference/)
- [Native empty-poll handling](https://github.com/openai/codex/blob/rust-v0.155.1/codex-rs/core/src/unified_exec/process_manager.rs)
- [Code-mode wait handling](https://github.com/openai/codex/blob/rust-v0.155.1/codex-rs/code-mode-runtime/src/service.rs)

`TestAgentBackendNativeCommandWait` first failed with the existing five-minute
stall error, then passed with simulated 45-minute quiet commands. It covers
fresh/resumed workers, supplemental tools, read-only workers, operator timeout
isolation, configured stream limits, and cancellation without real-time sleeps.

The issue's under-5% waiting-response target requires fresh production rollouts
after deployment. It has not been measured by this local change. The runtime
waiting windows were tested at the Detent transport boundary; the local test does
not exercise an actual model or spend production inference tokens.
