package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) registerHostedUsageRoutes(e *echo.Echo) {
	e.GET("/api/v2/organizations/:organization/activity", s.hostedActivity, s.operatorAuthority)
	e.GET("/api/v2/organizations/:organization/usage", s.hostedUsageReport, s.usageSessionOnly)
	e.GET("/api/v2/organizations/:organization/diagnostics/requests", s.hostedRequestMetrics, s.operatorAuthority)
	e.GET("/api/v2/organizations/:organization/diagnostics", s.hostedDiagnostics, s.usageSessionOnly, s.operatorAuthority)
	e.GET("/api/v2/organizations/:organization/reports", s.hostedReports, s.usageSessionOnly, s.operatorAuthority)
	e.GET("/api/v2/organizations/:organization/reports/options", s.hostedReportOptions, s.usageSessionOnly, s.operatorAuthority)
}

func (s *Service) usageSessionOnly(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if c.Request().Header.Get(echo.HeaderAuthorization) != "" {
			return s.hostedError(c, http.StatusForbidden, "This endpoint authenticates a hosted session")
		}
		return next(c)
	}
}

func (s *Service) usageCredentialError(c echo.Context, err error) error {
	if errors.Is(err, auth.ErrInvalidSession) {
		return c.JSON(http.StatusUnauthorized, hubapi.APIErrorResponse{Code: "unauthorized", Message: "A hosted session is required"})
	}
	if errors.Is(err, auth.ErrHostedIdentity) {
		return s.refusedAPIError(c, http.StatusForbidden, "forbidden", "This account has no access to this organization", hubapi.APIErrorResponse{Code: "forbidden", Message: "This account has no access to this organization"})
	}
	if errors.Is(err, sql.ErrNoRows) {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	return s.nativeAPIError(c, err)
}

func (s *Service) usageReadableProjects(ctx context.Context, credential hubapi.APICredential) ([]string, error) {
	projects, err := s.hostedReadableProjectsPostgres(ctx, credential)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(projects))
	for i, project := range projects {
		ids[i] = project.ID
	}
	return ids, nil
}

func (s *Service) hostedUsageReport(c echo.Context) error {
	credential, _, err := s.hostedCredential(c.Request().Context(), c)
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	ctx := c.Request().Context()
	report, err := s.readHostedUsage(ctx, credential, c.QueryParam("range"), c.QueryParam("project"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}

	if err := s.hostedAudit(ctx, credential.Hosted, "action", "GET "+c.Path(), "", http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, report)
}

func (s *Service) usageRows(ctx context.Context, window usageWindow, projects []string) ([]usageRow, error) {
	if len(projects) == 0 {
		return nil, nil
	}
	query := reportstore.OverviewQuery{From: window.From, Split: window.From, To: window.To, Limit: 100000}
	for _, project := range projects {
		query.Projects = append(query.Projects, tracker.ProjectID(project))
	}
	if scope, ok := reportsScopeFrom(ctx); ok {
		if scope.Issues != "" {
			return nil, errors.New("typed usage requires resolved report filters")
		}
		query.Principal = scope.ReporterPrincipal
		query.Filter = reportstore.Filter{Runners: scope.Attempts.Runners, Models: scope.Attempts.Models, Stages: scope.Attempts.Stages}
	}
	var facts []reportstore.UsageFact
	var attempts []reportstore.AttemptFact
	err := s.config.TenantRepository.ReadTenant(ctx, s.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
		usage, ok := r.(reportstore.UsageReader)
		if !ok {
			return errors.New("typed usage reader unavailable")
		}
		attempt, ok := r.(reportstore.AttemptReader)
		if !ok {
			return errors.New("typed attempt reader unavailable")
		}
		var err error
		facts, err = usage.ReadReportUsage(ctx, query)
		if err != nil {
			return err
		}
		attempts, err = attempt.ReadAttemptFacts(ctx, reportstore.AttemptQuery{Projects: query.Projects, Principal: query.Principal, From: query.From, To: query.To, Filter: query.Filter, Limit: 50000})
		return err
	})
	if err != nil {
		return nil, err
	}
	grouped, err := postgresReportUsageRows(facts)
	if err != nil {
		return nil, err
	}
	rows := []usageRow{}
	for _, attempt := range attempts {
		usage := grouped[attempt.AttemptID]
		if len(usage) == 0 {
			continue
		}
		usage[0].BusySeconds = usageBusySeconds(hubapi.FormatHubTime(attempt.StartedAt), hubapi.FormatHubTime(attempt.UpdatedAt), window)
		rows = append(rows, usage...)
		delete(grouped, attempt.AttemptID)
	}
	if len(grouped) != 0 {
		return nil, errors.New("typed usage has no scoped attempt")
	}
	return rows, nil
}

