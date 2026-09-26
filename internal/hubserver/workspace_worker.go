package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Worker endpoints for workspace sessions (decisions section 18.1). They
// mirror the conversation worker set of section 5: bind claims the workspace
// and returns the tuple and the checkout instructions, heartbeat renews the
// lease and carries the runner's report, and unbind releases it.
//
// All three are fenced by the workspace owner tuple, which is this workspace's
// own generation and not the subject attempt's. There is no state in which two
// runners hold the workspace: a bind that does not match the current lease, or
// that arrives after the re-bind grace, is refused with stale_execution.

// workspaceWorkerIdentity is the tuple every workspace worker request carries.
type workspaceWorkerIdentity struct {
	LeaseID      tracker.LeaseID      `json:"lease_id"`
	FencingToken tracker.FencingToken `json:"fencing_token,string"`
}

// workspaceBindRequest claims a workspace for the runner that holds its item's
// lease.
type workspaceBindRequest struct {
	workspaceWorkerIdentity
	// Capabilities is what this runner can actually serve for this workspace.
	// It is checked against requires: a runner that claimed the item and then
	// reports less than it promised fails the workspace rather than opening
	// one that cannot do what was asked.
	Capabilities workspacesession.Capabilities `json:"capabilities"`
	Isolation    string                        `json:"isolation,omitempty"`
}

// workspaceCheckout is what the runner is told to produce.
type workspaceCheckout struct {
	// WorkItemID is the subject issue, not the workspace item: the worktree
	// belongs to the work, and the workspace item is only how the runner was
	// handed the job.
	WorkItemID string `json:"work_item_id"`
	AttemptID  string `json:"attempt_id,omitempty"`
	Ref        string `json:"ref,omitempty"`
	HeadSHA    string `json:"head_sha,omitempty"`
	// Worktree is "retained" when the attempt's own worktree should still
	// exist on this runner and "fresh" when the retention window has passed
	// and the runner checks out head_sha into a new one.
	Worktree string `json:"worktree"`
	// ReadOnly is set while the subject attempt runs.
	ReadOnly bool `json:"read_only"`
	// Requires is the surface list the workspace was opened for.
	Requires []string `json:"requires"`
	// IdleTimeoutSeconds and HeartbeatSeconds are the deadlines the runner
	// must keep: a heartbeat every HeartbeatSeconds, and no expectation of
	// living past the workspace's own expiry.
	IdleTimeoutSeconds int       `json:"idle_timeout_seconds"`
	HeartbeatSeconds   int       `json:"heartbeat_seconds"`
	ExpiresAt          time.Time `json:"expires_at"`
	// Deny is the project's extra files denylist, so the runner filters with
	// the same list the hub would (section 18.4).
	Deny []string `json:"deny,omitempty"`
}

// workspaceBindResponse answers a successful bind.
type workspaceBindResponse struct {
	Owner    workspacesession.Owner   `json:"owner"`
	Checkout workspaceCheckout        `json:"checkout"`
	Session  workspacesession.Session `json:"workspace"`
}

// workspaceHeartbeatRequest renews the lease and carries the runner's report.
type workspaceHeartbeatRequest struct {
	workspaceWorkerIdentity
	// State is what the runner believes: starting while it prepares the
	// worktree, ready once the surfaces can be used. The hub accepts only the
	// moves the state machine allows and answers the stored state either way,
	// so a runner that disagrees converges rather than fighting.
	State        string                        `json:"state,omitempty"`
	Reason       string                        `json:"reason,omitempty"`
	HeadSHA      string                        `json:"head_sha,omitempty"`
	Capabilities workspacesession.Capabilities `json:"capabilities"`
	Isolation    string                        `json:"isolation,omitempty"`
	Worktree     string                        `json:"worktree,omitempty"`
	// WorktreePath and MachineHostname are where the worktree actually is
	// (section 18.13): the absolute path this runner prepared and the host it
	// prepared it on. The Open picker compares the hostname against the
	// reader's own machine and hands the operating system the path only when
	// the two agree.
	WorktreePath    string `json:"worktree_path,omitempty"`
	MachineHostname string `json:"machine_hostname,omitempty"`
}

