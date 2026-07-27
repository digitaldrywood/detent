package templates

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

// fleetView is the demoted ops view: what are the agents doing right now.
// History and trends belong to Reports; this page is for the present.
type fleetView struct {
	Exceptions []primitives.Exception
	AllClear   string
	Agents     []fleetAgentRow
	AgentCount string
	AgentPools []fleetAgentPool
	Figures    []primitives.Figure
	TPS        string
	Spend      string
	Uptime     string
	Merge      prPipelineMergeMetrics
	PRLanes    []fleetPRLane
	Metrics    fleetMetrics
}

// fleetAgentRow is one live agent in the hero panel — readable at arm's
// length: repo+issue, task title, elapsed, stage, progress, tps.
type fleetAgentRow struct {
	ID            string
	Repo          string
	Number        string
	URL           string
	Title         string
	Elapsed       string
	Stage         string
	Progress      int
	ProgressKind  primitives.Kind
	ProgressTitle string
	Telemetry     string
	StopPath      string
}

type fleetAgentPool struct {
	ID         string
	Name       string
	Usage      string
	Saturated  bool
	Draining   bool
	StatusKind primitives.Kind
}

type fleetPRLane struct {
	ID    string
	Title string
	Count string
	Empty string
	Cards []fleetPRCard
}

type fleetPRCard struct {
	DomID   string
	Ref     string
	URL     string
	Project string
	Meta    string
	Title   string
	Status  string
}

type fleetMetrics struct {
	SpendValue string
	SpendNote  string
	SpendPct   int
	SpendTitle string
	SpendWarn  bool
	HasSpend   bool

	TokensValue  string
	TokenChart   SeriesChartData
	HasTokens    bool
	ContextValue string
	ContextRef   string
	ContextURL   string
	ContextTitle string
	ContextPct   int
	ContextKind  primitives.Kind
	HasContext   bool

	QuotaValue string
	QuotaPct   int
	QuotaWarn  bool
	HasQuota   bool
}

func fleetViewFromDashboard(data DashboardData) fleetView {
	snapshot := data.Snapshot
	agentPools := fleetAgentPools(data)
	// Fleet and project Overview render agent/PR content into #snapshot, not
	// a board, so their Review sheets omit inline board actions.
	view := fleetView{
		Exceptions: boardExceptions(data, false),
		Agents:     fleetAgentRows(snapshot),
		AgentCount: fleetAgentCount(data, agentPools),
		AgentPools: agentPools,
		Figures:    boardFigures(snapshot),
		TPS:        throughputRate(snapshot),
		Spend:      formatUSD(snapshot.Budget.CurrentSpendUSD) + " today",
		Uptime:     "uptime " + runtimeLabel(snapshot),
		Merge:      prPipelineMergeSummary(snapshot),
		PRLanes:    fleetPRLanes(snapshot),
		Metrics:    fleetMetricsFromSnapshot(data),
	}
	if budgetEnrichmentPending(data) {
		view.Spend = "— today"
		view.Metrics.SpendValue = "—"
		view.Metrics.SpendNote = "Loading budget data."
		view.Metrics.SpendPct = 0
		view.Metrics.SpendTitle = ""
		view.Metrics.SpendWarn = false
		view.Metrics.HasSpend = false
	}
	if len(view.Exceptions) == 0 {
		if snapshotDegraded(snapshot) {
			// Body renders on degraded-with-prior-data, so the reassurance
			// line must flag that the tracker data is stale rather than
			// claim nothing needs attention.
			view.AllClear = "Showing last-known data — tracker refresh is degraded."
		} else {
			view.AllClear = "All clear — nothing needs you. " + fleetAgentsWorkingLabel(runningCount(snapshot))
		}
	}
	return view
}

func fleetAgentPools(data DashboardData) []fleetAgentPool {
	projectPool := ""
	if strings.TrimSpace(data.ProjectID) != "" {
		projectPool = strings.TrimSpace(data.Snapshot.Project.Pool)
		if projectPool == "" {
			projectPool = "default"
		}
	}

	pools := make([]fleetAgentPool, 0, len(data.Snapshot.AgentPools))
	for _, pool := range data.Snapshot.AgentPools {
		name := strings.TrimSpace(pool.Name)
		if name == "" || pool.Capacity <= 0 || projectPool != "" && name != projectPool {
			continue
		}
		saturated := pool.Used >= pool.Capacity
		kind := primitives.KindOK
		if saturated {
			kind = primitives.KindErr
		}
		id := "agent-pool-" + boardCardSlug(name)
		if pool.Generation > 0 {
			id += "-" + strconv.FormatUint(pool.Generation, 10)
		}
		pools = append(pools, fleetAgentPool{
			ID:         id,
			Name:       name,
			Usage:      formatCount(pool.Used) + " / " + formatCount(pool.Capacity),
			Saturated:  saturated,
			Draining:   pool.Draining,
			StatusKind: kind,
		})
	}
	sort.SliceStable(pools, func(i, j int) bool {
		if pools[i].Name == "default" {
			return pools[j].Name != "default"
		}
		if pools[j].Name == "default" {
			return false
		}
		return pools[i].Name < pools[j].Name
	})
	return pools
}

