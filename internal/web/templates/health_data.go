package templates

import (
	"sort"
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
	Link      string
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
		Rows:      append(append(healthRows(snapshot), healthActiveHoursRows(data)...), healthBudgetRows(data)...),
	}
	if len(snapshot.DispatchStalls) > 0 {
		view.Kind = primitives.KindErr
		view.Verdict = "Dispatch is stalled."
		view.Detail = boardCountLabel(len(snapshot.DispatchStalls), "Project needs", "Projects need") + " human attention."
		return view
	}
	if len(snapshot.TrackerUnavailable) > 0 {
		view.Kind = primitives.KindErr
		view.Verdict = "Tracker is unavailable."
		view.Detail = trackerUnavailableHealthDetail(snapshot.TrackerUnavailable)
		return view
	}
	if len(snapshot.ForgeUnavailable) > 0 {
		view.Kind = primitives.KindErr
		view.Verdict = "Forge writes are unavailable."
		view.Detail = forgeUnavailableHealthDetail(snapshot.ForgeUnavailable)
		return view
	}
	if len(snapshot.CIUnavailable) > 0 {
		view.Kind = primitives.KindErr
		view.Verdict = "CI is unavailable."
		view.Detail = ciUnavailableHealthDetail(snapshot.CIUnavailable)
		return view
	}
	if len(snapshot.StalenessWarnings) > 0 {
		view.Kind = primitives.KindWarn
		view.Verdict = "Fleet work is stale."
		view.Detail = stalenessHealthDetail(snapshot.StalenessWarnings)
		return view
	}
	if len(snapshot.StrandedActiveIssues) > 0 {
		view.Kind = primitives.KindWarn
		view.Verdict = "Active work has no live worker."
		view.Detail = strandedActiveHealthDetail(snapshot.StrandedActiveIssues)
		return view
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
		view.Detail = refreshStaleHealthDetail(snapshot)
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
		view.Detail = "Only the named projects are paused; expand breaker evidence for item and recovery details."
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
	if len(snapshot.AdmissionProposals) > 0 {
		view.Kind = primitives.KindWarn
		view.Verdict = boardCountLabel(len(snapshot.AdmissionProposals), "Admission proposal awaits human decision", "Admission proposals await human decision") + "."
		view.Detail = "Review the affected issues before the proposals expire."
		return view
	}
	if snapshot.Refresh.Behind() {
		view.Kind = primitives.KindNeutral
		view.Verdict = "Refresh loop is behind."
		view.Detail = "The latest tracker refresh succeeded; the loop is pacing behind its target cadence."
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

func healthActiveHoursRows(data DashboardData) []healthRow {
	rows := make([]healthRow, 0, len(data.Projects))
	for _, project := range data.Projects {
		active := project.ActiveHours
		if !active.Configured {
			continue
		}
		row := healthRow{
			ID:        "health-active-hours-" + boardCardSlug(project.ID),
			Component: "Active hours · " + projectSmallMultipleName(project),
			Kind:      primitives.KindOK,
		}
		switch {
		case active.OverrideActive:
			row.Status = "Override"
			row.Detail = "Dispatch allowed outside the recurring window"
			row.Resets = "at override expiry"
			if active.OverrideUntil != nil {
				row.ResetAt = *active.OverrideUntil
				row.Detail += " until " + projectActiveHoursTime(active.OverrideUntil, active.Timezone, "Mon, Jan 2 at 15:04 MST")
			}
		case active.WindowOpen:
			row.Status = "Open"
			row.Detail = "Dispatch admission is open in " + active.Timezone
			row.Resets = "at window close"
			if active.NextClose != nil {
				row.ResetAt = *active.NextClose
				row.Detail += " until " + projectActiveHoursTime(active.NextClose, active.Timezone, "Mon, Jan 2 at 15:04 MST")
			}
		default:
			row.Status = "Off hours"
			row.Detail = "New dispatches are waiting for the next window in " + active.Timezone
			row.Resets = "at window open"
			if active.NextOpen != nil {
				row.ResetAt = *active.NextOpen
				row.Detail += " at " + projectActiveHoursTime(active.NextOpen, active.Timezone, "Mon, Jan 2 at 15:04 MST")
			}
		}
		rows = append(rows, row)
	}
	return rows
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
		detail := formatUSD(project.CurrentSpendUSD) + " notional USD today"
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
	for _, stall := range snapshot.DispatchStalls {
		rows = append(rows, healthDispatchStallRow(stall))
	}
	rows = append(rows, healthTrackerUnavailableRows(snapshot.TrackerUnavailable)...)
	rows = append(rows, healthForgeUnavailableRows(snapshot.ForgeUnavailable)...)
	rows = append(rows, healthCIUnavailableRows(snapshot.CIUnavailable)...)
	for _, warning := range snapshot.StalenessWarnings {
		rows = append(rows, healthStalenessRow(warning))
	}
	rows = append(rows, healthStrandedActiveRows(snapshot.StrandedActiveIssues)...)
	if snapshot.RateLimits != nil {
		for _, provider := range []struct {
			id        string
			component string
			bucket    *telemetry.RateLimitBucket
		}{
			{id: "health-provider-primary", component: "Provider primary window", bucket: snapshot.RateLimits.Primary},
			{id: "health-provider-secondary", component: "Provider secondary window", bucket: snapshot.RateLimits.Secondary},
		} {
			if provider.bucket != nil {
				rows = append(rows, healthProviderBucketRow(provider.id, provider.component, provider.bucket))
			}
		}
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
	rows = append(rows, healthAdmissionProposalRows(snapshot.AdmissionProposals, snapshot.GeneratedAt)...)
	rows = append(rows, healthSchedulerRow(snapshot), healthUpdateRowAt(snapshot.Update, snapshot.GeneratedAt), healthBackoffRow(snapshot))
	for _, release := range healthReleases(snapshot) {
		rows = append(rows, healthReleaseRow(release))
	}
	for _, outage := range backendCapacityOutages(snapshot.BackendOutages) {
		rows = append(rows, healthBackendOutageRow(outage, snapshot.GeneratedAt))
	}
	return rows
}

func healthDispatchStallRow(stall telemetry.DispatchStatus) healthRow {
	projectID := strings.TrimSpace(stall.ProjectID)
	if projectID == "" {
		projectID = "Fleet"
	}
	detail := boardCountLabel(stall.CandidateCount, "candidate", "candidates") + " skipped for " + formatDuration(float64(stall.StallDurationSeconds))
	if waitReason := strings.TrimSpace(stall.WaitReason); waitReason != "" {
		detail += " · " + waitReason
	}
	return healthRow{
		ID:        "health-dispatch-stall-" + boardCardSlug(projectID),
		Component: "Dispatch · " + projectID,
		Kind:      primitives.KindErr,
		Status:    "Needs attention",
		Detail:    detail,
		Resets:    "operator action",
	}
}

func healthCIUnavailableRows(conditions []telemetry.CICondition) []healthRow {
	rows := make([]healthRow, 0, len(conditions))
	for index, condition := range conditions {
		projectID := strings.TrimSpace(condition.ProjectID)
		if projectID == "" {
			projectID = "project"
		}
		rows = append(rows, healthRow{
			ID:        "health-ci-unavailable-" + boardAlertRowSlug(projectID, index),
			Component: "CI · " + projectID,
			Kind:      primitives.KindErr,
			Status:    "Unavailable",
			Detail: boardCountLabel(condition.UnstartedCheckCount, "queued check", "queued checks") +
				" never started " + ciUnavailableConditionDetail(condition),
			Resets:   "when checks start",
			DetailAt: condition.LastObservedAt,
		})
	}
	return rows
}

func healthTrackerUnavailableRows(conditions []telemetry.TrackerCondition) []healthRow {
	rows := make([]healthRow, 0, len(conditions))
	for index, condition := range conditions {
		projectID := strings.TrimSpace(condition.ProjectID)
		if projectID == "" {
			projectID = "project"
		}
		connectorName := strings.TrimSpace(condition.Connector)
		if connectorName == "" {
			connectorName = "Tracker"
		}
		detail := "tracker_unavailable · " + strings.Trim(strings.TrimSpace(condition.Operation)+" · "+strings.TrimSpace(condition.ErrorClass), " ·")
		link := ""
		if providerSummary, providerLink := trackerProviderStatusSummary(condition); providerSummary != "" {
			detail = providerSummary
			link = providerLink
			if condition.ProviderStatus != nil && condition.ProviderStatus.Incident != nil {
				detail += " · " + condition.ProviderStatus.Incident.Name
			}
		}
		rows = append(rows, healthRow{
			ID:        "health-tracker-unavailable-" + boardAlertRowSlug(projectID, index),
			Component: connectorName + " tracker · " + projectID,
			Kind:      primitives.KindErr,
			Status:    "Unavailable",
			Detail:    detail,
			Link:      link,
			Resets:    "on successful canary",
			ResetAt:   condition.NextProbeAt,
			DetailAt:  condition.LastObservedAt,
		})
	}
	return rows
}

func healthForgeUnavailableRows(conditions []telemetry.ForgeCondition) []healthRow {
	rows := make([]healthRow, 0, len(conditions))
	for index, condition := range conditions {
		projectID := strings.TrimSpace(condition.ProjectID)
		if projectID == "" {
			projectID = "project"
		}
		host := strings.TrimSpace(condition.Host)
		if host == "" {
			host = "configured forge"
		}
		detail := "forge_unavailable · " + strings.Trim(strings.TrimSpace(condition.Operation)+" · "+strings.TrimSpace(condition.ErrorClass), " ·")
		rows = append(rows, healthRow{
			ID:        "health-forge-unavailable-" + boardAlertRowSlug(projectID+"-"+host, index),
			Component: "Forge writes · " + host + " · " + projectID,
			Kind:      primitives.KindErr,
			Status:    "Unavailable",
			Detail:    detail,
			Resets:    "on successful write canary",
			ResetAt:   condition.NextProbeAt,
			DetailAt:  condition.LastObservedAt,
		})
	}
	return rows
}

func trackerUnavailableHealthDetail(conditions []telemetry.TrackerCondition) string {
	if len(conditions) == 1 {
		condition := conditions[0]
		if providerSummary, _ := trackerProviderStatusSummary(condition); providerSummary != "" {
			return providerSummary + "; tracker-dependent dispatch is paused."
		}
		connectorName := strings.TrimSpace(condition.Connector)
		if connectorName == "" {
			connectorName = "Configured"
		}
		return connectorName + " tracker reads are unavailable; tracker-dependent dispatch is paused."
	}
	return boardCountLabel(len(conditions), "tracker connector is", "tracker connectors are") + " unavailable; tracker-dependent dispatch is paused."
}

func trackerProviderStatusSummary(condition telemetry.TrackerCondition) (string, string) {
	status := condition.ProviderStatus
	if status == nil {
		return "", ""
	}
	switch status.State {
	case telemetry.ProviderStatusCorroborated:
		if status.Incident == nil {
			return "provider status corroborated the outage", ""
		}
		provider := strings.TrimSpace(status.Provider)
		if provider == "" {
			provider = "Provider"
		}
		summary := provider + " incident"
		if len(status.Incident.Components) > 0 {
			summary += " affecting " + healthNaturalList(status.Incident.Components)
		}
		if phase := strings.TrimSpace(status.Incident.Status); phase != "" {
			summary += " — " + phase
		}
		return summary, strings.TrimSpace(status.Incident.URL)
	case telemetry.ProviderStatusNoMatch:
		return "no matching provider incident", ""
	case telemetry.ProviderStatusUnavailable:
		return "provider status unavailable", ""
	case telemetry.ProviderStatusPending:
		return "provider status check pending", ""
	default:
		return "", ""
	}
}

func healthNaturalList(values []string) string {
	clean := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			clean = append(clean, value)
		}
	}
	switch len(clean) {
	case 0:
		return ""
	case 1:
		return clean[0]
	case 2:
		return clean[0] + " and " + clean[1]
	default:
		return strings.Join(clean[:len(clean)-1], ", ") + ", and " + clean[len(clean)-1]
	}
}

func forgeUnavailableHealthDetail(conditions []telemetry.ForgeCondition) string {
	if len(conditions) == 1 {
		host := strings.TrimSpace(conditions[0].Host)
		if host == "" {
			host = "The configured forge"
		}
		return host + " cannot accept push or pull-request writes; write-dependent dispatch is paused."
	}
	return boardCountLabel(len(conditions), "forge host cannot", "forge hosts cannot") + " accept writes; write-dependent dispatch is paused."
}

func ciUnavailableHealthDetail(conditions []telemetry.CICondition) string {
	checkCount := 0
	pullRequestCount := 0
	oldestQueueSeconds := int64(0)
	for _, condition := range conditions {
		checkCount += condition.UnstartedCheckCount
		pullRequestCount += condition.PullRequestCount
		oldestQueueSeconds = max(oldestQueueSeconds, condition.OldestQueueSeconds)
	}
	detail := boardCountLabel(checkCount, "check is", "checks are") + " queued and unstarted across " +
		boardCountLabel(pullRequestCount, "PR", "PRs")
	if oldestQueueSeconds > 0 {
		detail += "; oldest queued " + formatDuration(float64(oldestQueueSeconds))
	}
	return detail + "."
}

func healthProviderBucketRow(id string, component string, bucket *telemetry.RateLimitBucket) healthRow {
	row := healthRow{
		ID:        id,
		Component: component,
		Kind:      primitives.KindOK,
		Status:    "Healthy",
		Detail:    "Provider window has full dispatch capacity",
		Resets:    "—",
	}
	if bucket.Limit > 0 {
		remaining := max(int64(0), min(bucket.Remaining, bucket.Limit))
		used := bucket.Limit - remaining
		remainingPct := int(float64(remaining) / float64(bucket.Limit) * 100)
		row.Quota = formatInt(used) + " / " + formatInt(bucket.Limit)
		row.QuotaPct = 100 - remainingPct
		if remaining < bucket.Limit {
			row.Status = "Pacing"
			row.Detail = formatCount(remainingPct) + "% remaining · dispatch capacity scales with the provider window"
		}
	}
	if bucket.ResetAt != nil {
		row.ResetAt = *bucket.ResetAt
	}
	return row
}

func healthAdmissionProposalRows(proposals []telemetry.AdmissionProposal, observedAt time.Time) []healthRow {
	byProject := make(map[string][]telemetry.AdmissionProposal)
	for _, proposal := range proposals {
		projectID := strings.TrimSpace(proposal.ProjectID)
		if projectID == "" {
			projectID = "project"
		}
		byProject[projectID] = append(byProject[projectID], proposal)
	}
	projects := make([]string, 0, len(byProject))
	for projectID := range byProject {
		projects = append(projects, projectID)
	}
	sort.Strings(projects)

	rows := make([]healthRow, 0, len(projects))
	for _, projectID := range projects {
		projectProposals := byProject[projectID]
		sort.Slice(projectProposals, func(i, j int) bool {
			return admissionProposalTarget(projectProposals[i]) < admissionProposalTarget(projectProposals[j])
		})
		details := make([]string, 0, len(projectProposals))
		for _, proposal := range projectProposals {
			details = append(details, admissionProposalTarget(proposal)+" · "+admissionProposalTiming(proposal, observedAt))
		}
		rows = append(rows, healthRow{
			ID:        "health-admission-proposals-" + boardCardSlug(projectID),
			Component: "Admission · " + projectID,
			Kind:      primitives.KindWarn,
			Status:    boardCountLabel(len(projectProposals), "awaiting decision", "awaiting decisions"),
			Detail:    strings.Join(details, "; "),
			Resets:    "on decision",
		})
	}
	return rows
}

func healthStrandedActiveRows(issues []telemetry.StrandedIssue) []healthRow {
	byProject := make(map[string][]telemetry.StrandedIssue)
	for _, issue := range issues {
		projectID := strings.TrimSpace(issue.ProjectID)
		if projectID == "" {
			projectID = "project"
		}
		byProject[projectID] = append(byProject[projectID], issue)
	}
	projects := make([]string, 0, len(byProject))
	for projectID := range byProject {
		projects = append(projects, projectID)
	}
	sort.Strings(projects)

	rows := make([]healthRow, 0, len(projects))
	for _, projectID := range projects {
		projectIssues := byProject[projectID]
		sort.Slice(projectIssues, func(i, j int) bool {
			return strandedActiveHealthTarget(projectIssues[i]) < strandedActiveHealthTarget(projectIssues[j])
		})
		details := make([]string, 0, len(projectIssues))
		for _, issue := range projectIssues {
			details = append(details, strandedActiveHealthIssueDetail(issue))
		}
		rows = append(rows, healthRow{
			ID:        "health-stranded-active-" + boardCardSlug(projectID),
			Component: "Active work · " + projectID,
			Kind:      primitives.KindWarn,
			Status:    "No live worker",
			Detail:    strings.Join(details, "; "),
			Resets:    "on dispatch",
		})
	}
	return rows
}

func strandedActiveHealthDetail(issues []telemetry.StrandedIssue) string {
	if len(issues) == 1 {
		return strandedActiveHealthIssueDetail(issues[0]) + "."
	}
	return formatCount(len(issues)) + " active issues have no live worker."
}

func strandedActiveHealthIssueDetail(issue telemetry.StrandedIssue) string {
	detail := strandedActiveHealthTarget(issue) + " · " + formatDuration(float64(issue.DurationSeconds))
	reason := strings.TrimSpace(issue.LastRefusalReason)
	if reason == "" {
		reason = "none recorded"
	}
	return detail + " · last refusal: " + reason
}

func strandedActiveHealthTarget(issue telemetry.StrandedIssue) string {
	for _, value := range []string{issue.Identifier, issue.IssueID, issue.IssueURL} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "issue"
}

func healthStalenessRow(warning telemetry.StalenessWarning) healthRow {
	component := "Fleet staleness"
	if projectID := strings.TrimSpace(warning.ProjectID); projectID != "" {
		component += " · " + projectID
	}
	detail := doctorStyleStalenessTarget(warning) + " · " + strings.TrimSpace(warning.Detail)
	if warning.AgeSeconds > 0 {
		detail += " · " + formatDuration(float64(warning.AgeSeconds))
	}
	status := "Stale"
	if warning.WaitingOnHuman {
		status = "Reminder due"
	}
	return healthRow{
		ID:        "health-staleness-" + boardCardSlug(warning.ID),
		Component: component,
		Kind:      primitives.KindWarn,
		Status:    status,
		Detail:    detail,
		Resets:    "on progress",
	}
}

func stalenessHealthDetail(warnings []telemetry.StalenessWarning) string {
	if len(warnings) == 1 {
		return doctorStyleStalenessTarget(warnings[0]) + " needs operator attention."
	}
	return formatCount(len(warnings)) + " warnings need operator attention."
}

func healthCopyPayload(view healthView) string {
	var payload strings.Builder
	payload.WriteString("Detent health — ")
	payload.WriteString(boardCountLabel(len(view.Rows), "signal", "signals"))
	payload.WriteString(" — checked ")
	if view.CheckedAt.IsZero() {
		payload.WriteString("unavailable")
	} else {
		payload.WriteString(view.CheckedAt.UTC().Format(time.RFC3339))
	}
	payload.WriteByte('\n')
	payload.WriteString(healthCopyPlainText(strings.TrimSpace(strings.TrimSpace(view.Verdict) + " " + strings.TrimSpace(view.Detail))))

	for index, row := range view.Rows {
		if index == 0 {
			payload.WriteString("\n\n")
		} else {
			payload.WriteByte('\n')
		}
		payload.WriteString(healthCopyKind(row.Kind))
		payload.WriteByte(' ')
		payload.WriteString(healthCopyPlainText(strings.TrimSpace(row.Component)))
		payload.WriteString(" | ")
		payload.WriteString(healthCopyPlainText(strings.TrimSpace(row.Status)))
		payload.WriteString(" | ")
		payload.WriteString(healthCopyPlainText(strings.TrimSpace(row.Detail)))
		if !row.DetailAt.IsZero() {
			payload.WriteString(" · observed ")
			payload.WriteString(row.DetailAt.UTC().Format(time.RFC3339))
		}
		if quota := strings.TrimSpace(row.Quota); quota != "" {
			payload.WriteString(" | ")
			payload.WriteString(healthCopyPlainText(quota))
		}
		payload.WriteString(" | resets ")
		if !row.ResetAt.IsZero() {
			payload.WriteString(row.ResetAt.UTC().Format(time.RFC3339))
		} else if resets := strings.TrimSpace(row.Resets); resets != "" {
			payload.WriteString(healthCopyPlainText(resets))
		} else {
			payload.WriteString("—")
		}
	}

	return payload.String()
}

func healthCopyPlainText(value string) string {
	const prefix = "{{detent-time:"
	for {
		start := strings.Index(value, prefix)
		if start < 0 {
			return value
		}
		endOffset := strings.Index(value[start+len(prefix):], "}}")
		if endOffset < 0 {
			return value
		}
		end := start + len(prefix) + endOffset
		token := value[start+len(prefix) : end]
		separator := strings.IndexByte(token, ':')
		if separator < 0 {
			return value
		}
		replacement := token[separator+1:]
		if parsed, err := time.Parse(time.RFC3339Nano, replacement); err == nil {
			replacement = parsed.UTC().Format(time.RFC3339)
		}
		value = value[:start] + replacement + value[end+2:]
	}
}

func healthCopyKind(kind primitives.Kind) string {
	switch kind {
	case primitives.KindOK:
		return "[OK]  "
	case primitives.KindWarn:
		return "[WARN]"
	case primitives.KindErr:
		return "[ERR] "
	default:
		return "[INFO]"
	}
}

func doctorStyleStalenessTarget(warning telemetry.StalenessWarning) string {
	for _, value := range []string{warning.Identifier, warning.IssueID, warning.ProjectID} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "fleet"
}

func healthUpdateRow(update telemetry.Update) healthRow {
	return healthUpdateRowAt(update, time.Now())
}

func healthUpdateRowAt(update telemetry.Update, now time.Time) healthRow {
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
	row.Status = strings.ReplaceAll(strings.TrimSpace(update.DisplayState(now)), "_", " ")
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

func detentUpdatePending(update telemetry.Update) bool {
	return strings.TrimSpace(update.State) == "pending_idle" && strings.TrimSpace(update.AvailableVersion) != ""
}

func detentPendingUpdateVersion(update telemetry.Update) string {
	version := strings.TrimSpace(update.AvailableVersion)
	if version == "" {
		return "update"
	}
	return version
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
		Detail:    runningCountLabel(snapshot) + " active sessions · " + formatCount(queued) + " queued",
		Resets:    "—",
	}
	if !runtimeCountComplete(snapshot) {
		row.Kind = primitives.KindNeutral
		row.Status = "Starting"
		row.Detail = "Active session count is not yet complete"
		return row
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
