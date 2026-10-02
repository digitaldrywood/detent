package hubclient

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func (c *NativeClient) RuntimeEvidence(ctx context.Context, item tracker.NativeWorkItemID, attempt string) (tracker.NativeRuntimeEvidence, error) {
	var result tracker.NativeRuntimeEvidence
	supported, err := c.HubFeature(ctx, tracker.NativeRuntimeEvidenceCapability)
	if err != nil {
		return result, err
	}
	if !supported {
		return result, ErrUnavailable
	}
	path := c.base() + "/work-items/" + url.PathEscape(string(item)) + "/runtime"
	if attempt != "" {
		key := "native_attempt_id"
		if !strings.HasPrefix(attempt, "attempt_") {
			key = "attempt_id"
		}
		path += "?" + key + "=" + url.QueryEscape(attempt)
	}
	err = c.client.request(ctx, http.MethodGet, path, nil, &result)
	return result, err
}

func (e *nativeExecution) ObserveRuntime(ctx context.Context, observation tracker.NativeRuntimeObservation) error {
	publish := observation.Activity != nil || observation.Landing != nil || observation.REST != nil
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.runtimeSupported == nil {
		if e.claim.source == nil || e.claim.source.client == nil {
			return runner.ErrExecutionAuthorityUnavailable
		}
		supported, err := e.claim.source.client.HubFeature(ctx, tracker.NativeRuntimeEvidenceCapability)
		if err != nil {
			return err
		}
		e.runtimeSupported = &supported
	}
	if !*e.runtimeSupported {
		return nil
	}
	if err := e.flush(ctx); err != nil {
		return err
	}
	if observation.Activity != nil {
		profile := workflowmetrics.PublicActivityProfile(*observation.Activity)
		observation.Activity = &profile
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	previous := e.data.Runtime
	if previous != nil {
		observation.PhasesDropped = previous.PhasesDropped
		observation.Phases = append([]tracker.NativePhase(nil), previous.Phases...)
		if observation.Phase == "" {
			observation.Phase = previous.Phase
		}
		if observation.Identity.IsZero() {
			observation.Identity = previous.Identity
		}
		if observation.Activity == nil {
			observation.Activity = previous.Activity
		}
		if observation.Landing == nil {
			observation.Landing = previous.Landing
		}
		if observation.REST == nil {
			observation.REST = previous.REST
		}
		if observation.LocalAttemptID == 0 {
			observation.LocalAttemptID = previous.LocalAttemptID
			observation.Generation = previous.Generation
		}
		if observation.HeartbeatAt.Before(previous.HeartbeatAt) {
			observation.HeartbeatAt = previous.HeartbeatAt
		}
		if observation.Phase != previous.Phase && len(observation.Phases) > 0 && observation.Phases[len(observation.Phases)-1].Name == previous.Phase {
			observation.Phases[len(observation.Phases)-1].FinishedAt = observation.HeartbeatAt
		}
	}
	if previous == nil || observation.Phase != previous.Phase {
		if len(observation.Phases) < 128 {
			observation.Phases = append(observation.Phases, tracker.NativePhase{Name: observation.Phase, StartedAt: observation.HeartbeatAt})
		} else {
			observation.PhasesDropped++
		}
	}
	e.data.Runtime = &observation
	e.runtimeDirty = true
	if e.data.Identity == nil || !publish {
		return nil
	}
	return e.append(ctx, "run.observed", "", nil)
}

func (e *nativeExecution) StartLanding(ctx context.Context, localAttempt int64, generation uint64) error {
	identity := tracker.NativeExecutionIdentity{Role: "merge", Backend: "git", Model: "none"}
	if reservation := e.claim.lease.ProviderReservation; reservation != nil {
		identity.Role, identity.Backend, identity.Model = reservation.Requirement.Role, reservation.Requirement.Backend, reservation.Requirement.Model
	}
	if err := e.ObserveRuntime(ctx, tracker.NativeRuntimeObservation{LocalAttemptID: localAttempt, Generation: generation, Phase: "merging", HeartbeatAt: time.Now().UTC(), Identity: agentidentity.Identity{Role: "merge", BackendKind: "git"}}); err != nil {
		return err
	}
	if err := e.Start(ctx, identity); err != nil {
		return err
	}
	e.mu.Lock()
	e.role = runner.RoleMerge
	e.mu.Unlock()
	return nil
}

func (e *nativeExecution) ObserveLanding(ctx context.Context, landing runner.NativeLanding) error {
	return e.ObserveRuntime(ctx, tracker.NativeRuntimeObservation{HeartbeatAt: time.Now().UTC(), Landing: &tracker.NativeLandingReceipt{ChangeID: landing.ChangeID, VersionID: landing.VersionID, HeadSHA: landing.HeadSHA, Landed: landing.Landed, MergeSHA: landing.MergeSHA, BaseRef: landing.BaseRef, Method: landing.Method, RefusalKind: landing.RefusalKind, ObservedAt: time.Now().UTC()}})
}

func (e *nativeExecution) FlushRuntime(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.runtimeDirty || e.data.Identity == nil {
		return nil
	}
	return e.append(ctx, "run.observed", "", nil)
}
