package runnerupdate

import (
	"context"
	"errors"
	"reflect"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func (s *Service) acceptStoredReceipt(ctx context.Context, reader tenantstore.Reader, runnerID string, state runnerstore.UpdateState, urgent runnerstore.UrgentUpdate, next *runnerauth.UpdateReceipt, running runnerauth.BuildEvidence) (bool, bool, error) {
	if failure := next.PreviousFailure; failure != nil {
		record, err := reader.RunnerUpdateRequest(ctx, failure.Request.ID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return false, false, err
		}
		if errors.Is(err, corestore.ErrNotFound) || !record.Request.Matches(failure.Request) || record.RunnerID != runnerID && (record.RunnerID != "" || !record.Request.Urgent) {
			next.PreviousFailure = nil
		}
	}
	known, accepted := acceptReceipt(state, urgent, next, running)
	if known {
		return known, accepted, nil
	}
	record, err := reader.RunnerUpdateRequest(ctx, next.Request.ID)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if !record.Request.Matches(next.Request) || record.RunnerID != runnerID && (record.RunnerID != "" || !record.Request.Urgent) {
		return false, false, nil
	}
	return true, state.Observation == nil || state.Observation.Receipt == nil, nil
}

func acceptReceipt(state runnerstore.UpdateState, urgent runnerstore.UrgentUpdate, next *runnerauth.UpdateReceipt, running runnerauth.BuildEvidence) (bool, bool) {
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
		ApplyUrgentRouting(&runner, UrgentUpdate{Revision: urgent.Revision, Request: urgent.Request}, running.Version)
		if runner.UpdateRequest == nil || !runner.UpdateRequest.Matches(next.Request) {
			return true, false
		}
		next.RetainPreviousFailure(previous)
		return true, true
	}
	next.RetainPreviousFailure(previous)
	nextAt, previousAt := canonicalTime(next.ObservedAt), canonicalTime(previous.ObservedAt)
	if nextAt.Before(previousAt) {
		return true, false
	}
	if nextAt.After(previousAt) {
		return true, true
	}
	if reflect.DeepEqual(next, previous) {
		return true, true
	}
	if previous.Applied != nil && next.Applied == nil || previous.VerifiedTarget != nil && next.VerifiedTarget == nil && next.Applied == nil || previous.Running != nil && next.Running == nil || previous.ExecutionArtifact && !next.ExecutionArtifact {
		return true, false
	}
	if next.Applied != nil && previous.Applied == nil || next.Running != nil && previous.Running == nil || next.ExecutionArtifact && !previous.ExecutionArtifact {
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
