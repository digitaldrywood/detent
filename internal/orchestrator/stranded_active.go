package orchestrator

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	strandedActiveState          = "in progress"
	strandedActiveRecoveryReason = "stranded_active_recovery"
)

type strandedActiveRecoveryDecision struct {
	TargetState string
	Evidence    string
	HoldReason  string
}

func strandedActiveIssueSnapshots(state State, issues []telemetry.Issue, now time.Time) []telemetry.StrandedIssue {
	if state.StrandedActiveThreshold <= 0 || now.IsZero() {
		return nil
	}

	diagnostics := make([]telemetry.StrandedIssue, 0)
	for _, issue := range issues {
		if normalizeState(issue.State) != strandedActiveState || strandedActiveIssueHasWorker(issue, state.Running, state.WorkAttempts, now) {
			continue
		}
		since, ok := strandedActiveSince(issue, state.WorkAttempts, now)
		if !ok {
			continue
		}
		duration := now.Sub(since)
		if duration <= state.StrandedActiveThreshold {
			continue
		}
		reason, refusedAt := latestStrandedActiveRefusal(issue, state.SchedulerDecisions)
		diagnostics = append(diagnostics, telemetry.StrandedIssue{
			IssueID:           strings.TrimSpace(issue.ID),
			Identifier:        strings.TrimSpace(issue.Identifier),
			IssueURL:          strings.TrimSpace(issue.URL),
			Title:             strings.TrimSpace(issue.Title),
			State:             strings.TrimSpace(issue.State),
			Since:             since.UTC(),
			DurationSeconds:   int64(duration / time.Second),
			ThresholdSeconds:  int64(state.StrandedActiveThreshold / time.Second),
			LastRefusalReason: reason,
			LastRefusalAt:     refusedAt,
		})
	}
	sort.Slice(diagnostics, func(i, j int) bool {
		return strandedActiveTarget(diagnostics[i]) < strandedActiveTarget(diagnostics[j])
	})
	return diagnostics
}

func (o *Orchestrator) recoverStrandedActiveIssues(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) map[string]struct{} {
	if o == nil || state == nil || len(issues) == 0 {
		return nil
	}
	diagnostics := strandedActiveIssueSnapshots(
		*state,
		issueSnapshots(issues, 0, 0, now, state.laneEntries),
		now,
	)
	transitioned := make(map[string]struct{}, len(diagnostics))
	for _, diagnostic := range diagnostics {
		issue, ok := strandedActiveConnectorIssue(diagnostic, issues)
		if !ok || strings.TrimSpace(issue.ID) == "" {
			continue
		}
		decision := o.strandedActiveRecoveryDecision(ctx, issue)
		if strings.TrimSpace(decision.TargetState) == "" {
			if o.logger != nil {
				o.logger.Debug(
					"stranded active issue recovery held",
					"issue_id", issue.ID,
					"identifier", issue.Identifier,
					"duration_seconds", diagnostic.DurationSeconds,
					"reason", decision.HoldReason,
				)
			}
			continue
		}
		if err := o.updateIssueStateByIDStrictWithMetadata(
			ctx,
			state,
			issue.ID,
			issue,
			decision.TargetState,
			now,
			strandedActiveRecoveryReason,
			workflowLaneMetadata{},
			laneMutationRevokeWorker,
		); err != nil {
			if o.logger != nil {
				o.logger.Warn(
					"stranded active issue recovery failed",
					"issue_id", issue.ID,
					"identifier", issue.Identifier,
					"duration_seconds", diagnostic.DurationSeconds,
					"target_state", decision.TargetState,
					"evidence", decision.Evidence,
					"error", err,
				)
			}
			continue
		}
		transitioned[issue.ID] = struct{}{}
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now,
			Event:   "stranded_active_recovered",
			Message: "recovered " + issueLabel(issue) + " to " + decision.TargetState + " after " + (time.Duration(diagnostic.DurationSeconds) * time.Second).String() + " without a live worker; evidence: " + decision.Evidence,
		})
		if o.logger != nil {
			o.logger.Info(
				"stranded active issue recovered",
				"issue_id", issue.ID,
				"identifier", issue.Identifier,
				"duration_seconds", diagnostic.DurationSeconds,
				"target_state", decision.TargetState,
				"evidence", decision.Evidence,
			)
		}
	}
	if len(transitioned) == 0 {
		return nil
	}
	return transitioned
}