func fleetAgentCount(data DashboardData, pools []fleetAgentPool) string {
	running := formatCount(runningCount(data.Snapshot)) + " running"
	if len(pools) != 1 {
		return running
	}
	pool := pools[0]
	if strings.TrimSpace(data.ProjectID) == "" && pool.Name == "default" && !pool.Draining {
		return running + " · " + pool.Usage + " capacity"
	}
	if strings.TrimSpace(data.ProjectID) != "" && pool.Name == "default" && !pool.Draining {
		return running + " · default " + pool.Usage
	}
	return running
}

func fleetAgentPoolClass(pool fleetAgentPool) string {
	base := "flex min-w-0 items-center gap-1.5 rounded-chip border px-2 py-1 font-mono text-2xs font-medium tabular-nums "
	if pool.Saturated {
		return base + "border-err/40 bg-err/10 text-err"
	}
	if pool.Draining {
		return base + "border-warn/40 bg-warn/10 text-warn"
	}
	return base + "border-line bg-elev text-sec"
}

func budgetEnrichmentPending(data DashboardData) bool {
	if !data.PendingEnrichment {
		return false
	}
	projectID := strings.TrimSpace(data.ProjectID)
	for _, project := range data.Projects {
		if projectID != "" && strings.TrimSpace(project.ID) != projectID {
			continue
		}
		if project.BudgetEnabled {
			return true
		}
	}
	return false
}

func fleetAgentsWorkingLabel(running int) string {
	switch running {
	case 0:
		return "No agents working."
	case 1:
		return "1 agent working."
	}
	return formatCount(running) + " agents working."
}

func fleetAgentRows(snapshot telemetry.Snapshot) []fleetAgentRow {
	typical := fleetTypicalRuntimeSeconds(snapshot)
	rows := make([]fleetAgentRow, 0, len(snapshot.Running))
	for _, running := range snapshot.Running {
		repo, number := splitIssueIdentifier(issueIdentifier(running.Issue))
		identity := boardCardIdentityToken(running.Identifier, running.ID, projectKanbanIssueNumber(running.Issue))
		row := fleetAgentRow{
			ID:        "agent-" + boardCardScopedSlug(running.ProjectID, identity),
			Repo:      repo,
			Number:    number,
			URL:       issueURL(running.Issue),
			Title:     issueTitle(running.Issue),
			Elapsed:   fleetAgentElapsed(running),
			Stage:     fleetAgentStage(running),
			Telemetry: fleetAgentTelemetry(running),
			StopPath:  StopRunDialogPath(running),
		}
		row.Progress, row.ProgressTitle, row.ProgressKind = fleetAgentProgress(running, typical)
		rows = append(rows, row)
	}
	return rows
}

func splitIssueIdentifier(identifier string) (string, string) {
	index := strings.LastIndex(identifier, "#")
	if index <= 0 || index >= len(identifier)-1 {
		return identifier, ""
	}
	return identifier[:index], identifier[index:]
}

func fleetAgentElapsed(running telemetry.Running) string {
	elapsed := formatDuration(running.RuntimeSeconds)
	if running.TurnCount == 1 {
		return elapsed + " · 1 turn"
	}
	return elapsed + " · " + strconv.Itoa(running.TurnCount) + " turns"
}

func fleetAgentStage(running telemetry.Running) string {
	if stage := strings.TrimSpace(running.LastEvent); stage != "" {
		return stage
	}
	return "working"
}

func fleetAgentTPS(running telemetry.Running) string {
	if running.Tokens.Total <= 0 || running.Tokens.RuntimeSeconds <= 0 {
		return ""
	}
	return formatDecimal(float64(running.Tokens.Total)/running.Tokens.RuntimeSeconds) + " tps"
}

func fleetAgentTelemetry(running telemetry.Running) string {
	parts := make([]string, 0, 2)
	if pressure, ok := running.Tokens.ContextPressure(); ok {
		parts = append(parts, "ctx "+formatContextPercent(pressure.PercentUsed))
	}
	if fraction, ok := running.Tokens.CacheReadFraction(); ok {
		parts = append(parts, "cache "+formatContextPercent(fraction*100))
	}
	if len(parts) > 0 {
		return strings.Join(parts, " · ")
	}
	return fleetAgentTPS(running)
}

