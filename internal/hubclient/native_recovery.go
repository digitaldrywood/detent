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
	err = c.client.requestWithHeaders(ctx, http.MethodGet, c.base()+path+"?view=recovery", http.Header{"X-Detent-Recovery-Projection": {"bounded"}}, nil, &result)
	if err == nil && result.Continuation != nil {
		return tracker.NativeRecovery{}, errors.New("read recovery: unbounded detail continuation is not candidate hydration")
	}
	if err == nil && (result.Issue.OrganizationID != c.organization || result.Issue.ProjectID != c.project || result.Issue.WorkItemID != id) {
		return result, errors.New("read recovery: scoped work item is missing")
	}
	return result, err
}

func (c *NativeClient) RecoveryEvidence(ctx context.Context, id tracker.NativeWorkItemID, cursor string) (tracker.NativeRecoveryPage, error) {
	path, err := nativeItemPath(id)
	if err != nil {
		return tracker.NativeRecoveryPage{}, err
	}
	params := url.Values{"view": {"recovery"}, "limit": {"1"}, "cursor": {cursor}}
	var result tracker.NativeRecovery
	if err := c.client.request(ctx, http.MethodGet, c.base()+path+"?"+params.Encode(), nil, &result); err != nil {
		return tracker.NativeRecoveryPage{}, err
	}
	page := result.Continuation
	if page == nil || page.OrganizationID != c.organization || page.ProjectID != c.project || page.WorkItemID != id || len(page.Data) > 256<<10 || page.Offset < 0 || page.TotalBytes < page.Offset || int64(len(page.Data)) > page.TotalBytes-page.Offset {
		return tracker.NativeRecoveryPage{}, errors.New("read recovery evidence: incomplete or mismatched record")
	}
	return *page, nil
}
