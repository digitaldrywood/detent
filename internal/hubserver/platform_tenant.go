package hubserver

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type platformTenantAllowance struct {
	Used  int64  `json:"used"`
	Limit *int64 `json:"limit"`
}

type platformTenantUsage struct {
	WindowEndsAt string                             `json:"window_ends_at"`
	Allowances   map[string]platformTenantAllowance `json:"allowances"`
}

func (s *Service) platformTenantUsage(c echo.Context) error {
	usage := platformTenantUsage{Allowances: map[string]platformTenantAllowance{}}
	if s.database.plans() == nil {
		return c.JSON(http.StatusOK, usage)
	}
	entitlement, err := s.database.hostedPlanUsage(c.Request().Context(), s.config.now())
	if err != nil {
		return s.internalAPIError(c, "usage_unavailable", "Usage is unavailable", err)
	}
	usage.WindowEndsAt = entitlement.WindowEndsAt.UTC().Format(time.RFC3339)
	for _, name := range hostedAllowanceNames() {
		row := platformTenantAllowance{Used: entitlement.Usage[name]}
		if limit, limited := entitlement.Allowances[name]; limited {
			row.Limit = &limit
		}
		usage.Allowances[name] = row
	}
	return c.JSON(http.StatusOK, usage)
}

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
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	type runner struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Health string `json:"health"`
	}
	runners := []runner{}
	for offset := 0; ; offset += 200 {
		rows, err := s.listRunnerRoutingData(ctx, s.accessOrganization(), 200, offset)
		if err != nil {
			return s.internalAPIError(c, "runners_unavailable", "Runners are unavailable", err)
		}
		for _, row := range rows {
			runners = append(runners, runner{ID: row.RunnerID, Name: row.DisplayName, Health: row.Status(s.config.now())})
		}
		if len(rows) < 200 {
			break
		}
	}
	return c.JSON(http.StatusOK, runners)
}

func (s *Service) platformTenantProjects(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	type project struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	projects := []project{}
	after := tracker.ProjectID("")
	for {
		var rows []corestore.Project
		err := s.config.TenantRepository.ReadTenant(ctx, s.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			rows, err = r.Projects(ctx, after, corestore.MaxPageSize)
			return err
		})
		if err != nil {
			return s.internalAPIError(c, "projects_unavailable", "Projects are unavailable", err)
		}
		for _, row := range rows {
			if row.DeletedAt == nil {
				projects = append(projects, project{ID: string(row.ID), Name: row.Name})
			}
			after = row.ID
		}
		if len(rows) < corestore.MaxPageSize {
			break
		}
	}
	slices.SortFunc(projects, func(a, b project) int { return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID)) })
	return c.JSON(http.StatusOK, projects)
}
