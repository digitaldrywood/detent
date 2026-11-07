package nativechange

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/runtimeevidence"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (e *nativeExecution) ObserveRuntime(ctx context.Context, observation tracker.NativeRuntimeObservation) (err error) {
	e.diagnosticMu.Lock()
	if observation.LocalAttemptID > 0 {
		e.diagnostic.LocalAttemptID = observation.LocalAttemptID
	}
	if observation.Generation > 0 {
		e.diagnostic.Generation = observation.Generation
	}
	if observation.Phase != "" {
		e.diagnostic.Stage = observation.Phase
	}
	e.diagnosticMu.Unlock()
	defer func() {
		if err != nil {
			err = e.executionError(ctx, err)
			if nativeTransportUnavailable(err) {
				slog.Default().Warn("native runtime observation unavailable", "work_item", e.claim.lease.WorkItemID, "error", err)
				err = nil
			}
		}
	}()
	e.mu.Lock()
	reconnecting, identity := e.reconnecting, e.data.Identity
	e.mu.Unlock()
	if reconnecting && identity != nil {
		if err := e.Start(ctx, *identity); err != nil {
			return err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.runtimeSupported == nil {
		if e.claim.source == nil || e.claim.source.NativeClient() == nil {
			return runner.ErrExecutionAuthorityUnavailable
		}
		supported, err := e.claim.source.NativeClient().HubFeature(ctx, tracker.NativeRuntimeEvidenceCapability)
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
		profile := *observation.Activity
		profile.StartEarlier(e.usageStartedAt)
		profile = workflowmetrics.PublicActivityProfile(profile)
		observation.Activity = &profile
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	previous := e.data.Runtime
	if observation.Validation != nil && observation.Phase == "" && (previous == nil || previous.Phase == "") {
		observation.Phase = "validation"
		observation.HeartbeatAt = observation.Validation.FinishedAt
		if observation.HeartbeatAt.IsZero() {
			observation.HeartbeatAt = e.scheduler.now().UTC()
		}
	}
	observation = runtimeevidence.Merge(previous, observation)
	current := runtimeevidence.Normalize(observation)
	var before tracker.NativeRuntimeObservation
	if previous != nil {
		before = runtimeevidence.Normalize(*previous)
	}
	heartbeatDue := e.runtimeHeartbeatDue(&observation)
	publish := heartbeatDue || previous == nil || len(before.Phases) == 0 && len(current.Phases) > 0 || current.Phase != before.Phase || !reflect.DeepEqual(current.WorkerLaunch, before.WorkerLaunch) || !reflect.DeepEqual(current.Identity, before.Identity) || !reflect.DeepEqual(current.Landing, before.Landing)
	changed := heartbeatDue || previous == nil || !reflect.DeepEqual(current, before)
	if !changed {
		return nil
	}
	e.data.Runtime = &observation
	e.runtimeDirty = true
	if e.data.Identity == nil || !publish {
		return nil
	}
	return e.append(ctx, "run.observed", "", nil)
}

func (e *nativeExecution) runtimeHeartbeatDue(observation *tracker.NativeRuntimeObservation) bool {
	return observation != nil && observation.HeartbeatAt.After(e.runtimePublishedAt) &&
		observation.HeartbeatAt.Sub(e.runtimePublishedAt) >= e.scheduler.leaseTTL/3
}

func (e *nativeExecution) activityBoundary(at time.Time, outcome string) {
	observation := runtimeevidence.ActivityBoundary(e.data.Runtime, e.role, e.usageStartedAt, at, outcome)
	e.data.Runtime = &observation
}

func (e *nativeExecution) StartLanding(ctx context.Context, localAttempt int64, generation uint64) error {
	e.mu.Lock()
	e.role = runner.RoleMerge
	e.mu.Unlock()
	if e.claim.landingCapacity > 0 {
		if err := e.renew(ctx, e.claim.landingCapacity); err != nil {
			if nativeAdmissionCapacityFull(err) {
				return &workspace.LandRefusal{Reason: err.Error()}
			}
			var failure *APIError
			if errors.As(err, &failure) && failure.Code == "no_claimable_work" {
				return &workspace.LandRefusal{Kind: workspace.LandRefusalNothing, Reason: "Current Change version is not ready for native landing"}
			}
			return err
		}
	}
	identity := tracker.NativeExecutionIdentity{Role: "merge", Backend: "git", Model: "none"}
	if reservation := e.claim.lease.ProviderReservation; reservation != nil {
		identity.Role, identity.Backend, identity.Model = reservation.Role, reservation.Backend, reservation.Model
	}
	e.mu.Lock()
	if e.data.Identity != nil {
		identity = *e.data.Identity
	}
	e.mu.Unlock()
	at := e.scheduler.now().UTC()
	runtimeIdentity := runner.NativeLandingIdentity(at)
	observation := tracker.NativeRuntimeObservation{LocalAttemptID: localAttempt, Generation: generation, Phase: "merging", HeartbeatAt: at, Identity: runtimeIdentity}
	if current := e.claim.recovery.Issue.ListChange; current != nil && current.LandingPending {
		target, err := e.LandingTarget(ctx)
		if err != nil {
			return err
		}
		observation.Landing = &tracker.NativeLandingReceipt{Waiting: true, ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: target.HeadSHA, CI: target.CI, ObservedAt: at}
	}
	if err := e.ObserveRuntime(ctx, observation); err != nil {
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
	e.mu.Lock()
	delivery := e.releaseDelivery
	e.mu.Unlock()
	if delivery != nil && delivery.DeliveredSHA != "" && landing.Landed && landing.ChangeID == "" && landing.MergeSHA == delivery.DeliveredSHA && landing.HeadSHA == delivery.SourceMergeSHA && landing.BaseRef == delivery.BaseRef {
		return nil
	}
	return e.ObserveRuntime(ctx, tracker.NativeRuntimeObservation{HeartbeatAt: time.Now().UTC(), Landing: &tracker.NativeLandingReceipt{Incorporation: landing.Incorporation, ConflictFiles: landing.ConflictFiles, SecurityAudit: landing.SecurityAudit, Pipeline: landing.Pipeline, PipelineDropped: landing.PipelineDropped, Waiting: landing.Waiting, Path: landing.Path, Packages: landing.Packages, CI: landing.CI, Gate: landing.Gate, ChangeID: landing.ChangeID, VersionID: landing.VersionID, HeadSHA: landing.HeadSHA, BaseSHA: landing.BaseSHA, Landed: landing.Landed, MergeSHA: landing.MergeSHA, BaseRef: landing.BaseRef, Method: landing.Method, Rebased: landing.Rebased, RefusalKind: landing.RefusalKind, Refusal: landing.RefusalKind, GateFailed: landing.GateFailed, ObservedAt: time.Now().UTC()}})
}

func (e *nativeExecution) FlushRuntime(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.flushObservations(ctx); err != nil {
		return err
	}
	if !e.runtimeDirty || e.data.Identity == nil {
		return nil
	}
	return e.append(ctx, "run.observed", "", nil, true)
}

func (e *nativeExecution) RecordPipelineTiming(timing gate.PipelineTiming) {
	if !timing.Valid() {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	observation := tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: e.scheduler.now().UTC()}
	if e.data.Runtime != nil {
		observation = *e.data.Runtime
	}
	for _, previous := range observation.Pipeline {
		if previous.ReceiptID == timing.ReceiptID {
			return
		}
	}
	if len(observation.Pipeline) < 1024 {
		observation.Pipeline = append(slices.Clone(observation.Pipeline), timing.Public())
	} else {
		observation.PipelineDropped++
	}
	e.data.Runtime = &observation
	e.runtimeDirty = true
}
