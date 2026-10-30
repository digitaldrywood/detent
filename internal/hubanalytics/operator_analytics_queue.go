package hubanalytics

import (
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type CapacityWait struct {
	Source               string    `json:"source"`
	Coverage             string    `json:"coverage"`
	ClaimsObserved       int       `json:"claims_observed"`
	IntervalsObserved    int       `json:"intervals_observed"`
	IntervalsUnavailable int       `json:"intervals_unavailable"`
	Seconds              float64   `json:"seconds"`
	SourceAt             time.Time `json:"source_at"`
	Partial              bool      `json:"partial"`
}

func CapacityInterval(prior, claim *tracker.NativeSchedulerDecision, start, finish time.Time) bool {
	return prior != nil && claim != nil && prior.Source == "native_provider_capacity" && prior.Outcome == "skipped" && claim.Source == "native_claim" && claim.Outcome == "claimed" && prior.RunnerID == claim.RunnerID && prior.WorkItemRevision > 0 && prior.WorkItemRevision == claim.WorkItemRevision && prior.At.Equal(start) && claim.At.Equal(finish) && finish.After(start)
}