func (o *Orchestrator) strandedActiveRecoveryDecision(ctx context.Context, issue connector.Issue) strandedActiveRecoveryDecision {
	openPullRequest, pullRequestUncertain := strandedActivePullRequestEvidence(issue)
	if openPullRequest {
		return strandedActiveRecoveryDecision{TargetState: autoPromoteReworkState, Evidence: "open_pull_request"}
	}

	snapshot := runpkg.BlockedRecoverySnapshot{WorkspaceStatus: "unavailable"}
	if o.recoveryInspector != nil {
		snapshot = o.recoveryInspector.BlockedRecoverySnapshot(ctx, runpkg.RunRequest{
			Issue:           issue,
			Mode:            RunModeImplement,
			SelectorContext: o.selectorContext(),
		})
	}
	workspaceWork, workspaceReady := strandedActiveWorkspaceEvidence(snapshot)
	if workspaceWork {
		evidence := "recoverable_workspace"
		if snapshot.UnpushedCommits > 0 {
			evidence = "unpushed_work"
		}
		return strandedActiveRecoveryDecision{TargetState: autoPromoteReworkState, Evidence: evidence}
	}
	if pullRequestUncertain {
		return strandedActiveRecoveryDecision{HoldReason: "pull_request_evidence_unavailable"}
	}
	if !workspaceReady {
		return strandedActiveRecoveryDecision{HoldReason: "workspace_evidence_unavailable"}
	}
	return strandedActiveRecoveryDecision{TargetState: "Todo", Evidence: "no_recovery_artifacts"}
}

func strandedActivePullRequestEvidence(issue connector.Issue) (bool, bool) {
	if issue.PullRequest != nil {
		if pullRequestHydrationBlocksProgress(issue.PullRequest) {
			return false, true
		}
		switch normalizePullRequestState(issue.PullRequest.State) {
		case "open":
			return true, false
		case "closed", "merged":
			return false, false
		case "":
			if issue.PullRequest.Number > 0 ||
				strings.TrimSpace(issue.PullRequest.URL) != "" ||
				strings.TrimSpace(issue.PullRequest.BranchName) != "" {
				return false, true
			}
		default:
			return false, true
		}
	}
	return false, issue.PRNumber != nil && *issue.PRNumber > 0
}

func strandedActiveWorkspaceEvidence(snapshot runpkg.BlockedRecoverySnapshot) (bool, bool) {
	status := strings.TrimSpace(snapshot.WorkspaceStatus)
	if status == "missing" {
		return false, true
	}
	if status != "present" || !snapshot.WorkspacePresent {
		return false, false
	}
	if snapshot.UnpushedCommits > 0 || snapshot.WorkspaceFiles > 0 {
		return true, true
	}
	head := strings.TrimSpace(snapshot.HeadSHA)
	base := strings.TrimSpace(snapshot.BaseFingerprint)
	if head != "" && base != "" {
		return head != base, true
	}
	if head != "" || base != "" {
		return false, false
	}
	return false, true
}

func strandedActiveConnectorIssue(diagnostic telemetry.StrandedIssue, issues []connector.Issue) (connector.Issue, bool) {
	for _, issue := range issues {
		if strandedActiveIdentityMatches(
			diagnostic.IssueID,
			diagnostic.Identifier,
			diagnostic.IssueURL,
			issue.ID,
			issue.Identifier,
			issue.URL,
		) {
			return issue, true
		}
	}
	return connector.Issue{}, false
}

