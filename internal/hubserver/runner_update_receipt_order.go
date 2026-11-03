package hubserver

import (
	"reflect"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func acceptRunnerUpdateReceipt(state runnerstore.UpdateState, urgent runnerstore.UrgentUpdate, next *runnerauth.UpdateReceipt, running runnerauth.BuildEvidence) (bool, bool) {
	var previous *runnerauth.UpdateReceipt
	if state.Observation != nil {
		previous = state.Observation.Receipt
	}
	known := state.Desired != nil && state.Desired.Matches(next.Request) || urgent.Request != nil && urgent.Request.Matches(next.Request) || previous != nil && previous.Request.Matches(next.Request)
	if !known {
		return false, false
	}
	if previous == nil {
		return true, true
	}
	if !previous.Request.Matches(next.Request) {
		runner := runnerauth.Runner{Routing: runnerauth.Routing{UpdateRequest: state.Desired}, Update: state.Observation}
		applyUrgentRunnerRouting(&runner, urgentRunnerUpdate{Revision: urgent.Revision, Request: urgent.Request}, running.Version)
		if runner.UpdateRequest == nil || !runner.UpdateRequest.Matches(next.Request) {
			return true, false
		}
		next.RetainPreviousFailure(previous)
		return true, true
	}
	next.RetainPreviousFailure(previous)
	nextAt, previousAt := canonicalRunnerUpdateTime(next.ObservedAt), canonicalRunnerUpdateTime(previous.ObservedAt)
	if nextAt.Before(previousAt) {
		return true, false
	}
	if nextAt.After(previousAt) {
		return true, true
	}
	if reflect.DeepEqual(next, previous) {
		return true, true
	}
	if previous.Applied != nil && next.Applied == nil || previous.VerifiedTarget != nil && next.VerifiedTarget == nil && next.Applied == nil || previous.Running != nil && next.Running == nil {
		return true, false
	}
	if next.Applied != nil && previous.Applied == nil || next.Running != nil && previous.Running == nil {
		return true, true
	}
	if next.Status == previous.Status {
		return true, false
	}
	switch previous.Status {
	case "draining", "applying":
		return true, true
	case "applied":
		return true, next.Status == "restart_requested" || next.Status == "running"
	case "restart_requested":
		return true, next.Status == "running"
	case "running":
		return true, next.Status == "applied" && next.Applied != nil && !running.Matches(*next.Applied) && running.ObservedAt.After(state.Observation.Running.ObservedAt)
	}
	return true, false
}
