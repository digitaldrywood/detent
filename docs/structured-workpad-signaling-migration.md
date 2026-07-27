# Structured Workpad Signaling Migration

Detent now treats blockers as structured data. New projects should use GitHub's
native `blocked_by` issue dependency relation first, then the Workpad
`detent-status` block. Narrative Workpad sentences are notes only and are never
read as blocker declarations when a valid structured block is present.

During the deprecation window, Detent still parses legacy issue-body
`Depends on:` and `Blocked by:` lines as fallback metadata. Projects that rely
only on prose Workpad blockers continue to run, but doctor reports them as
prose-only so owners can migrate the workflow prompt.

Apply this prompt-body diff to existing `WORKFLOW.md` files:

````diff
-Follow repository instructions, keep changes scoped to the issue, and keep a
-single persistent `## Codex Workpad` issue comment updated with the plan,
-validation evidence, blockers, and final handoff.
+Follow repository instructions, keep changes scoped to the issue, and keep a
+single persistent `## Codex Workpad` issue comment updated with the plan,
+validation evidence, and final handoff. Every Workpad update must include one
+`detent-status` fenced block. Detent reads blocker and human-action
+declarations from that block; narrative sentences are never read as blockers.
+
+```detent-status
+schema: 1
+status: in_progress
+blockers: []
+human_action: null
+```
+
+For dependency blockers, use this order:
+
+1. Create GitHub's native `blocked_by` dependency relation.
+
+```sh
+BLOCKED_NUMBER=<blocked-issue-number>
+BLOCKER_NUMBER=<blocker-issue-number>
+BLOCKER_ID="$(gh api repos/{owner}/{repo}/issues/$BLOCKER_NUMBER --jq '.id')"
+gh api --method POST "repos/{owner}/{repo}/issues/$BLOCKED_NUMBER/dependencies/blocked_by" -F issue_id="$BLOCKER_ID"
+```
+
+2. Declare the blocker in the Workpad status block.
+
+```detent-status
+schema: 1
+status: blocked
+blockers:
+  - ref: "owner/repo#123"
+    reason: "waiting for the dependency to merge"
+human_action: null
+```
+
+3. Legacy fallback during the deprecation window: if native dependencies are
+   unavailable and the project has not migrated, keep a machine-readable
+   issue-body line such as `Blocked by: #123` or `Depends on: owner/repo#123`.
+
+When `tracker.blocked_recovery` is enabled and the workflow intentionally
+parks recoverable PR maintenance in a configured source lane, use a structured
+reason code instead of prose:
+
+```detent-status
+schema: 1
+status: blocked
+reason_code: merge_conflict
+blockers: []
+human_action: null
+```
````

Also update the state-specific instructions:

````diff
-2. Create or update the persistent `## Codex Workpad` comment with the plan,
-   acceptance criteria, validation plan, and blockers.
+2. Create or update the persistent `## Codex Workpad` comment with the plan,
+   acceptance criteria, validation plan, and the `in_progress`
+   `detent-status` block shown above.

-1. Re-read the issue, pull request, comments, and `## Codex Workpad`.
+1. Re-read the issue, pull request, comments, and `## Codex Workpad`, including
+   the `detent-status` block.

-3. If implementation is complete, run the full pre-review gate and move the
-   issue to `Human Review` only when the gate passes.
+3. If implementation is complete, run the full pre-review gate, update the
+   Workpad block to `status: complete` with `blockers: []` and
+   `human_action: null`, and move the issue to `Human Review` only when the
+   gate passes.

-   unavailable, keep the issue in `Merging` and record the missing ship workflow
-   as an external blocker in the `## Codex Workpad`.
+   unavailable, keep the issue in `Merging` and record the missing ship workflow
+   as `human_action` in the `detent-status` block.
````

For `tracker.kind: github_local`, keep GitHub issues read-only unless a human
explicitly authorizes upstream metadata writes. The local Workpad still needs
the same `detent-status` block.
