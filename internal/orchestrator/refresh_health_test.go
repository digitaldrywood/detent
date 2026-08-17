package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestTrackerRefreshHealthTracksConsecutiveSourceFailures(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		failSource telemetry.RefreshSourceName
		fail       func(*refreshHealthConnector)
	}{
		{
			name:       "candidate fetch",
			failSource: telemetry.RefreshSourceCandidates,
			fail: func(tracker *refreshHealthConnector) {
				tracker.candidateErr = errors.New("candidate endpoint unavailable")
			},
		},
		{
			name:       "status drift fetch",
			failSource: telemetry.RefreshSourceDrift,
			fail: func(tracker *refreshHealthConnector) {
				tracker.driftErr = errors.New("drift endpoint unavailable")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(Config{
				PollInterval:        30 * time.Second,
				MaxConcurrentAgents: 1,
				ActiveStates:        []string{"Todo", "In Progress"},
				TerminalStates:      []string{"Done", "Cancelled"},
			})
			tracker := &refreshHealthConnector{}
			state := newState(cfg)
			orch := &Orchestrator{
				cfg:       cfg,
				connector: tracker,
				logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			}

			orch.tick(t.Context(), &state, now)
			tt.fail(tracker)
			for attempt := 1; attempt <= 3; attempt++ {
				orch.tick(t.Context(), &state, now.Add(time.Duration(attempt)*cfg.PollInterval))
			}

			snapshot := state.Snapshot(now.Add(3 * cfg.PollInterval))
			source, ok := snapshot.Refresh.Source(tt.failSource)
			if !ok {
				t.Fatalf("Refresh.Source(%q) missing from %#v", tt.failSource, snapshot.Refresh.Sources)
			}
			if source.FailureStreak != refreshFailureDegradedThreshold {
				t.Fatalf("FailureStreak = %d, want %d", source.FailureStreak, refreshFailureDegradedThreshold)
			}
			if source.LastSuccessAt == nil || !source.LastSuccessAt.Equal(now) {
				t.Fatalf("LastSuccessAt = %v, want %v", source.LastSuccessAt, now)
			}
			if source.LastError == "" || source.LastErrorAt == nil {
				t.Fatalf("source error = %q at %v, want retained error", source.LastError, source.LastErrorAt)
			}
			if !snapshot.Refresh.Degraded() {
				t.Fatalf("Refresh status = %q, want degraded", snapshot.Refresh.Status)
			}
			if !snapshot.Refresh.Stale(snapshot.GeneratedAt) {
				t.Fatal("Refresh.Stale() = false, want true")
			}
		})
	}
}

func TestMarkRefreshKeepsEffectiveIntervalForStaleness(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		name              string
		configured        time.Duration
		effective         time.Duration
		wantStaleAfter    time.Duration
		wantNextRefreshAt time.Time
	}{
		{name: "base cadence", configured: time.Minute, effective: time.Minute, wantStaleAfter: 2 * time.Minute, wantNextRefreshAt: now.Add(time.Minute)},
		{name: "rate limit adjusted cadence", configured: time.Minute, effective: 4 * time.Minute, wantStaleAfter: 8 * time.Minute, wantNextRefreshAt: now.Add(4 * time.Minute)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(Config{PollInterval: tt.configured})
			state := newState(cfg)
			state.PollInterval = tt.effective
			orch := &Orchestrator{cfg: cfg}
			orch.markRefresh(&state, now)

			if state.PollInterval != tt.effective {
				t.Fatalf("PollInterval = %s, want effective interval %s", state.PollInterval, tt.effective)
			}
			if !state.NextRefreshAt.Equal(tt.wantNextRefreshAt) {
				t.Fatalf("NextRefreshAt = %s, want %s", state.NextRefreshAt, tt.wantNextRefreshAt)
			}
			if got := refreshStaleAfter(state.PollInterval); got != tt.wantStaleAfter {
				t.Fatalf("refreshStaleAfter() = %s, want %s", got, tt.wantStaleAfter)
			}
		})
	}
}

func TestRefreshFailureThresholdFlowsFromWorkflow(t *testing.T) {
	t.Parallel()

	workflow := workflowconfig.Default()
	workflow.Polling.RefreshFailureThreshold = 5
	cfg := normalizeConfig(ConfigFromWorkflow(workflow))
	state := newState(cfg)
	snapshot := state.Snapshot(time.Date(2026, 8, 17, 15, 0, 0, 0, time.UTC))
	if snapshot.Refresh.FailureThreshold != 5 {
		t.Fatalf("Refresh.FailureThreshold = %d, want 5", snapshot.Refresh.FailureThreshold)
	}
}

type refreshHealthConnector struct {
	candidateErr error
	driftErr     error
}

func (c *refreshHealthConnector) Name() string {
	return "refresh-health"
}

func (c *refreshHealthConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, c.candidateErr
}

func (c *refreshHealthConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c *refreshHealthConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c *refreshHealthConnector) FetchStatusDrift(context.Context) (connector.StatusDrift, error) {
	return connector.StatusDrift{}, c.driftErr
}

func (c *refreshHealthConnector) CreateComment(context.Context, string, string) error {
	return nil
}

func (c *refreshHealthConnector) UpdateIssueState(context.Context, string, string) error {
	return nil
}

func (c *refreshHealthConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *refreshHealthConnector) SetField(context.Context, string, string, string) error {
	return nil
}
