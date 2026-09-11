package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestDependencyAutoUnblockRESTBudgetProgress(t *testing.T) {
	t.Parallel()
	for _, competingReads := range []int{4, 9} {
		t.Run(fmt.Sprintf("competing_reads_%d", competingReads), func(t *testing.T) {
			t.Parallel()
			tracker := newDependencyBudgetConnector(t, 9, competingReads)
			orch := dependencyAutoUnblockOrchestrator(tracker.dependencyAutoUnblockConnector, DependencyAutoUnblockConfig{Enabled: true})
			orch.connector = tracker
			var logs bytes.Buffer
			orch.logger = slog.New(slog.NewTextHandler(&logs, nil))
			state := newState(orch.cfg)
			now := time.Date(2026, 9, 8, 23, 20, 0, 0, time.UTC)
			for cycle := range 7 {
				orch.tick(t.Context(), &state, now.Add(time.Duration(cycle)*time.Minute))
				if state.RateLimits == nil || state.RateLimits.RESTUsage == nil {
					t.Fatal("missing REST telemetry")
				}
				usage := state.RateLimits.RESTUsage
				if usage.BillableRequests > 9 || usage.RateLimited || usage.ReserveHeld {
					t.Fatalf("unexpected REST usage: %+v", usage)
				}
				if cycle == 0 && (!usage.FanoutDeferred || !slices.ContainsFunc(state.RecentEvents, func(event telemetry.ActivityEvent) bool { return event.Event == "dependency_auto_unblock_deferred" })) {
					t.Fatal("missing classified dependency deferral telemetry")
				}
				if cycle == 0 {
					if tracker.lastRefreshBudget == nil {
						t.Fatal("missing shared refresh budget")
					}
					if _, allowed := tracker.lastRefreshBudget.Reserve(9*4, 4); allowed {
						t.Fatal("usage flush reset the refresh budget")
					}
					want := 0
					if competingReads == 4 {
						want = 1
					}
					if len(tracker.updates) != want {
						t.Fatalf("first refresh transitions = %d, want %d: %s", len(tracker.updates), want, logs.String())
					}
				}
			}
			if len(tracker.updates) != 3 {
				t.Fatalf("transitions after seven capped refreshes = %v, want all three", tracker.updates)
			}
			for _, issue := range tracker.stateIssues {
				if !slices.ContainsFunc(tracker.updates, func(update dependencyAutoUnblockUpdate) bool { return update.issueID == issue.ID }) {
					t.Errorf("issue %s never recovered", issue.ID)
				}
			}
		})
	}
}

func TestDependencyAutoUnblockRejectsStaleRESTBudgetEvidence(t *testing.T) {
	t.Parallel()
	tracker := newDependencyBudgetConnector(t, 1, 0)
	orch := dependencyAutoUnblockOrchestrator(tracker.dependencyAutoUnblockConnector, DependencyAutoUnblockConfig{Enabled: true})
	orch.connector = tracker
	var logs bytes.Buffer
	orch.logger = slog.New(slog.NewTextHandler(&logs, nil))
	state := newState(orch.cfg)
	orch.autoUnblockDependencyIssues(t.Context(), &state, tracker.stateIssues[:1], time.Now())
	if len(tracker.updates) != 0 {
		t.Fatalf("transitions = %v after blocker lookup was deferred, want none", tracker.updates)
	}
}

type dependencyBudgetConnector struct {
	*dependencyAutoUnblockConnector
	client                   *githubconnector.Client
	competingReads           int
	candidateErr             error
	identifierReads          map[string]int
	extraReads               map[string]int
	lastRefreshBudget        *connector.RESTFanoutBudget
	successfulCompetingReads int
}

