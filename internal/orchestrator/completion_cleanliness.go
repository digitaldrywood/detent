package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const (
	completionCleanlinessMetadataKey       = "completion_cleanliness"
	completionCleanlinessAccepted          = "accepted"
	completionCleanlinessRejected          = "rejected"
	completionCleanlinessEscalated         = "escalated"
	completionCleanlinessClean             = "clean"
	completionCleanlinessCommitted         = "committed"
	completionCleanlinessDiscarded         = "discarded"
	completionCleanlinessIntentionallyLeft = "intentionally_left"
	dirtyCompletionReason                  = "dirty_worktree_completion_rejected"
	dirtyCompletionResolutionReason        = "dirty_worktree_resolution_required"
	dirtyCompletionEscalationReason        = "dirty_worktree_completion_unresolved"
	completionCleanlinessRejectionLimit    = 2
)

type completionCleanlinessDecision struct {
	Attempted             bool
	Outcome               string
	Resolution            string
	Declaration           string
	Reason                string
	Statement             string
	Evidence              DiffStats
	ConsecutiveRejections int
	Block                 bool
	Warning               string
}

type completionCleanlinessRecord struct {
	Outcome               string                        `json:"outcome"`
	Resolution            string                        `json:"resolution,omitempty"`
	Declaration           string                        `json:"declaration,omitempty"`
	Reason                string                        `json:"reason,omitempty"`
	Statement             string                        `json:"statement,omitempty"`
	Evidence              completionCleanlinessEvidence `json:"evidence"`
	ConsecutiveRejections int                           `json:"consecutive_rejections,omitempty"`
	Warning               string                        `json:"warning,omitempty"`
}

type completionCleanlinessEvidence struct {
	FilesChanged            int      `json:"files_changed"`
	AddedLines              int      `json:"added_lines"`
	RemovedLines            int      `json:"removed_lines"`
	TrackedPaths            []string `json:"tracked_paths,omitempty"`
	UntrackedPaths          []string `json:"untracked_paths,omitempty"`
	UnpushedCommits         int      `json:"unpushed_commits,omitempty"`
	UnpushedCommitRefs      []string `json:"unpushed_commit_refs,omitempty"`
	CommitsNotInPullRequest []string `json:"commits_not_in_pull_request,omitempty"`
	RecoveryStateExpected   bool     `json:"recovery_state_expected,omitempty"`
	RecoveryStateAvailable  bool     `json:"recovery_state_available,omitempty"`
	HeadSHA                 string   `json:"head_sha,omitempty"`
	Fingerprint             string   `json:"fingerprint,omitempty"`
	Status                  string   `json:"status,omitempty"`
}

func (o *Orchestrator) evaluateCompletionCleanliness(
	ctx context.Context,
	running Running,
	issue connector.Issue,
	evidence DiffStats,
) completionCleanlinessDecision {
	attempted := completionCleanlinessAttempted(running, issue)
	intentionalStatement := completionCleanlinessIntentionalStatement(issue)
	if !attempted && intentionalStatement == "" {
		return completionCleanlinessDecision{}
	}
	decision := completionCleanlinessDecision{
		Evidence: evidence,
	}
	attempts, err := o.recentImplementCompletionAttempts(ctx, issue, running)
	if err != nil {
		if !attempted {
			return completionCleanlinessDecision{}
		}
		decision.Attempted = true
		decision.Warning = err.Error()
		decision.Outcome = completionCleanlinessEscalated
		decision.Reason = dirtyCompletionEscalationReason
		decision.ConsecutiveRejections = 1
		decision.Block = true
		return decision
	}
	previousRejections := consecutiveCompletionCleanlinessRejections(attempts)
	if !attempted && previousRejections == 0 {
		return completionCleanlinessDecision{}
	}
	decision.Attempted = true
	decision.Declaration = completionCleanlinessResolutionDeclaration(issue, running)
	if intentionalStatement != "" {
		decision.Outcome = completionCleanlinessEscalated
		decision.Resolution = completionCleanlinessIntentionallyLeft
		decision.Reason = dirtyCompletionEscalationReason
		decision.Statement = intentionalStatement
		decision.ConsecutiveRejections = previousRejections + 1
		decision.Block = true
		return decision
	}
	if !diffStatsPresent(evidence) {
		decision.Outcome = completionCleanlinessRejected
		decision.Reason = "workspace_cleanliness_unavailable"
		decision.ConsecutiveRejections = previousRejections + 1
		decision.Block = decision.ConsecutiveRejections >= completionCleanlinessRejectionLimit
		if decision.Block {
			decision.Outcome = completionCleanlinessEscalated
			decision.Reason = dirtyCompletionEscalationReason
		}
		return decision
	}
	if evidence.RecoveryStateExpected && !evidence.RecoveryStateAvailable {
		decision.Outcome = completionCleanlinessRejected
		decision.Reason = "workspace_recovery_state_unavailable"
		decision.ConsecutiveRejections = previousRejections + 1
		decision.Block = decision.ConsecutiveRejections >= completionCleanlinessRejectionLimit
		if decision.Block {
			decision.Outcome = completionCleanlinessEscalated
			decision.Reason = dirtyCompletionEscalationReason
		}
		return decision
	}
	if !completionCleanlinessDirty(evidence) {
		if previousRejections > 0 && !completionCleanlinessResolutionValid(decision.Declaration) {
			decision.Outcome = completionCleanlinessRejected
			decision.Reason = dirtyCompletionResolutionReason
			decision.ConsecutiveRejections = previousRejections + 1
			decision.Block = decision.ConsecutiveRejections >= completionCleanlinessRejectionLimit
			if decision.Block {
				decision.Outcome = completionCleanlinessEscalated
				decision.Reason = dirtyCompletionEscalationReason
			}
			return decision
		}
		decision.Outcome = completionCleanlinessAccepted
		decision.Resolution = completionCleanlinessClean
		if previousRejections > 0 {
			decision.Resolution = decision.Declaration
		}
		return decision
	}
	decision.Outcome = completionCleanlinessRejected
	decision.Resolution = "required"
	decision.Reason = dirtyCompletionReason
	decision.ConsecutiveRejections = previousRejections + 1
	if decision.ConsecutiveRejections >= completionCleanlinessRejectionLimit {
		decision.Outcome = completionCleanlinessEscalated
		decision.Reason = dirtyCompletionEscalationReason
		decision.Block = true
	}
	return decision
}

