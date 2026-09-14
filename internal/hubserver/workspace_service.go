package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The workspace session service (decisions section 18.1). It owns the state
// machine, the limits, the dispatch association and the sweeps that make the
// timeouts real. HTTP handlers, worker binding and the relay live in sibling
// files.

// Workspace failures, in the native error shape of section 5.

// workspaceExists reports a second request on an attempt that already has an
// open workspace. It names the existing id, because the client's next move is
// to use it rather than to retry.
func workspaceExists(id string) error {
	return &nativeError{
		Code: "workspace_exists", Message: "A workspace is already open on this attempt",
		status: http.StatusConflict, Details: map[string]any{"workspace_id": id},
	}
}

// workspaceLimit reports the per-organization or per-person cap. It names
// which one was reached, so the answer is actionable rather than only a
// refusal.
func workspaceLimit(scope string, limit int) error {
	return &nativeError{
		Code: "workspace_limit", Message: "The open workspace limit has been reached",
		status: http.StatusUnprocessableEntity, Details: map[string]any{"scope": scope, "limit": limit},
	}
}

// workspaceUnavailable reports that the hub is not running workspace sessions.
func workspaceUnavailable() error {
	return &nativeError{
		Code: "unsupported_control", Message: "Workspace sessions are not enabled on this hub",
		status: http.StatusUnprocessableEntity,
	}
}

// workspaceService owns workspace sessions inside the hub.
type workspaceService struct {
	server *Service
	config WorkspaceConfig
	logger *slog.Logger
	// broker wakes project stream subscribers after a transition committed.
	broker *projectEventBroker
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
		broker:        newProjectEventBroker(),
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
	w.stopOnce.Do(func() {
		close(w.stop)
		if w.cancelSweep != nil {
			w.cancelSweep()
		}
		<-w.stopped
		// The hub is going away, so there is no request or sweep whose context
		// this belongs to: the relay's last writes -- the action runs that were
		// still streaming -- are the process's own work, and a context of its
		// own is the honest description of that.
		w.relay.stop(context.Background())
	})
}

