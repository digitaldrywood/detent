package api

import (
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func NewRunnerState(path string) *RunnerState { return &RunnerState{path: path} }
func (c *Client) Enrollment() *RunnerState    { return c.runner }
func (r *RunnerState) ObserveProblems(problems []runnerauth.Problem, at time.Time) []runnerauth.Problem {
	r.routingMu.Lock()
	defer r.routingMu.Unlock()
	r.problems = runnerauth.MergeProblems(r.problems, problems, at)
	return slices.Clone(r.problems)
}
func (r *RunnerState) RememberHeartbeat(path string, report MachineHeartbeatReport, supported bool) {
	r.routingMu.Lock()
	defer r.routingMu.Unlock()
	r.heartbeatPath = path
	r.heartbeat = report
	r.hostMetricsSupported = supported
}