func newDependencyBudgetConnector(t *testing.T, cap int64, competingReads int) *dependencyBudgetConnector {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	client, err := githubconnector.NewClient(githubconnector.ClientConfig{Endpoint: server.URL, TokenSource: githubconnector.StaticTokenSource("test-token"), HTTPClient: server.Client(), RESTPolicy: githubconnector.RESTBudgetPolicy{FanoutMaxRequests: cap}, DisableConditionalRequests: true})
	if err != nil {
		t.Fatal(err)
	}
	base := &dependencyAutoUnblockConnector{}
	blocker := dependencyAutoUnblockIssue("issue-100", "Done")
	base.blockers = []connector.Issue{blocker}
	for _, number := range []int{1, 2, 3} {
		issue := dependencyAutoUnblockIssue(fmt.Sprintf("issue-%d", number), "Blocked")
		issue.BlockedBy = []connector.BlockedRef{{Identifier: blocker.Identifier, State: "Done"}}
		base.stateIssues = append(base.stateIssues, issue)
		base.hydratedIssues = append(base.hydratedIssues, issue)
	}
	return &dependencyBudgetConnector{dependencyAutoUnblockConnector: base, client: client, competingReads: competingReads, identifierReads: map[string]int{}}
}

func (c *dependencyBudgetConnector) FetchCandidateIssues(ctx context.Context) ([]connector.Issue, error) {
	c.lastRefreshBudget, _ = connector.RESTFanoutBudgetFromContext(ctx)
	for range c.competingReads {
		if err := c.client.REST(ctx, http.MethodGet, "/repos/digitaldrywood/detent/issues/999", nil, nil); err != nil {
			break
		}
		c.successfulCompetingReads++
	}
	return nil, c.candidateErr
}

func (c *dependencyBudgetConnector) FetchIssueStatesByIdentifiers(ctx context.Context, identifiers []string) ([]connector.Issue, error) {
	for _, identifier := range identifiers {
		c.identifierReads[identifier]++
		for range 1 + c.extraReads[identifier] {
			if err := c.client.REST(ctx, http.MethodGet, "/repos/digitaldrywood/detent/issues/1", nil, nil); err != nil {
				return nil, err
			}
		}
	}
	return c.dependencyAutoUnblockConnector.FetchIssueStatesByIdentifiers(ctx, identifiers)
}

func (c *dependencyBudgetConnector) FlushRESTRateLimitUsage() connector.RESTRateLimitUsage {
	return c.client.FlushRESTRateLimitUsage()
}

func (c *dependencyBudgetConnector) UpdateIssueState(ctx context.Context, issueID, state string) error {
	for i := range c.stateIssues {
		if c.stateIssues[i].ID == issueID {
			c.stateIssues[i].State = state
		}
	}
	for i := range c.hydratedIssues {
		if c.hydratedIssues[i].ID == issueID {
			c.hydratedIssues[i].State = state
		}
	}
	return c.dependencyAutoUnblockConnector.UpdateIssueState(ctx, issueID, state)
}

