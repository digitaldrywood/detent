package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func storeRunnerBoardProjection(ctx context.Context, tx *sql.Tx, scope nativeScope, runner string, view runnerauth.ProjectConfiguration) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM runner_project_diagnostic_attempts WHERE organization_id=? AND runner_id=? AND project_id=?`, scope.organization, runner, scope.project); err != nil {
		return err
	}
	if view.Diagnostics != nil {
		for position, r := range view.Diagnostics.Records {
			if _, err := tx.ExecContext(ctx, `INSERT INTO runner_project_diagnostic_attempts(organization_id,runner_id,project_id,position,record_key,issue_id,local_attempt_id,native_attempt_id,generation,fence,stage,running,native_claim,provider_completed_at,host_operation,operation_pending,lease_renewed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, scope.organization, runner, scope.project, position, r.Key, r.IssueID, r.LocalAttemptID, r.NativeAttemptID, r.Generation, r.Fence, r.Stage, slices.Contains(r.Membership, "running"), slices.Contains(r.Membership, "native_claim"), projectionTime(r.ProviderCompletedAt), r.HostOperation, r.OperationPending, projectionTime(r.LeaseRenewedAt)); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE runner_project_observations SET board_projection_version=1 WHERE organization_id=? AND runner_id=? AND project_id=?`, scope.organization, runner, scope.project)
	return err
}

func projectionTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return formatHubTime(at)
}

func readRunnerBoardProjection(ctx context.Context, q nativeQueryer, scope nativeScope, runner, item string) (runnerauth.ProjectConfiguration, error) {
	view := runnerauth.ProjectConfiguration{ProjectID: string(scope.project)}
	var observed, runtime sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT diagnostics_observed_at,runtime_observed_at FROM runner_project_observations WHERE organization_id=? AND runner_id=? AND project_id=?`, scope.organization, runner, scope.project).Scan(&observed, &runtime); err != nil {
		return view, err
	}
	if !observed.Valid {
		return view, nil
	}
	d := &runnerauth.ProjectDiagnostics{}
	var err error
	d.ObservedAt, err = parseTimeValue(observed.String)
	if err != nil {
		return view, err
	}
	if runtime.Valid {
		d.RuntimeObservedAt, err = parseTimeValue(runtime.String)
		if err != nil {
			return view, err
		}
	}
	view.Diagnostics = d
	rows, err := q.QueryContext(ctx, `SELECT record_key,issue_id,local_attempt_id,native_attempt_id,generation,fence,stage,running,native_claim,provider_completed_at,host_operation,operation_pending,lease_renewed_at FROM runner_project_diagnostic_attempts WHERE organization_id=? AND runner_id=? AND project_id=? AND issue_id=? ORDER BY position`, scope.organization, runner, scope.project, item)
	if err != nil {
		return view, err
	}
	defer rows.Close()
	for rows.Next() {
		var r runnerauth.DiagnosticAttempt
		var running, claim bool
		var completed, renewed string
		if err := rows.Scan(&r.Key, &r.IssueID, &r.LocalAttemptID, &r.NativeAttemptID, &r.Generation, &r.Fence, &r.Stage, &running, &claim, &completed, &r.HostOperation, &r.OperationPending, &renewed); err != nil {
			return view, err
		}
		if running {
			r.Membership = append(r.Membership, "running")
		}
		if claim {
			r.Membership = append(r.Membership, "native_claim")
		}
		if completed != "" {
			r.ProviderCompletedAt, err = parseTimeValue(completed)
			if err != nil {
				return view, err
			}
		}
		if renewed != "" {
			r.LeaseRenewedAt, err = parseTimeValue(renewed)
			if err != nil {
				return view, err
			}
		}
		d.Records = append(d.Records, r)
	}
	return view, rows.Err()
}

func (s *Service) convertRunnerBoardProjections(ctx context.Context) error {
	positions, err := s.storageProjectionPage(ctx, "runner-board", `SELECT o.runner_id FROM runner_project_observations o WHERE o.board_projection_version=0`, "o.organization_id,o.runner_id,o.project_id", storageConversionLimit(ctx))
	if err != nil {
		return err
	}
	type candidate struct {
		scope       nativeScope
		runner, raw string
	}
	var candidates []candidate
	for _, position := range positions {
		organization, ok := position.cursor[0].(string)
		if !ok {
			return errors.New("invalid runner board organization key")
		}
		project, ok := position.cursor[2].(string)
		if !ok {
			return errors.New("invalid runner board project key")
		}
		c := candidate{scope: nativeScope{organization: tracker.OrganizationID(organization), project: tracker.ProjectID(project)}, runner: position.id}
		if err := s.database.reader.QueryRowContext(ctx, "SELECT project_configuration_json FROM runner_identities WHERE organization_id=? AND id=?", c.scope.organization, c.runner).Scan(&c.raw); err != nil {
			return err
		}
		candidates = append(candidates, c)
	}
	for _, c := range candidates {
		if err := chargeStorageConversion(ctx, len(c.raw)); err != nil {
			return err
		}
		entry, err := projectConfigurationEntry(c.raw, string(c.scope.project))
		if err != nil {
			return err
		}
		var view runnerauth.ProjectConfiguration
		if entry.Valid {
			if err := json.Unmarshal([]byte(entry.String), &view); err != nil {
				return err
			}
		}
		if view.ProjectID != string(c.scope.project) {
			view.Diagnostics = nil
		}
		if err := s.convertRunnerBoardRow(ctx, c.scope, c.runner, c.raw, view); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) convertRunnerBoardRow(ctx context.Context, scope nativeScope, runner, raw string, view runnerauth.ProjectConfiguration) error {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ready int
	err = tx.QueryRowContext(ctx, `SELECT o.board_projection_version FROM runner_project_observations o JOIN runner_identities r ON r.id=o.runner_id AND r.organization_id=o.organization_id WHERE o.organization_id=? AND o.runner_id=? AND o.project_id=? AND r.project_configuration_json=?`, scope.organization, runner, scope.project, raw).Scan(&ready)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if ready == 1 {
		return nil
	}
	if err := storeRunnerBoardProjection(ctx, tx, scope, runner, view); err != nil {
		return err
	}
	return tx.Commit()
}
