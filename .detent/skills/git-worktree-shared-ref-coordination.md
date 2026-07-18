---
name: git-worktree-shared-ref-coordination
description: Diagnose and prevent concurrent Git ref updates across linked worktrees without serializing unrelated repositories.
when_to_use: Use when parallel workspace hooks or merge preparation fail with cannot-lock-ref or expected-value mismatches under a shared Git common directory.
---

# Git worktree shared-ref coordination

1. Confirm the affected paths are linked worktrees of the same repository with `git rev-parse --path-format=absolute --git-common-dir`.
2. Trace whether the ref update comes from Detent-owned Git commands or a configured workspace hook. Preserve the full hook output because the final error often identifies the shared remote-tracking ref.
3. Reproduce the overlap before editing. Have one hook hold an exclusive test resource while a second hook for the same source attempts to enter; also prove hooks for different sources can still overlap.
4. Coordinate at the source-repository boundary with a context-aware keyed lock. Key by the canonical source path or common Git directory, cover hooks that may invoke arbitrary Git commands, and cover Detent-owned fetch/rebase/push sequences.
5. Keep non-transient command and hook failures unchanged. Do not mark an entire failed hook successful merely because one ref-lock message looks transient.
6. Run the focused test repeatedly with `-race`, then run the repository validation gate.
