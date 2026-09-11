# Operator-approved invariants

These rules were approved by Cory in #2417. Identifiers are permanent. Changes
that weaken a rule, enforcing test, or enforcement require Cory's explicit
approval. Agents may propose changes but cannot authorize their own exceptions.
An ordinary compliant implementation does not require an additional review.

`make check-invariants` executes the named behavioral tests in `policy.json` and
requires evidence that every named test actually passed. A deleted, renamed,
skipped, or failed test fails the gate, including skipped subtests. `make check`
includes this gate. This is a selected regression suite, not proof of all possible
agent behavior. See [enforcement.md](enforcement.md) for the activation gaps.

## INV-001 — Protect the rules

**Rule:** Weakening or removing an invariant, its tests, or its enforcement
requires Cory's explicit approval. Agents cannot approve exceptions.

**Rationale:** A check that can authorize its own deletion provides no protection.

**Allowed:** Propose a researched policy correction; merge ordinary code that
satisfies the existing checks automatically.

**Forbidden:** Remove a failing invariant or let an agent-authored review grant
an exception.

**Enforcement:** `tools/invariantcheck` protection mode compares base and proposed
trees against protected paths. `TestProtectedChanges` exercises policy, workflow,
ownership, threshold, and gate changes. `invariant-policy.yml` runs the checker
and policy from the trusted base. Protection is deliberately conservative for
the listed files; it cannot classify semantic strengthening versus weakening.
Human exception authentication and server-side activation remain unresolved.

## INV-002 — Keep questions with the work

**Rule:** Clarification and approval questions, answers, and progress stay on the
original issue; waiting state belongs in persisted Detent records. Never create
synthetic question issues, dependencies, or YAML completion contracts.

**Rationale:** Operators need one coherent history, and answers must not become
unrelated work items.

**Allowed:** Comment on the assigned issue; create an independently actionable bug
or feature issue.

**Forbidden:** Create a placeholder approval issue and require its closure to
resume an existing PR.

**Enforcement:** `TestHumanQuestionRestartAndConcurrentRequests` exercises durable
same-issue questions, concurrent calls, and lost-response reconciliation.
`TestHumanQuestionReplyAuthorization` checks reply eligibility. These tests from
independently merged #2416 are registered in the invariant gate. Natural-language
answer interpretation remains a judgment boundary.

## INV-003 — Continue independent work

**Rule:** Complete work independent of a pending answer; pause only dependent
work, retain accepted changes and PR/rework state, and resume without restarting.

**Rationale:** Questions should not discard completed work or idle useful work.

**Allowed:** Validate an implemented change while waiting for a product preference.

**Forbidden:** Restart implementation or discard a retained workspace on recovery.

**Enforcement:** `TestReconcileRunningIssuesRetainsWorkerOutsideActiveLane` and
`TestServiceRestartsRecoverRetainedWorkAndDispatch` exercise retained work.
`TestHumanQuestionIndependentRework` exercises continued PR rework without
approving the pending decision. General judgments of independence require review.

## INV-004 — Own routine technical decisions

**Rule:** Resolve routine technical choices; escalate product preferences,
missing access, and authorization needs as specific questions with a researched
recommendation when possible.

**Rationale:** The operator should make product decisions, not perform delegated
technical investigation.

**Allowed:** Select a repository-standard implementation; ask a concrete question
after researching an access limitation.

**Forbidden:** Assign vague technical design work to Cory.

**Enforcement boundary:** Human review of issue comments and work products.
Deterministic checks cannot determine whether an arbitrary natural-language
question is a routine technical choice. No LLM verdict grants a policy exception.

## INV-005 — Do not weaken validation to pass

**Rule:** Do not lower thresholds, skip checks, suppress failures, or weaken tests
without explicit approval. Correct an incorrect expectation with an explanation
and continued coverage of the intended behavior.

**Rationale:** Green results must retain their meaning.

**Allowed:** Explain and correct a wrong expectation while testing the real outcome.

**Forbidden:** Skip a failing restart subtest or reduce the coverage floor.

**Enforcement:** `TestBehaviorEvidence` rejects successful no-test runs, deleted
tests, and skipped subtests. The protected paths cover the registered enforcing
test files, tooling, workflows, coverage configuration, and Makefile. Existing
70% aggregate and exact-file safety floors remain unchanged. Semantic weakening
elsewhere in an arbitrary test or implementation requires review; the gate does
not claim to decide that from source text.

