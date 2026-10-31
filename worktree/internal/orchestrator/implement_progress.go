package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const (
	implementProgressMetadataKey       = "completion_progress"
	implementProgressOutcomeNoProgress = "no_progress"
	implementProgressReasonNonDiff     = "verifiable_non_diff_progress"
	implementOperationalCompletion     = "operational_completion"
	implementDependencyDeferralReason  = "dependency_deferral"
	implementMergedCompletionReason    = "merged_pull_request_completion"
	strandedUnpushedWorkReason         = "stranded_unpushed_work"
	workpadBlockedUnactionedReason     = "workpad_blocked_unactioned"
	workspaceHeadUnavailableReason     = "workspace_head_unavailable_for_unpushed_check"
	pullRequestHeadUnavailableReason   = "pull_request_head_unavailable_for_unpushed_check"
	unpushedRemoteTruthUnavailable     = "unpushed_remote_truth_unavailable_without_pull_request"
)

type implementCompletionProgressDecision struct {
	Issue                  connector.Issue
	Outcome                store.WorkAttemptTerminalState
	Reason                 string
	CurrentSignature       autoPromoteReworkSignature
	PreviousSignature      autoPromoteReworkSignature
	PreviousSignatureFound bool
	FailedChecksAdded      []string
	FailedChecksRemoved    []string
	WorkspaceDiffStats     DiffStats
	WorkpadStatus          string
	SecurityAudit          securityaudit.Evaluation
	HumanAction            string
	TrackerState           string
	ConsecutiveHumanAction int
	BlockReason            string
	Block                  bool
	Warning                string
	DependencyDeferral     bool
	DependencyBlockers     []implementDependencyBlocker
	RejectedBlockerRefs    []string
	ProgressKinds          []string
	CompletionKind         string
}

type implementProgressRecord struct {
	Outcome                string                            `json:"outcome"`
	Reason                 string                            `json:"reason"`
	CurrentSignature       implementProgressSignatureRecord  `json:"current_signature"`
	PreviousSignature      *implementProgressSignatureRecord `json:"previous_signature,omitempty"`
	PreviousHeadSHA        string                            `json:"previous_head_sha,omitempty"`
	CurrentHeadSHA         string                            `json:"current_head_sha,omitempty"`
	FailedChecksAdded      []string                          `json:"failed_checks_added,omitempty"`
	FailedChecksRemoved    []string                          `json:"failed_checks_removed,omitempty"`
	WorkspaceDiffStats     implementProgressDiffStats        `json:"workspace_diffstat"`
	WorkpadStatus          string                            `json:"workpad_status,omitempty"`
	HumanAction            string                            `json:"human_action,omitempty"`
	TrackerState           string                            `json:"tracker_state,omitempty"`
	ConsecutiveHumanAction int                               `json:"consecutive_human_action,omitempty"`
	BlockReason            string                            `json:"block_reason,omitempty"`
	Warning                string                            `json:"warning,omitempty"`
	DependencyDeferral     bool                              `json:"dependency_deferral,omitempty"`
	DependencyBlockers     []implementDependencyBlocker      `json:"dependency_blockers,omitempty"`
	RejectedBlockerRefs    []string                          `json:"rejected_blocker_refs,omitempty"`
	ProgressKinds          []string                          `json:"progress_kinds,omitempty"`
	CompletionKind         string                            `json:"completion_kind,omitempty"`
	CompletionSourceStart  completionSourceStartRecord       `json:"-"`
}

type implementProgressArtifactSnapshot struct {
	PullRequestDiffFingerprint string
	TrackerState               string
	NativeBlockers             []string
	WorkpadRead                bool
	WorkpadStatus              string
	WorkpadReason              string
	WorkpadFields              map[string]string
	WorkpadReceiptHash         string
	CompletionKind             string
}

type implementDependencyBlocker struct {
	ID         string `json:"id,omitempty"`
	Identifier string `json:"identifier"`
	State      string `json:"state,omitempty"`
}

type implementProgressSignatureRecord struct {
	PRNumber     int64    `json:"pr_number,omitempty"`
	HeadSHA      string   `json:"head_sha,omitempty"`
	FailedChecks []string `json:"failed_checks,omitempty"`
}

type implementProgressDiffStats struct {
	FilesChanged                   int      `json:"files_changed"`
	AddedLines                     int      `json:"added_lines"`
	RemovedLines                   int      `json:"removed_lines"`
	UnpushedCommits                int      `json:"unpushed_commits,omitempty"`
	UnpushedCommitRefs             []string `json:"unpushed_commit_refs,omitempty"`
	TrackedPaths                   []string `json:"tracked_paths,omitempty"`
	UntrackedPaths                 []string `json:"untracked_paths,omitempty"`
	CommitsNotInPullRequest        []string `json:"commits_not_in_pull_request,omitempty"`
	PullRequestComparisonAvailable bool     `json:"pull_request_comparison_available,omitempty"`
	RecoveryStateExpected          bool     `json:"recovery_state_expected,omitempty"`
	RecoveryStateAvailable         bool     `json:"recovery_state_available,omitempty"`
	HeadSHA                        string   `json:"head_sha,omitempty"`
	TreeSHA                        string   `json:"tree_sha,omitempty"`
	Fingerprint                    string   `json:"fingerprint,omitempty"`
	Status                         string   `json:"status,omitempty"`
}

