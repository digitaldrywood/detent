package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestMergeIdleHeadDoesNotReserveSlot(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"waiting", "retrying", "unready", "running", "running other base", "running case-distinct base", "ready"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			for _, path := range []string{"planner", "fresh", "native"} {
				t.Run(path, func(t *testing.T) {
					t.Parallel()
					now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
					cfg := nativeMergeQueueTestConfig(Config{MaxConcurrentAgents: 3, MaxConcurrentAgentsByState: map[string]int{"Merging": 3}, ActiveStates: []string{"Merging"}, TerminalStates: []string{"Done"}, MergeFastPathEnabled: true, MergeFairnessAge: time.Hour})
					state := newState(cfg)
					head := nativeMergeQueueTestIssue(2221, "success")
					head.StageUpdatedAt = timePointer(now.Add(-3 * time.Hour))
					other := nativeMergeQueueTestIssue(2220, "success")
					other.StageUpdatedAt = timePointer(now.Add(-time.Minute))
					switch kind {
					case "waiting":
						head.PullRequest.CIStatus = "pending"
						reserveMergeCandidate(&state, head, now)
						state.Retry[head.ID] = Retry{Issue: head, DueAt: now.Add(time.Minute), Wait: RetryWait{Kind: retryWaitCurrentHeadCI, StartedAt: now}}
					case "retrying":
						state.Retry[head.ID] = Retry{Issue: head, DueAt: now.Add(time.Minute)}
					case "unready":
						head.PullRequest.HydrationUnavailableReason = "unavailable"
					case "running", "running other base", "running case-distinct base":
						if kind == "running other base" {
							head.PullRequest.BaseRef = "release"
						}
						if kind == "running case-distinct base" {
							head.PullRequest.BaseRef = "Main"
						}
						state.Running[head.ID] = Running{Issue: head, Mode: runpkg.RunModeMerge}
					}
					tracker := &nativeMergeQueueConnector{autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
					orch := Orchestrator{cfg: cfg, connector: tracker}
					issues := []connector.Issue{other, head}
					var got []string
					switch path {
					case "planner":
						for _, dispatch := range newDispatchPlanner(cfg).plan(&state, issues, now, dispatchPlanHooks{}).Dispatches {
							got = append(got, dispatch.IssueID)
						}
					case "fresh":
						for _, issue := range orch.mergeWorkerDispatchCandidates(&state, issues, now) {
							got = append(got, issue.ID)
						}
					case "native":
						orch.delegateNativeMergeQueueIssues(t.Context(), &state, issues, now)
						got = tracker.enqueued
					}
					want := []string{other.ID}
					if kind == "ready" {
						want = []string{head.ID, other.ID}
					}
					if kind == "running" && path == "native" {
						want = nil
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("started = %v, want %v", got, want)
					}
				})
			}
		})
	}
}

func TestPushedMergeHeadRequeuesWithoutHoldingSlot(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		output string
	}{
		{name: "programmatic base sync", output: runpkg.RunOutputMergeFastPathClean},
		{name: "agent pushed fix"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 9, 23, 17, 0, 0, 0, time.UTC)
			cfg := normalizeConfig(Config{
				MaxConcurrentAgents:        1,
				MaxConcurrentAgentsByState: map[string]int{"Merging": 1},
				ActiveStates:               []string{"Merging"},
				TerminalStates:             []string{"Done"},
				MergeFastPathEnabled:       true,
				MergeFairnessAge:           time.Hour,
				ContinuationRetryDelay:     time.Second,
			})
			waiting := nativeMergeQueueTestIssue(3045, "pending")
			waiting.PullRequest.HeadSHA = "pushed-head"
			waiting.StageUpdatedAt = timePointer(now.Add(-3 * time.Hour))
			peer := nativeMergeQueueTestIssue(3046, "success")
			peer.StageUpdatedAt = timePointer(now.Add(-2 * time.Hour))
			newer := nativeMergeQueueTestIssue(3047, "success")
			newer.StageUpdatedAt = timePointer(now.Add(-time.Minute))
			tracker := &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{waiting, peer, newer}}}
			orch := Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			state.Running[waiting.ID] = Running{Issue: waiting, Attempt: 1, Mode: runpkg.RunModeMerge}
			state.Claimed[waiting.ID] = Claimed{Issue: waiting}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID: waiting.ID, CompletedAt: now,
				Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge},
				Result: runpkg.RunResult{FinalState: runpkg.FinalStateCompleted, Output: tt.output,
					PullRequestHeadPushed: true, CITriggerLabelReapplied: true},
			})
			if _, running := state.Running[waiting.ID]; running {
				t.Fatal("pushed head retained Merging worker")
			}
			if _, claimed := state.Claimed[waiting.ID]; claimed {
				t.Fatal("pushed head retained claim")
			}
			if retry := state.Retry[waiting.ID]; retry.Wait.Kind != retryWaitCurrentHeadCI {
				t.Fatalf("retry wait = %q, want current-head CI", retry.Wait.Kind)
			}
			plan := newDispatchPlanner(cfg).plan(&state, []connector.Issue{waiting, peer, newer}, now.Add(time.Minute), dispatchPlanHooks{})
			if len(plan.Dispatches) != 1 || plan.Dispatches[0].IssueID != peer.ID {
				t.Fatalf("dispatches while CI pending = %#v, want same-repository peer", plan.Dispatches)
			}
			delete(state.Running, peer.ID)
			delete(state.Claimed, peer.ID)
			waiting.PullRequest.CIStatus = "success"
			plan = newDispatchPlanner(cfg).plan(&state, []connector.Issue{newer, waiting}, now.Add(2*time.Minute), dispatchPlanHooks{})
			if len(plan.Dispatches) != 1 || plan.Dispatches[0].IssueID != waiting.ID {
				t.Fatalf("dispatches after CI green = %#v, want aged waiting issue before newer issue", plan.Dispatches)
			}
		})
	}
}

