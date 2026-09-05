---
name: inherited-git-fixture-isolation
description: Diagnose Git fixtures that mutate a source repository through inherited environment settings.
when_to_use: Use when fixture identities or Git mutations escape temporary repositories, especially in hooks or linked worktrees.
---

# Isolate Git fixture subprocesses

- Inspect the origins of user.name and user.email read-only. Record inherited Git variable names without printing possible credential values. Do not attribute an existing override to a helper without evidence.
- Build a disposable source repository and linked worktree. Run the suspect fixture command with inherited GIT_DIR, GIT_COMMON_DIR, and GIT_CONFIG separately. A working directory or git -C does not override these routing settings.
- Compare the disposable source/common config byte-for-byte before and after each case. Never reproduce by targeting the real shared repository.
- When an entire test package owns temporary Git fixtures, clear inherited GIT_* variables in TestMain before any tests run. This also protects application subprocesses and hook commands reached by tests. Preserve non-Git environment settings and allow individual tests to set explicit Git variables afterward.
- Exercise the real fixture helper in a child test executable, injecting hostile Git settings only into that child. Starting the child from a disposable linked worktree tests both startup isolation and common-directory resolution without mutating the parallel parent environment.
- Cover routing variables, config file and command-config injection, index/worktree overrides, and author overrides. Assert fixture ownership and commit identity as well as unchanged source config. Give each case its own disposable source.
- Prove the regression fails before the fix, run focused race tests, and run the repository gate. Keep historical attribution separate from a verified reproduction mechanism.