func (o *Orchestrator) evaluateImplementCompletionCandidate(
	ctx context.Context,
	running Running,
	finalState string,
	_ bool,
) implementCompletionProgressDecision {
	decision := implementCompletionProgressDecision{
		Issue:              cloneIssue(running.Issue),
		Outcome:            store.WorkAttemptTerminalSuccess,
		Reason:             "success_without_progress_check",
		WorkspaceDiffStats: running.DiffStats,
	}
	if terminalStateForRun(nil, finalState) != store.WorkAttemptTerminalSuccess {
		decision.Reason = "final_state_not_completed"
		return decision
	}
	if !implementProgressLinkedPullRequest(running.Issue) {
		issue, workpadCurrent := o.refreshImplementCompletionIssue(ctx, running.Issue)
		decision.Issue = issue
		decision.TrackerState = strings.TrimSpace(issue.State)
		decision.ProgressKinds = implementProgressSourceKinds(running, issue)
		if workpadCurrent {
			decision.WorkpadStatus, decision.HumanAction = implementProgressBlockedHumanAction(issue)
			blockers, rejected, deferred := o.evaluateImplementDependencyDeferral(ctx, issue)
			decision.DependencyBlockers = blockers
			decision.RejectedBlockerRefs = rejected
			if deferred {
				decision.Reason = implementDependencyDeferralReason
				decision.DependencyDeferral = true
				decision.WorkpadStatus = workpad.StatusBlocked
				return decision
			}
		}
		operationalCompletionCandidate := false
		if workpadCurrent {
			_, operationalCompletionCandidate = operationalCompletionWithAuthorization(issue, running.DispatchProgress.CompletionKind == workpad.CompletionOperational)
			if operationalCompletionCandidate && implementProgressOperationalWorkspaceClean(running.DiffStats) {
				decision.WorkpadStatus = workpad.StatusComplete
				decision.Reason = implementOperationalCompletion
				decision.ProgressKinds = []string{"operational_completion"}
				decision.CompletionKind = workpad.CompletionOperational
				return decision
			}
		}
		if workpadCurrent && implementProgressMergedCompletionCandidate(issue, running.DiffStats) &&
			implementProgressLinkedPullRequest(issue) && issue.PullRequest == nil {
			hydrator, ok := o.connector.(connector.PullRequestHydrator)
			if !ok {
				decision.Warning = "pull request hydrator unavailable"
				o.warnImplementProgressHydration(issue, decision.Warning, nil)
			} else {
				hydrated, err := hydrator.HydratePullRequest(ctx, issue)
				if err != nil {
					decision.Warning = err.Error()
					o.warnImplementProgressHydration(issue, "pull request hydration failed", err)
				} else {
					issue = hydrated
					decision.Issue = issue
				}
			}
		}
		if workpadCurrent && implementProgressMergedCompletion(issue, running.DiffStats) {
			decision.WorkpadStatus = workpad.StatusComplete
			decision.CurrentSignature = autoPromoteReworkSignatureFromIssue(issue, staleMergedPullRequestSummaryFromIssue(issue))
			decision.Reason = implementMergedCompletionReason
			return decision
		}
		attempts, err := o.recentImplementCompletionAttempts(ctx, issue, running)
		if err != nil {
			decision.Warning = err.Error()
			if o.logger != nil {
				o.logger.Warn(
					"implement worker progress history lookup failed",
					"issue_id", running.Issue.ID,
					"identifier", running.Issue.Identifier,
					"error", err,
				)
			}
		}
		if decision.HumanAction != "" {
			decision.ConsecutiveHumanAction = 1 + consecutiveImplementBlockedHumanActionAttempts(
				attempts,
				decision.HumanAction,
				decision.TrackerState,
			)
			// Recorded human blockers suppress dispatch immediately, so routing
			// cannot depend on a second completion.
			decision.Outcome = store.WorkAttemptTerminalNoProgress
			decision.Reason = workpadBlockedUnactionedReason
			decision.BlockReason = workpadBlockedUnactionedReason
			decision.Block = true
			return decision
		}
		if operationalCompletionCandidate && (running.DiffStats.UnpushedCommits > 0 || running.DiffStats.CommitsAhead > 0) {
			decision.Outcome = store.WorkAttemptTerminalNoProgress
			decision.Reason = strandedUnpushedWorkReason

			return decision
		}
		if _, native := o.connector.(connector.WorkflowStateReader); !native {
			if stranded, deferReason := implementProgressUnpushedClassification(running.DiffStats, nil); deferReason != "" {
				decision.Outcome = store.WorkAttemptTerminalNoProgress
				decision.Reason = deferReason
				return decision
			} else if stranded {
				decision.Outcome = store.WorkAttemptTerminalNoProgress
				decision.Reason = strandedUnpushedWorkReason

				return decision
			}
		}
		if workpadCurrent && o.implementCompletionAccepted(running, issue, decision.WorkspaceDiffStats) {
			return decision
		}
		if len(decision.ProgressKinds) > 0 {
			decision.Reason = "signature_changed"
			return decision
		}
		if !diffStatsPresent(running.DiffStats) {
			decision.Outcome = store.WorkAttemptTerminalNoProgress
			decision.Reason = "workspace_diffstat_unavailable_without_pull_request"
			return decision
		}
		if !implementProgressDiffStatsClean(running.DiffStats) {
			if strings.TrimSpace(running.DiffStats.Fingerprint) == "" {
				decision.Outcome = store.WorkAttemptTerminalNoProgress
				decision.Reason = "workspace_diff_fingerprint_unavailable_without_pull_request"
				return decision
			}
			decision.Outcome = store.WorkAttemptTerminalNoProgress
			decision.Reason = "unchanged_workspace_diff_without_pull_request"

			return decision
		}
		decision.Outcome = store.WorkAttemptTerminalNoProgress
		decision.Reason = "completed_clean_diff_without_pull_request"

		return decision
	}
	hydrator, ok := o.connector.(connector.PullRequestHydrator)
	if !ok {
		decision.Outcome = store.WorkAttemptTerminalNoProgress
		decision.Reason = "pull_request_hydrator_unavailable"
		decision.Warning = "pull request hydrator unavailable"
		o.warnImplementProgressHydration(running.Issue, decision.Warning, nil)
		return decision
	}
	issue, err := hydrator.HydratePullRequest(ctx, running.Issue)
	if err != nil {
		decision.Outcome = store.WorkAttemptTerminalNoProgress
		decision.Reason = "pull_request_hydration_failed"
		decision.Warning = err.Error()
		o.warnImplementProgressHydration(running.Issue, "pull request hydration failed", err)
		return decision
	}
	decision.Issue = issue
	if reason := implementProgressHydrationUnavailableReason(issue.PullRequest); reason != "" {
		decision.Outcome = store.WorkAttemptTerminalNoProgress
		decision.Reason = "pull_request_hydration_unavailable"
		decision.Warning = reason
		o.warnImplementProgressHydration(issue, reason, nil)
		return decision
	}
	issue, workpadCurrent := o.refreshImplementCompletionIssue(ctx, issue)
	decision.Issue = issue
	decision.TrackerState = strings.TrimSpace(issue.State)
	if workpadCurrent && pullRequestMerged(issue.PullRequest) && implementProgressMergedCompletion(issue, running.DiffStats) {
		decision.WorkpadStatus = workpad.StatusComplete
		decision.CurrentSignature = autoPromoteReworkSignatureFromIssue(issue, staleMergedPullRequestSummaryFromIssue(issue))
		decision.Reason = implementMergedCompletionReason
		return decision
	}
	decision.WorkspaceDiffStats = implementProgressReconcilePullRequestEvidence(running.DiffStats, issue.PullRequest)
	if workpadCurrent {
		decision.WorkpadStatus = implementProgressArtifactSnapshotFromIssue(issue, true).WorkpadStatus
		_, decision.HumanAction = implementProgressBlockedHumanAction(issue)
	}
	decision.SecurityAudit = o.securityAuditEvaluation(ctx, issue)
	if workpadCurrent && !pullRequestMerged(issue.PullRequest) {
		blockers, rejected, deferred := o.evaluateImplementDependencyDeferral(ctx, issue)
		decision.DependencyBlockers = blockers
		decision.RejectedBlockerRefs = rejected
		if deferred {
			decision.Reason = implementDependencyDeferralReason
			decision.DependencyDeferral = true
			decision.WorkpadStatus = workpad.StatusBlocked
			return decision
		}
	}
	decision.ProgressKinds = implementProgressSourceKinds(running, issue)
	signature := autoPromoteReworkSignatureFromIssue(issue, AutoPromoteSummaryFromIssue(issue))
	decision.CurrentSignature = signature
	if !implementProgressSignatureUsable(signature) {
		decision.Outcome = store.WorkAttemptTerminalNoProgress
		decision.Reason = "pull_request_signature_incomplete"
		return decision
	}

	attempts, err := o.recentImplementCompletionAttempts(ctx, issue, running)
	if err != nil {
		decision.Warning = err.Error()
		if o.logger != nil {
			o.logger.Warn(
				"implement worker progress history lookup failed",
				"issue_id", issue.ID,
				"identifier", issue.Identifier,
				"error", err,
			)
		}
	}
	previous, ok := latestImplementProgressSignature(attempts)
	if decision.WorkpadStatus == workpad.StatusBlocked {
		decision.Outcome = store.WorkAttemptTerminalNoProgress
		decision.Reason = "workpad_blocked"

		if decision.HumanAction != "" {
			decision.ConsecutiveHumanAction = 1 + consecutiveImplementBlockedHumanActionAttempts(attempts, decision.HumanAction, decision.TrackerState)
			decision.Block = true
			decision.Reason = workpadBlockedUnactionedReason
			decision.BlockReason = workpadBlockedUnactionedReason
		}
		return decision
	}
	if ok {
		decision.PreviousSignature = previous
		decision.PreviousSignatureFound = true
		decision.FailedChecksAdded, decision.FailedChecksRemoved = implementProgressFailedCheckDelta(previous.FailedChecks, signature.FailedChecks)
	}
	if stranded, deferReason := implementProgressUnpushedClassification(decision.WorkspaceDiffStats, issue.PullRequest); deferReason != "" {
		decision.Outcome = store.WorkAttemptTerminalNoProgress
		decision.Reason = deferReason
		return decision
	} else if stranded {
		decision.Outcome = store.WorkAttemptTerminalNoProgress
		decision.Reason = strandedUnpushedWorkReason

		return decision
	}
	if workpadCurrent && o.implementCompletionAccepted(running, issue, decision.WorkspaceDiffStats) {
		return decision
	}
	if !ok {
		if running.Attempt > 1 && len(decision.ProgressKinds) == 0 && decision.WorkpadStatus != workpad.StatusComplete {
			decision.Outcome = store.WorkAttemptTerminalNoProgress
			decision.Reason = "unchanged_signature_clean_diff"
			return decision
		}
		decision.Reason = "first_completed_attempt"
		return decision
	}
	if len(decision.ProgressKinds) > 0 {
		decision.Reason = "signature_changed"
		return decision
	}
	if !implementProgressDiffStatsClean(decision.WorkspaceDiffStats) {
		if !diffStatsPresent(decision.WorkspaceDiffStats) {
			decision.Outcome = store.WorkAttemptTerminalNoProgress
			decision.Reason = "workspace_diffstat_unavailable"
			return decision
		}
		if strings.TrimSpace(decision.WorkspaceDiffStats.Fingerprint) == "" {
			decision.Outcome = store.WorkAttemptTerminalNoProgress
			decision.Reason = "workspace_diff_fingerprint_unavailable"
			return decision
		}
		decision.Outcome = store.WorkAttemptTerminalNoProgress
		decision.Reason = "unchanged_workspace_diff"
		return decision
	}

	decision.Outcome = store.WorkAttemptTerminalNoProgress
	decision.Reason = "unchanged_signature_clean_diff"

	return decision
}

