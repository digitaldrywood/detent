package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"time"

	isolationpolicy "github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type NativeClient struct {
	githubBatch  func(context.Context, tracker.GitHubBatchTask) error
	client       *Client
	organization tracker.OrganizationID
	project      tracker.ProjectID
}

func (c *Client) Native(organization tracker.OrganizationID, project tracker.ProjectID) (*NativeClient, error) {
	if !strings.HasPrefix(string(organization), "org_") || !strings.HasPrefix(string(project), "prj_") || strings.ContainsAny(string(organization)+string(project), "/?#%\\") {
		return nil, errors.New("native organization and project IDs are required")
	}
	return &NativeClient{client: c, organization: organization, project: project}, nil
}

func (c *NativeClient) base() string {
	return "/api/v2/organizations/" + string(c.organization) + "/projects/" + string(c.project)
}

type nativeCapabilities struct {
	Version        string   `json:"version"`
	ProtocolMajors []int    `json:"protocol_majors"`
	EventSchemas   []int    `json:"event_schema_versions"`
	Features       []string `json:"features"`
}

const capabilitiesTTL = 30 * time.Second

func (c *Client) cachedCapabilities(ctx context.Context) (nativeCapabilities, error) {
	c.capabilitiesMu.Lock()
	defer c.capabilitiesMu.Unlock()
	if !c.capabilitiesAt.IsZero() && (c.capabilitiesDigest != "" || time.Since(c.capabilitiesAt) < capabilitiesTTL) {
		return c.capabilities, nil
	}
	var capabilities nativeCapabilities
	if err := c.request(ctx, http.MethodGet, "/api/v2/capabilities", nil, &capabilities); err != nil {
		return nativeCapabilities{}, err
	}
	c.capabilities, c.capabilitiesAt = capabilities, time.Now()
	return capabilities, nil
}

func (c *Client) Version(ctx context.Context) (string, error) {
	capabilities, err := c.cachedCapabilities(ctx)
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(capabilities.Version)
	if version == "" {
		return "", errors.New("hub did not report its version")
	}
	return version, nil
}

func (c *NativeClient) capabilities(ctx context.Context) (nativeCapabilities, error) {
	return c.client.cachedCapabilities(ctx)
}

// HubFeature reports whether the hub advertises a feature on its capability
// document.
func (c *NativeClient) HubFeature(ctx context.Context, feature string) (bool, error) {
	capabilities, err := c.capabilities(ctx)
	if err != nil {
		return false, err
	}
	return slices.Contains(capabilities.Features, feature), nil
}

