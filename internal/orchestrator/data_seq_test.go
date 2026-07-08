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

func TestTickDataSeqChangesOnlyAfterSuccessfulRefresh(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	errFetch := errors.New("fetch failed")

	tests := []struct {
		name      string
		tracker   connector.Connector
		state     func(Config) State
		wantSeq   uint64
		wantFetch int
	}{
		{
			name:    "successful tick increments",
			tracker: &dataSeqConnector{},
			state: func(cfg Config) State {
				state := newState(cfg)
				state.DataSeq = 3
				return state
			},
			wantSeq:   4,
			wantFetch: 1,
		},
		{
			name:    "failed tick does not increment",
			tracker: &dataSeqConnector{fetchErr: errFetch},
			state: func(cfg Config) State {
				state := newState(cfg)
				state.DataSeq = 3
				return state
			},
			wantSeq:   3,
			wantFetch: 1,
		},
		{
			name:    "paused tick does not increment",
			tracker: &dataSeqConnector{},
			state: func(cfg Config) State {
				state := newState(cfg)
				state.DataSeq = 3
				resetAt := now.Add(10 * time.Minute)
				state.RateLimits = &telemetry.RateLimits{
					GitHubGraphQL: &telemetry.RateLimitBucket{
						Remaining: 0,
						Limit:     5000,
						Used:      5000,
						ResetAt:   &resetAt,
					},
				}
				return state
			},
			wantSeq: 3,
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
			state := tt.state(cfg)
			orch := &Orchestrator{
				cfg:       cfg,
				connector: tt.tracker,
				logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			}

			orch.tick(context.Background(), &state, now)

			if state.DataSeq != tt.wantSeq {
				t.Fatalf("DataSeq = %d, want %d", state.DataSeq, tt.wantSeq)
			}
			if tracker, ok := tt.tracker.(*dataSeqConnector); ok && tracker.fetches != tt.wantFetch {
				t.Fatalf("FetchCandidateIssues() calls = %d, want %d", tracker.fetches, tt.wantFetch)
			}
			if snapshot := state.Snapshot(now); snapshot.Refresh.DataSeq != state.DataSeq {
				t.Fatalf("Snapshot().Refresh.DataSeq = %d, want %d", snapshot.Refresh.DataSeq, state.DataSeq)
			}
			if cloned := state.clone(); cloned.DataSeq != state.DataSeq {
				t.Fatalf("clone().DataSeq = %d, want %d", cloned.DataSeq, state.DataSeq)
			}
		})
	}
}

type dataSeqConnector struct {
	fetchErr error
	fetches  int
}

func (c *dataSeqConnector) Name() string {
	return "data-seq"
}

func (c *dataSeqConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	c.fetches++
	if c.fetchErr != nil {
		return nil, c.fetchErr
	}
	return nil, nil
}

func (c *dataSeqConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c *dataSeqConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c *dataSeqConnector) CreateComment(context.Context, string, string) error {
	return nil
}

func (c *dataSeqConnector) UpdateIssueState(context.Context, string, string) error {
	return nil
}

func (c *dataSeqConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *dataSeqConnector) SetField(context.Context, string, string, string) error {
	return nil
}