func TestMergeDispatchUsesMergingCapacity(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		limit      int
		retry      bool
		wantPicked int
	}{
		{name: "three slots fresh", limit: 3, wantPicked: 2},
		{name: "three slots retry", limit: 3, retry: true, wantPicked: 2},
		{name: "one slot fresh", limit: 1},
		{name: "one slot retry", limit: 1, retry: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
			cfg := normalizeConfig(Config{
				MaxConcurrentAgents:        4,
				MaxConcurrentAgentsByState: map[string]int{"Merging": tt.limit},
				ActiveStates:               []string{"Merging"},
				TerminalStates:             []string{"Done"},
				MergeFastPathEnabled:       true,
			})
			state := newState(cfg)
			running := nativeMergeQueueTestIssue(2221, "success")
			state.Running[running.ID] = Running{Issue: running, Mode: runpkg.RunModeMerge}
			candidates := []connector.Issue{
				nativeMergeQueueTestIssue(2220, "success"),
				nativeMergeQueueTestIssue(2219, "success"),
				nativeMergeQueueTestIssue(2218, "success"),
			}
			for _, issue := range candidates {
				issue.PullRequest.BaseSHA = "same-base"
				if tt.retry {
					state.Retry[issue.ID] = Retry{Issue: issue, Attempt: 1, DueAt: now}
				}
			}
			if !tt.retry {
				orch := Orchestrator{cfg: cfg}
				fresh := orch.mergeWorkerDispatchCandidates(&state, candidates, now)
				if len(fresh) != tt.wantPicked {
					t.Fatalf("fresh candidates = %#v, want %d slots", fresh, tt.wantPicked)
				}
			}
			decisions := make(map[string]dispatchPlanDecision)
			plan := newDispatchPlanner(cfg).plan(&state, candidates, now, dispatchPlanHooks{
				decision: func(decision dispatchPlanDecision) { decisions[decision.Issue.ID] = decision },
			})
			if len(plan.Dispatches) != tt.wantPicked {
				t.Fatalf("dispatches = %#v, want %d", plan.Dispatches, tt.wantPicked)
			}
			selected, skipped := 0, 0
			for _, issue := range candidates {
				decision := decisions[issue.ID]
				if decision.Selected {
					selected++
				} else if decision.SkipReason == dispatchSkipLocalSlotUnavailable {
					skipped++
				} else {
					t.Errorf("%s skip = %q, want state capacity", issue.ID, decision.SkipReason)
				}
			}
			if selected != tt.wantPicked || skipped != len(candidates)-tt.wantPicked {
				t.Errorf("selected = %d, capacity skipped = %d; want %d and %d", selected, skipped, tt.wantPicked, len(candidates)-tt.wantPicked)
			}
		})
	}
}

func TestMergeDispatchAllowsHydratedBase(t *testing.T) {
	t.Parallel()
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry=%t", retry), func(t *testing.T) {
			t.Parallel()
			for _, tt := range []struct {
				cached, hydrated string
				want             int
			}{
				{"release", "main", 1}, {"", "main", 1}, {"main", "release", 1},
			} {
				t.Run(tt.cached+"_to_"+tt.hydrated, func(t *testing.T) {
					t.Parallel()
					now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
					cfg := normalizeConfig(Config{MaxConcurrentAgents: 3, ActiveStates: []string{"Merging"}, MergeFastPathEnabled: true})
					state := newState(cfg)
					running := nativeMergeQueueTestIssue(2221, "success")
					state.Running[running.ID] = Running{Issue: running, Mode: runpkg.RunModeMerge}
					issue := nativeMergeQueueTestIssue(2220, "success")
					issue.PullRequest.BaseRef = tt.cached
					if retry {
						state.Retry[issue.ID] = Retry{Issue: issue, DueAt: now}
					}
					hooks := dispatchPlanHooks{hydrate: func(i connector.Issue) (connector.Issue, bool) { i.PullRequest.BaseRef = tt.hydrated; return i, true }}
					plan := newDispatchPlanner(cfg).plan(&state, []connector.Issue{issue}, now, hooks)
					if len(plan.Dispatches) != tt.want {
						t.Fatalf("dispatches = %v, want %d", plan.Dispatches, tt.want)
					}
				})
			}
		})
	}
}

func TestMergeWaitMetadataRemainsIndependent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{ActiveStates: []string{"Merging"}, TerminalStates: []string{"Done"}})
	orch := Orchestrator{cfg: cfg}
	state := newState(cfg)
	issues := []connector.Issue{nativeMergeQueueTestIssue(2221, "pending"), nativeMergeQueueTestIssue(2220, "pending")}
	var attempts []store.WorkAttempt
	for index, issue := range issues {
		wait := orch.recordMergeReservationWait(&state, issue, now.Add(time.Duration(index)*time.Minute))
		attempts = append(attempts, store.WorkAttempt{ID: int64(index + 1), IssueID: issue.ID, Phase: "waiting", Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalSuccess, CompletedAt: now.Add(time.Duration(index) * time.Minute), WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{mergeReservationMetadataKey: wait})})
	}
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			current := state.clone()
			if restart {
				current = newState(cfg)
				orch.recoverMergeReservations(&current, attempts, issues, now.Add(2*time.Minute))
			}
			for index, issue := range issues {
				wait := orch.recordMergeReservationWait(&current, issue, now.Add(3*time.Minute))
				want := now.Add(time.Duration(index)*time.Minute + mergeWorkerCurrentHeadCIWaitTimeout)
				if wait.ExpiresAt != want {
					t.Fatalf("%s deadline = %v, want %v", issue.ID, wait.ExpiresAt, want)
				}
			}
		})
	}
}

