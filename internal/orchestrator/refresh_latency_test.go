package orchestrator

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/store/storetest"
)

// Replay the actual authorization and blocked owners with many excluded active
// cards and genuinely unresolved dependencies, using SQLite rather than a no-op
// evidence writer. Counters separate store time from provider fanout.
func BenchmarkRefreshMaintenance(b *testing.B) {
	backend := storetest.Open(b)
	metrics := &maintenanceStore{Store: backend}
	cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "latency"}, ActiveStates: []string{"In Progress"}, TerminalStates: []string{"Done"}, Authorization: selector.Selector{Labels: selector.Labels{Include: []string{"owned"}}}, DependencyAutoUnblock: DependencyAutoUnblockConfig{Enabled: true}})
	var excluded, blocked []connector.Issue
	for i := range 100 {
		excluded = append(excluded, connector.Issue{ID: fmt.Sprintf("excluded-%d", i), Identifier: fmt.Sprintf("owner/repo#%d", i+1), State: "In Progress"})
		issue := connector.Issue{ID: fmt.Sprintf("blocked-%d", i), Identifier: fmt.Sprintf("owner/repo#%d", i+101), State: "Blocked", Labels: []string{"owned"}, BlockedBy: []connector.BlockedRef{{ID: "dependency", Identifier: "owner/repo#1000", State: "In Progress", Source: connector.BlockedRefSourceNative}}, DependencySource: connector.BlockedRefSourceNative}
		blocked = append(blocked, issue)
		_, err := backend.RecordWorkflowPhaseEvent(b.Context(), store.WorkflowPhaseEvent{ProjectID: "latency", IssueID: issue.ID, Identifier: issue.Identifier, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "Blocked", Status: "entered", StartedAt: time.Now().Add(-time.Hour)})
		if err != nil {
			b.Fatal(err)
		}
	}
	tracker := &autoPromoteTickConnector{stateIssues: blocked, resolvedIssues: append(cloneIssues(blocked), connector.Issue{ID: "dependency", Identifier: "owner/repo#1000", State: "In Progress"})}
	orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: metrics, workflowMetrics: metrics}
	for _, phase := range []string{"authorize", "retired", "dependencies"} {
		b.Run(phase, func(b *testing.B) {
			metrics.reads, metrics.writes, metrics.readTime, metrics.writeTime = 0, 0, 0, 0
			metrics.batches = 0
			tracker.fetchComments = nil
			tracker.fetchIdentifiers = nil
			cycles := 0
			b.ReportAllocs()
			for b.Loop() {
				state := newState(cfg)
				switch phase {
				case "authorize":
					previous := tickPreviousState{pipeline: excluded}
					orch.filterAuthorizedTickIssues(b.Context(), &state, tickFetchedIssues{candidates: excluded, status: excluded}, &previous, time.Now())
				case "retired":
					orch.operatorReturnRetiredParks(b.Context(), &state, blocked, time.Now())
				case "dependencies":
					orch.operatorClearClosedDependencies(b.Context(), &state, blocked, time.Now())
				}
				cycles++
			}
			b.ReportMetric(float64(metrics.reads)/float64(cycles), "timeline_reads/op")
			b.ReportMetric(float64(metrics.writes)/float64(cycles), "decision_writes/op")
			b.ReportMetric(float64(metrics.batches)/float64(cycles), "batch_commits/op")
			b.ReportMetric(float64(metrics.readTime.Nanoseconds())/float64(cycles), "store_read_ns/op")
			b.ReportMetric(float64(metrics.writeTime.Nanoseconds())/float64(cycles), "store_write_ns/op")
			b.ReportMetric(float64(len(tracker.fetchComments))/float64(cycles), "comment_calls/op")
			b.ReportMetric(float64(len(tracker.fetchIdentifiers))/float64(cycles), "identity_calls/op")
		})
	}
}

type maintenanceStore struct {
	store.Store
	batches             int
	reads, writes       int
	readTime, writeTime time.Duration
}

