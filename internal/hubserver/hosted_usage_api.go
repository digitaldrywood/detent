package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hostedusage"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// The usage report. Every member of the
// organization reads it; the rows are filtered to the projects the member
// can read, so a member without a grant sees their own organization's shape
// without another team's spend. Cost is an estimate the client labels "API
// estimate": what a runner priced, or what the hub's own table prices.

// usageRanges are the windows the report offers, in the order the client
// shows them.
var usageRanges = map[string]time.Duration{
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
	"90d": 90 * 24 * time.Hour,
}

const defaultUsageRange = "7d"

// registerHostedUsageRoutes serves the usage report on the hosted API. The
// hosted boundary supplies the organization path check; the handler
// authenticates the hosted session itself, and usageSessionOnly refuses a
// bearer credential so the route never answers anything but that session.
func (s *Service) registerHostedUsageRoutes(e *echo.Echo) {
	e.GET("/api/v2/organizations/:organization/activity", s.hostedActivity, s.operatorAuthority)
	e.GET("/api/v2/organizations/:organization/usage", s.hostedUsageReport, s.usageSessionOnly)
	e.GET("/api/v2/organizations/:organization/diagnostics/requests", s.hostedRequestMetrics, s.operatorAuthority)
	e.GET("/api/v2/organizations/:organization/diagnostics", s.hostedDiagnostics, s.usageSessionOnly, s.operatorAuthority)
	e.GET("/api/v2/organizations/:organization/reports", s.hostedReports, s.usageSessionOnly, s.operatorAuthority)
	e.GET("/api/v2/organizations/:organization/reports/options", s.hostedReportOptions, s.usageSessionOnly, s.operatorAuthority)
}

// usageSessionOnly refuses a bearer credential on the session-authenticated
// usage route.
func (s *Service) usageSessionOnly(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if c.Request().Header.Get(echo.HeaderAuthorization) != "" {
			return s.hostedError(c, http.StatusForbidden, "This endpoint authenticates a hosted session")
		}
		return next(c)
	}
}

// usageCredentialError maps a hosted session failure onto the API error the
// client reads.
func (s *Service) usageCredentialError(c echo.Context, err error) error {
	if errors.Is(err, auth.ErrInvalidSession) {
		return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "unauthorized", Message: "A hosted session is required"})
	}
	if errors.Is(err, auth.ErrHostedIdentity) {
		return s.refusedAPIError(c, http.StatusForbidden, "forbidden", "This account has no access to this organization", apiErrorResponse{Code: "forbidden", Message: "This account has no access to this organization"})
	}
	if errors.Is(err, sql.ErrNoRows) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	return s.nativeAPIError(c, err)
}

// usageReadableProjects lists the projects the hosted member holds a grant
// for, which is what the report is filtered to.
func (s *Service) usageReadableProjects(ctx context.Context, credential apiCredential) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	rows, err := s.database.reader.QueryContext(ctx, `SELECT p.id FROM projects p JOIN hosted_project_grants g ON g.project_id = p.id
WHERE g.user_id = ? AND p.organization_id = ? ORDER BY p.id`, credential.Hosted.Subject, s.config.Hosted.OrganizationID)
	if err != nil {
		return nil, fmt.Errorf("list project grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	projects := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan project grant: %w", err)
		}
		projects = append(projects, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list project grants: %w", err)
	}
	return projects, nil
}

// hostedUsageReport implements GET /api/v2/organizations/:organization/usage.
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

// usageRangeWindow resolves the requested range. An unknown range is a
// validation failure rather than a silent default, so a client typo is
// visible instead of showing the wrong week.
func usageRangeWindow(value string, now time.Time) (usageWindow, error) {
	name := strings.TrimSpace(value)
	if strings.HasPrefix(name, "month:") {
		if name == "month:" {
			return usageWindow{}, nativePublicInvalid("Month must be YYYY-MM in UTC")
		}
		return costMonth(strings.TrimPrefix(name, "month:"), now)
	}
	if name == "" {
		name = defaultUsageRange
	}
	span, known := usageRanges[name]
	if !known {
		return usageWindow{}, nativePublicInvalid("Range must be 24h, 7d, 30d, 90d or month:YYYY-MM in UTC")
	}
	// Usage is stored per UTC hour, so the window starts on an hour boundary:
	// the current partial hour plus the full hours before it, span in all.
	// Every stored period at or after From lies wholly inside the window.
	to := now.UTC()
	from := to.Truncate(time.Hour).Add(time.Hour - span)
	return usageWindow{From: from, To: to, Hourly: span <= 24*time.Hour}, nil
}

