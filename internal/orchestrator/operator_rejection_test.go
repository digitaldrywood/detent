package orchestrator

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestOperatorRejectionPromotion(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name           string
		head           string
		draft          bool
		observed       bool
		reason         string
		automated      bool
		otherPR        bool
		hydrateFailure bool
		readFailure    bool
		writeFailure   bool
		newCommit      bool
		want           bool
	}{
		{name: "unchanged rejected head", head: "rejected"},
		{name: "automated rework is not rejection", head: "rejected", automated: true, reason: "ci_not_green", want: true},
		{name: "different PR with same head", head: "rejected", otherPR: true, want: true},
		{name: "dashboard rejection", head: "rejected", reason: "kanban_move"},
		{name: "observed rejection", head: "rejected", observed: true},
		{name: "observed rejection then push", head: "fixed", observed: true, want: true},
		{name: "new head", head: "fixed", want: true},
		{name: "hydration fails but rejection holds", head: "rejected", hydrateFailure: true},
		{name: "observed move survives hydration failure", head: "rejected", observed: true, hydrateFailure: true},
		{name: "new commit after unknown rejection", head: "fixed", hydrateFailure: true, newCommit: true, want: true},
		{name: "unknown rejection needs evidence of new commit", head: "fixed", hydrateFailure: true},
		{name: "history read failure", head: "rejected", readFailure: true},
		{name: "history write failure", head: "rejected", writeFailure: true},
		{name: "observed history write failure", head: "rejected", observed: true, writeFailure: true},
		{name: "new commit after history write failure", head: "fixed", writeFailure: true, newCommit: true, want: true},
		{name: "new draft head", head: "fixed", draft: true},
	} {
		for _, completion := range []bool{false, true} {
			name := tt.name + "/tick"
			if completion {
				name = tt.name + "/completion"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
				cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Rework"}, AutoPromote: AutoPromoteConfig{Enabled: true, PassState: "Human Review", GateWaitState: "review", Gate: gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)}}})
				cfg.Project.ID = defaultWorkflowMetricsProjectID
				issue := autoPromoteTickIssue("rejected-head", nil, &connector.PullRequest{Number: 42, URL: "https://github.com/digitaldrywood/detent/pull/42", State: "OPEN", HeadSHA: "rejected", MergeableState: "CLEAN", CIStatus: "success"})
				db, _ := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, now)
				backend := &operatorRejectionStore{Store: db}
				thin := cloneIssue(issue)
				thin.PullRequest = nil
				tracker := &operatorRejectionConnector{liveReworkConnector: &liveReworkConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{thin}}, live: issue}}
				if tt.hydrateFailure {
					tracker.hydrateErr = errors.New("forge unavailable")
				}
				o := newLaneMutationTestOrchestrator(cfg, tracker, backend, nil, now)
				state := newState(cfg)
				if tt.observed {
					if _, _, err := o.observeLane(t.Context(), &state, thin, now.Add(-time.Minute)); err != nil {
						t.Fatal(err)
					}
					thin.State = "Rework"
					backend.writeFailure = tt.writeFailure
					if _, _, err := o.observeLane(t.Context(), &state, thin, now); err != nil {
						t.Fatal(err)
					}
				} else {
					backend.writeFailure = tt.writeFailure
					origin := provenance.OriginHuman
					if tt.automated {
						origin = provenance.OriginDetent
					}
					result := o.applyOperatorMove(t.Context(), &state, OperatorMoveRequest{WriteTracker: true, IssueID: issue.ID, Identifier: issue.Identifier, FromState: issue.State, ToState: "Rework", Reason: tt.reason, Attribution: provenance.Attribution{Origin: origin}}, now)
					if result.err != nil {
						t.Fatal(result.err)
					}
				}
				if !tt.observed && len(tracker.updates) != 1 {
					t.Fatal("operator move was not applied")
				}
				backend.writeFailure = false
				backend.readFailure = tt.readFailure
				tracker.hydrateErr = nil
				tracker.updates = nil
				issue.State = "Rework"
				issue.PullRequest.HeadSHA = tt.head
				issue.PullRequest.Draft = tt.draft
				if tt.newCommit {
					committed := now.Add(time.Second)
					issue.PullRequest.HeadCommittedAt = &committed
				}
				if tt.otherPR {
					issue.PullRequest.Number = 43
					issue.PullRequest.URL = "https://github.com/digitaldrywood/detent/pull/43"
				}
				tracker.stateIssues = []connector.Issue{issue}
				tracker.live = issue
				// Recreate the orchestrator and state: rejection must survive a restart.
				o = newLaneMutationTestOrchestrator(cfg, tracker, backend, nil, now)
				state = newState(cfg)
				if tt.readFailure || tt.writeFailure && !tt.newCommit {
					other := dispatchTestIssue("unrelated-rework", "Rework")
					planner := o.liveDispatchPlanner(t.Context())
					plan := planner.plan(&state, []connector.Issue{issue, other}, now.Add(time.Minute), dispatchPlanHooks{})
					if slices.Contains(plan.DispatchOrder(), issue.ID) || !slices.Contains(plan.DispatchOrder(), other.ID) {
						t.Fatalf("history failure dispatch=%v, want unrelated issue only", plan.DispatchOrder())
					}
				}
				for tick := range 2 {
					if completion {
						state.Completed[issue.ID] = Completed{Issue: issue, FinalState: FinalStateCompleted}
						o.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now.Add(time.Duration(tick+1)*time.Minute))
					} else {
						o.autoPromoteHumanReviewIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(time.Duration(tick+1)*time.Minute))
					}
				}
				if got := len(tracker.updates) > 0; got != tt.want {
					t.Fatalf("promoted=%v, want %v; updates=%v", got, tt.want, tracker.updates)
				}
			})
		}
	}
}

