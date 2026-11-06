package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) RunnerLocalChecks(ctx context.Context, runnerID string, project tracker.ProjectID) (runnerauth.LocalChecks, error) {
	var checks runnerauth.LocalChecks
	var setupDeclared sql.NullBool
	var checkoutAt, configurationAt, doctorAt, providerAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT commit_name,commit_email,commit_signing,checkout_message,checkout_fix,runner_setup_declared,setup,checkout,configuration_message,configuration_fix,configuration,checkout_at,configuration_at,doctor_at,provider_at,doctor,provider,observed_at
		FROM runner_local_checks WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, s.organization, runnerID, project).
		Scan(&checks.CommitName, &checks.CommitEmail, &checks.CommitSigning, &checks.CheckoutMessage, &checks.CheckoutFix, &setupDeclared, &checks.Setup, &checks.Checkout, &checks.ConfigurationMessage, &checks.ConfigurationFix, &checks.Configuration, &checkoutAt, &configurationAt, &doctorAt, &providerAt, &checks.Doctor, &checks.Provider, &checks.ObservedAt)
	if err != nil {
		return runnerauth.LocalChecks{}, mapError(ctx, err)
	}
	if setupDeclared.Valid {
		value := setupDeclared.Bool
		checks.RunnerSetupDeclared = &value
	}
	checks.CheckoutAt = localCheckTime(checkoutAt)
	checks.ConfigurationAt = localCheckTime(configurationAt)
	checks.DoctorAt = localCheckTime(doctorAt)
	checks.ProviderAt = localCheckTime(providerAt)
	rows, err := s.db.QueryContext(ctx, `SELECT position,kind FROM runner_local_check_providers WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3 ORDER BY position`, s.organization, runnerID, project)
	if err != nil {
		return runnerauth.LocalChecks{}, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var position int
		var kind string
		if err := rows.Scan(&position, &kind); err != nil {
			return runnerauth.LocalChecks{}, mapError(ctx, err)
		}
		if position != len(checks.ProviderKinds) {
			return runnerauth.LocalChecks{}, corestore.ErrInvalidPage
		}
		checks.ProviderKinds = append(checks.ProviderKinds, kind)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return runnerauth.LocalChecks{}, err
	}
	if checks.Validate() != nil || checks.ObservedAt.IsZero() {
		return runnerauth.LocalChecks{}, corestore.ErrInvalidPage
	}
	return checks, nil
}

func (s *session) ReplaceRunnerLocalChecks(ctx context.Context, runnerID string, project tracker.ProjectID, checks runnerauth.LocalChecks) error {
	if runnerID == "" || project == "" || checks.Validate() != nil || checks.ObservedAt.IsZero() {
		return runnerstore.ErrInvalidRecord
	}
	if _, err := s.RunnerIdentity(ctx, runnerID); err != nil {
		return err
	}
	if _, err := s.ProjectRecord(ctx, project); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runner_local_checks
		(organization_id,runner_id,project_id,commit_name,commit_email,commit_signing,checkout_message,checkout_fix,runner_setup_declared,setup,checkout,configuration_message,configuration_fix,configuration,checkout_at,configuration_at,doctor_at,provider_at,doctor,provider,observed_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		ON CONFLICT(organization_id,runner_id,project_id) DO UPDATE SET commit_name=EXCLUDED.commit_name,commit_email=EXCLUDED.commit_email,commit_signing=EXCLUDED.commit_signing,checkout_message=EXCLUDED.checkout_message,checkout_fix=EXCLUDED.checkout_fix,runner_setup_declared=EXCLUDED.runner_setup_declared,setup=EXCLUDED.setup,checkout=EXCLUDED.checkout,configuration_message=EXCLUDED.configuration_message,configuration_fix=EXCLUDED.configuration_fix,configuration=EXCLUDED.configuration,checkout_at=EXCLUDED.checkout_at,configuration_at=EXCLUDED.configuration_at,doctor_at=EXCLUDED.doctor_at,provider_at=EXCLUDED.provider_at,doctor=EXCLUDED.doctor,provider=EXCLUDED.provider,observed_at=EXCLUDED.observed_at`, s.organization, runnerID, project, checks.CommitName, checks.CommitEmail, checks.CommitSigning, checks.CheckoutMessage, checks.CheckoutFix, boolValue(checks.RunnerSetupDeclared), checks.Setup, checks.Checkout, checks.ConfigurationMessage, checks.ConfigurationFix, checks.Configuration, localCheckTimeArg(checks.CheckoutAt), localCheckTimeArg(checks.ConfigurationAt), localCheckTimeArg(checks.DoctorAt), localCheckTimeArg(checks.ProviderAt), checks.Doctor, checks.Provider, checks.ObservedAt)
	if err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runner_local_check_providers WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, s.organization, runnerID, project); err != nil {
		return mapError(ctx, err)
	}
	for position, kind := range checks.ProviderKinds {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO runner_local_check_providers(organization_id,runner_id,project_id,position,kind) VALUES($1,$2,$3,$4,$5)`, s.organization, runnerID, project, position, kind); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

func localCheckTime(value sql.NullTime) time.Time {
	if value.Valid {
		return value.Time
	}
	return time.Time{}
}

func localCheckTimeArg(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func boolValue(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
}
