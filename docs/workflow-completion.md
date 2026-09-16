# Evidence-backed workflow decisions

Evidence-backed completion of already-merged work needs no prior authorization or human reply. Fetch the tracked branch, name the merging PR and merge commit, run `git merge-base --is-ancestor <merge-commit> <tracked-head>` successfully, and verify every acceptance criterion. State the completion decision plainly in this Workpad. Report `status: complete` with the current attempt identity and these `fields`:

- `completion_kind: operational`
- `completion_merged_pr`: merging PR URL
- `completion_merge_commit`: full merge commit SHA
- `completion_branch`: tracked branch ref (for example `origin/main`)
- `completion_branch_head`: full verified branch-head SHA
- `completion_ancestry: verified`: only after the ancestry command exits 0
- `completion_evidence`: acceptance criteria, commands, results, and ancestry command/exit status

The orchestrator records this evidence and closes the issue; workers never close it or write lane state. If evidence cannot be produced, finish independent investigation and ask a focused human question; do not claim completion. Other no-PR operational work still requires pre-dispatch issue-body `detent-completion` authorization (`schema: 1`, `completion_kind: operational`) and concrete `completion_evidence`. Otherwise the PR gate applies.

When evidence proves a defect belongs to another repository, route it without asking for permission. Use `file_machine_issue` with `repository: owner/repo`, a stable fingerprint, and a body carrying source locations, reproduction/ownership evidence, and why this repository cannot implement or pin the fix. Reuse an open matching fingerprint. The tool stamps the origin and links the upstream issue on the original issue; also state the routing decision and link in this Workpad. Workspace isolation limits edits, not upstream filing. Do not change upstream board state. If this issue still depends on the upstream fix, register the real dependency and report it through the existing blocker contract. Ask only if the target repository cannot be determined; missing credentials or write-policy failures are instance errors.
