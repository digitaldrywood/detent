package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"runtime"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type NativeClient struct {
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

func (c *NativeClient) Negotiate(ctx context.Context) error {
	var capabilities struct {
		ProtocolMajors []int    `json:"protocol_majors"`
		EventSchemas   []int    `json:"event_schema_versions"`
		Features       []string `json:"features"`
	}
	if err := c.client.request(ctx, http.MethodGet, "/api/v2/capabilities", nil, &capabilities); err != nil {
		return err
	}
	if !slices.Contains(capabilities.ProtocolMajors, 2) || !slices.Contains(capabilities.EventSchemas, 1) || !slices.Contains(capabilities.Features, "native_issues") || !slices.Contains(capabilities.Features, "scoped_collaboration") {
		return errors.New("hub does not support the required native protocol")
	}
	if !slices.Contains(capabilities.Features, "repository_policy") {
		return errors.New("hub does not support approved repository policy; upgrade Hub before dispatch")
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
	var result tracker.Page[tracker.NativeIssue]
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
	return result, err
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
	var result tracker.Page[tracker.NativeComment]
	path, err := nativeItemPath(id)
	if err != nil {
		return result, err
	}
	err = c.client.request(ctx, http.MethodGet, c.base()+path+"/comments?limit=10&cursor="+url.QueryEscape(cursor), nil, &result)
	return result, err
}

func (c *NativeClient) CreateComment(ctx context.Context, id tracker.NativeWorkItemID, request tracker.CreateComment) (tracker.NativeComment, error) {
	request.Mutation = c.fencedMutation(ctx, id, request.Mutation)
	var result tracker.NativeComment
	path, err := nativeItemPath(id)
	if err != nil {
		return result, err
	}
	err = c.client.request(ctx, http.MethodPost, c.base()+path+"/comments", request, &result)
	return result, err
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
	var result tracker.Page[tracker.CollaborationEvent]
	path, err := nativeItemPath(id)
	if err != nil {
		return result, err
	}
	err = c.client.request(ctx, http.MethodGet, c.base()+path+"/history?limit=100&cursor="+url.QueryEscape(cursor), nil, &result)
	return result, err
}

func (c *NativeClient) AppendEvent(ctx context.Context, id tracker.NativeWorkItemID, request tracker.NativeRunEvent) error {
	path, err := nativeItemPath(id)
	if err != nil {
		return err
	}
	return c.client.request(ctx, http.MethodPost, c.base()+path+"/events", request, nil)
}

func (c *NativeClient) RegisterMachine(ctx context.Context, machine Machine) error {
	request := struct {
		ID           tracker.MachineID `json:"id"`
		Hostname     string            `json:"hostname"`
		DisplayName  string            `json:"display_name"`
		Capacity     int               `json:"capacity"`
		Version      string            `json:"version"`
		OS           string            `json:"os"`
		Architecture string            `json:"architecture"`
	}{machine.ID, machine.Hostname, machine.DisplayName, machine.Capacity, machine.Version, runtime.GOOS, runtime.GOARCH}
	return c.client.request(ctx, http.MethodPost, c.base()+"/machines/register", request, nil)
}

func (c *NativeClient) Claim(ctx context.Context, request tracker.NativeClaim) (tracker.NativeLease, error) {
	var result tracker.NativeLease
	err := c.client.request(ctx, http.MethodPost, c.base()+"/claims", request, &result)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == "no_claimable_work" {
		return result, ErrNoClaimableWork
	}
	if err == nil {
		c.client.nativeLeases.Store(c.base()+"/"+string(result.WorkItemID), result)
	}
	return result, err
}

func (c *NativeClient) Renew(ctx context.Context, lease tracker.NativeLease, ttl int64) (tracker.NativeLease, error) {
	var result tracker.NativeLease
	err := c.client.request(ctx, http.MethodPost, c.base()+"/leases/"+url.PathEscape(string(lease.ID))+"/renew", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: ttl}, &result)
	return result, err
}

func (c *NativeClient) Release(ctx context.Context, lease tracker.NativeLease, reason string) error {
	return c.client.request(ctx, http.MethodPost, c.base()+"/leases/"+url.PathEscape(string(lease.ID))+"/release", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: reason}, nil)
}
