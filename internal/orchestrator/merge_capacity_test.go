package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

func TestReadyMergeAtWorkerCapacity(t *testing.T) {
	t.Parallel()
	for _, workers := range []int{0, 10} {
		t.Run(fmt.Sprintf("validation_waiters_%d", workers), func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 9, 2, 19, 35, 0, time.UTC)
			cfg := normalizeConfig(Config{
				MaxConcurrentAgents: 10, MergeFastPathEnabled: true,
				ActiveStates: []string{"Todo", "In Progress", "Merging", "Rework"}, TerminalStates: []string{"Done"},
				Project: scheduler.ProjectCandidate{ID: "detent", Weight: 1},
			})
			state := providerWindowState(cfg, workers)
			for id, running := range state.Running {
				running.LastEvent = "validation_waiting"
				state.Running[id] = running
			}
			issue := readyMergeCapacityIssue("ready", 2371)
			tracker := &contextCheckedMergeConnector{autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}}}
			global := scheduler.NewRoundRobin(scheduler.Config{Capacity: 1})
			globalGate := scheduler.NewGlobalDispatchGate(global)
			_, acquired, err := globalGate.TryAcquire(t.Context(), cfg.Project, scheduler.SlotRequest{State: "In Progress"}, now)
			if err != nil || !acquired {
				t.Fatalf("fill global capacity: acquired=%t err=%v", acquired, err)
			}
			orch := &Orchestrator{cfg: cfg, connector: tracker, globalDispatchGate: globalGate, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now)
			if len(tracker.merges) != 1 {
				t.Fatalf("merges = %v, want one ready merge at full global capacity with %d validation waiters", tracker.merges, workers)
			}
			if pool := globalGate.PoolSnapshot(); pool.Used != 1 {
				t.Fatalf("global pool used=%d, want unchanged validation worker", pool.Used)
			}
			if len(state.Running) != workers || len(state.Claimed) != 0 {
				t.Fatalf("running=%d claimed=%d, want unchanged workers and released merge claim", len(state.Running), len(state.Claimed))
			}
		})
	}
}

func readyMergeCapacityIssue(id string, number int) connector.Issue {
	issue := dispatchTestIssue(id, "Merging")
	issue.Identifier = fmt.Sprintf("digitaldrywood/detent#%d", number)
	issue.PRRepository = "digitaldrywood/detent"
	issue.PullRequest = &connector.PullRequest{
		Number: number, URL: fmt.Sprintf("https://github.test/digitaldrywood/detent/pull/%d", number),
		State: "open", MergeableState: "clean", CIStatus: "success", HeadSHA: "checked-head", BaseSHA: "checked-base",
	}
	return issue
}

