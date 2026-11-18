package runtimeevidence

import (
	"maps"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func Merge(previous *tracker.NativeRuntimeObservation, observation tracker.NativeRuntimeObservation) tracker.NativeRuntimeObservation {
	nativeSessionActivity := observation.Activity != nil && observation.Activity.SessionID > 0 && observation.Activity.AttemptID == 0
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
		if observation.LocalAttemptID == 0 && !nativeSessionActivity {
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
	if (previous == nil || len(observation.Phases) == 0 || observation.Phase != previous.Phase) && observation.Phase != "" && !observation.HeartbeatAt.IsZero() {
		if len(observation.Phases) < 128 {
			observation.Phases = append(observation.Phases, tracker.NativePhase{Name: observation.Phase, StartedAt: observation.HeartbeatAt})
		} else {
			observation.PhasesDropped++
		}
	}
	return observation
}

func Normalize(observation tracker.NativeRuntimeObservation) tracker.NativeRuntimeObservation {
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

func ActivityBoundary(previous *tracker.NativeRuntimeObservation, role string, startedAt, at time.Time, outcome string) tracker.NativeRuntimeObservation {
	observation := tracker.NativeRuntimeObservation{}
	if previous != nil {
		observation = *previous
	}
	stage := observation.Phase
	if stage == "" || stage == "completed" {
		switch role {
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
		Stage: stage, StartedAt: startedAt, AsOf: at, Status: "running", Coverage: "partial",
	}
	if observation.Activity != nil {
		profile = *observation.Activity
		profile.StartEarlier(startedAt)
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
		if role == runner.RoleMerge && observation.Identity.BackendKind == "git" && profile.SessionID == 0 {
			started := startedAt
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
	return observation
}
