package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *NativeClient) Attempts(ctx context.Context, id tracker.NativeWorkItemID, cursor string) (tracker.Page[tracker.NativeAttempt], error) {
	return c.AttemptsPage(ctx, id, cursor, 100)
}

func (c *NativeClient) blockerPage(ctx context.Context, id tracker.NativeWorkItemID, kind, cursor string, target any) error {
	path, err := nativeItemPath(id)
	if err != nil {
		return err
	}
	params := url.Values{"view": {"blockers"}, "limit": {"100"}, "cursor": {cursor}}
	return c.client.request(ctx, http.MethodGet, c.base()+path+"/"+kind+"?"+params.Encode(), nil, target)
}

func (c *NativeClient) Recovery(ctx context.Context, id tracker.NativeWorkItemID) (tracker.NativeRecovery, error) {
	var result tracker.NativeRecovery
	path, err := nativeItemPath(id)
	if err != nil {
		return result, err
	}
	err = c.client.request(ctx, http.MethodGet, c.base()+path+"?view=recovery", nil, &result)
	if err == nil && (result.Issue.OrganizationID != c.organization || result.Issue.ProjectID != c.project || result.Issue.WorkItemID != id) {
		return result, errors.New("read recovery: scoped work item is missing")
	}
	return result, err
}