func (s *maintenanceStore) IssueWorkflowTimeline(ctx context.Context, id store.IssueIdentity) (store.WorkflowTimeline, error) {
	start := time.Now()
	s.reads++
	v, e := s.Store.IssueWorkflowTimeline(ctx, id)
	s.readTime += time.Since(start)
	return v, e
}
func (s *maintenanceStore) RecordSchedulerDecision(ctx context.Context, d store.SchedulerDecision) (int64, error) {
	start := time.Now()
	s.writes++
	v, e := s.Store.RecordSchedulerDecision(ctx, d)
	s.writeTime += time.Since(start)
	return v, e
}

func (s *maintenanceStore) RecordSchedulerDecisions(ctx context.Context, d []store.SchedulerDecision) ([]int64, error) {
	start := time.Now()
	s.batches++
	s.writes += len(d)
	ids, e := s.Store.(store.SchedulerDecisionBatchStore).RecordSchedulerDecisions(ctx, d)
	s.writeTime += time.Since(start)
	return ids, e
}

// Catch per-card autocommit regression and lost refusals beyond the bounded live
// snapshot; every exclusion must still have durable per-issue evidence.
func TestAuthorizationBatchRetainsEveryRefusal(t *testing.T) {
	backend := storetest.Open(t)
	evidence := &maintenanceStore{Store: backend}
	cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "latency"}, ActiveStates: []string{"In Progress"}, Authorization: selector.Selector{Labels: selector.Labels{Include: []string{"owned"}}}})
	state := newState(cfg)
	var issues []connector.Issue
	for i := range 300 {
		issues = append(issues, connector.Issue{ID: fmt.Sprintf("excluded-%d", i), State: "In Progress"})
	}
	previous := tickPreviousState{pipeline: issues}
	orch := &Orchestrator{cfg: cfg, workAttempts: evidence}
	got := orch.filterAuthorizedTickIssues(t.Context(), &state, tickFetchedIssues{candidates: issues, status: issues}, &previous, time.Now())
	if len(got.candidates) != 0 || len(got.status) != 0 || len(previous.pipeline) != 0 || evidence.batches != 1 || evidence.writes != 300 {
		t.Fatalf("authorization batch: candidates=%d status=%d retained=%d batches=%d writes=%d", len(got.candidates), len(got.status), len(previous.pipeline), evidence.batches, evidence.writes)
	}
	reader := backend.(store.IssueSchedulerDecisionStore)
	for _, issue := range issues {
		rows, err := reader.ListIssueSchedulerDecisions(t.Context(), store.IssueSchedulerDecisionQuery{Identity: store.IssueIdentity{ProjectID: "latency", IssueID: issue.ID}})
		if err != nil || len(rows) != 1 || rows[0].Reason != dispatchSkipAuthorizationSelector || rows[0].MetadataJSON == "{}" {
			t.Fatalf("lost refusal for %s: %v %v", issue.ID, rows, err)
		}
	}
}