func (w *workspaceService) maintain(ctx context.Context) {
	defer close(w.stopped)
	ticker := time.NewTicker(w.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

// now reads the hub clock through the service's configuration so a test can
// drive every deadline.
func (w *workspaceService) now() time.Time { return w.server.config.now().UTC() }

// transition applies one move of the state machine inside the caller's
// transaction: it validates the move, updates the row, keeps occupancy in
// step, closes the dispatch association on a terminal state and appends the
// workspace.<state> project event with the resource as data.
//
// It never notifies subscribers itself. The wake has to happen after the
// commit, or a subscriber would read from its cursor and find nothing, so
// every caller ends with committed().
func (w *workspaceService) transition(ctx context.Context, tx *sql.Tx, record workspaceRecord, to, reason string, now time.Time) (workspaceRecord, error) {
	if !workspacesession.Transition(record.State, to) {
		return record, nativeStaleExecution("A workspace cannot move from " + record.State + " to " + to)
	}
	if reason != "" && !workspacesession.ValidReason(reason) {
		return record, nativeInvalid("Unknown workspace reason " + reason)
	}
	expected := record.Revision
	previous := record.State
	record.State = to
	record.Reason = reason
	record.Revision++
	record.UpdatedAt = now
	switch to {
	case workspacesession.StateReady:
		if record.OpenedAt == nil {
			opened := now
			record.OpenedAt = &opened
		}
		record.RebindDeadline = nil
	case workspacesession.StateRequested:
		// A re-request drops the runner's claim on the resource but keeps the
		// tuple, because the original runner may still re-bind with it inside
		// the grace window.
		record.Capabilities = nil
	}
	if err := updateWorkspace(ctx, tx, record, expected); err != nil {
		return record, err
	}
	switch {
	case to == workspacesession.StateStarting:
		if err := openWorkspaceOccupancy(ctx, tx, record, now); err != nil {
			return record, err
		}
	case workspacesession.Terminal(to):
		if err := closeWorkspaceOccupancy(ctx, tx, record.ID, now); err != nil {
			return record, err
		}
		if err := w.closeWorkspaceDispatchItem(ctx, tx, record, now); err != nil {
			return record, err
		}
	}
	if _, err := appendProjectEvent(ctx, tx, record.OrganizationID, record.ProjectID,
		workspacesession.EventType(to), record.ID, record.resource(), now); err != nil {
		return record, err
	}
	w.logger.Info("workspace.transitioned", "workspace_id", record.ID, "from", previous, "to", to,
		"reason", reason, "runner_id", record.RunnerID)
	return record, nil
}

// committed wakes the project's stream subscribers and, where the workspace
// can no longer serve what a person is holding, closes its relay connections.
// Call it after the transaction that transitioned committed, never inside it.
//
// It takes the context of whatever committed the transition, because closing a
// workspace's connections also records the action runs that were streaming on
// them (section 18.12) and that write needs one. The write drops the
// cancellation and keeps the values; see failExecRuns.
func (w *workspaceService) committed(ctx context.Context, record workspaceRecord) {
	w.broker.notify(record.OrganizationID, record.ProjectID)
	if w.relay == nil {
		return
	}
	switch {
	case workspacesession.Terminal(record.State):
		w.relay.closeWorkspace(ctx, record.ID, workspacesession.CodeWorkspaceClosed)
	case record.State == workspacesession.StateUnreachable, record.State == workspacesession.StateRequested:
		// The runner is gone or has been asked to re-bind, so the streams it
		// was serving cannot be served. A person holding one is told rather
		// than left waiting on frames that will never arrive.
		w.relay.closeWorkspace(ctx, record.ID, workspacesession.CodeStaleExecution)
	}
}

// endWorkspace takes a workspace to a terminal state from wherever it is. It
// is the single exit the sweeps, the DELETE handler and the unbind path share,
// so no caller has to know whether closing means closing → closed or a direct
// move.
func (w *workspaceService) endWorkspace(ctx context.Context, tx *sql.Tx, record workspaceRecord, terminal, reason string, now time.Time) (workspaceRecord, error) {
	if workspacesession.Terminal(record.State) {
		return record, nil
	}
	if workspacesession.Transition(record.State, terminal) {
		return w.transition(ctx, tx, record, terminal, reason, now)
	}
	// Every non-terminal state either reaches the terminal one directly or
	// reaches it through closing; there is no third shape.
	moved, err := w.transition(ctx, tx, record, workspacesession.StateClosing, reason, now)
	if err != nil {
		return record, err
	}
	return w.transition(ctx, tx, moved, terminal, reason, now)
}

// readWorkspaceForActor reads a workspace under the issue's read rule: the
// subject issue is resolved in the caller's scope, so a reader who cannot see
// the issue cannot see its workspace.
func (w *workspaceService) readWorkspaceForActor(ctx context.Context, query nativeQueryer, scope nativeScope, id string) (workspaceRecord, error) {
	if err := workspacesession.ValidateID(id); err != nil {
		return workspaceRecord{}, nativeNotFound()
	}
	record, err := readWorkspaceRow(ctx, query, scope, id)
	if errors.Is(err, sql.ErrNoRows) {
		return workspaceRecord{}, nativeNotFound()
	}
	if err != nil {
		return workspaceRecord{}, err
	}
	if _, _, err := readNativeIssue(ctx, query, scope, record.SubjectWorkItemID); err != nil {
		return workspaceRecord{}, err
	}
	return record, nil
}

// sweep enforces every deadline of section 18.1 that no request would reach on
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
	// After the deadlines, so a workspace this tick ended has its action runs
	// failed in the same tick rather than the next one (section 18.12).
	w.sweepActionRuns(ctx)
	// And after the failing, so a run on a workspace this tick ended is failed
	// rather than dispatched: the two read the same rows, and the order is what
	// decides which answer a run on a workspace that just closed gets.
	w.dispatchQueuedActionRuns(ctx)
	if _, err := sweepProjectEvents(ctx, w.server.database.db, now); err != nil {
		w.logger.Warn("workspace.project_event_sweep_failed", "error", err)
	}
	w.relay.sweep(ctx, now)
	if err := w.sweepRelayTickets(ctx, now); err != nil {
		w.logger.Warn("workspace.ticket_sweep_failed", "error", err)
	}
}

// dueWorkspaces reads every open workspace. The set is small by construction:
// plan.workspaces.max_open bounds it per organization, so the sweep reads it
// whole rather than encoding each deadline as a separate query.
func (w *workspaceService) dueWorkspaces(ctx context.Context, now time.Time) ([]workspaceRecord, error) {
	rows, err := w.server.database.db.QueryContext(ctx, `SELECT `+workspaceColumns+` FROM workspace_sessions
WHERE state IN `+openWorkspaceStates+` ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("query open workspaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := []workspaceRecord{}
	for rows.Next() {
		record, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate open workspaces: %w", err)
	}
	return records, nil
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
// so every branch of section 18.1's timing can be tested without a database.
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
// relay's person side (section 18.1).
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
	decision := decideDeadline(record, now)
	if !decision.apply {
		return record, false, nil
	}
	var moved workspaceRecord
	err := w.server.hubTransact(ctx, func(tx *sql.Tx, _ time.Time) error {
		current, err := readWorkspaceByID(ctx, tx, record.ID)
		if err != nil {
			return err
		}
		if current.Revision != record.Revision {
			// Something else moved it between the read and the write; the
			// next sweep decides again from what is actually stored.
			moved = current
			return errWorkspaceUnchanged
		}
		switch {
		case workspacesession.Terminal(decision.state):
			moved, err = w.endWorkspace(ctx, tx, current, decision.state, decision.reason, now)
			if err != nil {
				return err
			}
			return closeWorkspaceOccupancy(ctx, tx, moved.ID, decision.endedAt)
		default:
			moved, err = w.transition(ctx, tx, current, decision.state, decision.reason, now)
			return err
		}
	})
	if errors.Is(err, errWorkspaceUnchanged) {
		return moved, false, nil
	}
	if err != nil {
		return record, false, err
	}
	return moved, true, nil
}

// errWorkspaceUnchanged aborts a sweep transaction that found the workspace
// already moved. It is never returned to a caller.
var errWorkspaceUnchanged = errors.New("workspace changed under the sweep")

// recoverOpenWorkspaces re-marks every open workspace requested on hub restart
// and gives the original runner the re-bind grace (section 18.1). A workspace
// that was still only requested keeps waiting for a runner; one that had an
// owner keeps its tuple, because the grace exists precisely so that runner can
// present it again.
func (w *workspaceService) recoverOpenWorkspaces(ctx context.Context) error {
	now := w.now()
	records, err := w.dueWorkspaces(ctx, now)
	if err != nil {
		return err
	}
	recovered := 0
	for _, record := range records {
		if record.State == workspacesession.StateRequested && record.RebindDeadline == nil {
			continue
		}
		var moved workspaceRecord
		err := w.server.hubTransact(ctx, func(tx *sql.Tx, _ time.Time) error {
			current, err := readWorkspaceByID(ctx, tx, record.ID)
			if err != nil {
				return err
			}
			deadline := now.Add(workspacesession.RebindGrace)
			current.RebindDeadline = &deadline
			if current.State == workspacesession.StateRequested {
				// Already requested and already inside a grace: extend it
				// rather than move, so a restart loop cannot revoke a lease
				// the runner never had a chance to present.
				expected := current.Revision
				current.Revision++
				current.UpdatedAt = now
				return updateWorkspace(ctx, tx, current, expected)
			}
			moved, err = w.transition(ctx, tx, current, workspacesession.StateRequested, workspacesession.ReasonHubRestarted, now)
			return err
		})
		if err != nil {
			return fmt.Errorf("recover workspace %s: %w", record.ID, err)
		}
		recovered++
		if moved.ID != "" {
			w.committed(ctx, moved)
		}
	}
	if recovered > 0 {
		w.logger.Info("workspace.restart_recovered", "workspaces", recovered, "grace_seconds", int(workspacesession.RebindGrace/time.Second))
	}
	return nil
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
	queryErr := query.QueryRowContext(ctx, `SELECT data_json, updated_at FROM native_attempts
WHERE id = ? AND organization_id = ? AND project_id = ?`, attemptID, scope.organization, scope.project).Scan(&data, &updated)
	if errors.Is(queryErr, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if queryErr != nil {
		return "", time.Time{}, false, fmt.Errorf("read workspace attempt: %w", queryErr)
	}
	var execution tracker.NativeRunData
	if err := json.Unmarshal([]byte(data), &execution); err != nil {
		return "", time.Time{}, false, fmt.Errorf("decode workspace attempt data: %w", err)
	}
	finished, err := parseTimeValue(updated)
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("decode workspace attempt updated_at: %w", err)
	}
	return execution.RunnerID, finished, true, nil
}
