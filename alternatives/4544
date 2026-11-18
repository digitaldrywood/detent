package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/testenv"
)

type shutdownClaimScheduling struct {
	hubSchedulingSource
	workerExited <-chan struct{}
	released     chan error
}

func (s *shutdownClaimScheduling) ReleaseClaim(ctx context.Context, _, _ string) error {
	var err error
	select {
	case <-s.workerExited:
	default:
		err = errors.New("lease released before worker exit")
	}
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	if _, ok := ctx.Deadline(); !ok {
		err = errors.Join(err, errors.New("shutdown release has no deadline"))
	}
	s.released <- err
	return err
}

type shutdownClaimRunner struct {
	started   chan struct{}
	cancelled chan struct{}
	exit      chan struct{}
	exited    chan struct{}
}

func (r *shutdownClaimRunner) Run(ctx context.Context, _ RunRequest) (RunResult, error) {
	defer close(r.exited)
	close(r.started)
	<-ctx.Done()
	close(r.cancelled)
	<-r.exit
	return RunResult{}, ctx.Err()
}

func TestShutdownJoinsWorkerBeforeReleasingClaim(t *testing.T) {
	backend := &shutdownClaimRunner{started: make(chan struct{}), cancelled: make(chan struct{}), exit: make(chan struct{}), exited: make(chan struct{})}
	source := &shutdownClaimScheduling{hubSchedulingSource: hubSchedulingSource{issue: dispatchTestIssue("shutdown-claim", "Todo")}, workerExited: backend.exited, released: make(chan error, 1)}
	source.issue.Fields["detent_hub_work_item_id"] = "42"
	orch, err := New(Config{Project: schedulerProjectCandidate("widgets"), PollInterval: time.Hour, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}, SchedulingRepository: "acme/widgets"}, Dependencies{Connector: &hubSchedulingConnector{}, Scheduling: source, Runner: backend})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- orch.Run(ctx) }()
	joined := false
	t.Cleanup(func() {
		cancel()
		select {
		case <-backend.exit:
		default:
			close(backend.exit)
		}
		select {
		case <-backend.started:
			<-backend.exited
		default:
		}
		if !joined {
			<-done
		}
	})
	select {
	case <-backend.started:
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("worker did not start")
	}
	cancel()
	<-backend.cancelled
	select {
	case err := <-done:
		joined = true
		t.Errorf("shutdown returned before worker exit: %v", err)
	default:
	}
	close(backend.exit)
	if !joined {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("shutdown = %v", err)
		}
		joined = true
	}
	select {
	case err := <-source.released:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("shutdown left its claim held")
	}
}
