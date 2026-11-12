package hubserver

import (
	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runneradvice"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

const capacityGiB = runneradvice.CapacityGiB

type (
	runnerCapacityRead   = runneradvice.CapacityRead
	runnerRecommendation = runneradvice.Recommendation
	slotRange            = runneradvice.SlotRange
	recommendationHour   = runneradvice.Hour
	recommendationRun    = runneradvice.Run
)

func recommendRunner(r runnerauth.Routing, hours []recommendationHour, runs []recommendationRun) (runnerRecommendation, error) {
	return runneradvice.Recommend(r, hours, runs)
}

func runnerRecommendationFromFacts(r runnerauth.Routing, hours []recommendationHour, facts reportstore.RunnerCapacityFacts) (runnerRecommendation, error) {
	return runneradvice.FromFacts(r, hours, facts)
}

func recommendationHoursFromSegments(segments []hostmetrics.Summary) ([]recommendationHour, error) {
	return runneradvice.HoursFromSegments(segments)
}

func mergeHostSummary(a, b hostmetrics.Summary) (hostmetrics.Summary, bool) {
	return runneradvice.MergeHostSummary(a, b)
}
