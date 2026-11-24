package hubserver

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"

	"github.com/digitaldrywood/detent/internal/hubserver/workspacerelay"

	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"
)

// The workspace session service. It owns the state
// machine, the limits, the dispatch association and the sweeps that make the
// timeouts real. HTTP handlers, worker binding and the relay live in sibling
// files.

// workspaceService owns workspace sessions inside the hub.
type workspaceService struct {
	server *Service
	config WorkspaceConfig
	logger *slog.Logger
	// relay holds the live person and runner connections of every workspace
	// this hub process is serving.
	relay *workspaceRelay
	// stop ends the maintenance loop; stopped waits for it to exit, and
	// cancelSweep cuts short whatever the current sweep is in the middle of.
	stop        chan struct{}
	stopped     chan struct{}
	cancelSweep context.CancelFunc
	stopOnce    sync.Once
	// sweepInterval is how often the maintenance loop runs. It is a field so
	// a test can drive the timeouts without waiting on wall clock.
	sweepInterval time.Duration
	lifecycle     *workspacehub.Service
}

// workspaceSweepInterval is how often the maintenance loop checks for expired
// requests, missed heartbeats, lost leases and idle workspaces. It is well
// under the shortest deadline it enforces (two missed 30-second heartbeats).
const workspaceSweepInterval = 5 * time.Second

func newWorkspaceService(server *Service, cfg WorkspaceConfig) *workspaceService {
	service := &workspaceService{
		server:        server,
		config:        cfg,
		logger:        server.config.Logger.With("component", "workspace"),
		stop:          make(chan struct{}),
		stopped:       make(chan struct{}),
		sweepInterval: workspaceSweepInterval,
	}
	service.relay = newWorkspaceRelay(service)
	service.lifecycle = service.newLifecycle()
	return service
}

// start runs hub-restart recovery and then the maintenance loop.
//
// The loop outlives the request context that started the hub, so it gets a
// context of its own rather than inheriting one that is about to be cancelled
// or Background, which nothing can interrupt.
func (w *workspaceService) start(ctx context.Context) error {
	if w.server.runtimeHost != nil {
		close(w.stopped)
		return nil
	}
	if err := w.recoverOpenWorkspaces(ctx); err != nil {
		return err
	}
	sweepCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	w.cancelSweep = cancel
	go w.maintain(sweepCtx)
	return nil
}

// Stop ends the maintenance loop and closes every live relay connection.
func (w *workspaceService) Stop() {
	w.StopContext(context.Background())
}

func (w *workspaceService) StopContext(ctx context.Context) {
	w.stopOnce.Do(func() {
		close(w.stop)
		if w.cancelSweep != nil {
			w.cancelSweep()
		}
		<-w.stopped
		w.relay.stop(ctx)
	})
}

// now reads the hub clock through the service's configuration so a test can
// drive every deadline.
func (w *workspaceService) now() time.Time { return w.server.config.Clock().UTC() }

// committed closes the relay connections of a workspace that
// can no longer serve what a person is holding.
// Call it after the transaction that transitioned committed, never inside it.
//
// It takes the context of whatever committed the transition, because closing a
// workspace's connections also writes the terminal recordings that were
// streaming on them and that write needs one. The write drops
// the cancellation and keeps the values; see finishTerminalRecordings.
func (w *workspaceService) committed(ctx context.Context, record workspaceRecord) {
	if w.relay == nil {
		return
	}
	if reason, ends := workspacerelay.EndReason(record.State, true); ends {
		w.relay.closeWorkspace(ctx, record.ID, reason)
	}
}

func (w *workspaceService) closeIfEndedSinceUpgrade(ctx context.Context, connection *relayConnection) {
	w.relayHTTP().CloseIfEndedSinceUpgrade(ctx, connection)
}

func (w *workspaceService) maintain(ctx context.Context) {
	workspacehub.Maintain(ctx, w.stop, w.stopped, func() time.Duration { return w.sweepInterval }, time.NewTimer, w.sweep)
}

func (w *workspaceService) sweep(ctx context.Context) {
	workspacehub.Sweep(ctx, workspacehub.SweepDeps{
		Now: w.now,
		Due: func(ctx context.Context, now time.Time) ([]workspacehub.Record, error) {
			rows, err := w.lifecycleService().Due(ctx, now)
			return rows, hubapi.FromFeatureError(err)
		},
		ApplyDeadline: func(ctx context.Context, record workspacehub.Record, now time.Time) (workspacehub.Record, bool, error) {
			next, changed, err := w.lifecycleService().ApplyDeadline(ctx, record, now)
			return next, changed, hubapi.FromFeatureError(err)
		},
		Committed: func(ctx context.Context, record workspacehub.Record) {
			w.committed(ctx, workspaceRecord(record))
		},
		SweepActions:    w.sweepActionRuns,
		DispatchActions: w.dispatchQueuedActionRuns,
		SweepRelay:      w.relay.sweep,
		SweepTickets:    w.sweepRelayTickets,
		Logger:          w.logger,
	})
}
