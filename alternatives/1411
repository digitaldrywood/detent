package planner

import (
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func CompletionDisposition(outcome, message string) *tracker.NativeDisposition {
	disposition := runner.NativeDispositionFromMessage(message)
	if outcome == "succeeded" && disposition == nil {
		return &tracker.NativeDisposition{Status: "in_progress"}
	}
	return disposition
}
