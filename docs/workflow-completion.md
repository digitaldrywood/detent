# Evidence-backed workflow decisions

Evidence-backed completion of already-merged work needs no prior authorization or human reply. Fetch the tracked branch, name the merging PR and merge commit, run `git merge-base --is-ancestor <merge-commit> <tracked-head>` successfully, and verify every acceptance criterion. State the completion decision plainly in this Workpad. Report `status: complete` with the current attempt identity and these `fields`:

- `completion_kind: operational`
- `completion_merged_pr`: merging PR URL
- `completion_merge_commit`: full merge commit SHA
- `completion_branch`: tracked branch ref (for example `origin/main`)
- `completion_branch_head`: full verified branch-head SHA
- `completion_ancestry: verified`: only after the ancestry command exits 0
- `completion_evidence`: acceptance criteria, commands, results, and ancestry command/exit status

The orchestrator records this evidence and closes the issue; workers never close it or write lane state. If evidence cannot be produced, finish independent investigation and record the specific missing human input as a Workpad `human_action` with `status: blocked`; do not claim completion. Other no-PR operational work still requires pre-dispatch issue-body `detent-completion` authorization (`schema: 1`, `completion_kind: operational`) and concrete `completion_evidence`. Otherwise the PR gate applies.

## Incident and existing linked-PR completion

The reported incident was [pyroapex#2159](https://github.com/digitaldrywood/pyroapex/issues/2159).
Its Workpad identified merged PR #2141, merge commit
`1522faa0f27ff15e237dc4f144a772cec044035b`, ancestry in `origin/main`,
and 100 passing race repetitions. PR #2141 closes #2116; #2159 has no GitHub
closing-PR reference or PR cross-reference. Mentioning the fix in Workpad prose
does not make it a recognized PR. The worker kept `in_progress` and asked for
operational authorization, so it never submitted a complete merged-PR candidate.

This is the no-recognized-PR case. The existing recognized merged-PR path remains
unchanged: it requires a clean workspace, complete Workpad, and acceptable CI
evidence. Use that path when the issue already has a recognized merged PR; the
operational evidence exception does not bypass its requirements.
