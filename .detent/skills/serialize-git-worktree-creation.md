---
name: serialize-git-worktree-creation
description: Prevent concurrent Git fetch and worktree creation from observing partially written shared repository metadata.
when_to_use: Use when adding remote fetches or other shared-repository mutations to concurrent workspace creation paths.
---

# Serialize Git worktree creation

- Treat the source repository's object store, refs, and worktree metadata as shared state even when every worker has a separate filesystem path.
- Reproduce concurrent creation with one backend, unique issue branches, and a real local remote. Use an upload-pack wrapper with an atomic directory lock and overlap marker to widen and detect concurrent remote operations.
- Serialize remote-default discovery, explicit-refspec fetch, branch inspection, and `git worktree add` as one repository operation. Do not release the lock between fetch and add; that reintroduces the partially written worktree window.
- Scope the mutex to the backend when one backend owns the source repository. If independent backends or processes can share the Git common directory, use a common-directory keyed or OS-backed lock instead.
- Preserve existing managed branches without resetting them, and fail before branch or worktree creation when a configured remote cannot be resolved or fetched.
- Run the concurrent regression repeatedly with `go test -race`, then run the repository's full validation gate.
