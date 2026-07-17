package templates

import (
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

// healthView renders the whole Health page: one verdict sentence, one
// details table, one footnote. An at-rest system reads as one line.
type healthView struct {
	Kind           primitives.Kind
	Verdict        string
	Detail         string
	DetailAt       time.Time
	DetailRelative bool
	CheckedAt      time.Time
	Rows           []healthRow
	Footnote       string
}

type healthRow struct {
	ID        string
	Component string
	Kind      primitives.Kind
	Status    string
	Detail    string
	Quota     string
	QuotaPct  int
	QuotaWarn bool
	Resets    string
	ResetAt   time.Time
	DetailAt  time.Time
}

func healthViewFromDashboard(data DashboardData) healthView {
	snapshot := data.Snapshot
	api := gitHubAPIHealth(snapshot)
	view := healthView{
		CheckedAt: snapshot.GeneratedAt,
		Footnote:  "GitHub quota bars turn amber at 90%; project budget bars warn at 80% and turn red at the cap.",
		Rows:      append(healthRows(snapshot), healthBudgetRows(data)...),
	}
	outages := backendCapacityOutages(snapshot.BackendOutages)
	if len(outages) > 0 {
		view.Kind = primitives.KindWarn
		view.Verdict = backendCapacityHealthVerdict(snapshot)
		view.Detail, view.DetailAt, view.DetailRelative = backendCapacityOutageDetailParts(outages[0], snapshot.GeneratedAt)
		return view
	}
	if refreshFreshnessKind(snapshot) == primitives.KindWarn {
		view.Kind = primitives.KindWarn
		view.Verdict = "Tracker data is stale."
		view.Detail = refreshStaleBannerDetail(snapshot)
		return view
	}
	switch api.State {
	case gitHubAPIHealthStateWarning:
		view.Kind = primitives.KindWarn
		view.Verdict = api.Label + "."
		view.Detail = api.Detail
		return view
	case gitHubAPIHealthStateBackoff, gitHubAPIHealthStateExhausted:
		view.Kind = primitives.KindErr
		view.Verdict = api.Label + "."
		view.Detail = api.Detail
		return view
	}
	if summary, ok := boardFailureBreakerSummary(snapshot.FailureBreakers); ok {
		view.Kind = primitives.KindWarn
		view.Verdict = summary.Title + "."
		view.Detail = "Dispatch remains paused until a canary succeeds."
		return view
	}
	if summaries := healthDispatchRecoverySummaries(snapshot.DispatchRecoveries); len(summaries) > 0 {
		details := make([]string, 0, len(summaries))
		for _, summary := range summaries {
			details = append(details, summary.Title)
		}
		view.Kind = primitives.KindWarn
		view.Verdict = "Dispatch is waiting on capacity."
		view.Detail = strings.Join(details, "; ") + "."
		return view
	}
	switch api.State {
	case gitHubAPIHealthStateHealthy, gitHubAPIHealthStateAtRest:
		view.Kind = primitives.KindOK
		view.Verdict = "All systems nominal."
		view.Detail = api.Summary
	default:
		view.Kind = primitives.KindNeutral
		view.Verdict = "Waiting for the first health snapshot."
		view.Detail = api.Detail
	}
	return view
}

func healthBudgetRows(data DashboardData) []healthRow {
	rows := make([]healthRow, 0, len(data.Projects))
	for _, project := range data.Projects {
		if !project.BudgetEnabled || project.PerDayMaxUSD <= 0 {
			continue
		}
		percent := int(project.CurrentSpendUSD / project.PerDayMaxUSD * 100)
		if percent < 0 {
			percent = 0
		}
		kind := primitives.KindOK
		status := "Healthy"
		if percent >= 100 {
			kind = primitives.KindErr
			status = "At limit"
		} else if percent >= 80 {
			kind = primitives.KindWarn
			status = "Approaching limit"
		}
		detail := formatUSD(project.CurrentSpendUSD) + " spent today"
		if project.BudgetOverride != nil {
			observedAt := project.BudgetObservedAt
			if observedAt.IsZero() {
				observedAt = data.Snapshot.GeneratedAt
			}
			remaining := project.BudgetOverride.ExpiresAt.Sub(observedAt).Round(time.Second)
			if remaining < 0 {
				remaining = 0
			}
			detail += " · override daily " + optionalUSD(project.BudgetOverride.PerDayMaxUSD) + " / issue " + optionalUSD(project.BudgetOverride.PerIssueMaxUSD) + " expires in " + remaining.String() + " at " + project.BudgetOverride.ExpiresAt.Format(time.RFC3339) + " · " + project.BudgetOverride.Reason
		}
		rows = append(rows, healthRow{
			ID:        "health-budget-" + boardCardSlug(project.ID),
			Component: "Budget · " + projectSmallMultipleName(project),
			Kind:      kind,
			Status:    status,
			Detail:    detail,
			Quota:     formatUSD(project.CurrentSpendUSD) + " / " + formatUSD(project.PerDayMaxUSD),
			QuotaPct:  percent,
			QuotaWarn: percent >= 80,
			ResetAt:   project.BudgetResetAt,
		})
	}
	return rows
}

func healthRows(snapshot telemetry.Snapshot) []healthRow {
	rows := make([]healthRow, 0, 4)
	if snapshot.RateLimits != nil {
		if bucket := snapshot.RateLimits.GitHubREST; bucket != nil {
			row := healthBucketRow("health-github-rest", "GitHub REST", bucket)
			// Keep the exhaustion/backoff detail on unhealthy rows rather than
			// overwriting it with the raw request count.
			if usage := snapshot.RateLimits.RESTUsage; usage != nil && usage.TotalRequests > 0 && row.Kind == primitives.KindOK {
				row.Detail = formatInt(usage.TotalRequests) + " requests in the last poll cycle"
			}
			rows = append(rows, row)
		}
		if bucket := snapshot.RateLimits.GitHubGraphQL; bucket != nil {
			row := healthBucketRow("health-github-graphql", "GitHub GraphQL", bucket)
			if cost := snapshot.RateLimits.GraphQLCost; cost != nil && cost.TotalQueries > 0 && row.Kind == primitives.KindOK {
				row.Detail = formatInt(cost.TotalQueries) + " queries · cost " + formatInt(cost.TotalCost)
			}
			rows = append(rows, row)
		}
	}
	rows = append(rows, healthRefreshRows(snapshot)...)
	rows = append(rows, healthSchedulerRow(snapshot), healthUpdateRow(snapshot.Update), healthBackoffRow(snapshot))
	for _, release := range healthReleases(snapshot) {
		rows = append(rows, healthReleaseRow(release))
	}
	for _, outage := range backendCapacityOutages(snapshot.BackendOutages) {
		rows = append(rows, healthBackendOutageRow(outage, snapshot.GeneratedAt))
	}
	return rows
}

func healthUpdateRow(update telemetry.Update) healthRow {
	row := healthRow{
		ID:        "health-update",
		Component: "Detent update",
		Kind:      primitives.KindNeutral,
		Status:    "Disabled",
		Detail:    "Automatic update checks are disabled",
		Resets:    "—",
	}
	if !update.Enabled {
		if update.LastAppliedVersion != "" {
			row.Detail += " · last applied " + update.LastAppliedVersion
		}
		return row
	}
	row.Kind = primitives.KindOK
	row.Status = strings.ReplaceAll(strings.TrimSpace(update.State), "_", " ")
	if row.Status == "" || row.Status == "scheduled" {
		row.Status = "Scheduled"
	}
	row.Detail = "Checks every " + formatCount(update.CheckIntervalHours) + " hours"
	if update.AutoApplyEnabled {
		row.Detail += " · auto-apply enabled"
	} else {
		row.Detail += " · notify only"
	}
	if update.AvailableVersion != "" {
		row.Kind = primitives.KindWarn
		row.Detail += " · " + update.AvailableVersion + " available"
	}
	if update.LastAppliedVersion != "" {
		row.Detail += " · last applied " + update.LastAppliedVersion
	}
	if update.LastCheckAt != nil {
		row.DetailAt = *update.LastCheckAt
	}
	if update.NextCheckAt != nil {
		row.ResetAt = *update.NextCheckAt
	}
	if update.LastError != "" {
		row.Kind = primitives.KindErr
		row.Status = "Failed"
		row.Detail = update.LastError
	}
	return row
}

func healthReleases(snapshot telemetry.Snapshot) []telemetry.Release {
	if len(snapshot.Releases) > 0 {
		return snapshot.Releases
	}
	if !snapshot.Release.IsZero() {
		return []telemetry.Release{snapshot.Release}
	}
	return nil
}

func healthReleaseRow(release telemetry.Release) healthRow {
	component := "Auto-release"
	if strings.TrimSpace(release.ProjectID) != "" {
		component += " · " + release.ProjectID
	}
	status := strings.ReplaceAll(strings.TrimSpace(release.State), "_", " ")
	if status == "" {
		status = "Waiting"
	}
	detail := "Last " + release.LastRelease + " · " + formatCount(release.UnreleasedMerges) + " unreleased merges"
	if release.LastRelease == "" {
		detail = formatCount(release.UnreleasedMerges) + " unreleased merges · no prior release"
	}
	kind := primitives.KindOK
	switch release.State {
	case "failed":
		kind = primitives.KindErr
		detail = release.LastError
	case "waiting_for_ci", "rerunning_ci", "release_pending":
		kind = primitives.KindWarn
	}
	row := healthRow{ID: "health-release-" + boardCardSlug(release.ProjectID), Component: component, Kind: kind, Status: status, Detail: detail, Resets: "—"}
	if release.NextTriggerAt != nil {
		row.ResetAt = *release.NextTriggerAt
	}
	return row
}

func healthBackendOutageRow(outage telemetry.BackendOutage, now time.Time) healthRow {
	detail, resumeAt, _ := backendCapacityOutageDetailParts(outage, now)
	return healthRow{
		ID:        "health-backend-" + boardCardSlug(outage.BackendID),
		Component: "Backend " + outage.BackendID,
		Kind:      primitives.KindWarn,
		Status:    "Usage limit",
		Detail:    detail,
		DetailAt:  resumeAt,
		Resets:    "—",
		ResetAt:   outage.ResumeAt,
	}
}

func healthBucketRow(id string, component string, bucket *telemetry.RateLimitBucket) healthRow {
	row := healthRow{
		ID:        id,
		Component: component,
		Kind:      primitives.KindOK,
		Status:    "Healthy",
		Detail:    "Within budget",
		Resets:    "—",
	}
	switch strings.TrimSpace(bucket.Status) {
	case telemetry.RateLimitStatusBackoff:
		row.Kind = primitives.KindWarn
		row.Status = "Backoff"
		row.Detail = "Requests in backoff"
	case telemetry.RateLimitStatusExhausted:
		row.Kind = primitives.KindErr
		row.Status = "Exhausted"
		row.Detail = "Quota exhausted"
	}
	// A bucket can be exhausted via zero remaining without an explicit status
	// (the orchestrator and primary-exhausted demo snapshots use this shape),
	// so mirror the verdict's exhaustion test to avoid a Healthy row under an
	// exhaustion alert.
	if row.Kind != primitives.KindErr && gitHubAPIBucketExhausted(bucket) {
		row.Kind = primitives.KindErr
		row.Status = "Exhausted"
		row.Detail = "Quota exhausted"
	}
	if bucket.Limit > 0 {
		used := bucket.Limit - bucket.Remaining
		if used < 0 {
			used = 0
		}
		row.Quota = formatInt(used) + " / " + formatInt(bucket.Limit)
		row.QuotaPct = int(float64(used) / float64(bucket.Limit) * 100)
		row.QuotaWarn = row.QuotaPct >= 90
	}
	if bucket.ResetAt != nil {
		row.ResetAt = *bucket.ResetAt
	}
	return row
}

func healthSchedulerRow(snapshot telemetry.Snapshot) healthRow {
	running := runningCount(snapshot)
	queued := queueCount(snapshot)
	row := healthRow{
		ID:        "health-scheduler",
		Component: "Scheduler",
		Kind:      primitives.KindOK,
		Status:    "Running",
		Detail:    formatCount(running) + " active sessions · " + formatCount(queued) + " queued",
		Resets:    "—",
	}
	if running == 0 && queued == 0 {
		row.Status = "Idle"
		row.Detail = "No active sessions or queued work"
	}
	return row
}

func healthBackoffRow(snapshot telemetry.Snapshot) healthRow {
	row := healthRow{
		ID:        "health-backoff",
		Component: "Backoff",
		Kind:      primitives.KindOK,
		Status:    "None",
		Detail:    "No endpoints in backoff",
		Resets:    "—",
	}
	if snapshot.RateLimits == nil {
		return row
	}
	affected := make([]string, 0, 2)
	seen := make(map[string]bool, 3)
	addAffected := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		affected = append(affected, name)
	}
	for _, candidate := range []struct {
		name   string
		bucket *telemetry.RateLimitBucket
	}{
		{name: "REST", bucket: snapshot.RateLimits.GitHubREST},
		{name: "GraphQL", bucket: snapshot.RateLimits.GitHubGraphQL},
		{name: "primary", bucket: snapshot.RateLimits.Primary},
	} {
		if candidate.bucket == nil {
			continue
		}
		switch strings.TrimSpace(candidate.bucket.Status) {
		case telemetry.RateLimitStatusBackoff, telemetry.RateLimitStatusExhausted:
			addAffected(candidate.name)
		}
	}
	// A secondary REST throttle surfaces through RESTUsage.RateLimited /
	// BackoffUntil, not a bucket status, so include it explicitly.
	if gitHubAPIInBackoff(snapshot) {
		addAffected("REST")
	}
	if len(affected) > 0 {
		row.Kind = primitives.KindWarn
		row.Status = "Active"
		row.Detail = "Backoff active on " + strings.Join(affected, ", ")
	}
	return row
}

func healthVerdictGlyphClass(kind primitives.Kind) string {
	return "text-sm leading-none " + boardExtraTextClass(kind)
}

func healthRowClass(last bool) string {
	base := "grid grid-cols-[180px_140px_minmax(0,1fr)_200px_90px] items-center gap-3.5 px-4 py-2.5"
	if !last {
		return base + " border-b border-line"
	}
	return base
}

func healthQuotaBarClass(warn bool) string {
	if warn {
		return "h-full bg-warn"
	}
	return "h-full bg-sec"
}

func healthQuotaBarKindClass(kind primitives.Kind, warn bool) string {
	if kind == primitives.KindErr {
		return "h-full bg-err"
	}
	return healthQuotaBarClass(warn)
}

func HealthShellDataFromDashboardV2(data DashboardData) DashboardShellData {
	shell := DashboardShellDataFromDashboard(data)
	shell.ActiveNav = "health"
	shell.IncludeDashboardCharts = false
	return shell
}
