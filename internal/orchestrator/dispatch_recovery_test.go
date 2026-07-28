package orchestrator

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

func TestDispatchRecoveryRampBoundsDispatchUntilProgress(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{MaxConcurrentAgents: 4, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}})
	orch := &Orchestrator{cfg: cfg}
	state := newState(cfg)
	orch.activateDispatchRecovery(&state, dispatchRecoveryGitHubREST, "REST quota recovered", now, "")

	issues := []connector.Issue{
		dispatchTestIssue("issue-1", "Todo"),
		dispatchTestIssue("issue-2", "Todo"),
		dispatchTestIssue("issue-3", "Todo"),
	}
	dispatched := []string{}
	hooks := dispatchPlanHooks{dispatch: func(issue connector.Issue, _ int, _ string) bool {
		_, allowed, _ := tryReserveDispatchRecovery(&state, issue.ID, now)
		if allowed {
			dispatched = append(dispatched, issue.ID)
		}
		return allowed
	}}
	orch.dispatchPlanner().plan(&state, issues, now, hooks)

	if len(dispatched) != 1 || dispatched[0] != "issue-1" {
		t.Fatalf("dispatched = %#v, want only issue-1 canary", dispatched)
	}
	if reason := dispatchRecoveryBlockReason(&state, now); reason != "github_rest_recovery" {
		t.Fatalf("dispatchRecoveryBlockReason() = %q, want github_rest_recovery", reason)
	}

	orch.advanceDispatchRecovery(&state, "issue-1", now.Add(time.Second))
	if _, allowed, reason := tryReserveDispatchRecovery(&state, "issue-2", now.Add(time.Second)); !allowed || reason != "" {
		t.Fatalf("second admission = %v, %q, want allowed", allowed, reason)
	}
	if _, allowed, reason := tryReserveDispatchRecovery(&state, "issue-3", now.Add(time.Second)); allowed || reason != "github_rest_recovery" {
		t.Fatalf("third admission = %v, %q, want bounded recovery", allowed, reason)
	}

	snapshot := state.Snapshot(now.Add(time.Second))
	if len(snapshot.DispatchRecoveries) != 1 {
		t.Fatalf("DispatchRecoveries = %#v, want one", snapshot.DispatchRecoveries)
	}
	recovery := snapshot.DispatchRecoveries[0]
	if recovery.Kind != dispatchRecoveryGitHubREST || recovery.Status != dispatchRecoveryStatusRamping || recovery.Limit != 2 || recovery.MaxConcurrent != 4 || recovery.Admitted != 2 || recovery.Progressed != 1 {
		t.Fatalf("DispatchRecoveries[0] = %#v, want bounded REST ramp telemetry", recovery)
	}

	var capacity map[string]any
	if err := json.Unmarshal([]byte(orch.capacitySnapshotJSON(&state, issues[0])), &capacity); err != nil {
		t.Fatalf("capacitySnapshotJSON() error = %v", err)
	}
	if recoveries, ok := capacity["dispatch_recoveries"].([]any); !ok || len(recoveries) != 1 {
		t.Fatalf("dispatch_recoveries = %#v, want one durable recovery record", capacity["dispatch_recoveries"])
	}
}

func TestDispatchRecoveryTelemetryUsesAgentPoolCapacity(t *testing.T) {
	t.Parallel()

	project := scheduler.ProjectCandidate{ID: "detent", Pool: "video"}
	gate, err := scheduler.NewPoolRegistry([]scheduler.PoolConfig{
		{Name: scheduler.DefaultPoolName, Scheduler: scheduler.Config{Kind: "weighted", Capacity: 1}},
		{Name: "video", BurstTo: 8, Scheduler: scheduler.Config{Kind: "weighted", Capacity: 5}},
	}, []scheduler.ProjectCandidate{project})
	if err != nil {
		t.Fatalf("NewPoolRegistry() error = %v", err)
	}
	projectGate := gate.GateFor(project.ID)
	slots := make([]scheduler.Slot, 0, 6)
	for range 6 {
		slot, acquired, err := projectGate.TryAcquire(
			context.Background(),
			project,
			scheduler.SlotRequest{State: "Todo"},
			time.Now(),
		)
		if err != nil {
			t.Fatalf("TryAcquire() error = %v", err)
		}
		if !acquired {
			t.Fatal("TryAcquire() acquired = false, want true")
		}
		slots = append(slots, slot)
	}
	t.Cleanup(func() {
		for _, slot := range slots {
			if err := projectGate.Release(slot); err != nil {
				t.Fatalf("Release() error = %v", err)
			}
		}
	})
	cfg := normalizeConfig(Config{MaxConcurrentAgents: 2, Project: project})
	orch := &Orchestrator{cfg: cfg, globalDispatchGate: gate}
	state := newState(cfg)
	pool := orch.dispatchPoolSnapshot()
	state.PoolName = pool.Name
	state.PoolCapacity = pool.Capacity
	orch.activateDispatchRecovery(&state, dispatchRecoveryGitHubREST, "REST quota recovered", time.Now(), "")

	snapshot := state.Snapshot(time.Now())
	if len(snapshot.DispatchRecoveries) != 1 {
		t.Fatalf("DispatchRecoveries = %#v, want one", snapshot.DispatchRecoveries)
	}
	recovery := snapshot.DispatchRecoveries[0]
	if recovery.Pool != "video" || recovery.MaxConcurrent != 8 {
		t.Fatalf("DispatchRecoveries[0] = %#v, want video pool burst capacity 8", recovery)
	}

	var capacity map[string]any
	if err := json.Unmarshal([]byte(orch.capacitySnapshotJSON(&state, connector.Issue{State: "Todo"})), &capacity); err != nil {
		t.Fatalf("capacitySnapshotJSON() error = %v", err)
	}
	if capacity["pool"] != "video" || capacity["pool_capacity"] != float64(8) ||
		capacity["pool_guaranteed"] != float64(5) ||
		capacity["pool_burst_to"] != float64(8) ||
		capacity["pool_borrowed"] != float64(1) {
		t.Fatalf("capacity snapshot = %#v, want video guarantee 5, burst 8, borrowed 1", capacity)
	}
	holders, ok := capacity["holders"].([]any)
	if !ok || len(holders) != 1 || holders[0] != "detent" {
		t.Fatalf("capacity holders = %#v, want detent", capacity["holders"])
	}
}

