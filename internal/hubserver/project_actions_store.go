package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Durable storage for project actions and their runs (decisions section
// 18.12). Like the workspace store, every write takes a transaction from the
// caller: a run's status change and the action_run.<status> event it emits are
// one commit, because a client that saw the event and then read a different
// status would have no way to tell which one was true.

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
	// cannot both start a process (decisions section 18.12, migration 00032).
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

func scanAction(row interface{ Scan(...any) error }) (actionRecord, error) {
	var record actionRecord
	var created, updated string
	if err := row.Scan(&record.ID, &record.OrganizationID, &record.ProjectID, &record.Name,
		&record.Command, &record.Keybinding, &record.Icon, &record.PreviewURL, &record.OpenPreview,
		&record.RunOnWorktreeCreation, &record.CreatedBy, &record.Revision, &created, &updated); err != nil {
		return record, err
	}
	var err error
	if record.CreatedAt, err = parseTimeValue(created); err != nil {
		return record, fmt.Errorf("decode action %s created_at: %w", record.ID, err)
	}
	if record.UpdatedAt, err = parseTimeValue(updated); err != nil {
		return record, fmt.Errorf("decode action %s updated_at: %w", record.ID, err)
	}
	return record, nil
}

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

// readProjectAction reads one action inside a scope, so an action of another
// project is simply not found.
func readProjectAction(ctx context.Context, query nativeQueryer, scope nativeScope, id string) (actionRecord, error) {
	return scanAction(query.QueryRowContext(ctx, `SELECT `+actionColumns+` FROM project_actions
WHERE id = ? AND organization_id = ? AND project_id = ?`, id, scope.organization, scope.project))
}

// Authoring order, which is also the order the run-on-worktree-creation set
// runs in: an author who wants install before build writes install first, and
// nothing else in the contract would say so. It is project_actions_project_idx's
// own order, so both listings are index scans.
//
// Neither listing takes a cursor. MaxActionsPerProject bounds the set at 50 and
// the create endpoint refuses the fifty-first, so one page is always the whole
// catalogue -- the same reason listNativeLabels answers without one. A cursor
// here would be a parameter every client had to loop over to learn what it
// already had.
const projectActionOrder = ` ORDER BY created_at, id`

// readProjectActions lists a project's whole catalogue.
func readProjectActions(ctx context.Context, query nativeQueryer, organization tracker.OrganizationID, project tracker.ProjectID) ([]actionRecord, error) {
	return scanActions(query.QueryContext(ctx, `SELECT `+actionColumns+` FROM project_actions
WHERE organization_id = ? AND project_id = ?`+projectActionOrder, organization, project))
}

// readRunOnCreationActions lists the actions a fresh worktree runs. It is
// written out rather than folded into readProjectActions behind a flag: the two
// have different callers, different audiences -- one a person's menu, one the
// bind response -- and a reader of either should see the whole predicate.
func readRunOnCreationActions(ctx context.Context, query nativeQueryer, organization tracker.OrganizationID, project tracker.ProjectID) ([]actionRecord, error) {
	return scanActions(query.QueryContext(ctx, `SELECT `+actionColumns+` FROM project_actions
WHERE organization_id = ? AND project_id = ? AND run_on_worktree_creation = 1`+projectActionOrder,
		organization, project))
}

func scanActions(rows *sql.Rows, queryErr error) ([]actionRecord, error) {
	if queryErr != nil {
		return nil, fmt.Errorf("query project actions: %w", queryErr)
	}
	defer func() { _ = rows.Close() }()
	records := []actionRecord{}
	for rows.Next() {
		record, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project actions: %w", err)
	}
	return records, nil
}

// countProjectActions reports how many actions a project holds, which is what
// MaxActionsPerProject is applied to inside the create transaction.
func countProjectActions(ctx context.Context, tx *sql.Tx, scope nativeScope) (int, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM project_actions
WHERE organization_id = ? AND project_id = ?`, scope.organization, scope.project).Scan(&count); err != nil {
		return 0, fmt.Errorf("count project actions: %w", err)
	}
	return count, nil
}

// projectActionKeybindingOwner reports the action that already claims a chord
// in this project, ignoring one id so a PATCH that keeps its own chord is not
// a conflict with itself. An empty chord is never a conflict: it means the
// action has no shortcut, and any number of actions may have none.
func projectActionKeybindingOwner(ctx context.Context, tx *sql.Tx, scope nativeScope, keybinding, except string) (actionRecord, bool, error) {
	if keybinding == "" {
		return actionRecord{}, false, nil
	}
	record, err := scanAction(tx.QueryRowContext(ctx, `SELECT `+actionColumns+` FROM project_actions
WHERE organization_id = ? AND project_id = ? AND keybinding = ? AND id <> ?`,
		scope.organization, scope.project, keybinding, except))
	if errors.Is(err, sql.ErrNoRows) {
		return actionRecord{}, false, nil
	}
	if err != nil {
		return actionRecord{}, false, err
	}
	return record, true, nil
}

// updateProjectActionRow rewrites every mutable column and bumps the revision.
// The write is conditional on the revision the caller read, so two concurrent
// edits cannot both believe they applied: the loser writes nothing and is told
// to read again.
func updateProjectActionRow(ctx context.Context, tx *sql.Tx, record actionRecord, expected int64) error {
	result, err := tx.ExecContext(ctx, `UPDATE project_actions SET name = ?, command = ?, keybinding = ?,
 icon = ?, preview_url = ?, open_preview = ?, run_on_worktree_creation = ?, revision = ?, updated_at = ?
WHERE id = ? AND revision = ?`,
		record.Name, record.Command, record.Keybinding, record.Icon, record.PreviewURL,
		record.OpenPreview, record.RunOnWorktreeCreation, record.Revision,
		formatHubTime(record.UpdatedAt), record.ID, expected)
	if err != nil {
		return fmt.Errorf("update project action: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count project action update: %w", err)
	}
	if changed != 1 {
		return nativeConflict(tracker.Revision(expected))
	}
	return nil
}

// deleteProjectActionRow removes an action. Its runs go with it: the migration
// declares ON DELETE CASCADE, because a run of an action nobody can name any
// more is a row no surface could ever show.
func deleteProjectActionRow(ctx context.Context, tx *sql.Tx, scope nativeScope, id string) error {
	result, err := tx.ExecContext(ctx, `DELETE FROM project_actions
WHERE id = ? AND organization_id = ? AND project_id = ?`, id, scope.organization, scope.project)
	if err != nil {
		return fmt.Errorf("delete project action: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count project action delete: %w", err)
	}
	if changed != 1 {
		return nativeNotFound()
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

// readProjectActionRun reads one run of one action inside a scope.
func readProjectActionRun(ctx context.Context, query nativeQueryer, scope nativeScope, actionID, runID string) (actionRunRecord, error) {
	return scanActionRun(query.QueryRowContext(ctx, `SELECT `+actionRunColumns+` FROM project_action_runs
WHERE id = ? AND action_id = ? AND organization_id = ? AND project_id = ?`,
		runID, actionID, scope.organization, scope.project))
}

// readProjectActionRuns lists one action's runs, newest first. The filter and
// the order are project_action_runs_action_idx's own, so the page is an index
// scan; the scope columns are in the predicate as well, so a run of another
// project cannot be reached through an action id guessed from elsewhere.
func readProjectActionRuns(ctx context.Context, query nativeQueryer, scope nativeScope, actionID string, limit int) ([]actionRunRecord, error) {
	rows, err := query.QueryContext(ctx, `SELECT `+actionRunColumns+` FROM project_action_runs
WHERE action_id = ? AND organization_id = ? AND project_id = ?
ORDER BY created_at DESC, id DESC LIMIT ?`, actionID, scope.organization, scope.project, limit)
	if err != nil {
		return nil, fmt.Errorf("query project action runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := []actionRunRecord{}
	for rows.Next() {
		record, err := scanActionRun(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project action runs: %w", err)
	}
	return records, nil
}

// activeRunStatuses and terminalWorkspaceStates are spelt out for SQL rather
// than derived from workspacesession.TerminalRunStatus and Terminal, so the
// sweep's predicate and the contract it enforces stay the same sentence. The
// same choice openWorkspaceStates makes, for the same reason.
const (
	activeRunStatuses       = `('queued', 'running')`
	terminalWorkspaceStates = `('closed', 'failed')`
)

// actionRunSweepBatch bounds one sweep of orphaned runs.
//
// It is projectEventSweepBatch's number and its reason: a long-neglected hub
// must not turn one five-second tick into a transaction of unbounded length.
// Each row here costs a row write and an event rather than a delete, so the
// same count is a larger transaction than the event sweep's -- still far short
// of one a tick cannot finish, and what is left over is taken by the next tick.
const actionRunSweepBatch = 500

// boundWorkspaceStates are the states a workspace is actually serving a runner
// relay connection in. A run may only be handed out on a workspace in one of
// them, because a workspace no runner has bound has no worktree to run in.
const boundWorkspaceStates = `('ready', 'idle')`

// actionRunDispatchBatch bounds how many queued runs one tick hands out.
//
// It is small where actionRunSweepBatch is large, and the asymmetry is the
// point: failing a row costs a write, while dispatching one starts a process on
// somebody's machine. A hub catching up after an outage must not start five
// hundred commands in one tick; what is left over is taken by the next one.
const actionRunDispatchBatch = 20

// readDispatchableActionRuns lists the runs still queued past the grace period
// on a workspace a runner has bound, oldest first (decisions section 18.12).
//
// The grace period is the whole of the rule, which is why it is in the
// predicate rather than applied to the rows afterwards. A person who queued a
// run and then opened the exec channel for it is the executor the contract
// prefers, because only their stream shows the output as it is produced. The
// hub steps in for the run nobody came back for -- which is every run a
// headless caller makes, and is exactly the run the eighth dogfood run found
// sitting queued for three minutes and forty seconds.
//
// read_only is in the predicate for the same reason it is in the queue's own
// refusal: a workspace can become the one an attempt is editing after a run was
// queued on it, and a command must not write into a worktree a model is using.
func readDispatchableActionRuns(ctx context.Context, query nativeQueryer, before time.Time, limit int) ([]actionRunRecord, error) {
	rows, err := query.QueryContext(ctx, `SELECT `+actionRunColumns+`
FROM project_action_runs
WHERE status = 'queued' AND claimed_by = '' AND created_at <= ?
 AND workspace_id IN (SELECT id FROM workspace_sessions
  WHERE state IN `+boundWorkspaceStates+` AND read_only = 0)
ORDER BY created_at, id LIMIT ?`, formatHubTime(before), limit)
	if err != nil {
		return nil, fmt.Errorf("query dispatchable action runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := []actionRunRecord{}
	for rows.Next() {
		record, err := scanActionRun(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dispatchable action runs: %w", err)
	}
	return records, nil
}

// orphanedActionRun is one run to fail and the reason its workspace ended.
//
// The workspace's reason is carried rather than assumed, because the relay's
// own teardown reads the same column: if the sweep stamped a blanket reason
// the two recovery paths would disagree about why the very same run died, and
// which answer an operator saw would depend on which path got there first.
type orphanedActionRun struct {
	run             actionRunRecord
	workspaceReason string
}

// readOrphanedActionRuns lists runs still queued or running on a workspace
// that has reached a terminal state, oldest first, with that workspace's own
// end reason.
//
// The predicate is the workspace's state and never a run's age, which is what
// makes it exact rather than a guess: a terminal workspace holds no runner and
// no worktree, so no legitimate report can still arrive for one of its runs.
// A long `bun test` on a healthy workspace is indistinguishable from a stuck
// row by age alone, so an age heuristic here would fail live runs.
func readOrphanedActionRuns(ctx context.Context, tx *sql.Tx, limit int) ([]orphanedActionRun, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+actionRunColumns+`,
 (SELECT reason FROM workspace_sessions WHERE id = project_action_runs.workspace_id)
FROM project_action_runs
WHERE status IN `+activeRunStatuses+`
 AND workspace_id IN (SELECT id FROM workspace_sessions WHERE state IN `+terminalWorkspaceStates+`)
ORDER BY created_at, id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query orphaned action runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	orphaned := []orphanedActionRun{}
	for rows.Next() {
		var reason sql.NullString
		record, err := scanActionRun(trailingScanner{rows: rows, trailing: []any{&reason}})
		if err != nil {
			return nil, err
		}
		orphaned = append(orphaned, orphanedActionRun{run: record, workspaceReason: reason.String})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate orphaned action runs: %w", err)
	}
	return orphaned, nil
}

// trailingScanner binds the caller's own destinations after the ones a scan
// function supplies, so one scan function serves a plain read and one that
// selects an extra column beside it. It is prefixedScanner's mirror.
type trailingScanner struct {
	rows     *sql.Rows
	trailing []any
}

func (t trailingScanner) Scan(dest ...any) error {
	combined := make([]any, 0, len(dest)+len(t.trailing))
	combined = append(combined, dest...)
	combined = append(combined, t.trailing...)
	return t.rows.Scan(combined...)
}

// readProjectActionRunByID reads one run without a project scope, for the
// relay paths that resolve the project from the row itself -- a relay frame
// names a run id and nothing else.
func readProjectActionRunByID(ctx context.Context, query nativeQueryer, runID string) (actionRunRecord, error) {
	return scanActionRun(query.QueryRowContext(ctx, `SELECT `+actionRunColumns+`
FROM project_action_runs WHERE id = ?`, runID))
}

// readWorkspaceActionRun reads one run of a workspace, which is the read the
// worker report is fenced by: a runner may only move a run of the workspace it
// currently holds.
func readWorkspaceActionRun(ctx context.Context, tx *sql.Tx, workspaceID, runID string) (actionRunRecord, error) {
	return scanActionRun(tx.QueryRowContext(ctx, `SELECT `+actionRunColumns+` FROM project_action_runs
WHERE id = ? AND workspace_id = ?`, runID, workspaceID))
}

// updateProjectActionRunRow rewrites every mutable column and bumps the
// revision, conditional on the revision the caller read. The receipt is
// recomputed here rather than taken from the record, so no caller can store a
// row whose output_artifact and output_bytes disagree.
func updateProjectActionRunRow(ctx context.Context, tx *sql.Tx, record actionRunRecord, expected int64) error {
	result, err := tx.ExecContext(ctx, `UPDATE project_action_runs SET status = ?, exit_code = ?,
 reason = ?, started_at = ?, finished_at = ?, output = ?, output_bytes = ?, output_artifact = ?,
 truncated = ?, claimed_by = ?, revision = ?, updated_at = ?
WHERE id = ? AND revision = ?`,
		record.Status, nullInt(record.ExitCode), record.Reason, nullTime(record.StartedAt),
		nullTime(record.FinishedAt), record.Output, record.OutputBytes,
		actionRunOutputPath(record), record.Truncated, record.ClaimedBy, record.Revision,
		formatHubTime(record.UpdatedAt), record.ID, expected)
	if err != nil {
		return fmt.Errorf("update project action run: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count project action run update: %w", err)
	}
	if changed != 1 {
		return nativeConflict(tracker.Revision(expected))
	}
	return nil
}
