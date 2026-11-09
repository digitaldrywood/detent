package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
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
	return &nativeError{
		Code: "workspace_exists", Message: "A workspace is already open on this attempt",
		status: http.StatusConflict, Details: map[string]any{"workspace_id": id},
		publicMessage: true,
	}
}

// workspaceLimit reports the per-organization or per-person cap. It names
// which one was reached, so the answer is actionable rather than only a
// refusal.
func workspaceLimit(scope string, limit int) error {
	return &nativeError{
		Code: "workspace_limit", Message: "The open workspace limit has been reached",
		status: http.StatusUnprocessableEntity, Details: map[string]any{"scope": scope, "limit": limit},
		publicMessage: true,
	}
}

// workspaceUnavailable reports that the hub is not running workspace sessions.
func workspaceUnavailable() error {
	return &nativeError{
		Code: "unsupported_control", Message: "Workspace sessions are not enabled on this hub",
		status:        http.StatusUnprocessableEntity,
		publicMessage: true,
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
	sweepInterval  time.Duration
	afterWorkspace string
	afterActionRun string
	restartAt      time.Time
}

// workspaceSweepInterval is how often the maintenance loop checks for expired
// requests, missed heartbeats, lost leases and idle workspaces. It is well
// under the shortest deadline it enforces (two missed 30-second heartbeats).
const workspaceSweepInterval = 5 * time.Second

func newWorkspaceService(server *Service, cfg WorkspaceConfig) *workspaceService {
	service := &workspaceService{
		server:        server,
		config:        cfg.normalized(),
		logger:        server.config.Logger.With("component", "workspace"),
		stop:          make(chan struct{}),
		stopped:       make(chan struct{}),
		sweepInterval: workspaceSweepInterval,
	}
	service.relay = newWorkspaceRelay(service)
	return service
}

// start runs hub-restart recovery and then the maintenance loop.
//
// The loop outlives the request context that started the hub, so it gets a
// context of its own rather than inheriting one that is about to be cancelled
// or Background, which nothing can interrupt.
func (w *workspaceService) start(ctx context.Context) error {
	w.restartAt = w.now()
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
	ticks := w.config.maintenanceTicks
	var timer *time.Timer
	if ticks == nil {
		timer = time.NewTimer(workspaceTickInterval(w.sweepInterval))
		defer timer.Stop()
		ticks = timer.C
	}
	for {
		select {
		case <-w.stop:
			return
		case <-ctx.Done():
			return
		case <-ticks:
			w.sweep(ctx)
			if timer != nil {
				timer.Reset(workspaceTickInterval(w.sweepInterval))
			}
		}
	}
}

// now reads the hub clock through the service's configuration so a test can
// drive every deadline.
func (w *workspaceService) now() time.Time { return w.server.config.now().UTC() }

func (w *workspaceService) transition(ctx context.Context, _ *sql.Tx, record workspaceRecord, to, reason string, now time.Time) (workspaceRecord, error) {
	scope := nativeScope{organization: record.OrganizationID, project: record.ProjectID}
	var finish workspaceDispatchClose
	var err error
	if workspacesession.Terminal(to) {
		finish, err = w.server.prepareWorkspaceDispatchClose(ctx, scope, record.ID)
		if err != nil {
			return record, err
		}
	}
	var moved workspaceRecord
	err = w.server.workspaceWrite(ctx, scope, func(writer workspaceWriter, _ time.Time) error {
		var err error
		moved, err = w.transitionRecord(ctx, writer, record, to, reason, now)
		if err != nil {
			return err
		}
		if finish != nil {
			return finish(writer)
		}
		return nil
	})
	return moved, err
}

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
	var record workspaceRecord
	var err error

	record, err = w.readWorkspaceIDRecord(ctx, connection.scope.organization, connection.workspaceID)

	if err != nil {
		// The periodic authority re-check reads the same row and closes the
		// connection when it can, so a failed read here is logged, not fatal.
		w.logger.Warn("workspace.relay_attach_recheck_failed", "workspace_id", connection.workspaceID,
			"connection_id", connection.id, "error", err)
		return
	}
	if reason, ends := relayEndReason(record.State, connection.runner); ends {
		w.relay.closeWorkspace(ctx, record.ID, reason)
	}
}

// endWorkspace takes a workspace to a terminal state from wherever it is. It
// is the single exit the sweeps, the DELETE handler and the unbind path share,
// so no caller has to know whether closing means closing → closed or a direct
// move.

