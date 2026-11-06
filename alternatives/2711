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

func (s *session) ProjectRunnerChecks(ctx context.Context, project tracker.ProjectID, limit int) (map[string]runnerauth.LocalChecks, error) {
	if project == "" || limit < 1 || limit > 10000 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT runner_id,commit_name,commit_email,commit_signing,checkout_message,checkout_fix,runner_setup_declared,setup,checkout,configuration_message,configuration_fix,configuration,checkout_at,configuration_at,doctor_at,provider_at,doctor,provider,observed_at FROM runner_local_checks WHERE organization_id=$1 AND project_id=$2 ORDER BY runner_id LIMIT $3`, s.organization, project, limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := map[string]runnerauth.LocalChecks{}
	for rows.Next() {
		var id string
		var checks runnerauth.LocalChecks
		var setup sql.NullBool
		var checkout, configuration, doctor, provider sql.NullTime
		if err := rows.Scan(&id, &checks.CommitName, &checks.CommitEmail, &checks.CommitSigning, &checks.CheckoutMessage, &checks.CheckoutFix, &setup, &checks.Setup, &checks.Checkout, &checks.ConfigurationMessage, &checks.ConfigurationFix, &checks.Configuration, &checkout, &configuration, &doctor, &provider, &checks.Doctor, &checks.Provider, &checks.ObservedAt); err != nil {
			return nil, mapError(ctx, err)
		}
		if setup.Valid {
			checks.RunnerSetupDeclared = &setup.Bool
		}
		checks.CheckoutAt, checks.ConfigurationAt, checks.DoctorAt, checks.ProviderAt = localCheckTime(checkout), localCheckTime(configuration), localCheckTime(doctor), localCheckTime(provider)
		result[id] = checks
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	if err := rows.Close(); err != nil {
		return nil, mapError(ctx, err)
	}
	if len(result) > limit {
		return nil, runnerstore.ErrLimitExceeded
	}
	rows, err = s.db.QueryContext(ctx, `SELECT p.runner_id,p.position,p.kind FROM runner_local_check_providers p JOIN (SELECT runner_id FROM runner_local_checks WHERE organization_id=$1 AND project_id=$2 ORDER BY runner_id LIMIT $3) c ON c.runner_id=p.runner_id WHERE p.organization_id=$1 AND p.project_id=$2 ORDER BY p.runner_id,p.position`, s.organization, project, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind string
		var position int
		if err := rows.Scan(&id, &position, &kind); err != nil {
			return nil, mapError(ctx, err)
		}
		checks, ok := result[id]
		if !ok || position != len(checks.ProviderKinds) {
			return nil, corestore.ErrInvalidPage
		}
		checks.ProviderKinds = append(checks.ProviderKinds, kind)
		result[id] = checks
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	for _, checks := range result {
		if checks.Validate() != nil || checks.ObservedAt.IsZero() {
			return nil, corestore.ErrInvalidPage
		}
	}
	return result, nil
}

func (s *session) RunnerRepositoryLists(ctx context.Context, limit int) (map[string]runnerauth.RepositoryList, error) {
	if limit < 1 || limit > 10000 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT l.runner_id,l.request_id,l.pending,l.reason,l.listed_at FROM runner_repository_lists l JOIN runner_identities r ON r.organization_id=l.organization_id AND r.runner_id=l.runner_id WHERE l.organization_id=$1 AND r.removed_at IS NULL ORDER BY l.runner_id LIMIT $2`, s.organization, limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := map[string]runnerauth.RepositoryList{}
	for rows.Next() {
		var id string
		var list runnerauth.RepositoryList
		var listed sql.NullTime
		if err := rows.Scan(&id, &list.RequestID, &list.Pending, &list.Reason, &listed); err != nil {
			return nil, mapError(ctx, err)
		}
		if listed.Valid {
			list.ListedAt = listed.Time.UTC().Format(time.RFC3339Nano)
		}
		list.Repositories = []runnerauth.Repository{}
		result[id] = list
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	if err := rows.Close(); err != nil {
		return nil, mapError(ctx, err)
	}
	if len(result) > limit {
		return nil, runnerstore.ErrLimitExceeded
	}
	rows, err = s.db.QueryContext(ctx, `SELECT p.runner_id,p.owner,p.name,p.private,p.pushed_at FROM runner_repositories p JOIN runner_identities r ON r.organization_id=p.organization_id AND r.runner_id=p.runner_id WHERE p.organization_id=$1 AND r.removed_at IS NULL ORDER BY p.runner_id,p.owner,p.name LIMIT $2`, s.organization, limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id string
		var repo runnerauth.Repository
		if err := rows.Scan(&id, &repo.Owner, &repo.Name, &repo.Private, &repo.PushedAt); err != nil {
			return nil, mapError(ctx, err)
		}
		count++
		if count > limit {
			return nil, runnerstore.ErrLimitExceeded
		}
		list, ok := result[id]
		if !ok {
			return nil, corestore.ErrInvalidPage
		}
		list.Repositories = append(list.Repositories, repo)
		result[id] = list
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	for _, list := range result {
		if list.Validate() != nil {
			return nil, corestore.ErrInvalidPage
		}
	}
	return result, nil
}