func (s *Service) usageLimits(ctx context.Context) (map[string]usageLimit, error) {
	limits := map[string]usageLimit{}
	if s.database.plansService().Plans() == nil {
		return limits, nil
	}
	entitlement, err := hubapi.FeatureResult(s.database.plansService().PlanUsage(ctx, s.config.Clock()))
	if err != nil {
		return nil, err
	}
	for _, name := range hostedplans.AllowanceNames() {
		limit, allowed := entitlement.Allowances[name]
		if !allowed {
			continue
		}
		limits[name] = usageLimit{Used: entitlement.Usage[name], Limit: limit}
	}
	return limits, nil
}

func (s *Service) usageRunnerCapacity(ctx context.Context) (map[string]runnerCapacity, error) {
	capacity := map[string]runnerCapacity{}
	for offset := 0; ; offset += 200 {
		rows, err := s.listRunnerRoutingData(ctx, s.accessOrganization(), 200, offset)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			capacity[row.RunnerID] = runnerCapacity{DisplayName: row.DisplayName, Limit: row.CapacityLimit}
		}
		if len(rows) < 200 {
			return capacity, nil
		}
	}
}

func (s *Service) readHostedUsage(ctx context.Context, credential hubapi.APICredential, rangeName, project string) (usageReport, error) {
	window, err := usageRangeWindow(rangeName, s.config.Clock())
	if err != nil {
		return usageReport{}, err
	}
	projects, err := s.usageReadableProjects(ctx, credential)
	if err != nil {
		return usageReport{}, err
	}
	if requested := strings.TrimSpace(project); requested != "" {
		if !slices.Contains(projects, requested) {
			// A project the member cannot read is indistinguishable from one
			// that does not exist.
			return usageReport{}, hubapi.NativeNotFound()
		}
		projects = []string{requested}
	}
	rows, err := s.usageRows(ctx, window, projects)
	if err != nil {
		return usageReport{}, err
	}
	limits, err := s.usageLimits(ctx)
	if err != nil {
		return usageReport{}, err
	}
	capacity, err := s.usageRunnerCapacity(ctx)
	if err != nil {
		return usageReport{}, err
	}
	chatRows, err := s.chatUsageRows(ctx, window, projects)
	if err != nil {
		return usageReport{}, err
	}
	report := buildUsageReport(window, append(rows, chatRows...), limits, capacity, s.usagePrices())
	report.Chat, err = s.database.chatUsageSummary(ctx, s.database.hostedOrganization, window, projects)
	if err != nil {
		return usageReport{}, err
	}
	month, err := costMonth("", s.config.Clock())
	if strings.HasPrefix(strings.TrimSpace(rangeName), "month:") {
		month = window
	}
	if err != nil {
		return usageReport{}, err
	}
	costScope := "readable_projects"
	if strings.TrimSpace(project) != "" {
		costScope = "project"
	}
	costs, err := s.database.monthlyCosts(ctx, s.config.Hosted.OrganizationID, costScope, projects, month, s.config.Clock())
	if err != nil {
		return usageReport{}, err
	}
	report.MonthlyCosts = &costs
	return report, nil
}
