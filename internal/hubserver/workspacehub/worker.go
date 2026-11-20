package workspacehub

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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

// WorkerIdentity is the tuple every workspace worker request carries.
type WorkerIdentity struct {
	LeaseID      tracker.LeaseID      `json:"lease_id"`
	FencingToken tracker.FencingToken `json:"fencing_token,string"`
}

// bindRequest claims a workspace for the runner that holds its item's
// lease.
type bindRequest struct {
	WorkerIdentity
	// Capabilities is what this runner can actually serve for this workspace.
	// It is checked against requires: a runner that claimed the item and then
	// reports less than it promised fails the workspace rather than opening
	// one that cannot do what was asked.
	Capabilities workspacesession.Capabilities `json:"capabilities"`
	Isolation    string                        `json:"isolation,omitempty"`
}

// Checkout is what the runner is told to produce.
type Checkout struct {
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

// BindResponse answers a successful bind.
type BindResponse struct {
	Owner    workspacesession.Owner   `json:"owner"`
	Checkout Checkout                 `json:"checkout"`
	Session  workspacesession.Session `json:"workspace"`
	// Actions is the project's run-on-worktree-creation set, in authoring
	// order. The order is the contract: an author
	// who wants install before build writes install first, and nothing else
	// in the shape would say so. The runner starts them itself, which is why
	// they arrive with the checkout rather than being asked for later -- a
	// worktree that is ready before its setup ran is ready in name only.
	Actions []workspacesession.Action `json:"actions,omitempty"`
}

// heartbeatRequest renews the lease and carries the runner's report.
type heartbeatRequest struct {
	WorkerIdentity
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

// HeartbeatResponse answers a heartbeat with the workspace as stored,
// which is how a runner learns it has been asked to close.
type HeartbeatResponse struct {
	Session workspacesession.Session `json:"workspace"`
}

// unbindRequest releases the workspace.
type unbindRequest struct {
	WorkerIdentity
	Reason string `json:"reason"`
}

func (o *Operations) BindWorker(c echo.Context) error {
	var request bindRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return o.InvalidAPIRequest(c, err)
	}
	err := o.RequireWorkspaces()
	if err != nil {
		return o.NativeAPIError(c, err)
	}
	if !workspacesession.ValidIsolation(request.Isolation) {
		return o.NativeAPIError(c, hubapi.NativePublicInvalid("isolation must be sandbox, container or user"))
	}
	scope, ctx := hubscope.NativeRequestScope(c), c.Request().Context()
	var checkout Checkout
	var actions []workspacesession.Action
	var prepared []actionstore.Record
	prepare := func(ctx context.Context, r tenantstore.Reader, row postgres.WorkspaceSession) error {
		checkout = Checkout{WorkItemID: row.SubjectWorkItemID, AttemptID: row.AttemptID, Ref: row.Ref, HeadSHA: row.HeadSHA, Worktree: workspacesession.WorktreeFresh, ReadOnly: row.ReadOnly, Requires: row.Requires, IdleTimeoutSeconds: int(row.IdleTimeoutSeconds), HeartbeatSeconds: int(workspacesession.HeartbeatInterval / time.Second), ExpiresAt: row.ExpiresAt, Deny: o.Config().FilesDeny}
		owner, ok := r.(actionstore.Reader)
		if !ok {
			return hubapi.WorkspaceUnavailable()
		}
		var err error
		prepared, err = owner.ProjectActions(ctx, scope.Project)
		return err
	}
	hydrate := func(row postgres.WorkspaceSession) error {
		if row.AttemptID != "" {
			attempt, err := o.ReadNativeAttemptTenant(ctx, scope, row.SubjectWorkItemID, row.AttemptID)
			if err != nil {
				return err
			}
			if attempt.RunnerID == scope.Credential.Runner.RunnerID && (attempt.Status == "running" || o.now().Before(attempt.UpdatedAt.Add(o.Config().RetainAfterRun))) {
				checkout.Worktree = workspacesession.WorktreeRetained
			}
		}
		actions = nil
		if checkout.Worktree == workspacesession.WorktreeFresh && !row.ReadOnly && request.Capabilities.Has(workspacesession.CapabilityExec) && row.RebindDeadline == nil {
			for _, record := range prepared {
				if record.Action.RunOnWorktreeCreation {
					action, err := o.HydrateActionRecord(ctx, scope, record)
					if err != nil {
						return err
					}
					actions = append(actions, action)
				}
			}
		}
		return nil
	}
	bound, err := o.mutateWorkspaceWorker(ctx, scope, c.Param("workspace"), request.WorkerIdentity, prepare, hydrate, func(row postgres.WorkspaceSession, now time.Time) (postgres.WorkspaceSession, string, string, error) {
		if row.State != workspacesession.StateRequested {
			return row, "", "", hubapi.NativeStaleExecution("The workspace is not waiting for a runner")
		}
		if row.RebindDeadline != nil && (now.After(*row.RebindDeadline) || row.RunnerID != "" && row.RunnerID != scope.Credential.Runner.RunnerID) {
			return row, "", "", hubapi.NativeStaleExecution("The workspace re-bind grace has passed or belongs to another runner")
		}
		if !request.Capabilities.Satisfies(row.Requires) {
			return row, "", "", hubapi.NativePublicInvalid("The runner does not serve every required surface")
		}
		if !TerminalIsolationAllowed(row.Requires, o.Config().Terminal.Isolation, request.Isolation) {
			return row, "", "", workspaceIsolationRefused()
		}
		target := workspacesession.StateStarting
		if row.RebindDeadline != nil {
			target = workspacesession.StateReady
		}
		row.RunnerID, row.MachineID, row.LeaseID, row.FencingToken = scope.Credential.Runner.RunnerID, string(scope.Credential.Runner.MachineID), string(request.LeaseID), int64(request.FencingToken)
		row.Capabilities = CapabilityNames(request.Capabilities)
		row.Isolation = request.Isolation
		row.LastHeartbeatAt = &now
		return row, target, "", nil
	}, false)
	if err != nil {
		return o.NativeAPIError(c, err)
	}
	o.Committed(ctx, bound)
	return c.JSON(http.StatusOK, BindResponse{Owner: bound.Owner(), Checkout: checkout, Session: bound.Resource(), Actions: actions})
}

// CreatesWorktree reports whether a bind hands the runner a new, writable
// worktree it may run the project's run-on-worktree-creation actions in. A
// re-bind after a hub restart or a retained worktree already ran them, and a
// read-only workspace or a runner without exec must not run them at all.
func CreatesWorktree(record Record, checkout Checkout) bool {
	return record.State == workspacesession.StateStarting &&
		checkout.Worktree == workspacesession.WorktreeFresh &&
		!record.ReadOnly &&
		record.Capabilities != nil && record.Capabilities.Has(workspacesession.CapabilityExec)
}

// heartbeatWorkspaceWorker implements POST .../worker/heartbeat. It renews the
// workspace lease and carries the runner's state, head_sha, capabilities and
// isolation.
func (o *Operations) HeartbeatWorker(c echo.Context) error {
	var request heartbeatRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return o.InvalidAPIRequest(c, err)
	}
	err := o.RequireWorkspaces()
	if err != nil {
		return o.NativeAPIError(c, err)
	}
	if request.State != "" && !workspacesession.ValidState(request.State) {
		return o.NativeAPIError(c, hubapi.NativePublicInvalid("state names no workspace state"))
	}
	if !workspacesession.ValidIsolation(request.Isolation) {
		return o.NativeAPIError(c, hubapi.NativePublicInvalid("isolation must be sandbox, container or user"))
	}
	if !workspacesession.ValidWorktree(request.Worktree) {
		return o.NativeAPIError(c, hubapi.NativePublicInvalid("worktree must be retained or fresh"))
	}
	if request.Reason != "" && !workspacesession.ValidReason(request.Reason) {
		return o.NativeAPIError(c, hubapi.NativePublicInvalid("reason names no workspace reason"))
	}
	scope, ctx := hubscope.NativeRequestScope(c), c.Request().Context()
	beat, err := o.mutateWorkspaceWorker(ctx, scope, c.Param("workspace"), request.WorkerIdentity, nil, nil, func(row postgres.WorkspaceSession, now time.Time) (postgres.WorkspaceSession, string, string, error) {
		if workspacesession.Terminal(row.State) {
			return row, "", "", hubapi.NativeStaleExecution("The workspace has ended")
		}
		isolation := row.Isolation
		if request.Isolation != "" {
			isolation = request.Isolation
		}
		if !TerminalIsolationAllowed(row.Requires, o.Config().Terminal.Isolation, isolation) {
			return row, "", "", workspaceIsolationRefused()
		}
		row.LastHeartbeatAt = &now
		if request.HeadSHA != "" {
			row.HeadSHA = request.HeadSHA
		}
		if request.Worktree != "" {
			row.Worktree = request.Worktree
		}
		if request.Isolation != "" {
			row.Isolation = request.Isolation
		}
		if request.WorktreePath != "" {
			row.WorktreePath = request.WorktreePath
		}
		if request.MachineHostname != "" {
			row.MachineHostname = request.MachineHostname
		}
		row.Capabilities = CapabilityNames(request.Capabilities)
		target, reason := request.State, request.Reason
		if request.Reason == workspacesession.ReasonWorktreeMissing {
			target = workspacesession.StateFailed
		}
		if target != "" && target != row.State && workspacesession.Transition(row.State, target) {
			return row, target, reason, nil
		}
		if row.State == workspacesession.StateUnreachable {
			return row, workspacesession.StateReady, "", nil
		}
		return row, row.State, row.Reason, nil
	}, true)
	if err != nil {
		return o.NativeAPIError(c, err)
	}
	o.Committed(ctx, beat)
	return c.JSON(http.StatusOK, HeartbeatResponse{Session: beat.Resource()})
}

