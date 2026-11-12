package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Durable storage for workspace sessions. Everything
// here takes a transaction from the caller: a transition, the event it emits
// and the occupancy row it opens or closes are one commit, because a client
// that saw the event and then read a different state would have no way to tell
// which one was true.

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
