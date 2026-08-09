package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func TestDispatchReadyIssuesKeepsRetryAttemptWhenCapacityIsFull(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:   1,
		MaxRetryBackoff:       time.Minute,
		FailureRetryBaseDelay: time.Second,
		ActiveStates:          []string{"Todo"},
		TerminalStates:        []string{"Done"},
	})
	orch := Orchestrator{cfg: cfg}
	state := newState(cfg)
	now := time.Now()
	running := retryTestIssue("running", "digitaldrywood/detent#20")
	retrying := retryTestIssue("retrying", "digitaldrywood/detent#21")

	state.Running[running.ID] = Running{Issue: running, StartedAt: now}
	state.Claimed[retrying.ID] = Claimed{Issue: retrying, ClaimedAt: now.Add(-time.Minute)}
	state.Retry[retrying.ID] = Retry{
		Issue:   retrying,
		Attempt: 2,
		DueAt:   now.Add(-time.Millisecond),
		Error:   "previous failure",
	}

	orch.dispatchReadyIssues(context.Background(), &state, []connector.Issue{retrying}, now)

	retry, ok := state.Retry[retrying.ID]
	if !ok {
		t.Fatalf("Retry[%q] missing after capacity refusal", retrying.ID)
	}
	if retry.Attempt != 2 {
		t.Fatalf("Retry[%q].Attempt = %d, want 2", retrying.ID, retry.Attempt)
	}
	if retry.Error != "no available orchestrator slots" {
		t.Fatalf("Retry[%q].Error = %q, want no available orchestrator slots", retrying.ID, retry.Error)
	}
	if !retry.DueAt.After(now) {
		t.Fatalf("Retry[%q].DueAt = %s, want after %s", retrying.ID, retry.DueAt, now)
	}
}

func TestApplyRuntimeUpdateRefreshesSupervisorRetryConfig(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		PollInterval:           time.Hour,
		MaxConcurrentAgents:    1,
		MaxRetryBackoff:        time.Minute,
		FailureRetryBaseDelay:  10 * time.Second,
		ActiveStates:           []string{"Todo"},
		TerminalStates:         []string{"Done"},
		PrioritizeUnblockers:   true,
		ContinuationRetryDelay: time.Second,
	})
	orch := Orchestrator{
		cfg:        cfg,
		supervisor: newTestSupervisor(t, FakeRunner{}, cfg),
	}
	state := newState(cfg)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	orch.applyRuntimeUpdate(&state, RuntimeUpdate{
		Config: Config{
			PollInterval:            time.Hour,
			MaxConcurrentAgents:     1,
			MaxRetryBackoff:         2 * time.Second,
			FailureRetryBaseDelay:   time.Second,
			StrandedActiveThreshold: 42 * time.Second,
			ActiveStates:            []string{"Todo"},
			TerminalStates:          []string{"Done"},
		},
	}, ticker)

	if got := orch.supervisor.RetryDelay(4); got != 2*time.Second {
		t.Fatalf("RetryDelay(4) = %s, want reloaded 2s cap", got)
	}
	if state.PrioritizeUnblockers {
		t.Fatal("State.PrioritizeUnblockers = true, want reloaded false")
	}
	if state.StrandedActiveThreshold != 42*time.Second {
		t.Fatalf("State.StrandedActiveThreshold = %s, want reloaded 42s", state.StrandedActiveThreshold)
	}
}

func TestDispatchReadyIssuesRanksDueRetriesWithCandidates(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:     1,
		DispatchPriorityByState: []string{"Merging"},
		ActiveStates:            []string{"Todo", "Merging"},
		TerminalStates:          []string{"Done"},
	})
	orch := Orchestrator{
		cfg:        cfg,
		supervisor: newTestSupervisor(t, FakeRunner{}, cfg),
		runResults: make(chan runpkg.Completion, 1),
	}
	state := newState(cfg)
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	retrying := retryTestIssue("retrying", "digitaldrywood/detent#21")
	merging := retryTestIssue("merging", "digitaldrywood/detent#22")
	merging.State = "Merging"
	priority := 4
	merging.Priority = &priority

	state.Claimed[retrying.ID] = Claimed{Issue: retrying, ClaimedAt: now.Add(-time.Minute)}
	state.Retry[retrying.ID] = Retry{
		Issue:   retrying,
		Attempt: 2,
		DueAt:   now.Add(-time.Millisecond),
		Error:   "previous failure",
	}

	issues := []connector.Issue{retrying, merging}
	sortIssuesForDispatch(issues, cfg.DispatchPriorityByState, cfg.DispatchPriorityByLabel, cfg.PrioritizeUnblockers)
	orch.dispatchReadyIssues(context.Background(), &state, issues, now)

	if _, ok := state.Running[merging.ID]; !ok {
		t.Fatalf("Running[%q] missing", merging.ID)
	}
	if _, ok := state.Running[retrying.ID]; ok {
		t.Fatalf("Running[%q] present", retrying.ID)
	}
	if retry, ok := state.Retry[retrying.ID]; !ok {
		t.Fatalf("Retry[%q] missing", retrying.ID)
	} else if retry.Attempt != 2 {
		t.Fatalf("Retry[%q].Attempt = %d, want 2", retrying.ID, retry.Attempt)
	}
}