func TestDeferredDependencyUnblockRevalidatesEvidence(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"ready", "human owned issue", "changed source state", "reopened dependency", "missing dependency", "missing issue", "human evidence removed", "workpad hold", "operator stop", "new dependency", "new comment dependency", "native-only comment"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := newDependencyBudgetConnector(t, 9, 9)
			tracker := &dependencyBudgetCommentConnector{dependencyBudgetConnector: base}
			orch := dependencyAutoUnblockOrchestrator(base.dependencyAutoUnblockConnector, DependencyAutoUnblockConfig{Enabled: true})
			orch.connector = tracker
			state := newState(orch.cfg)
			now := time.Date(2026, 9, 8, 23, 20, 0, 0, time.UTC)
			_, _ = tracker.FetchCandidateIssues(t.Context())
			orch.autoUnblockDependencyIssues(t.Context(), &state, base.stateIssues[:1], now)
			state.BoardIssues = cloneIssues(base.stateIssues[:1])
			state.dependencyUnblockEarly = true
			tracker.candidateErr = errors.New("ordinary fetch unavailable")
			tracker.competingReads = 0
			tracker.FlushRESTRateLimitUsage()
			switch name {
			case "human owned issue":
				base.hydratedIssues[0].Labels = []string{"human-owned"}
			case "changed source state":
				base.hydratedIssues[0].State = "In Progress"
			case "reopened dependency":
				base.blockers[0].State = "In Progress"
			case "missing dependency":
				base.blockers = nil
			case "missing issue":
				base.hydratedIssues = nil
				base.stateIssues = nil
			case "human evidence removed":
				human := humanDependencyIssue("", true)
				human.Identifier = base.blockers[0].Identifier
				base.blockers[0] = human
			case "workpad hold":
				tracker.currentComments = []connector.IssueComment{{Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: wait for approval\n```"}}
			case "operator stop":
				state.Blocked[base.stateIssues[0].ID] = Blocked{Issue: base.stateIssues[0], Source: BlockedSourceOperatorStop, Reason: "operator stop"}
			case "new dependency":
				base.hydratedIssues[0].Description = "Depends on: #200"
			case "new comment dependency", "native-only comment":
				tracker.currentComments = []connector.IssueComment{{Body: "Depends on: #200"}}
				if name == "native-only comment" {
					orch.cfg.DependencySource = "native_only"
				}
			}
			orch.tick(t.Context(), &state, now.Add(time.Minute))
			want := 0
			if name == "ready" || name == "native-only comment" {
				want = 1
			}
			if len(base.updates) != want {
				t.Fatalf("transitions = %v, want %d with fresh %s evidence", base.updates, want, name)
			}
		})
	}
}

type dependencyBudgetCommentConnector struct {
	*dependencyBudgetConnector
	currentComments []connector.IssueComment
}

func (c *dependencyBudgetCommentConnector) FetchIssueComments(ctx context.Context, _ connector.Issue) ([]connector.IssueComment, error) {
	if err := c.client.REST(ctx, http.MethodGet, "/repos/digitaldrywood/detent/issues/1/comments", nil, nil); err != nil {
		return nil, err
	}
	return c.currentComments, nil
}

func TestDependencyUnblockScanFairness(t *testing.T) {
	t.Parallel()
	tracker := newDependencyBudgetConnector(t, 9, 9)
	tracker.extraReads = map[string]int{tracker.stateIssues[0].Identifier: 10}
	orch := dependencyAutoUnblockOrchestrator(tracker.dependencyAutoUnblockConnector, DependencyAutoUnblockConfig{Enabled: true})
	orch.connector = tracker
	state := newState(orch.cfg)
	now := time.Date(2026, 9, 8, 23, 20, 0, 0, time.UTC)
	for cycle := range 13 {
		orch.tick(t.Context(), &state, now.Add(time.Duration(cycle)*time.Minute))
		if cycle == 0 {
			newcomer := dependencyAutoUnblockIssue("issue-4", "Blocked")
			newcomer.BlockedBy = tracker.stateIssues[1].BlockedBy
			tracker.stateIssues = append(tracker.stateIssues, newcomer)
			tracker.hydratedIssues = append(tracker.hydratedIssues, newcomer)
		}
	}
	want := []dependencyAutoUnblockUpdate{{issueID: "issue-2", state: "Todo"}, {issueID: "issue-3", state: "Todo"}, {issueID: "issue-4", state: "Todo"}}
	if !slices.Equal(tracker.updates, want) {
		t.Fatalf("transitions = %v, want FIFO progress %v despite oversized first item", tracker.updates, want)
	}
	if tracker.stateIssues[0].State != "Blocked" {
		t.Fatal("oversized issue must remain blocked for a later scan")
	}
}

func TestDeferredDependencyUnblockPreservesProviderLimits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		remaining string
		status    int
		reserve   int64
	}{
		{name: "primary", remaining: "0", status: http.StatusForbidden},
		{name: "reserve", remaining: "1", status: http.StatusOK, reserve: 1},
		{name: "secondary", remaining: "99", status: http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-RateLimit-Limit", "100")
				w.Header().Set("X-RateLimit-Remaining", tc.remaining)
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
				if tc.status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "60")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(server.Close)
			client, err := githubconnector.NewClient(githubconnector.ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), TokenSource: githubconnector.StaticTokenSource("test-token"), RESTPolicy: githubconnector.RESTBudgetPolicy{FanoutMaxRequests: 9, MinRemainingReserve: tc.reserve}, DisableConditionalRequests: true})
			if err != nil {
				t.Fatal(err)
			}
			tracker := newDependencyBudgetConnector(t, 9, 0)
			tracker.client = client
			orch := dependencyAutoUnblockOrchestrator(tracker.dependencyAutoUnblockConnector, DependencyAutoUnblockConfig{Enabled: true})
			orch.connector = tracker
			state := newState(orch.cfg)
			state.BoardIssues = cloneIssues(tracker.stateIssues[:1])
			state.dependencyUnblockEarly = true
			tracker.candidateErr = errors.New("ordinary fetch unavailable")
			tracker.competingReads = 0
			orch.tick(t.Context(), &state, time.Now())
			if len(tracker.updates) != 0 || calls.Load() != 1 {
				t.Fatalf("transitions = %v, requests = %d, want no transition or request past provider limit", tracker.updates, calls.Load())
			}
		})
	}
}

func TestDependencyAutoUnblockRefreshesCommentsWithoutIdentifier(t *testing.T) {
	t.Parallel()
	for _, hold := range []bool{false, true} {
		t.Run(fmt.Sprintf("workpad_hold_%t", hold), func(t *testing.T) {
			t.Parallel()
			base := newDependencyBudgetConnector(t, 9, 0)
			base.stateIssues[0].Identifier = ""
			tracker := &dependencyBudgetCommentConnector{dependencyBudgetConnector: base}
			if hold {
				tracker.currentComments = []connector.IssueComment{{Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: wait for approval\n```"}}
			}
			orch := dependencyAutoUnblockOrchestrator(base.dependencyAutoUnblockConnector, DependencyAutoUnblockConfig{Enabled: true})
			orch.connector = tracker
			state := newState(orch.cfg)
			orch.autoUnblockDependencyIssues(t.Context(), &state, base.stateIssues[:1], time.Now())
			want := 1
			if hold {
				want = 0
			}
			if len(tracker.updates) != want {
				t.Fatalf("transitions = %v, want %d after current workpad check", tracker.updates, want)
			}
		})
	}
}

