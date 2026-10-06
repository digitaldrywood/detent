package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeOrganization struct {
	ID    tracker.OrganizationID `json:"organization_id"`
	Name  string                 `json:"name"`
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
	var serverID string
	if err := s.database.db.QueryRowContext(ctx, "SELECT id FROM hub_identity").Scan(&serverID); err != nil {
		return nativeCapabilitiesResponse{}, err
	}
	features := []string{"native_issues", "scoped_collaboration", "revision_conflicts", "idempotent_mutations", "scoped_runner_identity", "repository_policy", "change_requests", tracker.NativeExecutionCapability, tracker.NativeRuntimeEvidenceCapability, tracker.NativeAdmissionEvidenceCapability, tracker.NativeAdmissionObservationCapability, tracker.NativeProviderCapacityCapability, tracker.NativeCheckoutRepositoryCapability, tracker.NativeLocalChecksCapability, tracker.NativeRunnerCapacityCapability, tracker.NativeProjectConfigurationCapability, tracker.NativeRunnerUpdateCapability}
	if s.workspaces != nil {
		features = append(features, tracker.NativeWorkspaceCapability)
	}
	features = append(features, tracker.NativeRunnerSetupCapability)
	features = append(features, tracker.NativeDispatchPriorityCapability, tracker.NativeDispatchWaitCapability)
	return nativeCapabilitiesResponse{
		ServerID: serverID, Version: s.config.Version, MinimumRunnerVersion: minimumRunnerVersion(s.config.Version),
		ProtocolMajors: []int{1, 2}, EventSchemas: []int{1}, Features: features,
		MaxRequestBytes: maxAPIRequestBodyBytes, MaxPageSize: maxAPIPageLimit,
	}, nil
}

func (s *Service) nativeOrganizations(c echo.Context) error {
	rows, err := s.database.db.QueryContext(c.Request().Context(), "SELECT id, name, local FROM organizations ORDER BY id")
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer rows.Close()
	result := tracker.Page[nativeOrganization]{Items: []nativeOrganization{}}
	for rows.Next() {
		var organization nativeOrganization
		if err := rows.Scan(&organization.ID, &organization.Name, &organization.Local); err != nil {
			return s.nativeAPIError(c, err)
		}
		result.Items = append(result.Items, organization)
	}
	if err := rows.Err(); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) createNativeOrganization(c echo.Context) error {
	var request struct {
		Name string `json:"name"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
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
	var request createNativeProjectRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, nativeNotFound())
	}
	scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}
	c.Set("native_scope", scope)
	return s.nativeMutation(c, request.Mutation, request, s.createNativeProjectOperation(request))
}

func (s *Service) createNativeProjectOperation(request createNativeProjectRequest) func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error) {
	return func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		if strings.TrimSpace(request.Name) == "" || len(request.Name) > 200 {
			return nil, nativeInvalid("Project name is required and limited to 200 bytes")
		}
		if err := validateNativeStates(request.States); err != nil {
			return nil, err
		}
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM organizations WHERE id = ?", scope.organization).Scan(&exists); err != nil {
			return nil, err
		}
		if exists != 1 {
			return nil, nativeNotFound()
		}
		project := tracker.NativeProject{ID: tracker.ProjectID(newNativeID("prj")), OrganizationID: scope.organization, Name: request.Name, Profile: "native", States: request.States, RequireDependencies: true}
		if request.RequireDependencies != nil {
			project.RequireDependencies = *request.RequireDependencies
		}
		states, err := marshalNative(project.States)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO projects (id, organization_id, name, profile, states_json, require_dependencies, created_at, github_repository_enabled, scheduling_rank) VALUES (?, ?, ?, 'native', ?, ?, ?, 0, (SELECT COALESCE(max(scheduling_rank),-1)+1 FROM projects WHERE organization_id=?))", project.ID, project.OrganizationID, project.Name, states, project.RequireDependencies, formatHubTime(now), project.OrganizationID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE organizations SET scheduling_revision = scheduling_revision + 1 WHERE id = ?", project.OrganizationID); err != nil {
			return nil, err
		}
		for _, state := range project.States {
			if _, err := tx.ExecContext(ctx, "INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)", project.ID, state.Name, state.Name, state.Terminal, state.Dispatchable, formatHubTime(now), formatHubTime(now)); err != nil {
				return nil, err
			}
		}
		return project, nil
	}
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
		return invalidAPIRequest(c, err)
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

func readNativeProject(ctx context.Context, query nativeQueryer, scope nativeScope) (tracker.NativeProject, error) {
	var project tracker.NativeProject
	var states string
	if err := query.QueryRowContext(ctx, "SELECT id, organization_id, name, profile, states_json, require_dependencies, workflow_markdown FROM projects WHERE organization_id = ? AND id = ?", scope.organization, scope.project).Scan(&project.ID, &project.OrganizationID, &project.Name, &project.Profile, &states, &project.RequireDependencies, &project.WorkflowMarkdown); err != nil {
		return project, err
	}
	if err := json.Unmarshal([]byte(states), &project.States); err != nil {
		return project, err
	}
	selection, err := readCloudModelSelection(ctx, query, scope)
	if err != nil {
		return project, err
	}
	project.ModelSelection, err = json.Marshal(selection.Effective)
	return project, err
}

func (s *Service) getNativeProject(c echo.Context) error {
	project, err := readNativeProject(c.Request().Context(), s.database.db, nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, project)
}

func updateNativeProjectStates(ctx context.Context, tx *sql.Tx, scope nativeScope, states []tracker.NativeState, now time.Time) error {
	var source string
	if err := tx.QueryRowContext(ctx, "SELECT workflow_source FROM projects WHERE organization_id=? AND id=?", scope.organization, scope.project).Scan(&source); err != nil {
		return err
	}
	if source != "" {
		return nativeWorkflowInvalid("Workflow is controlled by the repository; edit its definition and approve the new repository policy")
	}
	return applyNativeProjectStates(ctx, tx, scope, states, now)
}

func applyNativeProjectStates(ctx context.Context, tx *sql.Tx, scope nativeScope, states []tracker.NativeState, now time.Time) error {
	project, err := readNativeProject(ctx, tx, scope)
	if err != nil {
		return err
	}
	if project.Profile != "native" {
		return nativeInvalid("Compatibility project workflow is externally owned")
	}
	if err := validateNativeStates(states); err != nil {
		return err
	}
	encoded, err := marshalNative(states)
	if err != nil {
		return err
	}
	var occupied int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM issues i JOIN workflow_states w ON w.id=i.workflow_state_id
WHERE i.project_id=? AND NOT EXISTS (SELECT 1 FROM json_each(?) s WHERE json_extract(s.value,'$.name')=w.detent_state)`, scope.project, encoded).Scan(&occupied); err != nil {
		return err
	}
	if occupied != 0 {
		return nativeWorkflowInvalid("Workflow states used by work items cannot be removed")
	}
	for _, state := range states {
		if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(project_id,source_name) DO UPDATE SET terminal=excluded.terminal,dispatchable=excluded.dispatchable,updated_at=excluded.updated_at`, scope.project, state.Name, state.Name, state.Terminal, state.Dispatchable, formatHubTime(now), formatHubTime(now)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workflow_states WHERE project_id=?
AND NOT EXISTS (SELECT 1 FROM json_each(?) s WHERE json_extract(s.value,'$.name')=workflow_states.detent_state)`, scope.project, encoded); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE projects SET states_json=? WHERE organization_id=? AND id=?", encoded, scope.organization, scope.project)
	return err
}