// usageRows reads the stored usage of one window, joined to the runner that
// ran each attempt. The runner comes from lease_runners, which names exactly
// one runner per lease; joining through the machine would repeat a row for
// every runner that shares it. Periods are compared as fixed-width UTC
// timestamps in the stored T format, not as dates.
func (s *Service) usageRows(ctx context.Context, window usageWindow, projects []string) ([]usageRow, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	return readUsageRows(ctx, s.database.reader, s.config.Hosted.OrganizationID, window, projects, int(^uint(0)>>1))
}

func readUsageRows(ctx context.Context, database nativeQueryer, organization string, window usageWindow, projects []string, limit int) ([]usageRow, error) {
	if len(projects) == 0 {
		return nil, nil
	}
	encoded, err := sqlList(projects)
	if err != nil {
		return nil, fmt.Errorf("encode readable projects: %w", err)
	}
	scoped, scopedArgs, err := reportsScopedClause(ctx, "a")
	if err != nil {
		return nil, err
	}
	attemptSource := "native_attempts"
	if ownership, filtered := reportsScopeFrom(ctx); filtered {
		scope := nativeScope{organization: tracker.OrganizationID(organization)}
		if len(projects) == 1 {
			scope.project = tracker.ProjectID(projects[0])
		}
		database, err = prepareReportsQuery(ctx, database, scope, "report_attempts")
		if err != nil {
			return nil, err
		}
		if ownership.attempts.attempts() {
			attemptSource = "report_attempts"
		}
	}
	projectClause, projectArg := "u.project_id IN (SELECT value FROM sql_rows(?))", any(encoded)
	if len(projects) == 1 {
		projectClause, projectArg = "u.project_id = ?", projects[0]
	}
	query := `SELECT u.attempt_id, u.period, u.provider, u.model, u.input, u.cached_input, u.output, u.cost_estimate, u.currency,
 coalesce(r.id, ''), coalesce(r.display_name, ''), coalesce(a.started_at, ''), coalesce(a.updated_at, '')
FROM attempt_usage u
LEFT JOIN ` + attemptSource + ` a ON a.id = u.attempt_id
LEFT JOIN lease_runners lr ON lr.lease_id = a.lease_id
LEFT JOIN runner_identities r ON r.id = lr.runner_id AND r.organization_id = u.organization_id
WHERE u.organization_id = ? AND u.period >= ? AND u.period <= ?
 AND ` + projectClause + scoped + `
ORDER BY u.period, u.provider, u.model, u.attempt_id LIMIT ? OFFSET ?`
	args := append([]any{organization, window.From.UTC().Format(usagePeriodLayout), window.To.UTC().Format(usagePeriodLayout), projectArg}, scopedArgs...)
	result, err := queryAnalyticsRows(ctx, database, query, append(args, limit, analyticsPopulationOffset(ctx, "usage"))...)
	if err != nil {
		return nil, fmt.Errorf("read usage: %w", err)
	}
	defer result.Close()
	rows := []usageRow{}
	for result.Next() {
		var row usageRow
		var period, started, updated string
		if err := result.Scan(&row.AttemptID, &period, &row.Provider, &row.Model, &row.Input, &row.CachedInput, &row.Output,
			&row.Cost, &row.Currency, &row.RunnerID, &row.RunnerName, &started, &updated); err != nil {
			return nil, fmt.Errorf("read usage: %w", err)
		}
		parsed, err := time.Parse(usagePeriodLayout, period)
		if err != nil {
			return nil, fmt.Errorf("decode usage period %q: %w", period, err)
		}
		row.Period = parsed.UTC()
		row.BusySeconds = usageBusySeconds(started, updated, window)
		rows = append(rows, row)
		if len(rows) > maxAnalyticsPopulation && limit == maxAnalyticsPopulation+1 && row.Period.Before(window.To) {
			if paging, ok := ctx.Value(analyticsPagingKey{}).(*analyticsPaging); ok {
				paging.next["usage"] = fmt.Sprintf("usage:%d:0", analyticsPopulationOffset(ctx, "usage")+maxAnalyticsPopulation)
			}
		}
	}
	result.Close()
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("read usage: %w", err)
	}
	return rows, nil
}

