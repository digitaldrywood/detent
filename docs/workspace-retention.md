# Workspace retention

The existing periodic workspace reaper applies these operator-approved limits
(#2681). It does not introduce another timer or configuration setting.

| Artifact | Retention |
| --- | --- |
| Retained completed worktree | Seven days after Done, Cancelled, or issue closure |
| Quarantine | Three days; at most the five newest entries per workdir |
| Hook log file | Fourteen days since its last modification |
| Attempt scratch directory | A terminal work attempt, or no session registry row and at least one hour old |
| Cleanup ownership record | Removed when its workspace path is absent |

Active issue ownership and live processes continue to protect workspaces.
Live processes also protect quarantine and scratch directories. Scratch UUIDs
are resolved through recorded session cleanup paths, not interpreted as numeric
work-attempt IDs. Failed tracker or registry reads do not authorize removal.
Artifact directories cannot redirect cleanup through symlinks.

GitHub closure time is preserved separately from the issue's last update time.
Lane retention uses the lane-entry timestamp, including the existing tracker
transition reader when needed. If completion time cannot be verified, the
retention sweep leaves the worktree for ordinary cleanup or a later sweep.
Ordinary cleanup of already-delivered, clean workspaces is unchanged.

## Recovering an expired retained workspace

Before removing a retained worktree, the sweep creates a durable directory under
`<workdir>/.detent/retained/` containing:

- `commits.bundle`: verified Git bundle containing HEAD and its complete ancestry.
- `working-tree.tar.gz`: working-tree files, including untracked and ignored files.
- `working-tree.diff`: binary diff between HEAD and the working tree.
- `staged.diff`: binary diff between HEAD and the index.

The log records the commit SHA, bundle, diff, and archive paths. Archives have no
automatic expiry. Archive failures, including an unresolved Git index, leave the
original worktree intact. Partial archives from failed archival attempts are
removed.

Restore into a new directory:

```sh
git clone /path/to/archive/commits.bundle recovered
cd recovered
git apply --cached /path/to/archive/staged.diff
git apply /path/to/archive/working-tree.diff
tar -xzf /path/to/archive/working-tree.tar.gz
```

Skip each `git apply` command when its diff is empty. Applying the working-tree
diff also restores tracked-file deletions, which a tar extraction alone cannot do. The working-tree archive includes
ignored artifacts; review those before using a recovered environment.

## Sweep totals

Each sweep logs removal counts and file bytes per class. `detent status` shows
the most recent in-memory totals for each workdir; they reset on restart until
the next sweep. Completed-workspace byte totals subtract the new archive size
and are floored at zero. These are logical file sizes, not filesystem block or
APFS clone accounting. The sweep never claims an archive failure as reclaimed
workspace bytes.
