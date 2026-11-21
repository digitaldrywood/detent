package api

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *NativeClient) Scope() string                          { return c.base() }
func (c *NativeClient) Transport() *Client                     { return c.client }
func (c *NativeClient) OrganizationID() tracker.OrganizationID { return c.organization }
func (c *Client) Request(ctx context.Context, method, path string, input, output any) error {
	return c.request(ctx, method, path, input, output)
}
func (c *Client) RequestWithHeaders(ctx context.Context, method, path string, headers http.Header, input, output any) error {
	return c.requestWithHeaders(ctx, method, path, headers, input, output)
}

func (c *Client) Enrolled() bool { return c.runner != nil }
func (c *Client) IdentityFile() string {
	if c.runner == nil {
		return ""
	}
	return c.runner.path
}
func (c *Client) AvailabilityState() (runnerauth.Availability, <-chan struct{}, error) {
	return c.runner.availabilityState()
}
func (c *Client) SetLocalPolicy(project tracker.ProjectID, id string) {
	c.runner.setLocalPolicy(project, id)
}
func (c *Client) ReleasedExecutions(runnerID string) []runnerauth.ReleasedExecution {
	c.runner.routingMu.Lock()
	defer c.runner.routingMu.Unlock()
	snapshot := c.runner.routing
	if snapshot == nil || snapshot.RunnerID != runnerID || snapshot.ClaimState == nil {
		return nil
	}
	return slices.Clone(snapshot.ClaimState.ReleasedExecutions)
}
func (c *NativeClient) AttemptWorkers(ctx context.Context, after string) (runnerauth.AttemptWorkerPage, error) {
	if c.client.runner == nil || c.project == "" {
		return runnerauth.AttemptWorkerPage{}, ErrUnavailable
	}
	file, err := runnerauth.Load(c.client.runner.path)
	if err != nil {
		return runnerauth.AttemptWorkerPage{}, err
	}
	path := c.base() + "/machines/" + url.PathEscape(string(file.Identity.MachineID)) + "/heartbeat?" + url.Values{"attempt_worker_after": {after}}.Encode()
	var response runnerauth.AttemptWorkerInventory
	if err := c.client.request(ctx, http.MethodPost, path, struct{}{}, &response); err != nil {
		return runnerauth.AttemptWorkerPage{}, err
	}
	if response.RunnerID != file.Identity.RunnerID || response.Page == nil || response.Page.ProjectID != c.project {
		return runnerauth.AttemptWorkerPage{}, ErrUnavailable
	}
	return *response.Page, nil
}

func (c *Client) ClaimSlots() []runnerauth.ClaimSlot {
	c.runner.routingMu.Lock()
	defer c.runner.routingMu.Unlock()
	if c.runner.routing == nil || c.runner.routing.ClaimState == nil {
		return nil
	}
	return slices.Clone(c.runner.routing.ClaimState.Slots)
}
func (c *Client) ArtifactConfiguration() (string, int64) {
	if c.artifactSettings != nil {
		return c.artifactSettings()
	}
	return c.artifactServiceID, c.artifactBytes
}
func (c *Client) ArtifactToken(ctx context.Context) (string, error) {
	if c.runner != nil {
		return c.runnerToken(ctx)
	}
	return c.tokenSource(), nil
}
func (c *NativeClient) ForgetPolicyApproval() {
	state := c.heartbeatProject()
	state.mu.Lock()
	defer state.mu.Unlock()
	state.approvalSet = false
	state.approvalRev++
}
func (c *Client) RememberLease(scope string, item tracker.NativeWorkItemID, lease tracker.NativeLease) {
	c.nativeLeases.Store(scope+"/"+string(item), lease)
}
func (c *Client) FlushHostMetrics(ctx context.Context, summaries []hostmetrics.Summary) ([]hostmetrics.Acknowledgment, error) {
	c.runner.routingMu.Lock()
	previous, path, supported := c.runner.heartbeat, c.runner.heartbeatPath, c.runner.hostMetricsSupported
	previous.Problems = slices.Clone(c.runner.problems)
	previous.SettingsRejected = c.runner.settingsRejected
	previous.ProjectConfiguration = nil
	c.runner.routingMu.Unlock()
	if path == "" || !supported {
		return nil, nil
	}
	previous.HostMetrics = summaries
	var response struct {
		HostMetricsAcknowledged []hostmetrics.Acknowledgment `json:"host_metrics_acknowledged"`
	}
	err := c.request(ctx, http.MethodPost, path, previous, &response)
	return response.HostMetricsAcknowledged, err
}

func (c *Client) RoutingRevision() int64 {
	c.runner.routingMu.Lock()
	defer c.runner.routingMu.Unlock()
	if c.runner.routing == nil {
		return 0
	}
	return c.runner.routing.Revision
}
func (c *Client) Draining() bool {
	c.runner.routingMu.Lock()
	defer c.runner.routingMu.Unlock()
	return c.runner.routing != nil && c.runner.routing.Routing.State == "draining"
}
func (c *Client) ProjectAtZeroCapacity(project tracker.ProjectID) bool {
	if c.runner == nil {
		return false
	}
	c.runner.routingMu.Lock()
	defer c.runner.routingMu.Unlock()
	if c.runner.routing == nil {
		return false
	}
	limit, configured := c.runner.routing.Routing.ProjectCapacityLimits[project]
	return configured && limit == 0
}

func (c *Client) ProjectConfigurationRequest() *runnerauth.ProjectConfigurationRequest {
	return c.runner.projectConfigurationRequest()
}
func (c *Client) UpdateRequest() *runnerauth.UpdateRequest     { return c.runner.updateRequest() }
func (c *Client) CapacityRequest() *runnerauth.CapacityRequest { return c.runner.capacityRequest() }
func (c *Client) ForgetLease(scope string, lease tracker.NativeLease) {
	c.nativeLeases.CompareAndDelete(scope+"/"+string(lease.WorkItemID), lease)
}
func (c *Client) RequestWithDeadline(ctx context.Context, timeout time.Duration, method, path string, headers http.Header, input, output any) error {
	return c.requestWithDeadline(ctx, timeout, method, path, headers, input, output)
}
func (c *Client) Download(ctx context.Context, path string, limit int64) ([]byte, string, error) {
	return c.download(ctx, path, limit)
}
func (c *NativeClient) SetGitHubBatch(batch func(context.Context, tracker.GitHubBatchTask) error) {
	c.githubBatch = batch
}
func (c *NativeClient) RenewCapacity(ctx context.Context, lease tracker.NativeLease, ttl int64, landingCapacity int) (tracker.NativeLease, error) {
	return c.renew(ctx, lease, ttl, landingCapacity)
}
func (c *NativeClient) ReleaseAttempt(ctx context.Context, lease tracker.NativeLease, reason string, cancelAttempt bool) error {
	return c.release(ctx, lease, reason, cancelAttempt)
}
