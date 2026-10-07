package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type reportsCompletion struct {
	Lanes   []operatortool.AnalyticsLaneDuration `json:"lanes"`
	Done    int                                  `json:"done"`
	System  operatortool.AnalyticsDuration       `json:"system"`
	Lead    operatortool.AnalyticsDuration       `json:"lead"`
	Working operatortool.AnalyticsDuration       `json:"working"`
	Partial bool                                 `json:"partial"`
}

type reportsStage struct {
	Stage         string                         `json:"stage"`
	Model         string                         `json:"model"`
	Effort        string                         `json:"effort"`
	Sessions      int                            `json:"sessions"`
	Succeeded     int                            `json:"succeeded"`
	Duration      operatortool.AnalyticsDuration `json:"duration"`
	Tokens        int64                          `json:"tokens"`
	Input         int64                          `json:"input"`
	Cached        int64                          `json:"cached"`
	Cost          float64                        `json:"cost_usd"`
	Issues        int                            `json:"issues"`
	UsageObserved int                            `json:"usage_observed"`
	Disabled      bool                           `json:"disabled"`
}

type reportsSpend struct {
	WorkItemID string  `json:"work_item_id"`
	Title      string  `json:"title"`
	Cost       float64 `json:"cost_usd"`
}

type reportsBucket struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Done   int       `json:"done"`
	Failed int       `json:"failed"`
	Slots  *float64  `json:"slots"`
}

type hostedReportsReport struct {
	Analytics   nativeAnalyticsProject `json:"analytics"`
	Completion  reportsCompletion      `json:"completion"`
	Previous    reportsCompletion      `json:"previous"`
	Stages      []reportsStage         `json:"stages"`
	Spend       []reportsSpend         `json:"spend"`
	Throughput  []reportsBucket        `json:"throughput"`
	Titles      map[string]string      `json:"titles"`
	Coverage    []diagnosticsCoverage  `json:"coverage"`
	Tokens      int64                  `json:"tokens"`
	Input       int64                  `json:"input"`
	Cached      int64                  `json:"cached"`
	Cost        float64                `json:"cost_usd"`
	FirstTry    *float64               `json:"first_try_percent"`
	Unavailable []string               `json:"unavailable"`
}

func (s *Service) hostedReports(c echo.Context) error {
	ctx := c.Request().Context()
	credential, _, err := s.hostedCredential(ctx, c)
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	projects, err := s.usageReadableProjects(ctx, credential)
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	project := c.QueryParam("project")
	if !slices.Contains(projects, project) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	hours := map[string]int{"24h": 24, "48h": 48, "7d": 168, "30d": 720}[c.QueryParam("range")]
	if hours == 0 {
		return s.nativeAPIError(c, nativeInvalid("Unsupported report range"))
	}
	to := s.config.now().UTC().Truncate(time.Hour)
	bucket := time.Hour
	if hours > 240 {
		bucket = 4 * time.Hour
	}
	w := operatortool.AnalyticsWindow{From: to.Add(-time.Duration(hours) * time.Hour), To: to, Bucket: bucket}
	analytics, err := s.readAnalyticsReport(ctx, credential, operatortool.AnalyticsRequest{ProjectID: project, Limit: maxAnalyticsPopulation}, w)
	if err != nil || len(analytics.Projects) != 1 {
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "Reports are temporarily unavailable")
	}
	report, err := s.readHostedReports(ctx, analytics)
	if err != nil {
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "Reports are temporarily unavailable")
	}
	return c.JSON(http.StatusOK, report)
}

