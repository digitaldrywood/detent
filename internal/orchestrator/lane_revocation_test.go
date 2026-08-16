package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/procgroup"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestLaneRevocationPreservesTrackerDestination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		destination   string
		wantCompleted bool
	}{
		{name: "blocked lane", destination: "Blocked"},
		{name: "review lane", destination: "Review"},
		{name: "terminal lane", destination: "Done", wantCompleted: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 8, 16, 18, 35, 0, 0, time.UTC)
			issue := laneRevocationIssue("issue-22", "digitaldrywood/video-studio#22", "Production")
			parked := cloneIssue(issue)
			parked.State = tt.destination
			attempts := &recordingWorkAttemptStore{}
			tracker := &runningStateConnector{issues: []connector.Issue{parked}}
			cfg := normalizeConfig(Config{
				Project:        scheduler.ProjectCandidate{ID: "video-studio"},
				ActiveStates:   []string{"Todo", "Production", "Rework"},
				ObservedStates: []string{"Blocked", "Review"},
				TerminalStates: []string{"Done", "Cancelled"},
			})
			orch := &Orchestrator{
				cfg:                    cfg,
				connector:              tracker,
				workAttempts:           attempts,
				pendingLaneRevocations: map[string]*pendingLaneRevocation{},
				logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
				now:                    func() time.Time { return now },
			}
			state := newState(cfg)
			runCtx, stop := context.WithCancelCause(context.Background())
			state.Running[issue.ID] = Running{
				Issue:         issue,
				Attempt:       2,
				WorkAttemptID: 42,
				Generation:    7,
				StartedAt:     now.Add(-time.Hour),
				stop:          stop,
			}
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Hour)}
			state.Retry[issue.ID] = Retry{Issue: issue, Attempt: 3, DueAt: now.Add(time.Hour)}

			orch.reconcileRunningIssues(t.Context(), &state, now)

			if !errors.Is(context.Cause(runCtx), runpkg.ErrLaneRevoked) {
				t.Fatalf("context cause = %v, want ErrLaneRevoked", context.Cause(runCtx))
			}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID: issue.ID,
				Request: runpkg.RunRequest{
					Issue:         issue,
					WorkAttemptID: 42,
					Generation:    7,
				},
				CompletedAt: now.Add(time.Second),
				Err:         runpkg.ErrLaneRevoked,
				Result:      runpkg.RunResult{FinalState: runpkg.FinalStateLaneRevoked},
			})

			if _, ok := state.Running[issue.ID]; ok {
				t.Fatalf("Running[%q] present after lane revocation", issue.ID)
			}
			if _, ok := state.Claimed[issue.ID]; ok {
				t.Fatalf("Claimed[%q] present after lane revocation", issue.ID)
			}
			if _, ok := state.Retry[issue.ID]; ok {
				t.Fatalf("Retry[%q] present after lane revocation", issue.ID)
			}
			_, completed := state.Completed[issue.ID]
			if completed != tt.wantCompleted {
				t.Fatalf("Completed[%q] present = %v, want %v", issue.ID, completed, tt.wantCompleted)
			}
			if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalLaneRevoked {
				t.Fatalf("work attempt completions = %#v, want lane_revoked", attempts.completions)
			}
			if len(tracker.updates) != 0 || len(tracker.setFieldCalls) != 0 {
				t.Fatalf("tracker writes = updates %#v fields %#v, want none", tracker.updates, tracker.setFieldCalls)
			}
			if tracker.issues[0].State != tt.destination {
				t.Fatalf("tracker state = %q, want preserved %q", tracker.issues[0].State, tt.destination)
			}
		})
	}
}

