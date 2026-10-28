package hubserver

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"golang.org/x/sync/errgroup"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type reportsCompletion struct {
	Lanes     []operatortool.AnalyticsLaneDuration `json:"lanes"`
	Done      int                                  `json:"done"`
	Cancelled int                                  `json:"cancelled"`
	Cost      float64                              `json:"cost_usd"`
	CostP50   *float64                             `json:"cost_p50_usd,omitempty"`
	CostP90   *float64                             `json:"cost_p90_usd,omitempty"`
	System    operatortool.AnalyticsDuration       `json:"system"`
	Lead      operatortool.AnalyticsDuration       `json:"lead"`
	Working   operatortool.AnalyticsDuration       `json:"working"`
	Partial   bool                                 `json:"partial"`
	perIssue  []float64
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
	Pipeline      pipelineReport         `json:"pipeline"`
	Requests      *requestMetricsReport  `json:"requests,omitempty"`
	Analytics     nativeAnalyticsProject `json:"analytics"`
	Completion    reportsCompletion      `json:"completion"`
	Previous      reportsCompletion      `json:"previous"`
	Series        []reportsSeriesBucket  `json:"series"`
	FailedIssues  int                    `json:"failed_issues"`
	Runners       []reportsRunner        `json:"runners"`
	OpenWork      reportsOpenWork        `json:"open_work"`
	Prior         reportsPrior           `json:"prior"`
	Stages        []reportsStage         `json:"stages"`
	Spend         []reportsSpend         `json:"spend"`
	Throughput    []reportsBucket        `json:"throughput"`
	Titles        map[string]string      `json:"titles"`
	Coverage      []diagnosticsCoverage  `json:"coverage"`
	Rework        []reportClassTotal     `json:"rework_classes"`
	Failures      []reportClassTotal     `json:"failure_classes"`
	Tokens        int64                  `json:"tokens"`
	Input         int64                  `json:"input"`
	Cached        int64                  `json:"cached"`
	Cost          float64                `json:"cost_usd"`
	FirstTry      *float64               `json:"first_try_percent"`
	Cancelled     int                    `json:"cancelled"`
	PriorCost     *float64               `json:"previous_cost_usd,omitempty"`
	PriorFirst    *float64               `json:"previous_first_try_percent"`
	PriorFailed   *int                   `json:"previous_failed,omitempty"`
	Unavailable   []string               `json:"unavailable"`
	PriorRecorded bool                   `json:"prior_recorded"`
	firstTry      reportsRatio
	priorFirst    reportsRatio
	// firstTryMissing and priorFirstMissing mark a rate that had landings
	// to measure but could not be computed, as opposed to having none.
	firstTryMissing   bool
	priorFirstMissing bool
}

// reportsRatio is the count behind a percentage, so that the percentages of
// several projects combine exactly.
type reportsRatio struct{ part, total int }

const (
	reportsProjectWorkers  = 4
	reportsAllProjectsTime = 2 * hubReadTimeout
)

// reportsReadableProjects is the projects a report may read: the
// credential's granted projects that also pass the read authorization, all
// of them for `all` or the one requested.
func (s *Service) reportsReadableProjects(ctx context.Context, credential apiCredential, project string) ([]string, bool, error) {
	granted, err := s.usageReadableProjects(ctx, credential)
	if err != nil {
		return nil, false, err
	}
	if project != reportsAllProjects {
		return []string{project}, slices.Contains(granted, project), nil
	}
	readable := slices.DeleteFunc(granted, func(id string) bool {
		_, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: id})
		return err != nil
	})
	return readable, len(readable) > 0, nil
}