func (s *Service) readHostedReports(ctx context.Context, analytics nativeAnalyticsReport) (hostedReportsReport, error) {
	p := analytics.Projects[0]
	w := analytics.Window
	out := hostedReportsReport{Analytics: p, Stages: []reportsStage{}, Spend: []reportsSpend{}, Throughput: []reportsBucket{}, Titles: map[string]string{}, Unavailable: append([]string{}, p.Unavailable...)}
	diagnostics, err := s.readHostedDiagnostics(ctx, analytics)
	if err != nil {
		return out, err
	}
	out.Coverage = diagnostics.Coverage
	for i, source := range out.Coverage {
		key := map[string]string{
			"Transitions":       "lane_residence_and_unclaimed_waits_unavailable",
			"Usage joins":       "recorded_usage",
			"Activity receipts": "instruction_activity",
			"Queue intervals":   "provider_capacity_wait",
			"Skip reasons":      "recorded_skip_reasons",
		}[source.Source]
		if key != "" && slices.Contains(p.Unavailable, key) {
			out.Coverage[i].Observed, out.Coverage[i].Total = nil, nil
		}
	}
	for _, b := range p.Digest {
		out.Throughput = append(out.Throughput, reportsBucket{From: b.From, To: b.To})
	}
	scope := nativeScope{organization: tracker.OrganizationID(analytics.OrganizationID), project: tracker.ProjectID(p.ProjectID)}
	historyWindow := w
	historyWindow.From = w.From.Add(-w.To.Sub(w.From))
	states, issues, clipped, err := readAnalyticsResidenceHistory(ctx, s.database.db, scope, historyWindow)
	if err != nil {
		return out, err
	}
	timelines := make([]analyticsResidenceTimeline, 0, len(issues))
	for _, issue := range issues {
		timelines = append(timelines, buildAnalyticsResidenceTimeline(issue, states, w.To))
	}
	out.Completion = reportsCompletions(timelines, states, w.From, w.To)
	out.Previous = reportsCompletions(timelines, states, historyWindow.From, w.From)
	out.Completion.Partial = out.Completion.Partial || clipped
	out.Previous.Partial = out.Previous.Partial || clipped
	for _, timeline := range timelines {
		for _, done := range timeline.done {
			if !done.Before(w.From) && done.Before(w.To) {
				if i := nativeAnalyticsBucketIndex(done, w); i >= 0 {
					out.Throughput[i].Done++
				}
				break
			}
		}
	}
	rows, err := readUsageRows(ctx, s.database.db, analytics.OrganizationID, usageWindow{From: w.From, To: w.To}, []string{p.ProjectID}, maxAnalyticsPopulation+1)
	if err != nil {
		return out, err
	}
	if len(rows) > maxAnalyticsPopulation {
		rows = rows[:maxAnalyticsPopulation]
		out.Unavailable = append(out.Unavailable, "reports_usage_complete_population")
	}
	usage := map[string][]usageRow{}
	for _, row := range rows {
		if !row.Period.Before(w.To) {
			continue
		}
		usage[row.AttemptID] = append(usage[row.AttemptID], row)
		out.Tokens += row.tokens()
		out.Input += row.Input
		out.Cached += row.CachedInput
		out.Cost += row.Cost
	}
	out.Stages, out.Spend = reportsAttemptTotals(p.Attempts.Items, usage)
	policy, err := diagnosticsPolicyValues(ctx, s.database.db, scope)
	if err != nil {
		return out, err
	}
	for _, gate := range []struct{ name, stage string }{{"Plan gate", "plan"}, {"Validator gate", "validator"}} {
		if policy[gate.name] == "false" {
			out.Stages = append(out.Stages, reportsStage{Stage: gate.stage, Disabled: true})
		}
	}
	if policy["Plan gate"] == "" || policy["Validator gate"] == "" {
		out.Unavailable = append(out.Unavailable, "stage_gate_configuration")
	}
	for _, a := range p.Attempts.Items {
		if a.Status == "failed" {
			if i := nativeAnalyticsBucketIndex(a.ObservedAt, w); i >= 0 {
				out.Throughput[i].Failed++
			}
		}
	}
	for i, b := range out.Throughput {
		var seconds float64
		for _, capacity := range diagnostics.Capacity {
			if !capacity.Hour.Before(b.From) && capacity.Hour.Before(b.To) {
				seconds += capacity.Slots * 3600
			}
		}
		slots := seconds / b.To.Sub(b.From).Seconds()
		out.Throughput[i].Slots = &slots
	}
	rate, available, err := s.reportsFirstTry(ctx, scope, p)
	if err != nil {
		return out, err
	}
	if available {
		out.FirstTry = &rate
	} else {
		out.Unavailable = append(out.Unavailable, "landed_first_try")
	}
	titleIDs := []string{}
	for _, item := range p.LaneResidence.Aging.Items {
		titleIDs = append(titleIDs, item.WorkItemID)
	}
	for _, item := range out.Spend {
		titleIDs = append(titleIDs, item.WorkItemID)
	}
	for _, signature := range p.FailureSignatures.Items {
		titleIDs = append(titleIDs, signature.WorkItems...)
	}
	encodedIDs, err := marshalNative(titleIDs)
	if err != nil {
		return out, err
	}
	titleRows, err := s.database.db.QueryContext(ctx, "SELECT native_id,title FROM issues WHERE organization_id=? AND project_id=? AND native_id IN (SELECT value FROM json_each(?))", scope.organization, scope.project, encodedIDs)
	if err != nil {
		return out, err
	}
	defer titleRows.Close()
	for titleRows.Next() {
		var id, title string
		if err := titleRows.Scan(&id, &title); err != nil {
			return out, err
		}
		out.Titles[id] = title
	}
	if err := titleRows.Err(); err != nil {
		return out, err
	}
	for i := range out.Spend {
		out.Spend[i].Title = out.Titles[out.Spend[i].WorkItemID]
	}
	if p.Attempts.NextOffset != nil || p.Partial {
		out.Unavailable = append(out.Unavailable, "stage_and_issue_spend_complete_population", "failed_attempts_complete_population")
	}
	if out.Completion.Partial {
		out.Unavailable = append(out.Unavailable, "completion_complete_population")
	}
	for _, source := range []struct {
		name        string
		observed    int
		partial     bool
		unavailable bool
	}{
		{"Lane residence", p.LaneResidence.IssuesObserved, p.LaneResidence.Partial, slices.Contains(p.Unavailable, "lane_residence_and_unclaimed_waits_unavailable")},
		{"Completion timing", out.Completion.System.Count, out.Completion.Partial, slices.Contains(p.Unavailable, "lane_residence_and_unclaimed_waits_unavailable")},
		{"Aging items", len(p.LaneResidence.Aging.Items), p.LaneResidence.Partial, slices.Contains(p.Unavailable, "lane_residence_and_unclaimed_waits_unavailable")},
		{"Failure signatures", len(p.FailureSignatures.Items), p.Partial, false},
	} {
		observed := source.observed
		var total *int
		if !source.partial {
			value := observed
			total = &value
		}
		coverage := diagnosticsCoverage{Source: source.name, Observed: &observed, Total: total}
		if source.unavailable {
			coverage.Observed, coverage.Total = nil, nil
		}
		out.Coverage = append(out.Coverage, coverage)
	}
	return out, nil
}

