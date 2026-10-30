package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func repositoryPolicyPath(repository string) (string, error) {
	parts := strings.Split(strings.TrimSpace(repository), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", errors.New("policy_mismatch: repository owner/name is required")
	}
	return "/api/v1/repositories/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/policy", nil
}

func (c *Client) ProjectPolicy(ctx context.Context, repository string) (policy.Approval, error) {
	var approval policy.Approval
	path, err := repositoryPolicyPath(repository)
	if err != nil {
		return approval, err
	}
	err = c.request(ctx, http.MethodGet, path, nil, &approval)
	return approval, err
}

func (c *Client) ApproveProjectPolicy(ctx context.Context, repository string, change policy.Change) (policy.Approval, error) {
	var approval policy.Approval
	path, err := repositoryPolicyPath(repository)
	if err != nil {
		return approval, err
	}
	err = c.request(ctx, http.MethodPut, path, change, &approval)
	return approval, err
}

func (c *NativeClient) ProjectPolicy(ctx context.Context) (policy.Approval, error) {
	state := c.heartbeatProject()
	state.mu.Lock()
	if state.changes != nil && state.approvalSet {
		approval, err := state.approval, state.approvalErr
		state.mu.Unlock()
		return approval, err
	}
	revision := state.approvalRev
	state.mu.Unlock()
	approval, err := c.readExecutionPolicy(ctx)
	state.mu.Lock()
	defer state.mu.Unlock()
	if c.client.runner != nil && err == nil {
		if state.changes != nil && state.approvalRev == revision {
			state.approval, state.approvalErr, state.approvalSet = approval, err, true
		}
	}
	return approval, err
}

func (c *NativeClient) readExecutionPolicy(ctx context.Context) (policy.Approval, error) {
	var approval policy.Approval
	err := retryableProjectRead(ctx, c.client.request(ctx, http.MethodGet, c.base()+"/policy?execution_configuration=1", nil, &approval))
	if c.client.runner != nil && err == nil {
		c.client.runner.setApprovedPolicy(c.project, approval.Policy.ID)
	}
	return approval, err
}

func (c *NativeClient) ApproveProjectPolicy(ctx context.Context, change policy.Change) (policy.Approval, error) {
	var approval policy.Approval
	err := c.client.request(ctx, http.MethodPut, c.base()+"/policy", change, &approval)
	return approval, err
}

func (c *NativeClient) ReportObservedPolicy(ctx context.Context, descriptor policy.Descriptor) error {
	return c.reportPolicyObservation(ctx, policy.Observation{Descriptor: descriptor})
}

func (c *NativeClient) reportPolicyObservation(ctx context.Context, observation policy.Observation) error {
	return c.client.request(ctx, http.MethodPost, c.base()+"/policy/observed", observation, nil)
}

func (s *Scheduler) ProjectWorkflowMarkdown(ctx context.Context, project string) (string, error) {
	source := s.nativeProject(project)
	if source == nil {
		return "", nil
	}
	definition, err := source.client.Project(ctx)
	return definition.WorkflowMarkdown, err
}

func (s *Scheduler) CheckProjectPolicy(ctx context.Context, project, repository string, descriptor policy.Descriptor) error {
	return s.CheckProjectPolicyWithSource(ctx, project, repository, descriptor, nil)
}

func (s *Scheduler) CheckProjectPolicyWithSource(ctx context.Context, project, repository string, descriptor policy.Descriptor, provenance *policy.RepositorySource) error {
	if err := descriptor.Validate(); err != nil {
		return fmt.Errorf("invalid project configuration: %w", err)
	}
	source := s.nativeProject(project)
	if source == nil {
		path, err := repositoryPolicyPath(repository)
		if err != nil {
			return err
		}
		return s.client.request(ctx, http.MethodPost, path+"/observed", policy.Observation{Descriptor: descriptor, Source: provenance}, nil)
	}
	if s.client.runner != nil {
		s.client.runner.setLocalPolicy(source.client.project, descriptor.ID)
		if err := s.ensureNativeMachine(ctx, source); err != nil {
			return err
		}
	}
	return s.reportObservedPolicy(ctx, project, source, policy.Observation{Descriptor: descriptor, Source: provenance})
}