func TestMergeReleasedWaitStartsFreshOnRedispatch(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		mutate  func(*connector.Issue)
		elapsed time.Duration
	}{
		{name: "failed checks", mutate: func(i *connector.Issue) { i.PullRequest.CIStatus = "failure" }},
		{name: "expired", elapsed: mergeWorkerCurrentHeadCIWaitTimeout},
		{name: "withdrawn", mutate: func(i *connector.Issue) { i.State = "Rework" }},
		{name: "changed head", mutate: func(i *connector.Issue) { i.PullRequest.HeadSHA = "replacement-head" }},
		{name: "changed repository", mutate: func(i *connector.Issue) { i.PRRepository = "example/replacement" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			cfg := normalizeConfig(Config{ActiveStates: []string{"Merging"}, TerminalStates: []string{"Done"}, MergeFastPathEnabled: true})
			orch := Orchestrator{cfg: cfg}
			state := newState(cfg)
			issue := nativeMergeQueueTestIssue(2221, "success")
			issue.PullRequest.BaseSHA = "base"
			old := reserveMergeCandidate(&state, issue, now)
			old.RefreshHeadSHA = issue.PullRequest.HeadSHA
			state.mergeReservations[issue.ID] = old
			if tt.mutate != nil {
				tt.mutate(&issue)
			}
			later := now.Add(time.Minute + tt.elapsed)
			orch.reconcileMergeReservations(&state, []connector.Issue{issue}, later)
			issue.State = "Merging"
			issue.PullRequest.CIStatus = "success"
			if !newDispatchPlanner(cfg).readyMergeControlCandidate(&state, issue) {
				t.Error("released refresh marker excludes merge-control admission")
			}
			fresh := reserveMergeCandidate(&state, issue, later)
			if fresh.ReleasedReason != "" || fresh.RefreshHeadSHA != "" || fresh.Repository != mergeWorkerRepositoryKey(issue) || fresh.HeadSHA != issue.PullRequest.HeadSHA || !fresh.StartedAt.Equal(later) || !fresh.ExpiresAt.Equal(later.Add(mergeWorkerCurrentHeadCIWaitTimeout)) {
				t.Fatalf("redispatch inherited stale wait metadata: %+v", fresh)
			}
			issue.PullRequest.CIStatus = "pending"
			wait := orch.recordMergeReservationWait(&state, issue, later.Add(time.Minute))
			restarted := newState(cfg)
			orch.recoverMergeReservations(&restarted, []store.WorkAttempt{{
				ID: 2, IssueID: issue.ID, Phase: "waiting", Status: store.WorkAttemptStatusTerminal,
				TerminalState: store.WorkAttemptTerminalSuccess, CompletedAt: later.Add(time.Minute),
				WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{mergeReservationMetadataKey: wait}),
			}}, []connector.Issue{issue}, later.Add(2*time.Minute))
			if got := restarted.mergeReservations[issue.ID]; got != fresh {
				t.Fatalf("recovered wait = %+v, want %+v", got, fresh)
			}
			if retry := restarted.Retry[issue.ID]; retry.Wait.Kind != retryWaitCurrentHeadCI || !retry.Wait.StartedAt.Equal(later) {
				t.Fatalf("recovered retry = %+v", retry)
			}
		})
	}
}

func TestMergeCIWaitAllowsReadyRetryBeforeFairnessAge(t *testing.T) {
	t.Parallel()
	for _, ci := range []string{"pending", "success"} {
		t.Run(ci, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 5, 15, 13, 37, 0, time.UTC)
			cfg := normalizeConfig(Config{
				MaxConcurrentAgents:        3,
				MaxConcurrentAgentsByState: map[string]int{"Merging": 1},
				ActiveStates:               []string{"Todo", "Merging"},
				TerminalStates:             []string{"Done"},
				MergeFastPathEnabled:       true,
				MergeFairnessAge:           2 * time.Hour,
				ContinuationRetryDelay:     time.Second,
			})
			orch := Orchestrator{cfg: cfg}
			state := newState(cfg)
			selected := nativeMergeQueueTestIssue(2221, "pending")
			selected.StageUpdatedAt = timePointer(now.Add(-3 * time.Minute))
			selected.PullRequest.HeadSHA = "validated-prerequisite-head"
			selected.PullRequest.BaseSHA = "base-before-other-merge"
			other := nativeMergeQueueTestIssue(2220, "success")
			other.StageUpdatedAt = timePointer(now.Add(-15 * time.Minute))
			state.Retry[other.ID] = Retry{Issue: other, Attempt: 2, DueAt: now}
			orch.waitForMergeWorkerCurrentHeadCI(t.Context(), &state,
				runpkg.Completion{IssueID: selected.ID, CompletedAt: now},
				Running{Issue: selected, Attempt: 1}, selected)
			selected.PullRequest.CIStatus = ci
			plan := newDispatchPlanner(cfg).plan(&state, []connector.Issue{other, selected}, now.Add(time.Minute), dispatchPlanHooks{})
			if len(plan.Dispatches) != 1 || plan.Dispatches[0].IssueID != other.ID {
				t.Fatalf("dispatches = %#v, want the older ready retry", plan.Dispatches)
			}
		})
	}
}

