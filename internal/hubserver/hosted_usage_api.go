package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// The usage report (decisions section 17.5). Every member of the
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
	e.GET("/api/v2/organizations/:organization/usage", s.hostedUsageReport, s.usageSessionOnly)
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
		return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "forbidden", Message: "This account has no access to this organization"})
	}
	if errors.Is(err, sql.ErrNoRows) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	return s.nativeAPIError(c, err)
}

// usageReadableProjects lists the projects the hosted member holds a grant
// for, which is what the report is filtered to.
func (s *Service) usageReadableProjects(ctx context.Context, credential apiCredential) ([]string, error) {
	rows, err := s.database.db.QueryContext(ctx, `SELECT p.id FROM projects p JOIN hosted_project_grants g ON g.project_id = p.id
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

type usageWindow struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Hourly reports the series per hour rather than per day: the client
	// draws the 24h range from hourly buckets (contracts/usage.ts, isHourly).
	Hourly bool `json:"-"`
}

type usageTotal struct {
	Cost     float64 `json:"cost"`
	Tokens   int64   `json:"tokens"`
	Sessions int64   `json:"sessions"`
}

type usageProvider struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	Sessions int64   `json:"sessions"`
	Cost     float64 `json:"cost"`
	Share    float64 `json:"share"`
	Tokens   int64   `json:"tokens"`
}

// usageBucketTime is when one period of the daily series starts. A day
// bucket is written as the day alone and an hourly bucket keeps its clock:
// that is how the client tells the two apart (contracts/usage.ts, isHourly),
// so a day written with a clock would be drawn as one hour and the daily
// chart would be empty.
type usageBucketTime struct {
	time.Time
	Hourly bool
}

const usageHourLayout = "2006-01-02T15:04:05.000Z07:00"

func (t usageBucketTime) MarshalJSON() ([]byte, error) {
	if t.Hourly {
		return json.Marshal(t.UTC().Format(usageHourLayout))
	}
	return json.Marshal(t.UTC().Format(usageDayLayout))
}

func (t *usageBucketTime) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	if parsed, err := time.Parse(usageDayLayout, text); err == nil {
		t.Time, t.Hourly = parsed.UTC(), false
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return fmt.Errorf("decode usage period %q: %w", text, err)
	}
	t.Time, t.Hourly = parsed.UTC(), true
	return nil
}

type usageDay struct {
	Day        usageBucketTime    `json:"day"`
	Cost       float64            `json:"cost"`
	Tokens     int64              `json:"tokens"`
	ByProvider map[string]float64 `json:"by_provider"`
}

type usageTotals struct {
	Processed     int64   `json:"processed"`
	CachedInput   int64   `json:"cached_input"`
	UncachedInput int64   `json:"uncached_input"`
	Output        int64   `json:"output"`
	CacheSavings  float64 `json:"cache_savings"`
}

type usageModel struct {
	Model    string  `json:"model"`
	Provider string  `json:"provider"`
	Cost     float64 `json:"cost"`
	Share    float64 `json:"share"`
	Tokens   int64   `json:"tokens"`
}

type usageBreakdown struct {
	ByModel []usageModel `json:"by_model"`
	ByDay   []usageDay   `json:"by_day"`
}

type usageLimit struct {
	Used  int64 `json:"used"`
	Limit int64 `json:"limit"`
}

type usageRunner struct {
	ID           string  `json:"id"`
	DisplayName  string  `json:"display_name"`
	Sessions     int64   `json:"sessions"`
	Tokens       int64   `json:"tokens"`
	Cost         float64 `json:"cost"`
	BusySeconds  int64   `json:"busy_seconds"`
	CapacityUsed float64 `json:"capacity_used"`
}

type usageReport struct {
	Range     usageWindow           `json:"range"`
	Total     usageTotal            `json:"total"`
	Providers []usageProvider       `json:"providers"`
	Daily     []usageDay            `json:"daily"`
	Totals    usageTotals           `json:"totals"`
	Breakdown usageBreakdown        `json:"breakdown"`
	Limits    map[string]usageLimit `json:"limits"`
	Runners   []usageRunner         `json:"runners"`
	Currency  string                `json:"currency"`
}

// usageRow is one stored attempt_usage row joined with what the report needs
// to attribute it: the runner that ran the attempt and how long it was busy.
type usageRow struct {
	AttemptID   string
	Period      time.Time
	Provider    string
	Model       string
	Input       int64
	CachedInput int64
	Output      int64
	Cost        float64
	Currency    string
	RunnerID    string
	RunnerName  string
	BusySeconds int64
}

func (r usageRow) tokens() int64 { return r.Input + r.Output }

// hostedUsageReport implements GET /api/v2/organizations/:organization/usage.
func (s *Service) hostedUsageReport(c echo.Context) error {
	credential, _, err := s.hostedCredential(c)
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	ctx := c.Request().Context()
	window, err := usageRangeWindow(c.QueryParam("range"), s.config.now())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	projects, err := s.usageReadableProjects(ctx, credential)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if requested := strings.TrimSpace(c.QueryParam("project")); requested != "" {
		if !slices.Contains(projects, requested) {
			// A project the member cannot read is indistinguishable from one
			// that does not exist.
			return s.nativeAPIError(c, nativeNotFound())
		}
		projects = []string{requested}
	}
	rows, err := s.usageRows(ctx, window, projects)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	limits, err := s.usageLimits(ctx)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	capacity, err := s.usageRunnerCapacity(ctx)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	report := buildUsageReport(window, rows, limits, capacity, s.usagePrices())
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
	if name == "" {
		name = defaultUsageRange
	}
	span, known := usageRanges[name]
	if !known {
		return usageWindow{}, nativeInvalid("Range must be one of 24h, 7d, 30d or 90d")
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
	if len(projects) == 0 {
		return nil, nil
	}
	encoded, err := marshalNative(projects)
	if err != nil {
		return nil, fmt.Errorf("encode readable projects: %w", err)
	}
	query := `SELECT u.attempt_id, u.period, u.provider, u.model, u.input, u.cached_input, u.output, u.cost_estimate, u.currency,
 coalesce(r.id, ''), coalesce(r.display_name, ''), coalesce(a.started_at, ''), coalesce(a.updated_at, '')
FROM attempt_usage u
LEFT JOIN native_attempts a ON a.id = u.attempt_id
LEFT JOIN lease_runners lr ON lr.lease_id = a.lease_id
LEFT JOIN runner_identities r ON r.id = lr.runner_id AND r.organization_id = u.organization_id
WHERE u.organization_id = ? AND u.period >= ? AND u.period <= ?
 AND u.project_id IN (SELECT value FROM json_each(?))
ORDER BY u.period, u.provider, u.model, u.attempt_id`
	result, err := s.database.db.QueryContext(ctx, query,
		s.config.Hosted.OrganizationID, window.From.UTC().Format(usagePeriodLayout), window.To.UTC().Format(usagePeriodLayout), encoded)
	if err != nil {
		return nil, fmt.Errorf("read usage: %w", err)
	}
	defer func() { _ = result.Close() }()
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
	}
	if err := errors.Join(result.Err(), result.Close()); err != nil {
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
	if s.database.hostedPlans == nil {
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
	capacity := map[string]runnerCapacity{}
	rows, err := s.database.db.QueryContext(ctx, "SELECT id FROM runner_identities WHERE organization_id = ? ORDER BY display_name, id",
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
		runner, err := readRunner(ctx, s.database.db, tracker.OrganizationID(s.config.Hosted.OrganizationID), id, s.config.now())
		if err != nil {
			return nil, fmt.Errorf("read runner %s: %w", id, err)
		}
		capacity[id] = runnerCapacity{DisplayName: runner.DisplayName, Limit: runner.CapacityLimit}
	}
	return capacity, nil
}

type runnerCapacity struct {
	DisplayName string
	Limit       int
}

// buildUsageReport folds the stored rows into the section 17.5 shape. It is a
// pure function of what it is given so the aggregation is testable without a
// database.
func buildUsageReport(window usageWindow, rows []usageRow, limits map[string]usageLimit, capacity map[string]runnerCapacity, prices UsageConfig) usageReport {
	report := usageReport{
		Range: window, Providers: []usageProvider{}, Daily: []usageDay{},
		Breakdown: usageBreakdown{ByModel: []usageModel{}, ByDay: []usageDay{}},
		Limits:    limits, Runners: []usageRunner{}, Currency: prices.currency(),
	}
	if report.Limits == nil {
		report.Limits = map[string]usageLimit{}
	}
	providers := map[string]*usageProvider{}
	providerAttempts := map[string]map[string]bool{}
	models := map[string]*usageModel{}
	days := map[string]*usageDay{}
	runners := map[string]*usageRunner{}
	runnerAttempts := map[string]map[string]bool{}
	attempts := map[string]bool{}
	busy := map[string]int64{}
	for _, row := range rows {
		tokens := row.tokens()
		attempts[row.AttemptID] = true
		// A hub reports in one currency. A row stored under another (the
		// table's currency changed since) still counts its tokens, but its
		// cost cannot be added without a conversion the hub does not have.
		if row.Currency != "" && row.Currency != report.Currency {
			row.Cost = 0
		}
		report.Total.Cost += row.Cost
		report.Total.Tokens += tokens
		report.Totals.Processed += tokens
		report.Totals.CachedInput += row.CachedInput
		report.Totals.UncachedInput += usageUncached(row.Input, row.CachedInput)
		report.Totals.Output += row.Output
		report.Totals.CacheSavings += prices.cacheSaving(row.Model, row.Input, row.CachedInput)

		provider, known := providers[row.Provider]
		servedAttempts := providerAttempts[row.Provider]
		if !known || servedAttempts == nil {
			provider = &usageProvider{ID: row.Provider, Label: tracker.UsageProviderLabel(row.Provider)}
			servedAttempts = map[string]bool{}
			providers[row.Provider] = provider
			providerAttempts[row.Provider] = servedAttempts
		}
		provider.Cost += row.Cost
		provider.Tokens += tokens
		servedAttempts[row.AttemptID] = true

		modelKey := row.Provider + "\x00" + row.Model
		model, known := models[modelKey]
		if !known {
			model = &usageModel{Model: row.Model, Provider: row.Provider}
			models[modelKey] = model
		}
		model.Cost += row.Cost
		model.Tokens += tokens

		bucket := usageBucketTime{Time: row.Period.Truncate(time.Hour), Hourly: true}
		if !window.Hourly {
			bucket = usageBucketTime{Time: time.Date(row.Period.Year(), row.Period.Month(), row.Period.Day(), 0, 0, 0, 0, time.UTC)}
		}
		bucketKey := bucket.UTC().Format(usagePeriodLayout)
		day, known := days[bucketKey]
		if !known {
			day = &usageDay{Day: bucket, ByProvider: map[string]float64{}}
			days[bucketKey] = day
		}
		day.Cost += row.Cost
		day.Tokens += tokens
		day.ByProvider[row.Provider] += row.Cost

		if row.RunnerID == "" {
			continue
		}
		runner, known := runners[row.RunnerID]
		ranAttempts := runnerAttempts[row.RunnerID]
		if !known || ranAttempts == nil {
			runner = &usageRunner{ID: row.RunnerID, DisplayName: row.RunnerName}
			ranAttempts = map[string]bool{}
			runners[row.RunnerID] = runner
			runnerAttempts[row.RunnerID] = ranAttempts
		}
		runner.Cost += row.Cost
		runner.Tokens += tokens
		// An attempt is counted once however many models it used.
		if !ranAttempts[row.AttemptID] {
			ranAttempts[row.AttemptID] = true
			busy[row.RunnerID] += row.BusySeconds
		}
	}
	report.Total.Sessions = int64(len(attempts))
	report.Total.Cost = usageRound(report.Total.Cost)
	report.Totals.CacheSavings = usageRound(report.Totals.CacheSavings)
	total := report.Total.Cost

	for _, provider := range providers {
		provider.Sessions = int64(len(providerAttempts[provider.ID]))
		provider.Cost = usageRound(provider.Cost)
		provider.Share = usageShare(provider.Cost, total)
		report.Providers = append(report.Providers, *provider)
	}
	slices.SortFunc(report.Providers, func(a, b usageProvider) int { return usageRank(a.Cost, b.Cost, a.ID, b.ID) })

	for _, model := range models {
		model.Cost = usageRound(model.Cost)
		model.Share = usageShare(model.Cost, total)
		report.Breakdown.ByModel = append(report.Breakdown.ByModel, *model)
	}
	slices.SortFunc(report.Breakdown.ByModel, func(a, b usageModel) int { return usageRank(a.Cost, b.Cost, a.Model, b.Model) })

	for _, day := range days {
		day.Cost = usageRound(day.Cost)
		for provider, cost := range day.ByProvider {
			day.ByProvider[provider] = usageRound(cost)
		}
		report.Daily = append(report.Daily, *day)
	}
	slices.SortFunc(report.Daily, func(a, b usageDay) int { return a.Day.Compare(b.Day.Time) })
	report.Breakdown.ByDay = report.Daily

	span := window.To.Sub(window.From).Seconds()
	for _, runner := range runners {
		runner.Sessions = int64(len(runnerAttempts[runner.ID]))
		runner.Cost = usageRound(runner.Cost)
		runner.BusySeconds = busy[runner.ID]
		if known, found := capacity[runner.ID]; found {
			if strings.TrimSpace(runner.DisplayName) == "" {
				runner.DisplayName = known.DisplayName
			}
			runner.CapacityUsed = usageCapacityUsed(runner.BusySeconds, span, known.Limit)
		}
		report.Runners = append(report.Runners, *runner)
	}
	slices.SortFunc(report.Runners, func(a, b usageRunner) int { return usageRank(a.Cost, b.Cost, a.ID, b.ID) })
	return report
}

// usageUncached is the input a provider had to read rather than serve from
// its cache.
func usageUncached(input, cached int64) int64 {
	if cached >= input {
		return 0
	}
	return input - cached
}

// usageShare is one row's part of the window's cost. A window that cost
// nothing has no shares rather than a division by zero.
func usageShare(cost, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return math.Round(cost/total*10000) / 10000
}

// usageRound keeps money to four decimals: enough for a fraction of a cent,
// short of floating-point noise.
func usageRound(value float64) float64 { return math.Round(value*10000) / 10000 }

// usageCapacityUsed is the share of the window the runner was busy, against
// the concurrent work it is allowed. A runner with no capacity limit reports
// none rather than a division by zero.
func usageCapacityUsed(busySeconds int64, span float64, limit int) float64 {
	if busySeconds <= 0 || span <= 0 || limit <= 0 {
		return 0
	}
	used := float64(busySeconds) / (span * float64(limit))
	return math.Round(min(used, 1)*10000) / 10000
}

// usageRank orders a report list by cost, descending, with the identifier
// breaking ties so the order is stable.
func usageRank(leftCost, rightCost float64, leftID, rightID string) int {
	switch {
	case leftCost > rightCost:
		return -1
	case leftCost < rightCost:
		return 1
	default:
		return strings.Compare(leftID, rightID)
	}
}
