package project

import (
	"context"
	"errors"
	"fmt"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
)

type policyChecker interface {
	CheckProjectPolicy(context.Context, string, string, policy.Descriptor) error
}

func ResolvePolicy(cfg globalconfig.Project, workflow workflowconfig.Workflow) (policy.Descriptor, error) {
	workflow.Config = EffectivePolicyConfig(cfg, workflow.Config)
	if err := ValidateNativeTrackerFeatures(workflow.Config); err != nil {
		return policy.Descriptor{}, err
	}
	return workflowconfig.ResolvePolicy(workflow)
}

// EffectivePolicyConfig applies the project settings used when resolving a
// policy, including the global intake override.
func EffectivePolicyConfig(cfg globalconfig.Project, workflow workflowconfig.Config) workflowconfig.Config {
	workflow = workflow.WithAgentDefaults(cfg.GlobalAgents, cfg.GlobalBudget).WithWorkerDefaults(cfg.GlobalWorker)
	return workflowConfigWithProjectIdentity(cfg, workflow)
}

// MapNativeTracker applies the same tracker selection used at project startup.
// A local tracker choice remains authoritative even for a mapped project.
func MapNativeTracker(workflow workflowconfig.Config, mapped bool) workflowconfig.Config {
	if mapped && (workflow.Tracker.Kind == workflowconfig.TrackerGitHub || workflow.Tracker.Kind == workflowconfig.TrackerGitHubLocal) {
		workflow = workflow.ForNativeTracker()
	}
	return workflow
}

// ValidateNativeTrackerFeatures explains required migrations before a mapped
// project can be approved or started.
func ValidateNativeTrackerFeatures(workflow workflowconfig.Config) error {
	if workflow.Tracker.Kind != workflowconfig.TrackerHubNative {
		return nil
	}
	var problems []error
	if workflow.Intake.Enabled() {
		problems = append(problems, errors.New("intake.sources requires tracker.kind github; migrate intake to a separate GitHub-tracked project before using hub_native"))
	}
	if len(workflow.Routines) > 0 {
		problems = append(problems, errors.New("routines requires tracker.kind github or memory; migrate scheduled routines to a supported project before using hub_native"))
	}
	return errors.Join(problems...)
}

func configureProjectPolicy(ctx context.Context, cfg globalconfig.Project, workflow *workflowconfig.Workflow, scheduling orchestrator.SchedulingSource) error {
	checker, ok := scheduling.(policyChecker)
	if !ok {
		return nil
	}
	var provenance *policy.RepositorySource
	sourceChecker, hasSourceChecker := scheduling.(interface {
		CheckProjectPolicyWithSource(context.Context, string, string, policy.Descriptor, *policy.RepositorySource) error
	})
	if hasSourceChecker && workflow.Config.Tracker.Kind == workflowconfig.TrackerHubNative {
		*workflow, provenance = repositoryPolicySource(ctx, cfg, *workflow)
	}
	if source, ok := scheduling.(interface {
		ResolveProjectWorkflow(context.Context, string, workflowconfig.Workflow, *policy.RepositorySource) (workflowconfig.Workflow, error)
	}); ok && workflow.Config.Tracker.Kind == workflowconfig.TrackerHubNative {
		resolved, err := source.ResolveProjectWorkflow(ctx, cfg.ID, *workflow, provenance)
		if err != nil {
			return err
		}
		*workflow = resolved
	}
	descriptor, err := ResolvePolicy(cfg, *workflow)
	if err != nil {
		return fmt.Errorf("resolve repository policy: %w", err)
	}
	if hasSourceChecker {
		err = sourceChecker.CheckProjectPolicyWithSource(ctx, cfg.ID, workflow.Config.Tracker.Repository, descriptor, provenance)
	} else {
		err = checker.CheckProjectPolicy(ctx, cfg.ID, workflow.Config.Tracker.Repository, descriptor)
	}
	if err != nil {
		return err
	}
	workflow.Config.Policy = descriptor
	return nil
}
