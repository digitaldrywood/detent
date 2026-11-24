package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/orgsettings"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type organizationLogo = orgsettings.Logo
type setOrganizationLogoRequest = orgsettings.LogoRequest
type renameOrganizationRequest = orgsettings.RenameRequest
type organizationNameSettings = orgsettings.NameSettings
type cloudModelSelectionRequest = orgsettings.ModelRequest

func (s *Service) organizationSettingsService(scope hubscope.NativeScope) *orgsettings.Service {
	deps := orgsettings.Deps{
		Repository: s.config.TenantRepository, AccessRepository: s.config.AccessRepository, Details: s.runDetailStore(), Now: s.config.Clock,
		RecordError: hubapi.ProjectRecordError, Unavailable: hubapi.ErrProjectParticipants,
		Recheck: func(ctx context.Context) error { return s.recheckProjectRecordMutation(ctx, scope) },
		Receipt: func(ctx context.Context, operation, key, hash string) (json.RawMessage, bool, error) {
			return s.changeRecordReceipt(ctx, scope, operation, key, hash)
		},
		Replay: func(ctx context.Context, operation, key string, input any) (json.RawMessage, bool, error) {
			return s.nativeCommandReplay(ctx, scope, operation, key, input)
		},
		Mutate: func(ctx context.Context, operation string, mutation tracker.Mutation, input, value any, apply func(tenantstore.Writer) error) (json.RawMessage, error) {
			return s.tenantReceiptMutation(ctx, scope, operation, mutation, input, value, apply)
		},
		RequireFeature: func(ctx context.Context, r tenantstore.Reader, feature string, now time.Time) error {
			return hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, r, feature, now))
		},
		CreateIcon: func(ctx context.Context, w tenantstore.Writer, metadata attachment.Metadata) error {
			return s.createIconAttachment(ctx, w, scope, metadata)
		},
		Icons:  s.config.Icons,
		Access: projectAccessFilter(scope.Credential, "", "", 200),
		Granted: func(ctx context.Context, r accessstore.Reader, grants []accessstore.ProjectAccess) ([]accessstore.ProjectAccess, error) {
			return s.grantedAccessProjects(ctx, r, scope.Organization, scope.Credential, grants)
		},
		Manager: hubscope.CanManageProjectSecrets(scope.Credential),
	}
	if s.hostedShared() {
		deps.Icons = s.config.Hosted.SharedEntry.Icons
	}
	if scope.Credential.Hosted != nil {
		audit := projectRecordActor(scope).Audit
		deps.Audit = &audit
	}
	return orgsettings.New(deps)
}