func TestDispatchReadyIssuesDefersNotReadyMergeRetryBehindReadyHead(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		DispatchPriorityByState: []string{"Merging"},
		ActiveStates:            []string{"Merging"},
		TerminalStates:          []string{"Done"},
	})
	orch := Orchestrator{
		cfg:        cfg,
		supervisor: newTestSupervisor(t, FakeRunner{}, cfg),
		runResults: make(chan runpkg.Completion, 1),
	}
	state := newState(cfg)
	now := time.Date(2026, 7, 15, 8, 15, 0, 0, time.UTC)
	waiting := nativeMergeQueueTestIssue(1323, "pending")
	waiting.ID = "issue-deferred-head"
	waitingCreatedAt := now.Add(-time.Hour)
	waiting.CreatedAt = &waitingCreatedAt
	ready := nativeMergeQueueTestIssue(1324, "success")
	ready.ID = "issue-ready-after-deferral"
	ready.Identifier = "digitaldrywood/pyroapex#1324"
	ready.PRRepository = "digitaldrywood/pyroapex"
	ready.PullRequest.URL = "https://github.test/digitaldrywood/pyroapex/pull/1324"
	readyCreatedAt := now.Add(-time.Minute)
	ready.CreatedAt = &readyCreatedAt

	state.Claimed[waiting.ID] = Claimed{Issue: waiting, ClaimedAt: now.Add(-time.Minute)}
	state.Retry[waiting.ID] = Retry{
		Issue:   waiting,
		Attempt: 1,
		DueAt:   now.Add(-time.Millisecond),
		Error:   "waiting for current-head CI",
	}

	orch.dispatchReadyIssues(context.Background(), &state, []connector.Issue{waiting, ready}, now)

	if _, ok := state.Running[ready.ID]; !ok {
		t.Fatalf("Running[%q] missing", ready.ID)
	}
	if _, ok := state.Running[waiting.ID]; ok {
		t.Fatalf("Running[%q] present while a ready head was queued", waiting.ID)
	}
	if _, ok := state.Retry[waiting.ID]; !ok {
		t.Fatalf("Retry[%q] missing after ready head dispatch", waiting.ID)
	}
}

func TestDispatchReadyIssuesMergeFairnessReservation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 9, 4, 20, 0, 0, time.UTC)
	threshold := 2 * time.Hour
	tests := []struct {
		name             string
		enteredAt        time.Time
		wantReadyRunning bool
		wantReadyReason  string
	}{
		{
			name:             "non-aged retry yields to clean head",
			enteredAt:        now.Add(-threshold + time.Second),
			wantReadyRunning: true,
			wantReadyReason:  mergeSelectionReasonClean,
		},
		{
			name:            "aged retry reserves lane after invalidation",
			enteredAt:       now.Add(-threshold),
			wantReadyReason: dispatchSkipMergeFairnessReserved,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(Config{
				MaxConcurrentAgents: 1,
				MaxConcurrentAgentsByState: map[string]int{
					"Merging": 1,
				},
				DispatchPriorityByState: []string{"Merging"},
				ActiveStates:            []string{"Merging"},
				TerminalStates:          []string{"Done"},
				MergeFairnessAge:        threshold,
			})
			orch := Orchestrator{
				cfg:        cfg,
				supervisor: newTestSupervisor(t, FakeRunner{}, cfg),
				runResults: make(chan runpkg.Completion, 1),
			}
			state := newState(cfg)
			invalidated := nativeMergeQueueTestIssue(1748, "pending")
			invalidated.ID = "issue-invalidated-aged-head"
			invalidated.StageUpdatedAt = timePointer(tt.enteredAt)
			invalidated.PullRequest.MergeableState = "behind"
			ready := nativeMergeQueueTestIssue(1749, "success")
			ready.ID = "issue-new-clean-head"
			ready.Identifier = "digitaldrywood/pyroapex#1749"
			ready.PRRepository = "digitaldrywood/pyroapex"
			ready.PullRequest.URL = "https://github.test/digitaldrywood/pyroapex/pull/1749"
			ready.StageUpdatedAt = timePointer(now.Add(-time.Minute))

			state.Claimed[invalidated.ID] = Claimed{Issue: invalidated, ClaimedAt: now.Add(-time.Minute)}
			state.Retry[invalidated.ID] = Retry{
				Issue:   invalidated,
				Attempt: 1,
				DueAt:   now.Add(time.Minute),
				Error:   "waiting for current-head CI",
			}

			orch.dispatchReadyIssues(context.Background(), &state, []connector.Issue{invalidated, ready}, now)

			_, readyRunning := state.Running[ready.ID]
			if readyRunning != tt.wantReadyRunning {
				t.Fatalf("ready running = %t, want %t", readyRunning, tt.wantReadyRunning)
			}
			if _, ok := state.Retry[invalidated.ID]; !ok {
				t.Fatalf("Retry[%q] missing", invalidated.ID)
			}
			gotReason := ""
			for _, decision := range state.SchedulerDecisions {
				if decision.IssueID == ready.ID {
					gotReason = decision.Reason
				}
			}
			if gotReason != tt.wantReadyReason {
				t.Fatalf("ready decision reason = %q, want %q", gotReason, tt.wantReadyReason)
			}
		})
	}
}

