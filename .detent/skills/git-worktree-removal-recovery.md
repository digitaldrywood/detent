---
name: git-worktree-removal-recovery
description: Diagnose and safely recover Git worktree removals that fail after partially deleting managed metadata.
when_to_use: Use when worktree cleanup fails, a managed path is misclassified after removal starts, or read-only files leave a partially removed worktree.
---

# Git worktree removal recovery

- Reproduce with an isolated source repository and place its managed worktree below a different ancestor repository. Add a read-only nonempty directory so Git removal fails after starting deletion.
- Snapshot managed ownership before `git worktree remove`; a nonzero exit does not mean Git left the worktree's `.git` pointer or registration intact.
- After failure, inspect the remaining `.git` pointer, `git -C <source> worktree list --porcelain`, and Git discovery from the remaining path. Treat ancestor discovery as environmental evidence, not ownership.
- Remediate permissions within the validated workspace root. Retry Git removal only while the path is still registered to the source; otherwise remove only the path whose ownership was confirmed before removal, then prune stale worktree metadata.
- Never use post-failure Git discovery alone to authorize raw deletion. Keep tests that prove foreign repositories are refused and unchanged.
- Make the regression deterministic with a foreign ancestor repository rather than relying on the test runner's host repository layout.
