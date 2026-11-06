package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
	if s.config.TenantRepository != nil {
		scope := nativeRequestScope(c)
		scope.requireHostedAdmin = true
		requestID := newNativeID("repos")
		return s.runnerRepositoryMutationTenant(c, scope, request.Mutation, request, map[string]string{"request_id": requestID}, func(ctx context.Context, w tenantstore.Writer, now time.Time) error {
			identity, err := w.RunnerIdentity(ctx, request.RunnerID)
			if err != nil || identity.OrganizationID != scope.organization || identity.RemovedAt != nil {
				return nativeNotFound()
			}
			return w.RequestRunnerRepositories(ctx, request.RunnerID, requestID)
		})
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
		requestID := payloadNativeID(ctx, "repos")
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
	if s.config.TenantRepository != nil {
		scope := nativeRequestScope(c)
		return s.runnerRepositoryMutationTenant(c, scope, request.Mutation, request, map[string]bool{"saved": true}, func(ctx context.Context, w tenantstore.Writer, now time.Time) error {
			identity, err := w.RunnerIdentityByCredential(ctx, scope.credential.ID)
			if err != nil {
				if errors.Is(err, corestore.ErrNotFound) {
					return nativeNotFound()
				}
				return err
			}
			credential, err := w.AccessCredential(ctx, scope.credential.ID)
			if err != nil {
				if errors.Is(err, corestore.ErrNotFound) {
					return nativeNotFound()
				}
				return err
			}
			if identity.OrganizationID != scope.organization || identity.RunnerID != scope.credential.Runner.RunnerID || identity.MachineID != scope.credential.Runner.MachineID || identity.RemovedAt != nil || identity.TokenID != credential.ID || !identity.ExpiresAt.After(now) || credential.OrganizationID != scope.organization || credential.Scope != string(apiScopeWorker) || credential.Kind != "service" || !credential.Valid(scope.credential.Hash, now) || !slices.Contains(identity.Operations, runnerauth.Heartbeat) {
				return nativeNotFound()
			}
			allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, now)
			if err != nil {
				return err
			}
			if !allowed {
				return nativeNotFound()
			}
			list, err := w.RunnerRepositoryList(ctx, identity.RunnerID)
			if err != nil {
				if errors.Is(err, corestore.ErrNotFound) {
					return nativePublicInvalid("Repository list request is no longer current")
				}
				return err
			}
			if !list.Pending || list.RequestID != request.RequestID {
				return nativePublicInvalid("Repository list request is no longer current")
			}
			listedAt := now
			list.Reason = request.Reason
			list.Pending = false
			list.ListedAt = &listedAt
			repositories := make([]runnerstore.Repository, 0, len(request.Repositories))
			for _, repository := range request.Repositories {
				repositories = append(repositories, runnerstore.Repository{OrganizationID: scope.organization, RunnerID: identity.RunnerID, Owner: repository.Owner, Name: repository.Name, Private: repository.Private, PushedAt: repository.PushedAt, ListedAt: listedAt})
			}
			if err := w.ReplaceRunnerRepositories(ctx, list, repositories); err != nil {
				if errors.Is(err, corestore.ErrRevisionConflict) {
					return nativePublicInvalid("Repository list request is no longer current")
				}
				return err
			}
			return nil
		})
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

func (s *Service) runnerRepositoryMutationTenant(c echo.Context, scope nativeScope, command tracker.Mutation, input, response any, mutate func(context.Context, tenantstore.Writer, time.Time) error) error {
	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return s.nativeAPIError(c, nativePublicInvalid("An idempotency key of at most 128 bytes is required"))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	hash, err := nativeCommandHash(input)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	content := eventstore.NewContent(s.runDetailStore())
	responseRaw, err := json.Marshal(response)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	preparedAt := s.config.now().UTC()
	preparedReceipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: preparedAt}, responseRaw)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	usageHash, err := nativeCommandHash([]string{string(scope.organization), scope.credential.ID, operation, command.IdempotencyKey})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var prior *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		now := s.config.now().UTC()
		if scope.credential.Runner.RunnerID != "" {
			if _, err := authorizeRunnerRepositoryWriter(ctx, w, scope, now); err != nil {
				return err
			}
		} else if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		receipt, err := w.Receipt(ctx, scope.credential.ID, operation, command.IdempotencyKey)
		if err == nil {
			if receipt.RequestHash != hash {
				return nativeConflict(0)
			}
			prior = &receipt
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := s.database.requireHostedFeatureRecord(ctx, w, hostedMutationFeature(c), now); err != nil {
			return err
		}
		if err := mutate(ctx, w, now); err != nil {
			return err
		}
		if err := s.applyRunnerTenantUsage(ctx, w, now, "runner_"+usageHash, false, false); err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, preparedReceipt)
		return err
	})
	if err == nil {
		if prior != nil {
			response, readErr := content.Receipt(ctx, *prior)
			if readErr != nil {
				return s.nativeAPIError(c, readErr)
			}
			return c.JSONBlob(http.StatusOK, response)
		}
		return c.JSONBlob(http.StatusOK, responseRaw)
	}
	if errors.Is(err, corestore.ErrUniqueViolation) || errors.Is(err, corestore.ErrSerializationFailure) {
		var replay *eventstore.Receipt
		readErr := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			now := s.config.now().UTC()
			if scope.credential.Runner.RunnerID != "" {
				if _, err := authorizeRunnerRepositoryWriter(ctx, w, scope, now); err != nil {
					return err
				}
			} else if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			receipt, err := w.Receipt(ctx, scope.credential.ID, operation, command.IdempotencyKey)
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if receipt.RequestHash != hash {
				return nativeConflict(0)
			}
			replay = &receipt
			return nil
		})
		if readErr != nil {
			return s.nativeAPIError(c, readErr)
		}
		if replay != nil {
			response, readErr := content.Receipt(ctx, *replay)
			if readErr != nil {
				return s.nativeAPIError(c, readErr)
			}
			return c.JSONBlob(http.StatusOK, response)
		}
	}
	return s.nativeAPIError(c, projectRecordError(err))
}