// unbindWorkspaceWorker implements POST .../worker/unbind.
func (o *Operations) UnbindWorker(c echo.Context) error {
	var request unbindRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return o.InvalidAPIRequest(c, err)
	}
	err := o.RequireWorkspaces()
	if err != nil {
		return o.NativeAPIError(c, err)
	}
	if request.Reason != "" && !workspacesession.ValidReason(request.Reason) {
		return o.NativeAPIError(c, hubapi.NativePublicInvalid("reason names no workspace reason"))
	}
	scope, ctx := hubscope.NativeRequestScope(c), c.Request().Context()
	ended, err := o.mutateWorkspaceWorker(ctx, scope, c.Param("workspace"), request.WorkerIdentity, nil, nil, func(row postgres.WorkspaceSession, now time.Time) (postgres.WorkspaceSession, string, string, error) {
		if workspacesession.Terminal(row.State) {
			return row, row.State, row.Reason, nil
		}
		target := workspacesession.StateFailed
		if row.State == workspacesession.StateClosing || request.Reason == workspacesession.ReasonClosedByActor || request.Reason == workspacesession.ReasonExpired {
			target = workspacesession.StateClosed
		}
		return row, target, request.Reason, nil
	}, true)
	if err != nil {
		return o.NativeAPIError(c, err)
	}
	o.Committed(ctx, ended)
	return c.NoContent(http.StatusNoContent)
}

