package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestFetchTickIssuesOwnsScan(t *testing.T) {
	for _, tt := range []struct {
		name               string
		beginErr, fetchErr error
	}{
		{name: "success"}, {name: "fetch failure", fetchErr: errors.New("fetch failed")}, {name: "cancelled wait", beginErr: context.Canceled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tracker := &scanLifecycleConnector{beginErr: tt.beginErr, fetchErr: tt.fetchErr, t: t}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo"}})
			o := &Orchestrator{cfg: cfg, connector: tracker, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			state := newState(cfg)
			_, ok := o.fetchTickIssues(t.Context(), &state, time.Now(), githubBudgetReserveDecision{})
			if want := tt.beginErr == nil && tt.fetchErr == nil; ok != want {
				t.Errorf("success = %v, want %v", ok, want)
			}
			if tracker.held {
				t.Error("scan not released")
			}
			if want := tt.beginErr == nil; tracker.fetched != want || tracker.released != want {
				t.Errorf("fetched=%v released=%v, want %v", tracker.fetched, tracker.released, want)
			}
		})
	}
}

type scanLifecycleConnector struct {
	connector.Connector
	t                       *testing.T
	beginErr, fetchErr      error
	held, fetched, released bool
}

func (c *scanLifecycleConnector) BeginRefreshScan(context.Context) (func(), error) {
	if c.beginErr != nil {
		return nil, c.beginErr
	}
	c.held = true
	return func() { c.held = false; c.released = true }, nil
}
func (c *scanLifecycleConnector) CombinedRefreshEnabled() bool { return true }
func (c *scanLifecycleConnector) FetchRefreshIssues(context.Context, []string, []string, connector.IssueFilterHint) connector.RefreshIssueResult {
	if !c.held {
		c.t.Error("fetch without scan ownership")
	}
	c.fetched = true
	return connector.RefreshIssueResult{CandidateError: c.fetchErr}
}
