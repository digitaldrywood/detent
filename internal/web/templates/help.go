package templates

import (
	"strings"
	"unicode"
)

type helpTerm string

const (
	helpAgentActivity       helpTerm = "agent-activity"
	helpAgeTurn             helpTerm = "age-turn"
	helpBackoffQueue        helpTerm = "backoff-queue"
	helpBlocked             helpTerm = "blocked"
	helpBoardHealth         helpTerm = "board-health"
	helpBudget              helpTerm = "budget"
	helpBudgetHistory       helpTerm = "budget-history"
	helpCompleted           helpTerm = "completed"
	helpCreditsRateBucket   helpTerm = "credits-rate-bucket"
	helpCumulativeFlow      helpTerm = "cumulative-flow"
	helpCurrentSpend        helpTerm = "current-spend"
	helpDailyCap            helpTerm = "daily-cap"
	helpDiff                helpTerm = "diff"
	helpEvent               helpTerm = "event"
	helpIssueCap            helpTerm = "issue-cap"
	helpLifetimeTotals      helpTerm = "lifetime-totals"
	helpModelBuckets        helpTerm = "model-buckets"
	helpPrimaryRateBucket   helpTerm = "primary-rate-bucket"
	helpProjectedSpend      helpTerm = "projected-spend"
	helpQueue               helpTerm = "queue"
	helpRateLimits          helpTerm = "rate-limits"
	helpRecentSessions      helpTerm = "recent-sessions"
	helpReportsEvents       helpTerm = "reports-events"
	helpReportsModels       helpTerm = "reports-models"
	helpReportsSpend        helpTerm = "reports-spend"
	helpReportsTokens       helpTerm = "reports-tokens"
	helpRunning             helpTerm = "running"
	helpSecondaryRateBucket helpTerm = "secondary-rate-bucket"
	helpSession             helpTerm = "session"
	helpSettingsConfig      helpTerm = "settings-config"
	helpSettingsProjects    helpTerm = "settings-projects"
	helpSettingsRuntime     helpTerm = "settings-runtime"
	helpSpendTrend          helpTerm = "spend-trend"
	helpStateDistribution   helpTerm = "state-distribution"
	helpThroughput          helpTerm = "throughput"
	helpTokenTrend          helpTerm = "token-trend"
	helpTokens              helpTerm = "tokens"
	helpTopIssues           helpTerm = "top-issues"
	helpTopPRs              helpTerm = "top-prs"
	helpTopProjects         helpTerm = "top-projects"
	helpRuntime             helpTerm = "runtime"
)

type helpEntry struct {
	Label       string
	Description string
}