func TestMergeReservationLifecycle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name    string
		mutate  func(*connector.Issue)
		elapsed time.Duration
		reason  string
	}{
		{name: "pending CI"},
		{name: "green CI", mutate: func(i *connector.Issue) { i.PullRequest.CIStatus = "success" }},
		{name: "external base advance", mutate: func(i *connector.Issue) {
			i.PullRequest.BaseSHA = "external-base"
			i.PullRequest.MergeableState = "behind"
		}},
		{name: "external head change", mutate: func(i *connector.Issue) { i.PullRequest.HeadSHA = "external-head" }, reason: "head_changed"},
		{name: "failed checks", mutate: func(i *connector.Issue) { i.PullRequest.CIStatus = "failure" }, reason: "required_checks_failed"},
		{name: "failed required check with pending aggregate", mutate: func(i *connector.Issue) {
			i.PullRequest.MergeableState = "blocked"
			i.PullRequest.RunningChecks = []string{"Windows Core"}
			i.PullRequest.RequiredCheckFailures = []connector.PullRequestCheck{{Name: "GoReleaser Snapshot", Status: "completed", Conclusion: "failure"}, {Name: "Windows Core", Status: "in_progress"}}
		}, reason: "required_checks_failed"},
		{name: "missing required check needs startup", mutate: func(i *connector.Issue) {
			i.PullRequest.MergeableState = "blocked"
			i.PullRequest.RequiredCheckFailures = []connector.PullRequestCheck{{Name: "Windows Core", Status: "missing", Conclusion: "missing"}}
		}},
		{name: "running required check", mutate: func(i *connector.Issue) {
			i.PullRequest.RequiredCheckFailures = []connector.PullRequestCheck{{Name: "Windows Core", Status: "in_progress"}}
		}},
		{name: "ignored check telemetry", mutate: func(i *connector.Issue) {
			i.PullRequest.SlowChecks = []connector.PullRequestCheck{{Name: "Optional", Status: "completed", Conclusion: "neutral"}, {Name: "Cancelled", Status: "completed", Conclusion: "cancelled"}}
		}},
		{name: "degraded failure is not authoritative", mutate: func(i *connector.Issue) {
			i.PullRequest.CIStatus = "failure"
			i.PullRequest.HydrationDegradedReason = "rate_limited"
			i.PullRequest.RequiredCheckFailures = []connector.PullRequestCheck{{Name: "Test", Status: "completed", Conclusion: "failure"}}
		}},
		{name: "unavailable failure is not authoritative", mutate: func(i *connector.Issue) {
			i.PullRequest.CIStatus = "failure"
			i.PullRequest.HydrationUnavailableReason = "checks_unavailable"
		}},
		{name: "conflict", mutate: func(i *connector.Issue) { i.PullRequest.MergeableState = "dirty" }, reason: "conflict"},
		{name: "withdrawal", mutate: func(i *connector.Issue) { i.State = "Rework" }, reason: "withdrawn"},
		{name: "closed", mutate: func(i *connector.Issue) { i.PullRequest.State = "closed" }, reason: "withdrawn"},
		{name: "draft", mutate: func(i *connector.Issue) { i.PullRequest.Draft = true }, reason: "withdrawn"},
		{name: "native queue", mutate: func(i *connector.Issue) {
			i.PullRequest.MergeQueueEntry = &connector.PullRequestMergeQueueEntry{ID: "entry"}
		}, reason: "native_queue"},
		{name: "stalled CI expires", elapsed: mergeWorkerCurrentHeadCIWaitTimeout, reason: "expired"},
		{name: "degraded hydration bounded", mutate: func(i *connector.Issue) { i.PullRequest.HydrationDegradedReason = "unavailable" }},
		{name: "degraded hydration expires", mutate: func(i *connector.Issue) { i.PullRequest.HydrationDegradedReason = "unavailable" }, elapsed: mergeWorkerCurrentHeadCIWaitTimeout, reason: "expired"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{ActiveStates: []string{"Merging"}, TerminalStates: []string{"Done"}})
			state := newState(cfg)
			issue := nativeMergeQueueTestIssue(2221, "pending")
			original := reserveMergeCandidate(&state, issue, now)
			if tt.mutate != nil {
				tt.mutate(&issue)
			}
			var logs bytes.Buffer
			orch := Orchestrator{cfg: cfg, logger: slog.New(slog.NewTextHandler(&logs, nil))}
			orch.reconcileMergeReservations(&state, []connector.Issue{issue}, now.Add(tt.elapsed))
			reservation, active := state.mergeReservations[original.IssueID]
			if active != (tt.reason == "") {
				t.Fatalf("reservation = %#v, active = %t, want reason %q", reservation, active, tt.reason)
			}
			if tt.reason != "" && !strings.Contains(logs.String(), "reason="+tt.reason) {
				t.Fatalf("missing release reason in %s", logs.String())
			}
			if active && !reservation.ExpiresAt.Equal(original.ExpiresAt) {
				t.Fatal("reservation deadline moved")
			}
		})
	}
}

func TestMergeReservationAllowsIndependentWork(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC)
	for _, age := range []time.Duration{time.Minute, 3 * time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 3, MaxConcurrentAgentsByState: map[string]int{"Merging": 1}, ActiveStates: []string{"Merging", "Todo"}, TerminalStates: []string{"Done"}})
			state := newState(cfg)
			waiting := nativeMergeQueueTestIssue(2221, "pending")
			waiting.StageUpdatedAt = timePointer(now.Add(-age))
			reserveMergeCandidate(&state, waiting, now)
			state.Retry[waiting.ID] = Retry{Issue: waiting, Attempt: 1, DueAt: now.Add(time.Minute), Wait: RetryWait{Kind: retryWaitCurrentHeadCI, StartedAt: now}}
			other := nativeMergeQueueTestIssue(2220, "success")
			other.PRRepository = "example/other"
			implementation := dispatchTestIssue("implementation", "Todo")
			plan := newDispatchPlanner(cfg).plan(&state, []connector.Issue{waiting, other, implementation}, now, dispatchPlanHooks{})
			if len(plan.Dispatches) != 2 {
				t.Fatalf("dispatches = %#v, want unrelated merge and implementation", plan.Dispatches)
			}
			if _, held := state.Running[waiting.ID]; held {
				t.Fatal("CI wait holds worker slot")
			}
		})
	}
}

