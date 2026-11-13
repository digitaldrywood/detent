package hubserver

import (
	"context"
	"database/sql"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func storeRunnerBoardProjection(ctx context.Context, tx *sql.Tx, scope hubscope.NativeScope, runner string, view runnerauth.ProjectConfiguration) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM runner_project_diagnostic_attempts WHERE organization_id=? AND runner_id=? AND project_id=?`, scope.Organization, runner, scope.Project); err != nil {
		return err
	}
	if view.Diagnostics != nil {
		for position, r := range view.Diagnostics.Records {
			if _, err := tx.ExecContext(ctx, `INSERT INTO runner_project_diagnostic_attempts(organization_id,runner_id,project_id,position,record_key,issue_id,local_attempt_id,native_attempt_id,generation,fence,stage,running,native_claim,provider_completed_at,host_operation,operation_pending,lease_renewed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, scope.Organization, runner, scope.Project, position, r.Key, r.IssueID, r.LocalAttemptID, r.NativeAttemptID, r.Generation, r.Fence, r.Stage, slices.Contains(r.Membership, "running"), slices.Contains(r.Membership, "native_claim"), projectionTime(r.ProviderCompletedAt), r.HostOperation, r.OperationPending, projectionTime(r.LeaseRenewedAt)); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE runner_project_observations SET board_projection_version=1 WHERE organization_id=? AND runner_id=? AND project_id=?`, scope.Organization, runner, scope.Project)
	return err
}

func projectionTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return hubapi.FormatHubTime(at)
}