func TestReadyMergeCapacitySafety(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*connector.Issue)
		setup  func(*State, connector.Issue)
		fresh  bool
	}{
		{name: "stale head", fresh: true, mutate: func(i *connector.Issue) { i.PullRequest.HeadSHA = "new-head" }},
		{name: "stale base", fresh: true, mutate: func(i *connector.Issue) { i.PullRequest.BaseSHA = "new-base" }},
		{name: "fresh CI pending", fresh: true, mutate: func(i *connector.Issue) { i.PullRequest.CIStatus = "pending" }},
		{name: "fresh review thread", fresh: true, mutate: func(i *connector.Issue) {
			i.PullRequest.UnresolvedReviewThreads = []connector.PullRequestReviewThread{{}}
		}},
		{name: "human withdrew merging", fresh: true, mutate: func(i *connector.Issue) { i.State = "Human Review" }},
		{name: "draft", mutate: func(i *connector.Issue) { i.PullRequest.Draft = true }},
		{name: "unknown base", mutate: func(i *connector.Issue) { i.PullRequest.BaseSHA = "" }},
		{name: "CI failed", mutate: func(i *connector.Issue) { i.PullRequest.CIStatus = "failure" }},
		{name: "behind", mutate: func(i *connector.Issue) { i.PullRequest.MergeableState = "behind" }},
		{name: "merging lane full", setup: func(s *State, _ connector.Issue) {
			running := s.Running["running-0"]
			running.Issue = readyMergeCapacityIssue("running-0", 2372)
			running.Issue.PRRepository = "digitaldrywood/other"
			running.Issue.PullRequest.URL = "https://github.test/digitaldrywood/other/pull/2372"
			s.Running["running-0"] = running
		}},
		{name: "provider hold", setup: func(s *State, _ connector.Issue) {
			scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
			s.BackendOutages[scope.Key()] = BackendOutage{Scope: scope, ResumeAt: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)}
		}},
		{name: "already claimed", setup: func(s *State, i connector.Issue) { s.Claimed[i.ID] = Claimed{Issue: i} }},
		{name: "human hold", setup: func(s *State, i connector.Issue) { s.Blocked[i.ID] = Blocked{Issue: i, Reason: "human hold"} }},
		{name: "fallback retry", setup: func(s *State, i connector.Issue) {
			s.Retry[i.ID] = Retry{Issue: i, DueAt: time.Time{}, MergePrecheck: &runpkg.MergePrecheck{Status: "conflict"}}
		}},
		{name: "base refresh required", setup: func(s *State, i connector.Issue) {
			r := reserveMergeCandidate(s, i, time.Date(2026, 9, 9, 2, 19, 35, 0, time.UTC))
			r.RefreshHeadSHA = i.PullRequest.HeadSHA
			s.mergeReservations[r.Repository] = r
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 9, 2, 19, 35, 0, time.UTC)
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, MergeFastPathEnabled: true, ActiveStates: []string{"Todo", "In Progress", "Merging", "Rework"}, TerminalStates: []string{"Done"}, AutoPromote: AutoPromoteConfig{Gate: gate.Config{Kind: gate.KindCommand}}})
			state := providerWindowState(cfg, 1)
			issue := readyMergeCapacityIssue("ready", 2371)
			fresh := cloneIssue(issue)
			if tt.mutate != nil {
				if tt.fresh {
					tt.mutate(&fresh)
				} else {
					tt.mutate(&issue)
					fresh = cloneIssue(issue)
				}
			}
			if tt.setup != nil {
				tt.setup(&state, issue)
			}
			tracker := &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}, hydratedIssues: []connector.Issue{fresh}}
			orch := &Orchestrator{cfg: cfg, connector: tracker, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			orch.capacityController = backendCapacityTestController{scope: backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}}
			orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now)
			if len(tracker.merges) != 0 {
				t.Fatalf("unsafe merge: %v", tracker.merges)
			}
			if len(state.Running) != 1 {
				t.Fatalf("workers=%d, want unchanged active worker", len(state.Running))
			}
			if tt.name == "stale head" || tt.name == "stale base" {
				if retry := state.Retry[issue.ID]; retry.Wait.Kind != retryWaitCurrentHeadCI || !sameMergeControlRevision(retry.Issue, fresh) {
					t.Fatalf("retry=%+v, want CI wait for the fresh revision", retry)
				}
				tracker.stateIssues = []connector.Issue{fresh}
				orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{fresh}, now.Add(time.Minute))
				if len(tracker.merges) != 1 || tracker.merges[0].headSHA != fresh.PullRequest.HeadSHA {
					t.Fatalf("merges=%v, want freshly validated head after retry", tracker.merges)
				}
			}
		})
	}
}

func TestReadyMergeCapacityOrdering(t *testing.T) {
	t.Parallel()
	for _, reserved := range []bool{false, true} {
		t.Run(fmt.Sprintf("reservation_%t", reserved), func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 9, 2, 19, 35, 0, time.UTC)
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, MergeFastPathEnabled: true, MergeFairnessAge: time.Hour, ActiveStates: []string{"Todo", "In Progress", "Merging"}, TerminalStates: []string{"Done"}})
			state := providerWindowState(cfg, 1)
			oldest := readyMergeCapacityIssue("oldest", 2371)
			oldest.StageUpdatedAt = timePointer(now.Add(-2 * time.Hour))
			newer := readyMergeCapacityIssue("newer", 2372)
			newer.StageUpdatedAt = timePointer(now.Add(-time.Minute))
			first, second := oldest, newer
			if reserved {
				reserveMergeCandidate(&state, newer, now)
				first, second = newer, oldest
			}
			tracker := &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{newer, oldest}}}
			orch := &Orchestrator{cfg: cfg, connector: tracker, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{newer, oldest, first}, now)
			if len(tracker.merges) != 1 || tracker.merges[0].number != first.PullRequest.Number {
				t.Fatalf("first pass merges=%v, want only PR %d", tracker.merges, first.PullRequest.Number)
			}
			tracker.stateIssues = []connector.Issue{second}
			orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{second}, now.Add(time.Minute))
			if len(tracker.merges) != 2 || tracker.merges[1].number != second.PullRequest.Number {
				t.Fatalf("second pass merges=%v, want PR %d next", tracker.merges, second.PullRequest.Number)
			}
		})
	}
}