func TestDispatchReadyIssuesReleasesMissingAgedRetryBeforeFairnessReservation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 9, 5, 0, 0, 0, time.UTC)
	threshold := 2 * time.Hour
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		DispatchPriorityByState: []string{"Merging"},
		ActiveStates:            []string{"Merging"},
		TerminalStates:          []string{"Done"},
		MergeFairnessAge:        threshold,
	})
	orch := Orchestrator{
		cfg:        cfg,
		supervisor: newTestSupervisor(t, FakeRunner{}, cfg),
		runResults: make(chan runpkg.Completion, 1),
	}
	state := newState(cfg)
	missing := nativeMergeQueueTestIssue(1748, "pending")
	missing.ID = "issue-missing-aged-retry"
	missing.StageUpdatedAt = timePointer(now.Add(-threshold))
	ready := nativeMergeQueueTestIssue(1751, "success")
	ready.ID = "issue-current-clean-head"
	ready.Identifier = "digitaldrywood/pyroapex#1751"
	ready.PRRepository = "digitaldrywood/pyroapex"
	ready.PullRequest.URL = "https://github.test/digitaldrywood/pyroapex/pull/1751"
	ready.StageUpdatedAt = timePointer(now.Add(-time.Minute))

	state.Claimed[missing.ID] = Claimed{Issue: missing, ClaimedAt: now.Add(-time.Minute)}
	state.Retry[missing.ID] = Retry{
		Issue:   missing,
		Attempt: 1,
		DueAt:   now.Add(-time.Millisecond),
		Error:   "waiting for current-head CI",
	}

	orch.dispatchReadyIssues(context.Background(), &state, []connector.Issue{ready}, now)

	if _, ok := state.Retry[missing.ID]; ok {
		t.Fatalf("Retry[%q] present after missing due retry cleanup", missing.ID)
	}
	if _, ok := state.Running[ready.ID]; !ok {
		t.Fatalf("Running[%q] missing after stale reservation cleanup", ready.ID)
	}
	gotReason := ""
	for _, decision := range state.SchedulerDecisions {
		if decision.IssueID == ready.ID {
			gotReason = decision.Reason
		}
	}
	if gotReason != mergeSelectionReasonClean {
		t.Fatalf("ready decision reason = %q, want %q", gotReason, mergeSelectionReasonClean)
	}
}

func TestDispatchReadyIssuesRevisitsDeferredMergeWhenCurrentHeadBecomesReady(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		DispatchPriorityByState: []string{"Merging"},
		ActiveStates:            []string{"Merging"},
		TerminalStates:          []string{"Done"},
	})
	orch := Orchestrator{
		cfg:        cfg,
		supervisor: newTestSupervisor(t, FakeRunner{}, cfg),
		runResults: make(chan runpkg.Completion, 1),
	}
	state := newState(cfg)
	now := time.Date(2026, 7, 15, 8, 30, 0, 0, time.UTC)
	queueHead := nativeMergeQueueTestIssue(1326, "pending")
	queueHead.ID = "issue-older-waiting-head"
	queueHeadCreatedAt := now.Add(-time.Hour)
	queueHead.CreatedAt = &queueHeadCreatedAt
	deferred := nativeMergeQueueTestIssue(1327, "pending")
	deferred.ID = "issue-deferred-now-ready"
	deferred.Identifier = "digitaldrywood/pyroapex#1327"
	deferred.PRRepository = "digitaldrywood/pyroapex"
	deferred.PullRequest.URL = "https://github.test/digitaldrywood/pyroapex/pull/1327"
	deferredCreatedAt := now.Add(-time.Minute)
	deferred.CreatedAt = &deferredCreatedAt
	refreshed := cloneIssue(deferred)
	refreshed.PullRequest.CIStatus = "success"

	state.Claimed[deferred.ID] = Claimed{Issue: deferred, ClaimedAt: now.Add(-time.Minute)}
	state.Retry[deferred.ID] = Retry{
		Issue:   deferred,
		Attempt: 1,
		DueAt:   now.Add(-time.Millisecond),
		Error:   "waiting for current-head CI",
	}

	orch.dispatchReadyIssues(context.Background(), &state, []connector.Issue{queueHead, refreshed}, now)

	running, ok := state.Running[deferred.ID]
	if !ok {
		t.Fatalf("Running[%q] missing", deferred.ID)
	}
	if running.Issue.PullRequest == nil || running.Issue.PullRequest.CIStatus != "success" {
		t.Fatalf("Running[%q].Issue.PullRequest = %#v, want refreshed green head", deferred.ID, running.Issue.PullRequest)
	}
	if _, ok := state.Running[queueHead.ID]; ok {
		t.Fatalf("Running[%q] present while refreshed deferred head was ready", queueHead.ID)
	}
	if _, ok := state.Retry[deferred.ID]; ok {
		t.Fatalf("Retry[%q] present after refreshed head dispatch", deferred.ID)
	}
}

