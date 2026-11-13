package hubserver

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeOrganization struct {
	Logo  organizationLogo       `json:"logo"`
	ID    tracker.OrganizationID `json:"organization_id"`
	Name  string                 `json:"name"`
	Slug  string                 `json:"slug"`
	Local bool                   `json:"local"`
}

func (s *Service) nativeCapabilities(c echo.Context) error {
	response, err := s.readNativeCapabilities(c.Request().Context())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, response)
}

type nativeCapabilitiesResponse struct {
	MinimumRunnerVersion string   `json:"minimum_runner_version"`
	ServerID             string   `json:"server_id"`
	Version              string   `json:"version"`
	ProtocolMajors       []int    `json:"protocol_majors"`
	EventSchemas         []int    `json:"event_schema_versions"`
	Features             []string `json:"features"`
	MaxRequestBytes      int      `json:"max_request_bytes"`
	MaxPageSize          int      `json:"max_page_size"`
}

func (s *Service) readNativeCapabilities(ctx context.Context) (nativeCapabilitiesResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	var serverID string
	organization := s.database.hostedOrganization
	if organization == "" || s.config.AccessRepository == nil {
		return nativeCapabilitiesResponse{}, ErrHostedDatabaseBinding
	}
	if err := s.config.AccessRepository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		identity, err := reader.HubIdentity(ctx)
		if err == nil {
			serverID = identity.ID
		}
		return err
	}); err != nil {
		return nativeCapabilitiesResponse{}, err
	}
	features := []string{progresscheck.InvestigationCapability, progresscheck.RetrievedEvidenceCapability, "native_issues", "scoped_collaboration", "revision_conflicts", "idempotent_mutations", "scoped_runner_identity", "repository_policy", "change_requests", tracker.NativeExecutionCapability, tracker.NativeRuntimeEvidenceCapability, tracker.NativePreProviderFailureCapability, tracker.NativeAdmissionEvidenceCapability, tracker.NativeAdmissionObservationCapability, tracker.NativeProviderCapacityCapability, tracker.NativeCheckoutRepositoryCapability, tracker.NativeRunnerSkillsCapability, tracker.NativeLocalChecksCapability, tracker.NativeRunnerCapacityCapability, tracker.NativeProjectConfigurationCapability, tracker.NativeProjectDiagnosticsCapability, tracker.NativeRunnerUpdateCapability}
	if s.workspaces != nil {
		features = append(features, tracker.NativeWorkspaceCapability)
	}
	features = append(features, tracker.NativeRunnerCommitPolicyCapability, tracker.NativeRunnerSetupCapability, tracker.NativeProjectCheckoutCapability, tracker.NativeRunnerSetupDeclarationCapability)
	features = append(features, tracker.NativePreStartConversationBindingCapability, tracker.NativeDispatchPriorityCapability, tracker.NativeDispatchWaitCapability, tracker.NativeHeartbeatChangesCapability, tracker.NativeHostMetricsCapability, tracker.NativeLandingCapacityCapability)
	return nativeCapabilitiesResponse{
		ServerID: serverID, Version: s.config.Version, MinimumRunnerVersion: minimumRunnerVersion(s.config.Version),
		ProtocolMajors: []int{1, 2}, EventSchemas: []int{1}, Features: features,
		MaxRequestBytes: maxAPIRequestBodyBytes, MaxPageSize: maxAPIPageLimit,
	}, nil
}

func (s *Service) nativeOrganizations(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	organization := s.database.hostedOrganization
	if organization == "" || s.config.AccessRepository == nil {
		return s.nativeAPIError(c, ErrHostedDatabaseBinding)
	}
	result := tracker.Page[nativeOrganization]{Items: []nativeOrganization{}}
	err := s.config.AccessRepository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		record, err := reader.AccessOrganization(ctx)
		if err != nil {
			return err
		}
		logo, err := reader.OrganizationLogo(ctx)
		if err != nil {
			return fmt.Errorf("read organization logo: %w", err)
		}
		if logo.ImageKey != "" {
			logo.URL = "/organizations/" + string(record.OrganizationID) + "/api/v2/attachments/" + logo.ImageKey
		}
		result.Items = append(result.Items, nativeOrganization{ID: record.OrganizationID, Name: record.Name, Slug: record.Slug, Local: record.Local, Logo: logo})
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) createNativeOrganization(c echo.Context) error {
	var request struct {
		Name string `json:"name"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	result, err := s.createNativeOrganizationFor(c.Request().Context(), request.Name)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusCreated, result)
}

type createNativeProjectRequest struct {
	tracker.Mutation
	Name                string                `json:"name"`
	States              []tracker.NativeState `json:"states"`
	RequireDependencies *bool                 `json:"require_dependencies,omitempty"`
}

func (s *Service) createNativeProject(c echo.Context) error {
	if _, _, basic := c.Request().BasicAuth(); basic && s.config.Hosted == nil {
		return s.localAppCreateProject(c)
	}
	var request createNativeProjectRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, nativeNotFound())
	}
	scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}
	c.Set("native_scope", scope)
	return s.nativeMutation(c, request.Mutation, request, nil)
}

func validateNativeStates(states []tracker.NativeState) error {
	if err := policy.ValidateStates(states); err != nil {
		return nativeWorkflowInvalid(err.Error())
	}
	return nil
}

func nativeWorkflowInvalid(message string) error {
	return &nativeError{Code: "invalid_request", Message: message, status: http.StatusUnprocessableEntity, publicMessage: true}
}

func (s *Service) grantNativeToken(c echo.Context) error {
	var request struct {
		OrganizationID tracker.OrganizationID `json:"organization_id"`
		ProjectID      tracker.ProjectID      `json:"project_id"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := s.grantNativeTokenFor(c.Request().Context(), c.Param("id"), string(request.OrganizationID), string(request.ProjectID)); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

type nativeQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Service) getNativeProject(c echo.Context) error {
	return s.getProjectRecord(c)
}