func (o *Orchestrator) implementCompletionAccepted(running Running, issue connector.Issue, diffStats DiffStats) bool {
	if !implementProgressDiffStatsClean(diffStats) {
		return false
	}
	signal, _ := autoPromoteIssueWorkpadSignal(issue)
	if signal != nil && strings.TrimSpace(signal.Fields[workpad.FieldCompletionKind]) != "" {
		return false
	}
	if workpad.CurrentAttemptCompletion(signal, running.WorkAttemptID, running.Generation) {
		return true
	}
	cfg := gate.Effective(o.cfg.AutoPromote.Gate)
	if cfg.Kind != gate.KindArtifact || signal == nil || signal.Invalid != nil || signal.Source != workpad.SourceStructured || signal.Status != workpad.StatusComplete || signal.HumanAction != "" || len(signal.Blockers) != 0 || !running.DispatchWorkpadRead {
		return false
	}
	hash := artifactGateWorkpadStatusHash(issue.Comments)
	if hash == "" || hash == running.DispatchWorkpadHash {
		return false
	}
	field, value, err := artifactGateCompletionFieldUpdate(signal.Fields, cfg.Artifact)
	if err != nil {
		return false
	}
	status, present := artifactStatusFieldFromIssue(issue, field)
	return present && strings.EqualFold(strings.TrimSpace(status), value)
}

