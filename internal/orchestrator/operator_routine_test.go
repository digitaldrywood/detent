package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/coordination"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
)

type recordingLaneLedger struct {
	mu      sync.Mutex
	actions []string
}

func (l *recordingLaneLedger) LaneWriteLock() sync.Locker { return &l.mu }
func (l *recordingLaneLedger) PrepareLaneWrite(_ context.Context, _ string, write coordination.LaneWrite) (coordination.LaneWrite, error) {
	return write, nil
}
func (l *recordingLaneLedger) ResolveLaneWrite(context.Context, uint64, string) error { return nil }
func (l *recordingLaneLedger) LatestLaneWrite(context.Context, store.IssueIdentity) (coordination.LaneWrite, string, error) {
	return coordination.LaneWrite{}, "", store.ErrNotFound
}
func (l *recordingLaneLedger) LaneObservation(context.Context, store.IssueIdentity) (store.LaneObservation, error) {
	return store.LaneObservation{}, store.ErrNotFound
}
func (l *recordingLaneLedger) SaveLaneObservation(context.Context, store.IssueIdentity, store.LaneObservation) error {
	return nil
}
func (l *recordingLaneLedger) RecordLaneWriteAction(_ context.Context, token uint64, origin, kind string) error {
	l.actions = append(l.actions, origin+"/"+kind+"/"+time.Duration(token).String())
	return nil
}

func TestOperatorConfigActionEnabled(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		actions []string
		kind    string
		want    bool
	}{
		{"defaults clear dependencies", nil, workflowconfig.OperatorActionClearClosedDependencies, true},
		{"defaults restore merging", nil, workflowconfig.OperatorActionRestoreStuckMerging, true},
		{"defaults exclude merge when wedged", nil, workflowconfig.OperatorActionMergeWhenWedged, false},
		{"explicit empty disables everything", []string{}, workflowconfig.OperatorActionClearClosedDependencies, false},
		{"explicit list", []string{"Merge_When_Wedged"}, workflowconfig.OperatorActionMergeWhenWedged, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := (OperatorConfig{Actions: tt.actions}).actionEnabled(tt.kind); got != tt.want {
				t.Fatalf("actionEnabled(%s) = %t, want %t", tt.kind, got, tt.want)
			}
		})
	}
}

