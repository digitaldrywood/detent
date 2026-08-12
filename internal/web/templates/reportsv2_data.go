package templates

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/efficiency"
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
	Digest      reportsDigest
	Efficiency  reportsEfficiency
	Outcomes    reportsCostPerOutcome
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

type reportsDigest struct {
	Timezone string
	Days     []reportsDigestDay
}

type reportsDigestDay struct {
	Date     string
	Label    string
	Today    bool
	Metrics  []reportsDigestMetric
	Projects []DailyDigestProjectData
}

type reportsDigestMetric struct {
	ID     string
	Label  string
	Value  string
	Full   string
	Delta  string
	Trend  string
	Detail string
}

type reportsEfficiency struct {
	Available  bool
	Issues     string
	Anomalies  string
	Rows       []reportsEfficiencyRow
	Dwell      []reportsEfficiencyDwell
	CacheChart SeriesChartData
	HasTrend   bool
}

type reportsEfficiencyRow struct {
	Label    string
	Current  string
	Baseline string
	Delta    string
}

type reportsEfficiencyDwell struct {
	Label    string
	Current  string
	Baseline string
}

type reportsCostPerOutcome struct {
	Window   string
	Options  []reportsOutcomeWindowOption
	Projects []reportsOutcomeProject
}

type reportsOutcomeWindowOption struct {
	Label  string
	Href   string
	Active bool
}

type reportsOutcomeProject struct {
	ID         string
	Name       string
	MergedPRs  string
	Closed     string
	Metrics    []reportsOutcomeMetric
	TokenChart SplitSeriesChartData
	SpendChart SplitSeriesChartData
	HasTrend   bool
}

type reportsOutcomeMetric struct {
	ID     string
	Label  string
	Value  string
	Detail string
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
		Digest:      reportsDigestView(data.Digest),
		Efficiency:  reportsEfficiencyView(data.Efficiency),
		Outcomes:    reportsCostPerOutcomeView(data),
	}
	if view.HasSeries {
		view.TokenChart = reportsCumulativeTokens(data)
	}
	return view
}

func reportsCostPerOutcomeView(data ReportsData) reportsCostPerOutcome {
	window := strings.TrimSpace(data.OutcomeWindow)
	if window == "" {
		window = "7d"
	}
	view := reportsCostPerOutcome{Window: window, Options: make([]reportsOutcomeWindowOption, 0, 3)}
	for _, option := range []string{"24h", "7d", "30d"} {
		view.Options = append(view.Options, reportsOutcomeWindowOption{Label: option, Href: reportsOutcomeWindowHref(data, option), Active: option == window})
	}
	projectNames := make(map[string]string, len(data.Projects))
	for _, project := range data.Projects {
		projectNames[strings.TrimSpace(project.ID)] = projectSmallMultipleName(project)
	}
	for _, project := range data.CostPerOutcome.Projects {
		name := projectNames[strings.TrimSpace(project.ProjectID)]
		if name == "" {
			name = project.ProjectID
		}
		current := project.Current
		item := reportsOutcomeProject{
			ID:        project.ProjectID,
			Name:      name,
			MergedPRs: formatInt(current.MergedPRs),
			Closed:    formatInt(current.ClosedIssues),
			Metrics: []reportsOutcomeMetric{
				{ID: "tokens-merged-pr", Label: "Tokens / merged PR", Value: reportsOutcomeTokenValue(current.TokensPerMergedPR, current.MergedPRs), Detail: reportsOutcomeCount(current.MergedPRs, "merged PR", "merged PRs")},
				{ID: "spend-merged-pr", Label: "Notional USD / merged PR", Value: reportsOutcomeSpendValue(current.SpendPerMergedPRUSD, current.MergedPRs), Detail: reportsOutcomeCount(current.MergedPRs, "merged PR", "merged PRs")},
				{ID: "tokens-closed-issue", Label: "Tokens / closed issue", Value: reportsOutcomeTokenValue(current.TokensPerClosedIssue, current.ClosedIssues), Detail: reportsOutcomeCount(current.ClosedIssues, "closed issue", "closed issues")},
				{ID: "spend-closed-issue", Label: "Notional USD / closed issue", Value: reportsOutcomeSpendValue(current.SpendPerClosedIssueUSD, current.ClosedIssues), Detail: reportsOutcomeCount(current.ClosedIssues, "closed issue", "closed issues")},
			},
		}
		if len(project.Trend) > 0 {
			item.HasTrend = true
			tokenPoints := make([]SplitSeriesPoint, 0, len(project.Trend))
			spendPoints := make([]SplitSeriesPoint, 0, len(project.Trend))
			for _, point := range project.Trend {
				label := reportsOutcomePointLabel(point.From, window)
				tokenPoints = append(tokenPoints, SplitSeriesPoint{Label: label, Input: point.Metrics.TokensPerMergedPR, Output: point.Metrics.TokensPerClosedIssue})
				spendPoints = append(spendPoints, SplitSeriesPoint{Label: label, Input: point.Metrics.SpendPerMergedPRUSD, Output: point.Metrics.SpendPerClosedIssueUSD})
			}
			item.TokenChart = SplitSeriesChartData{Title: name + " tokens per outcome", AriaLabel: name + " tokens per outcome trend", InputLabel: "Merged PR", OutputLabel: "Closed issue", Points: tokenPoints, ValueSuffix: " tokens", Class: "h-28 w-full"}
			item.SpendChart = SplitSeriesChartData{Title: name + " notional USD per outcome", AriaLabel: name + " notional USD per outcome trend", InputLabel: "Merged PR", OutputLabel: "Closed issue", Points: spendPoints, ValueSuffix: " notional USD", Class: "h-28 w-full", InputClass: "text-accent", OutputClass: "text-ok"}
		}
		view.Projects = append(view.Projects, item)
	}
	return view
}