var helpDefinitions = map[helpTerm]helpEntry{
	helpAgentActivity:       {Label: "Agent activity", Description: "Timeline of running and recently completed Codex sessions in the latest snapshot."},
	helpAgeTurn:             {Label: "Runtime / turns", Description: "Elapsed session runtime paired with the number of completed Codex turns."},
	helpBackoffQueue:        {Label: "Backoff queue", Description: "Retries delayed after rate limits, transient errors, or orchestration failures. Due time is when retry becomes eligible."},
	helpBlocked:             {Label: "Blocked", Description: "Sessions paused because Codex needs operator input, approval, or an external prerequisite."},
	helpBoardHealth:         {Label: "Board health", Description: "Distribution and completion progress for the tracked project states in the latest snapshot."},
	helpBudget:              {Label: "Budget", Description: "Spend controls and current usage for the active project. Monetary values are in USD."},
	helpBudgetHistory:       {Label: "Budget history", Description: "Recent daily budget spend recorded for the active project. Units: USD."},
	helpCompleted:           {Label: "Completed", Description: "Sessions that finished and emitted a final state in the latest snapshot."},
	helpCreditsRateBucket:   {Label: "Credits rate bucket", Description: "Credit balance or credit bucket state from the Codex rate-limit snapshot."},
	helpCumulativeFlow:      {Label: "Cumulative flow", Description: "Completed issue count over recent snapshots, shown as issues over time."},
	helpCurrentSpend:        {Label: "Current spend", Description: "Usage cost already recorded for the current budget window. Units: USD."},
	helpDailyCap:            {Label: "Daily cap", Description: "Maximum spend allowed for a project during the current day. Units: USD."},
	helpDiff:                {Label: "Diff", Description: "Workspace diff summary: added lines, removed lines, and changed files."},
	helpEvent:               {Label: "Event", Description: "Most recent Codex event or status message observed for the session."},
	helpIssueCap:            {Label: "Issue cap", Description: "Maximum spend allowed for one issue or session. Units: USD."},
	helpLifetimeTotals:      {Label: "Lifetime totals", Description: "All-time usage totals from the local ledger, including tokens, sessions, runtime, and runs."},
	helpModelBuckets:        {Label: "Model split", Description: "Token distribution grouped by model name from recorded usage events."},
	helpPrimaryRateBucket:   {Label: "Primary rate bucket", Description: "Primary API request bucket, shown as remaining, used, limit, and reset when known."},
	helpProjectedSpend:      {Label: "Projected spend", Description: "Estimated additional cost for active work. Units: USD."},
	helpQueue:               {Label: "Queue", Description: "Issue sessions waiting to start or retry. The count is queued items, not running agents."},
	helpRateLimits:          {Label: "Rate limits", Description: "Latest Codex API rate-limit snapshot by bucket, including remaining, used, limit, and reset."},
	helpRecentSessions:      {Label: "Recent sessions", Description: "Most recent completed Codex sessions retained in the telemetry snapshot."},
	helpReportsEvents:       {Label: "Usage events", Description: "Completed ledger rows recorded for the selected report window."},
	helpReportsModels:       {Label: "Models", Description: "Distinct model buckets present in the selected report window."},
	helpReportsSpend:        {Label: "Total spend", Description: "Total usage cost for the selected report window. Units: USD."},
	helpReportsTokens:       {Label: "Report tokens", Description: "Input plus output model tokens recorded in the selected report window. Units: tokens."},
	helpRunning:             {Label: "Running", Description: "Issue sessions currently assigned to Codex and actively producing events."},
	helpSecondaryRateBucket: {Label: "Secondary rate bucket", Description: "Secondary burst-control bucket, shown as remaining, used, limit, and reset when known."},
	helpSession:             {Label: "Session", Description: "Codex session identifier used to correlate logs, dashboard rows, and copied IDs."},
	helpSettingsConfig:      {Label: "Global config", Description: "Resolved startup configuration path and discovery rule for this Symphony server."},
	helpSettingsProjects:    {Label: "Projects", Description: "Loaded project configurations, tracker wiring, weights, priority, and pause state."},
	helpSettingsRuntime:     {Label: "Runtime paths", Description: "Local runtime files and listener address used by the running Symphony server."},
	helpSpendTrend:          {Label: "Spend trend", Description: "Daily usage cost across tracked projects. Units: USD."},
	helpStateDistribution:   {Label: "State distribution", Description: "Current issue counts by workflow state in the tracked project."},
	helpThroughput:          {Label: "Token throughput", Description: "Rolling token processing rate. Units: tps, tokens per second."},
	helpTokenTrend:          {Label: "Token trend", Description: "Input and output token totals over time. Units: tokens."},
	helpTokens:              {Label: "Tokens", Description: "Input plus output model tokens counted for running or completed usage. Units: tokens."},
	helpTopIssues:           {Label: "Top issues by tokens", Description: "Issue buckets with the highest recorded token usage in the selected report window."},
	helpTopPRs:              {Label: "Top PRs by tokens", Description: "Pull request buckets with the highest recorded token usage in the selected report window."},
	helpTopProjects:         {Label: "Per-project breakdown", Description: "Token and spend distribution across tracked projects in the selected report window."},
	helpRuntime:             {Label: "Runtime", Description: "Elapsed agent execution time. Values are aggregate or per-session durations."},
}

func helpLabel(term helpTerm) string {
	return helpDefinitions[term].Label
}

func helpDescription(term helpTerm) string {
	return helpDefinitions[term].Description
}

func hasHelp(term helpTerm) bool {
	_, ok := helpDefinitions[term]
	return ok
}

func helpID(term helpTerm, scope string) string {
	return "help-" + helpIDPart(scope) + "-" + helpIDPart(string(term))
}

func helpIDPart(value string) string {
	var builder strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if builder.Len() > 0 && !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}

	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "term"
	}
	return result
}

func rateLimitHelpTerm(name string) helpTerm {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "primary":
		return helpPrimaryRateBucket
	case "secondary":
		return helpSecondaryRateBucket
	case "credits":
		return helpCreditsRateBucket
	default:
		return helpRateLimits
	}
}
