package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/provenance"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const dispatchLoopDetectedReason = "dispatch_loop_detected"

const dispatchLoopStartMetadataKey = "dispatch_loop_start"

func dispatchLoopBlockMessage(decision implementCompletionProgressDecision) string {
	return fmt.Sprintf(
		"loop detected after %d dispatches without lane, diff, commit, or pull request advancement",
		decision.ConsecutiveNoProgress,
	)
}

type dispatchLoopFingerprint struct {
	Lane            string   `json:"lane,omitempty"`
	PRNumber        int64    `json:"pr_number,omitempty"`
	PRHeadSHA       string   `json:"pr_head_sha,omitempty"`
	FailedChecks    []string `json:"failed_checks,omitempty"`
	FilesChanged    int      `json:"files_changed"`
	AddedLines      int      `json:"added_lines"`
	RemovedLines    int      `json:"removed_lines"`
	UnpushedCommits int      `json:"unpushed_commits,omitempty"`
	WorkspaceHead   string   `json:"workspace_head,omitempty"`
	DiffFingerprint string   `json:"diff_fingerprint,omitempty"`
	DiffStatus      string   `json:"diff_status,omitempty"`
}

type dispatchLoopStartRecord struct {
	Fingerprint            dispatchLoopFingerprint `json:"fingerprint"`
	Captured               bool                    `json:"captured"`
	Persisted              bool                    `json:"persisted"`
	LaneAvailable          bool                    `json:"lane_available"`
	PullRequestAvailable   bool                    `json:"pull_request_available"`
	WorkspaceDiffAvailable bool                    `json:"workspace_diff_available"`
	WorkspaceHeadAvailable bool                    `json:"workspace_head_available"`
}

// Historical progress evidence remains readable; enforcement belongs solely to
// the issue allowance at dispatch, not completion fingerprints.
func (o *Orchestrator) evaluateDispatchLoopProgress(_ context.Context, _ Running, decision implementCompletionProgressDecision) implementCompletionProgressDecision {
	return resetDispatchLoopDecision(decision)
}

func operatorAcknowledgesRecoveryPark(event store.WorkflowPhaseEvent, metadata workflowLaneMetadata) bool {
	if event.Reason == "operator_move" && metadata.Provenance.Origin == provenance.OriginHuman {
		return true
	}
	if metadata.Provenance.Initiator == provenance.InitiatorHuman && metadata.Provenance.Basis == provenance.BasisAuthenticatedHuman {
		return true
	}
	switch event.Reason {
	case "kanban_move", "kanban_move_field":
		return true
	default:
		return false
	}
}

func resetDispatchLoopState(state *State, issue connector.Issue, acknowledgedAt time.Time) bool {
	if state == nil || acknowledgedAt.IsZero() {
		return false
	}
	key := workflowIssueIdentityKey(issue)
	if key == "" {
		return false
	}
	if state.dispatchLoopResets == nil {
		state.dispatchLoopResets = map[string]time.Time{}
	}
	state.dispatchLoopResets[key] = laterDispatchLoopTime(state.dispatchLoopResets[key], acknowledgedAt)
	cleared := false
	for _, attempt := range state.WorkAttempts {
		matches := snapshotIssueMatches(telemetryIssue(issue, 0, 0, acknowledgedAt, nil), attempt.IssueID, attempt.Identifier, attempt.IssueURL)
		if matches && strings.EqualFold(strings.TrimSpace(attempt.Status), string(store.WorkAttemptStatusTerminal)) &&
			attempt.CompletedAt != nil && !attempt.CompletedAt.After(acknowledgedAt) {
			record, ok := implementProgressRecordFromAnyAttempt(store.WorkAttempt{
				TerminalState:      store.WorkAttemptTerminalState(attempt.TerminalState),
				WorkerMetadataJSON: attempt.WorkerMetadataJSON,
			})
			cleared = cleared || ok && record.ConsecutiveNoProgress > 0
		}
	}
	return cleared
}

