---
name: launchd-codex-tcc-isolation
description: Isolate Codex app-server startup stalls that occur only in a macOS launchd process after initialize succeeds.
when_to_use: Use when direct Codex app-server probes work but a launchd-managed Detent worker times out at thread/start or thread/resume.
---

# Launchd Codex TCC Isolation

- Keep the live Detent job untouched. Use a unique temporary launchd label, port `0`, isolated config/database/workspace/Codex home, and a trap that unloads only the temporary job.
- Reproduce with a bounded JSON-RPC probe that sends `initialize`, `initialized`, and `thread/start` with an absolute workspace path. Compare direct and launchd runs from both `/` and a checkout before attributing the failure to cwd.
- Match launchd's visible environment in a direct process. If the direct probe still succeeds, distinguish visible environment from macOS responsible-process and TCC context.
- Bisect Codex home state from a fresh authenticated home. Restore config, plugins, hooks, and skills independently; then isolate individual skill entries when the skills root reproduces the stall.
- For a suspected symlink into `~/Desktop`, `~/Documents`, `~/Downloads`, `~/Library/CloudStorage`, or `~/Library/Mobile Documents`, run one minimal read through the temporary launch agent. `Operation not permitted` confirms that launchd lacks the terminal process's Files & Folders access.
- Do not mutate or relocate operator-owned skills as part of diagnosis. A service mitigation should preserve auth, config, plugins, state, worker temp directories, process-group ownership, and startup diagnostics while excluding only inaccessible host-skill links with an explicit warning.
- Verify the final binary through an isolated launchd-started Detent service. Require a non-empty provider thread ID, non-empty turn ID, and token updates, then unload the temporary job and confirm no probe process remains.
