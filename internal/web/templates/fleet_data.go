package templates

import (
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
	Figures    []primitives.Figure
	TPS        string
	Spend      string
	Uptime     string
	PRLanes    []fleetPRLane
	Metrics    fleetMetrics
}

// fleetAgentRow is one live agent in the hero panel — readable at arm's
// length: repo+issue, task title, elapsed, stage, progress, tps.
type fleetAgentRow struct {
	ID            string
	Repo          string
	Number        string
	Title         string
	Elapsed       string
	Stage         string
	Progress      int
	ProgressTitle string
	TPS           string
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
	Project string
	Meta    string
	Title   string
}

type fleetMetrics struct {
	SpendValue string
	SpendNote  string
	SpendPct   int
	SpendTitle string
	SpendWarn  bool
	HasSpend   bool

	TokensValue string
	TokenChart  SeriesChartData
	HasTokens   bool

	QuotaValue string
	QuotaPct   int
	QuotaWarn  bool
	HasQuota   bool
}

func fleetViewFromDashboard(data DashboardData) fleetView {
	snapshot := data.Snapshot
	// Fleet and project Overview render agent/PR content into #snapshot, not
	// a board, so their Review sheets omit inline board actions.
	view := fleetView{
		Exceptions: boardExceptions(data, false),
		Agents:     fleetAgentRows(snapshot),
		AgentCount: formatCount(runningCount(snapshot)) + " running",
		Figures:    boardFigures(snapshot),
		TPS:        throughputRate(snapshot),
		Spend:      formatUSD(snapshot.Budget.CurrentSpendUSD) + " today",
		Uptime:     "uptime " + runtimeLabel(snapshot),
		PRLanes:    fleetPRLanes(snapshot),
		Metrics:    fleetMetricsFromSnapshot(data),
	}
	if len(view.Exceptions) == 0 {
		view.AllClear = "All clear — nothing needs you. " + fleetAgentsWorkingLabel(runningCount(snapshot))
	}
	return view
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
		// Non-GitHub tracker identifiers (e.g. memory IDs like MT-1) have no
		// #number, so fall back to the full identifier to keep each agent
		// row's DOM id unique and stable for SSE morph/highlight targeting.
		idKey := number
		if strings.TrimSpace(idKey) == "" {
			idKey = repo
		}
		row := fleetAgentRow{
			ID:      "agent-" + boardCardSlug(running.ProjectID, idKey),
			Repo:    repo,
			Number:  number,
			Title:   issueTitle(running.Issue),
			Elapsed: fleetAgentElapsed(running),
			Stage:   fleetAgentStage(running),
			TPS:     fleetAgentTPS(running),
		}
		row.Progress, row.ProgressTitle = fleetAgentProgress(running, typical)
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

func fleetAgentProgress(running telemetry.Running, typical float64) (int, string) {
	if typical <= 0 || running.RuntimeSeconds <= 0 {
		return 0, ""
	}
	percent := int(running.RuntimeSeconds / typical * 100)
	if percent > 95 {
		percent = 95
	}
	if percent < 2 {
		percent = 2
	}
	return percent, "Elapsed vs typical session runtime (" + formatDuration(typical) + ")"
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
				DomID:   "pr-card-" + boardCardSlug(card.ProjectID, card.IssueNumber),
				Ref:     fleetPRCardRef(card),
				Project: strings.TrimSpace(card.ProjectID),
				Meta:    boardCompactAge(card.TimeInStage),
				Title:   card.Title,
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

	if points := tokenTrendPoints(snapshot); len(points) > 1 {
		metrics.HasTokens = true
		metrics.TokenChart = SeriesChartData{
			Title:      "Token throughput trend",
			AriaLabel:  "Rolling token throughput trend",
			Points:     throughputTrendPoints(snapshot),
			ColorClass: "text-sec",
			Class:      "h-6 w-full",
		}
	}
	metrics.TokensValue = fleetCompactTokens(snapshot.Tokens.Total)

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

func fleetAgentRowClass(last bool) string {
	base := "grid grid-cols-[minmax(0,1.6fr)_130px_150px_minmax(0,1fr)_90px] items-center gap-4 px-4 py-3.5"
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
