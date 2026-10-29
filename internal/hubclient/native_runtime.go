package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (c *NativeClient) HasRegisteredRunner() bool {
	return c.client.runner != nil
}

func (c *NativeClient) RuntimeEvidence(ctx context.Context, item tracker.NativeWorkItemID, attempt string, admission ...tracker.NativeAdmissionContext) (tracker.NativeRuntimeEvidence, error) {
	var result tracker.NativeRuntimeEvidence
	supported, err := c.HubFeature(ctx, tracker.NativeRuntimeEvidenceCapability)
	if err != nil {
		return result, err
	}
	if !supported {
		return result, ErrUnavailable
	}
	path := c.base() + "/work-items/" + url.PathEscape(string(item)) + "/runtime"
	params := url.Values{}
	if attempt != "" {
		key := "native_attempt_id"
		if !strings.HasPrefix(attempt, "attempt_") {
			key = "attempt_id"
		}
		params.Set(key, attempt)
	}

	if c.client.runner != nil && len(admission) > 0 {
		supported, err := c.HubFeature(ctx, tracker.NativeAdmissionEvidenceCapability)
		if err != nil {
			return result, err
		}
		if supported {
			raw, err := json.Marshal(admission[0])
			if err != nil {
				return result, err
			}
			params.Set("admission", string(raw))
		}
	}
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	err = c.client.request(ctx, http.MethodGet, path, nil, &result)
	return result, err
}

func (c *NativeClient) GitHubTimings(ctx context.Context, item tracker.NativeWorkItemID, attempt, runnerID string) (tracker.NativeGitHubTimingEvidence, error) {
	var result tracker.NativeGitHubTimingEvidence
	params := url.Values{"native_attempt_id": {attempt}, "runner_id": {runnerID}}
	path := c.base() + "/work-items/" + url.PathEscape(string(item)) + "/runtime/github-timings?" + params.Encode()
	err := c.client.request(ctx, http.MethodGet, path, nil, &result)
	return result, err
}

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
		profile := *observation.Activity
		profile.StartEarlier(e.usageStartedAt)
		profile = workflowmetrics.PublicActivityProfile(profile)
		observation.Activity = &profile
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	previous := e.data.Runtime
	if previous != nil {
		if observation.WorkerLaunch == nil {
			observation.WorkerLaunch = previous.WorkerLaunch
		}
		observation.Pipeline, observation.PipelineDropped = gate.MergePipeline(previous.Pipeline, observation.Pipeline, max(previous.PipelineDropped, observation.PipelineDropped))
		if observation.Validation == nil {
			observation.Validation = previous.Validation
		}
		if observation.Recovery == nil {
			observation.Recovery = previous.Recovery
		}
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
		if observation.Completion == nil {
			observation.Completion = previous.Completion
		}
		if observation.REST == nil {
			observation.REST = previous.REST
		}
		if observation.GitHub == nil {
			observation.GitHub = previous.GitHub
		}
		if observation.LocalAttemptID == 0 {
			observation.LocalAttemptID = previous.LocalAttemptID
			observation.Generation = previous.Generation
		}
		if previous.LocalAttemptID == 0 && observation.LocalAttemptID > 0 && observation.Activity != nil && observation.Activity.SessionID == 0 {
			profile := *observation.Activity
			profile.AttemptID, profile.Generation = observation.LocalAttemptID, observation.Generation
			observation.Activity = &profile
		}
		if observation.HeartbeatAt.Before(previous.HeartbeatAt) {
			observation.HeartbeatAt = previous.HeartbeatAt
		}
		if observation.Phase != previous.Phase && len(observation.Phases) > 0 && observation.Phases[len(observation.Phases)-1].Name == previous.Phase {
			observation.Phases[len(observation.Phases)-1].FinishedAt = observation.HeartbeatAt
		}
	}
	if previous == nil || len(observation.Phases) == 0 || observation.Phase != previous.Phase {
		if len(observation.Phases) < 128 {
			observation.Phases = append(observation.Phases, tracker.NativePhase{Name: observation.Phase, StartedAt: observation.HeartbeatAt})
		} else {
			observation.PhasesDropped++
		}
	}
	current := runtimeEvidence(observation)
	var before tracker.NativeRuntimeObservation
	if previous != nil {
		before = runtimeEvidence(*previous)
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
	observation := tracker.NativeRuntimeObservation{}
	if e.data.Runtime != nil {
		observation = *e.data.Runtime
	}
	stage := observation.Phase
	if stage == "" || stage == "completed" {
		switch e.role {
		case runner.RoleMerge:
			stage = "merging"
		case runner.RoleRework:
			stage = "rework"
		case runner.RolePlan:
			stage = "planning"
		case runner.RoleValidator:
			stage = "validation"
		default:
			stage = "implementation"
		}
	}
	if observation.Phase == "" {
		observation.Phase = stage
	}
	profile := workflowmetrics.ActivityProfile{
		Schema: 1, AttemptID: observation.LocalAttemptID, Generation: observation.Generation,
		Stage: stage, StartedAt: e.usageStartedAt, AsOf: at, Status: "running", Coverage: "partial",
	}
	if observation.Activity != nil {
		profile = *observation.Activity
		profile.StartEarlier(e.usageStartedAt)
	}
	if outcome != "" {
		if at.Before(profile.AsOf) {
			at = profile.AsOf
		}
		profile.AsOf, profile.FinishedAt = at, at
		if profile.Status == "running" || profile.Status == "ended_without_terminal_event" {
			switch outcome {
			case "succeeded":
				profile.Status = "completed"
			case "interrupted", "cancelled":
				profile.Status = "cancelled"
			default:
				profile.Status = "failed"
			}
		}
		if e.role == runner.RoleMerge && observation.Identity.BackendKind == "git" && profile.SessionID == 0 {
			started := e.usageStartedAt
			for _, phase := range observation.Phases {
				if phase.Name == "merging" && phase.StartedAt.After(started) {
					started = phase.StartedAt
				}
			}
			profile.Spans = slices.Clone(profile.Spans)
			profile.Spans = append(profile.Spans, workflowmetrics.ActivitySpan{ID: "native_landing", Kind: "merge", Evidence: "git_merge", Attribution: "observed", StartedAt: started, FinishedAt: at, Outcome: profile.Status})
		}
	}
	profile = workflowmetrics.PublicActivityProfile(profile)
	observation.Activity = &profile
	observation.HeartbeatAt = at
	e.data.Runtime = &observation
}

