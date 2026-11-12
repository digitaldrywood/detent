package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Durable storage for workspace sessions. Everything
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
