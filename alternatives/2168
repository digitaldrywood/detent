package hubserver

import (
	"context"
	"database/sql"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readRunnerRepositories(ctx context.Context, db nativeQueryer, runner string) (*runnerauth.RepositoryList, error) {
	list := &runnerauth.RepositoryList{Repositories: []runnerauth.Repository{}}
	rows, err := db.QueryContext(ctx, `SELECT l.request_id,l.pending,l.reason,l.listed_at,r.owner,r.name,r.private,r.pushed_at FROM runner_repository_lists l LEFT JOIN runner_repositories r ON r.runner_id=l.runner_id WHERE l.runner_id=? ORDER BY julianday(r.pushed_at) DESC,r.owner,r.name LIMIT ?`, runner, runnerauth.RepositoryListLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var owner, name, pushed sql.NullString
		var private sql.NullBool
		if err := rows.Scan(&list.RequestID, &list.Pending, &list.Reason, &list.ListedAt, &owner, &name, &private, &pushed); err != nil {
			return nil, err
		}
		if !owner.Valid {
			continue
		}
		repo := runnerauth.Repository{Owner: owner.String, Name: name.String, Private: private.Bool}
		if pushed.Valid {
			at, err := time.Parse(time.RFC3339Nano, pushed.String)
			if err != nil {
				return nil, err
			}
			repo.PushedAt = &at
		}
		list.Repositories = append(list.Repositories, repo)
	}
	return list, rows.Err()
}

func (s *Service) requestRunnerRepositories(c echo.Context) error {
	var request struct {
		tracker.Mutation
		RunnerID string `json:"runner_id"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	if _, err := s.readRunnerRouting(c.Request().Context(), scope, request.RunnerID); err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		scope.managedRunner = request.RunnerID
		if err := s.requireRunnerAdministration(ctx, tx, scope, now); err != nil {
			return nil, err
		}
		var id string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM runner_identities WHERE id=? AND organization_id=? AND removed_at IS NULL`, request.RunnerID, scope.organization).Scan(&id); err != nil {
			return nil, nativeNotFound()
		}
		requestID := newNativeID("repos")
		_, err := tx.ExecContext(ctx, `INSERT INTO runner_repository_lists(runner_id,request_id,pending) VALUES(?,?,1) ON CONFLICT(runner_id) DO UPDATE SET request_id=excluded.request_id,pending=1`, id, requestID)
		return map[string]string{"request_id": requestID}, err
	})
}

func (s *Service) reportRunnerRepositories(c echo.Context) error {
	var request struct {
		tracker.Mutation
		runnerauth.RepositoryList
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := request.Validate(); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		id := scope.credential.Runner.RunnerID
		if id == "" || scope.credential.Runner.OrganizationID != scope.organization {
			return nil, nativeNotFound()
		}
		result, err := tx.ExecContext(ctx, `UPDATE runner_repository_lists SET pending=0,reason=?,listed_at=? WHERE runner_id=? AND request_id=? AND EXISTS(SELECT 1 FROM runner_identities WHERE id=? AND organization_id=? AND removed_at IS NULL)`, request.Reason, formatHubTime(now), id, request.RequestID, id, scope.organization)
		if err != nil {
			return nil, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if count != 1 {
			return nil, nativePublicInvalid("Repository list request is no longer current")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM runner_repositories WHERE runner_id=?`, id); err != nil {
			return nil, err
		}
		for _, repo := range request.Repositories {
			var pushed *string
			if repo.PushedAt != nil {
				stamp := formatHubTime(*repo.PushedAt)
				pushed = &stamp
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO runner_repositories(runner_id,owner,name,private,pushed_at,listed_at) VALUES(?,?,?,?,?,?)`, id, repo.Owner, repo.Name, repo.Private, pushed, formatHubTime(now)); err != nil {
				return nil, err
			}
		}
		return map[string]bool{"saved": true}, nil
	})
}

func (s *Service) registerRunnerRepositoryRoutes(e *echo.Echo) {
	e.POST(nativeBase+"/onboarding/repositories", s.requestRunnerRepositories, s.requireOnboardingAdmin())
	e.POST(nativeBase+"/onboarding/repositories/report", s.reportRunnerRepositories, s.requireNativeScope(apiScopeWorker))
}