// fleetTypicalRuntimeSeconds is the mean completed-session runtime, used
// as the honest denominator for the hero progress bar: how far along this
// session is versus a typical one, capped below 100%.
func fleetTypicalRuntimeSeconds(snapshot telemetry.Snapshot) float64 {
	total := 0.0
	count := 0
	for _, completed := range snapshot.Completed {
		if completed.RuntimeSeconds > 0 {
			total += completed.RuntimeSeconds
			count++
		}
	}
	if count > 0 {
		return total / float64(count)
	}
	if snapshot.LifetimeTotals.Sessions > 0 && snapshot.LifetimeTotals.RuntimeSeconds > 0 {
		return float64(snapshot.LifetimeTotals.RuntimeSeconds) / float64(snapshot.LifetimeTotals.Sessions)
	}
	return 0
}

func fleetAgentProgress(running telemetry.Running, typical float64) (int, string, primitives.Kind) {
	if pressure, ok := running.Tokens.ContextPressure(); ok {
		percent := int(math.Round(pressure.PercentUsed))
		if percent > 100 {
			percent = 100
		}
		if percent < 1 && pressure.PercentUsed > 0 {
			percent = 1
		}
		return percent, contextPressureTitle(running.Tokens), contextPressureStateKind(pressure.ThresholdState)
	}
	if typical <= 0 || running.RuntimeSeconds <= 0 {
		return 0, "", primitives.KindOK
	}
	percent := int(running.RuntimeSeconds / typical * 100)
	if percent > 95 {
		percent = 95
	}
	if percent < 2 {
		percent = 2
	}
	return percent, "Elapsed vs typical session runtime (" + formatDuration(typical) + ")", primitives.KindOK
}

type fleetContextPressure struct {
	Issue    telemetry.Issue
	Pressure telemetry.ContextPressure
	Tokens   telemetry.Tokens
}

func fleetPRLanes(snapshot telemetry.Snapshot) []fleetPRLane {
	lanes := prPipelineLanes(snapshot)
	views := make([]fleetPRLane, 0, len(lanes))
	for _, lane := range lanes {
		view := fleetPRLane{
			ID:    "pr-lane-" + lane.ID,
			Title: lane.Title,
			Count: lane.CountLabel,
			Empty: fleetPRLaneEmpty(lane.ID),
		}
		for _, card := range lane.Cards {
			view.Cards = append(view.Cards, fleetPRCard{
				DomID:   "pr-card-" + boardCardScopedSlug(card.ProjectID, card.IdentityToken),
				Ref:     fleetPRCardRef(card),
				URL:     fleetPRCardURL(card),
				Project: strings.TrimSpace(card.ProjectID),
				Meta:    boardCompactAge(card.TimeInStage),
				Title:   card.Title,
				Status:  card.MergeLaneStatus,
			})
		}
		views = append(views, view)
	}
	return views
}

func fleetPRLaneEmpty(id string) string {
	switch id {
	case "human-review":
		return "No PRs in review"
	case "merging":
		return "Nothing is merging"
	case "done-today":
		return "No PRs merged today"
	}
	return "Empty"
}

func fleetPRCardRef(card prPipelineCard) string {
	if number := strings.TrimSpace(card.Identity.PullRequestLabel); number != "" {
		return number
	}
	return card.IssueNumber
}

func fleetPRCardURL(card prPipelineCard) string {
	if strings.TrimSpace(card.Identity.PullRequestLabel) != "" {
		return card.Identity.PullRequestURL
	}
	return card.Identity.IssueURL
}

