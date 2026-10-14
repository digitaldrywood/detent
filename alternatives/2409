package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	workspaceBranchHoldSchema     = 1
	workspaceBranchHoldErrorClass = "workspace_branch_held"
	retryWaitWorkspaceBranchHeld  = "workspace_branch_held"
)

type workspaceBranchHoldMetadata struct {
	Schema       int       `json:"schema"`
	Branch       string    `json:"branch"`
	WorktreePath string    `json:"worktree_path"`
	PRNumber     int       `json:"pr_number,omitempty"`
	DetectedAt   time.Time `json:"detected_at"`
	NextProbeAt  time.Time `json:"next_probe_at"`
}

func (o *Orchestrator) handleWorkspaceBranchHoldCompletion(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
) bool {
	var heldErr *runpkg.WorkspaceBranchHeldError
	if !errors.As(event.Err, &heldErr) || heldErr == nil {
		return false
	}
	detectedAt := event.CompletedAt.UTC()
	delay := max(event.RetryDelay, o.cfg.PollInterval)
	if delay <= 0 {
		delay = defaultOverloadRetryDelay
	}
	nextProbeAt := detectedAt.Add(delay)
	message := heldErr.Error()
	o.releaseTerminalAttemptClaim(ctx, state, running.Issue, event.CompletedAt)
	o.completeDurableWorkAttemptWithMetadata(
		ctx,
		state,
		running,
		event.CompletedAt,
		store.WorkAttemptTerminalCapacity,
		workspaceBranchHoldErrorClass,
		message,
		"waiting",
		message,
		map[string]any{"workspace_branch_hold": workspaceBranchHoldMetadata{
			Schema:       workspaceBranchHoldSchema,
			Branch:       strings.TrimSpace(heldErr.Branch),
			WorktreePath: strings.TrimSpace(heldErr.WorktreePath),
			PRNumber:     heldErr.PRNumber,
			DetectedAt:   detectedAt,
			NextProbeAt:  nextProbeAt,
		}},
	)
	if workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
		return true
	}
	if state.FailureBreaker.Active() && state.FailureBreaker.CanaryIssueID == running.Issue.ID {
		o.deferProjectFailureBreakerCanary(state, running.Issue.ID, event.CompletedAt, delay)
	}
	state.Retry[running.Issue.ID] = Retry{
		Issue:      cloneIssue(running.Issue),
		Attempt:    running.Attempt,
		DueAt:      nextProbeAt,
		Error:      message,
		WorkerHost: running.WorkerHost,
		Wait: RetryWait{
			Kind:                retryWaitWorkspaceBranchHeld,
			StartedAt:           detectedAt,
			WorkspaceBranch:     strings.TrimSpace(heldErr.Branch),
			WorkspaceHolderPath: strings.TrimSpace(heldErr.WorktreePath),
			WorkspacePRNumber:   heldErr.PRNumber,
		},
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      detectedAt,
		Event:   "workspace_branch_hold_waiting",
		Message: message,
	})
	return true
}

func (o *Orchestrator) pollWorkspaceBranchHold(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	retry Retry,
	now time.Time,
) (Retry, bool, string) {
	if retry.Wait.Kind != retryWaitWorkspaceBranchHeld {
		return retry, false, ""
	}
	if o.workspaceHoldInspector == nil {
		return retry, false, ""
	}
	probeIssue := cloneIssue(issue)
	if strings.TrimSpace(probeIssue.BranchName) == "" {
		probeIssue.BranchName = strings.TrimSpace(retry.Wait.WorkspaceBranch)
	}
	if probeIssue.PRNumber == nil && retry.Wait.WorkspacePRNumber > 0 {
		number := retry.Wait.WorkspacePRNumber
		probeIssue.PRNumber = &number
	}
	hold, err := o.workspaceHoldInspector.InspectWorkspaceBranchHold(ctx, probeIssue)
	retry.Issue = cloneIssue(issue)
	retry.Wait.PollCount++
	delay := o.cfg.PollInterval
	if delay <= 0 {
		delay = defaultOverloadRetryDelay
	}
	retry.DueAt = now.Add(delay)
	if err != nil {
		retry.Error = workspaceBranchHoldMessage(retry.Wait) + "; hold check failed: " + err.Error()
		state.Retry[issue.ID] = retry
		return retry, true, dispatchSkipWorkspaceBranchHeld
	}
	if hold.Held {
		if branch := strings.TrimSpace(hold.Branch); branch != "" {
			retry.Wait.WorkspaceBranch = branch
		}
		if path := strings.TrimSpace(hold.WorktreePath); path != "" {
			retry.Wait.WorkspaceHolderPath = path
		}
		if hold.PRNumber > 0 {
			retry.Wait.WorkspacePRNumber = hold.PRNumber
		}
		retry.Error = workspaceBranchHoldMessage(retry.Wait)
		state.Retry[issue.ID] = retry
		return retry, true, dispatchSkipWorkspaceBranchHeld
	}

	retry.Error = ""
	retry.Wait = RetryWait{}
	state.Retry[issue.ID] = retry
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now.UTC(),
		Event:   "workspace_branch_hold_released",
		Message: "branch hold released for " + issueLabel(issue) + "; dispatch will resume",
	})
	return retry, false, ""
}

