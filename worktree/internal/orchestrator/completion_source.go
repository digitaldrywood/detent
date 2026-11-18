package orchestrator

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

const completionSourceStartMetadataKey = "completion_source"

type completionSourceFingerprint struct {
	Lane            string   `json:"lane,omitempty"`
	PRNumber        int64    `json:"pr_number,omitempty"`
	PRHeadSHA       string   `json:"pr_head_sha,omitempty"`
	FailedChecks    []string `json:"failed_checks,omitempty"`
	FilesChanged    int      `json:"files_changed"`
	AddedLines      int      `json:"added_lines"`
	RemovedLines    int      `json:"removed_lines"`
	UnpushedCommits int      `json:"unpushed_commits,omitempty"`
	WorkspaceTree   string   `json:"workspace_tree,omitempty"`
	WorkspaceHead   string   `json:"workspace_head,omitempty"`
	DiffFingerprint string   `json:"diff_fingerprint,omitempty"`
	DiffStatus      string   `json:"diff_status,omitempty"`
}

type completionSourceStartRecord struct {
	PRDiffFingerprint      string                      `json:"pr_diff_fingerprint,omitempty"`
	PRMergeableState       string                      `json:"pr_mergeable_state,omitempty"`
	Fingerprint            completionSourceFingerprint `json:"fingerprint"`
	Captured               bool                        `json:"captured"`
	Persisted              bool                        `json:"persisted"`
	LaneAvailable          bool                        `json:"lane_available"`
	PullRequestAvailable   bool                        `json:"pull_request_available"`
	WorkspaceDiffAvailable bool                        `json:"workspace_diff_available"`
	WorkspaceHeadAvailable bool                        `json:"workspace_head_available"`
}

func newCompletionSourceStartRecord(issue connector.Issue, mode string) completionSourceStartRecord {
	if strings.TrimSpace(mode) != RunModeImplement && (strings.TrimSpace(mode) != runpkg.RunModeMerge || mergeWorkerIssue(issue)) {
		return completionSourceStartRecord{}
	}
	signature := autoPromoteReworkSignatureFromIssue(issue, AutoPromoteSummaryFromIssue(issue))
	lane := normalizeState(issue.State)
	return completionSourceStartRecord{
		Fingerprint:          completionSourceFingerprintFromValues(lane, signature, implementProgressDiffStats{}),
		LaneAvailable:        lane != "",
		PullRequestAvailable: completionSourcePullRequestEvidenceAvailable(workAttemptPRNumber(issue), signature, ""),
	}
}

func completionSourceStartRecordFromSnapshot(
	running Running,
	snapshot runpkg.CompletionSourceStartSnapshot,
) completionSourceStartRecord {
	record := running.CompletionSourceStart
	diff := implementProgressDiffStatsFromDiffStats(snapshot.DiffStats)
	record.Fingerprint = completionSourceFingerprintFromValues(
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

func (f completionSourceFingerprint) signature() autoPromoteReworkSignature {
	return autoPromoteReworkSignature{
		PRNumber:     f.PRNumber,
		HeadSHA:      strings.TrimSpace(f.PRHeadSHA),
		FailedChecks: append([]string(nil), f.FailedChecks...),
	}
}

func completionSourcePullRequestEvidenceAvailable(number *int64, signature autoPromoteReworkSignature, reason string) bool {
	switch strings.TrimSpace(reason) {
	case "pull_request_hydrator_unavailable", "pull_request_hydration_failed", "pull_request_hydration_unavailable":
		return false
	}
	if number == nil && signature.PRNumber == 0 {
		return true
	}
	return implementProgressSignatureUsable(signature)
}

func completionSourceFingerprintFromValues(lane string, signature autoPromoteReworkSignature, diff implementProgressDiffStats) completionSourceFingerprint {
	diffStatus := strings.TrimSpace(diff.Status)
	if diffStatus == "" && diff.FilesChanged == 0 && diff.AddedLines == 0 && diff.RemovedLines == 0 &&
		diff.UnpushedCommits == 0 && strings.TrimSpace(diff.HeadSHA) == "" && strings.TrimSpace(diff.Fingerprint) == "" {
		diffStatus = "clean"
	}
	return completionSourceFingerprint{
		Lane:            normalizeState(lane),
		PRNumber:        signature.PRNumber,
		PRHeadSHA:       strings.TrimSpace(signature.HeadSHA),
		FailedChecks:    autoPromoteCanonicalChecks(signature.FailedChecks),
		FilesChanged:    diff.FilesChanged,
		AddedLines:      diff.AddedLines,
		RemovedLines:    diff.RemovedLines,
		UnpushedCommits: diff.UnpushedCommits,
		WorkspaceHead:   strings.TrimSpace(diff.HeadSHA),
		WorkspaceTree:   strings.TrimSpace(diff.TreeSHA),
		DiffFingerprint: strings.TrimSpace(diff.Fingerprint),
		DiffStatus:      diffStatus,
	}
}