func dispatchLoopSnapshotAttemptVisible(issue telemetry.Issue, attempt telemetry.WorkAttempt, resets map[string]time.Time) bool {
	key := workflowIssueIdentityKey(connector.Issue{ID: issue.ID, Identifier: issue.Identifier, URL: issue.URL})
	resetAt := resets[key]
	return resetAt.IsZero() || attempt.CompletedAt == nil || attempt.CompletedAt.After(resetAt)
}

func laterDispatchLoopTime(current, candidate time.Time) time.Time {
	if candidate.After(current) {
		return candidate
	}
	return current
}

func resetDispatchLoopDecision(decision implementCompletionProgressDecision) implementCompletionProgressDecision {
	decision.ConsecutiveNoProgress = 0
	if decision.BlockReason == noProgressLimitReason {
		decision.Block = false
		decision.BlockReason = ""
	}
	return decision
}

func newDispatchLoopStartRecord(issue connector.Issue, mode string) dispatchLoopStartRecord {
	if strings.TrimSpace(mode) != RunModeImplement {
		return dispatchLoopStartRecord{}
	}
	signature := autoPromoteReworkSignatureFromIssue(issue, AutoPromoteSummaryFromIssue(issue))
	lane := normalizeState(issue.State)
	return dispatchLoopStartRecord{
		Fingerprint:          dispatchLoopFingerprintFromValues(lane, signature, implementProgressDiffStats{}),
		LaneAvailable:        lane != "",
		PullRequestAvailable: dispatchLoopPullRequestEvidenceAvailable(workAttemptPRNumber(issue), signature, ""),
	}
}

func dispatchLoopStartRecordFromSnapshot(
	running Running,
	snapshot runpkg.DispatchLoopStartSnapshot,
) dispatchLoopStartRecord {
	record := running.DispatchLoopStart
	diff := implementProgressDiffStatsFromDiffStats(snapshot.DiffStats)
	record.Fingerprint = dispatchLoopFingerprintFromValues(
		normalizeState(firstNonBlank(running.DispatchSourceState, running.Issue.State)),
		record.Fingerprint.signature(),
		diff,
	)
	record.Captured = true
	record.Persisted = true
	record.WorkspaceDiffAvailable = snapshot.WorkspaceDiffAvailable
	record.WorkspaceHeadAvailable = snapshot.WorkspaceHeadAvailable
	return record
}

func (f dispatchLoopFingerprint) signature() autoPromoteReworkSignature {
	return autoPromoteReworkSignature{
		PRNumber:     f.PRNumber,
		HeadSHA:      strings.TrimSpace(f.PRHeadSHA),
		FailedChecks: append([]string(nil), f.FailedChecks...),
	}
}

func dispatchLoopPullRequestEvidenceAvailable(number *int64, signature autoPromoteReworkSignature, reason string) bool {
	switch strings.TrimSpace(reason) {
	case "pull_request_hydrator_unavailable", "pull_request_hydration_failed", "pull_request_hydration_unavailable":
		return false
	}
	if number == nil && signature.PRNumber == 0 {
		return true
	}
	return implementProgressSignatureUsable(signature)
}

func dispatchLoopFingerprintFromValues(lane string, signature autoPromoteReworkSignature, diff implementProgressDiffStats) dispatchLoopFingerprint {
	diffStatus := strings.TrimSpace(diff.Status)
	if diffStatus == "" && diff.FilesChanged == 0 && diff.AddedLines == 0 && diff.RemovedLines == 0 &&
		diff.UnpushedCommits == 0 && strings.TrimSpace(diff.HeadSHA) == "" && strings.TrimSpace(diff.Fingerprint) == "" {
		diffStatus = "clean"
	}
	return dispatchLoopFingerprint{
		Lane:            normalizeState(lane),
		PRNumber:        signature.PRNumber,
		PRHeadSHA:       strings.TrimSpace(signature.HeadSHA),
		FailedChecks:    autoPromoteCanonicalChecks(signature.FailedChecks),
		FilesChanged:    diff.FilesChanged,
		AddedLines:      diff.AddedLines,
		RemovedLines:    diff.RemovedLines,
		UnpushedCommits: diff.UnpushedCommits,
		WorkspaceHead:   strings.TrimSpace(diff.HeadSHA),
		DiffFingerprint: strings.TrimSpace(diff.Fingerprint),
		DiffStatus:      diffStatus,
	}
}