func (s *Service) registerRunnerRepositoryRoutes(e *echo.Echo) {
	e.POST(nativeBase+"/onboarding/repositories", s.requestRunnerRepositories, s.requireOnboardingAdmin())
	e.POST(nativeBase+"/onboarding/repositories/report", s.reportRunnerRepositories, s.requireNativeScope(apiScopeWorker))
}

func runnerRepositoryRoute(path string) bool {
	return path == nativeBase+"/onboarding/repositories" || path == nativeBase+"/onboarding/repositories/report"
}

func runnerRepositoryReportRoute(path string) bool {
	return path == nativeBase+"/onboarding/repositories/report"
}

func (s *Service) authorizeRunnerRepositoryProject(ctx context.Context, scope nativeScope) error {
	return s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := authorizeRunnerRepositoryReader(ctx, r, scope, s.config.now().UTC())
		return err
	})
}

func authorizeRunnerRepositoryWriter(ctx context.Context, w tenantstore.Writer, scope nativeScope, now time.Time) (runnerstore.Identity, error) {
	return authorizeRunnerRepositoryReader(ctx, w, scope, now)
}

func authorizeRunnerRepositoryReader(ctx context.Context, r tenantstore.Reader, scope nativeScope, now time.Time) (runnerstore.Identity, error) {
	identity, err := r.RunnerIdentityByCredential(ctx, scope.credential.ID)
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			return runnerstore.Identity{}, nativeNotFound()
		}
		return runnerstore.Identity{}, err
	}
	credential, err := r.AccessCredential(ctx, scope.credential.ID)
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			return runnerstore.Identity{}, nativeNotFound()
		}
		return runnerstore.Identity{}, err
	}
	if identity.OrganizationID != scope.organization || identity.RunnerID != scope.credential.Runner.RunnerID || identity.MachineID != scope.credential.Runner.MachineID || identity.RemovedAt != nil || identity.TokenID != credential.ID || !identity.ExpiresAt.After(now) || credential.OrganizationID != scope.organization || credential.Scope != string(apiScopeWorker) || credential.Kind != "service" || !credential.Valid(scope.credential.Hash, now) || !slices.Contains(identity.Operations, runnerauth.Heartbeat) {
		return runnerstore.Identity{}, nativeNotFound()
	}
	allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, now)
	if err != nil {
		return runnerstore.Identity{}, err
	}
	if !allowed {
		return runnerstore.Identity{}, nativeNotFound()
	}
	return identity, nil
}
