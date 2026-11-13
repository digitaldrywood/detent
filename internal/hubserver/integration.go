package hubserver

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ProjectIntegration = projectstore.Integration

func (s *Service) githubRequestCounts(c echo.Context) error {
	counts := []GitHubRequestCount{}
	if s.config.GitHubRequestCounts != nil {
		counts = s.config.GitHubRequestCounts()
	}
	return c.JSON(http.StatusOK, counts)
}

func (s *Service) getCutoverReceipt(c echo.Context) error {
	result, err := s.readCutoverReceipt(c.Request().Context(), hubscope.NativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) projectIntegration(ctx context.Context, scope hubscope.NativeScope) (ProjectIntegration, error) {
	record, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return ProjectIntegration{}, err
	}
	result := projectIntegrationRecord(record)
	result.WorkflowMarkdown, err = s.projectRecordWorkflow(ctx, record)
	if err != nil {
		return result, err
	}
	if err := s.authorizeProjectIntegrationLive(ctx, scope); err != nil {
		return ProjectIntegration{}, err
	}
	return result, s.applyProjectIntegrationLive(ctx, scope, &result)
}

func (s *Service) authorizeProjectIntegrationLive(ctx context.Context, scope hubscope.NativeScope) error {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	return hubapi.ProjectRecordError(s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		return s.authorizeAccessScope(ctx, r, scope, false)
	}))
}

func (s *Service) applyProjectIntegrationLive(ctx context.Context, scope hubscope.NativeScope, result *ProjectIntegration) error {
	if result.CheckoutRepository != "" {
		verified, err := s.verifiedProjectCheckout(ctx, nil, scope, result.CheckoutRepository, s.config.Clock())
		if err != nil {
			return err
		}
		result.CheckoutVerified = verified
	}
	available := s.config.ReconcileBackend != nil
	result.GitHubTransportAvailable = &available
	repository := result.CheckoutRepository
	if repository == "" {
		repository = result.Repository
	}
	if available && result.Profile == "native" && repository != "" && s.config.GitHubAppInstallation != nil {
		app, err := s.config.GitHubAppInstallation(ctx, repository)
		if err != nil {
			return err
		}
		result.GitHubAppSlug, result.GitHubAppInstallURL = app.Slug, app.InstallURL
		result.GitHubAppInstalled = &app.Installed
	}
	return nil
}

type updateProjectIntegrationRequest struct {
	AgingEnabled              *bool           `json:"aging_enabled,omitempty"`
	DuplicateChallenge        *bool           `json:"duplicate_challenge,omitempty"`
	ArchiveCompletedAfterDays json.RawMessage `json:"archive_completed_after_days,omitempty"`
	ArchiveCancelledAfterDays json.RawMessage `json:"archive_cancelled_after_days,omitempty"`
	tracker.Mutation
	States            *[]tracker.NativeState `json:"states,omitempty"`
	WorkflowMarkdown  *string                `json:"workflow_markdown,omitempty"`
	ExpectedRevision  tracker.Revision       `json:"expected_revision,string"`
	Intake            string                 `json:"intake"`
	Projection        string                 `json:"projection"`
	RepositoryEnabled bool                   `json:"repository_enabled"`
}

func (s *Service) updateProjectIntegration(c echo.Context) error {
	var request updateProjectIntegrationRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	scope.RequireHostedAdmin = true
	c.Set("native_scope", scope)
	return s.nativeMutation(c, request.Mutation, request, nil)
}

func archivePeriod(raw json.RawMessage, current *int) (*int, error) {
	if len(raw) == 0 {
		return current, nil
	}
	var days *int
	if err := json.Unmarshal(raw, &days); err != nil {
		return nil, hubapi.NativePublicInvalid("Archive period must be 7, 14, 30, 60, 90 days or null")
	}
	if days != nil {
		switch *days {
		case 7, 14, 30, 60, 90:
		default:
			return nil, hubapi.NativePublicInvalid("Archive period must be 7, 14, 30, 60, 90 days or null")
		}
	}
	return days, nil
}

func (s *Service) registerIntegrationRoutes(e *echo.Echo) {
	if s.config.Hosted == nil {
		operator := s.requireAPIScope(hubapi.APIScopeOperator)
		e.GET(hostedOrganizationBase+"/model-selection", s.getOrganizationModelSelection, operator)
		e.PUT(hostedOrganizationBase+"/model-selection", s.updateOrganizationModelSelection, operator)
	}
	e.GET("/api/v2/github/requests", s.githubRequestCounts, s.requireInstanceAdmin())
	e.GET(hubapi.NativeBase+"/model-selection", s.getCloudModelSelection, s.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator))
	e.PUT(hubapi.NativeBase+"/model-selection", s.updateCloudModelSelection, s.requireNativeScope(hubapi.APIScopeOperator, hubapi.APIScopeAdmin))
	e.GET(hubapi.NativeBase+"/content-settings", s.getCloudContentSettings, s.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator))
	e.PUT(hubapi.NativeBase+"/content-settings", s.updateCloudContentSettings, s.requireNativeScope(hubapi.APIScopeOperator, hubapi.APIScopeAdmin))
	read := s.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator)
	operator := s.requireNativeScope(hubapi.APIScopeOperator)
	admin := s.requireNativeScope(hubapi.APIScopeAdmin)
	e.GET(hubapi.NativeBase+"/integration", s.getProjectIntegrationPage, s.requireAPIScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator))
	e.PUT(hubapi.NativeBase+"/integration", s.updateProjectIntegration, admin)
	e.POST(hubapi.NativeBase+"/integration/repository", s.bindNativeRepository, admin)
	e.POST(hubapi.NativeBase+"/integration/cutover", s.cutoverProject, admin)
	e.GET(hubapi.NativeBase+"/integration/cutover", s.getCutoverReceipt, read)
	e.POST(hubapi.NativeBase+"/imports", s.startGitHubImport, operator)
	e.GET(hubapi.NativeBase+"/imports/:import", s.getGitHubImport, read)
	e.POST(hubapi.NativeBase+"/imports/:import/advance", s.advanceGitHubImport, operator)
	e.GET(hubapi.NativeBase+"/imports/:import/records", s.listGitHubImportRecords, read)
	e.POST(hubapi.NativeBase+"/work-items/:item/projection", s.projectNativeSummary, operator)
}