func (s *Service) hostedReports(c echo.Context) error {
	ctx := c.Request().Context()
	credential, _, err := s.hostedCredential(ctx, c)
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	params := c.QueryParams()
	project := params.Get("project")
	scope := params.Get("scope")
	if scope != "" && (scope != "organization" || project != reportsAllProjects) {
		return s.nativeAPIError(c, nativePublicInvalid("Organization report scope requires all projects"))
	}
	selected, found, err := s.reportsReadableProjects(ctx, credential, project)
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	if !found {
		return s.nativeAPIError(c, nativeNotFound())
	}
	w, err := reportsWindow(params, s.config.now())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	filter, err := parseReportsFilter(params)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	compare := params.Get("compare")
	if compare != "" && compare != "prior" && compare != "off" {
		return s.nativeAPIError(c, nativePublicInvalid("compare must be prior or off"))
	}
	if scope == "organization" {
		if !w.From.Equal(w.From.Truncate(time.Hour)) || !w.To.Equal(w.To.Truncate(time.Hour)) {
			return s.nativeAPIError(c, nativePublicInvalid("Organization report windows use complete UTC hours"))
		}
		projectFilter := reportsQueryValues(params, "projects")
		if len(projectFilter) > reportsMaxFilterValues || len(strings.Join(projectFilter, "\x00")) > 4096 {
			return s.nativeAPIError(c, nativePublicInvalid("Report project filter accepts at most 32 values"))
		}
		selected = filterProductivityProjects(selected, projectFilter)
		report, err := s.readOrganizationProductivity(ctx, selected, w, filter)
		if err != nil {
			s.config.Logger.Warn("read organization reports", "error", err)
			return s.hostedJSONError(c, http.StatusServiceUnavailable, "Reports are temporarily unavailable")
		}
		return c.JSON(http.StatusOK, report)
	}
	var report hostedReportsReport
	if project == reportsAllProjects {
		report, err = s.readOrganizationReport(ctx, credential, selected, w, filter)
	} else {
		var readable bool
		report, readable, err = s.readProjectReport(ctx, credential, project, w, filter)
		if err == nil && !readable {
			err = errors.New("project report not readable")
		}
	}
	if err != nil {
		s.config.Logger.Warn("read reports", "project", project, "error", err)
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "Reports are temporarily unavailable")
	}
	if compare == "off" {
		report = withoutPriorPeriod(report)
	}
	return c.JSON(http.StatusOK, report)
}

func (s *Service) readOrganizationReport(ctx context.Context, credential apiCredential, projects []string, w operatortool.AnalyticsWindow, filter reportsFilter) (hostedReportsReport, error) {
	ctx, cancel := context.WithTimeout(ctx, reportsAllProjectsTime)
	defer cancel()
	reports := make([]hostedReportsReport, len(projects))
	readable := make([]bool, len(projects))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(reportsProjectWorkers)
	for i, id := range projects {
		group.Go(func() error {
			report, ok, err := s.readProjectReport(groupCtx, credential, id, w, filter)
			if err != nil {
				return fmt.Errorf("read project %s: %w", id, err)
			}
			reports[i], readable[i] = report, ok
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return hostedReportsReport{}, err
	}
	read := make([]hostedReportsReport, 0, len(reports))
	for i, report := range reports {
		if readable[i] {
			read = append(read, report)
		}
	}
	if len(read) == 0 {
		return hostedReportsReport{}, errors.New("no project report readable")
	}
	return mergeHostedReports(read, w), nil
}

// hostedReportOptions lists the values the filter chips offer, read only
// from the projects the viewer's report can read. It is a separate read so
// the report itself runs only its own statements.
func (s *Service) hostedReportOptions(c echo.Context) error {
	ctx := c.Request().Context()
	credential, _, err := s.hostedCredential(ctx, c)
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	params := c.QueryParams()
	projects, found, err := s.reportsReadableProjects(ctx, credential, params.Get("project"))
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	if !found {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if params.Get("project") != reportsAllProjects {
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: projects[0]}); err != nil {
			return s.nativeAPIError(c, nativeNotFound())
		}
	}
	w, err := reportsWindow(params, s.config.now())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	ctx, reporterErr := prepareReporterReadContext(ctx, s.database.reader, nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), credential: credential})
	if reporterErr != nil {
		return s.nativeAPIError(c, reporterErr)
	}
	facets, err := s.readReportsFacets(ctx, projects, w)
	if err != nil {
		s.config.Logger.Warn("read report options", "error", err)
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "Report filters are temporarily unavailable")
	}
	return c.JSON(http.StatusOK, facets)
}

