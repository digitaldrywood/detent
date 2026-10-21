---
name: git-worktree-shared-ref-coordination
aliases:
  - git-worktree-removal-recovery
  - serialize-git-worktree-creation
description: "Coordinate shared Git refs, worktree creation, and partial removal across concurrent worktrees."
when_to_use: "Use when parallel workspace hooks or merge preparation fail with cannot-lock-ref or expected-value mismatches under a shared Git common directory. Also use for git worktree removal recovery, serialize git worktree creation."
---

# Git worktree shared-ref coordination

1. Confirm the affected paths are linked worktrees of the same repository with `git rev-parse --path-format=absolute --git-common-dir`.
2. Trace whether the ref update comes from Detent-owned Git commands or a configured workspace hook. Preserve the full hook output because the final error often identifies the shared remote-tracking ref.
3. Reproduce the overlap before editing. Have one hook hold an exclusive test resource while a second hook for the same source attempts to enter; also prove hooks for different sources can still overlap.
4. Coordinate at the source-repository boundary with a context-aware keyed lock. Key by the canonical common Git directory, cover hooks that may invoke arbitrary Git commands, and cover Detent-owned fetch/rebase/push sequences. Different configured source paths can be linked worktrees of the same common directory.
5. Keep non-transient command and hook failures unchanged. Do not mark an entire failed hook successful merely because one ref-lock message looks transient.
6. Run the focused test repeatedly with `-race`.

## Git worktree removal recovery

Use this case when worktree cleanup fails, a managed path is misclassified after removal starts, or read-only files leave a partially removed worktree.

- Reproduce with an isolated source repository and place its managed worktree below a different ancestor repository. Add a read-only nonempty directory so Git removal fails after starting deletion.
- Snapshot managed ownership before `git worktree remove`; a nonzero exit does not mean Git left the worktree's `.git` pointer or registration intact.
- After failure, inspect the remaining `.git` pointer, `git -C <source> worktree list --porcelain`, and Git discovery from the remaining path. Treat ancestor discovery as environmental evidence, not ownership.
- Remediate permissions within the validated workspace root. Retry Git removal only while the path is still registered to the source; otherwise remove only the path whose ownership was confirmed before removal, then prune stale worktree metadata.
- Never use post-failure Git discovery alone to authorize raw deletion. Keep tests that prove foreign repositories are refused and unchanged.
- Make the regression deterministic with a foreign ancestor repository rather than relying on the test runner's host repository layout.

## Serialize Git worktree creation

Use this case when adding remote fetches or other shared-repository mutations to concurrent workspace creation paths.

- Treat the source repository's object store, refs, and worktree metadata as shared state even when every worker has a separate filesystem path.
- Reproduce concurrent creation with one backend, unique issue branches, and a real local remote. Use an upload-pack wrapper with an atomic directory lock and overlap marker to widen and detect concurrent remote operations.
- Serialize remote-default discovery, explicit-refspec fetch, branch inspection, and `git worktree add` as one repository operation. Do not release the lock between fetch and add; that reintroduces the partially written worktree window.
- Scope the mutex to the backend when one backend owns the source repository. If independent backends or processes can share the Git common directory, use a common-directory keyed or OS-backed lock instead.
- Preserve existing managed branches without resetting them, and fail before branch or worktree creation when a configured remote cannot be resolved or fetched.
- Run the concurrent regression repeatedly with `go test -race`.