func TestMergeReservationFailedHeadAdmitsNextCandidate(t *testing.T) {
	t.Parallel()
	for _, restart := range []string{"none", "before failure", "after failure"} {
		t.Run(restart, func(t *testing.T) {
			t.Parallel()
			for _, ci := range []string{"failure", "pending"} {
				t.Run(ci, func(t *testing.T) {
					t.Parallel()
					now := time.Date(2026, 9, 8, 15, 40, 0, 0, time.UTC)
					cfg := normalizeConfig(Config{MaxConcurrentAgents: 3, MaxConcurrentAgentsByState: map[string]int{"Merging": 1}, ActiveStates: []string{"Merging", "Rework"}, TerminalStates: []string{"Done"}, MergeFastPathEnabled: true})
					issue := nativeMergeQueueTestIssue(2320, "pending")
					issue.StageUpdatedAt = timePointer(now.Add(-time.Hour))
					other := nativeMergeQueueTestIssue(2318, "success")
					attempts := &recordingWorkAttemptStore{}
					tracker := &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue, other}}}
					orch := Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts}
					state := newState(cfg)
					orch.waitForMergeWorkerCurrentHeadCI(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: now}, Running{Issue: issue, Attempt: 1, WorkAttemptID: 1}, issue)
					if len(attempts.completions) != 1 {
						t.Fatalf("completions = %d, want 1", len(attempts.completions))
					}
					completion := attempts.completions[0]
					attempts.history = []store.WorkAttempt{{ID: 1, IssueID: issue.ID, Phase: completion.Phase, Status: completion.Status, TerminalState: completion.TerminalState, AttemptNumber: 1, CompletedAt: now, WorkerMetadataJSON: completion.WorkerMetadataJSON}}
					if restart == "before failure" {
						state = newState(cfg)
						orch.restoreDurableMergeReservations(t.Context(), &state, []connector.Issue{issue}, now.Add(time.Minute))
					}
					issue.PullRequest.CIStatus = ci
					issue.PullRequest.MergeableState = "blocked"
					issue.PullRequest.RunningChecks = []string{"Verify (ubuntu-latest)", "Windows Core"}
					issue.PullRequest.RequiredCheckFailures = []connector.PullRequestCheck{{Name: "GoReleaser Snapshot", Status: "completed", Conclusion: "failure"}, {Name: "Windows Core", Status: "in_progress"}}
					tracker.stateIssues = []connector.Issue{issue, other}
					if restart == "after failure" {
						state = newState(cfg)
						orch.restoreDurableMergeReservations(t.Context(), &state, []connector.Issue{issue}, now.Add(3*time.Minute))
					}
					orch.reconcileMergeReservations(&state, tracker.stateIssues, now.Add(3*time.Minute))
					transitioned := orch.reconcileStaleMergingPullRequestIssues(t.Context(), &state, tracker.stateIssues, now.Add(3*time.Minute))
					if _, ok := transitioned[issue.ID]; !ok {
						t.Fatalf("failed head was not reconciled to Rework: updates=%#v wait=%q", tracker.updates, state.Retry[issue.ID].Wait.Kind)
					}
					if !reflect.DeepEqual(tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Rework"}}) {
						t.Fatalf("updates = %#v, want failed issue in Rework", tracker.updates)
					}
					if _, retry := state.Retry[issue.ID]; retry {
						t.Fatal("failed head retained CI wait retry")
					}
					candidates := orch.mergeWorkerDispatchCandidates(&state, issuesInStates(tracker.stateIssues, []string{"Merging"}), now.Add(3*time.Minute))
					plan := newDispatchPlanner(cfg).plan(&state, candidates, now.Add(3*time.Minute), dispatchPlanHooks{})
					if len(plan.Dispatches) != 1 || plan.Dispatches[0].IssueID != other.ID {
						t.Fatalf("dispatches = %#v, want next green candidate", plan.Dispatches)
					}
					if len(tracker.merges) != 0 {
						t.Fatal("failed head reached merge API")
					}
				})
			}
		})
	}
}

func TestMergeReservationTracksWorkerPushWithoutRenewal(t *testing.T) {
	t.Parallel()
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%t", expired), func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC)
			cfg := normalizeConfig(Config{ActiveStates: []string{"Merging"}})
			state := newState(cfg)
			issue := nativeMergeQueueTestIssue(2221, "success")
			original := reserveMergeCandidate(&state, issue, now)
			state.Running[issue.ID] = Running{Issue: cloneIssue(issue), Mode: runpkg.RunModeMerge}
			issue.PullRequest.HeadSHA = "worker-pushed-head"
			issue.PullRequest.CIStatus = "pending"
			elapsed := time.Minute
			if expired {
				elapsed = mergeWorkerCurrentHeadCIWaitTimeout
			}
			orch := Orchestrator{cfg: cfg}
			orch.reconcileMergeReservations(&state, []connector.Issue{issue}, now.Add(elapsed))
			delete(state.Running, issue.ID)
			reservation := orch.recordMergeReservationWait(&state, issue, now.Add(elapsed))
			if reservation.HeadSHA != issue.PullRequest.HeadSHA || reservation.ExpiresAt != original.ExpiresAt {
				t.Fatalf("reservation = %#v, want pushed head and original deadline", reservation)
			}
		})
	}
}

func TestMergeReservationTransientFailureDefersRerun(t *testing.T) {
	t.Parallel()
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 8, 15, 54, 0, 0, time.UTC)
			cfg := normalizeConfig(Config{ActiveStates: []string{"Merging", "Rework"}, TerminalStates: []string{"Done"}, MergeFastPathEnabled: true, AutoPromote: AutoPromoteConfig{Gate: gate.Config{TransientCIRetryLimit: new(1)}}})
			issue := nativeMergeQueueTestIssue(2320, "failure")
			issue.StageUpdatedAt = timePointer(now.Add(-2 * time.Hour))
			issue.PullRequest.MergeableState = "blocked"
			issue.PullRequest.RunningChecks = []string{"Windows"}
			issue.PullRequest.TransientFailedChecks = []connector.PullRequestCheck{{Name: "Snapshot", ID: 9001, WorkflowRunID: 8001, Status: "completed", Conclusion: "timed_out"}}
			other := nativeMergeQueueTestIssue(2318, "success")
			other.StageUpdatedAt = timePointer(now.Add(-time.Hour))
			other.PullRequest.MergeableState = "behind"
			tracker := &deferredMergeCIRetryConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue, other}}, err: connector.NewRetryableError("workflow is still running")}
			orch := Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			if !restart {
				reserveMergeCandidate(&state, issue, now.Add(-time.Minute))
				state.Retry[issue.ID] = Retry{Issue: issue, Wait: RetryWait{Kind: retryWaitCurrentHeadCI}}
			}
			orch.reconcileMergeReservations(&state, tracker.stateIssues, now)
			orch.reconcileStaleMergingPullRequestIssues(t.Context(), &state, tracker.stateIssues, now)
			if len(tracker.updates) != 0 || len(state.TransientCheckRetries) != 0 || len(tracker.comments) != 0 {
				t.Fatalf("active workflow caused rework or charged retry: updates=%v retries=%v comments=%v", tracker.updates, state.TransientCheckRetries, tracker.comments)
			}
			candidates := orch.mergeWorkerDispatchCandidates(&state, tracker.stateIssues, now)
			if len(candidates) != 1 || candidates[0].ID != other.ID {
				t.Fatalf("candidates=%v, want next candidate while transient rerun waits", candidates)
			}
			tracker.err = nil
			orch.reconcileStaleMergingPullRequestIssues(t.Context(), &state, tracker.stateIssues, now.Add(time.Minute))
			if len(tracker.reruns) != 1 || len(state.TransientCheckRetries) != 1 || len(tracker.updates) != 0 {
				t.Fatalf("settled workflow did not retry: reruns=%v retries=%v updates=%v", tracker.reruns, state.TransientCheckRetries, tracker.updates)
			}
			orch.reconcileStaleMergingPullRequestIssues(t.Context(), &state, tracker.stateIssues, now.Add(2*time.Minute))
			if !reflect.DeepEqual(tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Rework"}}) {
				t.Fatalf("exhausted retry updates=%v, want Rework", tracker.updates)
			}
		})
	}
}