## INV-006 — Verify the released commit and running artifact

**Rule:** Require successful mandatory evidence for the exact release commit,
connect the artifact to it, and verify the updated instance's version and commit.

**Rationale:** A green earlier PR head does not validate a squash merge or binary.

**Allowed:** Release the commit with all mandatory successful checks and compare
the running instance to that commit.

**Forbidden:** Accept missing, cancelled, skipped, or stale evidence, or infer a
running binary's identity from a tag alone.

**Enforcement:** `TestReleaseEvidence`, `TestRunningEvidence`, and
`TestMandatoryEvidence`. The release workflow checks every name in
the mandatory manifest on its exact SHA and fails without success. GoReleaser
embeds the full commit. Running mode compares `/api/v1/state` evidence with the
expected version and full commit. Existing signature and rollback tests exercise
artifact integrity. Automatic updater integration of commit evidence and
post-restart verification remains a gap; a manual invocation alone is not a
mandatory deployment gate. See the deployment-path audit in enforcement.md.

## INV-007 — Bound repeated failure

**Rule:** Retry repeated failure only with new evidence or a concrete correction;
retain diagnostics and report the blocker on the original issue.

**Rationale:** Retries must not cause endless loops, repeated comments, or spend.

**Allowed:** Resume after a corrected defect or newly available evidence.

**Forbidden:** Reset a retry counter by restarting the service.

**Enforcement:** `TestConfiguredTerminalRetryAfterStoreRestart`,
`TestConsecutiveRetryCycleCountAcrossServiceRestarts`,
`TestStartupRecoveryPersistsCrashLoopAcrossProcesses`, and
`TestStartupRecoveryAttemptsOneUpdatePerVersion`. Existing configured limits and
startup recovery's existing threshold are preserved. Assessing the usefulness
of novel evidence remains a review boundary. Release failure reporting currently
creates separate failure issues; reconciliation with the same-issue rule remains
a documented gap, not an approved exception.

## INV-008 — Preserve work and avoid duplicate actions

**Rule:** Preserve accepted work across restart/retry/concurrency; reconcile
uncertain external outcomes before repeating actions.

**Rationale:** Local durable state does not by itself provide exactly-once external
delivery.

**Allowed:** Recover retained work and reconcile an external action receipt.

**Forbidden:** Claim exactly-once delivery for an API without that guarantee.

**Enforcement:** Registered retained-work, retry-restart, one-update-per-version,
and rollback tests cover their named operations. `TestHumanQuestionRestartAndConcurrentRequests` covers same-issue comment
concurrency and uncertain delivery. These guards do not prove universal
deduplication of arbitrary agent messages, payments, or other external tools.

## INV-009 — Respect approval scope and persistence

**Rule:** Preserve an approval within its scope unless the proposed action changes
materially. Do not invent approval gates or extend approval to unrelated actions.

**Rationale:** Repeated approval requests waste attention; unrelated actions need
their own authority.

**Allowed:** Retain the approved attempt policy over restart.

**Forbidden:** Use an answer to a product question as permission to deploy.

**Enforcement:** `TestAttemptMetadataRetainsApprovedPolicy` and
`TestPolicyMismatchStopsDispatchWithoutAttempt` cover configured policy scope.
`TestHumanQuestionReplyAuthorization` covers reply eligibility, while
`TestHumanQuestionIndependentRework` ensures rework does not approve a question.
Natural-language interpretation
and the shared GitHub identity remain explicit boundaries.

## INV-010 — Control scope

**Rule:** Do not expand work into an unsolicited redesign or new operator workflow.
Necessary implementation decisions and useful independent follow-ups are allowed.

**Rationale:** Agents implement the approved outcome; Cory controls product operation.

**Allowed:** Choose internal implementation details and file an independent defect.

**Forbidden:** Require a new operator YAML-editing ritual to complete ordinary work.

**Enforcement boundary:** Human assessment of issue and PR scope against the
approved request. The gate protects the rule's definition but cannot prove all
natural-language scope decisions. LLM review may assist but cannot authorize
weakening. The manifest is maintained as repository code, not operator input.
