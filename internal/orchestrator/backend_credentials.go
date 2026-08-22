package orchestrator

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type backendCredentialChangeRequest struct {
	scope backendcapacity.Scope
	at    time.Time
	reply chan bool
}

func (o *Orchestrator) BackendCredentialChanged(ctx context.Context, scope backendcapacity.Scope) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	request := backendCredentialChangeRequest{
		scope: scope.Normalize(),
		at:    o.clockNow(),
		reply: make(chan bool, 1),
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-o.done:
		return false, ErrStopped
	case o.credentialChanges <- request:
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-o.done:
		return false, ErrStopped
	case scheduled := <-request.reply:
		return scheduled, nil
	}
}

func (o *Orchestrator) scheduleBackendCredentialProbe(state *State, scope backendcapacity.Scope, changedAt time.Time) bool {
	if state == nil {
		return false
	}
	key, outage, ok := matchingBackendOutage(state.BackendOutages, scope)
	if !ok || strings.TrimSpace(outage.ProbeIssueID) != "" {
		return false
	}
	if changedAt.IsZero() {
		changedAt = o.clockNow()
	}
	outage.NextProbeAt = changedAt
	outage.ProbeAttempts = 0
	state.BackendOutages[key] = outage
	for issueID, retry := range state.Retry {
		if !retry.CapacityScope.Matches(outage.Scope) {
			continue
		}
		retry.DueAt = changedAt
		state.Retry[issueID] = retry
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      changedAt,
		Event:   "backend_capacity_credential_changed",
		Message: "backend " + outage.Scope.BackendID + " credentials changed; capacity probe scheduled",
	})
	if o.logger != nil {
		telemetry.LogLifecycleMessage(o.logger, slog.LevelInfo, telemetry.LifecycleSafetyControl, "backend_capacity_credential_changed", "backend credentials changed during capacity outage", telemetry.LifecycleCorrelation{ProjectID: o.cfg.Project.ID},
			"backend_id", outage.Scope.BackendID,
			"backend_kind", outage.Scope.BackendKind,
			"provider", outage.Scope.Provider,
			"probe_at", changedAt,
		)
	}
	return true
}