func reportsOutcomeWindowHref(data ReportsData, window string) string {
	values := url.Values{"outcome_window": []string{window}}
	if value := strings.TrimSpace(data.ProjectID); value != "" {
		values.Set("project", value)
	}
	if value := strings.TrimSpace(data.Digest.Timezone); value != "" {
		values.Set("tz", value)
	}
	if value := strings.TrimSpace(data.Day.From); value != "" {
		values.Set("from", value)
	}
	if value := strings.TrimSpace(data.Day.To); value != "" {
		values.Set("to", value)
	}
	return "/reports?" + values.Encode() + "#reports-cost-outcomes"
}

func reportsOutcomeTokenValue(value float64, outcomes int64) string {
	if outcomes == 0 {
		return "—"
	}
	return fleetCompactTokens(int64(value))
}

func reportsOutcomeSpendValue(value float64, outcomes int64) string {
	if outcomes == 0 {
		return "—"
	}
	return formatUSD(value)
}

func reportsOutcomeCount(count int64, singular string, plural string) string {
	label := plural
	if count == 1 {
		label = singular
	}
	return formatInt(count) + " " + label
}

func reportsOutcomePointLabel(at time.Time, window string) string {
	if window == "24h" {
		return at.Format("15:04")
	}
	return at.Format("Jan 2")
}

func reportsOutcomeWindowClass(active bool) string {
	base := "rounded-card px-2.5 py-1.5 text-xs"
	if active {
		return base + " bg-elev font-medium text-text"
	}
	return base + " text-sec hover:text-text"
}

