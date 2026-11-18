package reportdata

import (
	"slices"
	"sort"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

func Duration(values []float64) operatortool.AnalyticsDuration {
	out := operatortool.AnalyticsDuration{Count: len(values)}
	if len(values) == 0 {
		return out
	}
	sort.Float64s(values)
	for _, seconds := range values {
		out.Seconds += seconds
	}
	quantile := func(q float64) float64 {
		index := q * float64(len(values)-1)
		lower := int(index)
		upper := min(lower+1, len(values)-1)
		return values[lower] + (values[upper]-values[lower])*(index-float64(lower))
	}
	out.P50Seconds, out.P90Seconds = quantile(0.5), quantile(0.9)
	return out
}
func SampledDuration(values []float64) operatortool.AnalyticsDuration {
	out := Duration(values)
	if len(values) > 0 {
		out.Samples = slices.Clone(values)
	}
	return out
}
