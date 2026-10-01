package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// NativePullRequestReference projects identifiers from the existing PR panel.
// Change/review details remain owned by that application's detail reads.
type NativePullRequestReference struct {
	Number    int       `json:"number"`
	URL       string    `json:"url"`
	FetchedAt time.Time `json:"fetched_at"`
}

// SameOrganization binds local project mappings to the same hub and tenant.
func (c *NativeClient) SameOrganization(other *NativeClient) bool {
	return other != nil && c.organization == other.organization && c.client.baseURL.String() == other.client.baseURL.String()
}

func (c *NativeClient) ProjectID() tracker.ProjectID { return c.project }

func (c *NativeClient) PullRequestReferences(ctx context.Context, id tracker.NativeWorkItemID) ([]NativePullRequestReference, error) {
	path, err := nativeItemPath(id)
	if err != nil {
		return nil, err
	}
	var result []NativePullRequestReference
	err = c.client.request(ctx, http.MethodGet, c.base()+path+"/pull-requests", nil, &result)
	return result, err
}

func (c *NativeClient) CommentsPage(ctx context.Context, id tracker.NativeWorkItemID, cursor string, limit int) (tracker.Page[tracker.NativeComment], error) {
	var result tracker.Page[tracker.NativeComment]
	err := c.readItemPage(ctx, id, "comments", cursor, limit, &result)
	return result, err
}

func (c *NativeClient) HistoryPage(ctx context.Context, id tracker.NativeWorkItemID, cursor string, limit int) (tracker.Page[tracker.CollaborationEvent], error) {
	var result tracker.Page[tracker.CollaborationEvent]
	err := c.readItemPage(ctx, id, "history", cursor, limit, &result)
	return result, err
}

func (c *NativeClient) AttemptsPage(ctx context.Context, id tracker.NativeWorkItemID, cursor string, limit int) (tracker.Page[tracker.NativeAttempt], error) {
	var result tracker.Page[tracker.NativeAttempt]
	err := c.readItemPage(ctx, id, "attempts", cursor, limit, &result)
	return result, err
}

func (c *NativeClient) readItemPage(ctx context.Context, id tracker.NativeWorkItemID, kind, cursor string, limit int, target any) error {
	path, err := nativeItemPath(id)
	if err != nil {
		return err
	}
	if limit < 1 || limit > 200 {
		return tracker.ErrInvalidCandidateQuery
	}
	params := url.Values{"limit": {strconv.Itoa(limit)}, "cursor": {cursor}}
	return c.client.request(ctx, http.MethodGet, c.base()+path+"/"+kind+"?"+params.Encode(), nil, target)
}

func (c *NativeClient) Version(ctx context.Context, id tracker.NativeWorkItemID, comment string, revision int64) (json.RawMessage, error) {
	var result json.RawMessage
	path, err := nativeItemPath(id)
	if err != nil {
		return result, err
	}
	if revision <= 0 {
		return result, tracker.ErrInvalidCandidateQuery
	}
	if comment != "" {
		path += "/comments/" + url.PathEscape(comment)
	}
	err = c.client.request(ctx, http.MethodGet, c.base()+path+"/versions/"+strconv.FormatInt(revision, 10), nil, &result)
	return result, err
}

func (c *NativeClient) Labels(ctx context.Context) ([]tracker.NativeLabel, error) {
	var result struct {
		Items []tracker.NativeLabel `json:"items"`
	}
	err := c.client.request(ctx, http.MethodGet, c.base()+"/labels", nil, &result)
	return result.Items, err
}