func (o *Orchestrator) implementProgressDispatchArtifactSnapshot(ctx context.Context, issue connector.Issue) implementProgressArtifactSnapshot {
	snapshot := implementProgressArtifactSnapshotFromIssue(issue, false)
	if o == nil || o.connector == nil {
		return snapshot
	}
	reader, ok := o.connector.(connector.IssueCommentReader)
	if !ok {
		return snapshot
	}
	comments, err := reader.FetchIssueComments(ctx, issue)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("implement progress dispatch artifact snapshot failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
		return snapshot
	}
	issue.Comments = comments
	return implementProgressArtifactSnapshotFromIssue(issue, true)
}

func implementProgressArtifactSnapshotFromIssue(issue connector.Issue, workpadRead bool) implementProgressArtifactSnapshot {
	snapshot := implementProgressArtifactSnapshot{
		TrackerState:   normalizeState(issue.State),
		NativeBlockers: implementProgressBlockedRefIdentifiers(issue.BlockedBy),
		WorkpadRead:    workpadRead,
	}
	if kind, found, err := workpad.CompletionAuthorizationFromIssueBody(issue.Description); err == nil && found {
		snapshot.CompletionKind = kind
	}
	if !workpadRead {
		return snapshot
	}
	signal, ok := autoPromoteIssueWorkpadSignal(issue)
	if !ok || signal == nil || signal.Invalid != nil || signal.Source != workpad.SourceStructured {
		return snapshot
	}
	snapshot.WorkpadStatus = strings.TrimSpace(signal.Status)
	snapshot.WorkpadReason = strings.TrimSpace(signal.ReasonCode)
	snapshot.WorkpadFields = cloneStringMap(signal.Fields)
	if snapshot.WorkpadStatus == workpad.StatusComplete {
		snapshot.WorkpadReceiptHash = artifactCompletionReceiptHash(issue.Comments)
	}
	return snapshot
}