// unbindReleaseReason is the lease_released reason an unbind records: the
// runner's own reason when it gave one, otherwise the workspace's end.
func unbindReleaseReason(reason string, record Record) string {
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
func (o *Operations) ForWorkItem(c echo.Context) error {
	err := o.RequireWorkspaces()
	if err != nil {
		return o.NativeAPIError(c, err)
	}
	identity, err := o.WorkerIdentityFromHeaders(c)
	if err != nil {
		return o.NativeAPIError(c, err)
	}
	scope, ctx := hubscope.NativeRequestScope(c), c.Request().Context()
	var resource workspacesession.Session
	err = o.Repository().ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.WorkspaceReader)
		if !ok {
			return hubapi.WorkspaceUnavailable()
		}
		row, err := owner.WorkspaceByDispatchItem(ctx, scope.Project, tracker.NativeWorkItemID(c.Param("item")))
		if err != nil {
			return err
		}
		row, _, err = o.workspaceWorkerRecord(ctx, r, scope, row.ID, identity, o.now(), false)
		if err != nil {
			return err
		}
		resource = sessionRecord(row).Resource()
		return nil
	})
	if err != nil {
		return o.NativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	return c.JSON(http.StatusOK, resource)
}

// ActionRunReport is the body of POST...
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
type ActionRunReport struct {
	WorkspaceIdentity WorkerIdentity `json:"workspace_identity"`
	ActionID          string         `json:"action_id"`
	RunID             string         `json:"run_id,omitempty"`
	Status            string         `json:"status"`
	ExitCode          *int           `json:"exit_code,omitempty"`
	Reason            string         `json:"reason,omitempty"`
	StartedAt         *time.Time     `json:"started_at,omitempty"`
	FinishedAt        *time.Time     `json:"finished_at,omitempty"`
	Output            string         `json:"output,omitempty"`
	Truncated         bool           `json:"truncated,omitempty"`
}

