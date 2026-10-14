package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type reportsCompletion struct {
	Lanes     []operatortool.AnalyticsLaneDuration `json:"lanes"`
	Done      int                                  `json:"done"`
	Cancelled int                                  `json:"cancelled"`
	Cost      float64                              `json:"cost_usd"`
	System    operatortool.AnalyticsDuration       `json:"system"`
	Lead      operatortool.AnalyticsDuration       `json:"lead"`
	Working   operatortool.AnalyticsDuration       `json:"working"`
	Partial   bool                                 `json:"partial"`
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
	Attempts   int     `json:"attempts"`
	Shipped    bool    `json:"shipped"`
	State      string  `json:"state,omitempty"`
}

type reportsBucket struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Done   int       `json:"done"`
	Failed int       `json:"failed"`
	Slots  *float64  `json:"slots"`
}

type hostedReportsReport struct {
	Pipeline    pipelineReport         `json:"pipeline"`
	Requests    *requestMetricsReport  `json:"requests,omitempty"`
	Analytics   nativeAnalyticsProject `json:"analytics"`
	Completion  reportsCompletion      `json:"completion"`
	Previous    reportsCompletion      `json:"previous"`
	Stages      []reportsStage         `json:"stages"`
	Spend       []reportsSpend         `json:"spend"`
	Throughput  []reportsBucket        `json:"throughput"`
	Titles      map[string]string      `json:"titles"`
	Coverage    []diagnosticsCoverage  `json:"coverage"`
	Rework      []reportClassTotal     `json:"rework_classes"`
	Failures    []reportClassTotal     `json:"failure_classes"`
	Tokens      int64                  `json:"tokens"`
	Input       int64                  `json:"input"`
	Cached      int64                  `json:"cached"`
	Cost        float64                `json:"cost_usd"`
	FirstTry    *float64               `json:"first_try_percent"`
	Cancelled   int                    `json:"cancelled"`
	PriorCost   *float64               `json:"previous_cost_usd,omitempty"`
	PriorFirst  *float64               `json:"previous_first_try_percent"`
	PriorFailed *int                   `json:"previous_failed,omitempty"`
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
	hours := map[string]int{"24h": 24, "48h": 48, "7d": 168, "30d": 720, "90d": 2160}[c.QueryParam("range")]
	if hours == 0 {
		return s.nativeAPIError(c, nativeInvalid("Unsupported report range"))
	}
	to := s.config.now().UTC().Truncate(time.Hour)
	bucket := time.Hour
	if hours > 720 {
		bucket = 24 * time.Hour
	} else if hours > 240 {
		bucket = 4 * time.Hour
	}
	w := operatortool.AnalyticsWindow{From: reportsWindowStart(to, hours), To: to, Bucket: bucket}
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
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	return s.readHostedReportsFrom(ctx, analytics, analyticsRollupReader{s.database.reader})
}

