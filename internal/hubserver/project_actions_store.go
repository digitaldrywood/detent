package hubserver

import (
	"context"
	"database/sql"
	"fmt"
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

// resource projects the row onto the wire shape.
func (r actionRecord) resource() workspacesession.Action {
	return workspacesession.Action{
		ID: r.ID, OrganizationID: string(r.OrganizationID), ProjectID: string(r.ProjectID),
		Name: r.Name, Command: r.Command, Keybinding: r.Keybinding, Icon: r.Icon,
		PreviewURL: r.PreviewURL, OpenPreview: r.OpenPreview,
		RunOnWorktreeCreation: r.RunOnWorktreeCreation, CreatedBy: r.CreatedBy,
		Revision: r.Revision, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
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

// insertProjectAction writes a new action at revision 1.
func insertProjectAction(ctx context.Context, tx *sql.Tx, record actionRecord) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO project_actions (`+actionColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.OrganizationID, record.ProjectID, record.Name, record.Command,
		record.Keybinding, record.Icon, record.PreviewURL, record.OpenPreview,
		record.RunOnWorktreeCreation, record.CreatedBy, record.Revision,
		formatHubTime(record.CreatedAt), formatHubTime(record.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert project action: %w", err)
	}
	return nil
}

const actionRunColumns = `id, action_id, organization_id, project_id, workspace_id, command, status,
 exit_code, reason, started_at, finished_at, output, output_bytes, output_artifact, truncated,
 created_by, claimed_by, revision, created_at, updated_at`

func scanActionRun(row interface{ Scan(...any) error }) (actionRunRecord, error) {
	var record actionRunRecord
	var exit sql.NullInt64
	var started, finished sql.NullString
	var created, updated string
	if err := row.Scan(&record.ID, &record.ActionID, &record.OrganizationID, &record.ProjectID,
		&record.WorkspaceID, &record.Command, &record.Status, &exit, &record.Reason, &started,
		&finished, &record.Output, &record.OutputBytes, &record.OutputArtifact, &record.Truncated,
		&record.CreatedBy, &record.ClaimedBy, &record.Revision, &created, &updated); err != nil {
		return record, err
	}
	if exit.Valid {
		code := int(exit.Int64)
		record.ExitCode = &code
	}
	var err error
	if record.CreatedAt, err = parseTimeValue(created); err != nil {
		return record, fmt.Errorf("decode action run %s created_at: %w", record.ID, err)
	}
	if record.UpdatedAt, err = parseTimeValue(updated); err != nil {
		return record, fmt.Errorf("decode action run %s updated_at: %w", record.ID, err)
	}
	for _, field := range []struct {
		value  sql.NullString
		target **time.Time
		name   string
	}{
		{started, &record.StartedAt, "started_at"},
		{finished, &record.FinishedAt, "finished_at"},
	} {
		if !field.value.Valid {
			continue
		}
		parsed, err := parseTimeValue(field.value.String)
		if err != nil {
			return record, fmt.Errorf("decode action run %s %s: %w", record.ID, field.name, err)
		}
		*field.target = &parsed
	}
	return record, nil
}

// nullInt encodes an optional exit code for storage. A run that never exited
// keeps it null, so "exit 0" can never be confused with "never ran".
func nullInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

// insertProjectActionRun writes a new run.
func insertProjectActionRun(ctx context.Context, tx *sql.Tx, record actionRunRecord) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO project_action_runs (`+actionRunColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.ActionID, record.OrganizationID, record.ProjectID, record.WorkspaceID,
		record.Command, record.Status, nullInt(record.ExitCode), record.Reason,
		nullTime(record.StartedAt), nullTime(record.FinishedAt), record.Output, record.OutputBytes,
		actionRunOutputPath(record), record.Truncated, record.CreatedBy, record.ClaimedBy,
		record.Revision, formatHubTime(record.CreatedAt), formatHubTime(record.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert project action run: %w", err)
	}
	return nil
}

const actionRunDispatchBatch = 20

// readProjectActionRunByID reads one run without a project scope, for the
// relay paths that resolve the project from the row itself -- a relay frame
// names a run id and nothing else.
func readProjectActionRunByID(ctx context.Context, query nativeQueryer, runID string) (actionRunRecord, error) {
	return scanActionRun(query.QueryRowContext(ctx, `SELECT `+actionRunColumns+`
FROM project_action_runs WHERE id = ?`, runID))
}