func implementProgressSourceKinds(running Running, issue connector.Issue) []string {
	baseline := running.CompletionSourceStart
	before := strings.TrimSpace(baseline.Fingerprint.PRHeadSHA)
	linked := baseline.Fingerprint.PRNumber > 0
	if !baseline.LaneAvailable && !baseline.Captured {
		linked = implementProgressLinkedPullRequest(running.Issue)
		if running.Issue.PullRequest != nil {
			before = strings.TrimSpace(running.Issue.PullRequest.HeadSHA)
		}
	}
	if pr := issue.PullRequest; pr != nil && !pullRequestHydrationBlocksProgress(pr) {
		after := strings.TrimSpace(pr.HeadSHA)
		if after != "" && after != before && (before != "" || !linked) {
			return []string{"pull_request"}
		}
	}
	beforeTree := strings.TrimSpace(baseline.Fingerprint.WorkspaceTree)
	afterTree := strings.TrimSpace(running.DiffStats.TreeSHA)
	if beforeTree != "" && afterTree != "" && beforeTree != afterTree {
		return []string{"committed_tree"}
	}
	return nil
}

func implementProgressBlockedRefIdentifiers(refs []connector.BlockedRef) []string {
	identifiers := make([]string, 0, len(refs))
	for _, ref := range refs {
		identifier := strings.TrimSpace(ref.Identifier)
		if identifier == "" {
			identifier = strings.TrimSpace(ref.ID)
		}
		if identifier != "" {
			identifiers = append(identifiers, identifier)
		}
	}
	slices.Sort(identifiers)
	return slices.Compact(identifiers)
}

func (o *Orchestrator) recentImplementCompletionAttempts(
	ctx context.Context,
	issue connector.Issue,
	running Running,
) ([]store.WorkAttempt, error) {
	if o == nil || o.workAttempts == nil {
		return nil, nil
	}
	limit := 20
	attempts, err := o.workAttempts.ListRecentTerminalWorkAttempts(ctx, store.WorkAttemptHistoryQuery{
		ProjectID:  strings.TrimSpace(o.cfg.Project.ID),
		IssueID:    strings.TrimSpace(issue.ID),
		Identifier: strings.TrimSpace(issue.Identifier),
		IssueURL:   strings.TrimSpace(issue.URL),
		WorkerType: workAttemptWorkerType(issue, running.Mode),
		Limit:      limit,
	})
	return attempts, err
}

func (o *Orchestrator) refreshImplementCompletionIssue(ctx context.Context, issue connector.Issue) (connector.Issue, bool) {
	if o == nil || o.connector == nil || strings.TrimSpace(issue.ID) == "" {
		return cloneIssue(issue), false
	}
	issues, err := o.connector.FetchIssueStatesByIDs(ctx, []string{issue.ID})
	if err != nil {
		o.warnImplementProgressRefresh(issue, "fetch tracker state failed", err)
		return cloneIssue(issue), false
	}
	refreshed := connector.Issue{}
	hydratedPullRequest := issue.PullRequest
	hydratedPRNumber := issue.PRNumber
	hydratedPRRepository := issue.PRRepository
	for _, candidate := range issues {
		if strings.TrimSpace(candidate.ID) == strings.TrimSpace(issue.ID) {
			refreshed = mergeIssueTrackerFields(issue, candidate)
			refreshed.Description = candidate.Description
			refreshed.Comments = cloneIssueComments(candidate.Comments)
			refreshed.CommentCount = candidate.CommentCount
			refreshed.CommentsComplete = candidate.CommentsComplete
			break
		}
	}
	if strings.TrimSpace(refreshed.ID) == "" {
		o.warnImplementProgressRefresh(issue, "tracker issue was not returned", nil)
		return cloneIssue(issue), false
	}
	if hydratedPullRequest != nil {
		refreshed.PullRequest = hydratedPullRequest
		refreshed.PRNumber = hydratedPRNumber
		refreshed.PRRepository = hydratedPRRepository
	}
	if !refreshed.CommentsComplete {
		reader, ok := o.connector.(connector.IssueCommentReader)
		if !ok {
			o.warnImplementProgressRefresh(refreshed, "issue comment reader unavailable", nil)
			return refreshed, false
		}
		comments, err := reader.FetchIssueComments(ctx, refreshed)
		if err != nil {
			o.warnImplementProgressRefresh(refreshed, "fetch workpad comments failed", err)
			return refreshed, false
		}
		refreshed.Comments = comments
	}
	refreshed.WorkpadSignal = nil
	if signal, ok := autoPromoteIssueWorkpadSignal(refreshed); ok {
		refreshed.WorkpadSignal = signal
	}
	if resolved, ok := o.resolveMergedCompletionPullRequest(ctx, refreshed); ok {
		refreshed = resolved
	}
	return refreshed, true
}