func reportsCompletions(timelines []analyticsResidenceTimeline, states map[string]tracker.NativeState, from, to time.Time) reportsCompletion {
	out := reportsCompletion{Lanes: []operatortool.AnalyticsLaneDuration{}}
	laneValues := map[string][]float64{}
	var system, lead, working []float64
	for _, timeline := range timelines {
		for _, done := range timeline.done {
			if done.Before(from) || !done.Before(to) {
				continue
			}
			out.Done++
			if timeline.issue.partial || timeline.started == nil {
				out.Partial = true
				break
			}
			var controlled, active float64
			issueLanes := map[string]float64{}
			for _, span := range timeline.spans {
				group := analyticsResidenceGroup(span.lane, states)
				if group == "excluded" {
					continue
				}
				seconds := max(0, minTime(span.to, done).Sub(span.from).Seconds())
				issueLanes[span.lane] += seconds
				if group == "system" {
					controlled += seconds
				}
				if span.lane == "In Progress" {
					active += seconds
				}
			}
			for lane, seconds := range issueLanes {
				laneValues[lane] = append(laneValues[lane], seconds)
			}
			system = append(system, controlled)
			working = append(working, active)
			lead = append(lead, done.Sub(timeline.issue.created).Seconds())
			break
		}
	}
	out.System = analyticsDuration(system)
	out.Lead = analyticsDuration(lead)
	out.Working = analyticsDuration(working)
	for lane, values := range laneValues {
		out.Lanes = append(out.Lanes, operatortool.AnalyticsLaneDuration{Lane: lane, Group: analyticsResidenceGroup(lane, states), AnalyticsDuration: analyticsDuration(values)})
	}
	sort.Slice(out.Lanes, func(i, j int) bool { return out.Lanes[i].Lane < out.Lanes[j].Lane })
	return out
}