func TestRunOperatorActionStampsAppliedLaneWrites(t *testing.T) {
	t.Parallel()
	ledger := &recordingLaneLedger{}
	orch := &Orchestrator{cfg: Config{Operator: OperatorConfig{Actions: []string{workflowconfig.OperatorActionRestoreStuckMerging}}}, laneLedger: ledger, laneWriteResults: map[string]string{}}
	ran := false
	orch.runOperatorAction(workflowconfig.OperatorActionRestoreStuckMerging, func() map[string]struct{} {
		ran = true
		if err := orch.resolveLaneWrite(t.Context(), coordination.LaneWrite{Issue: "i", FenceToken: 7}, "applied"); err != nil {
			t.Fatal(err)
		}
		if err := orch.resolveLaneWrite(t.Context(), coordination.LaneWrite{Issue: "j", FenceToken: 8}, "failed"); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	if !ran || len(ledger.actions) != 1 || ledger.actions[0] != "operator_routine/restore_stuck_merging/7ns" {
		t.Fatalf("ran=%t actions=%v", ran, ledger.actions)
	}
	if orch.laneWriteOrigin != "" || orch.laneWriteAction != "" {
		t.Fatalf("origin not cleared: %q/%q", orch.laneWriteOrigin, orch.laneWriteAction)
	}
	if err := orch.resolveLaneWrite(t.Context(), coordination.LaneWrite{Issue: "k", FenceToken: 9}, "applied"); err != nil || len(ledger.actions) != 1 {
		t.Fatalf("write outside the routine was stamped: %v %v", err, ledger.actions)
	}
	orch.runOperatorAction(workflowconfig.OperatorActionMergeWhenWedged, func() map[string]struct{} {
		t.Fatal("disabled action ran")
		return nil
	})
}

func operatorWedgedIssue(now time.Time) connector.Issue {
	issue := nativeMergeQueueTestIssue(970, "success")
	stage := now.Add(-3 * time.Hour)
	issue.StageUpdatedAt = &stage
	return issue
}

func TestOperatorMergeWedgedCandidate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 11, 18, 0, 0, 0, time.UTC)
	cfg := nativeMergeQueueTestConfig(Config{ActiveStates: []string{"Merging"}, TerminalStates: []string{"Done"}})
	for _, tt := range []struct {
		name   string
		mutate func(*connector.Issue, *State)
		want   bool
	}{
		{"green stale merging head", func(*connector.Issue, *State) {}, true},
		{"not merging", func(i *connector.Issue, _ *State) { i.State = "Rework" }, false},
		{"draft", func(i *connector.Issue, _ *State) { i.PullRequest.Draft = true }, false},
		{"ci pending", func(i *connector.Issue, _ *State) { i.PullRequest.CIStatus = "pending" }, false},
		{"unresolved thread", func(i *connector.Issue, _ *State) {
			i.PullRequest.UnresolvedReviewThreads = []connector.PullRequestReviewThread{{Body: "please fix"}}
		}, false},
		{"dirty", func(i *connector.Issue, _ *State) { i.PullRequest.MergeableState = "dirty" }, false},
		{"too young", func(i *connector.Issue, _ *State) { stage := now.Add(-time.Hour); i.StageUpdatedAt = &stage }, false},
		{"unknown age", func(i *connector.Issue, _ *State) { i.StageUpdatedAt = nil }, false},
		{"provider queue entry present", func(i *connector.Issue, _ *State) {
			i.PullRequest.MergeQueueEntry = &connector.PullRequestMergeQueueEntry{ID: "MQE"}
		}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := operatorWedgedIssue(now)
			state := newState(cfg)
			tt.mutate(&issue, &state)
			if got := operatorMergeWedgedCandidate(issue, &state, cfg, now); got != tt.want {
				t.Fatalf("candidate = %t, want %t", got, tt.want)
			}
		})
	}
}

type operatorQueueInspectingConnector struct {
	*autoPromoteTickMergeConnector
	status connector.PullRequestMergeQueueStatus
	err    error
}

func (c *operatorQueueInspectingConnector) InspectPullRequestMergeQueue(context.Context, connector.Issue) (connector.PullRequestMergeQueueStatus, error) {
	return c.status, c.err
}