func implementProgressBlockedHumanAction(issue connector.Issue) (string, string) {
	signal, ok := autoPromoteIssueWorkpadSignal(issue)
	if !ok || signal == nil || signal.Invalid != nil || signal.Source != workpad.SourceStructured {
		return "", ""
	}
	if strings.TrimSpace(signal.Status) != workpad.StatusBlocked {
		return "", ""
	}
	humanAction := strings.TrimSpace(signal.HumanAction)
	if humanAction == "" {
		for _, blocker := range signal.Blockers {
			if blocker.Owner == workpad.BlockerOwnerHuman {
				humanAction = firstNonBlank(blocker.Reason, blocker.Ref, blocker.Identifier)
				if humanAction != "" {
					break
				}
			}
		}
	}
	if humanAction == "" {
		return "", ""
	}
	return workpad.StatusBlocked, humanAction
}

func implementProgressMergedCompletion(issue connector.Issue, diffStats DiffStats) bool {
	if !implementProgressMergedCompletionCandidate(issue, diffStats) {
		return false
	}
	pullRequest := issue.PullRequest
	if workAttemptPRNumber(issue) == nil || !pullRequestMerged(pullRequest) ||
		pullRequestHydrationBlocksProgress(pullRequest) || strings.TrimSpace(pullRequest.HeadSHA) == "" {
		return false
	}
	decision := staleMergedPullRequestDecision(issue, staleMergedPullRequestSummaryFromIssue(issue))
	return decision.Reason == AutoPromoteReasonPullRequestMerged
}

func implementProgressMergedCompletionCandidate(issue connector.Issue, diffStats DiffStats) bool {
	if diffStats.UnpushedCommits > 0 || !implementProgressDiffStatsClean(diffStats) {
		return false
	}
	signal, ok := autoPromoteIssueWorkpadSignal(issue)
	if !ok || signal == nil || signal.Invalid != nil || signal.Source != workpad.SourceStructured ||
		(strings.TrimSpace(signal.Status) != workpad.StatusComplete && !completionForgeSupersedesWorkpad(issue.PullRequest, signal)) ||
		strings.TrimSpace(signal.HumanAction) != "" || len(signal.Blockers) > 0 {
		return false
	}
	return true
}

func latestImplementProgressSignature(attempts []store.WorkAttempt) (autoPromoteReworkSignature, bool) {
	for _, attempt := range attempts {
		record, ok := implementProgressRecordFromAttempt(attempt)
		if !ok {
			continue
		}
		signature := record.CurrentSignature.signature()
		if implementProgressSignatureUsable(signature) {
			return signature, true
		}
	}
	return autoPromoteReworkSignature{}, false
}

func consecutiveImplementBlockedHumanActionAttempts(attempts []store.WorkAttempt, humanAction string, trackerState string) int {
	humanAction = strings.TrimSpace(humanAction)
	trackerState = normalizeState(trackerState)
	if humanAction == "" || trackerState == "" {
		return 0
	}
	count := 0
	for _, attempt := range attempts {
		record, ok := implementProgressRecordFromAttempt(attempt)
		if !ok || strings.TrimSpace(record.WorkpadStatus) != workpad.StatusBlocked ||
			strings.TrimSpace(record.HumanAction) != humanAction ||
			normalizeState(record.TrackerState) != trackerState {
			return count
		}
		count++
	}
	return count
}

func implementProgressRecordMatchesNoProgress(record implementProgressRecord, current autoPromoteReworkSignature) bool {
	if !implementProgressSignatureEqual(record.CurrentSignature.signature(), current) {
		return false
	}
	if strings.TrimSpace(record.Outcome) == implementProgressOutcomeNoProgress {
		return true
	}
	return !implementProgressSignatureUsable(current) &&
		strings.TrimSpace(record.Outcome) == string(store.WorkAttemptTerminalSuccess) &&
		strings.TrimSpace(record.Reason) == "no_linked_pull_request" &&
		record.WorkspaceDiffStats.Status != "" &&
		record.WorkspaceDiffStats.FilesChanged == 0 &&
		record.WorkspaceDiffStats.AddedLines == 0 &&
		record.WorkspaceDiffStats.RemovedLines == 0
}

func implementProgressRecordFromAttempt(attempt store.WorkAttempt) (implementProgressRecord, bool) {
	if attempt.TerminalState != store.WorkAttemptTerminalSuccess &&
		attempt.TerminalState != store.WorkAttemptTerminalNoProgress {
		return implementProgressRecord{}, false
	}
	return implementProgressRecordFromAnyAttempt(attempt)
}

