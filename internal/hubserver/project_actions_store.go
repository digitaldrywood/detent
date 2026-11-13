package hubserver

import (
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Durable storage for project actions and their runs.
// Like the workspace store, every write takes a transaction from the
// caller.

// actionRecord is one row of project_actions.
type actionRecord struct {
	ID                    string
	OrganizationID        tracker.OrganizationID
	ProjectID             tracker.ProjectID
	Name                  string
	Command               string
	Keybinding            string
	Icon                  string
	PreviewURL            string
	OpenPreview           bool
	RunOnWorktreeCreation bool
	CreatedBy             string
	Revision              int64
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// actionRunRecord is one row of project_action_runs. Output is the stored
// bytes rather than a handle, which is what migration 00030 explains.
type actionRunRecord struct {
	ID             string
	ActionID       string
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	WorkspaceID    string
	Command        string
	Status         string
	ExitCode       *int
	Reason         string
	StartedAt      *time.Time
	FinishedAt     *time.Time
	Output         string
	OutputBytes    int64
	OutputArtifact string
	Truncated      bool
	CreatedBy      string
	// ClaimedBy names the executor that took this run out of queued: a
	// person's relay connection, or the hub's own dispatch when it handed the
	// run to the runner holding the workspace. It is written in the same
	// transaction that moves the row, so two executors racing for one run
	// cannot both start a process (migration 00032).
	ClaimedBy string
	Revision  int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// resource projects the row onto the wire shape.
func (r actionRunRecord) resource() workspacesession.Run {
	return workspacesession.Run{
		ID: r.ID, ActionID: r.ActionID, WorkspaceID: r.WorkspaceID, Command: r.Command,
		Status: r.Status, ExitCode: r.ExitCode, Reason: r.Reason, StartedAt: r.StartedAt,
		FinishedAt: r.FinishedAt, OutputArtifact: r.OutputArtifact, OutputBytes: r.OutputBytes,
		Truncated: r.Truncated, CreatedBy: r.CreatedBy, Revision: r.Revision,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// actionRunOutputPath is what output_artifact holds: the path of the endpoint
// that serves the run's output, so the receipt and the route can never name
// different places. It stays empty until there is output to read, because a
// client handed a link expects bytes behind it.
func actionRunOutputPath(r actionRunRecord) string {
	if r.OutputBytes <= 0 {
		return ""
	}
	return "/api/v2/organizations/" + string(r.OrganizationID) + "/projects/" + string(r.ProjectID) +
		"/actions/" + r.ActionID + "/runs/" + r.ID + "/output"
}

const actionColumns = `id, organization_id, project_id, name, command, keybinding, icon, preview_url,
 open_preview, run_on_worktree_creation, created_by, revision, created_at, updated_at`

const actionRunColumns = `id, action_id, organization_id, project_id, workspace_id, command, status,
 exit_code, reason, started_at, finished_at, output, output_bytes, output_artifact, truncated,
 created_by, claimed_by, revision, created_at, updated_at`

// actionRunSweepBatch bounds one sweep of orphaned runs.
//
// A long-neglected hub must not turn one five-second tick into a transaction of
// unbounded length; what is left over is taken by the next tick.
const actionRunSweepBatch = 500

// actionRunDispatchBatch bounds how many queued runs one tick hands out.
//
// It is small where actionRunSweepBatch is large, and the asymmetry is the
// point: failing a row costs a write, while dispatching one starts a process on
// somebody's machine. A hub catching up after an outage must not start five
// hundred commands in one tick; what is left over is taken by the next one.
const actionRunDispatchBatch = 20