// workspaceHeartbeatResponse answers a heartbeat with the workspace as stored,
// which is how a runner learns it has been asked to close.
type workspaceHeartbeatResponse struct {
	Session workspacesession.Session `json:"workspace"`
}

// workspaceUnbindRequest releases the workspace.
type workspaceUnbindRequest struct {
	workspaceWorkerIdentity
	Reason string `json:"reason"`
}

// loadWorkspaceForWorker resolves the workspace a worker request names and
// proves the caller currently owns it. The lease is read and re-validated on
// every call, not trusted from the bind, because a lease that expired between
// two heartbeats is exactly the case the fencing token exists for.
func (w *workspaceService) loadWorkspaceForWorker(ctx context.Context, tx *sql.Tx, scope nativeScope, id string, identity workspaceWorkerIdentity, now time.Time) (workspaceRecord, leaseRecord, error) {
	if err := workspacesession.ValidateID(id); err != nil {
		return workspaceRecord{}, leaseRecord{}, nativeNotFound()
	}
	if strings.TrimSpace(string(identity.LeaseID)) == "" || identity.FencingToken <= 0 {
		return workspaceRecord{}, leaseRecord{}, nativeInvalid("A lease and fencing token are required")
	}
	record, err := readWorkspaceRow(ctx, tx, scope, id)
	if errors.Is(err, sql.ErrNoRows) {
		return workspaceRecord{}, leaseRecord{}, nativeNotFound()
	}
	if err != nil {
		return workspaceRecord{}, leaseRecord{}, err
	}
	if err := requireRunnerAuthority(ctx, tx, scope, now); err != nil {
		return workspaceRecord{}, leaseRecord{}, err
	}
	lease, leaseFound, err := readLeaseByID(ctx, tx, identity.LeaseID)
	if err != nil {
		return workspaceRecord{}, leaseRecord{}, err
	}
	if !leaseFound {
		return workspaceRecord{}, leaseRecord{}, nativeNotFound()
	}
	if err := requireCurrentLease(lease, identity.FencingToken, now); err != nil {
		return workspaceRecord{}, leaseRecord{}, nativeStaleLease(err, "The workspace lease is no longer current")
	}
	if err := requireLeaseRunner(ctx, tx, identity.LeaseID, scope); err != nil {
		return workspaceRecord{}, leaseRecord{}, err
	}
	if err := requireWorkspaceLeaseItem(ctx, tx, record, lease); err != nil {
		return workspaceRecord{}, leaseRecord{}, err
	}
	return record, lease, nil
}

// requireWorkspaceLeaseItem proves the lease is held on this workspace's own
// dispatch item. Without it a runner holding any lease in the project could
// bind any workspace.
func requireWorkspaceLeaseItem(ctx context.Context, tx *sql.Tx, record workspaceRecord, lease leaseRecord) error {
	var native string
	err := tx.QueryRowContext(ctx, `SELECT wi.work_item_id FROM workspace_items wi
JOIN issues i ON i.native_id = wi.work_item_id AND i.organization_id = wi.organization_id AND i.project_id = wi.project_id
WHERE wi.workspace_id = ? AND i.id = ?`, record.ID, lease.issueID).Scan(&native)
	if errors.Is(err, sql.ErrNoRows) {
		return nativeStaleExecution("The lease does not hold this workspace's work item")
	}
	if err != nil {
		return err
	}
	return nil
}

