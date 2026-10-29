package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func storeLocalCheckFacts(ctx context.Context, tx *sql.Tx, runner string, project tracker.ProjectID, checks runnerauth.LocalChecks) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO runner_local_checks(runner_id,project_id,commit_name,commit_email,commit_signing,checkout_message,checkout_fix,runner_setup_declared,setup,checkout,configuration,checkout_at,configuration_at,doctor_at,provider_at,doctor,provider,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(runner_id,project_id) DO UPDATE SET commit_name=excluded.commit_name,commit_email=excluded.commit_email,commit_signing=excluded.commit_signing,checkout_message=excluded.checkout_message,checkout_fix=excluded.checkout_fix,runner_setup_declared=excluded.runner_setup_declared,setup=excluded.setup,checkout=excluded.checkout,configuration=excluded.configuration,checkout_at=excluded.checkout_at,configuration_at=excluded.configuration_at,doctor_at=excluded.doctor_at,provider_at=excluded.provider_at,doctor=excluded.doctor,provider=excluded.provider,observed_at=excluded.observed_at`, runner, project, checks.CommitName, checks.CommitEmail, checks.CommitSigning, checks.CheckoutMessage, checks.CheckoutFix, checks.RunnerSetupDeclared, checks.Setup, checks.Checkout, checks.Configuration, projectionTime(checks.CheckoutAt), projectionTime(checks.ConfigurationAt), projectionTime(checks.DoctorAt), projectionTime(checks.ProviderAt), checks.Doctor, checks.Provider, projectionTime(checks.ObservedAt))
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM runner_local_check_providers WHERE runner_id=? AND project_id=?`, runner, project); err != nil {
		return err
	}
	for position, kind := range checks.ProviderKinds {
		if _, err := tx.ExecContext(ctx, `INSERT INTO runner_local_check_providers(runner_id,project_id,position,kind) VALUES(?,?,?,?)`, runner, project, position, kind); err != nil {
			return err
		}
	}
	return nil
}

func readLocalCheckFacts(ctx context.Context, q nativeQueryer, runner string, project tracker.ProjectID) (sql.NullString, error) {
	var checks runnerauth.LocalChecks
	var checkoutAt string
	var configurationAt string
	var doctorAt string
	var providerAt string
	var observedAt string
	err := q.QueryRowContext(ctx, `SELECT commit_name,commit_email,commit_signing,checkout_message,checkout_fix,runner_setup_declared,setup,checkout,configuration,checkout_at,configuration_at,doctor_at,provider_at,doctor,provider,observed_at FROM runner_local_checks WHERE runner_id=? AND project_id=?`, runner, project).Scan(&checks.CommitName, &checks.CommitEmail, &checks.CommitSigning, &checks.CheckoutMessage, &checks.CheckoutFix, &checks.RunnerSetupDeclared, &checks.Setup, &checks.Checkout, &checks.Configuration, &checkoutAt, &configurationAt, &doctorAt, &providerAt, &checks.Doctor, &checks.Provider, &observedAt)
	if errors.Is(err, sql.ErrNoRows) {
		var raw string
		if err := q.QueryRowContext(ctx, `SELECT local_checks_json FROM runner_identities WHERE id=?`, runner).Scan(&raw); err != nil {
			return sql.NullString{}, err
		}
		var entries map[tracker.ProjectID]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			return sql.NullString{}, err
		}
		value, ok := entries[project]
		return sql.NullString{String: string(value), Valid: ok && string(value) != "null"}, nil
	}
	if err != nil {
		return sql.NullString{}, err
	}
	if checkoutAt != "" {
		checks.CheckoutAt, err = parseTimeValue(checkoutAt)
		if err != nil {
			return sql.NullString{}, err
		}
	}
	if configurationAt != "" {
		checks.ConfigurationAt, err = parseTimeValue(configurationAt)
		if err != nil {
			return sql.NullString{}, err
		}
	}
	if doctorAt != "" {
		checks.DoctorAt, err = parseTimeValue(doctorAt)
		if err != nil {
			return sql.NullString{}, err
		}
	}
	if providerAt != "" {
		checks.ProviderAt, err = parseTimeValue(providerAt)
		if err != nil {
			return sql.NullString{}, err
		}
	}
	if observedAt != "" {
		checks.ObservedAt, err = parseTimeValue(observedAt)
		if err != nil {
			return sql.NullString{}, err
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT kind FROM runner_local_check_providers WHERE runner_id=? AND project_id=? ORDER BY position`, runner, project)
	if err != nil {
		return sql.NullString{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			return sql.NullString{}, err
		}
		checks.ProviderKinds = append(checks.ProviderKinds, kind)
	}
	if err := rows.Err(); err != nil {
		return sql.NullString{}, err
	}
	raw, err := json.Marshal(checks)
	return sql.NullString{String: string(raw), Valid: err == nil}, err
}

func storeEncodedLocalChecks(ctx context.Context, tx *sql.Tx, scope nativeScope, raw string) error {
	var checks runnerauth.LocalChecks
	if err := json.Unmarshal([]byte(raw), &checks); err != nil {
		return err
	}
	runner := scope.credential.Runner.RunnerID
	if err := storeLocalCheckFacts(ctx, tx, runner, scope.project, checks); err != nil {
		return err
	}
	var document string
	if err := tx.QueryRowContext(ctx, `SELECT local_checks_json FROM runner_identities WHERE id=? AND organization_id=?`, runner, scope.organization).Scan(&document); err != nil {
		return err
	}
	var entries map[tracker.ProjectID]json.RawMessage
	if err := json.Unmarshal([]byte(document), &entries); err != nil {
		return err
	}
	if entries == nil {
		entries = make(map[tracker.ProjectID]json.RawMessage)
	}
	entries[scope.project] = json.RawMessage(raw)
	encoded, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE runner_identities SET local_checks_json=? WHERE id=? AND organization_id=?`, string(encoded), runner, scope.organization)
	return err
}