// Every non-terminal state either reaches the terminal one directly or
// reaches it through closing; there is no third shape.

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

// dueWorkspaces reads every open workspace. The set is small by construction:
// plan.workspaces.max_open bounds it per organization, so the sweep reads it
// whole rather than encoding each deadline as a separate query.
func (w *workspaceService) dueWorkspaces(ctx context.Context, now time.Time) ([]workspaceRecord, error) {

	return w.activeWorkspaceRecords(ctx)

}

// workspaceDeadline is what the sweep decided about one workspace.
type workspaceDeadline struct {
	state  string
	reason string
	// endedAt is when occupancy stopped, which is not always now: a lease
	// that expired stopped serving at expiry, not when the hub noticed.
	endedAt time.Time
	apply   bool
}

// decideDeadline reports the transition a workspace is due, if any. It is pure
// so every branch of the workspace session contract's timing can be tested without a database.
//
// It takes no configuration on purpose. Every bound it applies is already on
// the record -- requested_expires_at from workspaces.request_timeout,
// expires_at from workspaces.max_lifetime, idle_timeout_seconds from
// workspaces.idle_timeout -- because those were resolved when the workspace was
// created. Reading the live configuration here would mean a workspace's
// deadlines changed under it when an operator edited a file, which is not what
// a person who opened a panel five minutes ago agreed to.
func decideDeadline(record workspaceRecord, now time.Time) workspaceDeadline {
	switch record.State {
	case workspacesession.StateRequested:
		if record.RebindDeadline != nil {
			if now.Before(*record.RebindDeadline) {
				return workspaceDeadline{}
			}
			// The original runner did not come back inside the grace, so the
			// hub revokes that lease and the workspace fails rather than
			// being re-dispatched under a tuple two runners could hold.
			return workspaceDeadline{state: workspacesession.StateFailed, reason: workspacesession.ReasonHubRestarted, endedAt: now, apply: true}
		}
		if !now.Before(record.RequestedExpiresAt) {
			return workspaceDeadline{state: workspacesession.StateFailed, reason: workspacesession.ReasonNoRunner, endedAt: now, apply: true}
		}
	case workspacesession.StateStarting, workspacesession.StateReady, workspacesession.StateIdle:
		if record.LastHeartbeatAt != nil {
			expiry := record.LastHeartbeatAt.Add(workspacesession.LeaseTTL)
			if !now.Before(expiry) {
				return workspaceDeadline{state: workspacesession.StateFailed, reason: workspacesession.ReasonLeaseLost, endedAt: expiry, apply: true}
			}
			missed := record.LastHeartbeatAt.Add(workspacesession.MissedHeartbeats * workspacesession.HeartbeatInterval)
			if !now.Before(missed) && record.State != workspacesession.StateStarting {
				return workspaceDeadline{state: workspacesession.StateUnreachable, reason: workspacesession.ReasonRunnerRestarted, endedAt: now, apply: true}
			}
		}
		if !now.Before(record.ExpiresAt) {
			return workspaceDeadline{state: workspacesession.StateClosed, reason: workspacesession.ReasonExpired, endedAt: now, apply: true}
		}
		if idle := workspaceIdleSince(record); !idle.IsZero() {
			if !now.Before(idle.Add(time.Duration(record.IdleTimeoutSeconds) * time.Second)) {
				return workspaceDeadline{state: workspacesession.StateClosed, reason: workspacesession.ReasonExpired, endedAt: now, apply: true}
			}
			if record.State == workspacesession.StateReady && !now.Before(idle.Add(workspaceIdleMark)) {
				return workspaceDeadline{state: workspacesession.StateIdle, endedAt: now, apply: true}
			}
		}
	case workspacesession.StateUnreachable:
		if record.LastHeartbeatAt != nil {
			expiry := record.LastHeartbeatAt.Add(workspacesession.LeaseTTL)
			if !now.Before(expiry) {
				return workspaceDeadline{state: workspacesession.StateFailed, reason: workspacesession.ReasonLeaseLost, endedAt: expiry, apply: true}
			}
		}
		// An unreachable workspace closes after idle_timeout_seconds, counted
		// from when it went unreachable, which is the last update it had.
		if !now.Before(record.UpdatedAt.Add(time.Duration(record.IdleTimeoutSeconds) * time.Second)) {
			return workspaceDeadline{state: workspacesession.StateClosed, reason: workspacesession.ReasonExpired, endedAt: now, apply: true}
		}
	case workspacesession.StateClosing:
		// A runner that never answers the close must not hold the slot for
		// ever; the lease TTL is the same bound the rest of the machine uses.
		if !now.Before(record.UpdatedAt.Add(workspacesession.LeaseTTL)) {
			return workspaceDeadline{state: workspacesession.StateClosed, reason: record.Reason, endedAt: now, apply: true}
		}
	}
	if !now.Before(record.ExpiresAt) {
		return workspaceDeadline{state: workspacesession.StateClosed, reason: workspacesession.ReasonExpired, endedAt: now, apply: true}
	}
	return workspaceDeadline{}
}