func (c *NativeClient) Negotiate(ctx context.Context, required ...string) error {
	capabilities, err := c.capabilities(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(capabilities.ProtocolMajors, 2) || !slices.Contains(capabilities.EventSchemas, 1) || !slices.Contains(capabilities.Features, "native_issues") || !slices.Contains(capabilities.Features, "scoped_collaboration") {
		return errors.New("hub does not support the required native protocol")
	}
	if !slices.Contains(capabilities.Features, "repository_policy") {
		return errors.New("hub does not support approved repository policy; upgrade Hub before dispatch")
	}
	for _, feature := range required {
		if !slices.Contains(capabilities.Features, feature) {
			return errors.Join(ErrUnavailable, errors.New("Hub does not support the required "+feature+" capability"))
		}
	}
	project, err := c.Project(ctx)
	if err != nil {
		return err
	}
	if project.Profile != "native" {
		return errors.New("hub project is not native")
	}
	return nil
}

func (c *NativeClient) Project(ctx context.Context) (tracker.NativeProject, error) {
	var result tracker.NativeProject
	err := c.client.request(ctx, http.MethodGet, c.base(), nil, &result)
	return result, err
}

func nativeItemPath(id tracker.NativeWorkItemID) (string, error) {
	if !strings.HasPrefix(string(id), "wi_") || strings.ContainsAny(string(id), "/?#%\\") {
		return "", errors.New("native work item ID is invalid")
	}
	return "/work-items/" + string(id), nil
}

func (c *NativeClient) Issue(ctx context.Context, id tracker.NativeWorkItemID) (tracker.NativeIssue, error) {
	var result tracker.NativeIssue
	path, err := nativeItemPath(id)
	if err != nil {
		return result, err
	}
	err = c.client.request(ctx, http.MethodGet, c.base()+path, nil, &result)
	return result, err
}

func (c *NativeClient) Issues(ctx context.Context, query url.Values) (tracker.Page[tracker.NativeIssue], error) {
	result, err := c.IssuesPage(ctx, query)
	return result.Page, err
}

func (c *NativeClient) IssuesPage(ctx context.Context, query url.Values) (tracker.NativeIssuePage, error) {
	var result tracker.NativeIssuePage
	err := c.client.request(ctx, http.MethodGet, c.base()+"/work-items?"+query.Encode(), nil, &result)
	return result, err
}

func (c *NativeClient) CreateIssue(ctx context.Context, request tracker.CreateIssue) (tracker.NativeIssue, error) {
	var result tracker.NativeIssue
	err := c.client.request(ctx, http.MethodPost, c.base()+"/work-items", request, &result)
	return result, err
}

func (c *NativeClient) UpdateIssue(ctx context.Context, id tracker.NativeWorkItemID, request tracker.UpdateIssue) (tracker.NativeIssue, error) {
	request.Mutation = c.fencedMutation(ctx, id, request.Mutation)
	var result tracker.NativeIssue
	path, err := nativeItemPath(id)
	if err != nil {
		return result, err
	}
	err = c.client.request(ctx, http.MethodPatch, c.base()+path, request, &result)
	return result, err
}

func (c *NativeClient) Transition(ctx context.Context, id tracker.NativeWorkItemID, request tracker.Transition) (tracker.NativeIssue, error) {
	request.Mutation = c.fencedMutation(ctx, id, request.Mutation)
	var result tracker.NativeIssue
	path, err := nativeItemPath(id)
	if err != nil {
		return result, err
	}
	err = c.client.request(ctx, http.MethodPost, c.base()+path+"/workflow", request, &result)
	return result, nativeMutationError(err)
}

func (c *NativeClient) Dependency(ctx context.Context, id tracker.NativeWorkItemID, request tracker.DependencyMutation) (tracker.NativeIssue, error) {
	request.Mutation = c.fencedMutation(ctx, id, request.Mutation)
	var result tracker.NativeIssue
	path, err := nativeItemPath(id)
	if err != nil {
		return result, err
	}
	err = c.client.request(ctx, http.MethodPost, c.base()+path+"/dependencies", request, &result)
	return result, err
}

func (c *NativeClient) Comments(ctx context.Context, id tracker.NativeWorkItemID, cursor string) (tracker.Page[tracker.NativeComment], error) {
	return c.CommentsPage(ctx, id, cursor, 10)
}

func (c *NativeClient) CreateComment(ctx context.Context, id tracker.NativeWorkItemID, request tracker.CreateComment) (tracker.NativeComment, error) {
	request.Mutation = c.fencedMutation(ctx, id, request.Mutation)
	var result tracker.NativeComment
	path, err := nativeItemPath(id)
	if err != nil {
		return result, err
	}
	err = c.client.request(ctx, http.MethodPost, c.base()+path+"/comments", request, &result)
	return result, nativeMutationError(err)
}

func nativeMutationError(err error) error {
	if claimLost(err) {
		return errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
	}
	return err
}

func (c *NativeClient) UpdateComment(ctx context.Context, id tracker.NativeWorkItemID, commentID string, request tracker.UpdateComment) (tracker.NativeComment, error) {
	request.Mutation = c.fencedMutation(ctx, id, request.Mutation)
	var result tracker.NativeComment
	path, err := nativeItemPath(id)
	if err != nil {
		return result, err
	}
	if !strings.HasPrefix(commentID, "cmt_") || strings.ContainsAny(commentID, "/?#%\\") {
		return result, errors.New("native comment ID is invalid")
	}
	err = c.client.request(ctx, http.MethodPatch, c.base()+path+"/comments/"+commentID, request, &result)
	return result, err
}

func (c *NativeClient) History(ctx context.Context, id tracker.NativeWorkItemID, cursor string) (tracker.Page[tracker.CollaborationEvent], error) {
	return c.HistoryPage(ctx, id, cursor, 100)
}

func (c *NativeClient) AppendEvent(ctx context.Context, id tracker.NativeWorkItemID, request tracker.NativeRunEvent) error {
	path, err := nativeItemPath(id)
	if err != nil {
		return err
	}
	return c.client.request(ctx, http.MethodPost, c.base()+path+"/events", request, nil)
}

func (c *NativeClient) RegisterMachine(ctx context.Context, machine Machine) error {
	if c.client.runner != nil {
		return c.HeartbeatMachine(ctx, machine)
	}
	capabilities, isolation := machine.workspaceReport()
	request := struct {
		BackendIsolation isolationpolicy.Report    `json:"backend_isolation"`
		ProviderReports  []providercapacity.Report `json:"provider_reports,omitempty"`
		ID               tracker.MachineID         `json:"id"`
		Hostname         string                    `json:"hostname"`
		DisplayName      string                    `json:"display_name"`
		Capacity         int                       `json:"capacity"`
		Version          string                    `json:"version"`
		OS               string                    `json:"os"`
		Architecture     string                    `json:"architecture"`
		// Registration carries the same workspace report the heartbeat does,
		// so a restarted runner is eligible before its first heartbeat.
		WorkspaceCapabilities *workspacesession.Capabilities `json:"workspace_capabilities,omitempty"`
		WorkspaceIsolation    string                         `json:"workspace_isolation,omitempty"`
		Skills                *[]skills.ProviderSkill        `json:"skills,omitempty"`
		CheckoutRepository    *string                        `json:"checkout_repository,omitempty"`
	}{machine.BackendIsolation, machine.ProviderReports, machine.ID, machine.Hostname, machine.DisplayName, machine.Capacity, machine.Version, runtime.GOOS, runtime.GOARCH, capabilities, isolation, machine.Skills, machine.CheckoutRepository}
	return c.client.request(ctx, http.MethodPost, c.base()+"/machines/register", request, nil)
}

func (c *NativeClient) Claim(ctx context.Context, request tracker.NativeClaim) (tracker.NativeLease, error) {
	var result tracker.NativeLease
	if c.client.runner != nil {
		availability, err := c.client.runner.availability()
		if err != nil {
			return result, err
		}
		status, err := availability.Evaluate(time.Now())
		if err != nil {
			return result, err
		}
		if !status.Open {
			return result, ErrNoClaimableWork
		}
	}
	err := c.client.request(ctx, http.MethodPost, c.base()+"/claims", request, &result)
	if err == nil && c.client.runner != nil && result.IsolationPolicy == nil {
		return result, errors.Join(errors.New("runner claim isolation policy is unavailable"), c.Release(context.WithoutCancel(ctx), result, "work_item_hydration_failed"))
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == "no_claimable_work" {
		return result, ErrNoClaimableWork
	}
	if err == nil {
		c.client.nativeLeases.Store(c.base()+"/"+string(result.WorkItemID), result)
		if c.client.runner != nil {
			c.client.runner.claimSlot(result, true)
		}
	}
	return result, err
}

func (c *NativeClient) Renew(ctx context.Context, lease tracker.NativeLease, ttl int64) (tracker.NativeLease, error) {
	var result tracker.NativeLease
	err := c.client.request(ctx, http.MethodPost, c.base()+"/leases/"+url.PathEscape(string(lease.ID))+"/renew", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: ttl}, &result)
	return result, err
}

func (c *NativeClient) Release(ctx context.Context, lease tracker.NativeLease, reason string) error {
	err := c.client.request(ctx, http.MethodPost, c.base()+"/leases/"+url.PathEscape(string(lease.ID))+"/release", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: reason}, nil)
	if (err == nil || claimLost(err)) && c.client.runner != nil {
		c.client.runner.claimSlot(lease, false)
		c.slotFreed()
	}
	return err
}

// SetArchived calls the existing native archive/restore command with its revision.
func (c *NativeClient) SetArchived(ctx context.Context, id tracker.NativeWorkItemID, revision tracker.Revision, archived bool, command tracker.Mutation) (tracker.NativeIssue, error) {
	var result tracker.NativeIssue
	path, err := nativeItemPath(id)
	if err != nil {
		return result, err
	}
	operation := "restore"
	if archived {
		operation = "archive"
	}
	request := struct {
		tracker.Mutation
		ExpectedRevision tracker.Revision `json:"expected_revision,string"`
	}{c.fencedMutation(ctx, id, command), revision}
	err = c.client.request(ctx, http.MethodPost, c.base()+path+"/"+operation, request, &result)
	return result, err
}