func TestPullRequestHydrationRecoveryWaitsThenAdmitsOne(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 15, 13, 0, 0, 0, time.UTC)
	retryAt := now.Add(2 * time.Minute)
	cfg := normalizeConfig(Config{MaxConcurrentAgents: 3})
	orch := &Orchestrator{cfg: cfg}
	state := newState(cfg)
	blocked := connector.Issue{
		ID:    "issue-pr",
		State: "Rework",
		PullRequest: &connector.PullRequest{
			Number:                     42,
			State:                      "OPEN",
			HydrationUnavailableReason: "rest_budget_reserved",
			HydrationNextRetryAt:       &retryAt,
		},
	}

	orch.observePullRequestHydrationRecovery(&state, []connector.Issue{blocked}, now)
	waiting := state.DispatchRecoveries[dispatchRecoveryPullRequestHydration]
	if waiting.Status != dispatchRecoveryStatusWaiting || waiting.Reason != "rest_budget_reserved" || !waiting.ResumeAt.Equal(retryAt) {
		t.Fatalf("hydration wait = %#v", waiting)
	}

	recovered := blocked
	recovered.PullRequest = &connector.PullRequest{Number: 42, State: "OPEN"}
	orch.observePullRequestHydrationRecovery(&state, []connector.Issue{recovered}, retryAt)
	if _, allowed, reason := tryReserveDispatchRecovery(&state, "issue-pr", retryAt); !allowed || reason != "" {
		t.Fatalf("canary admission = %v, %q, want allowed", allowed, reason)
	}
	if _, allowed, reason := tryReserveDispatchRecovery(&state, "issue-next", retryAt); allowed || reason != "pull_request_hydration_recovery" {
		t.Fatalf("second admission = %v, %q, want hydration recovery wait", allowed, reason)
	}
}

func TestDispatchRecoveryCanaryFailureStartsBoundedBackoff(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{MaxConcurrentAgents: 4, OverloadRetryDelay: 45 * time.Second})
	orch := &Orchestrator{cfg: cfg}
	state := newState(cfg)
	orch.activateDispatchRecovery(&state, dispatchRecoveryBackendCapacity, "backend recovered", now, "")
	if _, allowed, _ := tryReserveDispatchRecovery(&state, "issue-canary", now); !allowed {
		t.Fatal("initial canary admission denied")
	}

	orch.backoffDispatchRecovery(&state, "issue-canary", now, 45*time.Second)
	recovery := state.DispatchRecoveries[dispatchRecoveryBackendCapacity]
	if recovery.Limit != 1 || len(recovery.Admissions) != 0 || !recovery.ResumeAt.Equal(now.Add(45*time.Second)) {
		t.Fatalf("recovery after failure = %#v", recovery)
	}
	if _, allowed, reason := tryReserveDispatchRecovery(&state, "issue-other", now.Add(44*time.Second)); allowed || reason != "backend_capacity_recovery" {
		t.Fatalf("early admission = %v, %q, want bounded backoff", allowed, reason)
	}
	if _, allowed, reason := tryReserveDispatchRecovery(&state, "issue-other", now.Add(45*time.Second)); !allowed || reason != "" {
		t.Fatalf("retry admission = %v, %q, want one canary", allowed, reason)
	}
}

func TestDispatchRecoveryPollIntervalWakesAtKnownResumeTime(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 15, 19, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{PollInterval: 30 * time.Second, MaxConcurrentAgents: 4})
	orch := &Orchestrator{cfg: cfg}
	state := newState(cfg)
	orch.activateDispatchRecovery(&state, dispatchRecoveryBackendCapacity, "backend recovering", now, "")
	recovery := state.DispatchRecoveries[dispatchRecoveryBackendCapacity]
	recovery.ResumeAt = now.Add(45 * time.Second)
	state.DispatchRecoveries[dispatchRecoveryBackendCapacity] = recovery

	if got := orch.adaptivePollInterval(&state, now); got != 30*time.Second {
		t.Fatalf("initial poll interval = %s, want base 30s", got)
	}
	if got := orch.adaptivePollInterval(&state, now.Add(30*time.Second)); got != 15*time.Second {
		t.Fatalf("follow-up poll interval = %s, want exact 15s recovery wake", got)
	}
}