func reportsEfficiencyView(rollup efficiency.Rollup) reportsEfficiency {
	current := rollup.Current
	baseline := rollup.Baseline
	view := reportsEfficiency{
		Available: current.Issues > 0,
		Issues:    formatInt(current.Issues),
		Anomalies: formatInt(current.Anomalies),
		Rows: []reportsEfficiencyRow{
			{Label: "Tokens / merged issue · p50", Current: fleetCompactTokens(int64(current.TokensPerIssue.P50)), Baseline: fleetCompactTokens(int64(baseline.TokensPerIssue.P50)), Delta: reportsEfficiencyDelta(current.TokensPerIssue.P50, baseline.TokensPerIssue.P50)},
			{Label: "Tokens / merged issue · p90", Current: fleetCompactTokens(int64(current.TokensPerIssue.P90)), Baseline: fleetCompactTokens(int64(baseline.TokensPerIssue.P90)), Delta: reportsEfficiencyDelta(current.TokensPerIssue.P90, baseline.TokensPerIssue.P90)},
			{Label: "Notional USD / merged issue · p50", Current: formatUSD(current.CostPerIssueUSD.P50), Baseline: formatUSD(baseline.CostPerIssueUSD.P50), Delta: reportsEfficiencyDelta(current.CostPerIssueUSD.P50, baseline.CostPerIssueUSD.P50)},
			{Label: "Notional USD / merged issue · p90", Current: formatUSD(current.CostPerIssueUSD.P90), Baseline: formatUSD(baseline.CostPerIssueUSD.P90), Delta: reportsEfficiencyDelta(current.CostPerIssueUSD.P90, baseline.CostPerIssueUSD.P90)},
			{Label: "Cache share", Current: reportCacheReadFraction(current.CacheShare), Baseline: reportCacheReadFraction(baseline.CacheShare), Delta: reportsEfficiencyDelta(current.CacheShare, baseline.CacheShare)},
			{Label: "Sessions / issue", Current: formatDecimal(current.SessionsPerIssue), Baseline: formatDecimal(baseline.SessionsPerIssue), Delta: reportsEfficiencyDelta(current.SessionsPerIssue, baseline.SessionsPerIssue)},
			{Label: "First-attempt merge rate", Current: reportCacheReadFraction(current.FirstAttemptMergeRate), Baseline: reportCacheReadFraction(baseline.FirstAttemptMergeRate), Delta: reportsEfficiencyDelta(current.FirstAttemptMergeRate, baseline.FirstAttemptMergeRate)},
		},
		Dwell: []reportsEfficiencyDwell{
			{Label: "Working", Current: formatDuration(float64(current.Dwell.WorkingSeconds)), Baseline: formatDuration(float64(baseline.Dwell.WorkingSeconds))},
			{Label: "Gate wait", Current: formatDuration(float64(current.Dwell.GateWaitSeconds)), Baseline: formatDuration(float64(baseline.Dwell.GateWaitSeconds))},
			{Label: "Merge train", Current: formatDuration(float64(current.Dwell.MergeTrainSeconds)), Baseline: formatDuration(float64(baseline.Dwell.MergeTrainSeconds))},
			{Label: "Parked", Current: formatDuration(float64(current.Dwell.ParkedSeconds)), Baseline: formatDuration(float64(baseline.Dwell.ParkedSeconds))},
		},
	}
	if len(rollup.CacheTrend) > 0 {
		view.HasTrend = true
		points := make([]webchart.Point, 0, len(rollup.CacheTrend))
		for _, point := range rollup.CacheTrend {
			points = append(points, webchart.Point{Label: point.Day, Value: point.CacheShare * 100})
		}
		view.CacheChart = SeriesChartData{Title: "Cache share trend", AriaLabel: "Daily cache share trend", Points: points, Class: "h-24 w-full", ColorClass: "text-accent"}
	}
	return view
}

func reportsEfficiencyDelta(current float64, baseline float64) string {
	if baseline == 0 {
		return "new"
	}
	delta := ((current - baseline) / baseline) * 100
	if delta > 0 {
		return "+" + formatDecimal(delta) + "%"
	}
	return formatDecimal(delta) + "%"
}

func reportsDigestView(data DailyDigestData) reportsDigest {
	view := reportsDigest{Timezone: data.Timezone, Days: []reportsDigestDay{}}
	start := max(0, len(data.Days)-dailyDigestVisibleDayCount)
	for index := len(data.Days) - 1; index >= start; index-- {
		day := data.Days[index]
		baselineStart := max(0, index-dailyDigestBaselineDayCount)
		baseline := data.Days[baselineStart:index]
		view.Days = append(view.Days, reportsDigestDay{
			Date:     day.Date,
			Label:    reportsDigestDateLabel(day.Date, index == len(data.Days)-1),
			Today:    index == len(data.Days)-1,
			Metrics:  reportsDigestMetrics(day, baseline),
			Projects: day.Projects,
		})
	}
	return view
}

const (
	dailyDigestVisibleDayCount  = 7
	dailyDigestBaselineDayCount = 7
)

