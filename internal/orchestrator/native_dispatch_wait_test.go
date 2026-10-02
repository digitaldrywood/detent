package orchestrator

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

type candidateWakeSource struct {
	*hubSchedulingSource
	ready     atomic.Bool
	waiting   chan struct{}
	stopped   chan struct{}
	trigger   chan struct{}
	refreshed chan struct{}
}

func (s *candidateWakeSource) FetchCandidateIssues(ctx context.Context, request SchedulingRequest) ([]connector.Issue, error) {
	select {
	case s.refreshed <- struct{}{}:
	default:
	}
	if !s.ready.Load() {
		return nil, nil
	}
	return s.hubSchedulingSource.FetchCandidateIssues(ctx, request)
}

func (s *candidateWakeSource) WaitCandidateChanges(ctx context.Context, _ string, wake chan<- struct{}) {
	defer close(s.stopped)
	close(s.waiting)
	select {
	case <-ctx.Done():
		return
	case <-s.trigger:
		s.ready.Store(true)
		select {
		case wake <- struct{}{}:
		case <-ctx.Done():
			return
		}
	}
	<-ctx.Done()
}

func TestRunNativeCandidateWake(t *testing.T) {
	for _, mode := range []string{"wake", "drain", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			issue := dispatchTestIssue("new-native", "Todo")
			issue.Fields["detent_hub_work_item_id"] = "42"
			source := &candidateWakeSource{hubSchedulingSource: &hubSchedulingSource{issue: issue}, waiting: make(chan struct{}), stopped: make(chan struct{}), trigger: make(chan struct{}), refreshed: make(chan struct{}, 1)}
			runner := &hubSchedulingRunner{started: make(chan struct{}, 1)}
			o, err := New(Config{PollInterval: time.Hour, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}, Project: schedulerProjectCandidate("widgets"), SchedulingRepository: "acme/widgets"}, Dependencies{Connector: &hubSchedulingConnector{}, Scheduling: source, Runner: runner})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- o.Run(ctx) }()
			await := func(signal <-chan struct{}) {
				t.Helper()
				select {
				case <-signal:
				case <-time.After(5 * time.Second):
					t.Fatal("lifecycle signal missing")
				}
			}
			await(source.waiting)
			await(source.refreshed)
			if mode == "wake" {
				close(source.trigger)
				await(runner.started)
			} else if mode == "drain" {
				drainCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
				defer stop()
				if err := o.Drain(drainCtx); err != nil {
					t.Fatal(err)
				}
				await(source.stopped)
				close(source.trigger)
				if len(runner.started) > 0 {
					t.Fatal("drain admitted new work")
				}
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("run did not stop")
			}
			await(source.stopped)
		})
	}
}
