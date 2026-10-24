---
name: portable-git-fixture-commands
aliases:
  - inherited-git-fixture-isolation
  - windows-shell-command-boundary
description: "Isolate Git fixtures from inherited environment and preserve command arguments across POSIX and Windows shells."
when_to_use: "Use when a shell-driven Git fixture passes on POSIX but Windows reports a quoted directory argument as invalid. Also use for inherited git fixture isolation, windows shell command boundary."
---

# Portable Git fixture commands

- Check the child command's actual error. Literal surrounding quotes in a rejected `git -C` directory can indicate command-line construction at the `cmd /C` boundary, before the behavior under test executes. A unit test of the constructed argument slice does not prove Windows execution works.
- For large GitHub Actions logs containing Go test JSON, identify events with `Action: fail`, then collect output for their package/test pairs. Searching for words such as `timeout` also matches passing test names and can hide the relevant failure.
- When the test only needs to mutate a generated sibling repository, derive its path with `filepath.Rel` from the command's working directory and normalize separators with `filepath.ToSlash`. An unquoted relative path is appropriate only when its remaining components are controlled fixture names without whitespace or shell metacharacters; a shared temporary-root prefix containing spaces then disappears.
- Keep quoted user-path coverage at the shared command boundary. Do not treat a fixture-relative path as a production quoting fix, strip user quotes, or skip Windows coverage. Track a discovered shared-shell defect separately when it is outside the current task.
- Verify the fixture still performs its intended mutation and that the protected operation rejects or accepts it for the intended reason. Confirm the corrected command through native Windows CI as well as focused local tests.

## Isolate Git fixture subprocesses

Use this case when fixture identities or Git mutations escape temporary repositories, especially in hooks or linked worktrees.

- Inspect the origins of user.name and user.email read-only. Record inherited Git variable names without printing possible credential values. Do not attribute an existing override to a helper without evidence.
- Build a disposable source repository and linked worktree. Run the suspect fixture command with inherited GIT_DIR, GIT_COMMON_DIR, and GIT_CONFIG separately. A working directory or git -C does not override these routing settings.
- Compare the disposable source/common config byte-for-byte before and after each case. Never reproduce by targeting the real shared repository.
- When an entire test package owns temporary Git fixtures, clear inherited GIT_* variables in TestMain before any tests run. This also protects application subprocesses and hook commands reached by tests. Preserve non-Git environment settings and allow individual tests to set explicit Git variables afterward.
- Exercise the real fixture helper in a child test executable, injecting hostile Git settings only into that child. Starting the child from a disposable linked worktree tests both startup isolation and common-directory resolution without mutating the parallel parent environment.
- Cover routing variables, config file and command-config injection, index/worktree overrides, and author overrides. Assert fixture ownership and commit identity as well as unchanged source config. Give each case its own disposable source.
- Prove the regression fails before the fix, run focused race tests. Keep historical attribution separate from a verified reproduction mechanism.

## Windows Shell Command Boundary

Use this case when: A configured Windows command works interactively but a Go-launched child receives literal quotes, split paths, expanded percent expressions, or corrupted backslashes.

- Trace both parsers: Go normally serializes argv for CommandLineToArgvW, while cmd.exe interprets a script. Inspect the actual child argv rather than only exec.Cmd.Args.
- Reproduce with a real child process. A test helper that emits its argv as JSON exposes empty arguments, quotes, spaces, percent signs, and trailing backslashes. Use Git with a quoted existing directory when the reported symptom involves git -C.
- Keep a legacy-launch control when Windows execution is available only in CI: require the recorded failure from the original boundary and success from the corrected boundary in the same Windows test.
- For cmd.exe, use Windows-specific SysProcAttr.CmdLine with an outer quote pair and /S /C. Preserve the operator's configured script verbatim; do not escape intentional operators or environment expansion.
- Treat appended literal arguments separately. Encode backslashes before quotes and at the end for the child parser, then escape cmd metacharacters. Batch-file escaping rules are not interchangeable with direct /C execution.
- Test configured shell syntax and literal appended arguments independently, including percent expressions that name real environment variables. Test a quoted executable path as well as quoted directory arguments.
- Keep non-cmd shells on their established launch path. Windows PowerShell's legacy native argument parsing differs from modern PowerShell; do not silently redefine that contract while repairing cmd.
- Cross-compilation proves build compatibility only. Require current-head Windows execution evidence before declaring the repair validated.
