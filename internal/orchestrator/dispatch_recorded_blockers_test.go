package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestDispatchRecordedPullRequestBlocker(t *testing.T) {
	for _, tt := range []struct {
		name    string
		missing bool
		retry   bool
	}{
		{name: "open PR across ten ticks"},
		{name: "unverifiable reference", missing: true},
		{name: "due retry", retry: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}})
			issue := dispatchTestIssue("2470", "Todo")
			issue.Identifier = "digitaldrywood/detent#2470"
			issue.Fields = map[string]string{"Status": "Todo"}
			issue.DependencySource = connector.BlockedRefSourceNative
			issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, Blockers: []workpad.Blocker{typedTestBlocker(workpad.Predicate{Type: workpad.PredicatePullRequestState, Identifier: "digitaldrywood/detent#2635", States: []string{"open"}})}}
			blocker := connector.Issue{ID: "2635", Identifier: "digitaldrywood/detent#2635", State: "In Progress", PullRequest: &connector.PullRequest{State: "open"}}
			tracker := &blockerEvidenceTestConnector{dependencyAutoUnblockConnector: &dependencyAutoUnblockConnector{hydratedIssues: []connector.Issue{issue}, blockers: []connector.Issue{blocker}}}
			if tt.missing {
				tracker.blockers = nil
			}
			runner := newWorkerHostRunner()
			orch := Orchestrator{cfg: cfg, connector: tracker, supervisor: newTestSupervisor(t, runner, cfg), runResults: make(chan runpkg.Completion)}
			state := newState(cfg)
			now := time.Now()
			savedRetry := Retry{Issue: issue, Attempt: 3, DueAt: now.Add(-time.Minute), RetryMode: runpkg.RetryModeResume, ResumeState: store.AgentResumeState{ProviderThreadID: "saved-thread"}, MergePrecheck: &runpkg.MergePrecheck{HeadSHA: "saved-head"}}
			if tt.retry {
				state.Retry[issue.ID] = savedRetry
			}
			for tick := range 10 {
				orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(time.Duration(tick)*time.Minute))
				if len(state.Running) != 0 {
					t.Fatalf("tick %d launched a worker while PR blocker remained unresolved", tick)
				}
				if tt.retry && !reflect.DeepEqual(state.Retry[issue.ID], savedRetry) {
					t.Fatalf("tick %d lost saved retry: %+v", tick, state.Retry)
				}
				if len(state.Blocked) != 0 {
					t.Fatal("dependency wait created a park")
				}
			}
			blocker.PullRequest = &connector.PullRequest{State: "merged"}
			tracker.blockers = []connector.Issue{blocker}
			orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(11*time.Minute))
			if len(state.Running) != 1 {
				t.Fatal("cleared PR blocker did not release dispatch")
			}
		})
	}
}

type queuedRecordedBlockerConnector struct {
	*blockerEvidenceTestConnector
	fresh connector.Issue
}

func (c *queuedRecordedBlockerConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return []connector.Issue{c.fresh}, nil
}

func TestQueuedDispatchRechecksRecordedBlockers(t *testing.T) {
	for _, prState := range []string{"open", "merged"} {
		t.Run(prState, func(t *testing.T) {
			now := time.Now()
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "project"}, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}})
			issue := dispatchTestIssue("2470", "Todo")
			issue.Identifier = "digitaldrywood/detent#2470"
			issue.DependencySource = connector.BlockedRefSourceNative
			issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, Blockers: []workpad.Blocker{typedTestBlocker(workpad.Predicate{Type: workpad.PredicatePullRequestState, Identifier: "digitaldrywood/detent#2635", States: []string{"open"}})}}
			fresh := cloneIssue(issue)
			fresh.WorkpadSignal = nil
			blocker := connector.Issue{ID: "2635", Identifier: "digitaldrywood/detent#2635", PullRequest: &connector.PullRequest{State: "merged"}}
			tracker := &queuedRecordedBlockerConnector{blockerEvidenceTestConnector: &blockerEvidenceTestConnector{dependencyAutoUnblockConnector: &dependencyAutoUnblockConnector{hydratedIssues: []connector.Issue{issue}, blockers: []connector.Issue{blocker}}}, fresh: fresh}
			gate := scheduler.NewGlobalDispatchGate(scheduler.NewStrictPriority(scheduler.Config{Capacity: 1}))
			held, ok, err := gate.TryAcquire(t.Context(), scheduler.ProjectCandidate{ID: "holder"}, scheduler.SlotRequest{State: "Todo"}, now)
			if err != nil || !ok {
				t.Fatalf("hold slot: %t %v", ok, err)
			}
			o := Orchestrator{cfg: cfg, connector: tracker, globalDispatchGate: gate, globalDispatchReady: make(chan struct{}, 1), globalDispatchPending: make(map[string]pendingGlobalDispatch), supervisor: newTestSupervisor(t, FakeRunner{}, cfg), runResults: make(chan runpkg.Completion, 1)}
			state := newState(cfg)
			defer o.releaseRunningSlots(&state)
			defer o.cancelPendingGlobalDispatches()
			o.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now)
			if len(o.globalDispatchPending) != 1 {
				t.Fatalf("not queued: %+v", state.SchedulerDecisions)
			}
			tracker.blockers[0].PullRequest = &connector.PullRequest{State: prState}
			if err := gate.Release(held); err != nil {
				t.Fatal(err)
			}
			o.dispatchGrantedRequests(t.Context(), &state, now.Add(time.Second))
			wantRunning := prState == "merged"
			if (len(state.Running) == 1) != wantRunning {
				t.Fatalf("running = %d for PR %s", len(state.Running), prState)
			}
			if !wantRunning && gate.PoolSnapshot().Used != 0 {
				t.Fatal("blocked grant leaked capacity")
			}
		})
	}
}

