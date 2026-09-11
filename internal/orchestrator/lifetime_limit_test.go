package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestEvaluateLifetimeLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		usage            store.TokenSpend
		sessionLimit     int64
		tokenLimit       int64
		wantSessions     bool
		wantTokens       bool
		wantLimitReached bool
	}{
		{name: "disabled", usage: store.TokenSpend{Sessions: 100, TotalTokens: 1000}},
		{name: "below both", usage: store.TokenSpend{Sessions: 14, TotalTokens: 39}, sessionLimit: 15, tokenLimit: 40},
		{name: "session boundary", usage: store.TokenSpend{Sessions: 15, TotalTokens: 39}, sessionLimit: 15, tokenLimit: 40, wantSessions: true, wantLimitReached: true},
		{name: "token boundary", usage: store.TokenSpend{Sessions: 14, TotalTokens: 40}, sessionLimit: 15, tokenLimit: 40, wantTokens: true, wantLimitReached: true},
		{name: "both exceeded", usage: store.TokenSpend{Sessions: 16, TotalTokens: 41}, sessionLimit: 15, tokenLimit: 40, wantSessions: true, wantTokens: true, wantLimitReached: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := evaluateLifetimeLimit(test.usage, test.sessionLimit, test.tokenLimit)
			if got.SessionsReached != test.wantSessions || got.TokensReached != test.wantTokens || got.reached() != test.wantLimitReached {
				t.Fatalf("evaluateLifetimeLimit() = %#v, want sessions=%t tokens=%t reached=%t", got, test.wantSessions, test.wantTokens, test.wantLimitReached)
			}
		})
	}
}

func TestEnforceLifetimeLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		usage        store.TokenSpend
		history      store.ProjectLifetimeUsage
		labels       []string
		usageErr     error
		sessionLimit int64
		tokenLimit   int64
		wantParked   bool
		wantComment  []string
	}{
		{name: "below limits", usage: store.TokenSpend{Sessions: 14, TotalTokens: 39_000_000}},
		{name: "session limit", usage: store.TokenSpend{Sessions: 15, TotalTokens: 12_000_000}, wantParked: true},
		{name: "token limit", usage: store.TokenSpend{Sessions: 8, TotalTokens: 40_000_000}, wantParked: true},
		{
			name:         "project p95 remains allowed by calibrated defaults",
			usage:        store.TokenSpend{Sessions: 60, TotalTokens: 200_000_000},
			history:      store.ProjectLifetimeUsage{ProjectID: "detent", CompletedIssues: 270, MeanSessions: 11.7, P95Sessions: 60, P95Tokens: 200_000_000},
			sessionLimit: 120,
			tokenLimit:   750_000_000,
		},
		{
			name:         "ten times mean reaches runaway cap",
			usage:        store.TokenSpend{Sessions: 120, TotalTokens: 200_000_000},
			history:      store.ProjectLifetimeUsage{ProjectID: "detent", CompletedIssues: 270, MeanSessions: 12, P95Sessions: 60, P95Tokens: 200_000_000},
			sessionLimit: 120,
			tokenLimit:   750_000_000,
			wantParked:   true,
			wantComment: []string{
				"120 sessions; project p95 is 60",
				"200000000 tokens; project p95 is 200000000",
				"270 completed issues",
			},
		},
		{name: "operator override", usage: store.TokenSpend{Sessions: 20, TotalTokens: 50_000_000}, labels: []string{"ALLOW-LIFETIME-LIMIT"}},
		{name: "usage unavailable fails open", usageErr: errors.New("database unavailable")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := lifetimeLimitTestConfig()
			if test.sessionLimit > 0 {
				cfg.LifetimeSessionLimit = test.sessionLimit
			}
			if test.tokenLimit > 0 {
				cfg.LifetimeTokenLimit = test.tokenLimit
			}
			tracker := &backendCapacityTestConnector{}
			usage := &lifetimeUsageStoreStub{spend: test.usage, history: test.history, err: test.usageErr}
			metrics := &lifetimeWorkflowMetricsStub{}
			orch := &Orchestrator{
				cfg:             cfg,
				connector:       tracker,
				lifetimeUsage:   usage,
				workflowMetrics: metrics,
			}
			state := newState(cfg)
			issue := lifetimeLimitTestIssue()
			issue.Labels = test.labels
			now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

			orch.enforceLifetimeLimits(t.Context(), &state, []connector.Issue{issue}, now)

			blocked, parked := state.Blocked[issue.ID]
			if parked != test.wantParked {
				t.Fatalf("parked = %t, want %t; blocked=%#v", parked, test.wantParked, blocked)
			}
			if !test.wantParked {
				if len(tracker.updates) != 0 || len(tracker.comments) != 0 {
					t.Fatalf("tracker mutations = updates %#v comments %#v, want none", tracker.updates, tracker.comments)
				}
				return
			}
			if len(tracker.updates) != 1 || tracker.updates[0].state != blockedStatusState {
				t.Fatalf("tracker updates = %#v, want one Blocked transition", tracker.updates)
			}
			if len(tracker.comments) != 1 {
				t.Fatalf("tracker comments = %d, want 1", len(tracker.comments))
			}
			for _, fragment := range test.wantComment {
				if !strings.Contains(tracker.comments[0], fragment) {
					t.Fatalf("tracker comment = %q, want containing %q", tracker.comments[0], fragment)
				}
			}
			if blocked.Recovery == nil || blocked.Recovery.Predicate != blockedRecoveryPredicateLifetimeLimit {
				t.Fatalf("blocked recovery = %#v, want lifetime predicate", blocked.Recovery)
			}
			if !blocked.NeedsHumanAttention {
				t.Fatal("lifetime hold must immediately need human attention")
			}
			if blocked.Recovery.LifetimeSessions != test.usage.Sessions || blocked.Recovery.LifetimeTokens != test.usage.TotalTokens {
				t.Fatalf("blocked recovery usage = %#v, want sessions=%d tokens=%d", blocked.Recovery, test.usage.Sessions, test.usage.TotalTokens)
			}
			decision := newDispatchPlanner(cfg).dispatchableIssueDecision(issue, &state, false, now, "")
			if decision.reason != dispatchSkipLifetimeLimit {
				t.Fatalf("dispatch skip reason = %q, want %q", decision.reason, dispatchSkipLifetimeLimit)
			}
		})
	}
}

