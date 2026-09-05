package hubclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

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
	claim, native := s.nativeClaims[issueID]
	s.mu.Unlock()
	if !ok || pinnedID != pinned.descriptor.ID {
		return errors.New("policy_mismatch: claim has no matching pinned repository policy; release it and request a new claim")
	}
	if err := s.CheckProjectPolicy(ctx, pinned.project, pinned.repository, pinned.descriptor); err != nil {
		return err
	}
	if native && s.client.runner != nil {
		r, err := claim.source.client.ValidateLease(ctx, claim.lease)
		if err != nil {
			return err
		}
		file, err := runnerauth.Load(s.client.runner.path)
		if err != nil {
			return err
		}
		if r.Binding != file.Identity.Binding || r.MachineID != s.machine.ID || r.OrganizationID != file.Identity.OrganizationID {
			return errors.New("selector_no_match: Hub lease runner does not match this host's enrolled identity")
		}
		return pinned.descriptor.Requirements.Match(r.RunnerID, string(r.MachineID), r.Tags)
	}
	return nil
}

func (c *NativeClient) ValidateLease(ctx context.Context, lease tracker.NativeLease) (runnerauth.Runner, error) {
	var result runnerauth.Runner
	err := c.client.request(ctx, http.MethodPost, c.base()+"/leases/"+url.PathEscape(string(lease.ID))+"/validate", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken}, &result)
	return result, err
}
