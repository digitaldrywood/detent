package orchestrator

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestStaleWorkerGenerationCannotCompleteFreshLease(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 18, 40, 0, 0, time.UTC)
	issue := laneRevocationIssue("issue-generation", "digitaldrywood/video-studio#23", "Production")
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	tracker := &runningStateConnector{issues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		Project:      scheduler.ProjectCandidate{ID: "video-studio"},
		ActiveStates: []string{"Todo", "Production", "Rework"},
	})
	orch := &Orchestrator{
		cfg:             cfg,
		connector:       tracker,
		workflowMetrics: metrics,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:             func() time.Time { return now },
	}
	state := newState(cfg)
	state.Running[issue.ID] = Running{Issue: issue, WorkAttemptID: 202, Generation: 2}

	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID: issue.ID,
		Request: runpkg.RunRequest{
			Issue:         issue,
			WorkAttemptID: 101,
			Generation:    1,
		},
		CompletedAt: now,
		Result:      runpkg.RunResult{FinalState: runpkg.FinalStateCompleted},
	})

	running, ok := state.Running[issue.ID]
	if !ok || running.Generation != 2 || running.WorkAttemptID != 202 {
		t.Fatalf("Running[%q] = %#v, want fresh generation 2 attempt 202", issue.ID, running)
	}
	if len(tracker.updates) != 0 || len(tracker.setFieldCalls) != 0 {
		t.Fatalf("tracker writes = updates %#v fields %#v, want none", tracker.updates, tracker.setFieldCalls)
	}
	events := metrics.snapshot()
	if len(events) != 1 || events[0].PhaseName != "stale_completion_rejected" || events[0].Status != "rejected" {
		t.Fatalf("workflow events = %#v, want stale completion rejection audit", events)
	}
}

func TestCompletionAfterLeaseCleanupRecordsRejection(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 18, 42, 0, 0, time.UTC)
	issue := laneRevocationIssue("issue-cleaned-lease", "digitaldrywood/video-studio#23", "Production")
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	cfg := normalizeConfig(Config{
		Project:      scheduler.ProjectCandidate{ID: "video-studio"},
		ActiveStates: []string{"Todo", "Production", "Rework"},
	})
	orch := &Orchestrator{
		cfg:             cfg,
		workflowMetrics: metrics,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:             func() time.Time { return now },
	}
	state := newState(cfg)

	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID: issue.ID,
		Request: runpkg.RunRequest{
			Issue:         issue,
			WorkAttemptID: 101,
			Generation:    1,
		},
		CompletedAt: now,
	})

	if !hasLaneRevocationEvent(state.RecentEvents, "stale_worker_completion_rejected") {
		t.Fatalf("RecentEvents = %#v, want stale completion rejection", state.RecentEvents)
	}
	events := metrics.snapshot()
	if len(events) != 1 || events[0].PhaseName != "stale_completion_rejected" || events[0].Status != "rejected" {
		t.Fatalf("workflow events = %#v, want stale completion rejection audit", events)
	}
}

func laneRevocationIssue(id string, identifier string, state string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = identifier
	issue.Title = "Lane revocation test"
	issue.State = state
	return issue
}

func hasLaneRevocationEvent(events []telemetry.ActivityEvent, name string) bool {
	for _, event := range events {
		if event.Event == name {
			return true
		}
	}
	return false
}
