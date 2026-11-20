package hubserver

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerfleet"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func applyRunnerProblemView(r *runnerauth.Runner, spriteName string, now time.Time) {
	runnerfleet.ApplyProblemView(r, spriteName, now)
}