func (s *Scheduler) ResolveProjectWorkflow(ctx context.Context, project string, workflow workflowconfig.Workflow, provenance *policy.RepositorySource) (workflowconfig.Workflow, error) {
	source := s.nativeProject(project)
	if source == nil {
		return workflow, nil
	}
	return source.client.ResolveProjectWorkflow(ctx, workflow, provenance)
}

func (c *NativeClient) ResolveProjectWorkflow(ctx context.Context, workflow workflowconfig.Workflow, provenance *policy.RepositorySource) (workflowconfig.Workflow, error) {
	project, err := c.Project(ctx)
	if err != nil {
		return workflowconfig.Workflow{}, fmt.Errorf("load Cloud model selection: %w", retryableProjectRead(ctx, err))
	}
	var selection workflowconfig.ModelSelection
	if len(project.ModelSelection) > 0 {
		if err := json.Unmarshal(project.ModelSelection, &selection); err != nil {
			return workflowconfig.Workflow{}, fmt.Errorf("decode Cloud model selection: %w", err)
		}
	}
	if selection.Sources == nil {
		selection.Sources = map[string]string{}
	}
	selection.Sources["authority"] = "cloud"
	if problems := selection.Validate(); len(problems) > 0 {
		return workflowconfig.Workflow{}, fmt.Errorf("invalid Cloud model selection: %s", strings.Join(problems, "; "))
	}
	workflow.Config.Agents.ModelSelection = selection

	supplied := workflow.Definition.Layout == workflowconfig.ProjectDefinitionSplit || workflow.Definition.Layout == workflowconfig.ProjectDefinitionLegacy
	if workflow.Definition.Layout == workflowconfig.ProjectDefinitionCloud || supplied && provenance != nil {
		return workflow, nil
	}
	approval, err := c.readExecutionPolicy(ctx)
	if err != nil {
		return workflowconfig.Workflow{}, fmt.Errorf("load approved shared project configuration: %w", err)
	}
	if approval.Policy.ID == "" {
		return workflow, nil
	}
	if supplied && approval.Policy.Authored == nil && (approval.Policy.Configuration == nil || workflow.DefinitionSources != nil) {
		return workflow, nil
	}
	return workflowconfig.ApplyExecutionConfiguration(workflow, approval.Policy, approval.ExecutionConfiguration)
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
	if err := source.client.reportPolicyObservation(ctx, observation); err != nil {
		s.mu.Lock()
		if s.reportedPolicies[project] == reportID {
			s.reportedPolicies[project] = previous
		}
		s.mu.Unlock()
		return fmt.Errorf("report the resolved repository policy to the Hub: %w", err)
	}
	state := source.client.heartbeatProject()
	state.mu.Lock()
	state.approvalSet = false
	state.approvalRev++
	state.mu.Unlock()
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
		r, err := claim.source.client.ValidateLease(ctx, claim.lease)
		if err != nil {
			return err
		}
		if s.client.runner == nil {
			return nil
		}
		file, err := runnerauth.Load(s.client.runner.path)
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

func (c *NativeClient) ValidateLease(ctx context.Context, lease tracker.NativeLease) (runnerauth.Runner, error) {
	var result runnerauth.Runner
	err := c.client.request(ctx, http.MethodPost, c.base()+"/leases/"+url.PathEscape(string(lease.ID))+"/validate", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken}, &result)
	return result, err
}

func retryableProjectRead(ctx context.Context, err error) error {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || !errors.Is(err, ErrUnavailable) {
		return err
	}
	var apiErr *APIError
	var transportErr net.Error
	retryable := errors.As(err, &transportErr) || errors.Is(err, io.ErrUnexpectedEOF)
	if errors.As(err, &apiErr) {
		retryable = apiErr.Status >= http.StatusInternalServerError
	}
	if !retryable {
		return err
	}
	return errors.Join(connector.NewRetryableError("project configuration read unavailable"), err)
}