// bindWorkspaceWorker implements POST .../workspaces/:workspace/worker/bind.
func (s *Service) bindWorkspaceWorker(c echo.Context) error {
	var request workspaceBindRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	service, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if !workspacesession.ValidIsolation(request.Isolation) {
		return s.nativeAPIError(c, nativeInvalid("isolation must be user or container"))
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	var response workspaceBindResponse
	var bound workspaceRecord
	err = s.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		record, _, err := service.loadWorkspaceForWorker(ctx, tx, scope, c.Param("workspace"), request.workspaceWorkerIdentity, now)
		if err != nil {
			return err
		}
		bound, err = service.bind(ctx, tx, scope, record, request, now)
		if err != nil {
			return err
		}
		checkout, err := service.checkoutFor(ctx, tx, bound, now)
		if err != nil {
			return err
		}
		response = workspaceBindResponse{Owner: bound.owner(), Checkout: checkout, Session: bound.resource()}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	service.committed(ctx, bound)
	return c.JSON(http.StatusOK, response)
}

// bind moves a requested workspace to starting, or back to ready when the
// original runner re-bound inside the hub-restart grace.
func (w *workspaceService) bind(ctx context.Context, tx *sql.Tx, scope nativeScope, record workspaceRecord, request workspaceBindRequest, now time.Time) (workspaceRecord, error) {
	if record.State != workspacesession.StateRequested {
		return record, nativeStaleExecution("The workspace is not waiting for a runner")
	}
	runnerID := scope.credential.Runner.RunnerID
	rebinding := record.RebindDeadline != nil
	if rebinding {
		// Hub restart: only the runner that held the workspace may present
		// the old tuple, and only inside the grace. A late bind is refused
		// with stale_execution rather than silently accepted, because the
		// hub may already have revoked that lease.
		if now.After(*record.RebindDeadline) {
			return record, nativeStaleExecution("The workspace re-bind grace has passed")
		}
		if record.RunnerID != "" && record.RunnerID != runnerID {
			return record, nativeStaleExecution("The workspace belongs to another runner")
		}
	}
	if !request.Capabilities.Satisfies(record.Requires) {
		return record, nativeInvalid("The runner does not serve every required surface")
	}
	capabilities := request.Capabilities
	record.RunnerID = runnerID
	record.MachineID = scope.credential.Runner.MachineID
	record.LeaseID = request.LeaseID
	record.FencingToken = request.FencingToken
	record.Capabilities = &capabilities
	record.Isolation = request.Isolation
	heartbeat := now
	record.LastHeartbeatAt = &heartbeat
	if rebinding {
		// The workspace was open before the restart, so it returns to ready
		// with no relay sessions carried over; the runner still has the
		// worktree it had.
		return w.transition(ctx, tx, record, workspacesession.StateReady, "", now)
	}
	return w.transition(ctx, tx, record, workspacesession.StateStarting, "", now)
}

// checkoutFor builds the instructions a bound runner needs. It decides
// retained versus fresh from the retention window rather than asking the
// runner, so the two sides cannot disagree about which worktree is meant.
func (w *workspaceService) checkoutFor(ctx context.Context, tx *sql.Tx, record workspaceRecord, now time.Time) (workspaceCheckout, error) {
	checkout := workspaceCheckout{
		WorkItemID: record.SubjectWorkItemID, AttemptID: record.AttemptID, Ref: record.Ref,
		HeadSHA: record.HeadSHA, Worktree: workspacesession.WorktreeFresh, ReadOnly: record.ReadOnly,
		Requires: record.Requires, IdleTimeoutSeconds: record.IdleTimeoutSeconds,
		HeartbeatSeconds: int(workspacesession.HeartbeatInterval / time.Second),
		ExpiresAt:        record.ExpiresAt, Deny: w.config.FilesDeny,
	}
	if record.AttemptID == "" {
		return checkout, nil
	}
	retained, err := retainedWorktreeRunnerFor(ctx, tx, record, w.config.RetainAfterRun, now)
	if err != nil {
		return checkout, err
	}
	if retained != "" && retained == record.RunnerID {
		checkout.Worktree = workspacesession.WorktreeRetained
	}
	return checkout, nil
}

// heartbeatWorkspaceWorker implements POST .../worker/heartbeat. It renews the
// workspace lease and carries the runner's state, head_sha, capabilities and
// isolation.
func (s *Service) heartbeatWorkspaceWorker(c echo.Context) error {
	var request workspaceHeartbeatRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	service, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if request.State != "" && !workspacesession.ValidState(request.State) {
		return s.nativeAPIError(c, nativeInvalid("state names no workspace state"))
	}
	if !workspacesession.ValidIsolation(request.Isolation) {
		return s.nativeAPIError(c, nativeInvalid("isolation must be user or container"))
	}
	if !workspacesession.ValidWorktree(request.Worktree) {
		return s.nativeAPIError(c, nativeInvalid("worktree must be retained or fresh"))
	}
	if request.Reason != "" && !workspacesession.ValidReason(request.Reason) {
		return s.nativeAPIError(c, nativeInvalid("reason names no workspace reason"))
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	var beat workspaceRecord
	var moved bool
	err = s.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		record, lease, err := service.loadWorkspaceForWorker(ctx, tx, scope, c.Param("workspace"), request.workspaceWorkerIdentity, now)
		if err != nil {
			return err
		}
		beat, moved, err = service.heartbeat(ctx, tx, record, request, now)
		if err != nil {
			return err
		}
		return renewWorkspaceLease(ctx, tx, lease, beat, now)
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if moved {
		service.committed(ctx, beat)
	}
	return c.JSON(http.StatusOK, workspaceHeartbeatResponse{Session: beat.resource()})
}

// renewWorkspaceLease is the half of the heartbeat that section 18.1 names
// first: "POST .../worker/heartbeat (renews the lease ...)". It runs in the
// same transaction as the session row, so the two can never disagree about
// whether this generation is still alive, and the runner needs no second call
// to POST /leases/:id/renew to keep a workspace past one lease TTL.
//
// The window is the workspace's own LeaseTTL rather than whatever the claim
// asked for, because the workspace machine already measures lease_lost from it:
// a lease that outlived the heartbeat the hub counts from would hand a runner
// authority over a worktree the hub had already given up on.
//
// A heartbeat that ended the workspace releases the lease instead of renewing
// it. A closed or failed workspace holds no runner slot and serves no frame, so
// leaving its claim alive would keep one of the runner's capacity slots and let
// the relay revalidate a person's frame against a generation that is over.
func renewWorkspaceLease(ctx context.Context, tx *sql.Tx, lease leaseRecord, record workspaceRecord, now time.Time) error {
	if workspacesession.Terminal(record.State) {
		reason := record.Reason
		if reason == "" {
			reason = record.State
		}
		return releaseLeaseRow(ctx, tx, lease, reason, now)
	}
	return renewLeaseRow(ctx, tx, lease.session.ID, lease.session.FencingToken, now.Add(workspacesession.LeaseTTL), now)
}

// heartbeat records the runner's report and applies the state it asks for when
// the machine allows it. A report that asks for an illegal move is not an
// error: the stored state is returned and the runner converges on it, because
// a heartbeat that failed would also stop renewing the lease.
func (w *workspaceService) heartbeat(ctx context.Context, tx *sql.Tx, record workspaceRecord, request workspaceHeartbeatRequest, now time.Time) (workspaceRecord, bool, error) {
	if workspacesession.Terminal(record.State) {
		return record, false, nativeStaleExecution("The workspace has ended")
	}
	expected := record.Revision
	beat := now
	record.LastHeartbeatAt = &beat
	if request.HeadSHA != "" {
		record.HeadSHA = request.HeadSHA
	}
	if request.Worktree != "" {
		record.Worktree = request.Worktree
	}
	if request.Isolation != "" {
		record.Isolation = request.Isolation
	}
	// The path and the host are corrected by whatever the runner last said and
	// never cleared by a beat that omits them: a runner too old to report them
	// would otherwise erase what a newer one had already recorded.
	if request.WorktreePath != "" {
		record.WorktreePath = request.WorktreePath
	}
	if request.MachineHostname != "" {
		record.MachineHostname = request.MachineHostname
	}
	capabilities := request.Capabilities
	record.Capabilities = &capabilities
	target := request.State
	// A runner that reports worktree_missing fails the workspace whatever
	// state it claims: the worktree is what the surfaces are.
	if request.Reason == workspacesession.ReasonWorktreeMissing {
		target = workspacesession.StateFailed
	}
	if target != "" && target != record.State && workspacesession.Transition(record.State, target) {
		moved, err := w.transition(ctx, tx, record, target, request.Reason, now)
		return moved, true, err
	}
	if record.State == workspacesession.StateUnreachable {
		// The runner is answering again, which is the only way back.
		moved, err := w.transition(ctx, tx, record, workspacesession.StateReady, "", now)
		return moved, true, err
	}
	record.Revision++
	record.UpdatedAt = now
	if err := updateWorkspace(ctx, tx, record, expected); err != nil {
		return record, false, err
	}
	return record, false, nil
}

// unbindWorkspaceWorker implements POST .../worker/unbind.
func (s *Service) unbindWorkspaceWorker(c echo.Context) error {
	var request workspaceUnbindRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	service, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if request.Reason != "" && !workspacesession.ValidReason(request.Reason) {
		return s.nativeAPIError(c, nativeInvalid("reason names no workspace reason"))
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	var ended workspaceRecord
	err = s.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		record, _, err := service.loadWorkspaceForWorker(ctx, tx, scope, c.Param("workspace"), request.workspaceWorkerIdentity, now)
		if err != nil {
			return err
		}
		if workspacesession.Terminal(record.State) {
			ended = record
			return nil
		}
		// A runner restart unbinds every workspace it held and the workspace
		// fails; a clean close reached closing first and ends closed. The
		// reason is what separates the two, so it decides the terminal state
		// rather than a second field that could disagree with it.
		terminal := workspacesession.StateFailed
		if record.State == workspacesession.StateClosing || request.Reason == workspacesession.ReasonClosedByActor || request.Reason == workspacesession.ReasonExpired {
			terminal = workspacesession.StateClosed
		}
		ended, err = service.endWorkspace(ctx, tx, record, terminal, request.Reason, now)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	service.committed(ctx, ended)
	return c.NoContent(http.StatusNoContent)
}

// workspaceForWorkItem implements GET .../work-items/:item/workspace.
//
// A runner claims a work item and is handed a lease on it; the worker
// endpoints are addressed by workspace id. Something has to join the two, and
// the runner cannot be asked to parse it out of the issue body. This is the
// same shape POST /work-items/:item/conversation/bind uses to resolve a
// conversation from the item a runner was handed, and it is a read rather than
// a bind because the runner may need it before it is ready to bind.
//
// It is fenced the same way the bind is: the caller must hold the current lease
// on this very item, so a runner cannot read the workspace of work it did not
// claim.
func (s *Service) workspaceForWorkItem(c echo.Context) error {
	if _, err := s.requireWorkspaces(); err != nil {
		return s.nativeAPIError(c, err)
	}
	identity, err := workerRelayIdentity(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	var resource workspacesession.Session
	err = s.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		record, found, err := readWorkspaceByItem(ctx, tx, c.Param("item"))
		if err != nil {
			return err
		}
		if !found {
			return nativeNotFound()
		}
		if err := requireRunnerAuthority(ctx, tx, scope, now); err != nil {
			return err
		}
		lease, leaseFound, err := readLeaseByID(ctx, tx, identity.LeaseID)
		if err != nil {
			return err
		}
		if !leaseFound {
			return nativeNotFound()
		}
		if err := requireCurrentLease(lease, identity.FencingToken, now); err != nil {
			return nativeStaleLease(err, "The workspace lease is no longer current")
		}
		if err := requireLeaseRunner(ctx, tx, identity.LeaseID, scope); err != nil {
			return err
		}
		if err := requireWorkspaceLeaseItem(ctx, tx, record, lease); err != nil {
			return err
		}
		resource = record.resource()
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, resource)
}

// workerRelayIdentity reads the workspace tuple from the upgrade's headers.
func workerRelayIdentity(c echo.Context) (workspaceWorkerIdentity, error) {
	lease := strings.TrimSpace(c.Request().Header.Get("X-Detent-Lease"))
	token := strings.TrimSpace(c.Request().Header.Get("X-Detent-Fencing-Token"))
	if lease == "" || token == "" {
		return workspaceWorkerIdentity{}, nativeInvalid("A lease and fencing token header are required")
	}
	var fencing int64
	if _, err := fmt.Sscanf(token, "%d", &fencing); err != nil || fencing <= 0 {
		return workspaceWorkerIdentity{}, nativeInvalid("The fencing token header is invalid")
	}
	return workspaceWorkerIdentity{LeaseID: tracker.LeaseID(lease), FencingToken: tracker.FencingToken(fencing)}, nil
}
