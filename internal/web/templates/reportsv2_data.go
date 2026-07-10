package templates

import (
	"sort"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/telemetry"
	webchart "github.com/digitaldrywood/detent/internal/web/chart"
)

// reportsView is where history lives: a KPI figure row (plain figures,
// not cards), two charts, two top-consumer tables, and the budget and
// cycle-time trends that moved here from the old fleet/analytics pages.
type reportsView struct {
	KPIs        []reportsKPI
	Spend       reportsSpendChart
	TokensTotal string
	TokenChart  SeriesChartData
	HasSeries   bool
	TopIssues   []reportsTopRow
	TopPRs      []reportsTopRow
	Budget      reportsBudget
	CycleTime   reportsCycleTime
	Release     reportsRelease
}

type reportsKPI struct {
	ID    string
	Value string
	Label string
	Full  string
}

type reportsSpendChart struct {
	Chart      BarChartData
	Annotation string
	HasSpend   bool
}

type reportsTopRow struct {
	ID     string
	Ref    string
	URL    string
	Title  string
	Tokens string
	Cost   string
}

type reportsBudget struct {
	State   string // ok | empty | unavailable | disabled
	Detail  string
	Value   string
	Percent int
	Warn    bool
	Chart   BarChartData
	HasDays bool
}

type reportsCycleTime struct {
	Available bool
	Average   string
	Completed string
	Chart     BarChartData
}

type reportsRelease struct {
	Available bool
	Last      string
	Merges    string
	Next      string
	State     string
}

const reportsTopRowLimit = 5

func reportsViewFromData(data ReportsData) reportsView {
	view := reportsView{
		KPIs:        reportsKPIs(data),
		Spend:       reportsSpendBars(data),
		TokensTotal: formatInt(data.Day.Totals.TotalTokens),
		HasSeries:   len(data.Day.Series) > 0,
		TopIssues:   reportsTopRows("issue", data.Issue.Breakdowns, data.Snapshot),
		TopPRs:      reportsTopRows("pr", data.PR.Breakdowns, data.Snapshot),
		Budget:      reportsBudgetView(data),
		CycleTime:   reportsCycleTimeView(data),
		Release:     reportsReleaseView(data),
	}
	if view.HasSeries {
		view.TokenChart = reportsCumulativeTokens(data)
	}
	return view
}

func reportsReleaseView(data ReportsData) reportsRelease {
	release := data.Snapshot.Release
	for _, candidate := range data.Snapshot.Releases {
		if strings.TrimSpace(data.ProjectID) == "" || strings.EqualFold(strings.TrimSpace(candidate.ProjectID), strings.TrimSpace(data.ProjectID)) {
			release = candidate
			break
		}
	}
	if release.IsZero() || !release.Enabled {
		return reportsRelease{}
	}
	last := release.LastRelease
	if last == "" {
		last = "None yet"
	}
	next := "Count threshold pending"
	if release.NextTriggerAt != nil {
		next = release.NextTriggerAt.UTC().Format("Jan 02 15:04 UTC")
	}
	return reportsRelease{
		Available: true,
		Last:      last,
		Merges:    formatCount(release.UnreleasedMerges),
		Next:      next,
		State:     strings.ReplaceAll(release.State, "_", " "),
	}
}

func reportsKPIs(data ReportsData) []reportsKPI {
	totals := data.Day.Totals
	return []reportsKPI{
		{ID: "kpi-spend", Value: formatUSD(totals.SpendUSD), Label: "Total spend"},
		{ID: "kpi-tokens", Value: fleetCompactTokens(totals.TotalTokens), Label: "Tokens", Full: formatInt(totals.TotalTokens)},
		{ID: "kpi-cache", Value: reportCacheReadFraction(totals.CacheReadFraction), Label: "Cache hit"},
		{ID: "kpi-sessions", Value: formatInt(totals.Events), Label: "Sessions"},
	}
}

