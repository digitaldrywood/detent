package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeOrganization struct {
	ID    tracker.OrganizationID `json:"organization_id"`
	Name  string                 `json:"name"`
	Local bool                   `json:"local"`
}

func (s *Service) nativeCapabilities(c echo.Context) error {
	var serverID string
	if err := s.database.db.QueryRowContext(c.Request().Context(), "SELECT id FROM hub_identity").Scan(&serverID); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, struct {
		ServerID        string   `json:"server_id"`
		ProtocolMajors  []int    `json:"protocol_majors"`
		EventSchemas    []int    `json:"event_schema_versions"`
		Features        []string `json:"features"`
		MaxRequestBytes int      `json:"max_request_bytes"`
		MaxPageSize     int      `json:"max_page_size"`
	}{serverID, []int{1, 2}, []int{1}, []string{"native_issues", "scoped_collaboration", "revision_conflicts", "idempotent_mutations", "scoped_runner_identity", "repository_policy"}, maxAPIRequestBodyBytes, maxAPIPageLimit})
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
	if strings.TrimSpace(request.Name) == "" || len(request.Name) > 200 {
		return s.nativeAPIError(c, nativeInvalid("Organization name is required and limited to 200 bytes"))
	}
	result := nativeOrganization{ID: tracker.OrganizationID(newNativeID("org")), Name: request.Name}
	_, err := s.database.db.ExecContext(c.Request().Context(), "INSERT INTO organizations (id, name, created_at) VALUES (?, ?, ?)", result.ID, result.Name, formatHubTime(s.config.now()))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusCreated, result)
}

func (s *Service) createNativeProject(c echo.Context) error {
	var request struct {
		tracker.Mutation
		Name                string                `json:"name"`
		States              []tracker.NativeState `json:"states"`
		RequireDependencies *bool                 `json:"require_dependencies,omitempty"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, nativeNotFound())
	}
	scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}
	c.Set("native_scope", scope)
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
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
		if _, err := tx.ExecContext(ctx, "INSERT INTO projects (id, organization_id, name, profile, states_json, require_dependencies, created_at, github_repository_enabled) VALUES (?, ?, ?, 'native', ?, ?, ?, 0)", project.ID, project.OrganizationID, project.Name, states, project.RequireDependencies, formatHubTime(now)); err != nil {
			return nil, err
		}
		for _, state := range project.States {
			if _, err := tx.ExecContext(ctx, "INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)", project.ID, state.Name, state.Name, state.Terminal, state.Dispatchable, formatHubTime(now), formatHubTime(now)); err != nil {
				return nil, err
			}
		}
		return project, nil
	})
}

func validateNativeStates(states []tracker.NativeState) error {
	if len(states) == 0 || len(states) > 50 {
		return nativeInvalid("Between 1 and 50 workflow states are required")
	}
	names := make(map[string]bool, len(states))
	for _, state := range states {
		if strings.TrimSpace(state.Name) == "" || len(state.Name) > 100 || names[state.Name] || state.Terminal && state.Dispatchable {
			return nativeInvalid("Workflow states must be unique and valid")
		}
		names[state.Name] = true
	}
	for _, state := range states {
		for _, target := range state.Transitions {
			if !names[target] {
				return nativeInvalid("Workflow transition target does not exist")
			}
		}
	}
	return nil
}

func (s *Service) grantNativeToken(c echo.Context) error {
	var request struct {
		OrganizationID tracker.OrganizationID `json:"organization_id"`
		ProjectID      tracker.ProjectID      `json:"project_id"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if c.Param("id") == bootstrapTokenID {
		return s.nativeAPIError(c, nativeInvalid("Bootstrap administrator cannot be converted to a project token"))
	}
	ctx := c.Request().Context()
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer tx.Rollback()
	var runnerCount int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM runner_identities WHERE token_id = ?", c.Param("id")).Scan(&runnerCount); err != nil {
		return s.nativeAPIError(c, err)
	}
	if runnerCount != 0 {
		return s.nativeAPIError(c, nativeInvalid("Runner grants are fixed at enrollment; revoke and enroll a new identity to change authority"))
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO token_grants (token_id, organization_id, project_id) VALUES (?, ?, ?) ON CONFLICT DO NOTHING", c.Param("id"), request.OrganizationID, request.ProjectID); err != nil {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if _, err := tx.ExecContext(ctx, "UPDATE api_tokens SET native_only = 1 WHERE id = ?", c.Param("id")); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := tx.Commit(); err != nil {
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
	if err := query.QueryRowContext(ctx, "SELECT id, organization_id, name, profile, states_json, require_dependencies FROM projects WHERE organization_id = ? AND id = ?", scope.organization, scope.project).Scan(&project.ID, &project.OrganizationID, &project.Name, &project.Profile, &states, &project.RequireDependencies); err != nil {
		return project, err
	}
	if err := json.Unmarshal([]byte(states), &project.States); err != nil {
		return project, err
	}
	return project, nil
}

func (s *Service) getNativeProject(c echo.Context) error {
	project, err := readNativeProject(c.Request().Context(), s.database.db, nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, project)
}