// reportWorkspaceActionRun implements POST .../worker/action-runs. It is
// fenced by the workspace owner tuple exactly as the heartbeat is: a runner
// may only write runs of the workspace it currently holds.
func (o *Operations) ReportActionRun(c echo.Context) error {
	var request ActionRunReport
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return o.InvalidAPIRequest(c, err)
	}
	err := o.RequireWorkspaces()
	if err != nil {
		return o.NativeAPIError(c, err)
	}
	if !workspacesession.ValidRunStatus(request.Status) {
		return o.NativeAPIError(c, hubapi.NativePublicInvalid("status names no run status"))
	}
	if request.Reason != "" && !o.ValidActionRunReason(request.Reason) {
		return o.NativeAPIError(c, hubapi.NativePublicInvalid("reason names no run reason"))
	}
	scope := hubscope.NativeRequestScope(c)
	ctx := c.Request().Context()
	exit, err := o.ActionRunExitCode(request.ExitCode)
	if err != nil {
		return o.NativeAPIError(c, err)
	}
	var record Record
	var existing postgres.ActionRun
	err = o.AccessRead(ctx, scope.Organization, func(reader tenantstore.Reader) error {
		var err error
		record, err = o.ReadRelayWorker(ctx, reader, scope, c.Param("workspace"), request.WorkspaceIdentity)
		if err != nil {
			return err
		}
		owner, ok := reader.(postgres.RelayActionReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		action, err := owner.ProjectAction(ctx, scope.Project, strings.TrimSpace(request.ActionID))
		if err != nil {
			return err
		}
		if strings.TrimSpace(request.RunID) == "" {
			if !action.Action.RunOnWorktreeCreation {
				return hubapi.NativePublicInvalid("Only a run-on-worktree-creation action may be reported without a run id")
			}
			return nil
		}
		existing, err = owner.ActionRun(ctx, postgres.ActionRunInput{OrganizationID: scope.Organization, ProjectID: scope.Project, ID: strings.TrimSpace(request.RunID)})
		if err != nil {
			return err
		}
		if existing.WorkspaceID != record.ID || existing.ActionID != action.Action.ID {
			return hubapi.NativeNotFound()
		}
		if !o.ForwardRunStatus(existing.Status, request.Status) {
			return hubapi.NativeStaleExecution("A run cannot move from " + existing.Status + " to " + request.Status)
		}
		if existing.ClaimedBy != "" && existing.ClaimedBy != workerActionRunClaimant(record) {
			return hubapi.NativeStaleExecution("The action run belongs to another executor")
		}
		return nil
	})
	if err != nil {
		return o.NativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	output := []byte(request.Output)
	truncated := request.Truncated
	if len(output) > workspacesession.MaxExecOutputBytes {
		output = output[:workspacesession.MaxExecOutputBytes]
		truncated = true
	}
	ref, err := o.PrepareRelayOutput(ctx, scope.Organization, output)
	if err != nil {
		return o.NativeAPIError(c, err)
	}
	var previousOutput *attachment.ArchiveReference
	if ref.Key != "" && existing.OutputObjectKey != nil && *existing.OutputObjectKey != ref.Key {
		previous, err := o.RelayArchiveReference(ctx, scope.Organization, attachment.ArchiveReference{Key: *existing.OutputObjectKey, SHA256: existing.OutputSha256, CompressedSize: existing.OutputBytes, UncompressedSize: existing.OutputContentBytes})
		if err != nil {
			return o.NativeAPIError(c, err)
		}
		previousOutput = &previous
	}
	var key *string
	if ref.Key != "" {
		key = &ref.Key
	}
	now := o.now()
	id := strings.TrimSpace(request.RunID)
	if id == "" {
		id = hubapi.NewID("actionrun")
	}
	var reported postgres.ActionRun
	err = o.AccessWrite(ctx, scope.Organization, func(writer tenantstore.Writer) error {
		if _, err := o.ReadRelayWorker(ctx, writer, scope, record.ID, request.WorkspaceIdentity); err != nil {
			return err
		}
		owner, ok := writer.(postgres.RelayActionWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		var command actionstore.Record
		if existing.ID == "" {
			if err := writer.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
			command, err = owner.ProjectAction(ctx, scope.Project, strings.TrimSpace(request.ActionID))
			if err != nil {
				return err
			}
			reported, err = owner.CreateReportedActionRun(ctx, postgres.CreateReportedActionRunInput{OrganizationID: scope.Organization, ProjectID: scope.Project, ID: id, ActionID: strings.TrimSpace(request.ActionID), WorkspaceID: record.ID, RunnerID: record.RunnerID, LeaseID: string(record.LeaseID), FencingToken: int64(record.FencingToken), Status: request.Status, ExitCode: exit, Reason: request.Reason, StartedAt: request.StartedAt, FinishedAt: request.FinishedAt, OutputObjectKey: key, OutputSha256: ref.SHA256, OutputBytes: ref.CompressedSize, OutputContentBytes: ref.UncompressedSize, Truncated: truncated, CreatedAt: now})
		} else {
			reported, err = owner.ApplyReportedActionRun(ctx, postgres.ApplyReportedActionRunInput{OrganizationID: scope.Organization, ProjectID: scope.Project, ID: existing.ID, ActionID: existing.ActionID, WorkspaceID: record.ID, RunnerID: record.RunnerID, LeaseID: string(record.LeaseID), FencingToken: int64(record.FencingToken), ExpectedRevision: existing.Revision, Status: request.Status, ExitCode: exit, Reason: request.Reason, StartedAt: request.StartedAt, FinishedAt: request.FinishedAt, OutputPresent: len(output) != 0, OutputObjectKey: key, OutputSha256: ref.SHA256, OutputBytes: ref.CompressedSize, OutputContentBytes: ref.UncompressedSize, Truncated: truncated, UpdatedAt: now})
		}
		if err != nil {
			return err
		}
		if existing.ID == "" {
			if command.Command.Key != reported.CommandObjectKey || command.Command.SHA256 != reported.CommandSha256 {
				return artifact.ErrIntegrity
			}
			if err := o.RetainRelayArchive(ctx, writer, scope.Organization, scope.Project, reported.ID, "project-action-run-command", command.Command, now); err != nil {
				return err
			}
		}
		if ref.Key != "" {
			if err := o.RetainRelayArchive(ctx, writer, scope.Organization, scope.Project, reported.ID, "project-action-run-output", ref, now); err != nil {
				return err
			}
			if previousOutput != nil {
				object, err := writer.ArchiveObjectByReference(ctx, *previousOutput)
				if err != nil {
					return err
				}
				if err := writer.RemoveArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: scope.Organization, ArchiveID: object.ID, Kind: "project-action-run-output", ID: actionstore.ArchiveOwnerID(scope.Project, reported.ID)}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		err = hubapi.NativeStaleExecution("The action run report is no longer current")
	}
	if err != nil {
		return o.NativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	result, err := o.ActionRunResource(ctx, reported, true)
	if err != nil {
		return o.NativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

// workerActionRunClaimant is what claimed_by holds for a run the runner
// started itself and reported through the worker endpoint. It names the runner
// rather than a stream because there is no stream: this run has no relay state
// at all, which is exactly why the workspace sweep exists for it.
func workerActionRunClaimant(record Record) string {
	return "runner:" + record.RunnerID
}

// workerRelayIdentity reads the workspace tuple from the upgrade's headers.
func CapabilityNames(value workspacesession.Capabilities) []string {
	names := make([]string, 0, 6)
	for _, name := range []string{workspacesession.CapabilityFiles, workspacesession.CapabilityDiff, workspacesession.CapabilityPreview, workspacesession.CapabilityTerminal, workspacesession.CapabilityExec, workspacesession.CapabilityGit} {
		if value.Has(name) {
			names = append(names, name)
		}
	}
	return names
}

func (o *Operations) workspaceWorkerRecord(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, id string, identity WorkerIdentity, now time.Time, bound bool) (postgres.WorkspaceSession, leasestore.Lease, error) {
	if workspacesession.ValidateID(id) != nil {
		return postgres.WorkspaceSession{}, leasestore.Lease{}, hubapi.NativeNotFound()
	}
	if strings.TrimSpace(string(identity.LeaseID)) == "" || identity.FencingToken <= 0 {
		return postgres.WorkspaceSession{}, leasestore.Lease{}, hubapi.NativePublicInvalid("A lease and fencing token are required")
	}
	if _, err := o.ReadRunnerAuthority(ctx, r, scope, string(identity.LeaseID), now); err != nil {
		return postgres.WorkspaceSession{}, leasestore.Lease{}, err
	}
	owner, ok := r.(postgres.WorkspaceReader)
	if !ok {
		return postgres.WorkspaceSession{}, leasestore.Lease{}, hubapi.WorkspaceUnavailable()
	}
	row, err := owner.WorkspaceSession(ctx, scope.Project, id)
	if err != nil {
		return row, leasestore.Lease{}, err
	}
	lease, err := r.Lease(ctx, string(identity.LeaseID))
	if err != nil {
		return row, lease, err
	}
	if lease.FencingToken != int64(identity.FencingToken) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) || lease.RenewedAt.After(now) {
		return row, lease, hubapi.NativeStaleExecution("The workspace lease is no longer current")
	}
	link, err := owner.WorkspaceDispatchItem(ctx, scope.Project, id)
	if err != nil {
		return row, lease, err
	}
	item, err := r.WorkItem(ctx, scope.Project, link.WorkItemID)
	if err != nil {
		return row, lease, err
	}
	if lease.OrganizationID != scope.Organization || lease.IssueID != item.ID || lease.MachineID != string(scope.Credential.Runner.MachineID) {
		return row, lease, hubapi.NativeStaleExecution("The lease does not hold this workspace's work item")
	}
	if bound && (row.RunnerID != scope.Credential.Runner.RunnerID || row.MachineID != lease.MachineID || row.LeaseID != lease.LeaseID || row.FencingToken != lease.FencingToken) {
		return row, lease, hubapi.NativeStaleExecution("The workspace belongs to another execution")
	}
	return row, lease, nil
}

func (o *Operations) mutateWorkspaceWorker(ctx context.Context, scope hubscope.NativeScope, id string, identity WorkerIdentity, prepare func(context.Context, tenantstore.Reader, postgres.WorkspaceSession) error, hydrate func(postgres.WorkspaceSession) error, change func(postgres.WorkspaceSession, time.Time) (postgres.WorkspaceSession, string, string, error), bound bool) (Record, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Record{}, err
		}
		now := o.now()
		var before, planned postgres.WorkspaceSession
		var target, reason string
		var linkID tracker.NativeWorkItemID
		sharedRun := false
		err := o.Repository().ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			var held leasestore.Lease
			before, held, err = o.workspaceWorkerRecord(ctx, r, scope, id, identity, now, bound)
			if err != nil {
				return err
			}
			planned, target, reason, err = change(before, now)
			if err != nil {
				return err
			}
			if workspacesession.Terminal(target) {
				workspaces, ok := r.(postgres.WorkspaceReader)
				if !ok {
					return errors.New("tenant reader does not support workspace reads")
				}
				link, err := workspaces.WorkspaceDispatchItem(ctx, scope.Project, id)
				if err != nil {
					return err
				}
				if link.ClosedAt == nil {
					linkID = link.WorkItemID
				}
				sharedRun, err = workspaceLeaseRunning(ctx, r, held)
				if err != nil {
					return err
				}
			}
			if prepare != nil {
				return prepare(ctx, r, before)
			}
			return nil
		})
		if err != nil {
			return Record{}, hubapi.ProjectRecordError(err)
		}
		if hydrate != nil {
			if err := hydrate(before); err != nil {
				return Record{}, err
			}
		}
		closure, err := o.PrepareIssueClose(ctx, scope, string(linkID), now)
		if err != nil {
			return Record{}, err
		}
		var cancellation PreparedCancellation
		if sharedRun {
			cancellation, err = o.PrepareCancellation(ctx, scope, string(identity.LeaseID), tracker.NativeLeaseMutation{FencingToken: identity.FencingToken, Reason: cmp.Or(reason, target)})
			if err != nil {
				return Record{}, hubapi.ProjectRecordError(err)
			}
		}
		var result postgres.WorkspaceSession
		err = o.Repository().WriteTenant(ctx, scope.Organization, corestore.Serving, func(writer tenantstore.Writer) error {
			if _, err := o.ReadRunnerAuthority(ctx, writer, scope, string(identity.LeaseID), o.now()); err != nil {
				return err
			}
			if err := writer.LockAccessOrganization(ctx); err != nil {
				return err
			}
			if err := writer.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
			if closure.move {
				if _, err := writer.LockWorkItem(ctx, scope.Project, closure.plan.Issue.WorkItemID); err != nil {
					return err
				}
			}
			locker, ok := writer.(interface {
				LockLease(context.Context, string) (leasestore.Lease, error)
			})
			if !ok {
				return hubapi.WorkspaceUnavailable()
			}
			if _, err := locker.LockLease(ctx, string(identity.LeaseID)); err != nil {
				return err
			}
			current, lease, err := o.workspaceWorkerRecord(ctx, writer, scope, id, identity, o.now(), bound)
			if err != nil {
				return err
			}
			if current.Revision != before.Revision {
				return corestore.ErrSerializationFailure
			}
			owner, ok := writer.(postgres.WorkspaceWriter)
			if !ok {
				return errors.New("tenant writer does not support workspace writes")
			}
			if linkID != "" {
				if err := closure.Write(ctx, writer); err != nil {
					return err
				}
				if err := owner.CloseWorkspaceItem(ctx, scope.Project, linkID, now); err != nil {
					return err
				}
			}
			row := planned
			row.Revision = current.Revision
			if target == workspacesession.StateReady {
				if row.OpenedAt == nil {
					row.OpenedAt = &now
				}
				row.RebindDeadline = nil
			}
			row, err = o.UpdateWorkspaceState(ctx, owner, row, current, target, reason, now)
			if err != nil {
				return err
			}

			if target == workspacesession.StateStarting && current.State != workspacesession.StateStarting {
				if err := owner.CreateWorkspaceOccupancy(ctx, postgres.WorkspaceOccupancyStart{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkspaceID: id, RunnerID: row.RunnerID, StartedAt: now}); err != nil {
					return err
				}
			}
			fence := leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}
			if workspacesession.Terminal(target) {
				if row.RunnerID != "" {
					if err := owner.EndWorkspaceOccupancy(ctx, postgres.WorkspaceOccupancyEnd{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkspaceID: id, RunnerID: row.RunnerID, EndedAt: &now}); err != nil {
						return err
					}
				}
				if !cancellation.HasAttempt {
					running, err := workspaceLeaseRunning(ctx, writer, lease)
					if err != nil {
						return err
					}
					if running {
						return corestore.ErrSerializationFailure
					}
				}
				if cancellation.Record != nil {
					if err := cancellation.Record(ctx, writer, scope, lease); err != nil {
						if errors.Is(err, leasestore.ErrAttemptConflict) {
							return corestore.ErrSerializationFailure
						}
						return err
					}
				}
				_, err = writer.Release(ctx, fence, unbindReleaseReason(reason, sessionRecord(row)))
			} else if bound {
				_, err = writer.Renew(ctx, fence, workspacesession.LeaseTTL)
			}
			result = row
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) {
			continue
		}
		return sessionRecord(result), hubapi.ProjectRecordError(err)
	}
}

func workspaceLeaseRunning(ctx context.Context, r tenantstore.Reader, lease leasestore.Lease) (bool, error) {
	attempt, err := r.LatestAttempt(ctx, lease.IssueID)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, corestore.ErrNotFound) || errors.Is(err, leasestore.ErrAttemptConflict) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return attempt.LeaseID == lease.LeaseID && attempt.FencingToken == lease.FencingToken && attempt.Status == "running", nil
}
