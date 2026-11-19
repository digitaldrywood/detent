package hubserver

import (
	"github.com/digitaldrywood/detent/internal/hubserver/runnerfleet"
	"github.com/digitaldrywood/detent/internal/providercapacity"
)

type scopedProviderAccount struct {
	runnerID string
	providercapacity.Report
}

func sharedProviderAccount(a, b scopedProviderAccount) bool {
	return runnerfleet.SharedProviderAccount(
		runnerfleet.ProviderAccount{RunnerID: a.runnerID, Report: a.Report},
		runnerfleet.ProviderAccount{RunnerID: b.runnerID, Report: b.Report},
	)
}

func providerConcurrencyLimit(a, b int) int {
	return runnerfleet.ProviderConcurrencyLimit(a, b)
}