func runtimeEvidence(observation tracker.NativeRuntimeObservation) tracker.NativeRuntimeObservation {
	observation.HeartbeatAt = time.Time{}
	if observation.Activity != nil {
		profile := *observation.Activity
		profile.AsOf = time.Time{}
		if profile.Summary != nil {
			summary := *profile.Summary
			summary.Hourly = slices.Clone(summary.Hourly)
			for i := range summary.Hourly {
				hour := &summary.Hourly[i]
				hour.To = time.Time{}
				hour.Breakdown.ElapsedSeconds = 0
				hour.Breakdown.UnknownSeconds = 0
				hour.Breakdown.ByKind = maps.Clone(hour.Breakdown.ByKind)
				if _, exists := hour.Breakdown.ByKind["unobserved"]; exists {
					hour.Breakdown.ByKind["unobserved"] = 0
				}
			}
			for len(summary.Hourly) > 0 && summary.Hourly[len(summary.Hourly)-1].Breakdown.ObservedSeconds == 0 {
				summary.Hourly = summary.Hourly[:len(summary.Hourly)-1]
			}
			if len(summary.Hourly) == 0 {
				summary.Hourly = nil
			}
			summary.Through = time.Time{}
			if len(profile.Spans) == 0 {
				summary.DetailFrom = time.Time{}
			}
			summary.Breakdown.ElapsedSeconds = 0
			summary.Breakdown.UnknownSeconds = 0
			summary.Breakdown.ByKind = maps.Clone(summary.Breakdown.ByKind)
			if _, exists := summary.Breakdown.ByKind["unobserved"]; exists {
				summary.Breakdown.ByKind["unobserved"] = 0
			}
			profile.Summary = &summary
		}
		observation.Activity = &profile
	}
	if observation.Landing != nil {
		landing := *observation.Landing
		landing.ObservedAt = time.Time{}
		observation.Landing = &landing
	}
	if observation.REST != nil {
		rest := *observation.REST
		rest.ObservedAt = time.Time{}
		observation.REST = &rest
	}
	return observation
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
	return e.ObserveRuntime(ctx, tracker.NativeRuntimeObservation{HeartbeatAt: time.Now().UTC(), Landing: &tracker.NativeLandingReceipt{ConflictFiles: landing.ConflictFiles, SecurityAudit: landing.SecurityAudit, Pipeline: landing.Pipeline, PipelineDropped: landing.PipelineDropped, Waiting: landing.Waiting, Path: landing.Path, Packages: landing.Packages, CI: landing.CI, Gate: landing.Gate, ChangeID: landing.ChangeID, VersionID: landing.VersionID, HeadSHA: landing.HeadSHA, BaseSHA: landing.BaseSHA, Landed: landing.Landed, MergeSHA: landing.MergeSHA, BaseRef: landing.BaseRef, Method: landing.Method, Rebased: landing.Rebased, RefusalKind: landing.RefusalKind, Refusal: landing.RefusalKind, GateFailed: landing.GateFailed, ObservedAt: time.Now().UTC()}})
}

func (e *nativeExecution) FlushRuntime(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.flush(ctx); err != nil {
		return err
	}
	if !e.runtimeDirty || e.data.Identity == nil {
		return nil
	}
	return e.append(ctx, "run.observed", "", nil)
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
