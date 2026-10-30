package hubanalytics

import (
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

func MergeDurations(durations ...operatortool.AnalyticsDuration) operatortool.AnalyticsDuration {
	var samples []float64
	for _, d := range durations {
		if len(d.Histogram) > 0 {
			return MergeHistogramDurations(durations...)
		}
		samples = append(samples, d.Samples...)
	}
	return SampledDuration(samples)
}

func MergeLaneDurations(groups ...[]operatortool.AnalyticsLaneDuration) []operatortool.AnalyticsLaneDuration {
	byLane := map[string][]operatortool.AnalyticsDuration{}
	group := map[string]string{}
	for _, lanes := range groups {
		for _, lane := range lanes {
			byLane[lane.Lane] = append(byLane[lane.Lane], lane.AnalyticsDuration)
			group[lane.Lane] = lane.Group
		}
	}
	out := []operatortool.AnalyticsLaneDuration{}
	for lane, durations := range byLane {
		out = append(out, operatortool.AnalyticsLaneDuration{Lane: lane, Group: group[lane], AnalyticsDuration: MergeDurations(durations...)})
	}
	slices.SortFunc(out, func(a, b operatortool.AnalyticsLaneDuration) int { return strings.Compare(a.Lane, b.Lane) })
	return out
}
