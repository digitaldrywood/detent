package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

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
