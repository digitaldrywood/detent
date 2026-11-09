package hubserver

import (
	"fmt"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Worker endpoints for workspace sessions. They
// mirror the conversation worker set of the conversation contract: bind claims the workspace
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
	// the same list the hub would.
	Deny []string `json:"deny,omitempty"`
}

// workspaceBindResponse answers a successful bind.
type workspaceBindResponse struct {
	Owner    workspacesession.Owner   `json:"owner"`
	Checkout workspaceCheckout        `json:"checkout"`
	Session  workspacesession.Session `json:"workspace"`
	// Actions is the project's run-on-worktree-creation set, in authoring
	// order. The order is the contract: an author
	// who wants install before build writes install first, and nothing else
	// in the shape would say so. The runner starts them itself, which is why
	// they arrive with the checkout rather than being asked for later -- a
	// worktree that is ready before its setup ran is ready in name only.
	Actions []workspacesession.Action `json:"actions,omitempty"`
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
	// WorktreePath and MachineHostname are where the worktree actually is:
	// the absolute path this runner prepared and the host it
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

// requireWorkspaceLeaseItem proves the lease is held on this workspace's own
// dispatch item. Without it a runner holding any lease in the project could
// bind any workspace.

// bindWorkspaceWorker implements POST .../workspaces/:workspace/worker/bind.
func (s *Service) bindWorkspaceWorker(c echo.Context) error {

	return s.bindWorkspaceRecord(c)

}

// createsWorktree reports whether a bind hands the runner a new, writable
// worktree it may run the project's run-on-worktree-creation actions in. A
// re-bind after a hub restart or a retained worktree already ran them, and a
// read-only workspace or a runner without exec must not run them at all.
func createsWorktree(record workspaceRecord, checkout workspaceCheckout) bool {
	return record.State == workspacesession.StateStarting &&
		checkout.Worktree == workspacesession.WorktreeFresh &&
		!record.ReadOnly &&
		record.Capabilities != nil && record.Capabilities.Has(workspacesession.CapabilityExec)
}

// checkoutFor builds the instructions a bound runner needs. It decides
// retained versus fresh from the retention window rather than asking the
// runner, so the two sides cannot disagree about which worktree is meant.

// heartbeatWorkspaceWorker implements POST .../worker/heartbeat. It renews the
// workspace lease and carries the runner's state, head_sha, capabilities and
// isolation.
func (s *Service) heartbeatWorkspaceWorker(c echo.Context) error {

	return s.heartbeatWorkspaceRecord(c)

}

// unbindWorkspaceWorker implements POST .../worker/unbind.
func (s *Service) unbindWorkspaceWorker(c echo.Context) error {

	return s.unbindWorkspaceRecord(c)

	// A runner restart unbinds every workspace it held and the workspace
	// fails; a clean close reached closing first and ends closed. The
	// reason is what separates the two, so it decides the terminal state
	// rather than a second field that could disagree with it.

	// The runner has let go, so the lease goes with the workspace in the
	// same transaction. Left alive it would keep one of the runner's
	// capacity slots until it expired on its own.

}

// unbindReleaseReason is the lease_released reason an unbind records: the
// runner's own reason when it gave one, otherwise the workspace's end.
func unbindReleaseReason(reason string, record workspaceRecord) string {
	if reason != "" {
		return reason
	}
	if record.Reason != "" {
		return record.Reason
	}
	return record.State
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

	return s.workspaceRecordForWorkItem(c)

}

// workspaceActionRunReport is the body of POST...
// /workspaces/:workspace/worker/action-runs.
//
// It is how a run the runner started itself is recorded: a
// run-on-worktree-creation action has no person watching and no exec stream to
// carry its frames, so the runner reports the row instead. With no RunID the
// report creates the run; with one it moves the run it names.
//
// The tuple is a named field rather than embedded, unlike the other worker
// bodies. This body already carries a status, a reason and timestamps of its
// own, and a bare lease_id beside them would read as the run's rather than the
// workspace's.
type workspaceActionRunReport struct {
	IdempotencyKey    string                  `json:"idempotency_key,omitempty"`
	OutputSequence    int64                   `json:"output_sequence,omitempty"`
	WorkspaceIdentity workspaceWorkerIdentity `json:"workspace_identity"`
	ActionID          string                  `json:"action_id"`
	RunID             string                  `json:"run_id,omitempty"`
	Status            string                  `json:"status"`
	ExitCode          *int                    `json:"exit_code,omitempty"`
	Reason            string                  `json:"reason,omitempty"`
	StartedAt         *time.Time              `json:"started_at,omitempty"`
	FinishedAt        *time.Time              `json:"finished_at,omitempty"`
	Output            string                  `json:"output,omitempty"`
	Truncated         bool                    `json:"truncated,omitempty"`
}

// reportWorkspaceActionRun implements POST .../worker/action-runs. It is
// fenced by the workspace owner tuple exactly as the heartbeat is: a runner
// may only write runs of the workspace it currently holds.
func (s *Service) reportWorkspaceActionRun(c echo.Context) error {

	return s.reportWorkspaceActionRunRecord(c)

}

// recordActionRunReport applies one runner report: it creates the run when the
// report names none, and otherwise moves the run it names.

// The cap is applied here as well as in the runner, because the hub's
// column is what has to stay bounded whatever a runner reports.

// created_by stays empty: nobody asked for this run. The action
// carries run_on_worktree_creation and the runner started it --
// which is also what claimed_by says, so the column names an
// executor for every run that is not still queued rather than
// reading as unclaimed for this whole family of them.

// A report is a claim too. The hub's own dispatch skips a run that is
// no longer queued, so this cannot take one out from under it; what it
// prevents is a row that is running with nothing named as running it.

// workerRelayIdentity reads the workspace tuple from the upgrade's headers.
func workerRelayIdentity(c echo.Context) (workspaceWorkerIdentity, error) {
	lease := strings.TrimSpace(c.Request().Header.Get("X-Detent-Lease"))
	token := strings.TrimSpace(c.Request().Header.Get("X-Detent-Fencing-Token"))
	if lease == "" || token == "" {
		return workspaceWorkerIdentity{}, nativePublicInvalid("A lease and fencing token header are required")
	}
	var fencing int64
	if _, err := fmt.Sscanf(token, "%d", &fencing); err != nil || fencing <= 0 {
		return workspaceWorkerIdentity{}, nativePublicInvalid("The fencing token header is invalid")
	}
	return workspaceWorkerIdentity{LeaseID: tracker.LeaseID(lease), FencingToken: tracker.FencingToken(fencing)}, nil
}
