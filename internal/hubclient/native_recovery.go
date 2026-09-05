package hubclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *NativeClient) Attempts(ctx context.Context, id tracker.NativeWorkItemID, cursor string) (tracker.Page[tracker.NativeAttempt], error) {
	var result tracker.Page[tracker.NativeAttempt]
	path, err := nativeItemPath(id)
	if err != nil {
		return result, err
	}
	err = c.client.request(ctx, http.MethodGet, c.base()+path+"/attempts?limit=100&cursor="+url.QueryEscape(cursor), nil, &result)
	return result, err
}

func (c *NativeClient) Recovery(ctx context.Context, id tracker.NativeWorkItemID) (tracker.NativeRecovery, error) {
	var result tracker.NativeRecovery
	var err error
	result.Issue, err = c.Issue(ctx, id)
	if err != nil {
		return result, err
	}
	for cursor := ""; ; {
		page, err := c.Comments(ctx, id, cursor)
		if err != nil {
			return result, err
		}
		result.Discussion = append(result.Discussion, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	for cursor := ""; ; {
		page, err := c.Attempts(ctx, id, cursor)
		if err != nil {
			return result, err
		}
		result.Attempts = append(result.Attempts, page.Items...)
		for _, attempt := range page.Items {
			if attempt.Checkpoint != nil && attempt.Checkpoint.Change != nil {
				result.Change = attempt.Checkpoint.Change
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	for cursor := ""; ; {
		page, err := c.History(ctx, id, cursor)
		if err != nil {
			return result, err
		}
		result.History = append(result.History, page.Items...)
		for _, event := range page.Items {
			if event.Data.Change != nil && event.Data.Change.VersionID != "" {
				result.Change = event.Data.Change
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return result, nil
}