func strandedActiveIssueHasWorker(issue telemetry.Issue, running map[string]Running, attempts []telemetry.WorkAttempt, now time.Time) bool {
	for _, worker := range running {
		if strandedActiveIdentityMatches(
			issue.ID,
			issue.Identifier,
			issue.URL,
			worker.Issue.ID,
			worker.Issue.Identifier,
			worker.Issue.URL,
		) {
			return true
		}
	}
	for _, attempt := range attempts {
		if !strandedActiveWorkAttemptIsLive(attempt, now) {
			continue
		}
		if strandedActiveIdentityMatches(
			issue.ID,
			issue.Identifier,
			issue.URL,
			attempt.IssueID,
			attempt.Identifier,
			attempt.IssueURL,
		) {
			return true
		}
	}
	return false
}

func strandedActiveWorkAttemptIsLive(attempt telemetry.WorkAttempt, now time.Time) bool {
	if !strings.EqualFold(strings.TrimSpace(attempt.Status), string(store.WorkAttemptStatusActive)) || attempt.Stale {
		return false
	}
	return attempt.LeaseExpiresAt == nil || attempt.LeaseExpiresAt.After(now)
}

func strandedActiveSince(issue telemetry.Issue, attempts []telemetry.WorkAttempt, now time.Time) (time.Time, bool) {
	if issue.CurrentLaneEnteredAt == nil || issue.CurrentLaneEnteredAt.IsZero() || now.Before(*issue.CurrentLaneEnteredAt) {
		return time.Time{}, false
	}
	since := *issue.CurrentLaneEnteredAt
	for _, attempt := range attempts {
		if !strandedActiveIdentityMatches(
			issue.ID,
			issue.Identifier,
			issue.URL,
			attempt.IssueID,
			attempt.Identifier,
			attempt.IssueURL,
		) {
			continue
		}
		if attempt.CompletedAt != nil && !attempt.CompletedAt.After(now) && attempt.CompletedAt.After(since) {
			since = *attempt.CompletedAt
		}
		if strings.EqualFold(strings.TrimSpace(attempt.Status), string(store.WorkAttemptStatusActive)) &&
			attempt.LeaseExpiresAt != nil && !attempt.LeaseExpiresAt.After(now) && attempt.LeaseExpiresAt.After(since) {
			since = *attempt.LeaseExpiresAt
		}
	}
	return since, true
}

func latestStrandedActiveRefusal(issue telemetry.Issue, decisions []telemetry.SchedulerDecision) (string, *time.Time) {
	var latest telemetry.SchedulerDecision
	for _, decision := range decisions {
		if !strings.EqualFold(strings.TrimSpace(decision.Result), "skipped") ||
			!strandedActiveIdentityMatches(
				issue.ID,
				issue.Identifier,
				issue.URL,
				decision.IssueID,
				decision.Identifier,
				decision.IssueURL,
			) {
			continue
		}
		if latest.DecisionAt.IsZero() || decision.DecisionAt.After(latest.DecisionAt) {
			latest = decision
		}
	}
	if latest.DecisionAt.IsZero() {
		return "", nil
	}
	reason := strings.TrimSpace(latest.Reason)
	if reason == "" {
		reason = strings.TrimSpace(latest.WaitReason)
	}
	return reason, timePointer(latest.DecisionAt)
}

func strandedActiveIdentityMatches(leftID, leftIdentifier, leftURL, rightID, rightIdentifier, rightURL string) bool {
	for _, pair := range [][2]string{
		{leftID, rightID},
		{leftIdentifier, rightIdentifier},
		{leftURL, rightURL},
	} {
		left := strings.TrimSpace(pair[0])
		right := strings.TrimSpace(pair[1])
		if left != "" && right != "" && left == right {
			return true
		}
	}
	return false
}

func strandedActiveTarget(issue telemetry.StrandedIssue) string {
	for _, value := range []string{issue.Identifier, issue.IssueID, issue.IssueURL} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "issue"
}