func TestDeferredDependencyUnblockYieldsRefreshBudget(t *testing.T) {
	t.Parallel()
	tracker := newDependencyBudgetConnector(t, 9, 9)
	tracker.stateIssues = tracker.stateIssues[:1]
	tracker.hydratedIssues = tracker.hydratedIssues[:1]
	tracker.extraReads = map[string]int{tracker.stateIssues[0].Identifier: 10}
	orch := dependencyAutoUnblockOrchestrator(tracker.dependencyAutoUnblockConnector, DependencyAutoUnblockConfig{Enabled: true})
	orch.connector = tracker
	state := newState(orch.cfg)
	now := time.Date(2026, 9, 8, 23, 20, 0, 0, time.UTC)
	for cycle := range 4 {
		orch.tick(t.Context(), &state, now.Add(time.Duration(cycle)*time.Minute))
	}
	if got := tracker.identifierReads[tracker.stateIssues[0].Identifier]; got != 4 {
		t.Fatalf("unblock scans = %d, want exactly one per refresh", got)
	}
	if tracker.successfulCompetingReads <= 9 {
		t.Fatalf("ordinary reads = %d, want continued progress after an oversized recovery is queued", tracker.successfulCompetingReads)
	}
	if len(tracker.updates) != 0 || tracker.stateIssues[0].State != "Blocked" {
		t.Fatalf("transitions = %v, want oversized issue left blocked", tracker.updates)
	}
}

func TestDependencyAutoUnblockScanOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		after string
		want  []string
	}{
		{name: "initial", want: []string{"a", "b", "d"}},
		{name: "next identity", after: "a", want: []string{"b", "d", "a"}},
		{name: "removed cursor", after: "c", want: []string{"d", "a", "b"}},
		{name: "wrap", after: "z", want: []string{"a", "b", "d"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issues := []connector.Issue{{ID: "d", State: "Blocked"}, {ID: "", State: "Blocked"}, {ID: "b", State: "Blocked"}, {ID: "c", State: "Todo"}, {ID: "a", State: "Blocked"}}
			ordered := dependencyAutoUnblockOrder(issues, []string{"Blocked"}, tc.after)
			var got []string
			for _, issue := range ordered {
				got = append(got, issue.ID)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("order = %v, want %v", got, tc.want)
			}
			if issues[0].ID != "d" {
				t.Fatal("scan mutated shared board snapshot")
			}
		})
	}
}
