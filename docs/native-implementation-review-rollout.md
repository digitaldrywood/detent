# Native implementation review rollout

Native completion now produces an independent, read-only source review before
acceptance validation. Each pass uses a fresh session and records its own typed
evidence for the current immutable version. The source review uses the existing
implementation route; acceptance validation retains the validator route.

Required source review remains required when acceptance validation is disabled.
Neither record grants human approval. Source rework and wait retain the change
and stop acceptance validation. Actionable source rework returns to configured
Rework. A source-review wait on genuine complete source uses the existing
protected, operator-owned Blocked hold. It releases execution capacity without
scheduling another model review or implementation turn. The same owner preserves
complete source when its review producer fails, with instance attribution.
Human, permission and prerequisite holds retain their authority; publication and
provenance refusals do not qualify as completed source.

The operator observes the missing technical evidence, then explicitly resumes
through the supported workflow transition with its current revision. This move
does not approve the Change. The resumed execution restores the current validated
version in a fresh workspace, records a new execution session, and proceeds to
source review and acceptance without rebasing, committing or invoking a writable
turn. Version, head, base, repository, policy and retained source must still
match. Genuine source findings, partial work, failed landing checks or gates,
landing conflict recovery and protected review-conversation findings retain
their normal implementation paths. An acknowledged source finding retains Rework
even if the following summary refresh fails. A failed pass-summary refresh grants
no acceptance. Another evidence wait returns to the same
operator owner, with no automatic retry. If the workflow cannot provide a legal
nondispatching hold and an executable return, completion retains its refusal
owner instead of establishing an orphaned hold.

## Compatibility

This change affects the Hub, local runners, and both ends of SSH execution.
Existing JSON evidence storage needs no migration. A new runner requires the
Hub to accept `implementation_review`; an older Hub refuses that worker record.
The new Hub refuses landing when required source-review evidence is missing,
including for unlanded versions published by older runners. A new SSH worker
also requires its host to support `ReviewVersion` and `RecordReview` callbacks.

When source review is required and acceptance validation is enabled, acceptance
must be recorded after the latest passing source review in the evidence sequence.
Retained acceptance recorded before that source review does not satisfy the
gate; the unchanged version needs fresh acceptance after its source review.

Do not exempt older versions, equate validator evidence with source review, or
disable a required gate to accommodate an older runner.

## Adoption

1. Complete source and combined-tree landing validation and resolve independent
   and GitHub Codex review before publishing the candidate.
2. Inventory every runner served by the target Hub, including other projects
   and SSH hosts and workers. Use supported drains and preserve source and
   execution receipts. Wait for acknowledged drains and zero unsettled work;
   do not kill an active attempt to shorten adoption.
3. Obtain the operator's authorization for the actual target deployment. Deploy
   the compatible Hub first while the affected runners remain drained. Merging
   the source PR does not itself authorize a production release.
4. Adopt the matching reviewed build on all affected runners, upgrading each
   SSH host and worker together. Verify actual process build identities and
   fresh registrations before resuming dispatch.
5. Resume through the normal workflow. Retained changes without a current source
   review must obtain genuine current-head source-review and acceptance receipts
   under an authorized execution. Never manufacture approval or rewrite old
   receipts.
6. Verify a genuine required-review work item records two separate fresh sessions
   when validation is enabled, then reaches landing through its normal owner.
   Also verify actionable source rework returns to Rework, while complete source
   awaiting review evidence enters the protected operator hold and releases
   capacity without automatic review or coding. Observe the technical evidence
   and use the supported explicit resume; verify the restored version keeps its
   identity and reaches review with zero writable turns. A repeated wait retains
   the same owner. Preserve human, permission and prerequisite holds and instance
   failure attribution.

If any target cannot adopt the matching build, keep the affected rollout drained
and resolve compatibility before resuming. Do not roll back Hub enforcement or
change policy merely to make missing evidence appear accepted.