func reportsDigestMetrics(day DailyDigestDayData, baseline []DailyDigestDayData) []reportsDigestMetric {
	cacheShare := usageFraction(day.CachedInputTokens, day.InputTokens)
	failureRate := usageFraction(day.FailedSessions, day.Sessions)
	recoveries := day.OrphanResumed + day.OrphanFresh
	metrics := []reportsDigestMetric{
		reportsDigestCountMetric("shipped", "Shipped", day.IssuesShipped, baseline, func(value DailyDigestDayData) int64 { return value.IssuesShipped }),
		reportsDigestCountMetric("filed", "Filed", day.IssuesFiled, baseline, func(value DailyDigestDayData) int64 { return value.IssuesFiled }),
		reportsDigestCountMetric("releases", "Releases", day.ReleasesTagged, baseline, func(value DailyDigestDayData) int64 { return value.ReleasesTagged }),
		reportsDigestCountMetric("sessions", "Sessions", day.Sessions, baseline, func(value DailyDigestDayData) int64 { return value.Sessions }),
		reportsDigestCountMetric("tokens", "Tokens", day.TotalTokens, baseline, func(value DailyDigestDayData) int64 { return value.TotalTokens }),
		reportsDigestFloatMetric("cache", "Cached share", cacheShare, baseline, func(value DailyDigestDayData) float64 {
			return usageFraction(value.CachedInputTokens, value.InputTokens)
		}),
		reportsDigestFloatMetric("cost", "Estimated notional USD", day.SpendUSD, baseline, func(value DailyDigestDayData) float64 { return value.SpendUSD }),
		reportsDigestCountMetric("recoveries", "Orphan recoveries", recoveries, baseline, func(value DailyDigestDayData) int64 { return value.OrphanResumed + value.OrphanFresh }),
		reportsDigestCountMetric("outages", "Capacity outages", day.CapacityOutages, baseline, func(value DailyDigestDayData) int64 { return value.CapacityOutages }),
		reportsDigestCountMetric("breakers", "Breaker trips", day.BreakerTrips, baseline, func(value DailyDigestDayData) int64 { return value.BreakerTrips }),
		reportsDigestCountMetric("failures", "Failed sessions", day.FailedSessions, baseline, func(value DailyDigestDayData) int64 { return value.FailedSessions }),
		reportsDigestFloatMetric("tokens-per-merged", "Tokens / merged · p50", day.Efficiency.TokensPerIssue.P50, baseline, func(value DailyDigestDayData) float64 { return value.Efficiency.TokensPerIssue.P50 }),
		reportsDigestFloatMetric("cost-per-merged", "Notional USD / merged · p50", day.Efficiency.CostPerIssueUSD.P50, baseline, func(value DailyDigestDayData) float64 { return value.Efficiency.CostPerIssueUSD.P50 }),
		reportsDigestFloatMetric("sessions-per-issue", "Sessions / issue", day.Efficiency.SessionsPerIssue, baseline, func(value DailyDigestDayData) float64 { return value.Efficiency.SessionsPerIssue }),
		reportsDigestFloatMetric("first-attempt", "First-attempt merge", day.Efficiency.FirstAttemptMergeRate, baseline, func(value DailyDigestDayData) float64 { return value.Efficiency.FirstAttemptMergeRate }),
		reportsDigestCountMetric("efficiency-anomalies", "Efficiency anomalies", day.Efficiency.Anomalies, baseline, func(value DailyDigestDayData) int64 { return value.Efficiency.Anomalies }),
		reportsDigestCountMetric("dwell-working", "Working dwell", day.Efficiency.Dwell.WorkingSeconds, baseline, func(value DailyDigestDayData) int64 { return value.Efficiency.Dwell.WorkingSeconds }),
		reportsDigestCountMetric("dwell-gate", "Gate-wait dwell", day.Efficiency.Dwell.GateWaitSeconds, baseline, func(value DailyDigestDayData) int64 { return value.Efficiency.Dwell.GateWaitSeconds }),
		reportsDigestCountMetric("dwell-merge", "Merge-train dwell", day.Efficiency.Dwell.MergeTrainSeconds, baseline, func(value DailyDigestDayData) int64 { return value.Efficiency.Dwell.MergeTrainSeconds }),
		reportsDigestCountMetric("dwell-parked", "Parked dwell", day.Efficiency.Dwell.ParkedSeconds, baseline, func(value DailyDigestDayData) int64 { return value.Efficiency.Dwell.ParkedSeconds }),
	}
	metrics[4].Value = fleetCompactTokens(day.TotalTokens)
	metrics[4].Full = formatInt(day.TotalTokens)
	metrics[5].Value = reportCacheReadFraction(cacheShare)
	metrics[6].Value = formatUSD(day.SpendUSD)
	metrics[7].Detail = formatInt(day.OrphanResumed) + " reattached · " + formatInt(day.OrphanFresh) + " fresh"
	if day.CapacityOutages > 0 {
		metrics[8].Detail = formatDuration(float64(day.CapacitySeconds))
		if strings.TrimSpace(day.CapacityRecoveryMode) != "" {
			metrics[8].Detail += " · " + strings.ReplaceAll(day.CapacityRecoveryMode, "_", " ")
		}
	}
	if day.FailedSessions > 0 {
		metrics[10].Detail = reportCacheReadFraction(failureRate) + " of sessions"
		if strings.TrimSpace(day.DominantErrorClass) != "" {
			metrics[10].Detail += " · " + strings.ReplaceAll(day.DominantErrorClass, "_", " ")
		}
	}
	metrics[11].Value = fleetCompactTokens(int64(day.Efficiency.TokensPerIssue.P50))
	metrics[11].Detail = "p90 " + fleetCompactTokens(int64(day.Efficiency.TokensPerIssue.P90))
	metrics[12].Value = formatUSD(day.Efficiency.CostPerIssueUSD.P50)
	metrics[12].Detail = "p90 " + formatUSD(day.Efficiency.CostPerIssueUSD.P90)
	metrics[13].Value = formatDecimal(day.Efficiency.SessionsPerIssue)
	metrics[14].Value = reportCacheReadFraction(day.Efficiency.FirstAttemptMergeRate)
	for index := 16; index <= 19; index++ {
		metrics[index].Value = formatDuration(float64([]int64{
			day.Efficiency.Dwell.WorkingSeconds,
			day.Efficiency.Dwell.GateWaitSeconds,
			day.Efficiency.Dwell.MergeTrainSeconds,
			day.Efficiency.Dwell.ParkedSeconds,
		}[index-16]))
	}
	return metrics
}