type deferredMergeCIRetryConnector struct {
	*autoPromoteTickConnector
	err error
}

func (c *deferredMergeCIRetryConnector) RerunPullRequestChecks(ctx context.Context, issue connector.Issue, checks []connector.PullRequestCheck) error {
	if c.err != nil {
		return c.err
	}
	return c.autoPromoteTickConnector.RerunPullRequestChecks(ctx, issue, checks)
}

func TestMergeReservationRecoversCurrentWaitingHead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name   string
		mutate func(*connector.Issue, *store.WorkAttempt)
		want   bool
	}{
		{name: "pending", want: true},
		{name: "green after restart", mutate: func(i *connector.Issue, _ *store.WorkAttempt) { i.PullRequest.CIStatus = "success" }, want: true},
		{name: "external base advance", mutate: func(i *connector.Issue, _ *store.WorkAttempt) { i.PullRequest.BaseSHA = "external" }, want: true},
		{name: "head changed", mutate: func(i *connector.Issue, _ *store.WorkAttempt) { i.PullRequest.HeadSHA = "changed" }},
		{name: "withdrawn", mutate: func(i *connector.Issue, _ *store.WorkAttempt) { i.State = "Rework" }},
		{name: "failed", mutate: func(i *connector.Issue, _ *store.WorkAttempt) { i.PullRequest.CIStatus = "failure" }},
		{name: "failed required check while pending", mutate: func(i *connector.Issue, _ *store.WorkAttempt) {
			i.PullRequest.RequiredCheckFailures = []connector.PullRequestCheck{{Name: "Snapshot", Status: "completed", Conclusion: "failure"}, {Name: "Test", Status: "queued"}}
		}},
		{name: "missing check needs startup", mutate: func(i *connector.Issue, _ *store.WorkAttempt) {
			i.PullRequest.RequiredCheckFailures = []connector.PullRequestCheck{{Name: "Test", Status: "missing", Conclusion: "missing"}}
		}, want: true},
		{name: "newer completed attempt", mutate: func(_ *connector.Issue, a *store.WorkAttempt) { a.Phase = "completed" }},
		{name: "malformed metadata", mutate: func(_ *connector.Issue, a *store.WorkAttempt) { a.WorkerMetadataJSON = "{" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{ActiveStates: []string{"Merging"}, TerminalStates: []string{"Done"}})
			state := newState(cfg)
			issue := nativeMergeQueueTestIssue(2221, "pending")
			reservation := reserveMergeCandidate(&state, issue, now.Add(-10*time.Minute))
			attempt := store.WorkAttempt{ID: 1, IssueID: issue.ID, Phase: "waiting", Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalSuccess, AttemptNumber: 2, CompletedAt: now.Add(-9 * time.Minute), WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{mergeReservationMetadataKey: reservation})}
			if tt.mutate != nil {
				tt.mutate(&issue, &attempt)
			}
			tracker := &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}}
			orch := Orchestrator{cfg: cfg, connector: tracker}
			restarted := newState(cfg)
			orch.recoverMergeReservations(&restarted, []store.WorkAttempt{attempt}, []connector.Issue{issue}, now)
			_, restored := restarted.Retry[issue.ID]
			if restored != tt.want {
				t.Fatalf("restored = %t, want %t", restored, tt.want)
			}
			if restored && restarted.mergeReservations[reservation.IssueID].ExpiresAt != reservation.ExpiresAt {
				t.Fatal("restart renewed reservation")
			}
		})
	}
}

func TestMergeReservationExpiryAdmitsNextCandidate(t *testing.T) {
	t.Parallel()
	for _, retryQueued := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry=%t", retryQueued), func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC)
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 3, ActiveStates: []string{"Merging"}, MergeFastPathEnabled: true})
			orch := Orchestrator{cfg: cfg}
			state := newState(cfg)
			waiting := nativeMergeQueueTestIssue(2221, "pending")
			waiting.StageUpdatedAt = timePointer(now.Add(-3 * time.Hour))
			reserveMergeCandidate(&state, waiting, now.Add(-mergeWorkerCurrentHeadCIWaitTimeout))
			state.Retry[waiting.ID] = Retry{Issue: waiting, Attempt: 1, DueAt: now, Wait: RetryWait{Kind: retryWaitCurrentHeadCI, StartedAt: now.Add(-mergeWorkerCurrentHeadCIWaitTimeout)}}
			other := nativeMergeQueueTestIssue(2220, "success")
			if retryQueued {
				state.Retry[other.ID] = Retry{Issue: other, Attempt: 1, DueAt: now}
			} else {
				candidates := orch.mergeWorkerDispatchCandidates(&state, []connector.Issue{waiting, other}, now)
				if len(candidates) != 1 || candidates[0].ID != other.ID {
					t.Fatalf("candidates = %#v, want next candidate after expiry", candidates)
				}
			}
			plan := newDispatchPlanner(cfg).plan(&state, []connector.Issue{waiting, other}, now, dispatchPlanHooks{})
			if len(plan.Dispatches) != 1 || plan.Dispatches[0].IssueID != other.ID {
				t.Fatalf("dispatches = %#v, want next candidate after expiry", plan.Dispatches)
			}
		})
	}
}

