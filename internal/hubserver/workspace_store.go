package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Durable storage for workspace sessions (decisions section 18.1). Everything
// here takes a transaction from the caller: a transition, the event it emits
// and the occupancy row it opens or closes are one commit, because a client
// that saw the event and then read a different state would have no way to tell
// which one was true.

// workspaceRecord is one row of workspace_sessions.
type workspaceRecord struct {
	ID                 string
	OrganizationID     tracker.OrganizationID
	ProjectID          tracker.ProjectID
	SubjectWorkItemID  string
	AttemptID          string
	Ref                string
	HeadSHA            string
	RunnerID           string
	MachineID          tracker.MachineID
	MachineHostname    string
	WorktreePath       string
	LeaseID            tracker.LeaseID
	FencingToken       tracker.FencingToken
	State              string
	Reason             string
	Requires           []string
	Capabilities       *workspacesession.Capabilities
	Isolation          string
	Worktree           string
	ReadOnly           bool
	IdleTimeoutSeconds int
	ExpiresAt          time.Time
	RequestedExpiresAt time.Time
	OpenedAt           *time.Time
	LastActivityAt     *time.Time
	LastHeartbeatAt    *time.Time
	RebindDeadline     *time.Time
	CreatedBy          string
	Revision           int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// owner is the workspace owner tuple the worker endpoints and the relay fence
// against.
func (r workspaceRecord) owner() workspacesession.Owner {
	return workspacesession.Owner{
		WorkspaceID: r.ID, RunnerID: r.RunnerID, MachineID: string(r.MachineID),
		LeaseID: string(r.LeaseID), FencingToken: int64(r.FencingToken),
	}
}

// resource projects the row onto the wire shape. Relay sessions are attached
// by the caller, which is the only place that knows whether the reader is an
// owner or an admin.
func (r workspaceRecord) resource() workspacesession.Session {
	requires := r.Requires
	if requires == nil {
		requires = []string{}
	}
	return workspacesession.Session{
		ID: r.ID, OrganizationID: string(r.OrganizationID), ProjectID: string(r.ProjectID),
		WorkItemID: r.SubjectWorkItemID, AttemptID: r.AttemptID, Ref: r.Ref, HeadSHA: r.HeadSHA,
		RunnerID: r.RunnerID, MachineID: string(r.MachineID), MachineHostname: r.MachineHostname,
		WorktreePath: r.WorktreePath, State: r.State, Reason: r.Reason,
		Requires: requires, Capabilities: r.Capabilities, Isolation: r.Isolation, Worktree: r.Worktree,
		ReadOnly: r.ReadOnly, IdleTimeoutSeconds: r.IdleTimeoutSeconds, ExpiresAt: r.ExpiresAt,
		OpenedAt: r.OpenedAt, LastActivityAt: r.LastActivityAt, CreatedBy: r.CreatedBy,
		Revision: r.Revision, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

const workspaceColumns = `id, organization_id, project_id, subject_work_item_id, attempt_id, ref, head_sha,
 runner_id, machine_id, machine_hostname, worktree_path, lease_id, fencing_token, state, reason, requires_json,
 capabilities_json, isolation, worktree, read_only, idle_timeout_seconds, expires_at, requested_expires_at,
 opened_at, last_activity_at, last_heartbeat_at, rebind_deadline, created_by, revision, created_at, updated_at`

func scanWorkspace(row interface{ Scan(...any) error }) (workspaceRecord, error) {
	var record workspaceRecord
	var requires string
	var capabilities sql.NullString
	var expires, requestedExpires, created, updated string
	var opened, activity, heartbeat, rebind sql.NullString
	if err := row.Scan(&record.ID, &record.OrganizationID, &record.ProjectID, &record.SubjectWorkItemID,
		&record.AttemptID, &record.Ref, &record.HeadSHA, &record.RunnerID, &record.MachineID,
		&record.MachineHostname, &record.WorktreePath, &record.LeaseID,
		&record.FencingToken, &record.State, &record.Reason, &requires, &capabilities, &record.Isolation,
		&record.Worktree, &record.ReadOnly, &record.IdleTimeoutSeconds, &expires, &requestedExpires,
		&opened, &activity, &heartbeat, &rebind, &record.CreatedBy, &record.Revision, &created, &updated); err != nil {
		return record, err
	}
	if err := json.Unmarshal([]byte(requires), &record.Requires); err != nil {
		return record, fmt.Errorf("decode workspace %s requires: %w", record.ID, err)
	}
	if capabilities.Valid {
		var set workspacesession.Capabilities
		if err := json.Unmarshal([]byte(capabilities.String), &set); err != nil {
			return record, fmt.Errorf("decode workspace %s capabilities: %w", record.ID, err)
		}
		record.Capabilities = &set
	}
	var err error
	if record.ExpiresAt, err = parseTimeValue(expires); err != nil {
		return record, fmt.Errorf("decode workspace %s expires_at: %w", record.ID, err)
	}
	if record.RequestedExpiresAt, err = parseTimeValue(requestedExpires); err != nil {
		return record, fmt.Errorf("decode workspace %s requested_expires_at: %w", record.ID, err)
	}
	if record.CreatedAt, err = parseTimeValue(created); err != nil {
		return record, fmt.Errorf("decode workspace %s created_at: %w", record.ID, err)
	}
	if record.UpdatedAt, err = parseTimeValue(updated); err != nil {
		return record, fmt.Errorf("decode workspace %s updated_at: %w", record.ID, err)
	}
	for _, field := range []struct {
		value  sql.NullString
		target **time.Time
		name   string
	}{
		{opened, &record.OpenedAt, "opened_at"},
		{activity, &record.LastActivityAt, "last_activity_at"},
		{heartbeat, &record.LastHeartbeatAt, "last_heartbeat_at"},
		{rebind, &record.RebindDeadline, "rebind_deadline"},
	} {
		if !field.value.Valid {
			continue
		}
		parsed, err := parseTimeValue(field.value.String)
		if err != nil {
			return record, fmt.Errorf("decode workspace %s %s: %w", record.ID, field.name, err)
		}
		*field.target = &parsed
	}
	return record, nil
}

// nullTime encodes an optional timestamp for storage.
func nullTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatHubTime(*value)
}

// insertWorkspace writes a new workspace in its requested state.
func insertWorkspace(ctx context.Context, tx *sql.Tx, record workspaceRecord) error {
	requires, err := json.Marshal(record.Requires)
	if err != nil {
		return fmt.Errorf("encode workspace requires: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workspace_sessions (`+workspaceColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.OrganizationID, record.ProjectID, record.SubjectWorkItemID, record.AttemptID,
		record.Ref, record.HeadSHA, record.RunnerID, record.MachineID, record.MachineHostname,
		record.WorktreePath, record.LeaseID, record.FencingToken,
		record.State, record.Reason, string(requires), nil, record.Isolation, record.Worktree, record.ReadOnly,
		record.IdleTimeoutSeconds, formatHubTime(record.ExpiresAt), formatHubTime(record.RequestedExpiresAt),
		nullTime(record.OpenedAt), nullTime(record.LastActivityAt), nullTime(record.LastHeartbeatAt),
		nullTime(record.RebindDeadline), record.CreatedBy, record.Revision,
		formatHubTime(record.CreatedAt), formatHubTime(record.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert workspace: %w", err)
	}
	return nil
}

// updateWorkspace rewrites every mutable column and bumps the revision. The
// write is conditional on the revision the caller read, so two concurrent
// transitions cannot both believe they applied: the loser writes nothing and
// is told to read again.
func updateWorkspace(ctx context.Context, tx *sql.Tx, record workspaceRecord, expected int64) error {
	requires, err := json.Marshal(record.Requires)
	if err != nil {
		return fmt.Errorf("encode workspace requires: %w", err)
	}
	var capabilities any
	if record.Capabilities != nil {
		encoded, err := json.Marshal(record.Capabilities)
		if err != nil {
			return fmt.Errorf("encode workspace capabilities: %w", err)
		}
		capabilities = string(encoded)
	}
	result, err := tx.ExecContext(ctx, `UPDATE workspace_sessions SET attempt_id = ?, ref = ?, head_sha = ?,
 runner_id = ?, machine_id = ?, machine_hostname = ?, worktree_path = ?,
 lease_id = ?, fencing_token = ?, state = ?, reason = ?, requires_json = ?,
 capabilities_json = ?, isolation = ?, worktree = ?, read_only = ?, idle_timeout_seconds = ?, expires_at = ?,
 requested_expires_at = ?, opened_at = ?, last_activity_at = ?, last_heartbeat_at = ?, rebind_deadline = ?,
 revision = ?, updated_at = ?
WHERE id = ? AND revision = ?`,
		record.AttemptID, record.Ref, record.HeadSHA, record.RunnerID, record.MachineID,
		record.MachineHostname, record.WorktreePath, record.LeaseID,
		record.FencingToken, record.State, record.Reason, string(requires), capabilities, record.Isolation,
		record.Worktree, record.ReadOnly, record.IdleTimeoutSeconds, formatHubTime(record.ExpiresAt),
		formatHubTime(record.RequestedExpiresAt), nullTime(record.OpenedAt), nullTime(record.LastActivityAt),
		nullTime(record.LastHeartbeatAt), nullTime(record.RebindDeadline), record.Revision,
		formatHubTime(record.UpdatedAt), record.ID, expected)
	if err != nil {
		return fmt.Errorf("update workspace: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count workspace update: %w", err)
	}
	if changed != 1 {
		return nativeConflict(tracker.Revision(expected))
	}
	return nil
}

// readWorkspaceRow reads one workspace by id inside a scope. It is the raw
// read: the caller applies the issue's read rule on top.
func readWorkspaceRow(ctx context.Context, query nativeQueryer, scope nativeScope, id string) (workspaceRecord, error) {
	return scanWorkspace(query.QueryRowContext(ctx, `SELECT `+workspaceColumns+` FROM workspace_sessions
WHERE id = ? AND organization_id = ? AND project_id = ?`, id, scope.organization, scope.project))
}

// readWorkspaceByID reads one workspace without a project scope, for the
// worker and relay paths that resolve the project from the row itself.
func readWorkspaceByID(ctx context.Context, query nativeQueryer, id string) (workspaceRecord, error) {
	return scanWorkspace(query.QueryRowContext(ctx, `SELECT `+workspaceColumns+` FROM workspace_sessions WHERE id = ?`, id))
}

// listWorkspaceRows reads the workspaces of a project, newest first, optionally
// filtered by subject work item and state.
func listWorkspaceRows(ctx context.Context, query nativeQueryer, scope nativeScope, item, state string, limit int) ([]workspaceRecord, error) {
	statement := `SELECT ` + workspaceColumns + ` FROM workspace_sessions
WHERE organization_id = ? AND project_id = ?`
	args := []any{scope.organization, scope.project}
	if item != "" {
		statement += ` AND subject_work_item_id = ?`
		args = append(args, item)
	}
	if state != "" {
		statement += ` AND state = ?`
		args = append(args, state)
	}
	statement += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := query.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query workspaces: %w", err)
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
		return nil, fmt.Errorf("iterate workspaces: %w", err)
	}
	return records, nil
}

// openWorkspaceStates is the state list an "open" workspace holds. It is spelt
// out for SQL rather than derived from workspacesession.Open so the predicate
// and the index's partial predicate stay the same sentence.
const openWorkspaceStates = `('requested', 'starting', 'ready', 'idle', 'unreachable', 'closing')`

// readOpenWorkspaceForAttempt reports the workspace already open on an attempt,
// which is what answers a second request with 409 workspace_exists.
func readOpenWorkspaceForAttempt(ctx context.Context, tx *sql.Tx, scope nativeScope, attemptID string) (workspaceRecord, bool, error) {
	record, err := scanWorkspace(tx.QueryRowContext(ctx, `SELECT `+workspaceColumns+` FROM workspace_sessions
WHERE organization_id = ? AND project_id = ? AND attempt_id = ? AND state IN `+openWorkspaceStates,
		scope.organization, scope.project, attemptID))
	if errors.Is(err, sql.ErrNoRows) {
		return workspaceRecord{}, false, nil
	}
	if err != nil {
		return workspaceRecord{}, false, err
	}
	return record, true, nil
}

// countOpenWorkspaces reports the organization's open workspaces and how many
// of them one actor holds, in one read: the two limits of section 18.1 are
// checked together so a request cannot pass one and race past the other.
func countOpenWorkspaces(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, actor string) (total, mine int, err error) {
	row := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(CASE WHEN created_by = ? THEN 1 ELSE 0 END), 0)
FROM workspace_sessions WHERE organization_id = ? AND state IN `+openWorkspaceStates, actor, organization)
	if err := row.Scan(&total, &mine); err != nil {
		return 0, 0, fmt.Errorf("count open workspaces: %w", err)
	}
	return total, mine, nil
}

// workspaceItemRecord associates a workspace with the native issue that
// dispatches it.
type workspaceItemRecord struct {
	WorkItemID     string
	WorkspaceID    string
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	CreatedAt      time.Time
	ClosedAt       *time.Time
}

// insertWorkspaceItem records the association.
func insertWorkspaceItem(ctx context.Context, tx *sql.Tx, record workspaceItemRecord) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_items (work_item_id, workspace_id, organization_id, project_id, created_at)
VALUES (?, ?, ?, ?, ?)`, record.WorkItemID, record.WorkspaceID, record.OrganizationID, record.ProjectID,
		formatHubTime(record.CreatedAt)); err != nil {
		return fmt.Errorf("insert workspace item: %w", err)
	}
	return nil
}

// closeWorkspaceItem marks the association closed, which is what returns the
// issue to ordinary history and lets the claim gate stop offering it.
func closeWorkspaceItem(ctx context.Context, tx *sql.Tx, workspaceID string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE workspace_items SET closed_at = ?
WHERE workspace_id = ? AND closed_at IS NULL`, formatHubTime(now), workspaceID); err != nil {
		return fmt.Errorf("close workspace item: %w", err)
	}
	return nil
}

// readWorkspaceItem resolves the workspace a work item carries.
func readWorkspaceItem(ctx context.Context, query nativeQueryer, item string) (workspaceItemRecord, bool, error) {
	return scanWorkspaceItem(query.QueryRowContext(ctx, workspaceItemQuery+` WHERE work_item_id = ?`, item))
}

// readWorkspaceItemByWorkspace resolves the dispatch issue a workspace was
// given, which is the direction the close path reads it in.
func readWorkspaceItemByWorkspace(ctx context.Context, query nativeQueryer, workspace string) (workspaceItemRecord, bool, error) {
	return scanWorkspaceItem(query.QueryRowContext(ctx, workspaceItemQuery+` WHERE workspace_id = ?`, workspace))
}

// workspaceItemQuery selects one association row, without its filter.
const workspaceItemQuery = `SELECT work_item_id, workspace_id, organization_id, project_id, created_at, closed_at
FROM workspace_items`

func scanWorkspaceItem(row *sql.Row) (workspaceItemRecord, bool, error) {
	var record workspaceItemRecord
	var created string
	var closed sql.NullString
	err := row.Scan(&record.WorkItemID, &record.WorkspaceID,
		&record.OrganizationID, &record.ProjectID, &created, &closed)
	if errors.Is(err, sql.ErrNoRows) {
		return record, false, nil
	}
	if err != nil {
		return record, false, fmt.Errorf("read workspace item: %w", err)
	}
	if record.CreatedAt, err = parseTimeValue(created); err != nil {
		return record, false, fmt.Errorf("decode workspace item created_at: %w", err)
	}
	if closed.Valid {
		parsed, err := parseTimeValue(closed.String)
		if err != nil {
			return record, false, fmt.Errorf("decode workspace item closed_at: %w", err)
		}
		record.ClosedAt = &parsed
	}
	return record, true, nil
}

// readWorkspaceByItem resolves the workspace an issue dispatches, open or not.
func readWorkspaceByItem(ctx context.Context, query nativeQueryer, item string) (workspaceRecord, bool, error) {
	record, err := scanWorkspace(query.QueryRowContext(ctx, `SELECT `+prefixedWorkspaceColumns+` FROM workspace_sessions w
JOIN workspace_items i ON i.workspace_id = w.id WHERE i.work_item_id = ?`, item))
	if errors.Is(err, sql.ErrNoRows) {
		return workspaceRecord{}, false, nil
	}
	if err != nil {
		return workspaceRecord{}, false, err
	}
	return record, true, nil
}

// prefixedWorkspaceColumns is workspaceColumns qualified for a join. It is
// written out rather than derived from workspaceColumns at run time: a query
// built by concatenating a computed string is indistinguishable, to a reader
// and to a scanner, from one built out of input, and the two lists have to be
// read side by side anyway to see that they match.
const prefixedWorkspaceColumns = `w.id, w.organization_id, w.project_id, w.subject_work_item_id, w.attempt_id, w.ref, w.head_sha,
 w.runner_id, w.machine_id, w.machine_hostname, w.worktree_path, w.lease_id, w.fencing_token, w.state, w.reason,
 w.requires_json, w.capabilities_json, w.isolation, w.worktree, w.read_only, w.idle_timeout_seconds, w.expires_at,
 w.requested_expires_at, w.opened_at, w.last_activity_at, w.last_heartbeat_at, w.rebind_deadline, w.created_by,
 w.revision, w.created_at, w.updated_at`

// workspaceIssue is one workspace item the claim loop has to decide about: the
// workspace it carries, and whether the association is still open.
type workspaceIssue struct {
	record workspaceRecord
	open   bool
}

// workspaceIssues lists the issues that are workspace items together with the
// workspace they carry, so the claim loop can gate them without a query per
// candidate. Closed items are listed too, because a closed workspace item is
// not ordinary work waiting to be claimed: its issue is a dispatch record for
// a workspace that has ended, and the workspace lane has nothing left to do
// with it either (decisions section 18.1).
func workspaceIssues(ctx context.Context, tx *sql.Tx) (map[tracker.WorkItemID]workspaceIssue, error) {
	rows, err := tx.QueryContext(ctx, `SELECT i.id, wi.closed_at IS NULL, `+prefixedWorkspaceColumns+`
FROM workspace_items wi
JOIN workspace_sessions w ON w.id = wi.workspace_id
JOIN issues i ON i.native_id = wi.work_item_id AND i.organization_id = wi.organization_id AND i.project_id = wi.project_id`)
	if err != nil {
		return nil, fmt.Errorf("list workspace items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := map[tracker.WorkItemID]workspaceIssue{}
	for rows.Next() {
		var id tracker.WorkItemID
		var open bool
		values := make([]any, 0, 2)
		values = append(values, &id, &open)
		record, err := scanWorkspaceWithPrefix(rows, values)
		if err != nil {
			return nil, err
		}
		items[id] = workspaceIssue{record: record, open: open}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workspace items: %w", err)
	}
	return items, nil
}

// scanWorkspaceWithPrefix scans a row whose leading columns the caller already
// bound, followed by the workspace columns.
func scanWorkspaceWithPrefix(rows *sql.Rows, leading []any) (workspaceRecord, error) {
	return scanWorkspace(prefixedScanner{rows: rows, leading: leading})
}

// prefixedScanner binds the caller's leading destinations before the ones
// scanWorkspace supplies, so one scan function serves both the plain read and
// the joined one.
type prefixedScanner struct {
	rows    *sql.Rows
	leading []any
}

func (p prefixedScanner) Scan(dest ...any) error {
	combined := make([]any, 0, len(p.leading)+len(dest))
	combined = append(combined, p.leading...)
	combined = append(combined, dest...)
	return p.rows.Scan(combined...)
}

// openWorkspaceOccupancy writes the runner occupancy row for a workspace that
// has just become starting.
func openWorkspaceOccupancy(ctx context.Context, tx *sql.Tx, record workspaceRecord, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO workspace_occupancy
 (workspace_id, runner_id, organization_id, project_id, started_at) VALUES (?, ?, ?, ?, ?)`,
		record.ID, record.RunnerID, record.OrganizationID, record.ProjectID, formatHubTime(now)); err != nil {
		return fmt.Errorf("open workspace occupancy: %w", err)
	}
	return nil
}

