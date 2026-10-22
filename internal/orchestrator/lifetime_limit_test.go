package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
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
		session      *store.SessionFinish
		active       bool
	}{
		{name: "confirmed refusal leaves session allowance", session: &store.SessionFinish{TurnStartRefused: true}, sessionLimit: 1},
		{name: "actual failed turn exhausts session allowance", session: &store.SessionFinish{Turns: 1}, sessionLimit: 1, wantParked: true},
		{name: "unknown historical failure exhausts session allowance", session: &store.SessionFinish{}, sessionLimit: 1, wantParked: true},
		{name: "active session exhausts session allowance", active: true, sessionLimit: 1, wantParked: true},
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
			if test.session != nil || test.active {
				db, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "usage.db")})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := db.Close(); err != nil {
						t.Error(err)
					}
				})
				issue := lifetimeLimitTestIssue()
				at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
				id, err := db.StartSession(t.Context(), store.SessionStart{ProjectID: cfg.Project.ID, IssueID: issue.ID, StartedAt: at})
				if err != nil {
					t.Fatal(err)
				}
				if test.session != nil {
					finish := *test.session
					finish.CompletedAt, finish.FinalState, finish.TotalTokens = at.Add(time.Second), "failed", 67
					if err := db.FinishSession(t.Context(), id, finish); err != nil {
						t.Fatal(err)
					}
				}
				test.usage, err = db.IssueTokenSpend(t.Context(), store.IssueIdentity{ProjectID: cfg.Project.ID, IssueID: issue.ID})
				if err != nil {
					t.Fatal(err)
				}
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

			if len(state.Blocked) != 0 {
				t.Fatal("lifetime usage created a human hold")
			}
			retry, retrying := state.Retry[issue.ID]
			if retrying != test.wantParked {
				t.Fatalf("retry=%v, want threshold reached=%v", retrying, test.wantParked)
			}
			if retrying && (retry.Error != lifetimeLimitReason || !retry.DueAt.After(now) || retry.DueAt.After(now.Add(cfg.MaxRetryBackoff))) {
				t.Fatalf("lifetime retry lost bounded delay: %+v", retry)
			}
			if len(tracker.updates) != 0 || len(tracker.comments) != 0 {
				t.Fatal("usage retry mutated the executable lane")
			}
			if retrying {
				orch.enforceLifetimeLimits(t.Context(), &state, []connector.Issue{issue}, now.Add(time.Nanosecond))
				if next := state.Retry[issue.ID]; next.Attempt != retry.Attempt || !next.DueAt.Equal(retry.DueAt) {
					t.Fatal("refresh postponed an existing retry")
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