func completionCleanlinessResolutionDeclaration(issue connector.Issue, running Running) string {
	signal := issue.WorkpadSignal
	if signal == nil {
		if current, ok := autoPromoteIssueWorkpadSignal(issue); ok {
			signal = current
		}
	}
	if !workpad.CurrentAttemptCompletion(signal, running.WorkAttemptID, running.Generation) {
		return ""
	}
	resolution := strings.ToLower(strings.TrimSpace(signal.Fields[workpad.FieldCompletionCleanlinessResolution]))
	resolution = strings.ReplaceAll(resolution, "-", "_")
	return strings.Join(strings.Fields(resolution), "_")
}

func completionCleanlinessResolutionValid(resolution string) bool {
	return resolution == completionCleanlinessCommitted || resolution == completionCleanlinessDiscarded
}

func completionCleanlinessAttempted(running Running, issue connector.Issue) bool {
	if strings.TrimSpace(running.CompletionLane) != "" {
		return true
	}
	signal := issue.WorkpadSignal
	if signal == nil {
		if current, ok := autoPromoteIssueWorkpadSignal(issue); ok {
			signal = current
		}
	}
	return workpad.CurrentAttemptCompletion(signal, running.WorkAttemptID, running.Generation)
}

func completionCleanlinessDirty(evidence DiffStats) bool {
	return evidence.FilesChanged > 0 || evidence.AddedLines > 0 || evidence.RemovedLines > 0 ||
		len(evidence.TrackedPaths) > 0 || len(evidence.UntrackedPaths) > 0 || evidence.UnpushedCommits > 0 ||
		evidence.PullRequestComparisonAvailable && len(evidence.CommitsNotInPullRequest) > 0
}

func completionCleanlinessIntentionalStatement(issue connector.Issue) string {
	signal := issue.WorkpadSignal
	if signal == nil {
		if current, ok := autoPromoteIssueWorkpadSignal(issue); ok {
			signal = current
		}
	}
	if signal == nil || signal.Source != workpad.SourceStructured || strings.TrimSpace(signal.Status) != workpad.StatusBlocked {
		return ""
	}
	statements := []string{strings.TrimSpace(signal.HumanAction)}
	for _, blocker := range signal.Blockers {
		statements = append(statements, strings.TrimSpace(blocker.Reason))
	}
	statements = slices.DeleteFunc(statements, func(value string) bool { return value == "" })
	return strings.Join(statements, "; ")
}

func consecutiveCompletionCleanlinessRejections(attempts []store.WorkAttempt) int {
	count := 0
	for _, attempt := range attempts {
		record, ok := completionCleanlinessRecordFromAttempt(attempt)
		if !ok || record.Outcome == completionCleanlinessAccepted {
			break
		}
		if record.Outcome != completionCleanlinessRejected && record.Outcome != completionCleanlinessEscalated {
			break
		}
		count++
	}
	return count
}

