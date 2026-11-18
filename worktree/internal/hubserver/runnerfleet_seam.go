package hubserver

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerfleet"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func applyRunnerProblemView(r *runnerauth.Runner, spriteName string, now time.Time) {
	runnerfleet.ApplyProblemView(r, spriteName, now)
}

func mergeRunnerProblems(previous, current []runnerauth.Problem, protocol int, rejected bool, now time.Time) []runnerauth.Problem {
	return runnerfleet.MergeProblems(previous, current, protocol, rejected, now)
}

func appendRunnerRuntimeProblems(r *runnerauth.Runner, lease runnerstore.FleetLease, now time.Time) {
	runnerfleet.AppendRuntimeProblems(r, lease, now)
}
