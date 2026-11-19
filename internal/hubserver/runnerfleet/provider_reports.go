package runnerfleet

import (
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/providercapacity"
)

func ValidateProviderReports(reports []providercapacity.Report) error {
	if err := providercapacity.Validate(reports); err != nil {
		return hubapi.NativeInvalid(err.Error())
	}
	return nil
}

func MergeRunnerProviderReports(previous, reports []providercapacity.Report, now time.Time) []providercapacity.Report {
	merged := slices.Clone(reports)
	if merged == nil {
		merged = []providercapacity.Report{}
	}
	for i, report := range merged {
		for _, prior := range previous {
			if prior.Backend != report.Backend || prior.Provider != report.Provider || prior.AccountAlias != report.AccountAlias || prior.SharedAccountAlias != report.SharedAccountAlias || prior.ObservedAt.After(now) {
				continue
			}
			if report.ObservedAt.After(now) || prior.ObservedAt.After(report.ObservedAt) || prior.ObservedAt.Equal(report.ObservedAt) && prior.Availability == "exhausted" && report.Availability != "exhausted" {
				merged[i] = prior
			}
		}
	}
	return merged
}
