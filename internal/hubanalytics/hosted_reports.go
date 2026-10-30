package hubanalytics

import (
	"sort"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Completion struct {
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
	PerIssue  []float64                            `json:"-"`
}

func CompletionDurations(out Completion, states map[string]tracker.NativeState, system, lead, working []float64, lanes map[string][]float64) Completion {
	out.System = SampledDuration(system)
	out.Lead = SampledDuration(lead)
	out.Working = SampledDuration(working)
	for lane, values := range lanes {
		out.Lanes = append(out.Lanes, operatortool.AnalyticsLaneDuration{Lane: lane, Group: ResidenceGroup(lane, states), AnalyticsDuration: SampledDuration(values)})
	}
	sort.Slice(out.Lanes, func(i, j int) bool { return out.Lanes[i].Lane < out.Lanes[j].Lane })
	return out
}