// reportsSpendBars clamps bars at the P95 of the window and annotates the
// clamped days, so a single spike can never flatten the rest of the chart.
// Full values stay available in each bar's hover title.
func reportsSpendBars(data ReportsData) reportsSpendChart {
	series := data.Day.Series
	if len(series) == 0 {
		return reportsSpendChart{}
	}
	hasSpend := false
	values := make([]float64, 0, len(series))
	for _, bucket := range series {
		values = append(values, bucket.SpendUSD)
		if bucket.SpendUSD > 0 {
			hasSpend = true
		}
	}
	limit := reportsP95(values)

	clamped := make([]string, 0, 1)
	bars := make([]webchart.Point, 0, len(series))
	for _, bucket := range series {
		value := bucket.SpendUSD
		label := reportBucketLabel(bucket)
		if limit > 0 && value > limit {
			clamped = append(clamped, label+" ("+formatUSD(bucket.SpendUSD)+")")
			label += " (actual " + formatUSD(bucket.SpendUSD) + ", clamped)"
			value = limit
		}
		bars = append(bars, webchart.Point{Label: label, Value: value})
	}
	view := reportsSpendChart{
		HasSpend: hasSpend,
		Chart: BarChartData{
			Title:       "Daily spend",
			AriaLabel:   "Daily spend bar chart",
			Bars:        bars,
			ValueSuffix: "USD",
			Class:       "h-30 w-full",
			ColorClass:  "text-line",
		},
	}
	if len(clamped) > 0 {
		view.Annotation = strings.Join(clamped, ", ") + " clamped — hover for value"
	}
	return view
}

func reportsP95(values []float64) float64 {
	// Compute the percentile over positive spend only; otherwise a window
	// dominated by $0 buckets pushes P95 to 0, disables the clamp, and lets a
	// single spike flatten every other nonzero bar.
	sorted := make([]float64, 0, len(values))
	for _, v := range values {
		if v > 0 {
			sorted = append(sorted, v)
		}
	}
	if len(sorted) < 2 {
		return 0
	}
	sort.Float64s(sorted)
	index := int(float64(len(sorted))*0.95) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted)-1 {
		index = len(sorted) - 2
	}
	limit := sorted[index]
	if limit <= 0 {
		return 0
	}
	if sorted[len(sorted)-1] <= limit {
		return 0
	}
	return limit
}

func reportsCumulativeTokens(data ReportsData) SeriesChartData {
	points := make([]webchart.Point, 0, len(data.Day.Series))
	total := int64(0)
	for _, bucket := range data.Day.Series {
		total += bucket.TotalTokens
		points = append(points, webchart.Point{
			Label: reportBucketLabel(bucket),
			Value: float64(total),
		})
	}
	return SeriesChartData{
		Title:      "Cumulative tokens",
		AriaLabel:  "Cumulative token line chart",
		Points:     points,
		Class:      "h-30 w-full",
		ColorClass: "text-sec",
	}
}

func reportsTopRows(prefix string, buckets []UsageBucketData, snapshot telemetry.Snapshot) []reportsTopRow {
	sorted := append([]UsageBucketData(nil), buckets...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].TotalTokens > sorted[j].TotalTokens
	})
	if len(sorted) > reportsTopRowLimit {
		sorted = sorted[:reportsTopRowLimit]
	}
	rows := make([]reportsTopRow, 0, len(sorted))
	for i, bucket := range sorted {
		rows = append(rows, reportsTopRow{
			ID:     "top-" + prefix + "-" + strconv.Itoa(i),
			Ref:    analyticsRef(bucket.Bucket),
			URL:    reportsBucketURL(prefix, bucket.Bucket, snapshot),
			Title:  reportBucketLabel(bucket),
			Tokens: fleetCompactTokens(bucket.TotalTokens),
			Cost:   formatUSD(bucket.SpendUSD),
		})
	}
	return rows
}

func reportsBucketURL(prefix string, bucket string, snapshot telemetry.Snapshot) string {
	for _, issue := range reportsSnapshotIssues(snapshot) {
		switch prefix {
		case "issue":
			if strings.EqualFold(strings.TrimSpace(bucket), strings.TrimSpace(issue.Identifier)) {
				return issueURL(issue)
			}
		case "pr":
			if reportsPRBucketMatches(bucket, issue) {
				return pullRequestURL(issue)
			}
		}
	}
	return ""
}

