package reports

import (
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/reportdata"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestReportsRequestValidation(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 30, 0, 0, time.UTC)
	for _, tt := range []struct {
		name     string
		query    string
		from, to time.Time
		invalid  bool
	}{
		{"preset", "range=7d", time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC), false},
		{"custom days", "from=2026-10-01&to=2026-10-05", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC), false},
		{"custom through today ends at the last hour", "from=2026-10-12&to=2026-10-13", time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC), false},
		{"ninety days", "from=2026-07-15&to=2026-10-12", time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), false},
		{"over ninety days", "from=2026-07-14&to=2026-10-12", time.Time{}, time.Time{}, true},
		{"reversed", "from=2026-10-05&to=2026-10-01", time.Time{}, time.Time{}, true},
		{"future", "from=2026-10-14&to=2026-10-15", time.Time{}, time.Time{}, true},
		{"one end", "from=2026-10-01", time.Time{}, time.Time{}, true},
		{"unknown range", "range=1y", time.Time{}, time.Time{}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			params, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatal(err)
			}
			w, err := ReportsWindow(params, now)
			if (err != nil) != tt.invalid {
				t.Fatalf("err=%v invalid=%t", err, tt.invalid)
			}
			if !tt.invalid && (!w.From.Equal(tt.from) || !w.To.Equal(tt.to) || (w.To.Sub(w.From)-1)/w.Bucket+1 > 240) {
				t.Fatalf("window=%+v want %s..%s", w, tt.from, tt.to)
			}
		})
	}
	firstHour := time.Date(2026, 10, 13, 0, 30, 0, 0, time.UTC)
	if w, err := ReportsWindow(url.Values{"from": {"2026-10-13"}, "to": {"2026-10-13"}}, firstHour); err != nil || !w.From.Equal(firstHour.Truncate(24*time.Hour)) || !w.To.Equal(firstHour) {
		t.Fatalf("today in its first hour: window=%+v err=%v, want midnight to now", w, err)
	}
	for _, tt := range []struct {
		name    string
		query   string
		want    ReportsFilter
		invalid bool
	}{
		{"none", "", ReportsFilter{Labels: []string{}, Priorities: []string{}, Types: []string{}, FiledBy: []string{}, Runners: []string{}, Models: []string{}, Stages: []string{}}, false},
		{"repeated and bracketed", "labels=b&labels[]=a&labels=b&priorities=0&types=bug&filed_by=p&runners=r&models=m&stages=code", ReportsFilter{Labels: []string{"a", "b"}, Priorities: []string{"0"}, Types: []string{"bug"}, FiledBy: []string{"p"}, Runners: []string{"r"}, Models: []string{"m"}, Stages: []string{"code"}}, false},
		{"priority out of range", "priorities=4", ReportsFilter{}, true},
		{"priority not a number", "priorities=high", ReportsFilter{}, true},
		{"unknown type", "types=epic", ReportsFilter{}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			params, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseReportsFilter(params)
			if (err != nil) != tt.invalid {
				t.Fatalf("err=%v invalid=%t", err, tt.invalid)
			}
			if !tt.invalid && !slicesEqualFilter(got, tt.want) {
				t.Fatalf("filter=%+v want %+v", got, tt.want)
			}
		})
	}
}

