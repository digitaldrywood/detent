package reports

import (
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/reportdata"
	"github.com/digitaldrywood/detent/internal/hubserver/requestmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/usagereport"
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

type reportsBucket struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Done   int       `json:"done"`
	Failed int       `json:"failed"`
	Slots  *float64  `json:"slots"`
}

type HostedReportsReport struct {
	Pipeline      PipelineReport         `json:"pipeline"`
	Requests      *requestmetrics.Report `json:"requests,omitempty"`
	Analytics     NativeAnalyticsProject `json:"analytics"`
	Completion    reportsCompletion      `json:"completion"`
	Previous      reportsCompletion      `json:"previous"`
	Series        []reportsSeriesBucket  `json:"series"`
	FailedIssues  int                    `json:"failed_issues"`
	Runners       []reportsRunner        `json:"runners"`
	OpenWork      ReportsOpenWork        `json:"open_work"`
	Prior         reportsPrior           `json:"prior"`
	Stages        []reportdata.Stage     `json:"stages"`
	Spend         []reportdata.Spend     `json:"spend"`
	Throughput    []reportsBucket        `json:"throughput"`
	Titles        map[string]string      `json:"titles"`
	Coverage      []diagnosticsCoverage  `json:"coverage"`
	Rework        []ReportClassTotal     `json:"rework_classes"`
	Failures      []ReportClassTotal     `json:"failure_classes"`
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

type reportsRatio struct{ part, total int }

func reportReworkClasses(returns []qualityReturn, attempts []NativeAnalyticsAttempt, usage map[string][]usagereport.Row) []ReportClassTotal {
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

func ReportFailureClasses(failures []NativeFailure) []ReportClassTotal {
	classes := newReportClassAccumulator()
	for _, f := range failures {
		classes.add(ClassifyFailure(f.Example), f.WorkItem, f.Cost)
	}
	return classes.result()
}

func reportsCompletions(timelines []AnalyticsResidenceTimeline, states map[string]tracker.NativeState, to time.Time) reportsCompletion {
	out := reportsCompletion{Lanes: []operatortool.AnalyticsLaneDuration{}}
	laneValues := map[string][]float64{}
	var system, lead, working []float64
	for _, timeline := range timelines {
		var done time.Time
		for _, at := range timeline.Done {
			if at.Before(to) {
				done = at
			}
		}
		if done.IsZero() || timeline.Issue.Partial || timeline.Started == nil {
			out.Partial = true
			continue
		}
		var controlled, active float64
		issueLanes := map[string]float64{}
		if seed := timeline.seed; seed != nil {
			for lane, seconds := range seed.LaneSeconds {
				if analyticsResidenceGroup(lane, states) != "excluded" {
					issueLanes[lane] = seconds
				}
			}
			controlled, active = seed.SystemSeconds, issueLanes["In Progress"]
		}
		for _, span := range timeline.Spans {
			if analyticsResidenceGroup(span.Lane, states) == "excluded" {
				continue
			}
			seconds := max(0, hubapi.MinTime(span.To, done).Sub(span.From).Seconds())
			issueLanes[span.Lane] += seconds
			if analyticsResidenceGroup(span.Lane, states) == "system" {
				controlled += seconds
			}
			if span.Lane == "In Progress" {
				active += seconds
			}
		}
		for lane, seconds := range issueLanes {
			laneValues[lane] = append(laneValues[lane], seconds)
		}
		system = append(system, controlled)
		working = append(working, active)
		lead = append(lead, done.Sub(timeline.Issue.Created).Seconds())
	}
	return reportsCompletionDurations(out, states, system, lead, working, laneValues)
}

func reportsCompletionDurations(out reportsCompletion, states map[string]tracker.NativeState, system, lead, working []float64, lanes map[string][]float64) reportsCompletion {
	out.System = reportdata.SampledDuration(system)
	out.Lead = reportdata.SampledDuration(lead)
	out.Working = reportdata.SampledDuration(working)
	for lane, values := range lanes {
		out.Lanes = append(out.Lanes, operatortool.AnalyticsLaneDuration{Lane: lane, Group: analyticsResidenceGroup(lane, states), AnalyticsDuration: reportdata.SampledDuration(values)})
	}
	sort.Slice(out.Lanes, func(i, j int) bool { return out.Lanes[i].Lane < out.Lanes[j].Lane })
	return out
}
