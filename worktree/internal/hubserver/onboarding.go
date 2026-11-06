package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) registerOnboardingRoutes(e *echo.Echo) {
	s.registerRunnerRepositoryRoutes(e)
	e.GET(nativeBase+"/onboarding/issue-intake", s.getGitHubBatch, s.requireNativeScope(apiScopeOperator, apiScopeAdmin))
	e.POST(nativeBase+"/onboarding/issue-intake", s.commandGitHubBatch, s.requireOnboardingAdmin())
	e.POST(nativeBase+"/onboarding/issue-intake/result", s.reportGitHubBatch, s.requireNativeScope(apiScopeWorker))
	read := s.requireNativeScope(apiScopeOperator, apiScopeWorker, apiScopeAdmin)
	write := s.requireNativeScope(apiScopeOperator, apiScopeAdmin)
	e.PUT(nativeBase+"/onboarding/integration", s.updateProjectIntegration, s.requireOnboardingAdmin())
	e.POST(nativeBase+"/onboarding/repository", s.bindNativeRepository, s.requireOnboardingAdmin())
	e.GET(nativeBase+"/onboarding", s.getOnboarding, read)
	e.PUT(nativeBase+"/onboarding", s.saveOnboarding, write)
	e.PUT(nativeBase+"/onboarding/policy", s.approveProjectPolicy, s.requireOnboardingAdmin())
	e.PUT(nativeBase+"/onboarding/artifact-services/:service", s.bindArtifactService, s.requireOnboardingAdmin())
}

func readOnboardingProgress(ctx context.Context, query nativeQueryer, scope nativeScope) (onboarding.Progress, error) {
	var progress onboarding.Progress
	var raw string
	err := query.QueryRowContext(ctx, "SELECT progress_json FROM project_onboarding WHERE organization_id=? AND project_id=?", scope.organization, scope.project).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return progress, nil
	}
	if err != nil {
		return progress, err
	}
	return progress, json.Unmarshal([]byte(raw), &progress)
}

func (s *Service) projectOnboarding(ctx context.Context, scope nativeScope) (onboarding.Project, error) {
	if scope.credential.Scope == apiScopeWorker {
		progress, err := s.readOnboardingProgressRecord(ctx, scope)
		return onboarding.Project{Progress: progress}, err
	}
	return s.projectOnboardingRecord(ctx, scope)
}

func (s *Service) getOnboarding(c echo.Context) error {
	result, err := s.projectOnboarding(c.Request().Context(), nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

type saveOnboardingRequest struct {
	tracker.Mutation
	Progress onboarding.Progress `json:"progress"`
}

func (s *Service) saveOnboarding(c echo.Context) error {
	var request saveOnboardingRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := request.Progress.Validate(); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	if s.config.TenantRepository != nil {
		return s.saveOnboardingProgressRecord(c, request)
	}
	return s.nativeMutation(c, request.Mutation, request, s.saveOnboardingOperation(request))
}

func (s *Service) saveOnboardingOperation(request saveOnboardingRequest) func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error) {
	return func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		if err := request.Progress.Validate(); err != nil {
			return nil, nativeInvalid(err.Error())
		}
		current, err := readOnboardingProgress(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if current.Revision != request.Progress.Revision {
			return nil, nativeConflict(current.Revision)
		}
		progress := request.Progress
		progress.Revision++
		progress.UpdatedAt = formatHubTime(now)
		raw, err := marshalNative(progress)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO project_onboarding(organization_id,project_id,revision,progress_json,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(organization_id,project_id) DO UPDATE SET revision=excluded.revision,progress_json=excluded.progress_json,updated_at=excluded.updated_at`, scope.organization, scope.project, progress.Revision, raw, progress.UpdatedAt)
		return progress, err
	}
}

func (s *Service) requireOnboardingAdmin() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return s.requireNativeScope(apiScopeOperator, apiScopeAdmin)(func(c echo.Context) error {
			credential := nativeRequestScope(c).credential
			if credential.Hosted != nil {
				if credential.HostedRole != "owner" && credential.HostedRole != "admin" {
					return s.nativeAPIError(c, nativeNotFound())
				}
			} else if credential.Scope != apiScopeAdmin {
				return s.nativeAPIError(c, nativeNotFound())
			}
			return next(c)
		})
	}
}
