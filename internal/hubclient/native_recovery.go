package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"

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
	err = c.client.requestWithHeaders(ctx, http.MethodGet, c.base()+path+"?view=recovery", http.Header{"X-Detent-Recovery-Projection": {"bounded"}, "X-Detent-Recovery-Body": {"fragments"}}, nil, &result)
	if err == nil && result.Continuation != nil {
		return tracker.NativeRecovery{}, errors.New("read recovery: unbounded detail continuation is not candidate hydration")
	}
	if err == nil && (result.Issue.OrganizationID != c.organization || result.Issue.ProjectID != c.project || result.Issue.WorkItemID != id) {
		return result, errors.New("read recovery: scoped work item is missing")
	}
	if err == nil && result.Read != nil && result.Read.IssueBody != nil {
		err = c.restoreRecoveryBody(ctx, &result)
	} else if err == nil && result.Read != nil && slices.Contains(result.Read.Omitted, "issue.body") {
		err = errors.New("read recovery: required issue body reference is missing")
	}
	if err != nil {
		return tracker.NativeRecovery{}, err
	}
	return result, err
}

func (c *NativeClient) restoreRecoveryBody(ctx context.Context, recovery *tracker.NativeRecovery) error {
	read := recovery.Read.IssueBody
	const maxBody = 256 << 10
	if recovery.Issue.Body != "" || recovery.Issue.Revision <= 0 || read.Cursor == "" || len(read.SHA256) != 64 || read.TotalBytes < 2 || read.TotalBytes > 6*maxBody+2 {
		return errors.New("read recovery: invalid required issue body reference")
	}
	var body []byte
	cursor := read.Cursor
	for int64(len(body)) < read.TotalBytes {
		page, err := c.RecoveryEvidence(ctx, recovery.Issue.WorkItemID, cursor)
		if err != nil {
			return err
		}
		if page.Section != "issue_body" || page.RecordID != string(recovery.Issue.WorkItemID) || page.Revision != recovery.Issue.Revision || page.SHA256 != read.SHA256 || page.TotalBytes != read.TotalBytes || page.Offset != int64(len(body)) || len(page.Data) == 0 || len(page.Data) > 64<<10 {
			return errors.New("read recovery: required issue body changed or fragment is invalid")
		}
		body = append(body, page.Data...)
		if int64(len(body)) < read.TotalBytes && (page.NextCursor == "" || page.NextCursor == cursor) {
			return errors.New("read recovery: required issue body did not advance")
		}
		cursor = page.NextCursor
	}
	digest := sha256.Sum256(body)
	var restored string
	if hex.EncodeToString(digest[:]) != read.SHA256 || json.Unmarshal(body, &restored) != nil || len(restored) > maxBody {
		return errors.New("read recovery: required issue body digest or content is invalid")
	}
	recovery.Issue.Body = restored
	return nil
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

func (c *NativeClient) retainedChangeReviews(ctx context.Context, item tracker.NativeWorkItemID, version string) ([]tracker.ChangeReview, error) {
	var reviews []tracker.ChangeReview
	var record []byte
	var identity tracker.NativeRecoveryPage
	seen := map[string]bool{}
	for cursor := ""; ; {
		page, err := c.RecoveryEvidence(ctx, item, cursor)
		if err != nil {
			return nil, err
		}
		if page.Section != "reviews" {
			if len(record) != 0 || len(reviews) == 0 || !slices.Contains([]string{"checks", "change_discussion", "discussion", "attempts", "versions", "change", "issue_body", "complete"}, page.Section) {
				return nil, fmt.Errorf("%w: retained Change reviews are incomplete", ErrUnavailable)
			}
			return reviews, nil
		}
		if page.Offset == 0 {
			identity = page
		}
		if page.Offset != int64(len(record)) || len(page.Data) == 0 || len(page.Data) > 64<<10 || page.RecordID == "" || page.VersionID == "" || len(page.SHA256) != 64 || page.RecordID != identity.RecordID || page.VersionID != identity.VersionID || page.TotalBytes != identity.TotalBytes || page.SHA256 != identity.SHA256 {
			return nil, fmt.Errorf("%w: retained Change review fragment is invalid", ErrUnavailable)
		}
		record = append(record, page.Data...)
		if int64(len(record)) == page.TotalBytes {
			digest := sha256.Sum256(record)
			var review tracker.ChangeReview
			if hex.EncodeToString(digest[:]) != page.SHA256 || json.Unmarshal(record, &review) != nil || review.ID == "" || review.VersionID != page.VersionID {
				return nil, fmt.Errorf("%w: retained Change review digest or identity is invalid", ErrUnavailable)
			}
			if review.VersionID == version {
				if slices.ContainsFunc(reviews, func(previous tracker.ChangeReview) bool { return previous.ID == review.ID }) {
					return nil, fmt.Errorf("%w: retained Change review identity repeated", ErrUnavailable)
				}
				reviews = append(reviews, review)
			}
			record = nil
		}
		if page.NextCursor == "" || seen[page.NextCursor] {
			return nil, fmt.Errorf("%w: retained Change review cursor did not advance", ErrUnavailable)
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
}