func (s *Service) readHostedReportsFrom(ctx context.Context, analytics nativeAnalyticsReport, q nativeQueryer) (hostedReportsReport, error) {
	p := analytics.Projects[0]
	w := analytics.Window
	out := hostedReportsReport{Pipeline: p.Pipeline, Analytics: p, Stages: []reportsStage{}, Rework: []reportClassTotal{}, Failures: []reportClassTotal{}, Spend: []reportsSpend{}, Throughput: []reportsBucket{}, Series: []reportsSeriesBucket{}, Runners: []reportsRunner{}, Titles: map[string]string{}, Unavailable: append([]string{}, p.Unavailable...)}
	diagnostics, err := s.readHostedDiagnosticsFrom(ctx, analytics, q)
	if err != nil {
		return out, err
	}
	out.Requests = diagnostics.Requests
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
	q, err = analyticsProjectReader(ctx, q, scope)
	if err != nil {
		return out, err
	}
	states, err := analyticsResidenceStates(ctx, q, scope)
	if err != nil {
		return out, err
	}
	var shipped []nativeShippedIssue
	out.Completion, shipped, err = readReportsCompletion(ctx, q, scope, states, w.From, w.To)
	if err != nil {
		return out, err
	}
	out.Previous, _, err = readReportsCompletion(ctx, q, scope, states, w.From.Add(-w.To.Sub(w.From)), w.From)
	if err != nil {
		return out, err
	}
	for _, issue := range shipped {
		if i := nativeAnalyticsBucketIndex(issue.ClosedAt, w); i >= 0 {
			out.Throughput[i].Done++
		}
	}
	rows, err := readUsageRows(ctx, q, analytics.OrganizationID, usageWindow{From: w.From, To: w.To}, []string{p.ProjectID}, maxAnalyticsPopulation+1)
	if err != nil {
		return out, err
	}
	rows = rows[:min(len(rows), maxAnalyticsPopulation)]
	usage := map[string][]usageRow{}
	for _, row := range rows {
		if !row.Period.Before(w.To) {
			continue
		}
		usage[row.AttemptID] = append(usage[row.AttemptID], row)
		out.Tokens += row.tokens()
		out.Input += row.Input
		out.Cached += row.CachedInput
	}
	if out.Cost, _, err = readProjectSpend(ctx, q, scope, w.From, w.To); err != nil {
		return out, err
	}
	out.Stages, out.Spend = reportsAttemptTotals(p.allAttempts, usage)
	out.Runners = reportsRunnerTotals(p.allAttempts, usage)
	shipped := reportsShipped(timelines, w.From, w.To)
	reportsSpendOutcomes(out.Spend, p.allAttempts, timelines, shipped)
	out.Rework = reportReworkClasses(p.Quality.returns, p.allAttempts, usage)
	out.Failures = reportFailureClasses(p.failures)
	reportClassDetails(out.Rework, out.Failures, p.Quality.returns, p.failures)
	out.Series = reportsSeries(timelines, states, p.allAttempts, usage, shipped, w)
	reportsSeriesClasses(out.Series, p.Quality.returns, p.failures, w)
	policy, err := diagnosticsPolicyValues(ctx, q, scope)
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
	failedIssues := map[string]bool{}
	for _, a := range p.allAttempts {
		if a.Status == "failed" {
			failedIssues[a.WorkItemID] = true
			if i := nativeAnalyticsBucketIndex(a.ObservedAt, w); i >= 0 {
				out.Throughput[i].Failed++
			}
		}
	}
	out.FailedIssues = len(failedIssues)
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
	firstTryPopulation := p
	firstTryPopulation.Landings.Items = p.allLandings
	rate, available, err := s.reportsFirstTry(ctx, scope, firstTryPopulation)
	if err != nil {
		return out, err
	}
	if available {
		out.FirstTry = &rate
	} else {
		out.Unavailable = append(out.Unavailable, "landed_first_try")
	}
	out.Cancelled = out.Completion.Cancelled
	if err := s.readReportsPrior(ctx, q, scope, operatortool.AnalyticsWindow{From: w.From.Add(-w.To.Sub(w.From)), To: w.From}, &out); err != nil {
		return out, err
	}
	titleIDs := []string{}
	for _, item := range p.LaneResidence.Aging.Items {
		titleIDs = append(titleIDs, item.WorkItemID)
	}
	for _, item := range out.OpenWork.Items {
		titleIDs = append(titleIDs, item.WorkItemID)
	}
	for _, item := range out.Spend {
		titleIDs = append(titleIDs, item.WorkItemID)
	}
	for _, signature := range p.FailureSignatures.Items {
		titleIDs = append(titleIDs, signature.WorkItems...)
	}
	for _, class := range slices.Concat(out.Rework, out.Failures) {
		titleIDs = append(titleIDs, class.WorkItems...)
	}
	encodedIDs, err := marshalNative(titleIDs)
	if err != nil {
		return out, err
	}
	titleRows, err := q.QueryContext(ctx, "SELECT native_id,title FROM issues WHERE organization_id=? AND project_id=? AND native_id IN (SELECT value FROM json_each(?))", scope.organization, scope.project, encodedIDs)
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
	if p.Partial {
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

func reportReworkClasses(returns []qualityReturn, attempts []nativeAnalyticsAttempt, usage map[string][]usageRow) []reportClassTotal {
	classes := newReportClassAccumulator()
	byItem := map[string][]qualityReturn{}
	for _, r := range returns {
		classes.add(r.class, r.item, 0)
		byItem[r.item] = append(byItem[r.item], r)
	}
	for _, a := range attempts {
		var latest *qualityReturn
		for i, r := range byItem[a.WorkItemID] {
			if !r.at.After(a.StartedAt) && (latest == nil || r.at.After(latest.at)) {
				latest = &byItem[a.WorkItemID][i]
			}
		}
		if latest == nil {
			continue
		}
		for _, u := range usage[a.AttemptID] {
			classes.addCost(latest.class, u.Cost)
		}
	}
	return classes.result()
}

func reportFailureClasses(failures []nativeFailure) []reportClassTotal {
	classes := newReportClassAccumulator()
	for _, f := range failures {
		classes.add(classifyFailure(f.example), f.workItem, f.cost)
	}
	return classes.result()
}

func readReportsCompletion(ctx context.Context, q nativeQueryer, scope nativeScope, states map[string]tracker.NativeState, from, to time.Time) (reportsCompletion, []nativeShippedIssue, error) {
	population, err := readNativeClosedPopulation(ctx, q, scope, "", nil, from, to)
	if err != nil {
		return reportsCompletion{}, nil, err
	}
	ids := population.shippedIDs()
	out, err := readShippedCompletionTiming(ctx, q, scope, states, ids, to)
	if err != nil {
		return out, nil, err
	}
	out.Done, out.Cancelled = len(population.Shipped), population.Cancelled
	out.Cost, err = readNativeShippedSpend(ctx, q, scope, ids, to)
	return out, population.Shipped, err
}

func readShippedCompletionTiming(ctx context.Context, q nativeQueryer, scope nativeScope, states map[string]tracker.NativeState, ids []string, to time.Time) (reportsCompletion, error) {
	if _, projected := q.(analyticsRollupReader); projected {
		return readReportsCompletionRollups(ctx, q, scope, states, ids, to)
	}
	issues, err := readShippedResidenceIssues(ctx, q, scope, ids, to)
	if err != nil {
		return reportsCompletion{}, err
	}
	timelines := make([]analyticsResidenceTimeline, 0, len(issues))
	for _, issue := range issues {
		timelines = append(timelines, buildAnalyticsResidenceTimeline(issue, states, to))
	}
	out := reportsCompletions(timelines, states, to)
	out.Partial = out.Partial || len(issues) != len(ids)
	return out, nil
}

func reportsCompletions(timelines []analyticsResidenceTimeline, states map[string]tracker.NativeState, to time.Time) reportsCompletion {
	out := reportsCompletion{Lanes: []operatortool.AnalyticsLaneDuration{}}
	laneValues := map[string][]float64{}
	var system, lead, working []float64
	for _, timeline := range timelines {
		var done time.Time
		for _, at := range timeline.done {
			if at.Before(to) {
				done = at
			}
		}
		if done.IsZero() || timeline.issue.partial || timeline.started == nil {
			out.Partial = true
			continue
		}
		var controlled, active float64
		issueLanes := map[string]float64{}
		for _, span := range timeline.spans {
			if analyticsResidenceGroup(span.lane, states) == "excluded" {
				continue
			}
			seconds := max(0, minTime(span.to, done).Sub(span.from).Seconds())
			issueLanes[span.lane] += seconds
			if analyticsResidenceGroup(span.lane, states) == "system" {
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
	}
	return reportsCompletionDurations(out, states, system, lead, working, laneValues)
}

func reportsCompletionDurations(out reportsCompletion, states map[string]tracker.NativeState, system, lead, working []float64, lanes map[string][]float64) reportsCompletion {
	out.System = analyticsDuration(system)
	out.Lead = analyticsDuration(lead)
	out.Working = analyticsDuration(working)
	for lane, values := range lanes {
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
			seconds := closedAttemptSeconds(a.Phases)
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

func (s *Service) readReportsPrior(ctx context.Context, q nativeQueryer, scope nativeScope, prior operatortool.AnalyticsWindow, out *hostedReportsReport) error {
	cost, usageRows, err := readProjectSpend(ctx, q, scope, prior.From, prior.To)
	if err != nil {
		return err
	}
	if usageRows > 0 {
		out.PriorCost = &cost
	}
	var attempts, failed int
	if err := q.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(a.status='failed'),0) FROM native_attempts a WHERE a.organization_id=? AND a.project_id=? AND julianday(a.started_at)<julianday(?) AND julianday(a.updated_at)>=julianday(?) AND julianday(a.updated_at)<julianday(?)`, scope.organization, scope.project, formatHubTime(prior.To), formatHubTime(prior.From), formatHubTime(prior.To)).Scan(&attempts, &failed); err != nil {
		return fmt.Errorf("read prior failed attempts: %w", err)
	}
	if attempts > 0 {
		out.PriorFailed = &failed
	}
	landings, err := nativeAnalyticsLandings(ctx, q, scope, prior)
	if err != nil {
		return fmt.Errorf("read prior landings: %w", err)
	}
	population := nativeAnalyticsProject{Partial: len(landings) > maxAnalyticsPopulation}
	population.Landings.Items = landings
	rate, available, err := s.reportsFirstTry(ctx, scope, population)
	if err != nil {
		return fmt.Errorf("read prior first try: %w", err)
	}
	if available {
		out.PriorFirst = &rate
	}
	return nil
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
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	rows, err := s.database.reader.QueryContext(ctx, `SELECT coalesce(json_extract(data_json,'$.runtime.landing'),'null') FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND julianday(started_at)<=julianday(?) AND (json_extract(data_json,'$.runtime.identity.role')='merge' OR json_extract(data_json,'$.runtime.landing') IS NOT NULL) ORDER BY started_at,id LIMIT ?`, scope.organization, scope.project, landing.WorkItemID, formatHubTime(landing.Landing.LandedAt), maxAnalyticsPopulation+1)
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
