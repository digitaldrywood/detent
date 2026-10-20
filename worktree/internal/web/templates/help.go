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
	helpConcurrency         helpTerm = "concurrency"
	helpCreditsRateBucket   helpTerm = "credits-rate-bucket"
	helpCumulativeFlow      helpTerm = "cumulative-flow"
	helpCycleTime           helpTerm = "cycle-time"
	helpCurrentSpend        helpTerm = "current-spend"
	helpDailyCap            helpTerm = "daily-cap"
	helpDiff                helpTerm = "diff"
	helpEvent               helpTerm = "event"
	helpIssueCap            helpTerm = "issue-cap"
	helpLifetimeTotals      helpTerm = "lifetime-totals"
	helpModelBuckets        helpTerm = "model-buckets"
	helpPrimaryRateBucket   helpTerm = "primary-rate-bucket"
	helpProjectKanban       helpTerm = "project-kanban"
	helpProjectMultiples    helpTerm = "project-multiples"
	helpPRPipeline          helpTerm = "pr-pipeline"
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
	helpWorkflowMetrics     helpTerm = "workflow-metrics"
	helpRuntime             helpTerm = "runtime"
)

type helpEntry struct {
	Label       string
	Description string
}

var helpDefinitions = map[helpTerm]helpEntry{
	helpAgentActivity:       {Label: "Agent activity", Description: "A timeline of what running and recently completed agents are doing. Use it to spot stalls, bursts of work, or agents that are no longer moving."},
	helpAgeTurn:             {Label: "Runtime / turns", Description: "How long the session has been running and how many Codex turns it has completed. Older sessions with few turns may be waiting or stuck."},
	helpBackoffQueue:        {Label: "Retry and wait queue", Description: "Work waiting for CI or scheduled to retry after a failure, cooldown, or rate limit."},
	helpBlocked:             {Label: "Blocked", Description: "Issues Detent cannot continue without human help, such as missing dependencies, auth, approvals, or required actions."},
	helpBoardHealth:         {Label: "Board health", Description: "A snapshot of where tracked issues sit across the workflow. Watch it to see whether work is flowing or piling up in a state."},
	helpBudget:              {Label: "Budget", Description: "Optional notional USD guardrails derived from token pricing for the active project. If a cap is hit, Detent refuses to start new work until the limit is raised or resets."},
	helpBudgetHistory:       {Label: "Budget history", Description: "Recent daily notional USD for the project, computed from token pricing rather than provider invoices. Use it to see whether usage is trending normally before raising caps or starting more work."},
	helpCompleted:           {Label: "Completed", Description: "Sessions that finished and reported a final state. Compare it with Running and Queue to see whether agents are actually shipping work."},
	helpConcurrency:         {Label: "Hourly concurrency", Description: "Recorded work-attempt concurrency over the last 24 hours. Median and p90 expose sustained serialization that a brief maximum or the current running count can hide."},
	helpCreditsRateBucket:   {Label: "Credits rate bucket", Description: "How many Codex credits remain when the provider reports a credit balance. Watch it during heavy work because low credits can slow or stop agents."},
	helpCumulativeFlow:      {Label: "Session completions", Description: "How local completed-session count changes over recent snapshots. Compare it with current issue states to avoid mistaking session history for board status."},
	helpCycleTime:           {Label: "Cycle time", Description: "Completed issue duration from first recorded session start to latest successful completion in local run history."},
	helpCurrentSpend:        {Label: "Current notional USD", Description: "Pricing-table USD computed for the current budget window. It is not provider-billed spend under subscription auth."},
	helpDailyCap:            {Label: "Daily cap", Description: "The maximum notional USD this project may accumulate today. When active and reached, Detent stops starting new sessions for the day."},
	helpDiff:                {Label: "Diff", Description: "The current workspace change size in files and lines. Large or fast-growing diffs are a signal to review scope before the session drifts."},
	helpEvent:               {Label: "Event", Description: "The latest Codex status or message for the session. Use it to understand what the agent is doing without opening logs."},
	helpIssueCap:            {Label: "Issue cap", Description: "The maximum notional USD one issue or session may accumulate from token pricing. It prevents one difficult task from consuming the project budget."},
	helpLifetimeTotals:      {Label: "Lifetime totals", Description: "All-time local usage totals for tokens, sessions, runtime, runs, and orphan recovery. Resumed cache share measures cached input on the first resumed turns after restarts."},
	helpModelBuckets:        {Label: "Model split", Description: "Token usage grouped by model. It shows which models drive cost and volume when a report window looks expensive."},
	helpPrimaryRateBucket:   {Label: "Primary rate bucket", Description: "The main Codex API quota bucket for requests or tokens. When remaining capacity is low, agents may throttle until the reset."},
	helpProjectKanban:       {Label: "Project Kanban", Description: "Project board grouped by configured Detent workflow states. Empty configured lanes stay hidden unless pinned in lane visibility."},
	helpProjectMultiples:    {Label: "Projects", Description: "Compact per-project throughput, notional USD, and queue depth. Use it to compare active repositories sharing the same Detent process."},
	helpPRPipeline:          {Label: "PR pipeline", Description: "Pull requests currently waiting for human review, merging, or marked done by the tracker today. Watch this lane to see whether the merge train is moving."},
	helpProjectedSpend:      {Label: "Projected notional USD", Description: "Estimated pricing-table USD for active work if it continues at the current pace. It is not a provider invoice."},
	helpQueue:               {Label: "Queue", Description: "Issues waiting to start or retry. A growing queue means demand is higher than the available agent capacity or retry cooldowns."},
	helpRateLimits:          {Label: "Rate limits", Description: "Primary buckets show provider quota remaining and used before hourly reset. GitHub REST and GraphQL primary buckets are separate, and secondary REST backoff can still pause individual endpoint families."},
	helpRecentSessions:      {Label: "Recent sessions", Description: "The last completed Codex sessions Detent retained. Use it to audit what just happened without digging through logs."},
	helpReportsEvents:       {Label: "Usage events", Description: "Completed ledger rows in the selected report window. Low counts can make spend and token trends look sparse or misleading."},
	helpReportsModels:       {Label: "Models", Description: "How many model buckets appear in the report window. More buckets can explain mixed pricing or unexpected spend."},
	helpReportsSpend:        {Label: "Total notional USD", Description: "Total pricing-table USD computed for the selected report window. Under subscription auth this is not billed spend."},
	helpReportsTokens:       {Label: "Report tokens", Description: "Input plus output tokens recorded in the selected report window. Token volume explains most cost and throughput changes."},
	helpRunning:             {Label: "Running", Description: "Issues currently assigned to Codex and producing activity. Watch this to see how much work Detent is actively driving right now."},
	helpSecondaryRateBucket: {Label: "Secondary rate bucket", Description: "The burst-control quota bucket that can throttle even when primary quota remains. If it runs low, short spikes of parallel work may pause."},
	helpSession:             {Label: "Session", Description: "The Codex session ID tying dashboard rows to logs and copied references. Use it when investigating one agent's history."},
	helpSettingsConfig:      {Label: "Global config", Description: "The startup config Detent actually loaded. Check it when behavior does not match the file you expected to be active."},
	helpSettingsProjects:    {Label: "Projects", Description: "The project configs Detent loaded, including tracker wiring, priority, weight, and pause state. Use it to confirm the scheduler sees the right work."},
	helpSettingsRuntime:     {Label: "Runtime paths", Description: "The local files and listener address used by this Detent process. Check these when logs, database state, or the web server look wrong."},
	helpSpendTrend:          {Label: "Notional USD trend", Description: "Daily pricing-table USD across tracked projects. Use it to catch usage spikes before they trip configured brakes."},
	helpStateDistribution:   {Label: "Current issue states", Description: "Current issue counts by workflow state. Completed session history is shown separately so Done only reflects current tracked issue state."},
	helpThroughput:          {Label: "Token throughput", Description: "Tokens processed per second across running agents, shown as tps. It is a live pulse of how hard Detent is working right now."},
	helpTokenTrend:          {Label: "Token trend", Description: "Input and output token totals over time. Use it to see whether agents are reading context, generating large changes, or quieting down."},
	helpTokens:              {Label: "Tokens", Description: "Input plus output tokens used by running or completed work. High token counts usually mean higher cost and longer sessions."},
	helpTopIssues:           {Label: "Top issues by tokens", Description: "Issue buckets using the most tokens in the report window. Start here when one task seems to dominate usage."},
	helpTopPRs:              {Label: "Top PRs by tokens", Description: "Pull request buckets using the most tokens in the report window. Use it to find review or rework loops that are getting expensive."},
	helpTopProjects:         {Label: "Per-project breakdown", Description: "Token and notional USD split by project. It shows which project is driving shared capacity and budget pressure."},
	helpWorkflowMetrics:     {Label: "Workflow metrics", Description: "Lane and sub-phase timing from durable runtime events plus live card aging. Use it to identify whether time is going to AI work, CI, merge queue, review, or rate limits."},
	helpRuntime:             {Label: "Runtime", Description: "How long this Detent instance or agent session has been running. Long runtimes are worth checking when progress slows."},
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
