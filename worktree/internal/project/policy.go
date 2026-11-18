package project

import (
	"context"
	"fmt"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
)

type policyChecker interface {
	CheckProjectPolicy(context.Context, string, string, policy.Descriptor) error
}

type policySourceChecker interface {
	CheckProjectPolicyWithSource(context.Context, string, string, policy.Descriptor, *policy.RepositorySource) error
}

func ResolvePolicy(cfg globalconfig.Project, workflow workflowconfig.Workflow) (policy.Descriptor, error) {
	workflow.Config = EffectivePolicyConfig(cfg, workflow.Config)
	return workflowconfig.ResolvePolicy(workflow)
}

// EffectivePolicyConfig applies the project settings used when resolving a
// policy.
func EffectivePolicyConfig(cfg globalconfig.Project, workflow workflowconfig.Config) workflowconfig.Config {
	workflow = workflow.WithAgentDefaults(cfg.GlobalAgents, cfg.GlobalBudget).WithWorkerDefaults(cfg.GlobalWorker)
	return workflowConfigWithProjectIdentity(cfg, workflow)
}

// MapNativeTracker applies the same tracker selection used at project startup.
// A local tracker choice remains authoritative even for a mapped project.
func MapNativeTracker(workflow workflowconfig.Config, mapped bool) workflowconfig.Config {
	if mapped && workflow.Tracker.Kind == workflowconfig.TrackerGitHub {
		workflow = workflow.ForNativeTracker()
	}
	return workflow
}

func policySourceKind(cfg globalconfig.Project, workflow workflowconfig.Workflow) string {
	if workflow.Definition.Layout == workflowconfig.ProjectDefinitionCloud {
		return "cloud_workflow"
	}
	if cfg.WorkflowRef != "" {
		return "configured_committed_workflow"
	}
	return "configured_local_workflow_read_only"
}

func configureProjectPolicy(ctx context.Context, cfg globalconfig.Project, workflow *workflowconfig.Workflow, scheduling orchestrator.SchedulingSource) error {
	descriptor, provenance, err := resolveProjectPolicy(ctx, cfg, workflow, scheduling)
	if err != nil || descriptor.ID == "" {
		return err
	}
	return checkProjectPolicy(ctx, cfg, workflow, scheduling, descriptor, provenance)
}

func resolveProjectPolicy(ctx context.Context, cfg globalconfig.Project, workflow *workflowconfig.Workflow, scheduling orchestrator.SchedulingSource) (policy.Descriptor, *policy.RepositorySource, error) {
	if _, ok := scheduling.(policyChecker); !ok {
		return policy.Descriptor{}, nil, nil
	}
	var provenance *policy.RepositorySource
	if _, ok := scheduling.(policySourceChecker); ok && workflow.Config.Tracker.Kind == workflowconfig.TrackerHubNative {
		*workflow, provenance = repositoryPolicySource(ctx, cfg, *workflow)
	}
	if source, ok := scheduling.(interface {
		ResolveProjectWorkflow(context.Context, string, workflowconfig.Workflow, *policy.RepositorySource) (workflowconfig.Workflow, error)
	}); ok && workflow.Config.Tracker.Kind == workflowconfig.TrackerHubNative {
		resolved, err := source.ResolveProjectWorkflow(ctx, cfg.ID, *workflow, provenance)
		if err != nil {
			return policy.Descriptor{}, nil, err
		}
		*workflow = resolved
	}
	descriptor, err := ResolvePolicy(cfg, *workflow)
	if err != nil {
		return policy.Descriptor{}, nil, fmt.Errorf("resolve repository policy: %w", err)
	}
	return descriptor, provenance, nil
}

func checkProjectPolicy(ctx context.Context, cfg globalconfig.Project, workflow *workflowconfig.Workflow, scheduling orchestrator.SchedulingSource, descriptor policy.Descriptor, provenance *policy.RepositorySource) error {
	var err error
	if sourceChecker, ok := scheduling.(policySourceChecker); ok {
		err = sourceChecker.CheckProjectPolicyWithSource(ctx, cfg.ID, workflow.Config.Tracker.Repository, descriptor, provenance)
	} else if checker, ok := scheduling.(policyChecker); ok {
		err = checker.CheckProjectPolicy(ctx, cfg.ID, workflow.Config.Tracker.Repository, descriptor)
	}
	if err != nil {
		return err
	}
	workflow.Config.Policy = descriptor
	return nil
}
