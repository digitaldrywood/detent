package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/coordination"
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
		{"defaults return parks", nil, workflowconfig.OperatorActionReturnRetiredParks, true},
		{"defaults clear dependencies", nil, workflowconfig.OperatorActionClearClosedDependencies, true},
		{"defaults restore merging", nil, workflowconfig.OperatorActionRestoreStuckMerging, true},
		{"defaults exclude merge when wedged", nil, workflowconfig.OperatorActionMergeWhenWedged, false},
		{"explicit empty disables everything", []string{}, workflowconfig.OperatorActionReturnRetiredParks, false},
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
		{"queue entry present", func(i *connector.Issue, s *State) {
			s.nativeMergeQueueEntries[i.ID] = nativeMergeQueueEntry{Entry: connector.PullRequestMergeQueueEntry{ID: "MQE"}, HeadSHA: i.PullRequest.HeadSHA, CheckedAt: now}
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

func TestOperatorMergeWedgedPullRequestsMergesAndRecords(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 11, 18, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name       string
		actions    []string
		wantMerges int
	}{
		{"enabled", []string{workflowconfig.OperatorActionMergeWhenWedged}, 1},
		{"default off", nil, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := operatorWedgedIssue(now)
			tracker := &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}}
			cfg := nativeMergeQueueTestConfig(Config{ActiveStates: []string{"Merging"}, TerminalStates: []string{"Done"}, MergeMethod: "squash", Operator: OperatorConfig{Actions: tt.actions}})
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			transitioned := orch.operatorMergeWedgedPullRequests(t.Context(), &state, []connector.Issue{issue}, now)
			if len(tracker.merges) != tt.wantMerges || len(transitioned) != tt.wantMerges {
				t.Fatalf("merges=%#v transitioned=%v", tracker.merges, transitioned)
			}
			if tt.wantMerges == 0 {
				return
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
