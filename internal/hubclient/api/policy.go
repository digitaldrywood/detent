package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *Client) ProjectPolicy(ctx context.Context, repository string) (policy.Approval, error) {
	var approval policy.Approval
	path, err := RepositoryPolicyPath(repository)
	if err != nil {
		return approval, err
	}
	err = c.request(ctx, http.MethodGet, path, nil, &approval)
	return approval, err
}

func (c *Client) ApproveProjectPolicy(ctx context.Context, repository string, change policy.Change) (policy.Approval, error) {
	var approval policy.Approval
	path, err := RepositoryPolicyPath(repository)
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
	path := c.base() + "/policy?execution_configuration="
	err := c.client.request(ctx, http.MethodGet, path+strconv.Itoa(policy.ExecutionConfigurationVersion), nil, &approval)
	var refusal *APIError
	if errors.As(err, &refusal) && refusal.Status == http.StatusUnprocessableEntity && refusal.Code == "invalid_request" {
		err = c.client.request(ctx, http.MethodGet, path+"1", nil, &approval)
	}
	err = RetryableProjectRead(ctx, err)
	if errors.As(err, &refusal) && refusal.Status == http.StatusConflict && refusal.Code == "policy_mismatch" {
		err = errors.Join(policy.ErrExecutionConfigurationConflict, err)
	}
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
	return c.ReportPolicyObservation(ctx, policy.Observation{Descriptor: descriptor})
}

func (c *NativeClient) ReportPolicyObservation(ctx context.Context, observation policy.Observation) error {
	return c.client.request(ctx, http.MethodPost, c.base()+"/policy/observed", observation, nil)
}

func (c *NativeClient) ProjectWorkflowDefinition(ctx context.Context, local workflowconfig.Workflow) (workflowconfig.Workflow, error) {
	approval, err := c.readExecutionPolicy(ctx)
	if err != nil {
		return workflowconfig.Workflow{}, err
	}
	if approval.Policy.ID != "" {
		return workflowconfig.ApplyExecutionConfiguration(local, approval.Policy, approval.ExecutionConfiguration)
	}
	if local.DefinitionSources != nil {
		return local, nil
	}
	return workflowconfig.Workflow{}, fmt.Errorf("%w: no approved project policy", workflowconfig.ErrNoProjectDefinition)
}

func (c *NativeClient) ResolveProjectWorkflow(ctx context.Context, workflow workflowconfig.Workflow, provenance *policy.RepositorySource) (workflowconfig.Workflow, error) {
	project, err := c.Project(ctx)
	if err != nil {
		return workflowconfig.Workflow{}, fmt.Errorf("load Cloud model selection: %w", RetryableProjectRead(ctx, err))
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
	verified := supplied && provenance != nil && provenance.Commit != "" && provenance.Commit == workflow.Definition.Revision
	if supplied && provenance != nil && !verified {
		return workflowconfig.Workflow{}, errors.New("policy_mismatch: repository provenance does not match the supplied revision")
	}
	approval, err := c.readExecutionPolicy(ctx)
	if err != nil {
		return workflowconfig.Workflow{}, fmt.Errorf("load approved shared project configuration: %w", err)
	}
	if approval.Policy.ID == "" {
		if workflow.Config.Policy.ID != "" {
			return workflowconfig.Workflow{}, policy.ErrExecutionConfigurationConflict
		}
		return workflow, nil
	}
	if err := workflowconfig.ValidateNativePromptInputs(approval.Policy); err != nil {
		return workflowconfig.Workflow{}, errors.Join(policy.ErrExecutionConfigurationConflict, err)
	}
	if verified {
		candidate, err := workflowconfig.ResolvePolicy(workflow)
		if err != nil {
			return workflowconfig.Workflow{}, err
		}
		if candidate.Workflow == nil || candidate.Workflow.Revision != provenance.Commit {
			return workflowconfig.Workflow{}, errors.New("policy_mismatch: verified repository revision is required")
		}
		if candidate.ID == approval.Policy.ID && approval.Policy.Workflow != nil && candidate.Workflow.Source != approval.Policy.Workflow.Source {
			return workflowconfig.Workflow{}, workflowconfig.ErrNativeAuthoredMismatch
		}
		if approval.Policy.Workflow != nil && candidate.Workflow.Source == approval.Policy.Workflow.Source && approval.Policy.Workflow.Revision == provenance.Commit {
			if candidate.Authored != nil && approval.Policy.Authored != nil && candidate.Authored.Version != approval.Policy.Authored.Version {
				historical := workflow
				historical.Authored = &policy.Authored{Version: approval.Policy.Authored.Version}
				candidate, err = workflowconfig.ResolvePolicy(historical)
				if err != nil {
					return workflowconfig.Workflow{}, err
				}
			}
			if !candidate.SameRepositoryPolicyInputs(approval.Policy) {
				return workflowconfig.Workflow{}, workflowconfig.ErrNativeAuthoredMismatch
			}
		}
	}
	return workflowconfig.ApplyExecutionConfiguration(workflow, approval.Policy, approval.ExecutionConfiguration)
}

func (c *NativeClient) ValidateLease(ctx context.Context, lease tracker.NativeLease) (runnerauth.Runner, error) {
	var result runnerauth.Runner
	err := c.client.request(ctx, http.MethodPost, c.base()+"/leases/"+url.PathEscape(string(lease.ID))+"/validate", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken}, &result)
	return result, err
}

func RepositoryPolicyPath(repository string) (string, error) {
	parts := strings.Split(strings.TrimSpace(repository), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", errors.New("policy_mismatch: repository owner/name is required")
	}
	return "/api/v1/repositories/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/policy", nil
}

func RetryableProjectRead(ctx context.Context, err error) error {
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
