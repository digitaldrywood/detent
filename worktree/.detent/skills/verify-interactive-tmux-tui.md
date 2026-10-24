---
name: verify-interactive-tmux-tui
description: Reproduce and validate tmux behavior in Detent's real interactive TUI path without touching the live orchestrator.
when_to_use: Use when tmux integration works in unit or headless tests but is reported broken while an operator runs Detent's interactive terminal dashboard.
---

# Verify the interactive TUI inside tmux

- Build the target source into the Detent-provided temporary directory. For a release regression, export the exact tag there and build that source separately.
- Generate an isolated memory-tracker runtime under the same temporary directory, then stop it and reuse its config with the root `detent --config ... --port 0` command so stdout remains a TTY and launches the terminal dashboard.
- Start a private tmux server with its socket under the Detent temporary directory. If the absolute socket path exceeds the platform limit, change into that directory and use a relative `-S` socket name.
- Create a target window and an untouched control window. Disable `automatic-rename` and `allow-rename` on both so an explicit rename persists and terminal escape sequences cannot confound the result.
- Attach a real client to the target window before starting Detent. Launch Detent through `tmux send-keys`, then poll tmux window metadata for the expected name with a bounded deadline.
- Record stable window IDs, pane IDs, names, active flags, and current commands. Assert that only the caller window changes, the control window remains untouched, and the displayed counts match the isolated board state.
- Inspect the isolated runtime log for initialization and first-success records. Send the TUI its normal interrupt, verify the original target name is restored, and detach the private client. Never bind port 4000 or signal the live Detent process.
