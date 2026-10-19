---
name: git-fetch-arbitrary-pr-base
description: Fetch and rebase arbitrary pull request base branches safely when the source repository may use a limited fetch refspec.
when_to_use: Use when merge preparation must support per-PR base branches that a single-branch or refspec-limited clone may not already track.
---

# Fetch an arbitrary pull request base

- Read the per-PR base ref from pull request metadata; do not replace it with a repository-wide configuration value.
- Inspect `remote.<name>.fetch` before assuming `git fetch <remote> <branch>` creates `<remote>/<branch>`. A limited fetch refspec may update only `FETCH_HEAD`.
- Fetch into the remote-tracking ref explicitly with `git fetch <remote> +refs/heads/<branch>:refs/remotes/<remote>/<branch>`, then rebase onto `<remote>/<branch>`.
- Reproduce missing-ref failures by limiting the remote fetch refspec to another branch and deleting the target remote-tracking ref before the fetch.
- Cover both an explicit pull request base and the remote symbolic HEAD fallback with real-Git tests. Assert the intended base content is present and unrelated trunk content is absent.
