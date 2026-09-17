package orchestrator

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/store"
)

// Exercise real label discovery with the tick and lane writer, without PR
// hydration: closure alone must reach the existing reconciliation owner.
func TestTickReconcilesClosedLabelsWithoutPreviousPipeline(t *testing.T) {
	for _, tc := range []struct {
		name, lane   string
		previousTick bool
	}{
		{"fresh merging", "Merging", false},
		{"merging and closure between ticks", "Merging", true},
		{"closed todo", "Todo", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var visible atomic.Bool
			visible.Store(!tc.previousTick)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/repos/digitaldrywood/detent/issues" || r.URL.Query().Get("state") != "all" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					http.Error(w, "unexpected request", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if !visible.Load() || r.URL.Query().Get("labels") != "detent:"+normalizeState(tc.lane) {
					fmt.Fprint(w, `[]`)
					return
				}
				fmt.Fprintf(w, `[{"node_id":"I_2813","number":2813,"title":"Closed lane card","state":"closed","state_reason":"completed","labels":[{"name":"detent:%s"}]}]`, normalizeState(tc.lane))
			}))
			defer server.Close()
			reader, err := github.NewConnector(github.Config{
				Endpoint:           server.URL + "/graphql",
				Repository:         "digitaldrywood/detent",
				GitHubStatusSource: github.GitHubStatusSourceLabel,
				ActiveStates:       []string{"Todo", "Merging"},
				TerminalStates:     []string{"Done", "Cancelled"},
				TokenSource:        github.StaticTokenSource("test"),
				HTTPClient:         server.Client(),
			})
			if err != nil {
				t.Fatal(err)
			}
			tracker := &closedLabelRefreshConnector{reader: reader}
			orch := newStatusReconcileOrchestrator(&tracker.statusReconcileConnector)
			orch.connector = tracker
			orch.cfg.ActiveStates = []string{"Todo", "Merging"}
			state := newState(orch.cfg)
			now := time.Date(2026, 9, 17, 6, 42, 30, 0, time.UTC)
			if tc.previousTick {
				orch.tick(t.Context(), &state, now)
				if len(state.Pipeline) != 0 {
					t.Fatalf("previous pipeline = %#v", state.Pipeline)
				}
				// The issue enters Merging at 06:42:45 and closes at 06:43:00,
				// entirely between refreshes, so no prior pipeline can retain it.
				visible.Store(true)
			}
			now = now.Add(time.Minute)
			orch.tick(t.Context(), &state, now)
			if state.LastRefreshError != "" {
				t.Fatal(state.LastRefreshError)
			}
			if got := tracker.updates; len(got) != 1 || got[0] != (statusUpdate{issueID: "I_2813", state: "Done"}) {
				t.Fatalf("updates = %#v, want Done", got)
			}
			found := false
			for _, event := range state.RecentEvents {
				if event.Event == "closed_completed_status_reconciled" && event.At.Equal(now) {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing closure reconciliation event: %#v", state.RecentEvents)
			}
			if len(state.Running) != 0 || len(state.SchedulerDecisions) != 0 || state.DispatchStatus.EligibleCandidateCount != 0 {
				t.Fatalf("closed issue entered dispatch: %#v", state.DispatchStatus)
			}
			if tracker.fetchByIDCount != 0 {
				t.Fatalf("direct-ID reads = %d, want no previous membership", tracker.fetchByIDCount)
			}
		})
	}
}

type closedLabelRefreshConnector struct {
	statusReconcileConnector
	reader *github.Connector
}

func (c *closedLabelRefreshConnector) FetchCandidateIssues(ctx context.Context) ([]connector.Issue, error) {
	return c.reader.FetchIssueStateProbe(ctx, []string{"Todo"}, 100)
}

func (c *closedLabelRefreshConnector) FetchIssuesByStates(ctx context.Context, states []string) ([]connector.Issue, error) {
	return c.reader.FetchIssueStateProbe(ctx, states, 100)
}

// Closed snapshots remain useful to reconciliation, but must not consume queue
// positions or enter allowance accounting even when they were not completed.
func TestClosedLabelSnapshotsExcludedFromDispatch(t *testing.T) {
	for _, lane := range []string{"Todo", "In Progress", "Rework", "Merging"} {
		for _, reason := range []string{"completed", "not_planned", ""} {
			t.Run(lane+"/"+reason, func(t *testing.T) {
				tracker := &statusReconcileConnector{}
				orch := newStatusReconcileOrchestrator(tracker)
				orch.cfg.ActiveStates = []string{lane}
				orch.cfg.LifetimeSessionLimit = 1
				orch.lifetimeUsage = closedLabelUsageStore{t: t}
				issue := statusReconcileIssue("issue-2813", lane, true, reason)
				issue.AssignedToWorker = true
				issue.Fields["fixture"] = "hydrated"
				state := newState(orch.cfg)
				orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, time.Now())
				if len(state.SchedulerDecisions) != 0 || len(state.Running) != 0 || state.DispatchStatus.CandidateCount != 0 {
					t.Fatalf("closed snapshot entered dispatch: decisions=%+v status=%+v", state.SchedulerDecisions, state.DispatchStatus)
				}
			})
		}
	}
}

type closedLabelUsageStore struct {
	lifetimeUsageStoreStub
	t *testing.T
}

func (s closedLabelUsageStore) IssueTokenSpend(context.Context, store.IssueIdentity) (store.TokenSpend, error) {
	s.t.Error("closed snapshot entered allowance accounting")
	return store.TokenSpend{}, nil
}