func TestLaneChangeFinishRaceRejectsArtifactCompletion(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 18, 35, 0, 0, time.UTC)
	issue := laneRevocationIssue("issue-artifact-22", "digitaldrywood/video-studio#22", "Production")
	issue.Deliverable = &connector.Deliverable{Kind: "artifact"}
	issue.Fields = map[string]string{"render_status": "recut"}
	parked := cloneIssue(issue)
	parked.State = "Blocked"
	workpad := "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: complete\nfields:\n  render_status: pending_review\nblockers: []\nhuman_action: null\n```"
	tracker := &autoPromoteTickConnector{
		stateIssues: []connector.Issue{parked},
		issueComments: map[string][]connector.IssueComment{
			issue.ID: {{Body: workpad}},
		},
	}
	attempts := &recordingWorkAttemptStore{}
	cfg := normalizeConfig(Config{
		Project:        scheduler.ProjectCandidate{ID: "video-studio"},
		ActiveStates:   []string{"Todo", "Production", "Rework"},
		ObservedStates: []string{"Blocked", "Review"},
		TerminalStates: []string{"Done", "Cancelled"},
		AutoPromote: AutoPromoteConfig{
			Enabled:     true,
			SourceState: "Review",
			Gate:        artifactCompletionTestGate(),
		},
	})
	orch := &Orchestrator{
		cfg:                    cfg,
		connector:              tracker,
		workAttempts:           attempts,
		pendingLaneRevocations: map[string]*pendingLaneRevocation{},
		logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:                    func() time.Time { return now },
	}
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:               issue,
		Attempt:             1,
		WorkAttemptID:       44,
		Generation:          4,
		DispatchWorkpadRead: true,
		StartedAt:           now.Add(-time.Hour),
	}

	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID: issue.ID,
		Request: runpkg.RunRequest{
			Issue:         issue,
			WorkAttemptID: 44,
			Generation:    4,
		},
		CompletedAt: now,
		Result:      runpkg.RunResult{FinalState: runpkg.FinalStateCompleted},
	})

	if len(tracker.setFields) != 0 || len(tracker.updates) != 0 {
		t.Fatalf("tracker writes = fields %#v updates %#v, want none", tracker.setFields, tracker.updates)
	}
	if got := tracker.stateIssues[0].Fields["render_status"]; got != "recut" {
		t.Fatalf("render_status = %q, want recut", got)
	}
	if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalLaneRevoked {
		t.Fatalf("work attempt completions = %#v, want lane_revoked", attempts.completions)
	}
	if !hasLaneRevocationEvent(state.RecentEvents, "stale_worker_completion_rejected") {
		t.Fatalf("RecentEvents = %#v, want stale completion rejection", state.RecentEvents)
	}
}

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

