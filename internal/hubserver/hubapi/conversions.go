package hubapi

import (
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/reportdata"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func SampledDuration(values []float64) operatortool.AnalyticsDuration {
	return reportdata.SampledDuration(values)
}

func ValueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func StringsEqualFold(left, right string) bool {
	return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
}

func FormatHubTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func ParseTimeValue(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
}