func workspaceBranchHoldMessage(wait RetryWait) string {
	return (&runpkg.WorkspaceBranchHeldError{
		Branch:       strings.TrimSpace(wait.WorkspaceBranch),
		WorktreePath: strings.TrimSpace(wait.WorkspaceHolderPath),
		PRNumber:     wait.WorkspacePRNumber,
	}).Error()
}

func workspaceBranchHoldMetadataFromAttempt(attempt store.WorkAttempt) (workspaceBranchHoldMetadata, bool) {
	if attempt.Status != store.WorkAttemptStatusTerminal ||
		attempt.TerminalState != store.WorkAttemptTerminalCapacity ||
		strings.TrimSpace(attempt.ErrorClass) != workspaceBranchHoldErrorClass {
		return workspaceBranchHoldMetadata{}, false
	}
	var metadata struct {
		WorkspaceBranchHold workspaceBranchHoldMetadata `json:"workspace_branch_hold"`
	}
	if json.Unmarshal([]byte(attempt.WorkerMetadataJSON), &metadata) != nil {
		return workspaceBranchHoldMetadata{}, false
	}
	hold := metadata.WorkspaceBranchHold
	hold.Branch = strings.TrimSpace(hold.Branch)
	hold.WorktreePath = strings.TrimSpace(hold.WorktreePath)
	if hold.Schema != workspaceBranchHoldSchema || hold.Branch == "" || hold.WorktreePath == "" || hold.DetectedAt.IsZero() {
		return workspaceBranchHoldMetadata{}, false
	}
	return hold, true
}

func (o *Orchestrator) recoverWorkspaceBranchHolds(ctx context.Context, state *State, attempts []store.WorkAttempt, now time.Time) {
	if state == nil || len(attempts) == 0 {
		return
	}
	latest := latestStoreTerminalAttemptsByIssue(attempts)
	waits := make(map[string]workspaceBranchHoldMetadata)
	issueIDs := make([]string, 0, len(latest))
	for issueID, attempt := range latest {
		metadata, ok := workspaceBranchHoldMetadataFromAttempt(attempt)
		if !ok {
			continue
		}
		waits[issueID] = metadata
		issueIDs = append(issueIDs, issueID)
	}
	if len(waits) == 0 {
		return
	}
	issuesByID, validated := o.validateWorkspaceHoldIssues(ctx, issueIDs)
	for issueID, metadata := range waits {
		attempt := latest[issueID]
		issue := forgeWaitIssueFromAttempt(attempt)
		if validated {
			var ok bool
			issue, ok = issuesByID[issueID]
			if !ok || !stateIn(issue.State, o.cfg.ActiveStates) || workspaceIssueTerminal(issue, o.cfg.TerminalStates) {
				continue
			}
		}
		if strings.TrimSpace(issue.BranchName) == "" {
			issue.BranchName = metadata.Branch
		}
		nextProbeAt := metadata.NextProbeAt
		if nextProbeAt.IsZero() || nextProbeAt.Before(now) {
			nextProbeAt = now
		}
		state.Retry[issueID] = Retry{
			Issue:      cloneIssue(issue),
			Attempt:    attempt.AttemptNumber,
			DueAt:      nextProbeAt.UTC(),
			Error:      strings.TrimSpace(attempt.ErrorMessage),
			WorkerHost: strings.TrimSpace(attempt.WorkerHost),
			Wait: RetryWait{
				Kind:                retryWaitWorkspaceBranchHeld,
				StartedAt:           metadata.DetectedAt,
				WorkspaceBranch:     metadata.Branch,
				WorkspaceHolderPath: metadata.WorktreePath,
				WorkspacePRNumber:   metadata.PRNumber,
			},
		}
		recordStateEvent(state, telemetry.ActivityEvent{At: now.UTC(), Event: "workspace_branch_hold_restored", Message: "restored durable branch hold for " + issueLabel(issue)})
	}
}

func (o *Orchestrator) validateWorkspaceHoldIssues(ctx context.Context, issueIDs []string) (map[string]connector.Issue, bool) {
	if o == nil || o.connector == nil || len(issueIDs) == 0 {
		return nil, false
	}
	issues, err := o.connector.FetchIssueStatesByIDs(ctx, issueIDs)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("workspace branch hold issue validation failed; preserving durable waits", "issue_ids", issueIDs, "error", err)
		}
		return nil, false
	}
	byID := make(map[string]connector.Issue, len(issues))
	for _, issue := range issues {
		if issueID := strings.TrimSpace(issue.ID); issueID != "" {
			byID[issueID] = issue
		}
	}
	return byID, true
}