// usageBusySeconds is how long an attempt held its runner inside the report
// window: the attempt's interval is clamped to the window, so an attempt that
// started before it or ran past it cannot report more busy time than the
// window holds. An attempt whose timestamps the hub cannot read contributes
// nothing rather than a guess.
func usageBusySeconds(started, updated string, window usageWindow) int64 {
	if strings.TrimSpace(started) == "" || strings.TrimSpace(updated) == "" {
		return 0
	}
	from, err := parseTimeValue(started)
	if err != nil {
		return 0
	}
	to, err := parseTimeValue(updated)
	if err != nil {
		return 0
	}
	if !window.From.IsZero() && from.Before(window.From) {
		from = window.From
	}
	if !window.To.IsZero() && to.After(window.To) {
		to = window.To
	}
	seconds := int64(to.Sub(from).Seconds())
	if seconds < 0 {
		return 0
	}
	return seconds
}

// usageLimits pairs the entitlement's allowances with what has been used, so
// the report's Limits tab has the same numbers as the plan screen.
func (s *Service) usageLimits(ctx context.Context) (map[string]usageLimit, error) {
	limits := map[string]usageLimit{}
	if s.database.plans() == nil {
		return limits, nil
	}
	entitlement, err := s.database.hostedPlanUsage(ctx, s.config.now())
	if err != nil {
		return nil, err
	}
	for _, name := range hostedAllowanceNames() {
		limit, allowed := entitlement.Allowances[name]
		if !allowed {
			continue
		}
		limits[name] = usageLimit{Used: entitlement.Usage[name], Limit: limit}
	}
	return limits, nil
}

// usageRunnerCapacity is each runner's display name and how many concurrent
// leases it is allowed, which is what capacity_used is a fraction of.
func (s *Service) usageRunnerCapacity(ctx context.Context) (map[string]runnerCapacity, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	capacity := map[string]runnerCapacity{}
	rows, err := s.database.reader.QueryContext(ctx, "SELECT id FROM runner_identities WHERE organization_id = ? AND removed_at IS NULL ORDER BY display_name, id",
		tracker.OrganizationID(s.config.Hosted.OrganizationID))
	if err != nil {
		return nil, fmt.Errorf("list runners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan runner: %w", err)
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list runners: %w", err)
	}
	for _, id := range ids {
		runner, err := readRunnerWithClock(ctx, s.database.reader, tracker.OrganizationID(s.config.Hosted.OrganizationID), id, s.config.now)
		if err != nil {
			return nil, fmt.Errorf("read runner %s: %w", id, err)
		}
		capacity[id] = runnerCapacity{DisplayName: runner.DisplayName, Limit: runner.CapacityLimit}
	}
	return capacity, nil
}

func (s *Service) readHostedUsage(ctx context.Context, credential apiCredential, rangeName, project string) (usageReport, error) {
	window, err := usageRangeWindow(rangeName, s.config.now())
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
			return usageReport{}, nativeNotFound()
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
	month, err := costMonth("", s.config.now())
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
	costs, err := s.database.monthlyCosts(ctx, s.config.Hosted.OrganizationID, costScope, projects, month, s.config.now())
	if err != nil {
		return usageReport{}, err
	}
	report.MonthlyCosts = &costs
	return report, nil
}

type usageWindow = hostedusage.Window

type usageTotal = hostedusage.Total

type usageProvider = hostedusage.Provider

type usageBucketTime = hostedusage.BucketTime

type usageDay = hostedusage.Day

type usageTotals = hostedusage.Totals

type usageModel = hostedusage.Model

type usageBreakdown = hostedusage.Breakdown

type usageLimit = hostedusage.Limit

type usageRunner = hostedusage.Runner

type usageReport = hostedusage.Report[monthlyCostReport]

type usageRow = hostedusage.Row

type runnerCapacity = hostedusage.RunnerCapacity

type chatUsageSummary = hostedusage.ChatSummary

func buildUsageReport(window usageWindow, rows []usageRow, limits map[string]usageLimit, capacity map[string]runnerCapacity, prices UsageConfig) usageReport {
	return hostedusage.BuildReport[monthlyCostReport](window, rows, limits, capacity, prices)
}

func usageRound(value float64) float64 { return math.Round(value*10000) / 10000 }