func fleetMetricsFromSnapshot(data DashboardData) fleetMetrics {
	snapshot := data.Snapshot
	metrics := fleetMetrics{}

	budget := snapshot.Budget
	switch {
	case strings.TrimSpace(budget.DegradedReason) != "":
		metrics.SpendValue = "—"
		metrics.SpendNote = "Budget data unavailable."
	case !budget.Enabled:
		metrics.SpendValue = formatUSD(budget.CurrentSpendUSD)
		metrics.SpendNote = "Budget disabled — enable a daily cap in configuration."
	default:
		metrics.SpendValue = formatUSD(budget.CurrentSpendUSD)
		if cap := budget.PerDayMaxUSD; cap != nil && *cap > 0 {
			metrics.HasSpend = true
			percent := int(budget.CurrentSpendUSD / *cap * 100)
			if percent > 100 {
				percent = 100
			}
			metrics.SpendPct = percent
			metrics.SpendWarn = percent >= 90
			metrics.SpendTitle = formatUSD(budget.CurrentSpendUSD) + " of " + formatUSD(*cap) + " daily budget"
			metrics.SpendValue += " / " + formatUSD(*cap)
		}
		if budget.CurrentSpendUSD <= 0 && len(budget.Days) == 0 {
			metrics.SpendNote = "No budget spend yet."
		}
	}

	// Gate on the points the chart actually plots (throughputTrendPoints
	// filters zero/negative deltas and invalid timestamps), so the meter
	// never renders an empty SVG when the raw trend has no usable throughput.
	if points := throughputTrendPoints(snapshot); len(points) > 1 {
		metrics.HasTokens = true
		metrics.TokenChart = SeriesChartData{
			Title:      "Token throughput trend",
			AriaLabel:  "Rolling token throughput trend",
			Points:     points,
			ColorClass: "text-sec",
			Class:      "h-6 w-full",
		}
	}
	metrics.TokensValue = fleetCompactTokens(snapshot.Tokens.Total)
	if pressure, ok := fleetHighestContextPressure(snapshot); ok {
		metrics.HasContext = true
		metrics.ContextValue = formatContextPercent(pressure.Pressure.PercentUsed)
		metrics.ContextRef = contextPressureIssueLabel(pressure.Issue)
		metrics.ContextURL = issueURL(pressure.Issue)
		metrics.ContextTitle = contextPressureTitle(pressure.Tokens)
		metrics.ContextPct = int(math.Round(pressure.Pressure.PercentUsed))
		if metrics.ContextPct > 100 {
			metrics.ContextPct = 100
		}
		if metrics.ContextPct < 1 && pressure.Pressure.PercentUsed > 0 {
			metrics.ContextPct = 1
		}
		metrics.ContextKind = contextPressureStateKind(pressure.Pressure.ThresholdState)
	}

	if snapshot.RateLimits == nil {
		return metrics
	}
	if bucket := snapshot.RateLimits.GitHubREST; bucket != nil && bucket.Limit > 0 {
		metrics.HasQuota = true
		used := bucket.Limit - bucket.Remaining
		percent := int(float64(used) / float64(bucket.Limit) * 100)
		if percent < 0 {
			percent = 0
		}
		if percent > 100 {
			percent = 100
		}
		metrics.QuotaPct = percent
		metrics.QuotaWarn = percent >= 90
		metrics.QuotaValue = formatInt(used) + " / " + formatInt(bucket.Limit)
	}
	return metrics
}

func fleetHighestContextPressure(snapshot telemetry.Snapshot) (fleetContextPressure, bool) {
	var out fleetContextPressure
	found := false
	for _, running := range snapshot.Running {
		pressure, ok := running.Tokens.ContextPressure()
		if !ok {
			continue
		}
		if !found || pressure.PercentUsed > out.Pressure.PercentUsed {
			out = fleetContextPressure{Issue: running.Issue, Pressure: pressure, Tokens: running.Tokens}
			found = true
		}
	}
	return out, found
}

func contextPressureIssueLabel(issue telemetry.Issue) string {
	if number := strings.TrimSpace(projectKanbanIssueNumber(issue)); number != "" {
		return number
	}
	if identifier := strings.TrimSpace(issueIdentifier(issue)); identifier != "" {
		return identifier
	}
	return "active"
}

// fleetCompactTokens abbreviates large token counts (4,828,240,151 → 4.83B)
// so metric values never wrap; the full figure belongs to Reports.
func fleetCompactTokens(total int64) string {
	switch {
	case total >= 1_000_000_000:
		return strconv.FormatFloat(float64(total)/1_000_000_000, 'f', 2, 64) + "B"
	case total >= 1_000_000:
		return strconv.FormatFloat(float64(total)/1_000_000, 'f', 1, 64) + "M"
	case total >= 1_000:
		return strconv.FormatFloat(float64(total)/1_000, 'f', 1, 64) + "K"
	}
	return strconv.FormatInt(total, 10)
}

func fleetAgentRowClass(last bool, stoppable bool) string {
	base := "grid min-w-0 grid-cols-1 items-stretch gap-2.5 px-4 py-3.5 md:grid-cols-[minmax(0,1.6fr)_130px_150px_minmax(0,1fr)_90px] md:items-center md:gap-4"
	if stoppable {
		base = "grid min-w-0 grid-cols-1 items-stretch gap-2.5 px-4 py-3.5 md:grid-cols-[minmax(0,1.6fr)_130px_150px_minmax(0,1fr)_90px_36px] md:items-center md:gap-4"
	}
	if !last {
		return base + " border-b border-line"
	}
	return base
}

func fleetProgressStyle(percent int) string {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	return "width: " + strconv.Itoa(percent) + "%"
}

func fleetMeterClass(warn bool) string {
	if warn {
		return "h-full bg-warn"
	}
	return "h-full bg-sec"
}

func FleetShellDataFromDashboard(data DashboardData) DashboardShellData {
	shell := DashboardShellDataFromDashboard(data)
	shell.ActiveNav = "fleet"
	shell.IncludeDashboardCharts = false
	return shell
}