func TestMergeReservationPersistsAndRestoresWait(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		age        time.Duration
		refresh    bool
		apiRefusal bool
		want       bool
	}{
		{name: "CI wait", age: time.Minute, want: true},
		{name: "behind refresh before API", age: time.Minute, refresh: true, want: true},
		{name: "base changes at API", age: time.Minute, refresh: true, apiRefusal: true, want: true},
		{name: "expired", age: mergeWorkerCurrentHeadCIWaitTimeout},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC)
			cfg := normalizeConfig(Config{ActiveStates: []string{"Merging"}, MergeFastPathEnabled: true})
			state := newState(cfg)
			issue := nativeMergeQueueTestIssue(2221, "pending")
			attempts := &recordingWorkAttemptStore{}
			tracker := &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}}
			orch := Orchestrator{cfg: cfg, workAttempts: attempts, connector: tracker}
			running := Running{Issue: issue, Attempt: 2, WorkAttemptID: 1}
			event := runpkg.Completion{IssueID: issue.ID, CompletedAt: now}
			if tt.refresh {
				issue.PullRequest.CIStatus = "success"
				issue.PullRequest.MergeableState = "behind"
				if tt.apiRefusal {
					issue.PullRequest.MergeableState = "clean"
					tracker.err = connector.ErrPullRequestBaseOutOfDate
				}
				event.Result = runpkg.RunResult{FinalState: runpkg.FinalStateCompleted, Output: runpkg.RunOutputMergeFastPathCheckedHead}
				if !orch.completeProgrammaticMergeWorkerResult(t.Context(), &state, event, running, issue) {
					t.Fatal("base refusal was not handled")
				}
			} else {
				orch.waitForMergeWorkerCurrentHeadCI(t.Context(), &state, event, running, issue)
			}
			if len(attempts.completions) != 1 {
				t.Fatalf("completions = %d, want 1", len(attempts.completions))
			}
			completion := attempts.completions[0]
			attempts.history = []store.WorkAttempt{{ID: 1, IssueID: issue.ID, Phase: completion.Phase, Status: completion.Status, TerminalState: completion.TerminalState, AttemptNumber: 2, CompletedAt: now, WorkerMetadataJSON: completion.WorkerMetadataJSON}}
			restarted := newState(cfg)
			orch.restoreDurableMergeReservations(t.Context(), &restarted, []connector.Issue{issue}, now.Add(tt.age))
			reservation := restarted.mergeReservations[issue.ID]
			if tt.want && (reservation.ExpiresAt != now.Add(mergeWorkerCurrentHeadCIWaitTimeout) || (reservation.RefreshHeadSHA != "") != tt.refresh) {
				t.Fatalf("restored reservation = %#v", reservation)
			}
			orch.restoreDurableMergeReservations(t.Context(), &restarted, []connector.Issue{issue}, now.Add(tt.age))
			if len(attempts.historyQueries) != 1 {
				t.Fatalf("repeated recovery queries = %d, want 1", len(attempts.historyQueries))
			}
		})
	}
}

func TestMergeReservationNeverBypassesUnsafeHead(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		mutate func(*connector.PullRequest)
	}{
		{name: "failed required check", mutate: func(pr *connector.PullRequest) { pr.CIStatus = "failure" }},
		{name: "required failure despite green aggregate", mutate: func(pr *connector.PullRequest) {
			pr.RequiredCheckFailures = []connector.PullRequestCheck{{Name: "test", Conclusion: "failure"}}
		}},
		{name: "conflict", mutate: func(pr *connector.PullRequest) { pr.MergeableState = "dirty" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC)
			cfg := normalizeConfig(Config{ActiveStates: []string{"Merging", "Rework"}, MergeFastPathEnabled: true})
			state := newState(cfg)
			issue := nativeMergeQueueTestIssue(2221, "success")
			reserveMergeCandidate(&state, issue, now)
			tt.mutate(issue.PullRequest)
			tracker := &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}}
			orch := Orchestrator{cfg: cfg, connector: tracker}
			event := runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge}, Result: runpkg.RunResult{FinalState: runpkg.FinalStateCompleted, Output: runpkg.RunOutputMergeFastPathCheckedHead}}
			if !orch.completeProgrammaticMergeWorkerResult(t.Context(), &state, event, Running{Issue: issue, Attempt: 1}, issue) {
				t.Fatal("unsafe result not handled")
			}
			if len(tracker.merges) != 0 {
				t.Fatal("unsafe head reached merge API")
			}
		})
	}
}

