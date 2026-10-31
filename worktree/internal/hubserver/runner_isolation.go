package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func prepareRunnerIsolationReport(report isolation.Report) (string, error) {
	if report == nil {
		report = isolation.Report{}
	}
	if err := report.Validate(); err != nil {
		return "", nativeInvalid(err.Error())
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func updateRunnerIsolationReport(ctx context.Context, tx *sql.Tx, scope nativeScope, report isolation.Report) error {
	raw, err := prepareRunnerIsolationReport(report)
	if err != nil {
		return err
	}
	return storeEncodedRunnerIsolationReport(ctx, tx, scope, raw)
}

func storeEncodedRunnerIsolationReport(ctx context.Context, tx *sql.Tx, scope nativeScope, raw string) error {
	result, err := tx.ExecContext(ctx, "UPDATE runner_identities SET backend_isolation_json = ? WHERE id = ? AND organization_id = ? AND token_id = ?", raw, scope.credential.Runner.RunnerID, scope.organization, scope.credential.ID)
	return requireRunnerUpdate(result, err)
}

func validateReadRunnerIsolation(ctx context.Context, tx nativeQueryer, scope nativeScope, runner runnerauth.Runner) error {
	var encoded string
	if err := tx.QueryRowContext(ctx, "SELECT backend_isolation_json FROM runner_identities WHERE id = ? AND organization_id = ?", runner.RunnerID, scope.organization).Scan(&encoded); err != nil {
		return err
	}
	var report isolation.Report
	if err := json.Unmarshal([]byte(encoded), &report); err != nil {
		return err
	}
	if !report.SupportsOn(runner.IsolationTier, runner.OS) {
		return ErrNoClaimableWork
	}
	return nil
}
