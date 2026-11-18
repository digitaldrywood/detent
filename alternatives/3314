package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ProjectRunnerChecksForProjects(ctx context.Context, projects []tracker.ProjectID, limit int) (map[tracker.ProjectID]map[string]runnerauth.LocalChecks, error) {
	if len(projects) < 1 || len(projects) > corestore.MaxPageSize || limit < 1 || limit > 10000 {
		return nil, corestore.ErrInvalidPage
	}
	ids := make([]string, len(projects))
	for i, id := range projects {
		if id == "" {
			return nil, corestore.ErrInvalidPage
		}
		ids[i] = string(id)
	}
	rows, err := s.db.Query(ctx, `SELECT project_id,runner_id,commit_name,commit_email,commit_signing,checkout_message,checkout_fix,runner_setup_declared,setup,checkout,configuration_message,configuration_fix,configuration,checkout_at,configuration_at,doctor_at,provider_at,doctor,provider,observed_at FROM runner_local_checks WHERE organization_id=$1 AND project_id=ANY($2) ORDER BY project_id,runner_id LIMIT $3`, s.organization, ids, limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := map[tracker.ProjectID]map[string]runnerauth.LocalChecks{}
	count := 0
	for rows.Next() {
		var id string
		var project tracker.ProjectID
		var checks runnerauth.LocalChecks
		var setup pgtype.Bool
		var checkout, configuration, doctor, provider pgtype.Timestamptz
		if err := rows.Scan(&project, &id, &checks.CommitName, &checks.CommitEmail, &checks.CommitSigning, &checks.CheckoutMessage, &checks.CheckoutFix, &setup, &checks.Setup, &checks.Checkout, &checks.ConfigurationMessage, &checks.ConfigurationFix, &checks.Configuration, &checkout, &configuration, &doctor, &provider, &checks.Doctor, &checks.Provider, &checks.ObservedAt); err != nil {
			return nil, mapError(ctx, err)
		}
		if setup.Valid {
			checks.RunnerSetupDeclared = &setup.Bool
		}
		checks.CheckoutAt, checks.ConfigurationAt, checks.DoctorAt, checks.ProviderAt = localCheckTime(checkout), localCheckTime(configuration), localCheckTime(doctor), localCheckTime(provider)
		if result[project] == nil {
			result[project] = map[string]runnerauth.LocalChecks{}
		}
		result[project][id] = checks
		count++
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	rows.Close()
	if count > limit {
		return nil, runnerstore.ErrLimitExceeded
	}
	rows, err = s.db.Query(ctx, `SELECT p.project_id,p.runner_id,p.position,p.kind FROM runner_local_check_providers p
 WHERE p.organization_id=$1 AND p.project_id=ANY($2) ORDER BY p.project_id,p.runner_id,p.position LIMIT $3`, s.organization, ids, limit*16+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	providerCount := 0
	for rows.Next() {
		var project tracker.ProjectID
		var id, kind string
		var position int
		if err := rows.Scan(&project, &id, &position, &kind); err != nil {
			return nil, mapError(ctx, err)
		}
		checks, ok := result[project][id]
		if !ok || position != len(checks.ProviderKinds) {
			return nil, corestore.ErrInvalidPage
		}
		checks.ProviderKinds = append(checks.ProviderKinds, kind)
		result[project][id] = checks
		providerCount++
		if providerCount > limit*16 {
			return nil, runnerstore.ErrLimitExceeded
		}
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	for _, group := range result {
		for _, checks := range group {
			if checks.Validate() != nil || checks.ObservedAt.IsZero() {
				return nil, corestore.ErrInvalidPage
			}
		}
	}
	return result, nil
}