func TestMergeTrainDrainsWhileOtherHeadsWait(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name          string
		strict        bool
		external      bool
		wantRefreshes map[int]int
		wantMerged    []int
	}{
		{name: "protection bypass still requires integration", wantRefreshes: map[int]int{2221: 1, 2220: 2}, wantMerged: []int{2221, 2220}},
		{name: "strict protection", strict: true, wantRefreshes: map[int]int{2221: 1, 2220: 2}, wantMerged: []int{2221, 2220}},
		{name: "external base advance during CI", strict: true, external: true, wantRefreshes: map[int]int{2221: 3, 2220: 1}, wantMerged: []int{2220, 2221}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 5, 15, 12, 53, 0, time.UTC)
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 3, MaxConcurrentAgentsByState: map[string]int{"Merging": 1}, ActiveStates: []string{"Merging", "Todo"}, TerminalStates: []string{"Done"}, MergeFastPathEnabled: true, PrioritizeUnblockers: true, ContinuationRetryDelay: time.Second, FailureRetryBaseDelay: time.Second})
			selected := nativeMergeQueueTestIssue(2221, "success")
			selected.StageUpdatedAt = timePointer(now.Add(-3 * time.Minute))
			other := nativeMergeQueueTestIssue(2220, "success")
			other.StageUpdatedAt = timePointer(now.Add(-15 * time.Minute))
			for _, issue := range []*connector.Issue{&selected, &other} {
				issue.PullRequest.BaseSHA = "old-base"
				issue.PullRequest.MergeableState = "behind"
			}
			tracker := &mergeTrainConnector{autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{selected, other}}}, base: "base-0", strict: tt.strict, pending: map[string]int{}}
			backend := &mergeTrainWorkspace{mergeFastPathWorkspace: &mergeFastPathWorkspace{info: workspace.Info{Path: t.TempDir(), Branch: "detent/test"}}, tracker: tracker, refreshes: map[int]int{}}
			runner, err := runpkg.NewRunner(runpkg.Dependencies{Workflow: workflowconfig.Workflow{}, Workspace: backend, AgentBackend: &mergeFastPathAgentBackend{}, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			orch := Orchestrator{cfg: cfg, connector: tracker, logger: slog.New(slog.NewTextHandler(&logs, nil))}
			state := newState(cfg)
			state.Retry[other.ID] = Retry{Issue: cloneIssue(other), Attempt: 2, DueAt: now}
			advanced := false
			for tick := 0; tick < 40 && len(tracker.merged) < 2; tick++ {
				for index := range tracker.stateIssues {
					issue := &tracker.stateIssues[index]
					if remaining := tracker.pending[issue.ID]; remaining > 0 {
						tracker.pending[issue.ID]--
						if remaining == 1 {
							issue.PullRequest.CIStatus = "success"
						}
					}
					if issue.PullRequest.BaseSHA != tracker.base {
						issue.PullRequest.MergeableState = "behind"
					} else {
						issue.PullRequest.MergeableState = "clean"
					}
				}
				if tt.external && !advanced && tracker.pending[selected.ID] > 0 {
					tracker.base = "external-base"
					advanced = true
				}
				candidates := cloneIssues(issuesInStates(tracker.stateIssues, []string{"Merging"}))
				for n := range 3 {
					candidates = append(candidates, rankingDependencyIssue(fmt.Sprintf("dependent-%d", n), "Todo", selected.ID))
				}
				plan := newDispatchPlanner(cfg).plan(&state, candidates, now, dispatchPlanHooks{})
				for _, dispatch := range plan.Dispatches {
					running := state.Running[dispatch.IssueID]
					if !mergeWorkerIssue(running.Issue) {
						t.Fatalf("dependent dispatched before prerequisite: %#v", dispatch)
					}
					reservation := state.mergeReservations[running.Issue.ID]
					request := runpkg.RunRequest{Issue: running.Issue, Mode: runpkg.RunModeMerge, Attempt: running.Attempt, StartedAt: now, MergeRefreshHeadSHA: reservation.RefreshHeadSHA}
					running.Mode = runpkg.RunModeMerge
					state.Running[dispatch.IssueID] = running
					result, runErr := runner.Run(t.Context(), request)
					if runErr != nil {
						t.Fatal(runErr)
					}
					orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: dispatch.IssueID, CompletedAt: now, Request: request, Result: result})
				}
				if len(state.Running) > 0 || len(state.Claimed) > 0 {
					t.Fatal("merge CI wait retained a worker or claim")
				}
				now = now.Add(time.Minute)
			}
			if !reflect.DeepEqual(tracker.merged, tt.wantMerged) {
				t.Fatalf("merged = %v, want %v; logs: %s", tracker.merged, tt.wantMerged, logs.String())
			}
			if !reflect.DeepEqual(backend.refreshes, tt.wantRefreshes) {
				t.Fatalf("refreshes = %v, want %v", backend.refreshes, tt.wantRefreshes)
			}
			if strings.Contains(logs.String(), "reason=merge_api_rejected_out_of_date_base") {
				t.Fatalf("stale base reached merge API: %s", logs.String())
			}
		})
	}
}

type mergeTrainConnector struct {
	*autoPromoteTickMergeConnector
	base    string
	strict  bool
	merged  []int
	pending map[string]int
}

func (c *mergeTrainConnector) MergePullRequest(ctx context.Context, repository string, number int, headSHA, method string) error {
	for _, issue := range c.stateIssues {
		if pullRequestNumber(issue) != number {
			continue
		}
		if issue.PullRequest.HeadSHA != headSHA || !mergeWorkerCIGreen(issue.PullRequest.CIStatus) || len(issue.PullRequest.RequiredCheckFailures) > 0 {
			return errors.New("unsafe merge attempted")
		}
		if c.strict && issue.PullRequest.BaseSHA != c.base {
			return connector.ErrPullRequestBaseOutOfDate
		}
		if err := c.autoPromoteTickMergeConnector.MergePullRequest(ctx, repository, number, headSHA, method); err != nil {
			return err
		}
		c.merged = append(c.merged, number)
		c.base = fmt.Sprintf("merged-%d", number)
		return nil
	}
	return errors.New("pull request missing")
}

type mergeTrainWorkspace struct {
	*mergeFastPathWorkspace
	tracker   *mergeTrainConnector
	refreshes map[int]int
}

func (w *mergeTrainWorkspace) PrepareMerge(_ context.Context, _ workspace.Info, target workspace.Issue, _ workspace.MergePrepareOptions) (workspace.MergePrepareResult, error) {
	for index := range w.tracker.stateIssues {
		issue := &w.tracker.stateIssues[index]
		if issue.ID != target.ID {
			continue
		}
		w.refreshes[issue.PullRequest.Number]++
		issue.PullRequest.HeadSHA = fmt.Sprintf("refreshed-%d-%d", issue.PullRequest.Number, w.refreshes[issue.PullRequest.Number])
		issue.PullRequest.BaseSHA = w.tracker.base
		issue.PullRequest.CIStatus = "pending"
		issue.PullRequest.MergeableState = "clean"
		w.tracker.pending[issue.ID] = 4
		return workspace.MergePrepareResult{Status: workspace.MergePrepareStatusClean, HeadChanged: true}, nil
	}
	return workspace.MergePrepareResult{}, errors.New("pull request missing")
}
