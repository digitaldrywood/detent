package hubclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/digitaldrywood/detent/internal/policy"
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
	var approval policy.Approval
	err := c.client.request(ctx, http.MethodGet, c.base()+"/policy", nil, &approval)
	return approval, err
}

func (c *NativeClient) ApproveProjectPolicy(ctx context.Context, change policy.Change) (policy.Approval, error) {
	var approval policy.Approval
	err := c.client.request(ctx, http.MethodPut, c.base()+"/policy", change, &approval)
	return approval, err
}

func (s *Scheduler) CheckProjectPolicy(ctx context.Context, project, repository string, descriptor policy.Descriptor) error {
	if err := descriptor.Validate(); err != nil {
		return err
	}
	var approval policy.Approval
	var err error
	if source := s.nativeProjects[project]; source != nil {
		approval, err = source.client.ProjectPolicy(ctx)
	} else {
		approval, err = s.client.ProjectPolicy(ctx, repository)
	}
	if err != nil {
		return fmt.Errorf("check approved repository policy: %w", err)
	}
	return descriptor.Match(approval.Policy)
}

type claimPolicy struct {
	project    string
	repository string
	descriptor policy.Descriptor
}

func (s *Scheduler) checkClaimPolicy(ctx context.Context, issueID, pinnedID string) error {
	s.mu.Lock()
	pinned, ok := s.claimPolicies[issueID]
	s.mu.Unlock()
	if !ok || pinnedID != pinned.descriptor.ID {
		return errors.New("policy_mismatch: claim has no matching pinned repository policy; release it and request a new claim")
	}
	return s.CheckProjectPolicy(ctx, pinned.project, pinned.repository, pinned.descriptor)
}