func (s *Service) hostedOrganizationName(c echo.Context) error {
	scope, err := s.organizationModelSelectionScope(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	ctx := c.Request().Context()
	scope.RequireHostedAdmin = false
	name, err := s.organizationSettingsService(scope).Name(ctx, s.authorization().Scope(scope))
	if err != nil {
		return s.nativeAPIError(c, hubapi.FromFeatureError(err))
	}
	return c.JSON(http.StatusOK, organizationNameSettings{Name: name})
}

func (s *Service) renameHostedOrganization(c echo.Context) error {
	scope, err := s.organizationModelSelectionScope(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	if !hubscope.CanManageProjectSecrets(scope.Credential) {
		return s.hostedJSONError(c, http.StatusForbidden, "Organization settings require owner or admin access")
	}
	var request renameOrganizationRequest
	if hubapi.DecodeAPIJSON(c, &request) != nil {
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, "Enter an organization name of at most 120 bytes")
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || len(request.Name) > 120 {
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, "Enter an organization name of at most 120 bytes")
	}
	scope.RequireHostedAdmin = true
	raw, err := s.renameOrganizationRecord(c.Request().Context(), scope, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func (s *Service) renameOrganizationRecord(ctx context.Context, scope hubscope.NativeScope, request renameOrganizationRequest) (json.RawMessage, error) {
	scope.RequireHostedAdmin = true
	raw, err := s.organizationSettingsService(scope).Rename(ctx, s.authorization().Scope(scope), request)
	return raw, hubapi.FromFeatureError(err)
}

func organizationVisibleReader(r tenantstore.Reader) (projectstore.OrganizationReader, error) {
	return orgsettings.New(orgsettings.Deps{Unavailable: hubapi.ErrProjectParticipants}).VisibleReader(r)
}

func organizationVisibleWriter(w tenantstore.Writer) (projectstore.OrganizationWriter, error) {
	return orgsettings.New(orgsettings.Deps{Unavailable: hubapi.ErrProjectParticipants}).VisibleWriter(w)
}

func (s *Service) readOrganizationRankRecord(ctx context.Context, scope hubscope.NativeScope) (organizationProjectRank, error) {
	value, err := s.organizationSettingsService(scope).ReadRank(ctx, s.authorization().Scope(scope))
	return value, hubapi.FromFeatureError(err)
}

func (s *Service) updateOrganizationRankRecord(ctx context.Context, scope hubscope.NativeScope, change projectRankChange) (any, error) {
	scope.RequireHostedAdmin = true
	value, err := s.organizationSettingsService(scope).UpdateRank(ctx, s.authorization().Scope(scope), change)
	return value, hubapi.FromFeatureError(err)
}

func (s *Service) readOrganizationModels(ctx context.Context, organization tracker.OrganizationID) (cloudModelSelection, error) {
	value, err := s.organizationSettingsService(hubscope.NativeScope{Organization: organization}).ReadModels(ctx, organization)
	return value, hubapi.FromFeatureError(err)
}

func (s *Service) organizationModelsFromRecord(ctx context.Context, organization tracker.OrganizationID, row policystore.OrganizationModels) (cloudModelSelection, error) {
	value, err := s.organizationSettingsService(hubscope.NativeScope{Organization: organization}).ModelsFromRecord(ctx, organization, row)
	return value, hubapi.FromFeatureError(err)
}

func (s *Service) updateOrganizationModelRecord(ctx context.Context, scope hubscope.NativeScope, operation string, request cloudModelSelectionRequest) (json.RawMessage, error) {
	raw, err := s.organizationSettingsService(scope).UpdateModels(ctx, s.authorization().Scope(scope), operation, request)
	return raw, hubapi.FromFeatureError(err)
}

func (s *Service) organizationLogoScope(c echo.Context) (hubscope.NativeScope, error) {
	if scope, ok := c.Get("native_scope").(hubscope.NativeScope); ok {
		return scope, nil
	}
	return s.organizationModelSelectionScope(c)
}

func (s *Service) getOrganizationLogo(c echo.Context) error {
	scope, err := s.organizationLogoScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	logo, err := s.readOrganizationLogoRecord(c.Request().Context(), scope.Organization)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, logo)
}

func (s *Service) setOrganizationLogo(c echo.Context) error {
	scope, err := s.organizationLogoScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var request setOrganizationLogoRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	raw, err := s.setOrganizationLogoCommand(c.Request().Context(), scope, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func organizationLogoOperationID(scope hubscope.NativeScope) string {
	return orgsettings.LogoOperationID(scope.Organization)
}

func (s *Service) setOrganizationLogoCommand(ctx context.Context, scope hubscope.NativeScope, request setOrganizationLogoRequest) (json.RawMessage, error) {
	scope.Project, scope.RequireHostedAdmin = "", true
	raw, err := s.organizationSettingsService(scope).SetLogo(ctx, s.authorization().Scope(scope), request)
	return raw, hubapi.FromFeatureError(err)
}

func (s *Service) readOrganizationLogoRecord(ctx context.Context, organization tracker.OrganizationID) (organizationLogo, error) {
	logo, err := s.organizationSettingsService(hubscope.NativeScope{Organization: organization}).ReadLogo(ctx, organization)
	return logo, hubapi.FromFeatureError(err)
}

func (s *Service) getCurrentOrganizationLogoAttachment(c echo.Context) error {
	scope, err := s.organizationLogoScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	logo, err := s.readOrganizationLogoRecord(c.Request().Context(), scope.Organization)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.organizationLogoAttachment(c, scope, logo.ImageKey)
}

func (s *Service) getOrganizationLogoAttachment(c echo.Context) error {
	scope, err := s.organizationLogoScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.organizationLogoAttachment(c, scope, c.Param("attachment"))
}

func (s *Service) organizationLogoAttachment(c echo.Context, scope hubscope.NativeScope, id string) error {
	metadata, err := s.organizationSettingsService(scope).LogoAttachment(c.Request().Context(), s.authorization().Scope(scope), id)
	if err != nil {
		return s.nativeAPIError(c, hubapi.FromFeatureError(err))
	}
	metadata.AuthorizedPrincipal = scope.Credential.ID
	return c.JSON(http.StatusOK, metadata)
}
