package hubserver

import (
	"github.com/digitaldrywood/detent/internal/tracker"
)

type analyticsEventProjection struct {
	FromState *string                          `json:"from_state"`
	ToState   *string                          `json:"to_state"`
	Decision  *tracker.NativeSchedulerDecision `json:"decision"`
	Run       *struct {
		AttemptID string `json:"attempt_id"`
	} `json:"run"`
}