func reportsAttemptTotals(attempts []nativeAnalyticsAttempt, usage map[string][]usageRow) ([]reportsStage, []reportsSpend) {
	stages := map[[3]string]*reportsStage{}
	durations := map[[3]string][]float64{}
	issues := map[[3]string][]string{}
	spend := map[string]float64{}
	for _, a := range attempts {
		stage := a.Identity.Role
		if stage == "" {
			stage = "Unavailable"
		}
		model := a.Identity.Model()
		if model == "" {
			model = "Unavailable"
		}
		effort := a.Identity.ReasoningEffort.Value
		if effort == "" {
			effort = "Unavailable"
		}
		key := [3]string{stage, model, effort}
		row := stages[key]
		if row == nil {
			row = &reportsStage{Stage: stage, Model: model, Effort: effort}
			stages[key] = row
		}
		row.Sessions++
		issues[key] = append(issues[key], a.WorkItemID)
		if a.Status == "succeeded" {
			row.Succeeded++
		}
		if a.Status == "succeeded" || a.Status == "failed" {
			var seconds float64
			for _, phase := range a.Phases {
				if !phase.FinishedAt.IsZero() {
					seconds += max(0, phase.FinishedAt.Sub(phase.StartedAt).Seconds())
				}
			}
			if seconds > 0 {
				durations[key] = append(durations[key], seconds)
			}
		}
		if len(usage[a.AttemptID]) > 0 {
			row.UsageObserved++
		}
		for _, u := range usage[a.AttemptID] {
			row.Tokens += u.tokens()
			row.Input += u.Input
			row.Cached += u.CachedInput
			row.Cost += u.Cost
			spend[a.WorkItemID] += u.Cost
		}
	}
	result := []reportsStage{}
	for key, row := range stages {
		row.Duration = analyticsDuration(durations[key])
		slices.Sort(issues[key])
		row.Issues = len(slices.Compact(issues[key]))
		result = append(result, *row)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Stage != b.Stage {
			return a.Stage < b.Stage
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Effort < b.Effort
	})
	costs := []reportsSpend{}
	for id, cost := range spend {
		costs = append(costs, reportsSpend{WorkItemID: id, Cost: cost})
	}
	sort.Slice(costs, func(i, j int) bool {
		if costs[i].Cost == costs[j].Cost {
			return costs[i].WorkItemID < costs[j].WorkItemID
		}
		return costs[i].Cost > costs[j].Cost
	})
	return result, costs
}

func (s *Service) reportsFirstTry(ctx context.Context, scope nativeScope, p nativeAnalyticsProject) (float64, bool, error) {
	if p.Partial || len(p.Landings.Items) == 0 {
		return 0, false, nil
	}
	first, total := 0, 0
	seen := map[string]bool{}
	for _, landing := range p.Landings.Items {
		id := string(landing.WorkItemID)
		if seen[id] {
			continue
		}
		seen[id] = true
		count, complete, err := s.reportsLandingAttemptCount(ctx, scope, landing)
		if err != nil {
			return 0, false, err
		}
		if !complete {
			return 0, false, nil
		}
		if count == 1 {
			first++
		}
		total++
	}
	if total == 0 {
		return 0, false, nil
	}
	return 100 * float64(first) / float64(total), true, nil
}

func (s *Service) reportsLandingAttemptCount(ctx context.Context, scope nativeScope, landing nativeAnalyticsLanding) (int, bool, error) {
	rows, err := s.database.db.QueryContext(ctx, `SELECT coalesce(json_extract(data_json,'$.runtime.landing'),'null') FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND julianday(started_at)<=julianday(?) AND (json_extract(data_json,'$.runtime.identity.role')='merge' OR json_extract(data_json,'$.runtime.landing') IS NOT NULL) ORDER BY started_at,id LIMIT ?`, scope.organization, scope.project, landing.WorkItemID, formatHubTime(landing.Landing.LandedAt), maxAnalyticsPopulation+1)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return 0, false, err
		}
		var receipt *tracker.NativeLandingReceipt
		if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
			return 0, false, err
		}
		count++
		if receipt == nil || count > maxAnalyticsPopulation {
			return 0, false, nil
		}
		if receipt.Landed && receipt.ChangeID == landing.ChangeID {
			return count, true, nil
		}
	}
	return 0, false, rows.Err()
}