func completionCleanlinessRecordFromAttempt(attempt store.WorkAttempt) (completionCleanlinessRecord, bool) {
	var root struct {
		CompletionCleanliness completionCleanlinessRecord `json:"completion_cleanliness"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(attempt.WorkerMetadataJSON)), &root); err != nil || strings.TrimSpace(root.CompletionCleanliness.Outcome) == "" {
		return completionCleanlinessRecord{}, false
	}
	return root.CompletionCleanliness, true
}

func completionCleanlinessMetadata(decision completionCleanlinessDecision) map[string]any {
	if !decision.Attempted {
		return nil
	}
	return map[string]any{completionCleanlinessMetadataKey: completionCleanlinessRecord{
		Outcome:               decision.Outcome,
		Resolution:            decision.Resolution,
		Declaration:           decision.Declaration,
		Reason:                decision.Reason,
		Statement:             decision.Statement,
		Evidence:              completionCleanlinessEvidenceFromDiffStats(decision.Evidence),
		ConsecutiveRejections: decision.ConsecutiveRejections,
		Warning:               decision.Warning,
	}}
}

func completionCleanlinessEvidenceFromDiffStats(evidence DiffStats) completionCleanlinessEvidence {
	return completionCleanlinessEvidence{
		FilesChanged:            evidence.FilesChanged,
		AddedLines:              evidence.AddedLines,
		RemovedLines:            evidence.RemovedLines,
		TrackedPaths:            append([]string(nil), evidence.TrackedPaths...),
		UntrackedPaths:          append([]string(nil), evidence.UntrackedPaths...),
		UnpushedCommits:         evidence.UnpushedCommits,
		UnpushedCommitRefs:      append([]string(nil), evidence.UnpushedCommitRefs...),
		CommitsNotInPullRequest: append([]string(nil), evidence.CommitsNotInPullRequest...),
		RecoveryStateExpected:   evidence.RecoveryStateExpected,
		RecoveryStateAvailable:  evidence.RecoveryStateAvailable,
		HeadSHA:                 strings.TrimSpace(evidence.HeadSHA),
		Fingerprint:             strings.TrimSpace(evidence.Fingerprint),
		Status:                  strings.TrimSpace(evidence.Status),
	}
}

func (o *Orchestrator) commentCompletionCleanlinessRejection(ctx context.Context, issue connector.Issue, decision completionCleanlinessDecision) {
	if o == nil || o.connector == nil || strings.TrimSpace(issue.ID) == "" {
		return
	}
	if err := o.connector.CreateComment(ctx, issue.ID, completionCleanlinessRejectionComment(decision)); err != nil && o.logger != nil {
		o.logger.Warn("dirty completion rejection comment failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
	}
}

func completionCleanlinessRejectionComment(decision completionCleanlinessDecision) string {
	var b strings.Builder
	b.WriteString("Detent rejected this completion attempt because the workspace is not clean.\n\n")
	b.WriteString("- diffstat: ")
	b.WriteString(strconv.Itoa(decision.Evidence.FilesChanged))
	b.WriteString(" files, +")
	b.WriteString(strconv.Itoa(decision.Evidence.AddedLines))
	b.WriteString("/-")
	b.WriteString(strconv.Itoa(decision.Evidence.RemovedLines))
	if decision.Evidence.UnpushedCommits > 0 {
		b.WriteString("\n- unpushed_commits: ")
		b.WriteString(strconv.Itoa(decision.Evidence.UnpushedCommits))
	}
	appendQuotedEvidence(&b, "tracked_paths", decision.Evidence.TrackedPaths)
	appendQuotedEvidence(&b, "untracked_paths", decision.Evidence.UntrackedPaths)
	appendQuotedEvidence(&b, "unpushed_commit_refs", decision.Evidence.UnpushedCommitRefs)
	appendQuotedEvidence(&b, "commits_not_in_pull_request", decision.Evidence.CommitsNotInPullRequest)
	b.WriteString("\n\nInspect every path before the next completion attempt. Commit and publish work that belongs to the issue, discard stray artifacts or mistakes, or set the Workpad to `status: blocked` and state why files are intentionally left. For a clean retry, add `completion_cleanliness_resolution: committed` or `completion_cleanliness_resolution: discarded` under `fields:` in the current completion block. Detent will not accept `status: complete` while this evidence remains.")
	return b.String()
}

func (o *Orchestrator) blockCompletionCleanliness(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	decision completionCleanlinessDecision,
) bool {
	detail := fmt.Sprintf(
		"completion remained unresolved after %d rejection(s): %d files, +%d/-%d, %d unpushed commits, tracked paths %v, untracked paths %v",
		decision.ConsecutiveRejections,
		decision.Evidence.FilesChanged,
		decision.Evidence.AddedLines,
		decision.Evidence.RemovedLines,
		decision.Evidence.UnpushedCommits,
		decision.Evidence.TrackedPaths,
		decision.Evidence.UntrackedPaths,
	)
	humanAction := strings.TrimSpace(decision.Statement)
	if humanAction == "" {
		humanAction = "inspect the preserved workspace, commit and publish required work or discard stray changes, move the issue to Rework, then record completion_cleanliness_resolution as committed or discarded under `fields:` on completion"
	}
	event.Err = errors.New(detail)
	running.DiffStats = decision.Evidence
	return o.blockHumanOwnedWorkerFailure(
		ctx,
		state,
		event,
		running,
		dirtyCompletionEscalationReason,
		detail,
		humanAction,
		"worker_dirty_completion_escalated",
	)
}
