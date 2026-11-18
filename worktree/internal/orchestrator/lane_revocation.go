package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/procgroup"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (o *Orchestrator) reapRunningWorker(
	ctx context.Context,
	running Running,
	identity procgroup.Identity,
	reason string,
) (procgroup.TerminationOutcome, procgroup.Identity, error) {
	found := identity.PID > 0
	if !found {
		var err error
		identity, found, err = o.persistedWorkerProcess(ctx, running)
		if err != nil {
			return "", procgroup.Identity{}, fmt.Errorf("load persisted identity: %w", err)
		}
	}
	if !found {
		return procgroup.TerminationOutcomeAlreadyExited, procgroup.Identity{}, nil
	}
	reap := o.reapWorkerProcess
	if reap == nil {
		reap = procgroup.Terminate
	}
	outcome, err := reap(context.WithoutCancel(ctx), identity, o.workerReapGrace)
	if err != nil {
		return "", identity, err
	}
	if outcome == procgroup.TerminationOutcomeStaleIdentity {
		return outcome, identity, nil
	}
	if o.workerProcesses != nil && running.DetentSessionID > 0 {
		if err := o.workerProcesses.MarkSessionWorkerProcessReaped(context.WithoutCancel(ctx), running.DetentSessionID, store.WorkerProcessReap{
			ReapedAt: o.clockNow().UTC(),
			Outcome:  string(outcome),
			Reason:   strings.TrimSpace(reason),
		}); err != nil {
			return outcome, identity, fmt.Errorf("persist reap outcome: %w", err)
		}
	}
	return outcome, identity, nil
}

func completionMatchesRunning(event runpkg.Completion, running Running) bool {
	if event.Request.Generation > 0 && running.Generation > 0 && event.Request.Generation != running.Generation {
		return false
	}
	if event.Request.WorkAttemptID > 0 && running.WorkAttemptID > 0 && event.Request.WorkAttemptID != running.WorkAttemptID {
		return false
	}
	return true
}

func (o *Orchestrator) refreshCompletionLane(ctx context.Context, running Running) (connector.Issue, error) {
	if o == nil || o.connector == nil {
		return cloneIssue(running.Issue), nil
	}
	issues, err := o.connector.FetchIssueStatesByIDs(ctx, []string{running.Issue.ID})
	if err != nil {
		return connector.Issue{}, err
	}
	for _, issue := range issues {
		if strings.TrimSpace(issue.ID) == strings.TrimSpace(running.Issue.ID) {
			if strings.TrimSpace(issue.State) == "" {
				return connector.Issue{}, fmt.Errorf("issue %s has no lane in completion fence", issueLabel(running.Issue))
			}
			return mergeIssueTrackerFields(running.Issue, issue), nil
		}
	}
	return connector.Issue{}, fmt.Errorf("issue %s was not returned by completion fence", issueLabel(running.Issue))
}

func (o *Orchestrator) rejectWorkerCompletion(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	reason string,
	err error,
) {
	at := event.CompletedAt.UTC()
	if at.IsZero() {
		at = o.clockNow().UTC()
	}
	message := "rejected stale completion for " + issueLabel(event.Request.Issue) + ": " + reason
	if strings.TrimSpace(event.Request.Issue.ID) == "" {
		message = "rejected stale completion for " + issueLabel(running.Issue) + ": " + reason
	}
	if err != nil {
		message += ": " + err.Error()
	}
	recordStateEvent(state, telemetry.ActivityEvent{At: at, Event: "stale_worker_completion_rejected", Message: message})
	if o.logger != nil {
		o.logger.Warn(
			"stale worker completion rejected",
			"project_id", strings.TrimSpace(o.cfg.Project.ID),
			"issue_id", event.IssueID,
			"generation", event.Request.Generation,
			"current_generation", running.Generation,
			"work_attempt_id", event.Request.WorkAttemptID,
			"current_work_attempt_id", running.WorkAttemptID,
			"reason", reason,
			"error", err,
		)
	}
	if o.workflowMetrics != nil {
		_, recordErr := o.workflowMetrics.RecordWorkflowPhaseEvent(context.WithoutCancel(ctx), store.WorkflowPhaseEvent{
			ProjectID:  strings.TrimSpace(o.cfg.Project.ID),
			SessionID:  running.DetentSessionID,
			IssueID:    event.IssueID,
			Identifier: running.Issue.Identifier,
			PhaseType:  store.WorkflowPhaseTypeRecovery,
			PhaseName:  "stale_completion_rejected",
			Reason:     reason,
			Status:     "rejected",
			StartedAt:  at,
			FinishedAt: at,
			MetadataJSON: marshalWorkAttemptJSON(map[string]any{
				"generation":              event.Request.Generation,
				"current_generation":      running.Generation,
				"work_attempt_id":         event.Request.WorkAttemptID,
				"current_work_attempt_id": running.WorkAttemptID,
				"error":                   errorString(err),
			}),
		})
		if recordErr != nil && o.logger != nil {
			o.logger.Warn("stale completion audit persistence failed", "issue_id", event.IssueID, "error", recordErr)
		}
	}
}
