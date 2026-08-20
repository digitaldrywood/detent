package orchestrator

import (
	"context"
	"errors"
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
		name       string
		usage      store.TokenSpend
		labels     []string
		usageErr   error
		wantParked bool
	}{
		{name: "below limits", usage: store.TokenSpend{Sessions: 14, TotalTokens: 39_000_000}},
		{name: "session limit", usage: store.TokenSpend{Sessions: 15, TotalTokens: 12_000_000}, wantParked: true},
		{name: "token limit", usage: store.TokenSpend{Sessions: 8, TotalTokens: 40_000_000}, wantParked: true},
		{name: "operator override", usage: store.TokenSpend{Sessions: 20, TotalTokens: 50_000_000}, labels: []string{"ALLOW-LIFETIME-LIMIT"}},
		{name: "usage unavailable fails open", usageErr: errors.New("database unavailable")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := lifetimeLimitTestConfig()
			tracker := &backendCapacityTestConnector{}
			usage := &lifetimeUsageStoreStub{spend: test.usage, err: test.usageErr}
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
			if blocked.Recovery == nil || blocked.Recovery.Predicate != blockedRecoveryPredicateLifetimeLimit {
				t.Fatalf("blocked recovery = %#v, want lifetime predicate", blocked.Recovery)
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

func TestLifetimeLimitCooldownRecoveryPermitsOneSession(t *testing.T) {
	t.Parallel()

	cfg := lifetimeLimitTestConfig()
	tracker := &backendCapacityTestConnector{}
	usage := &lifetimeUsageStoreStub{spend: store.TokenSpend{Sessions: 15, TotalTokens: 12_000_000}}
	metrics := &lifetimeWorkflowMetricsStub{}
	orch := &Orchestrator{
		cfg:             cfg,
		connector:       tracker,
		lifetimeUsage:   usage,
		workflowMetrics: metrics,
	}
	state := newState(cfg)
	issue := lifetimeLimitTestIssue()
	parkedAt := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	orch.enforceLifetimeLimits(t.Context(), &state, []connector.Issue{issue}, parkedAt)
	blocked := state.Blocked[issue.ID]
	if blocked.Recovery == nil {
		t.Fatal("blocked recovery metadata is nil")
	}
	park := *blocked.Recovery
	blockedIssue := blocked.Issue

	handled, transitioned := orch.reconcileLifetimeLimitPark(t.Context(), &state, blockedIssue, park, parkedAt.Add(30*time.Minute))
	if !handled || transitioned {
		t.Fatalf("pre-cooldown recovery = handled %t transitioned %t, want true/false", handled, transitioned)
	}
	if got := len(tracker.updates); got != 1 {
		t.Fatalf("pre-cooldown tracker updates = %d, want initial park only", got)
	}

	handled, transitioned = orch.reconcileLifetimeLimitPark(t.Context(), &state, blockedIssue, park, parkedAt.Add(time.Hour))
	if !handled || !transitioned {
		t.Fatalf("post-cooldown recovery = handled %t transitioned %t, want true/true", handled, transitioned)
	}
	if got := tracker.updates[len(tracker.updates)-1].state; got != issue.State {
		t.Fatalf("recovery target = %q, want %q", got, issue.State)
	}
	if _, exists := state.Blocked[issue.ID]; exists {
		t.Fatal("blocked state retained after cooldown recovery")
	}
	if !orch.lifetimeLimitRecoveryPermit(t.Context(), issue, usage.spend) {
		t.Fatal("same lifetime usage was not permitted after cooldown")
	}
	advanced := usage.spend
	advanced.Sessions++
	if orch.lifetimeLimitRecoveryPermit(t.Context(), issue, advanced) {
		t.Fatal("cooldown recovery permitted more than one additional session")
	}
}

func lifetimeLimitTestConfig() Config {
	return normalizeConfig(Config{
		Project:                    scheduler.ProjectCandidate{ID: "detent"},
		ActiveStates:               []string{"Todo", "In Progress"},
		TerminalStates:             []string{"Done"},
		LifetimeSessionLimit:       15,
		LifetimeTokenLimit:         40_000_000,
		LifetimeLimitCooldown:      time.Hour,
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
	spend store.TokenSpend
	err   error
}

func (s *lifetimeUsageStoreStub) IssueTokenSpend(context.Context, store.IssueIdentity) (store.TokenSpend, error) {
	return s.spend, s.err
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
