package hubserver

import (
	"math"
	"slices"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

func mergeHistogramDurations(durations ...operatortool.AnalyticsDuration) operatortool.AnalyticsDuration {
	out := operatortool.AnalyticsDuration{}
	bins := map[int]operatortool.AnalyticsDurationBucket{}
	add := func(b operatortool.AnalyticsDurationBucket) {
		key := int(math.Ceil(math.Log2(b.Maximum+1) * 16))
		old, exists := bins[key]
		if !exists {
			bins[key] = b
			return
		}
		old.Count += b.Count
		old.Minimum = min(old.Minimum, b.Minimum)
		old.Maximum = max(old.Maximum, b.Maximum)
		bins[key] = old
	}
	for _, d := range durations {
		out.Count += d.Count
		out.Seconds += d.Seconds
		for _, b := range d.Histogram {
			add(b)
		}
		for _, v := range d.Samples {
			add(operatortool.AnalyticsDurationBucket{Count: 1, Minimum: v, Maximum: v})
		}
	}
	for _, b := range bins {
		out.Histogram = append(out.Histogram, b)
	}
	slices.SortFunc(out.Histogram, func(a, b operatortool.AnalyticsDurationBucket) int {
		if a.Minimum < b.Minimum {
			return -1
		}
		if a.Minimum > b.Minimum {
			return 1
		}
		return 0
	})
	valueAt := func(rank int) float64 {
		for _, b := range out.Histogram {
			if rank < b.Count {
				if b.Count == 1 {
					return (b.Minimum + b.Maximum) / 2
				}
				return b.Minimum + (b.Maximum-b.Minimum)*float64(rank)/float64(b.Count-1)
			}
			rank -= b.Count
		}
		return 0
	}
	quantile := func(q float64) float64 {
		rank := q * float64(max(0, out.Count-1))
		low := int(math.Floor(rank))
		high := int(math.Ceil(rank))
		lower := valueAt(low)
		return lower + (valueAt(high)-lower)*(rank-float64(low))
	}
	out.P50Seconds, out.P90Seconds = quantile(.5), quantile(.9)
	return out
}

type diagnosticsResidenceHour struct {
	transitions int
	todo        *int
	partial     bool
}
