# Workspace retention

The existing periodic workspace reaper applies these operator-approved limits
(#2681). It does not introduce another timer or configuration setting.

| Artifact | Retention |
| --- | --- |
| Completed worktree | One hour after entering a configured terminal lane or issue closure |
| Quarantine | Three days; at most the five newest entries per workdir |
| Hook log file | Fourteen days since its last modification |
| Attempt scratch directory | A terminal work attempt, or no session registry row and at least one hour old |
| Cleanup ownership record | Removed when its workspace path is absent |

Active issue ownership and live processes continue to protect workspaces.
Live processes also protect quarantine and scratch directories. Scratch UUIDs
are resolved through recorded session cleanup paths, not interpreted as numeric
work-attempt IDs. Failed tracker or registry reads do not authorize removal.
Artifact directories cannot redirect cleanup through symlinks.
Before deleting an expired quarantine tree, retention makes its directories
writable so read-only module caches can be removed. If a tree still cannot be
removed, the warning names that tree once per running orchestrator; later
sweeps continue trying to remove it.

Lane retention uses the lane-entry timestamp, including the existing tracker
transition reader when needed. If completion time cannot be verified, the
retention sweep leaves the worktree for ordinary cleanup or a later sweep.
Ordinary cleanup of already-delivered, clean workspaces is unchanged.

## Expired workspaces

Work that must survive is published to the Hub before its item reaches a
terminal state (INV-17), so an expired completed worktree is deleted without a
local copy. Recover completed work from its merged commits or Hub checkpoint,
never from the runner.

## Sweep totals

Each sweep logs removal counts and file bytes per class. `detent status` shows
the most recent in-memory totals for each workdir; they reset on restart until
the next sweep. These are logical file sizes, not filesystem block or APFS clone
accounting. Cleanup counts describe actual removed worktrees; verify path
absence before reporting recovered storage.