func TestDispatchReadyIssuesPreservesBlockedStatusForMissingDueRetry(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:   1,
		ActiveStates:          []string{"Todo"},
		TerminalStates:        []string{"Done"},
		MaxRetryBackoff:       time.Minute,
		FailureRetryBaseDelay: time.Second,
	})
	orch := Orchestrator{cfg: cfg}
	state := newState(cfg)
	now := time.Now()
	retrying := retryTestIssue("retrying", "digitaldrywood/detent#21")
	blocked := retrying
	blocked.State = "Blocked"

	state.Claimed[retrying.ID] = Claimed{Issue: retrying, ClaimedAt: now.Add(-time.Minute)}
	state.Retry[retrying.ID] = Retry{
		Issue:   retrying,
		Attempt: 2,
		DueAt:   now.Add(-time.Millisecond),
		Error:   "previous failure",
	}
	state.Blocked[retrying.ID] = Blocked{
		Issue:     blocked,
		Reason:    "human action needed",
		BlockedAt: now,
		Source:    BlockedSourceProjectStatus,
	}

	orch.dispatchReadyIssues(context.Background(), &state, nil, now)

	if _, ok := state.Blocked[retrying.ID]; !ok {
		t.Fatalf("Blocked[%q] missing after missing due retry cleanup", retrying.ID)
	}
	if _, ok := state.Claimed[retrying.ID]; ok {
		t.Fatalf("Claimed[%q] present after missing due retry cleanup", retrying.ID)
	}
	if _, ok := state.Retry[retrying.ID]; ok {
		t.Fatalf("Retry[%q] present after missing due retry cleanup", retrying.ID)
	}
}

func TestDispatchReadyIssuesKeepsAttemptWhenWorkerCapacityIsFull(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:        2,
		MaxRetryBackoff:            time.Minute,
		FailureRetryBaseDelay:      time.Second,
		ActiveStates:               []string{"Todo"},
		TerminalStates:             []string{"Done"},
		WorkerHosts:                []string{"worker-a"},
		MaxConcurrentAgentsPerHost: 1,
	})
	orch := Orchestrator{cfg: cfg}
	state := newState(cfg)
	now := time.Now()
	running := retryTestIssue("running", "digitaldrywood/detent#20")
	retrying := retryTestIssue("retrying", "digitaldrywood/detent#21")

	state.Running[running.ID] = Running{Issue: running, StartedAt: now, WorkerHost: "worker-a"}
	state.Claimed[retrying.ID] = Claimed{Issue: retrying, ClaimedAt: now.Add(-time.Minute)}
	state.Retry[retrying.ID] = Retry{
		Issue:      retrying,
		Attempt:    2,
		DueAt:      now.Add(-time.Millisecond),
		Error:      "previous failure",
		WorkerHost: "worker-a",
	}

	orch.dispatchReadyIssues(context.Background(), &state, []connector.Issue{retrying}, now)

	retry, ok := state.Retry[retrying.ID]
	if !ok {
		t.Fatalf("Retry[%q] missing after worker capacity refusal", retrying.ID)
	}
	if retry.Attempt != 2 {
		t.Fatalf("Retry[%q].Attempt = %d, want 2", retrying.ID, retry.Attempt)
	}
	if retry.WorkerHost != "worker-a" {
		t.Fatalf("Retry[%q].WorkerHost = %q, want worker-a", retrying.ID, retry.WorkerHost)
	}
}

func retryTestIssue(id, identifier string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = identifier
	issue.Title = "Retry issue"
	issue.State = "Todo"
	return issue
}