type commentRecordedBlockerConnector struct {
	*blockerEvidenceTestConnector
	comments   []connector.IssueComment
	commentErr error
}

func (c *commentRecordedBlockerConnector) FetchIssueComments(context.Context, connector.Issue) ([]connector.IssueComment, error) {
	return c.comments, c.commentErr
}

func TestDispatchLoadsRecordedBlockerComments(t *testing.T) {
	for _, tt := range []struct {
		name      string
		readError bool
	}{
		{name: "Todo comments omitted by state read"},
		{name: "comment read unavailable", readError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}})
			issue := dispatchTestIssue("2470", "Todo")
			issue.Identifier = "digitaldrywood/detent#2470"
			issue.Fields = map[string]string{"Status": "Todo"}
			issue.DependencySource = connector.BlockedRefSourceNative
			blocker := connector.Issue{ID: "2635", Identifier: "digitaldrywood/detent#2635", PullRequest: &connector.PullRequest{State: "open"}}
			tracker := &commentRecordedBlockerConnector{blockerEvidenceTestConnector: &blockerEvidenceTestConnector{dependencyAutoUnblockConnector: &dependencyAutoUnblockConnector{blockers: []connector.Issue{blocker}}}, comments: []connector.IssueComment{{Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: digitaldrywood/detent#2635\n    reason: waiting for PR\n    owner: orchestrator\n    predicate:\n      type: pull_request_state\n      states: [open]\n    recheck_interval: tick\nhuman_action: null\n```"}}}
			if tt.readError {
				tracker.commentErr = errors.New("comment service unavailable")
			}
			o := Orchestrator{cfg: cfg, connector: tracker, supervisor: newTestSupervisor(t, FakeRunner{}, cfg), runResults: make(chan runpkg.Completion, 1)}
			state := newState(cfg)
			now := time.Now()
			o.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now)
			if len(state.Running) != 0 {
				t.Fatal("dispatched without clear Workpad evidence")
			}
			if tt.readError {
				if got := state.SchedulerDecisions[0].Reason; got != dispatchSkipTrackerUnavailable {
					t.Fatalf("skip reason = %s, want tracker unavailability", got)
				}
			}
			tracker.commentErr = nil
			tracker.blockers[0].PullRequest = &connector.PullRequest{State: "merged"}
			o.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(time.Minute))
			if len(state.Running) != 1 {
				t.Fatal("cleared comments did not release dispatch")
			}
		})
	}
}

func TestDispatchCommentFailureRecordsInstanceEvidence(t *testing.T) {
	for _, retrying := range []bool{false, true} {
		t.Run(fmt.Sprint("retry=", retrying), func(t *testing.T) {
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}})
			issue := dispatchTestIssue("2470", "Todo")
			tracker := &commentRecordedBlockerConnector{blockerEvidenceTestConnector: &blockerEvidenceTestConnector{dependencyAutoUnblockConnector: &dependencyAutoUnblockConnector{}}, commentErr: trackerAvailabilityTestError("issue_comments", connector.TrackerAvailabilityClassServer)}
			o := Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			now := time.Now()
			saved := Retry{Issue: issue, Attempt: 4, DueAt: now.Add(-time.Minute), ResumeState: store.AgentResumeState{ProviderThreadID: "saved"}}
			for tick := range 2 {
				if retrying {
					state.Retry[issue.ID] = saved
					_, _, reason := o.liveDispatchPlanner(t.Context()).retryAction(&state, issue, saved, now.Add(time.Duration(tick)*time.Second))
					if reason != dispatchSkipTrackerUnavailable {
						t.Fatalf("reason = %s", reason)
					}
					if !reflect.DeepEqual(state.Retry[issue.ID], saved) {
						t.Fatal("tracker failure discarded retry")
					}
				} else {
					decision := o.liveDispatchPlanner(t.Context()).dispatchableIssueDecision(issue, &state, false, now.Add(time.Duration(tick)*time.Second), "")
					if decision.reason != dispatchSkipTrackerUnavailable {
						t.Fatalf("decision = %+v", decision)
					}
				}
			}
			if state.TrackerUnavailable == nil || state.TrackerUnavailable.Operation != "issue_comments" {
				t.Fatalf("missing instance outage: %+v", state.TrackerUnavailable)
			}
			if len(state.Blocked) != 0 {
				t.Fatal("tracker failure parked issue")
			}
		})
	}
}