func TestOperatorMergeWedgedPullRequestsMergesAndRecords(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 4, 59, 0, 0, time.UTC)
	deliveredAt := now.Add(5 * time.Minute)
	for _, tt := range []struct {
		name        string
		actions     []string
		wantMerges  int
		mergeError  error
		queueStatus *connector.PullRequestMergeQueueStatus
		queueError  error
	}{
		{"enabled", []string{workflowconfig.OperatorActionMergeWhenWedged}, 1, nil, nil, nil},
		{"default off", nil, 0, nil, nil, nil},
		{"failed merge", []string{workflowconfig.OperatorActionMergeWhenWedged}, 1, errors.New("merge failed"), nil, nil},
		{"provider reports queued", []string{workflowconfig.OperatorActionMergeWhenWedged}, 0, nil, &connector.PullRequestMergeQueueStatus{Available: true, Entry: &connector.PullRequestMergeQueueEntry{ID: "MQE"}}, nil},
		{"provider queue inspection fails", []string{workflowconfig.OperatorActionMergeWhenWedged}, 0, nil, &connector.PullRequestMergeQueueStatus{}, errors.New("queue read unavailable")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clock := now
			issue := operatorWedgedIssue(now)
			tracker := &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}}
			cfg := nativeMergeQueueTestConfig(Config{ActiveStates: []string{"Merging"}, TerminalStates: []string{"Done"}, MergeMethod: "squash", Operator: OperatorConfig{Actions: tt.actions}})
			tracker.err = tt.mergeError
			tracker.afterMerge = func() { clock = deliveredAt }
			var backend connector.Connector = tracker
			if tt.queueStatus != nil || tt.queueError != nil {
				status := connector.PullRequestMergeQueueStatus{}
				if tt.queueStatus != nil {
					status = *tt.queueStatus
				}
				backend = &operatorQueueInspectingConnector{autoPromoteTickMergeConnector: tracker, status: status, err: tt.queueError}
			}
			orch := &Orchestrator{cfg: cfg, connector: backend, now: func() time.Time { return clock }}
			state := newState(cfg)
			transitioned := orch.operatorMergeWedgedPullRequests(t.Context(), &state, []connector.Issue{issue}, now)
			wantTransitions := tt.wantMerges
			if tt.mergeError != nil {
				wantTransitions = 0
			}
			if len(tracker.merges) != tt.wantMerges || len(transitioned) != wantTransitions {
				t.Fatalf("merges=%#v transitioned=%v", tracker.merges, transitioned)
			}
			if wantTransitions == 0 {
				if len(tracker.updates) != 0 || len(orch.laneWrites) != 0 || len(state.MergeTimings) != 0 {
					t.Fatal("failed or disabled merge recorded delivery")
				}
				return
			}
			write := orch.laneWrites[issue.ID]
			if !write.WrittenAt.Equal(deliveredAt) || !state.MergeTimings[issue.ID].MergedAt.Equal(deliveredAt) {
				t.Fatalf("delivery write=%s merge=%s, want %s", write.WrittenAt, state.MergeTimings[issue.ID].MergedAt, deliveredAt)
			}
			if tracker.merges[0].method != "squash" || tracker.merges[0].headSHA != issue.PullRequest.HeadSHA {
				t.Fatalf("merge = %#v", tracker.merges[0])
			}
			if len(tracker.updates) != 1 || tracker.updates[0].state != "Done" {
				t.Fatalf("updates = %#v, want Done", tracker.updates)
			}
			if len(state.RecentEvents) == 0 || !strings.Contains(state.RecentEvents[len(state.RecentEvents)-1].Message, "operator routine merged") {
				t.Fatalf("activity = %#v", state.RecentEvents)
			}
		})
	}
}

func TestOperatorMergeWedgedSkipsFailedSecurityAudit(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 5, 0, 0, 0, time.UTC)
	orch, tracker, issue := mergingSecurityAuditFixture()
	stage := now.Add(-3 * time.Hour)
	issue.StageUpdatedAt = &stage
	tracker.stateIssues = []connector.Issue{issue}
	orch.cfg.Operator.Actions = []string{workflowconfig.OperatorActionMergeWhenWedged}
	orch.now = func() time.Time { return now }
	run := securityAuditPassingRun(issue)
	run.Verdict = securityaudit.VerdictFail
	run.Findings = []securityaudit.Finding{{ID: "authz", Severity: "p1", Body: "critical finding"}}
	if _, err := orch.securityAuditStore.RecordSecurityAuditRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	state := newState(orch.cfg)
	orch.tick(t.Context(), &state, now)
	if len(tracker.merges) != 0 || len(tracker.updates) != 0 || len(orch.laneWrites) != 0 || len(state.MergeTimings) != 0 {
		t.Fatalf("failed audit allowed wedged merge: merges=%#v updates=%#v writes=%#v timings=%#v", tracker.merges, tracker.updates, orch.laneWrites, state.MergeTimings)
	}
}