func reportsSnapshotIssues(snapshot telemetry.Snapshot) []telemetry.Issue {
	issues := append([]telemetry.Issue(nil), snapshot.BoardIssues...)
	issues = append(issues, snapshot.Pipeline...)
	issues = append(issues, snapshot.TrackerDrift.UntrackedOpen...)
	issues = append(issues, snapshot.TrackerDrift.OpenTerminal...)
	for _, row := range snapshot.Running {
		issues = append(issues, row.Issue)
	}
	for _, row := range snapshot.Queue {
		issues = append(issues, row.Issue)
	}
	for _, row := range snapshot.Blocked {
		issues = append(issues, row.Issue)
	}
	for _, row := range snapshot.Completed {
		issues = append(issues, row.Issue)
	}
	return issues
}

func reportsPRBucketMatches(bucket string, issue telemetry.Issue) bool {
	repository, reference, ok := strings.Cut(strings.TrimSpace(bucket), "#")
	if !ok || pullRequestNumber(issue) <= 0 || reference != strconv.Itoa(pullRequestNumber(issue)) {
		return false
	}
	issueRepository := pullRequestRepository(issue)
	if strings.EqualFold(repository, issueRepository) {
		return true
	}
	if index := strings.LastIndex(issueRepository, "/"); index >= 0 {
		issueRepository = issueRepository[index+1:]
	}
	return strings.EqualFold(repository, issueRepository)
}

func reportsBudgetView(data ReportsData) reportsBudget {
	budget := data.Snapshot.Budget
	view := reportsBudget{}
	switch {
	case !budget.Enabled:
		view.State = "disabled"
		view.Detail = "Budget disabled — spend is tracked but not limited."
		return view
	case strings.TrimSpace(budget.DegradedReason) != "":
		view.State = "unavailable"
		view.Detail = "Budget data unavailable. " + strings.TrimSpace(budget.DegradedReason)
		return view
	case budget.CurrentSpendUSD <= 0 && len(budget.Days) == 0:
		view.State = "empty"
		view.Detail = "No budget spend yet."
		return view
	}
	view.State = "ok"
	view.Value = formatUSD(budget.CurrentSpendUSD)
	if cap := budget.PerDayMaxUSD; cap != nil && *cap > 0 {
		view.Value += " / " + formatUSD(*cap)
		percent := int(budget.CurrentSpendUSD / *cap * 100)
		if percent > 100 {
			percent = 100
		}
		view.Percent = percent
		view.Warn = percent >= 90
	}
	if len(budget.Days) > 1 {
		bars := make([]webchart.Point, 0, len(budget.Days))
		for _, day := range budget.Days {
			bars = append(bars, webchart.Point{Label: day.Date, Value: day.SpendUSD})
		}
		view.HasDays = true
		view.Chart = BarChartData{
			Title:       "Budget burn-down",
			AriaLabel:   "Daily budget spend",
			Bars:        bars,
			ValueSuffix: "USD",
			Class:       "h-15 w-full",
			ColorClass:  "text-line",
		}
	}
	return view
}

func reportsCycleTimeView(data ReportsData) reportsCycleTime {
	report := data.Snapshot.CycleTime
	view := reportsCycleTime{Available: report.Available && (report.AverageSeconds > 0 || len(report.Buckets) > 0)}
	if !view.Available {
		return view
	}
	view.Average = formatDuration(float64(report.AverageSeconds))
	view.Completed = formatCount(len(report.Issues)) + " completed"
	if len(report.Buckets) > 0 {
		bars := make([]webchart.Point, 0, len(report.Buckets))
		for _, bucket := range report.Buckets {
			bars = append(bars, webchart.Point{
				Label: bucket.Label,
				Value: float64(bucket.Count),
			})
		}
		view.Chart = BarChartData{
			Title:       "Cycle time distribution",
			AriaLabel:   "Cycle time distribution",
			Bars:        bars,
			ValueSuffix: "issues",
			Class:       "h-15 w-full",
			ColorClass:  "text-line",
		}
	}
	return view
}

func reportsTopRowClass(last bool) string {
	base := "grid grid-cols-[90px_minmax(0,1fr)_110px_90px] items-center gap-3.5 px-4 py-2"
	if !last {
		return base + " border-b border-line"
	}
	return base
}

func ReportsShellDataFromData(data ReportsData) DashboardShellData {
	shell := reportsDashboardShellData(data)
	shell.IncludeDashboardCharts = false
	return shell
}