// closeWorkspaceOccupancy ends every open occupancy row of a workspace at
// endedAt. An unreachable workspace whose lease expires is ended at lease
// expiry rather than at the moment the hub noticed, so a crashed runner does
// not accrue time it was not serving.
func closeWorkspaceOccupancy(ctx context.Context, tx *sql.Tx, workspaceID string, endedAt time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE workspace_occupancy SET ended_at = ?
WHERE workspace_id = ? AND ended_at IS NULL`, formatHubTime(endedAt), workspaceID); err != nil {
		return fmt.Errorf("close workspace occupancy: %w", err)
	}
	return nil
}

// readWorkspaceRelaySessions reads a workspace's audit rows, newest first.
// They are served to the workspace's creator and to owners and admins only,
// which the caller decides: a row names who was inside a workspace, and that is
// not part of the issue's read rule.
func readWorkspaceRelaySessions(ctx context.Context, query nativeQueryer, workspaceID string) ([]workspacesession.RelaySession, error) {
	rows, err := query.QueryContext(ctx, `SELECT id, workspace_id, principal_id, subject, connection_id, channels_json,
 opened_at, closed_at, close_reason, bytes_in, bytes_out, support_reason
FROM workspace_relay_sessions WHERE workspace_id = ? ORDER BY opened_at DESC, id DESC LIMIT ?`,
		workspaceID, workspaceRelaySessionPage)
	if err != nil {
		return nil, fmt.Errorf("query workspace relay sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	sessions := []workspacesession.RelaySession{}
	for rows.Next() {
		var session workspacesession.RelaySession
		var channels, opened string
		var closed sql.NullString
		if err := rows.Scan(&session.ID, &session.WorkspaceID, &session.PrincipalID, &session.Subject,
			&session.ConnectionID, &channels, &opened, &closed, &session.CloseReason,
			&session.BytesIn, &session.BytesOut, &session.SupportReason); err != nil {
			return nil, fmt.Errorf("scan workspace relay session: %w", err)
		}
		if err := json.Unmarshal([]byte(channels), &session.Channels); err != nil {
			return nil, fmt.Errorf("decode workspace relay session channels: %w", err)
		}
		if session.OpenedAt, err = parseTimeValue(opened); err != nil {
			return nil, fmt.Errorf("decode workspace relay session opened_at: %w", err)
		}
		if closed.Valid {
			parsed, err := parseTimeValue(closed.String)
			if err != nil {
				return nil, fmt.Errorf("decode workspace relay session closed_at: %w", err)
			}
			session.ClosedAt = &parsed
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workspace relay sessions: %w", err)
	}
	return sessions, nil
}

// workspaceRelaySessionPage bounds the audit rows one resource carries. A
// workspace with more connections than this has a full record in the table; the
// resource is a summary, not the audit log itself.
const workspaceRelaySessionPage = 50
