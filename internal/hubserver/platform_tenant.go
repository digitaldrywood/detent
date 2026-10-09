package hubserver

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) platformTenantMembers(c echo.Context) error {
	ctx := c.Request().Context()
	provider, err := s.hostedProviderOrganization(ctx)
	if err != nil {
		return s.internalAPIError(c, "members_unavailable", "Members are unavailable", err)
	}
	result, err := s.hostedMembersFor(ctx, apiCredential{Hosted: &auth.HostedIdentity{OrganizationID: provider}, HostedRole: "owner"})
	if err != nil {
		return s.internalAPIError(c, "members_unavailable", "Members are unavailable", err)
	}
	type member struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	members := make([]member, 0, len(result.Members))
	for _, item := range result.Members {
		members = append(members, member{ID: item.UserID, Email: item.Email, Role: item.Role})
	}
	return c.JSON(http.StatusOK, members)
}

func (s *Service) platformTenantRunners(c echo.Context) error {
	ctx := c.Request().Context()
	organization := tracker.OrganizationID(s.config.Hosted.OrganizationID)
	rows, err := s.database.db.QueryContext(ctx, "SELECT id FROM runner_identities WHERE organization_id = ? AND removed_at IS NULL ORDER BY display_name, id", organization)
	if err != nil {
		return s.internalAPIError(c, "runners_unavailable", "Runners are unavailable", err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return s.internalAPIError(c, "runners_unavailable", "Runners are unavailable", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return s.internalAPIError(c, "runners_unavailable", "Runners are unavailable", err)
	}
	if err := rows.Close(); err != nil {
		return s.internalAPIError(c, "runners_unavailable", "Runners are unavailable", err)
	}
	type runner struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Health string `json:"health"`
	}
	runners := make([]runner, 0, len(ids))
	for _, id := range ids {
		item, err := readRunnerWithClock(ctx, s.database.db, organization, id, s.config.now)
		if err != nil {
			return s.internalAPIError(c, "runners_unavailable", "Runners are unavailable", err)
		}
		runners = append(runners, runner{ID: id, Name: item.DisplayName, Health: item.Status(s.config.now())})
	}
	return c.JSON(http.StatusOK, runners)
}

func (s *Service) platformTenantProjects(c echo.Context) error {
	rows, err := s.database.db.QueryContext(c.Request().Context(), "SELECT id,name FROM projects WHERE deleted_at IS NULL AND organization_id = ? ORDER BY name, id", s.config.Hosted.OrganizationID)
	if err != nil {
		return s.internalAPIError(c, "projects_unavailable", "Projects are unavailable", err)
	}
	defer rows.Close()
	type project struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	projects := []project{}
	for rows.Next() {
		var item project
		if err := rows.Scan(&item.ID, &item.Name); err != nil {
			return s.internalAPIError(c, "projects_unavailable", "Projects are unavailable", err)
		}
		projects = append(projects, item)
	}
	if err := rows.Err(); err != nil {
		return s.internalAPIError(c, "projects_unavailable", "Projects are unavailable", err)
	}
	return c.JSON(http.StatusOK, projects)
}