func TestRecordedBlockerDispatchOwnership(t *testing.T) {
	for _, retry := range []bool{false, true} {
		for _, tc := range []struct {
			name, owner, action string
			wantDispatch        bool
		}{
			{name: "instance report", owner: workpad.BlockerOwnerInstance, wantDispatch: true},
			{name: "human report", owner: workpad.BlockerOwnerHuman},
			{name: "instance and human action", owner: workpad.BlockerOwnerInstance, action: "make skill available or waive it"},
		} {
			t.Run(fmt.Sprintf("%s/retry=%t", tc.name, retry), func(t *testing.T) {
				cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Rework"}})
				issue := dispatchTestIssue("2802", "Rework")
				issue.Fields = map[string]string{"Status": "Rework"}
				issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, HumanAction: tc.action, Blockers: []workpad.Blocker{{Ref: "instance:ci-runner-hook", Owner: tc.owner, Reason: "runner hook unavailable", Unverifiable: true}}}
				tracker := &blockerEvidenceTestConnector{dependencyAutoUnblockConnector: &dependencyAutoUnblockConnector{hydratedIssues: []connector.Issue{issue}}}
				o := Orchestrator{cfg: cfg, connector: tracker, supervisor: newTestSupervisor(t, FakeRunner{}, cfg), runResults: make(chan runpkg.Completion, 1)}
				state := newState(cfg)
				now := time.Now()
				if retry {
					state.Retry[issue.ID] = Retry{Issue: issue, Attempt: 2, DueAt: now.Add(-time.Minute)}
				}
				o.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now)
				if (len(state.Running) > 0) != tc.wantDispatch {
					t.Fatalf("running=%d decisions=%+v", len(state.Running), state.SchedulerDecisions)
				}
				if !tc.wantDispatch {
					if len(state.SchedulerDecisions) == 0 {
						t.Fatal("missing scheduler decision")
					}
					d := state.SchedulerDecisions[0]
					want := "runner hook unavailable"
					if tc.action != "" {
						want = tc.action
					}
					if d.Reason != dispatchSkipBlockedByDependency || !strings.Contains(d.WaitReason, want) {
						t.Fatalf("decision=%+v, want evidence %q", d, want)
					}
				}
			})
		}
	}
}

func TestWorkpadHumanActionSnapshot(t *testing.T) {
	now := time.Now().UTC()
	at := now.Add(-time.Hour)
	for _, tc := range []struct {
		name, status, action string
		want                 bool
		prose                bool
		state                string
	}{
		{name: "outstanding", status: workpad.StatusBlocked, action: "make skill available or waive it", want: true},
		{name: "resumed", status: workpad.StatusInProgress, action: "make skill available or waive it"},
		{name: "empty", status: workpad.StatusBlocked, action: "  "},
		{name: "prose-only Rework", prose: true, state: "Rework"},
		{name: "running with stale prose", prose: true, state: "In Progress"},
		{name: "legacy prose Workpad", prose: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := dispatchTestIssue("2802", "Rework")
			issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: tc.status, HumanAction: tc.action, RecordedAt: &at}
			if tc.prose {
				issue.StageUpdatedAt = &at
				issue.BlockerReason = "waiting on #123 to merge"
				issue.WorkpadSignal = nil
				if tc.state != "" {
					issue.State = tc.state
				} else {
					issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceProse, HumanAction: issue.BlockerReason}
				}
			}
			o := Orchestrator{}
			evaluated := o.evaluateRecordedBlockers(t.Context(), nil, issue, nil, now)
			snapshot := telemetryIssue(issue, 0, 0, now, nil)
			if (snapshot.WorkpadHumanAction != nil) != tc.want {
				t.Fatalf("snapshot=%+v", snapshot.WorkpadHumanAction)
			}
			if tc.want && (len(evaluated.Evidence) != 1 || !reflect.DeepEqual(*snapshot.WorkpadHumanAction, evaluated.Evidence[0]) || snapshot.WorkpadHumanAction.AgeSeconds != 3600) {
				t.Fatalf("snapshot=%+v evaluation=%+v", snapshot.WorkpadHumanAction, evaluated)
			}
		})
	}
}
