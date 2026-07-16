---
name: subprocess-pressure-tracing
description: Measure subprocess fan-out behind intermittent crashes, fork/exec hangs, and orphaned child processes.
when_to_use: Use when focused tests pass but concurrent or race suites intermittently crash or hang an external command.
---

# Trace subprocess pressure

- Put a temporary executable with the same name as the target command first on `PATH`. Keep it under the Detent-provided temporary directory and have it append PID, timestamp, and arguments to a trace file before `exec` replaces it with the absolute real executable.
- Run the smallest concurrent package set that has exhibited the failure. Do not add retries or sleeps; preserve the original test scheduling and race settings.
- Group trace entries by normalized arguments and caller scenario. Separate required integration commands from accidental discovery against fake workspaces or ancestor repositories.
- Correlate the trace with the full failing log or stack. A failure in a later test-helper command after the production call succeeded indicates environmental process instability, not necessarily invalid fixture lifecycle.
- Prefer eliminating launches: reject non-target paths before spawning, bound repository discovery, and combine read-only queries into one command when the tool supports it.
- Repeat the same traced command after the change and record before/after launch counts. Then remove the tracing wrapper from `PATH` and run focused race repetitions plus the full validation gate.
- Keep real integration coverage for the external tool's observable behavior; replace only redundant setup or expectation commands with direct fixture facts when those facts are independently known.
