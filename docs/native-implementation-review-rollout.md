# Native implementation review rollout

Native completion now produces an independent, read-only source review before
acceptance validation. Each pass uses a fresh session and records its own typed
evidence for the current immutable version. The source review uses the existing
implementation route; acceptance validation retains the validator route.

Required source review remains required when acceptance validation is disabled.
Neither record grants human approval. Source rework and wait retain the change
and stop acceptance validation; recording or provenance errors fail completion.

## Compatibility

This change affects the Hub, local runners, and both ends of SSH execution.
Existing JSON evidence storage needs no migration. A new runner requires the
Hub to accept `implementation_review`; an older Hub refuses that worker record.
The new Hub refuses landing when required source-review evidence is missing,
including for unlanded versions published by older runners. A new SSH worker
also requires its host to support `ReviewVersion` and `RecordReview` callbacks.

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
   Also verify negative source verdicts retain Rework rather than failed-worker
   accounting. Preserve human and prerequisite holds.

If any target cannot adopt the matching build, keep the affected rollout drained
and resolve compatibility before resuming. Do not roll back Hub enforcement or
change policy merely to make missing evidence appear accepted.

## Executable source proof

`TestNativeImplementationReviewPrecedesAcceptanceValidator` covers ordering,
validator-disabled review, rework, wait, and recording errors.
`TestNativeImplementationReviewIsRecordedSeparately` exercises the real Hub's
typed records, current-head fences, separate acceptance session, and verdicts.
`TestChangeValidatorSessionAuthority` checks implementing-session and human
approval boundaries. `TestNativeChangeRunCompletion` checks source-verdict routing
while retaining prerequisite and human-action authority.

These tests prove the source behavior. Fleet adoption and a genuine landing are
separate runtime acceptance and must be recorded after deployment.