func reportsDigestCountMetric(id string, label string, current int64, baseline []DailyDigestDayData, value func(DailyDigestDayData) int64) reportsDigestMetric {
	average := digestAverage(baseline, func(day DailyDigestDayData) float64 { return float64(value(day)) })
	delta, trend := reportsDigestDelta(float64(current), average)
	return reportsDigestMetric{ID: id, Label: label, Value: formatInt(current), Delta: delta, Trend: trend}
}

func reportsDigestFloatMetric(id string, label string, current float64, baseline []DailyDigestDayData, value func(DailyDigestDayData) float64) reportsDigestMetric {
	average := digestAverage(baseline, value)
	delta, trend := reportsDigestDelta(current, average)
	return reportsDigestMetric{ID: id, Label: label, Delta: delta, Trend: trend}
}

func digestAverage(days []DailyDigestDayData, value func(DailyDigestDayData) float64) float64 {
	if len(days) == 0 {
		return 0
	}
	total := 0.0
	for _, day := range days {
		total += value(day)
	}
	return total / float64(len(days))
}

func reportsDigestDelta(current float64, average float64) (string, string) {
	switch {
	case average == 0 && current == 0:
		return "at 7d avg", "flat"
	case average == 0:
		return "new vs 7d", "up"
	}
	change := (current - average) / average * 100
	if change > -0.5 && change < 0.5 {
		return "at 7d avg", "flat"
	}
	if change > 0 {
		return "+" + strconv.FormatFloat(change, 'f', 0, 64) + "% vs 7d", "up"
	}
	return strconv.FormatFloat(change, 'f', 0, 64) + "% vs 7d", "down"
}

func reportsDigestDateLabel(date string, today bool) string {
	parsed, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return date
	}
	label := parsed.Format("Mon, Jan 2")
	if today {
		return "Today · " + label
	}
	return label
}

func reportsDigestDeltaClass(trend string) string {
	switch trend {
	case "up":
		return "text-warn"
	case "down":
		return "text-ok"
	default:
		return "text-dim"
	}
}

func usageFraction(numerator int64, denominator int64) float64 {
	if numerator <= 0 || denominator <= 0 {
		return 0
	}
	return min(float64(numerator)/float64(denominator), 1)
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
		next = localTimeToken(*release.NextTriggerAt, LocalDateTime)
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
		{ID: "kpi-spend", Value: formatUSD(totals.SpendUSD), Label: "Total notional USD"},
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
			Title:       "Daily notional USD",
			AriaLabel:   "Daily notional USD bar chart",
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
		view.Detail = "Budget disabled — notional USD is tracked but not limited."
		return view
	case strings.TrimSpace(budget.DegradedReason) != "":
		view.State = "unavailable"
		view.Detail = "Budget data unavailable. " + strings.TrimSpace(budget.DegradedReason)
		return view
	case budget.CurrentSpendUSD <= 0 && len(budget.Days) == 0:
		view.State = "empty"
		view.Detail = "No notional USD yet."
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
			AriaLabel:   "Daily budget notional USD",
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