func TestOperatorRejectionRepairDispatch(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, head   string
		wantDispatch bool
	}{
		{name: "rejected head needs worker", head: "rejected", wantDispatch: true},
		{name: "new completed head waits for gate", head: "fixed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Rework"}, AutoPromote: AutoPromoteConfig{Enabled: true, GateWaitState: autoPromoteGateWaitSource, Gate: gate.Config{Kind: gate.KindCommand}}})
			issue := dispatchTestIssueWithPullRequest("repair-rejected", "Human Review", "OPEN")
			issue.PullRequest.HeadSHA = "rejected"
			issue.PullRequest.MergeableState = "clean"
			issue.PullRequest.CIStatus = "success"
			issue.PullRequest.LatestCodexReviewState = "COMMENTED"
			tracker := &autoPromoteTickConnector{}
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			o := &Orchestrator{cfg: cfg, connector: tracker, workflowMetrics: metrics}
			now := time.Now()
			o.recordLaneTransition(t.Context(), issue, "Rework", now, "operator_move", workflowLaneMetadata{})
			issue.State = "Rework"
			issue.PullRequest.HeadSHA = tt.head
			state := newState(cfg)
			state.Completed[issue.ID] = Completed{Issue: issue, FinalState: FinalStateCompleted}
			planner := o.liveDispatchPlanner(t.Context())
			plan := planner.plan(&state, []connector.Issue{issue}, now.Add(time.Minute), dispatchPlanHooks{})
			if got := len(plan.DispatchOrder()) > 0; got != tt.wantDispatch {
				t.Fatalf("dispatch=%v, want dispatch=%v", plan.DispatchOrder(), tt.wantDispatch)
			}
		})
	}
}

type operatorRejectionConnector struct {
	*liveReworkConnector
	hydrateErr error
}

func (c *operatorRejectionConnector) HydratePullRequest(ctx context.Context, issue connector.Issue) (connector.Issue, error) {
	if c.hydrateErr != nil {
		return connector.Issue{}, c.hydrateErr
	}
	return c.liveReworkConnector.HydratePullRequest(ctx, issue)
}

type operatorRejectionStore struct {
	store.Store
	readFailure  bool
	writeFailure bool
}

func (s *operatorRejectionStore) IssueWorkflowTimeline(ctx context.Context, issue store.IssueIdentity) (store.WorkflowTimeline, error) {
	if s.readFailure {
		return store.WorkflowTimeline{}, errors.New("history unavailable")
	}
	return s.Store.IssueWorkflowTimeline(ctx, issue)
}

func (s *operatorRejectionStore) RecordWorkflowPhaseEvent(ctx context.Context, event store.WorkflowPhaseEvent) (int64, error) {
	if s.writeFailure {
		return 0, errors.New("history write failed")
	}
	return s.Store.RecordWorkflowPhaseEvent(ctx, event)
}