func TestLifetimeLimitParkReconciliation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		override    bool
		raised      bool
		legacy      bool
		wantRelease bool
	}{
		{name: "limit holds indefinitely"},
		{name: "legacy timer holds indefinitely", legacy: true},
		{name: "override releases", override: true, wantRelease: true},
		{name: "raised limits release", raised: true, wantRelease: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := lifetimeLimitTestConfig()
			tracker := &backendCapacityTestConnector{}
			metrics := &lifetimeWorkflowMetricsStub{}
			orch := &Orchestrator{cfg: cfg, connector: tracker, lifetimeUsage: &lifetimeUsageStoreStub{spend: store.TokenSpend{Sessions: 15, TotalTokens: 40_000_000}}, workflowMetrics: metrics}
			state := newState(cfg)
			issue := lifetimeLimitTestIssue()
			now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
			orch.enforceLifetimeLimits(t.Context(), &state, []connector.Issue{issue}, now)
			blocked := state.Blocked[issue.ID]
			if blocked.Recovery == nil {
				t.Fatal("missing park")
			}
			park := *blocked.Recovery
			if tt.legacy {
				park.ResumeAt = now.Add(time.Hour).Format(time.RFC3339Nano)
			}
			issue = blocked.Issue
			if tt.override {
				issue.Labels = []string{"ALLOW-LIFETIME-LIMIT"}
			}
			if tt.raised {
				orch.cfg.LifetimeSessionLimit = 16
				orch.cfg.LifetimeTokenLimit = 40_000_001
			}
			for _, elapsed := range []time.Duration{30 * time.Minute, time.Hour, 24 * time.Hour, 365 * 24 * time.Hour} {
				handled, released := orch.reconcileLifetimeLimitPark(t.Context(), &state, issue, park, now.Add(elapsed))
				if !handled || released != tt.wantRelease {
					t.Fatalf("after %s: handled=%t released=%t, want release=%t", elapsed, handled, released, tt.wantRelease)
				}
				if tt.wantRelease {
					if len(tracker.updates) != 2 || tracker.updates[1].state != "In Progress" {
						t.Fatalf("updates = %#v", tracker.updates)
					}
					if _, ok := state.Blocked[issue.ID]; ok {
						t.Fatal("released issue still blocked")
					}
					break
				}
				if len(tracker.updates) != 1 {
					t.Fatalf("park released after %s: %#v", elapsed, tracker.updates)
				}
			}
			for _, event := range metrics.events {
				if strings.Contains(event.MetadataJSON, "lifetime_limit_cooldown_recovery") {
					t.Fatal("recorded cooldown recovery")
				}
			}
		})
	}
}

func lifetimeLimitTestConfig() Config {
	return normalizeConfig(Config{
		Project:                    scheduler.ProjectCandidate{ID: "detent"},
		ActiveStates:               []string{"Todo", "In Progress"},
		TerminalStates:             []string{"Done"},
		LifetimeSessionLimit:       15,
		LifetimeTokenLimit:         40_000_000,
		LifetimeLimitOverrideLabel: "allow-lifetime-limit",
	})
}

func lifetimeLimitTestIssue() connector.Issue {
	issue := connector.NewIssue()
	issue.ID = "issue-1926"
	issue.Identifier = "digitaldrywood/detent#1926"
	issue.URL = "https://github.com/digitaldrywood/detent/issues/1926"
	issue.Title = "cap lifetime usage"
	issue.State = "In Progress"
	return issue
}

type lifetimeUsageStoreStub struct {
	spend   store.TokenSpend
	history store.ProjectLifetimeUsage
	err     error
}

func (s *lifetimeUsageStoreStub) IssueTokenSpend(context.Context, store.IssueIdentity) (store.TokenSpend, error) {
	return s.spend, s.err
}

func (s *lifetimeUsageStoreStub) ProjectLifetimeUsage(context.Context, string) (store.ProjectLifetimeUsage, error) {
	return s.history, nil
}

type lifetimeWorkflowMetricsStub struct {
	events []store.WorkflowPhaseEvent
}

func (s *lifetimeWorkflowMetricsStub) RecordWorkflowPhaseEvent(_ context.Context, event store.WorkflowPhaseEvent) (int64, error) {
	event.ID = int64(len(s.events) + 1)
	s.events = append(s.events, event)
	return event.ID, nil
}

func (s *lifetimeWorkflowMetricsStub) IssueWorkflowTimeline(_ context.Context, identity store.IssueIdentity) (store.WorkflowTimeline, error) {
	events := make([]store.WorkflowPhaseEvent, 0, len(s.events))
	for _, event := range s.events {
		if event.ProjectID == identity.ProjectID && event.IssueID == identity.IssueID {
			events = append(events, event)
		}
	}
	return store.WorkflowTimeline{Events: events}, nil
}
