package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/digitaldrywood/detent/internal/hubclient/recovery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *NativeClient) Attempts(ctx context.Context, id tracker.NativeWorkItemID, cursor string) (tracker.Page[tracker.NativeAttempt], error) {
	return c.AttemptsPage(ctx, id, cursor, 100)
}

func (c *NativeClient) blockerPage(ctx context.Context, id tracker.NativeWorkItemID, kind, cursor string, target any) error {
	path, err := NativeItemPath(id)
	if err != nil {
		return err
	}
	view := "blockers"
	if kind == "attempts" {
		view = "recorded_blockers"
	}
	params := url.Values{"view": {view}, "limit": {"100"}, "cursor": {cursor}}
	return c.client.request(ctx, http.MethodGet, c.base()+path+"/"+kind+"?"+params.Encode(), nil, target)
}

func (c *NativeClient) Recovery(ctx context.Context, id tracker.NativeWorkItemID) (tracker.NativeRecovery, error) {
	return recovery.Load(ctx, recoveryReader{c}, c.organization, c.project, id)
}

type recoveryReader struct{ *NativeClient }

func (r recoveryReader) Detail(ctx context.Context, id tracker.NativeWorkItemID, headers http.Header) (tracker.NativeRecovery, error) {
	var result tracker.NativeRecovery
	path, err := NativeItemPath(id)
	if err != nil {
		return result, err
	}
	err = r.client.requestWithHeaders(ctx, http.MethodGet, r.base()+path+"?view=recovery", headers, nil, &result)
	return result, err
}
func (r recoveryReader) Evidence(ctx context.Context, id tracker.NativeWorkItemID, cursor string) (tracker.NativeRecoveryPage, error) {
	return r.RecoveryEvidence(ctx, id, cursor)
}

func (c *NativeClient) RecoveryEvidence(ctx context.Context, id tracker.NativeWorkItemID, cursor string) (tracker.NativeRecoveryPage, error) {
	path, err := NativeItemPath(id)
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