func TestTickDoesNotAutoPromoteGitHubPullRequests(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		passState string
	}{
		{name: "default_merging_pass_state", passState: "Merging"},
		{name: "custom_pass_state", passState: "Ready for Pickup"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			readyTodo := autoPromoteTickIssue("todo-ready-pr", nil, &connector.PullRequest{
				Number: 101, URL: "https://github.test/example-org/example-project/pull/101",
				State: "OPEN", HeadSHA: "head-101", MergeableState: "clean", CIStatus: "success",
			})
			readyTodo.State = "Todo"
			completedActive := autoPromoteTickIssue("active-completed-pr", nil, &connector.PullRequest{
				Number: 102, URL: "https://github.test/example-org/example-project/pull/102",
				State: "OPEN", HeadSHA: "head-102", MergeableState: "clean", CIStatus: "success",
			})
			completedActive.State = "In Progress"
			timedOutActive := autoPromoteTickIssue("active-timeout-pr", nil, &connector.PullRequest{
				Number: 103, URL: "https://github.test/example-org/example-project/pull/103",
				State: "OPEN", HeadSHA: "head-103", MergeableState: "clean", CIStatus: "success",
			})
			timedOutActive.State = "In Progress"
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{readyTodo, completedActive, timedOutActive}}
			cfg := normalizeConfig(Config{
				ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
				TerminalStates: []string{"Done"},
				AutoPromote: AutoPromoteConfig{
					Enabled:               true,
					SourceState:           "Human Review",
					PassState:             tc.passState,
					GateWaitTimeout:       15 * time.Minute,
					GateWaitTimeoutAction: "merge",
					Gate:                  gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)},
				},
			})
			orch := &Orchestrator{cfg: cfg, connector: tracker, now: func() time.Time { return now }}
			state := newState(cfg)
			state.Completed[completedActive.ID] = Completed{Issue: cloneIssue(completedActive), FinalState: FinalStateCompleted, CompletedAt: now.Add(-time.Minute)}
			state.Completed[timedOutActive.ID] = Completed{Issue: cloneIssue(timedOutActive), FinalState: FinalStateCompleted, CompletedAt: now.Add(-30 * time.Minute)}
			orch.tick(t.Context(), &state, now)
			for _, update := range tracker.updates {
				if update.state == tc.passState || update.state == "Merging" {
					t.Fatalf("tick auto-promoted PR to %q: %+v", update.state, tracker.updates)
				}
			}
			if len(state.Running) != 0 {
				t.Fatalf("tick dispatched merge worker for PR: %+v", state.Running)
			}
		})
	}
}

func TestTickReconcilesStaleTodoHydratesWorkpadBlockerBeforePromotion(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 6, 16, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			PassState:     "Ready for Pickup",
			Gate:          gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	blocked := autoPromoteTickIssue("issue-stale-todo-workpad-blocker", []string{"bug"}, &connector.PullRequest{
		Number:                 180,
		URL:                    "https://github.test/example-org/example-project/pull/180",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "pass",
		CodexReviewSubmittedAt: &oldReview,
	})
	blocked.State = "Todo"
	blocked.Identifier = "example-org/example-project#175"
	clean := autoPromoteTickIssue("issue-stale-todo-clean", []string{"bug"}, &connector.PullRequest{
		Number:                 181,
		URL:                    "https://github.test/example-org/example-project/pull/181",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "pass",
		CodexReviewSubmittedAt: &oldReview,
	})
	clean.State = "Todo"
	clean.Identifier = "example-org/example-project#176"
	tracker := &autoPromoteTickConnector{
		stateIssues: []connector.Issue{blocked, clean},
		issueComments: map[string][]connector.IssueComment{
			blocked.ID: {{
				Body: "## Codex Workpad\n```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Approve copying the assets\n```",
			}},
		},
	}
	orch := &Orchestrator{cfg: cfg, connector: tracker, now: func() time.Time { return now }}
	state := newState(cfg)
	orch.tick(t.Context(), &state, now)

	wantUpdates := []autoPromoteTickUpdate{
		{issueID: blocked.ID, state: "Blocked"},
		{issueID: clean.ID, state: "Human Review"},
	}
	if !reflect.DeepEqual(tracker.updates, wantUpdates) {
		t.Fatalf("updates = %#v, want %#v", tracker.updates, wantUpdates)
	}
	if got, want := tracker.fetchComments, []string{blocked.ID, clean.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("FetchIssueComments issue IDs = %#v, want %#v", got, want)
	}
}