// Hold the actual dependency provider call indefinitely. Eligible unrelated
// implementation must already have been admitted when maintenance reaches it.
func TestTickDispatchPrecedesBlockedMaintenance(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(fmt.Sprintf("priority_%t", early), func(t *testing.T) {
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "latency"}, ActiveStates: []string{"In Progress"}, TerminalStates: []string{"Done"}, MaxConcurrentAgents: 1, DependencyAutoUnblock: DependencyAutoUnblockConfig{Enabled: true}})
			ready := connector.Issue{ID: "ready", AssignedToWorker: true, Title: "Approved implementation", Identifier: "owner/repo#1", State: "In Progress"}
			dependent := connector.Issue{ID: "dependent", AssignedToWorker: true, Title: "Unresolved implementation", Identifier: "owner/repo#2", State: "In Progress", BlockedBy: []connector.BlockedRef{{ID: "dependency", Identifier: "owner/repo#1000", State: "In Progress", Source: connector.BlockedRefSourceNative}}}
			issues := []connector.Issue{ready, dependent}
			for i := range 100 {
				issues = append(issues, connector.Issue{ID: fmt.Sprintf("blocked-%03d", i), Identifier: fmt.Sprintf("owner/repo#%d", i+100), State: "Blocked", BlockedBy: dependent.BlockedBy, DependencySource: connector.BlockedRefSourceNative})
			}
			base := &autoPromoteTickConnector{stateIssues: issues, resolvedIssues: append(cloneIssues(issues), connector.Issue{ID: "dependency", Identifier: "owner/repo#1000", State: "In Progress"})}
			state := newState(cfg)
			state.BoardIssues = cloneIssues(issues)
			state.dependencyUnblockEarly = early
			held := make(chan struct{})
			release := make(chan struct{})
			finished := make(chan struct{})
			tracker := &heldMaintenanceConnector{autoPromoteTickConnector: base, entered: held, release: release, observe: func() {
				if _, ok := state.Running[ready.ID]; !ok {
					t.Error("approved implementation was still waiting when full blocked maintenance began")
				}
				if _, ok := state.Running[dependent.ID]; ok {
					t.Error("unresolved dependent dispatched")
				}
			}, priority: early}
			runner := newWorkerHostRunner()
			orch := &Orchestrator{cfg: cfg, connector: tracker, supervisor: newTestSupervisor(t, runner, cfg), runResults: make(chan runpkg.Completion, 2)}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go func() { defer close(finished); orch.tick(ctx, &state, time.Now()) }()
			select {
			case <-held:
			case <-time.After(10 * time.Second):
				t.Fatal("maintenance did not reach held provider")
			}
			request := receiveWorkerHostRunRequest(t, runner.started)
			if request.Issue.ID != ready.ID {
				t.Fatalf("dispatched %s", request.Issue.ID)
			}
			close(release)
			select {
			case <-finished:
			case <-time.After(10 * time.Second):
				t.Fatal("tick did not finish")
			}
			cancel()
			if len(base.updates) != 0 {
				t.Fatalf("unresolved cards advanced: %v", base.updates)
			}
		})
	}
}

type heldMaintenanceConnector struct {
	*autoPromoteTickConnector
	entered  chan struct{}
	release  <-chan struct{}
	observe  func()
	priority bool
	held     bool
}

func (c *heldMaintenanceConnector) FetchIssueStatesByIdentifiers(ctx context.Context, refs []string) ([]connector.Issue, error) {
	// The one pre-fetch priority recovery may look up its own issue and dependency.
	// All other blocked-card self hydration belongs to the full maintenance pass.
	if len(refs) == 1 && refs[0] != "owner/repo#1000" && !c.held {
		if c.priority {
			c.priority = false
		} else {
			c.held = true
			c.observe()
			close(c.entered)
			select {
			case <-c.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return c.autoPromoteTickConnector.FetchIssueStatesByIdentifiers(ctx, refs)
}

// Catch admission using an active candidate when the same fresh status batch
// says Blocked, and stale project-status evidence delaying a released candidate.
func TestDispatchCandidateStatusBeforeMaintenance(t *testing.T) {
	for _, blocked := range []bool{true, false} {
		t.Run(fmt.Sprintf("blocked_%t", blocked), func(t *testing.T) {
			cfg := normalizeConfig(Config{ActiveStates: []string{"In Progress"}, TerminalStates: []string{"Done"}, MaxConcurrentAgents: 1})
			issue := dispatchTestIssue("ready", "In Progress")
			issue.AssignedToWorker = true
			status := cloneIssue(issue)
			if blocked {
				status.State = "Blocked"
			}
			state := newState(cfg)
			state.Blocked[issue.ID] = Blocked{Issue: issue, Source: BlockedSourceProjectStatus, Reason: "blocked by project status"}
			runner := newWorkerHostRunner()
			orch := &Orchestrator{cfg: cfg, connector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}, supervisor: newTestSupervisor(t, runner, cfg), runResults: make(chan runpkg.Completion, 1)}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			orch.dispatchTickIssues(ctx, &state, tickFetchedIssues{candidates: []connector.Issue{issue}, status: []connector.Issue{status}, statusOK: true}, tickTransitionRefresh{blockedRefreshOK: true}, tickPreviousState{}, nil, time.Now(), nil)
			_, running := state.Running[issue.ID]
			if running == blocked {
				t.Fatalf("running=%t with current blocked status=%t; decisions=%+v", running, blocked, state.SchedulerDecisions)
			}
		})
	}
}