// workspaceIdleMark is how long a ready workspace goes without a
// person-originated frame before it reports idle. It is a fraction of the idle
// timeout rather than a separate setting: idle is a label on the way to
// expiry, not a deadline of its own.
const workspaceIdleMark = 2 * time.Minute

// workspaceIdleSince reports when the idle clock started: the last
// person-originated activity, or the moment the workspace opened. Runner output
// and watches never reset it, which is why the column is written only by the
// relay's person side.
func workspaceIdleSince(record workspaceRecord) time.Time {
	if record.LastActivityAt != nil {
		return *record.LastActivityAt
	}
	if record.OpenedAt != nil {
		return *record.OpenedAt
	}
	return record.CreatedAt
}

// applyDeadline commits the decided transition.
func (w *workspaceService) applyDeadline(ctx context.Context, record workspaceRecord, now time.Time) (workspaceRecord, bool, error) {

	return w.applyDeadlineRecord(ctx, record, now)

	// Something else moved it between the read and the write; the
	// next sweep decides again from what is actually stored.

}

// errWorkspaceUnchanged aborts a sweep transaction that found the workspace
// already moved. It is never returned to a caller.
var errWorkspaceUnchanged = errors.New("workspace changed under the sweep")

// recoverOpenWorkspaces re-marks every open workspace requested on hub restart
// and gives the original runner the re-bind grace. A workspace
// that was still only requested keeps waiting for a runner; one that had an
// owner keeps its tuple, because the grace exists precisely so that runner can
// present it again.
func (w *workspaceService) recoverOpenWorkspaces(ctx context.Context) error {

	return w.recoverWorkspaceRecords(ctx)

	// Already requested and already inside a grace: extend it
	// rather than move, so a restart loop cannot revoke a lease
	// the runner never had a chance to present.

}

// workspaceReadOnly reports whether the subject attempt is still running, which
// is what holds a workspace read-only and refuses a terminal on it.
func workspaceAttemptRunning(ctx context.Context, query nativeQueryer, scope nativeScope, attemptID string) (bool, error) {
	if attemptID == "" {
		return false, nil
	}
	var status string
	err := query.QueryRowContext(ctx, `SELECT status FROM native_attempts
WHERE id = ? AND organization_id = ? AND project_id = ?`, attemptID, scope.organization, scope.project).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read workspace attempt status: %w", err)
	}
	return status == "running", nil
}

// workspaceAttemptRunner reports which runner ran an attempt and when it
// finished, so a retained worktree can be offered only to the runner that
// holds it.
func workspaceAttemptRunner(ctx context.Context, query nativeQueryer, scope nativeScope, attemptID string) (runnerID string, finishedAt time.Time, found bool, err error) {
	var updated string
	var data string
	queryErr := query.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(payload_object_key,''),data_json), updated_at FROM native_attempts
WHERE id = ? AND organization_id = ? AND project_id = ?`, attemptID, scope.organization, scope.project).Scan(&data, &updated)
	if errors.Is(queryErr, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if queryErr != nil {
		return "", time.Time{}, false, fmt.Errorf("read workspace attempt: %w", queryErr)
	}
	var execution tracker.NativeRunData
	if err := decodeStorageJSON(ctx, []byte(data), &execution); err != nil {
		return "", time.Time{}, false, fmt.Errorf("decode workspace attempt data: %w", err)
	}
	finished, err := parseTimeValue(updated)
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("decode workspace attempt updated_at: %w", err)
	}
	return execution.RunnerID, finished, true, nil
}

func workspaceTickInterval(base time.Duration) time.Duration {
	if base <= 0 {
		base = workspaceSweepInterval
	}
	spread := max(int64(base/5), 1)
	return base - time.Duration(spread) + time.Duration(rand.N(2*spread))
}
