package hubserver

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The workspace session service. It owns the state
// machine, the limits, the dispatch association and the sweeps that make the
// timeouts real. HTTP handlers, worker binding and the relay live in sibling
// files.

// Workspace failures, in the native error shape of the conversation contract.

// workspaceExists reports a second request on an attempt that already has an
// open workspace. It names the existing id, because the client's next move is
// to use it rather than to retry.
func workspaceExists(id string) error {
	return &hubapi.NativeError{
		Code: "workspace_exists", Message: "A workspace is already open on this attempt",
		Status: http.StatusConflict, Details: map[string]any{"workspace_id": id},
		PublicMessage: true,
	}
}

// workspaceLimit reports the per-organization or per-person cap. It names
// which one was reached, so the answer is actionable rather than only a
// refusal.
func workspaceLimit(scope string, limit int) error {
	return &hubapi.NativeError{
		Code: "workspace_limit", Message: "The open workspace limit has been reached",
		Status: http.StatusUnprocessableEntity, Details: map[string]any{"scope": scope, "limit": limit},
		PublicMessage: true,
	}
}

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

func (w *workspaceService) maintain(ctx context.Context) {
	defer close(w.stopped)
	next := func() time.Duration {
		spread := w.sweepInterval / 2
		if spread <= 0 {
			return w.sweepInterval
		}
		return w.sweepInterval - spread/2 + time.Duration(rand.Int64N(int64(spread)))
	}
	timer := time.NewTimer(next())
	defer timer.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ctx.Done():
			return
		case <-timer.C:
			w.sweep(ctx)
			timer.Reset(next())
		}
	}
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
	if reason, ends := relayEndReason(record.State, true); ends {
		w.relay.closeWorkspace(ctx, record.ID, reason)
	}
}

// relayEndReason reports whether a workspace in this state can no longer serve
// a relay connection, and the code the connection is closed with.
//
// A terminal workspace serves nobody. An unreachable or re-requested one has
// lost the runner that was serving its streams, which ends the runner's
// connection and every stream a person held on it; a person may still connect
// to one afterwards and is answered per frame that no runner is serving.
func relayEndReason(state string, servingRunner bool) (string, bool) {
	switch {
	case workspacesession.Terminal(state):
		return workspacesession.CodeWorkspaceClosed, true
	case servingRunner && (state == workspacesession.StateUnreachable || state == workspacesession.StateRequested):
		return workspacesession.CodeStaleExecution, true
	}
	return "", false
}

// closeIfEndedSinceUpgrade closes a connection whose workspace ended between
// the upgrade's state check and the connection joining the relay.
//
// committed() closes a workspace's connections after the ending transaction
// commits, and it can only close the connections already attached. A
// connection that passed its check before the commit and attached after the
// close would otherwise sit on a workspace that is over, with nothing ever
// telling it so. Re-reading the row after attaching closes that window: either
// the close ran after the attach and found the connection, or the attach ran
// after the commit and this read sees the ended state.
func (w *workspaceService) closeIfEndedSinceUpgrade(ctx context.Context, connection *relayConnection) {
	var record postgres.WorkspaceSession
	err := w.server.accessRead(ctx, connection.Scope.Organization, func(reader tenantstore.Reader) error {
		owner, ok := reader.(postgres.WorkspaceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		record, err = owner.WorkspaceSession(ctx, connection.Scope.Project, connection.WorkspaceID)
		return err
	})
	if err != nil {
		// The periodic authority re-check reads the same row and closes the
		// connection when it can, so a failed read here is logged, not fatal.
		w.logger.Warn("workspace.relay_attach_recheck_failed", "workspace_id", connection.WorkspaceID,
			"connection_id", connection.ID, "error", err)
		return
	}
	if reason, ends := relayEndReason(record.State, connection.Runner); ends {
		w.relay.closeWorkspace(ctx, record.ID, reason)
	}
}

// sweep enforces every deadline of the workspace session contract that no request would reach on
// its own: a request no runner claimed, two missed heartbeats, a lost lease, an
// idle workspace, the hard lifetime cap and the re-bind grace.
func (w *workspaceService) sweep(ctx context.Context) {
	now := w.now()
	records, err := w.dueWorkspaces(ctx, now)
	if err != nil {
		w.logger.Warn("workspace.sweep_failed", "error", err)
		return
	}
	for _, record := range records {
		moved, changed, err := w.applyDeadline(ctx, record, now)
		if err != nil {
			w.logger.Warn("workspace.sweep_transition_failed", "workspace_id", record.ID, "error", err)
			continue
		}
		if changed {
			w.committed(ctx, moved)
		}
	}
	w.sweepActionRuns(ctx)
	w.dispatchQueuedActionRuns(ctx)
	w.relay.sweep(ctx, now)
	if err := w.sweepRelayTickets(ctx, now); err != nil {
		w.logger.Warn("workspace.ticket_sweep_failed", "error", err)
	}
}
