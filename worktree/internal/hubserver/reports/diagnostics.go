package reports

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/requestmetrics"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type diagnosticsCoverage struct {
	Source   string `json:"source"`
	Observed *int   `json:"observed"`
	Total    *int   `json:"total"`
}

type DiagnosticsCapacity struct {
	Hour  time.Time `json:"hour"`
	Slots float64   `json:"slots"`
	Todo  *int      `json:"todo"`
}

type DiagnosticsReport struct {
	Requests      *requestmetrics.Report     `json:"requests,omitempty"`
	From          time.Time                  `json:"from"`
	To            time.Time                  `json:"to"`
	Partial       bool                       `json:"partial"`
	Findings      any                        `json:"findings"`
	DetectorTick  *time.Time                 `json:"detector_tick"`
	Capacity      []DiagnosticsCapacity      `json:"capacity"`
	Claimed       *int                       `json:"claimed"`
	Ready         *int                       `json:"ready"`
	Skipped       *int                       `json:"skipped"`
	SkipReasons   []NativeAnalyticsSkip      `json:"skip_reasons"`
	MergeEntered  int                        `json:"merge_entered"`
	MergeLanded   int                        `json:"merge_landed"`
	MergeWaitP50  *float64                   `json:"merge_wait_p50"`
	MergeWaitP90  *float64                   `json:"merge_wait_p90"`
	MergeWaitMax  *float64                   `json:"merge_wait_max"`
	Refused       []DiagnosticsRefusal       `json:"refused"`
	Coverage      []diagnosticsCoverage      `json:"coverage"`
	Configuration []DiagnosticsConfiguration `json:"configuration"`
}

type DiagnosticsRefusal struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

type DiagnosticsConfiguration struct {
	Name   string  `json:"name"`
	Value  *string `json:"value"`
	Effect *string `json:"effect"`
}

func diagnosticsTodoDepth(issues []AnalyticsResidenceIssue, states map[string]tracker.NativeState, clipped bool, at time.Time) *int {
	if clipped {
		return nil
	}
	count := 0
	for _, issue := range issues {
		if !issue.Created.Before(at) {
			continue
		}
		past := issue
		past.Events = nil
		for _, event := range issue.Events {
			if event.At.Before(at) {
				past.Events = append(past.Events, event)
			}
		}
		timeline := BuildAnalyticsResidenceTimeline(past, states, at)
		if timeline.Issue.Partial {
			return nil
		}
		if timeline.State == "Todo" {
			count++
		}
	}
	return &count
}
