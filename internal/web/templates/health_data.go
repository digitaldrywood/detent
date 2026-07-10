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
		Footnote:  "Quota bars turn amber only at 90% — polling continues normally; backoff engages automatically when a limit is exhausted.",
		Rows:      healthRows(snapshot),
	}
	outages := backendCapacityOutages(snapshot.BackendOutages)
	if len(outages) > 0 {
		view.Kind = primitives.KindWarn
		view.Verdict = backendCapacityHealthVerdict(snapshot)
		view.Detail, view.DetailAt, view.DetailRelative = backendCapacityOutageDetailParts(outages[0], snapshot.GeneratedAt)
		return view
	}
	switch api.State {
	case gitHubAPIHealthStateHealthy, gitHubAPIHealthStateAtRest:
		view.Kind = primitives.KindOK
		view.Verdict = "All systems nominal."
		view.Detail = api.Summary
	case gitHubAPIHealthStateWarning:
		view.Kind = primitives.KindWarn
		view.Verdict = api.Label + "."
		view.Detail = api.Detail
	case gitHubAPIHealthStateBackoff, gitHubAPIHealthStateExhausted:
		view.Kind = primitives.KindErr
		view.Verdict = api.Label + "."
		view.Detail = api.Detail
	default:
		view.Kind = primitives.KindNeutral
		view.Verdict = "Waiting for the first health snapshot."
		view.Detail = api.Detail
	}
	return view
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
	rows = append(rows, healthSchedulerRow(snapshot), healthBackoffRow(snapshot))
	for _, release := range healthReleases(snapshot) {
		rows = append(rows, healthReleaseRow(release))
	}
	for _, outage := range backendCapacityOutages(snapshot.BackendOutages) {
		rows = append(rows, healthBackendOutageRow(outage, snapshot.GeneratedAt))
	}
	return rows
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

func HealthShellDataFromDashboardV2(data DashboardData) DashboardShellData {
	shell := DashboardShellDataFromDashboard(data)
	shell.ActiveNav = "health"
	shell.IncludeDashboardCharts = false
	return shell
}
