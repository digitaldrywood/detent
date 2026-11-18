package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func (s *Scheduler) ProjectWorkflowMarkdown(ctx context.Context, project string) (string, error) {
	source := s.nativeProject(project)
	if source == nil {
		return "", nil
	}
	definition, err := source.NativeClient().Project(ctx)
	return definition.WorkflowMarkdown, err
}

func (s *Scheduler) ProjectWorkflowDefinition(ctx context.Context, project string, local workflowconfig.Workflow) (workflowconfig.Workflow, error) {
	native, ok := s.NativeClient(project)
	if !ok {
		return workflowconfig.Workflow{}, workflowconfig.ErrNoProjectDefinition
	}
	workflow, err := native.ProjectWorkflowDefinition(ctx, local)
	if !errors.Is(err, policy.ErrExecutionConfigurationConflict) && !errors.Is(err, workflowconfig.ErrNoProjectDefinition) {
		return workflow, err
	}
	markdown, readErr := s.ProjectWorkflowMarkdown(ctx, project)
	if readErr != nil {
		return workflowconfig.Workflow{}, api.RetryableProjectRead(ctx, readErr)
	}
	if markdown == "" {
		return workflowconfig.Workflow{}, err
	}
	workflow, err = workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: []byte(markdown)})
	if err != nil {
		return workflowconfig.Workflow{}, err
	}
	workflow.Definition.Layout = workflowconfig.ProjectDefinitionCloud
	return workflow, nil
}

func (s *Scheduler) CheckProjectPolicy(ctx context.Context, project, repository string, descriptor policy.Descriptor) error {
	return s.CheckProjectPolicyWithSource(ctx, project, repository, descriptor, nil)
}

func (s *Scheduler) CheckProjectPolicyWithSource(ctx context.Context, project, repository string, descriptor policy.Descriptor, provenance *policy.RepositorySource) error {
	return s.CheckProjectPolicyObservation(ctx, project, descriptor.ID, policy.Observation{Descriptor: descriptor, Source: provenance})
}

func (s *Scheduler) CheckProjectPolicyObservation(ctx context.Context, project, executingID string, observation policy.Observation) error {
	if err := observation.Descriptor.Validate(); err != nil {
		return fmt.Errorf("invalid project configuration: %w", err)
	}
	source := s.nativeProject(project)
	if source == nil {
		return fmt.Errorf("project %q is not mapped to a Hub-native project; generic Hub tracker mode was removed", project)
	}
	if s.client.Enrolled() {
		if executingID != "" {
			s.client.SetLocalPolicy(source.NativeClient().ProjectID(), executingID)
		}
		if err := s.ensureNativeMachine(ctx, source); err != nil {
			return err
		}
	}
	return s.reportObservedPolicy(ctx, project, source, observation)
}

func (s *Scheduler) ResolveProjectWorkflow(ctx context.Context, project string, workflow workflowconfig.Workflow, provenance *policy.RepositorySource) (workflowconfig.Workflow, error) {
	source := s.nativeProject(project)
	if source == nil {
		return workflow, nil
	}
	resolved, err := source.NativeClient().ResolveProjectWorkflow(ctx, workflow, provenance)
	if err == nil && s.client.Enrolled() && resolved.Config.Policy.ID == "" {
		return workflowconfig.Workflow{}, policy.ErrExecutionConfigurationConflict
	}
	return resolved, err
}

func (s *Scheduler) reportObservedPolicy(ctx context.Context, project string, source *NativeConnector, observation policy.Observation) error {
	raw, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	reportID := policy.Digest(raw)
	s.mu.Lock()
	if s.reportedPolicies[project] == reportID {
		s.mu.Unlock()
		return nil
	}
	if s.reportedPolicies == nil {
		s.reportedPolicies = map[string]string{}
	}
	previous := s.reportedPolicies[project]
	s.reportedPolicies[project] = reportID
	s.mu.Unlock()
	if err := source.NativeClient().ReportPolicyObservation(ctx, observation); err != nil {
		s.mu.Lock()
		if s.reportedPolicies[project] == reportID {
			s.reportedPolicies[project] = previous
		}
		s.mu.Unlock()
		return fmt.Errorf("report the resolved repository policy to the Hub: %w", err)
	}
	source.NativeClient().ForgetPolicyApproval()
	return nil
}

type claimPolicy struct {
	project    string
	repository string
	descriptor policy.Descriptor
}

func (s *Scheduler) checkClaimPolicy(ctx context.Context, issueID, pinnedID string) error {
	if err := s.checkClaimPolicyApproval(ctx, issueID, pinnedID); err != nil {
		return err
	}
	return s.checkClaimRunner(ctx, issueID, pinnedID)
}

func (s *Scheduler) checkClaimPolicyApproval(ctx context.Context, issueID, pinnedID string) error {
	s.mu.Lock()
	pinned, ok := s.claimPolicies[issueID]
	_, native := s.nativeClaims[issueID]
	s.mu.Unlock()
	if !ok || pinnedID != pinned.descriptor.ID {
		return &APIError{Status: http.StatusConflict, Code: "policy_mismatch", Message: "claim has no matching pinned repository policy; release it and request a new claim"}
	}
	s.projectsMu.RLock()
	assigned := s.nativeProjects[pinned.project] != nil
	s.projectsMu.RUnlock()
	if native && !assigned {
		return &APIError{Status: http.StatusForbidden, Code: "selector_no_match", Message: "project is no longer assigned to this runner"}
	}
	if assigned {
		return pinned.descriptor.Validate()
	}
	return s.CheckProjectPolicy(ctx, pinned.project, pinned.repository, pinned.descriptor)
}

func (s *Scheduler) checkClaimRunner(ctx context.Context, issueID, pinnedID string) error {
	s.mu.Lock()
	pinned, ok := s.claimPolicies[issueID]
	claim, native := s.nativeClaims[issueID]
	s.mu.Unlock()
	if !ok || pinnedID != pinned.descriptor.ID {
		return &APIError{Status: http.StatusConflict, Code: "policy_mismatch", Message: "claim has no matching pinned repository policy; release it and request a new claim"}
	}
	if native {
		r, err := claim.source.NativeClient().ValidateLease(ctx, claim.lease)
		if err != nil {
			return err
		}
		if !s.client.Enrolled() {
			return nil
		}
		file, err := runnerauth.Load(s.client.IdentityFile())
		if err != nil {
			return err
		}
		if r.Binding != file.Identity.Binding || r.MachineID != s.machine.ID || r.OrganizationID != file.Identity.OrganizationID {
			return &APIError{Status: http.StatusForbidden, Code: "selector_no_match", Message: "Hub lease runner does not match this host's enrolled identity"}
		}
		if err := pinned.descriptor.Requirements.Match(r.RunnerID, string(r.MachineID), r.Tags); err != nil {
			return &APIError{Status: http.StatusForbidden, Code: "selector_no_match", Message: err.Error()}
		}
	}
	return nil
}
