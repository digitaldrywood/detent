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

type projectWorkflowResolver interface {
	ResolveProjectWorkflow(context.Context, string, workflowconfig.Workflow, *policy.RepositorySource) (workflowconfig.Workflow, error)
}

type policyChecker interface {
	CheckProjectPolicy(context.Context, string, string, policy.Descriptor) error
}

type policySourceChecker interface {
	CheckProjectPolicyObservation(context.Context, string, string, policy.Observation) error
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

func ResolveRunnerWorkflow(ctx context.Context, cfg globalconfig.Project, workflow workflowconfig.Workflow, scheduling orchestrator.SchedulingSource) (workflowconfig.Workflow, error) {
	_, _, err := resolveProjectPolicy(ctx, cfg, &workflow, scheduling)
	return workflow, err
}

func configureProjectPolicy(ctx context.Context, cfg globalconfig.Project, workflow *workflowconfig.Workflow, scheduling orchestrator.SchedulingSource) (policy.Observation, error) {
	descriptor, observation, err := resolveProjectPolicy(ctx, cfg, workflow, scheduling)
	if err != nil || descriptor.ID == "" {
		return observation, err
	}
	return observation, checkProjectPolicy(ctx, cfg, workflow, scheduling, descriptor, observation)
}

func resolveProjectPolicy(ctx context.Context, cfg globalconfig.Project, workflow *workflowconfig.Workflow, scheduling orchestrator.SchedulingSource) (policy.Descriptor, policy.Observation, error) {
	if _, ok := scheduling.(policyChecker); !ok {
		return policy.Descriptor{}, policy.Observation{}, nil
	}
	var provenance *policy.RepositorySource
	reporter, canReport := scheduling.(policySourceChecker)
	if canReport && workflow.Config.Tracker.Kind == workflowconfig.TrackerHubNative && workflow.DefinitionSources != nil {
		*workflow, provenance = repositoryPolicySource(ctx, cfg, *workflow)
	}
	selected := *workflow
	if source, ok := scheduling.(projectWorkflowResolver); ok && workflow.Config.Tracker.Kind == workflowconfig.TrackerHubNative {
		resolved, err := source.ResolveProjectWorkflow(ctx, cfg.ID, *workflow, provenance)
		if err != nil {
			if canReport && (provenance != nil || workflow.Definition.Layout == workflowconfig.ProjectDefinitionCloud && workflow.DefinitionSources != nil) && errors.Is(err, policy.ErrExecutionConfigurationConflict) {
				err = errors.Join(err, reportVerifiedPolicyRepair(ctx, cfg, selected, provenance, reporter))
			}
			return policy.Descriptor{}, policy.Observation{}, err
		}
		*workflow = resolved
	}
	descriptor, err := ResolvePolicy(cfg, *workflow)
	if err != nil {
		return policy.Descriptor{}, policy.Observation{}, fmt.Errorf("resolve repository policy: %w", err)
	}
	observation, err := observedRepositoryPolicy(cfg, selected, descriptor, provenance)
	return descriptor, observation, err
}

func observedRepositoryPolicy(cfg globalconfig.Project, selected workflowconfig.Workflow, descriptor policy.Descriptor, provenance *policy.RepositorySource) (policy.Observation, error) {
	observed := descriptor
	if provenance != nil {
		candidate, err := ResolvePolicy(cfg, selected)
		if err != nil {
			return policy.Observation{}, err
		}
		if descriptor.Workflow == nil || candidate.Workflow == nil || candidate.Workflow.Source != descriptor.Workflow.Source {
			provenance = nil
		} else if !candidate.SameRepositoryPolicyInputs(descriptor) || candidate.Authored != nil && descriptor.Authored != nil && candidate.Authored.SourceVersion() != descriptor.Authored.SourceVersion() {
			observed = candidate
		}
	}
	if provenance != nil && observed.Authored != nil && observed.Workflow != nil && observed.Workflow.Revision != provenance.Commit {
		repositoryWorkflow := *observed.Workflow
		repositoryWorkflow.Revision = provenance.Commit
		observed.Workflow = &repositoryWorkflow
	}
	return policy.Observation{Descriptor: observed, Source: provenance}, nil
}

func reportVerifiedPolicyRepair(ctx context.Context, cfg globalconfig.Project, selected workflowconfig.Workflow, provenance *policy.RepositorySource, reporter policySourceChecker) error {
	candidate, err := ResolvePolicy(cfg, selected)
	if err != nil {
		return err
	}
	if err := workflowconfig.ValidateNativePromptInputs(candidate); err != nil {
		return err
	}
	return reporter.CheckProjectPolicyObservation(ctx, cfg.ID, "", policy.Observation{Descriptor: candidate, Source: provenance})
}

func checkProjectPolicy(ctx context.Context, cfg globalconfig.Project, workflow *workflowconfig.Workflow, scheduling orchestrator.SchedulingSource, descriptor policy.Descriptor, observation policy.Observation) error {
	var err error
	if sourceChecker, ok := scheduling.(policySourceChecker); ok {
		err = sourceChecker.CheckProjectPolicyObservation(ctx, cfg.ID, descriptor.ID, observation)
	} else if checker, ok := scheduling.(policyChecker); ok {
		err = checker.CheckProjectPolicy(ctx, cfg.ID, workflow.Config.Tracker.Repository, descriptor)
	}
	if err != nil {
		return err
	}
	workflow.Config.Policy = descriptor
	return nil
}