func TestReadyMergeRespectsGlobalPause(t *testing.T) {
	t.Parallel()
	cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, MergeFastPathEnabled: true, ActiveStates: []string{"In Progress", "Merging"}, TerminalStates: []string{"Done"}, Project: scheduler.ProjectCandidate{ID: "detent", Weight: 1}})
	state := providerWindowState(cfg, 1)
	issue := readyMergeCapacityIssue("ready", 2371)
	tracker := &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}}
	global := scheduler.NewGlobalDispatchGate(scheduler.NewRoundRobin(scheduler.Config{Capacity: 1}))
	resume := global.PauseDispatch()
	defer resume()
	orch := &Orchestrator{cfg: cfg, connector: tracker, globalDispatchGate: global, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, time.Now())
	if len(tracker.merges) != 0 {
		t.Fatalf("merges=%v during global pause", tracker.merges)
	}
}

type contextCheckedMergeConnector struct {
	*autoPromoteTickMergeConnector
}

func (c *contextCheckedMergeConnector) MergePullRequest(ctx context.Context, repository string, number int, head, method string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("control merge has no deadline")
	}
	return c.autoPromoteTickMergeConnector.MergePullRequest(ctx, repository, number, head, method)
}

func TestReadyMergeControlBoundsIndependentRepositories(t *testing.T) {
	t.Parallel()
	for _, workers := range []int{0, 1} {
		t.Run(fmt.Sprintf("project_workers_%d", workers), func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 9, 2, 19, 35, 0, time.UTC)
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, MergeFastPathEnabled: true, ActiveStates: []string{"In Progress", "Merging"}, TerminalStates: []string{"Done"}, Project: scheduler.ProjectCandidate{ID: "detent", Weight: 1}})
			state := providerWindowState(cfg, workers)
			first := readyMergeCapacityIssue("first", 2371)
			second := readyMergeCapacityIssue("second", 2372)
			second.PRRepository = "digitaldrywood/other"
			second.PullRequest.URL = "https://github.test/digitaldrywood/other/pull/2372"
			tracker := &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{first, second}}}
			global := scheduler.NewGlobalDispatchGate(scheduler.NewRoundRobin(scheduler.Config{Capacity: 1}))
			if _, ok, err := global.TryAcquire(t.Context(), cfg.Project, scheduler.SlotRequest{State: "In Progress"}, now); !ok || err != nil {
				t.Fatalf("fill global capacity: ok=%t err=%v", ok, err)
			}
			orch := &Orchestrator{cfg: cfg, connector: tracker, globalDispatchGate: global, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{first, second}, now)
			if len(tracker.merges) != 1 {
				t.Fatalf("merges=%v, want one control operation across repositories", tracker.merges)
			}
			if len(state.Running) != workers || global.PoolSnapshot().Used != 1 {
				t.Fatal("control operation changed worker capacity")
			}
		})
	}
}

func TestCapacityWaitReasonsMatchDispatchStatus(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		workers int
		want    string
	}{
		{name: "project", workers: 1, want: scheduler.DecisionReasonProjectCapacityFull},
		{name: "global", workers: 0, want: scheduler.DispatchGateReasonGlobalCapacityFull},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 9, 2, 19, 35, 0, time.UTC)
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}, Project: scheduler.ProjectCandidate{ID: "detent", Weight: 1}})
			state := providerWindowState(cfg, tt.workers)
			issue := dispatchTestIssue("queued", "Todo")
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			global := scheduler.NewGlobalDispatchGate(scheduler.NewRoundRobin(scheduler.Config{Capacity: 1}))
			if _, ok, err := global.TryAcquire(t.Context(), cfg.Project, scheduler.SlotRequest{State: "In Progress"}, now); !ok || err != nil {
				t.Fatalf("fill global capacity: ok=%t err=%v", ok, err)
			}
			orch := &Orchestrator{cfg: cfg, connector: tracker, globalDispatchGate: global, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now)
			if status := state.DispatchStatus; status.WaitReasonCode != tt.want || status.WaitReason != tt.want {
				t.Fatalf("dispatch status=%+v, want matching %s code and reason", status, tt.want)
			}
		})
	}
}