func TestMergeHostedReports(t *testing.T) {
	w := operatortool.AnalyticsWindow{From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), Bucket: time.Hour}
	failed := func(n int) *int { return &n }
	cost := func(v float64) *float64 { return &v }
	report := func(done int, cycle []float64, first reportsRatio, priorFailed *int, priorCost *float64, issueCosts []float64) HostedReportsReport {
		return HostedReportsReport{
			Completion:  reportsCompletion{Done: done, System: reportdata.SampledDuration(slices.Clone(cycle)), perIssue: issueCosts, Cost: 1},
			Analytics:   NativeAnalyticsProject{ProjectID: "p", Unavailable: []string{"recorded_usage", "activity_complete_window"}},
			Runners:     []reportsRunner{{RunnerID: "r", Sessions: 1, Duration: reportdata.SampledDuration(slices.Clone(cycle))}},
			Spend:       []reportdata.Spend{{WorkItemID: "wi", Cost: float64(done)}},
			firstTry:    first,
			FirstTry:    first.percent(),
			PriorFailed: priorFailed,
			PriorCost:   priorCost,
			Unavailable: []string{},
		}
	}
	for _, tt := range []struct {
		name        string
		reports     []HostedReportsReport
		done        int
		p50         float64
		firstTry    *float64
		priorFailed *int
		priorCost   *float64
		unavailable []string
	}{
		{"hourly histogram and exact samples", []HostedReportsReport{
			func() HostedReportsReport {
				r := report(2, nil, reportsRatio{1, 2}, failed(1), cost(2), nil)
				d := operatortool.AnalyticsDuration{Count: 2, Seconds: 30, Histogram: []operatortool.AnalyticsDurationBucket{{Count: 1, Minimum: 10, Maximum: 10}, {Count: 1, Minimum: 20, Maximum: 20}}}
				r.Completion.System = d
				r.Runners[0].Duration = d
				return r
			}(),
			report(2, []float64{30, 100}, reportsRatio{1, 2}, failed(1), cost(2), nil),
		}, 4, 25, new(50.0), failed(2), cost(4), []string{"activity_complete_window", "recorded_usage"}},
		{"combined samples and counts", []HostedReportsReport{
			report(2, []float64{10, 20}, reportsRatio{1, 2}, failed(1), cost(2), []float64{1, 3}),
			report(3, []float64{30, 40, 50}, reportsRatio{3, 3}, failed(2), nil, []float64{5}),
		}, 5, 30, new(80.0), failed(3), cost(2), []string{"activity_complete_window", "recorded_usage"}},
		{"a recorded prior period without cost leaves the organization's prior cost unavailable", []HostedReportsReport{
			report(1, []float64{10}, reportsRatio{1, 1}, failed(0), cost(4), nil),
			func() HostedReportsReport {
				r := report(1, []float64{20}, reportsRatio{1, 1}, failed(0), nil, nil)
				r.PriorRecorded = true
				return r
			}(),
		}, 2, 15, new(100.0), failed(0), nil, []string{"activity_complete_window", "recorded_usage"}},
		{"a first-try rate a project could not compute leaves the organization's unavailable", []HostedReportsReport{
			report(1, []float64{10}, reportsRatio{1, 1}, failed(0), nil, nil),
			func() HostedReportsReport {
				r := report(1, []float64{20}, reportsRatio{}, failed(0), nil, nil)
				r.firstTryMissing = true
				return r
			}(),
		}, 2, 15, nil, failed(0), nil, []string{"activity_complete_window", "recorded_usage"}},
		{"a project without a prior count has no prior total", []HostedReportsReport{
			report(1, []float64{10}, reportsRatio{}, failed(1), nil, nil),
			report(1, []float64{20}, reportsRatio{}, nil, nil, nil),
		}, 2, 15, nil, nil, nil, []string{"activity_complete_window", "recorded_usage"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeHostedReports(tt.reports, w)
			if got.Completion.Done != tt.done || got.Completion.System.P50Seconds != tt.p50 || got.Runners[0].Duration.P50Seconds != tt.p50 || got.Runners[0].Sessions != len(tt.reports) {
				t.Fatalf("completion=%+v runners=%+v", got.Completion, got.Runners)
			}
			if (got.FirstTry == nil) != (tt.firstTry == nil) || got.FirstTry != nil && *got.FirstTry != *tt.firstTry {
				t.Fatalf("first try=%v want %v", got.FirstTry, tt.firstTry)
			}
			if (got.PriorFailed == nil) != (tt.priorFailed == nil) || got.PriorFailed != nil && *got.PriorFailed != *tt.priorFailed {
				t.Fatalf("prior failed=%v want %v", got.PriorFailed, tt.priorFailed)
			}
			if (got.PriorCost == nil) != (tt.priorCost == nil) || got.PriorCost != nil && *got.PriorCost != *tt.priorCost {
				t.Fatalf("prior cost=%v want %v", got.PriorCost, tt.priorCost)
			}
			if !slices.Equal(got.Analytics.Unavailable, tt.unavailable) || got.Analytics.ProjectID != ReportsAllProjects || len(got.Spend) != len(tt.reports) || got.Spend[0].Cost < got.Spend[1].Cost {
				t.Fatalf("analytics=%+v spend=%+v", got.Analytics.Unavailable, got.Spend)
			}
		})
	}
}

func TestMergeReportClassesAndCoverage(t *testing.T) {
	observed := func(n int) *int { return &n }
	reports := []HostedReportsReport{
		{
			Failures: []ReportClassTotal{{Class: "Hub request timed out", Count: 3, itemCounts: map[string]int{"wi_a": 1, "wi_b": 2}}},
			Coverage: []diagnosticsCoverage{{Source: "Usage joins", Observed: observed(2), Total: observed(4)}, {Source: "Skip reasons", Observed: observed(1), Total: observed(1)}},
		},
		{
			Failures: []ReportClassTotal{{Class: "Hub request timed out", Count: 5, itemCounts: map[string]int{"wi_c": 4, "wi_a": 1}}},
			Coverage: []diagnosticsCoverage{{Source: "Usage joins", Observed: observed(3), Total: nil}},
		},
	}
	classes := mergeReportClasses(reports, func(r HostedReportsReport) []ReportClassTotal { return r.Failures })
	if len(classes) != 1 || classes[0].Count != 8 || !slices.Equal(classes[0].WorkItems, []string{"wi_c", "wi_a", "wi_b"}) {
		t.Fatalf("classes=%+v, want one class of 8 with its issues ordered by combined count", classes)
	}
	coverage := mergeReportsCoverage(reports)
	if len(coverage) != 2 || coverage[0].Observed == nil || *coverage[0].Observed != 5 || coverage[0].Total != nil || coverage[1].Observed != nil {
		t.Fatalf("coverage=%+v, want usage joins observed 5 with no total and skip reasons unobserved where a project did not report it", coverage)
	}
}