func TestMovingBackToActiveLaneCreatesFreshGeneration(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 18, 45, 0, 0, time.UTC)
	issue := laneRevocationIssue("issue-reentry", "digitaldrywood/video-studio#24", "Production")
	tracker := &runningStateConnector{issues: []connector.Issue{issue}}
	runner := newWorkerHostRunner()
	cfg := normalizeConfig(Config{
		Project:             scheduler.ProjectCandidate{ID: "video-studio"},
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "Production", "Rework"},
		ObservedStates:      []string{"Blocked"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	orch, err := New(cfg, Dependencies{Connector: tracker, Runner: runner, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	state := newState(orch.cfg)
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if !orch.dispatchIssue(runCtx, &state, issue, 1, now, "") {
		t.Fatal("first dispatch = false, want true")
	}
	firstRequest := receiveWorkerHostRunRequest(t, runner.started)
	parked := cloneIssue(issue)
	parked.State = "Blocked"
	tracker.issues = []connector.Issue{parked}
	orch.reconcileRunningIssues(runCtx, &state, now.Add(time.Second))
	firstCompletion := receiveLaneRevocationCompletion(t, orch.runResults)
	orch.handleRunResult(runCtx, &state, firstCompletion)

	tracker.issues = []connector.Issue{issue}
	if !orch.dispatchIssue(runCtx, &state, issue, 2, now.Add(2*time.Second), "") {
		t.Fatal("second dispatch = false, want true")
	}
	secondRequest := receiveWorkerHostRunRequest(t, runner.started)
	if secondRequest.Generation <= firstRequest.Generation {
		t.Fatalf("second generation = %d, want greater than first generation %d", secondRequest.Generation, firstRequest.Generation)
	}

	orch.handleRunResult(runCtx, &state, firstCompletion)
	running, ok := state.Running[issue.ID]
	if !ok || running.Generation != secondRequest.Generation {
		t.Fatalf("Running[%q] = %#v, want fresh generation %d", issue.ID, running, secondRequest.Generation)
	}
	if !hasLaneRevocationEvent(state.RecentEvents, "stale_worker_completion_rejected") {
		t.Fatalf("RecentEvents = %#v, want stale completion rejection", state.RecentEvents)
	}

	if running.stop != nil {
		running.stop(runpkg.ErrLaneRevoked)
	}
	_ = receiveLaneRevocationCompletion(t, orch.runResults)
}

func TestLaneRevocationRecordsGraceTimeoutEscalation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 18, 50, 0, 0, time.UTC)
	issue := laneRevocationIssue("issue-escalation", "digitaldrywood/video-studio#25", "Production")
	parked := cloneIssue(issue)
	parked.State = "Blocked"
	identity := procgroup.Identity{PID: 2501, GroupID: 2501, StartedAt: now.Add(-time.Hour)}
	processes := &laneRevocationProcessStore{active: []store.WorkerProcess{{
		SessionID: 55,
		IssueID:   issue.ID,
		WorkerProcessIdentity: store.WorkerProcessIdentity{
			PID:       identity.PID,
			GroupID:   identity.GroupID,
			StartedAt: identity.StartedAt,
		},
	}}}
	cfg := normalizeConfig(Config{ActiveStates: []string{"Production"}})
	orch := &Orchestrator{
		cfg:                    cfg,
		connector:              &runningStateConnector{issues: []connector.Issue{parked}},
		workerProcesses:        processes,
		workerReapGrace:        20 * time.Millisecond,
		pendingLaneRevocations: map[string]*pendingLaneRevocation{},
		reapWorkerProcess: func(_ context.Context, got procgroup.Identity, grace time.Duration) (procgroup.TerminationOutcome, error) {
			if got != identity || grace != 20*time.Millisecond {
				t.Fatalf("reap arguments = %#v, %s, want %#v, 20ms", got, grace, identity)
			}
			return procgroup.TerminationOutcomeKilled, nil
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:    func() time.Time { return now },
	}
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:           issue,
		Generation:      5,
		DetentSessionID: 55,
		WorkerProcess:   identity,
	}

	orch.reconcileRunningIssues(t.Context(), &state, now)

	for _, event := range []string{"worker_lane_stop_requested", "worker_lane_stop_escalated", "worker_lane_stop_result"} {
		if !hasLaneRevocationEvent(state.RecentEvents, event) {
			t.Fatalf("RecentEvents = %#v, want %s", state.RecentEvents, event)
		}
	}
	if len(processes.reaped) != 1 || processes.reaped[0].Outcome != store.WorkerProcessOutcomeKilled {
		t.Fatalf("process reap records = %#v, want killed_after_timeout", processes.reaped)
	}
}

func TestReconcileRunningIssuesStopsVideoStudioIncidentWorkers(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 18, 35, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{ActiveStates: []string{"Production"}})
	state := newState(cfg)
	tracker := &runningStateConnector{}
	orch := &Orchestrator{
		cfg:                    cfg,
		connector:              tracker,
		pendingLaneRevocations: map[string]*pendingLaneRevocation{},
		logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:                    func() time.Time { return now },
	}
	contexts := make([]context.Context, 0, 6)
	for number := 22; number <= 27; number++ {
		id := "wi-video-studio-" + strconv.Itoa(number)
		if number == 22 {
			id = "wi-0c7d736611a111641bd57b97"
		}
		issue := laneRevocationIssue(id, "digitaldrywood/video-studio#"+strconv.Itoa(number), "Production")
		parked := cloneIssue(issue)
		parked.State = "Blocked"
		tracker.issues = append(tracker.issues, parked)
		runCtx, stop := context.WithCancelCause(context.Background())
		contexts = append(contexts, runCtx)
		state.Running[id] = Running{Issue: issue, Generation: uint64(number), stop: stop}
	}

	orch.reconcileRunningIssues(t.Context(), &state, now)

	for index, runCtx := range contexts {
		if !errors.Is(context.Cause(runCtx), runpkg.ErrLaneRevoked) {
			t.Fatalf("worker #%d context cause = %v, want ErrLaneRevoked", index+22, context.Cause(runCtx))
		}
	}
	if len(orch.pendingLaneRevocations) != 6 {
		t.Fatalf("pending lane revocations = %d, want 6", len(orch.pendingLaneRevocations))
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

func receiveLaneRevocationCompletion(t *testing.T, completions <-chan runpkg.Completion) runpkg.Completion {
	t.Helper()

	select {
	case completion := <-completions:
		return completion
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for worker completion")
		return runpkg.Completion{}
	}
}

func hasLaneRevocationEvent(events []telemetry.ActivityEvent, name string) bool {
	for _, event := range events {
		if event.Event == name {
			return true
		}
	}
	return false
}

type laneRevocationProcessStore struct {
	active []store.WorkerProcess
	reaped []store.WorkerProcessReap
}

func (s *laneRevocationProcessStore) ListActiveWorkerProcesses(context.Context) ([]store.WorkerProcess, error) {
	return append([]store.WorkerProcess(nil), s.active...), nil
}

func (s *laneRevocationProcessStore) MarkSessionWorkerProcessReaped(_ context.Context, _ int64, reap store.WorkerProcessReap) error {
	s.reaped = append(s.reaped, reap)
	return nil
}
