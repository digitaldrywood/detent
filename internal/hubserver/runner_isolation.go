package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/isolation"
)

func updateRunnerIsolationReport(ctx context.Context, tx *sql.Tx, scope nativeScope, report isolation.Report) error {
	if report == nil {
		report = isolation.Report{}
	}
	if err := report.Validate(); err != nil {
		return nativeInvalid(err.Error())
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE runner_identities SET backend_isolation_json = ? WHERE id = ? AND organization_id = ? AND token_id = ?", string(encoded), scope.credential.Runner.RunnerID, scope.organization, scope.credential.ID)
	return requireRunnerUpdate(result, err)
}

func validateRunnerIsolation(ctx context.Context, tx nativeQueryer, scope nativeScope, now time.Time) error {
	runner, err := readRunner(ctx, tx, scope.organization, scope.credential.Runner.RunnerID, now)
	if err != nil {
		return err
	}
	var encoded string
	if err := tx.QueryRowContext(ctx, "SELECT backend_isolation_json FROM runner_identities WHERE id = ? AND organization_id = ?", runner.RunnerID, scope.organization).Scan(&encoded); err != nil {
		return err
	}
	var report isolation.Report
	if err := json.Unmarshal([]byte(encoded), &report); err != nil {
		return err
	}
	if !report.Supports(runner.IsolationTier) {
		return ErrNoClaimableWork
	}
	return nil
}