func implementProgressRecordFromAnyAttempt(attempt store.WorkAttempt) (implementProgressRecord, bool) {
	var root struct {
		CompletionProgress    implementProgressRecord     `json:"completion_progress"`
		CompletionSourceStart completionSourceStartRecord `json:"completion_source"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(attempt.WorkerMetadataJSON)), &root); err != nil {
		return implementProgressRecord{}, false
	}
	record := root.CompletionProgress
	if strings.TrimSpace(record.Outcome) == "" {
		return implementProgressRecord{}, false
	}
	record.CompletionSourceStart = root.CompletionSourceStart
	return record, true
}

func implementCompletionProgressMetadata(decision implementCompletionProgressDecision) map[string]any {
	record := implementProgressRecord{
		Outcome:                string(decision.Outcome),
		Reason:                 decision.Reason,
		CurrentSignature:       implementProgressSignatureRecordFromSignature(decision.CurrentSignature),
		PreviousHeadSHA:        decision.PreviousSignature.HeadSHA,
		CurrentHeadSHA:         decision.CurrentSignature.HeadSHA,
		FailedChecksAdded:      append([]string(nil), decision.FailedChecksAdded...),
		FailedChecksRemoved:    append([]string(nil), decision.FailedChecksRemoved...),
		WorkspaceDiffStats:     implementProgressDiffStatsFromDiffStats(decision.WorkspaceDiffStats),
		WorkpadStatus:          strings.TrimSpace(decision.WorkpadStatus),
		HumanAction:            strings.TrimSpace(decision.HumanAction),
		TrackerState:           strings.TrimSpace(decision.TrackerState),
		ConsecutiveHumanAction: decision.ConsecutiveHumanAction,
		BlockReason:            strings.TrimSpace(decision.BlockReason),
		Warning:                strings.TrimSpace(decision.Warning),
		DependencyDeferral:     decision.DependencyDeferral,
		DependencyBlockers:     append([]implementDependencyBlocker(nil), decision.DependencyBlockers...),
		RejectedBlockerRefs:    append([]string(nil), decision.RejectedBlockerRefs...),
		ProgressKinds:          append([]string(nil), decision.ProgressKinds...),
		CompletionKind:         strings.TrimSpace(decision.CompletionKind),
	}
	if decision.PreviousSignatureFound {
		previous := implementProgressSignatureRecordFromSignature(decision.PreviousSignature)
		record.PreviousSignature = &previous
	}
	return map[string]any{implementProgressMetadataKey: record}
}

func implementProgressSignatureRecordFromSignature(signature autoPromoteReworkSignature) implementProgressSignatureRecord {
	return implementProgressSignatureRecord{
		PRNumber:     signature.PRNumber,
		HeadSHA:      strings.TrimSpace(signature.HeadSHA),
		FailedChecks: append([]string(nil), signature.FailedChecks...),
	}
}

func (r implementProgressSignatureRecord) signature() autoPromoteReworkSignature {
	return autoPromoteReworkSignature{
		PRNumber:     r.PRNumber,
		HeadSHA:      strings.TrimSpace(r.HeadSHA),
		FailedChecks: autoPromoteCanonicalChecks(r.FailedChecks),
	}
}

func implementProgressDiffStatsFromDiffStats(diffStats DiffStats) implementProgressDiffStats {
	return implementProgressDiffStats{
		FilesChanged:                   diffStats.FilesChanged,
		AddedLines:                     diffStats.AddedLines,
		RemovedLines:                   diffStats.RemovedLines,
		UnpushedCommits:                diffStats.UnpushedCommits,
		UnpushedCommitRefs:             append([]string(nil), diffStats.UnpushedCommitRefs...),
		TrackedPaths:                   append([]string(nil), diffStats.TrackedPaths...),
		UntrackedPaths:                 append([]string(nil), diffStats.UntrackedPaths...),
		CommitsNotInPullRequest:        append([]string(nil), diffStats.CommitsNotInPullRequest...),
		PullRequestComparisonAvailable: diffStats.PullRequestComparisonAvailable,
		RecoveryStateExpected:          diffStats.RecoveryStateExpected,
		RecoveryStateAvailable:         diffStats.RecoveryStateAvailable,
		HeadSHA:                        strings.TrimSpace(diffStats.HeadSHA),
		TreeSHA:                        strings.TrimSpace(diffStats.TreeSHA),
		Fingerprint:                    strings.TrimSpace(diffStats.Fingerprint),
		Status:                         strings.TrimSpace(diffStats.Status),
	}
}

func implementProgressSignatureUsable(signature autoPromoteReworkSignature) bool {
	return signature.PRNumber > 0 && strings.TrimSpace(signature.HeadSHA) != ""
}

func implementProgressSignatureEqual(left autoPromoteReworkSignature, right autoPromoteReworkSignature) bool {
	return left.PRNumber == right.PRNumber &&
		strings.TrimSpace(left.HeadSHA) == strings.TrimSpace(right.HeadSHA) &&
		slices.Equal(autoPromoteCanonicalChecks(left.FailedChecks), autoPromoteCanonicalChecks(right.FailedChecks))
}

func implementProgressDiffStatsClean(diffStats DiffStats) bool {
	return diffStatsPresent(diffStats) &&
		diffStats.FilesChanged == 0 &&
		diffStats.AddedLines == 0 &&
		diffStats.RemovedLines == 0
}

func implementProgressOperationalWorkspaceClean(diffStats DiffStats) bool {
	return implementProgressDiffStatsClean(diffStats) &&
		diffStats.UnpushedCommits == 0 &&
		diffStats.CommitsAhead == 0 &&
		len(diffStats.TrackedPaths) == 0 &&
		len(diffStats.UntrackedPaths) == 0 &&
		len(diffStats.CommitsNotInPullRequest) == 0
}

func implementProgressReconcilePullRequestEvidence(diffStats DiffStats, pullRequest *connector.PullRequest) DiffStats {
	if pullRequest == nil || strings.TrimSpace(diffStats.HeadSHA) == "" ||
		strings.TrimSpace(diffStats.HeadSHA) != strings.TrimSpace(pullRequest.HeadSHA) {
		return diffStats
	}
	diffStats.UnpushedCommits = 0
	diffStats.UnpushedCommitRefs = nil
	diffStats.CommitsNotInPullRequest = nil
	diffStats.PullRequestComparisonAvailable = true
	return diffStats
}

func implementProgressUnpushedClassification(diffStats DiffStats, pullRequest *connector.PullRequest) (bool, string) {
	if pullRequest != nil {
		if len(diffStats.TrackedPaths) > 0 || len(diffStats.UntrackedPaths) > 0 {
			return true, ""
		}
		if diffStats.PullRequestComparisonAvailable {
			return len(diffStats.CommitsNotInPullRequest) > 0, ""
		}
	}
	if diffStats.UnpushedCommits <= 0 {
		return false, ""
	}
	if pullRequest == nil {
		if !implementProgressDiffStatsClean(diffStats) {
			return true, ""
		}
		return false, unpushedRemoteTruthUnavailable
	}
	workspaceHead := strings.TrimSpace(diffStats.HeadSHA)
	if workspaceHead == "" {
		return false, workspaceHeadUnavailableReason
	}
	pullRequestHead := strings.TrimSpace(pullRequest.HeadSHA)
	if pullRequestHead == "" {
		return false, pullRequestHeadUnavailableReason
	}
	if workspaceHead == pullRequestHead {
		return false, ""
	}
	return true, ""
}

func implementProgressLinkedPullRequest(issue connector.Issue) bool {
	return workAttemptPRNumber(issue) != nil
}

func implementProgressHydrationUnavailableReason(pullRequest *connector.PullRequest) string {
	reasons := make([]string, 0, 2)
	if reason := pullRequestHydrationUnavailableReason(pullRequest); reason != "" {
		reasons = append(reasons, reason)
	}
	if reason := pullRequestHydrationDegradedReason(pullRequest); reason != "" {
		reasons = append(reasons, reason)
	}
	return strings.Join(reasons, "; ")
}

func implementProgressFailedCheckDelta(previous []string, current []string) ([]string, []string) {
	previous = autoPromoteCanonicalChecks(previous)
	current = autoPromoteCanonicalChecks(current)
	added := make([]string, 0)
	removed := make([]string, 0)
	for _, check := range current {
		if !slices.Contains(previous, check) {
			added = append(added, check)
		}
	}
	for _, check := range previous {
		if !slices.Contains(current, check) {
			removed = append(removed, check)
		}
	}
	return added, removed
}

func (o *Orchestrator) warnImplementProgressHydration(issue connector.Issue, message string, err error) {
	if o == nil || o.logger == nil {
		return
	}
	attrs := []any{
		"issue_id", issue.ID,
		"identifier", issue.Identifier,
		"reason", strings.TrimSpace(message),
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	o.logger.Warn("implement worker progress evidence unavailable", attrs...)
}

func (o *Orchestrator) warnImplementProgressRefresh(issue connector.Issue, message string, err error) {
	if o == nil || o.logger == nil {
		return
	}
	attrs := []any{
		"issue_id", issue.ID,
		"identifier", issue.Identifier,
		"reason", strings.TrimSpace(message),
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	o.logger.Warn("implement worker tracker refresh failed open", attrs...)
}

func (o *Orchestrator) blockImplementProgress(
	ctx context.Context,
	state *State,
	running Running,
	decision implementCompletionProgressDecision,
	event runpkg.Completion,
) bool {
	if action := explicitHumanAction(decision.Issue); action != "" {
		running.Issue = decision.Issue
		event.Err = errors.New(action)
		return o.blockHumanOwnedWorkerFailure(ctx, state, event, running, workpadBlockedUnactionedReason, action, action, "worker_permission_wait")
	}
	running.Issue = decision.Issue
	return o.retryMachineCompletion(ctx, state, event, running, nextAttempt(running.Attempt), decision.Reason)
}

func (o *Orchestrator) finishImplementDependencyDeferral(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	completedAt time.Time,
) {
	if !o.currentBlockedOperatorStop(ctx, state, issue) && !blockedRecoveryHumanOnly(issue) {
		sourceState, _ := o.dispatchTimelineTransitionContext(ctx, issue)
		target := dependencyWaitTarget(issue, sourceState)
		if normalizeState(issue.State) != normalizeState(target) {
			if err := o.updateIssueState(ctx, state, issue, target, completedAt, "dependency_wait"); err != nil && o.logger != nil {
				o.logger.Warn("dependency wait lane update failed", "issue_id", issue.ID, "error", err)
			}
		}
	}
	if err := o.abandonClaim(ctx, issue.ID); err != nil && o.logger != nil {
		o.logger.Warn("implement dependency deferral claim release failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
	}
	o.releaseClaim(state, issue.ID)
	delete(state.mergeReservations, issue.ID)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      completedAt,
		Event:   "implement_dependency_deferred",
		Message: "deferred " + issueLabel(issue) + " while declared dependencies remain unresolved",
	})
}

func appendQuotedEvidence(b *strings.Builder, label string, values []string) {
	if len(values) == 0 {
		return
	}
	b.WriteString("\n- ")
	b.WriteString(label)
	b.WriteString(": ")
	for index, value := range values {
		if index > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Quote(value))
	}
}