// readProjectReport reads one project's report, narrowed by the filter. A
// project the credential cannot read returns false.
func (s *Service) readProjectReport(ctx context.Context, credential apiCredential, project string, w operatortool.AnalyticsWindow, filter reportsFilter) (hostedReportsReport, bool, error) {
	scope := nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), project: tracker.ProjectID(project), credential: credential}
	ctx, reporterErr := prepareReporterReadContext(ctx, s.database.reader, scope)
	if reporterErr != nil {
		return hostedReportsReport{}, false, reporterErr
	}
	ctx, err := withReportsScope(ctx, scope, filter, w.From.Add(-w.To.Sub(w.From)), w.To)
	if err != nil {
		return hostedReportsReport{}, false, err
	}
	ctx = withReportsQualityInputs(ctx, w)
	done := timeSpan(ctx, "reports_analytics")
	analytics, err := s.readAnalyticsReport(ctx, credential, operatortool.AnalyticsRequest{ProjectID: project, Limit: maxAnalyticsPopulation}, w)
	done()
	if err != nil {
		return hostedReportsReport{}, false, fmt.Errorf("read analytics: %w", err)
	}
	if len(analytics.Projects) != 1 {
		return hostedReportsReport{}, false, nil
	}
	report, err := s.readHostedReports(ctx, analytics)
	if err != nil {
		return report, false, fmt.Errorf("read report: %w", err)
	}
	done = timeSpan(ctx, "reports_prior_quality")
	err = s.reportsPriorQuality(ctx, scope, analytics.Window, &report.Prior)
	done()
	if err != nil {
		return report, false, fmt.Errorf("read prior quality: %w", err)
	}
	report.PriorRecorded = report.PriorCost != nil || report.PriorFirst != nil || report.Previous.Done+report.Previous.Cancelled > 0 || report.PriorFailed != nil && *report.PriorFailed > 0
	return report, true, nil
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
	done := timeSpan(ctx, "reports_diagnostics")
	diagnostics, err := s.readHostedDiagnosticsFrom(ctx, analytics, q)
	done()
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
	q, err = prepareReportsQuery(ctx, q, scope, "report_attempts", "report_events")
	if err != nil {
		return out, err
	}

	states, err := analyticsResidenceStates(ctx, q, scope)
	if err != nil {
		return out, err
	}
	var shipped, priorShipped []nativeShippedIssue
	done = timeSpan(ctx, "reports_completion")
	out.Completion, shipped, err = readReportsCompletion(ctx, q, scope, states, w.From, w.To)
	done()
	if err != nil {
		return out, err
	}
	done = timeSpan(ctx, "reports_prior_completion")
	out.Previous, priorShipped, err = readReportsCompletion(ctx, q, scope, states, w.From.Add(-w.To.Sub(w.From)), w.From)
	done()
	if err != nil {
		return out, err
	}
	inputs := reportsInputs{done: reportsDoneFrom(shipped), priorDone: reportsDoneFrom(priorShipped), timing: func(ids []string, to time.Time) (reportsCompletion, error) {
		return readShippedCompletionTiming(ctx, q, scope, states, ids, to)
	}}
	done = timeSpan(ctx, "reports_residence")
	err = readReportsResidence(ctx, q, scope, w, &inputs)
	done()
	if err != nil {
		return out, err
	}
	for _, issue := range shipped {
		if i := nativeAnalyticsBucketIndex(issue.ClosedAt, w); i >= 0 {
			out.Throughput[i].Done++
		}
	}
	done = timeSpan(ctx, "reports_usage")
	rows, err := readUsageRows(ctx, q, analytics.OrganizationID, usageWindow{From: w.From, To: w.To}, []string{p.ProjectID}, maxAnalyticsPopulation+1)
	done()
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
	done = timeSpan(ctx, "reports_spend")
	out.Cost, _, err = readProjectSpend(ctx, q, scope, w.From, w.To)
	done()
	if err != nil {
		return out, err
	}
	done = timeSpan(ctx, "reports_totals")
	out.Stages, out.Spend = reportsAttemptTotals(p.allAttempts, usage)
	out.Runners = reportsRunnerTotals(p.allAttempts, usage)
	reportsSpendOutcomes(out.Spend, p.allAttempts, inputs)
	out.OpenWork = reportsOpenWorkFor(inputs, p.LaneResidence.Lanes, w)
	out.Prior = reportsPrior{LaneSeconds: inputs.lanes(w.From.Add(-w.To.Sub(w.From)), w.From)}
	out.Rework = reportReworkClasses(p.Quality.returns, p.allAttempts, usage)
	out.Failures = reportFailureClasses(p.failures)
	reportClassDetails(out.Rework, out.Failures, p.Quality.returns, p.failures)
	done()
	done = timeSpan(ctx, "reports_tracking")
	err = s.reportsTrackingIssues(ctx, scope, out.Failures, p.failures)
	done()
	if err != nil {
		return out, err
	}
	done = timeSpan(ctx, "reports_series")
	out.Series, err = reportsSeries(inputs, p.allAttempts, usage, w)
	if err == nil {
		reportsSeriesClasses(out.Series, p.Quality.returns, p.failures, w)
	}
	done()
	if err != nil {
		return out, err
	}
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
	done = timeSpan(ctx, "reports_first_try")
	first, total, available, err := s.reportsFirstTryCounts(ctx, q, scope, firstTryPopulation)
	done()
	if err != nil {
		return out, err
	}
	if available {
		out.firstTry = reportsRatio{first, total}
		out.FirstTry = new(100 * float64(first) / float64(total))
	} else {
		out.Unavailable = append(out.Unavailable, "landed_first_try")
		out.firstTryMissing = len(firstTryPopulation.Landings.Items) > 0
	}
	out.Cancelled = out.Completion.Cancelled
	done = timeSpan(ctx, "reports_prior")
	err = s.readReportsPrior(ctx, q, scope, operatortool.AnalyticsWindow{From: w.From.Add(-w.To.Sub(w.From)), To: w.From}, &out)
	done()
	if err != nil {
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
		if class.TrackingWorkItemID != "" {
			titleIDs = append(titleIDs, class.TrackingWorkItemID)
		}
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
	var perIssue map[string]float64
	out.Cost, perIssue, err = readNativeShippedSpend(ctx, q, scope, ids, to)
	if len(perIssue) > 0 {
		costs := make([]float64, 0, len(perIssue))
		for _, cost := range perIssue {
			costs = append(costs, cost)
		}
		spread := sampledDuration(costs)
		out.CostP50, out.CostP90 = &spread.P50Seconds, &spread.P90Seconds
		out.perIssue = spread.Samples
	}
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
	out.System = sampledDuration(system)
	out.Lead = sampledDuration(lead)
	out.Working = sampledDuration(working)
	for lane, values := range lanes {
		out.Lanes = append(out.Lanes, operatortool.AnalyticsLaneDuration{Lane: lane, Group: analyticsResidenceGroup(lane, states), AnalyticsDuration: sampledDuration(values)})
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
		row.Duration = sampledDuration(durations[key])
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
	var attempts int
	scoped, scopedArgs, err := reportsScopedClause(ctx, "a")
	if err != nil {
		return err
	}
	attemptSource := "native_attempts"
	if ownership, ok := reportsScopeFrom(ctx); ok && ownership.attempts.attempts() {
		attemptSource = analyticsAttemptsInStartedOrder(q)
	}
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM `+attemptSource+` a WHERE a.organization_id=? AND a.project_id=? AND a.updated_at>=? AND a.updated_at<? AND julianday(a.started_at)<julianday(?) AND julianday(a.updated_at)>=julianday(?) AND julianday(a.updated_at)<julianday(?)`+scoped, append([]any{scope.organization, scope.project, prior.From.UTC().Add(-24 * time.Hour).Format(time.DateOnly), prior.To.UTC().Add(48 * time.Hour).Format(time.DateOnly), formatHubTime(prior.To), formatHubTime(prior.From), formatHubTime(prior.To)}, scopedArgs...)...).Scan(&attempts); err != nil {
		return fmt.Errorf("read prior attempts: %w", err)
	}
	failures, partial, err := readNativeFailures(ctx, q, scope, "", &prior)
	if err != nil {
		return fmt.Errorf("read prior failed attempts: %w", err)
	}
	if attempts > 0 && !partial {
		failed := len(failures)
		out.PriorFailed = &failed
	}
	landings, err := nativeAnalyticsLandings(ctx, q, scope, prior)
	if err != nil {
		return fmt.Errorf("read prior landings: %w", err)
	}
	population := nativeAnalyticsProject{Partial: len(landings) > maxAnalyticsPopulation}
	population.Landings.Items = landings
	first, total, available, err := s.reportsFirstTryCounts(ctx, q, scope, population)
	if err != nil {
		return fmt.Errorf("read prior first try: %w", err)
	}
	if available {
		out.priorFirst = reportsRatio{first, total}
		out.PriorFirst = new(100 * float64(first) / float64(total))
	} else {
		out.priorFirstMissing = len(landings) > 0
	}
	return nil
}

func (s *Service) reportsFirstTry(ctx context.Context, scope nativeScope, p nativeAnalyticsProject) (float64, bool, error) {
	first, total, available, err := s.reportsFirstTryCounts(ctx, s.database.reader, scope, p)
	if err != nil || !available {
		return 0, false, err
	}
	return 100 * float64(first) / float64(total), true, nil
}

func (s *Service) reportsFirstTryCounts(ctx context.Context, q nativeQueryer, scope nativeScope, p nativeAnalyticsProject) (int, int, bool, error) {
	if p.Partial || len(p.Landings.Items) == 0 {
		return 0, 0, false, nil
	}
	landings := []nativeAnalyticsLanding{}
	seen := map[string]bool{}
	for _, landing := range p.Landings.Items {
		if !seen[string(landing.WorkItemID)] {
			seen[string(landing.WorkItemID)] = true
			landings = append(landings, landing)
		}
	}
	receipts, err := readLandingAttemptReceipts(ctx, q, scope, landings)
	if err != nil {
		return 0, 0, false, err
	}
	first, total := 0, 0
	for i, landing := range landings {
		count, complete, err := landingAttemptCount(landing, receipts[i])
		if err != nil {
			return 0, 0, false, err
		}
		if !complete {
			return 0, 0, false, nil
		}
		if count == 1 {
			first++
		}
		total++
	}
	if total == 0 {
		return 0, 0, false, nil
	}
	return first, total, true, nil
}

// readLandingAttemptReceipts reads, for every landing in one statement, the
// landing receipts of the merge attempts its work item started by the time
// it landed, in start order.
func readLandingAttemptReceipts(ctx context.Context, q nativeQueryer, scope nativeScope, landings []nativeAnalyticsLanding) ([][]string, error) {
	q, prepareErr := prepareReportsQuery(ctx, q, scope, "report_attempts")
	if prepareErr != nil {
		return nil, prepareErr
	}
	type landed struct {
		WorkItemID string `json:"work_item_id"`
		LandedAt   string `json:"landed_at"`
	}
	keys := make([]landed, 0, len(landings))
	for _, landing := range landings {
		keys = append(keys, landed{string(landing.WorkItemID), formatHubTime(landing.Landing.LandedAt)})
	}
	encoded, err := marshalNative(keys)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT l.key,a.started_at,a.id,a.data_json FROM json_each(?) l
CROSS JOIN report_attempts a ON a.organization_id=? AND a.project_id=? AND a.work_item_id=json_extract(l.value,'$.work_item_id') AND julianday(+a.started_at)<=julianday(json_extract(l.value,'$.landed_at'))
AND (a.role='merge' OR a.has_landing_receipt=1)`, encoded, scope.organization, scope.project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type receipt struct{ started, id, raw string }
	found := make([][]receipt, len(landings))
	for rows.Next() {
		var landing int
		var r receipt
		if err := rows.Scan(&landing, &r.started, &r.id, &r.raw); err != nil {
			return nil, err
		}
		var data tracker.NativeRunData
		if err := decodeActivityRun(ctx, q, r.raw, &data); err != nil {
			return nil, err
		}
		var landingReceipt *tracker.NativeLandingReceipt
		if data.Runtime != nil {
			landingReceipt = data.Runtime.Landing
		}
		r.raw, err = marshalNative(landingReceipt)
		if err != nil {
			return nil, err
		}
		found[landing] = append(found[landing], r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([][]string, len(landings))
	for i, receipts := range found {
		slices.SortFunc(receipts, func(a, b receipt) int {
			return cmp.Or(strings.Compare(a.started, b.started), strings.Compare(a.id, b.id))
		})
		for _, r := range receipts[:min(len(receipts), maxAnalyticsPopulation+1)] {
			out[i] = append(out[i], r.raw)
		}
	}
	return out, nil
}

// landingAttemptCount is the number of merge attempts up to and including
// the one that landed the change, and false when that attempt is not among
// the recorded receipts.
func landingAttemptCount(landing nativeAnalyticsLanding, receipts []string) (int, bool, error) {
	count := 0
	for _, raw := range receipts {
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
	return 0, false, nil
}
