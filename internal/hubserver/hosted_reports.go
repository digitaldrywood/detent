package hubserver

import (
	"context"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/labstack/echo/v4"

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
	return s.hostedReportsPostgres(c)
}

// hostedReportOptions lists the values the filter chips offer, read only
// from the projects the viewer's report can read. It is a separate read so
// the report itself runs only its own statements.
func (s *Service) hostedReportOptions(c echo.Context) error {
	return s.hostedReportOptionsPostgres(c)
}

// readProjectReport reads one project's report, narrowed by the filter. A
// project the credential cannot read returns false.
func (s *Service) readProjectReport(ctx context.Context, credential apiCredential, project string, w operatortool.AnalyticsWindow, filter reportsFilter) (hostedReportsReport, bool, error) {
	if s.config.TenantRepository == nil {
		return hostedReportsReport{}, false, errors.New("PostgreSQL report reader is unavailable")
	}
	projects, err := s.analyticsReadableProjects(ctx, credential, operatortool.AnalyticsRequest{ProjectID: project})
	if err != nil || len(projects) != 1 {
		return hostedReportsReport{}, false, err
	}
	report, err := s.readPostgresHostedReport(ctx, credential, projects, w, filter, true)
	return report, err == nil, err
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
