package hubclient

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func changePath(item tracker.NativeWorkItemID, change, version string) (string, error) {
	path, err := nativeItemPath(item)
	if err != nil {
		return "", err
	}
	for _, part := range []struct{ value, prefix string }{{change, "change_"}, {version, "version_"}} {
		if part.value == "" {
			continue
		}
		if !strings.HasPrefix(part.value, part.prefix) || strings.ContainsAny(part.value, "/?#%\\") || part.value == part.prefix {
			return "", errors.New("invalid Change Request or version identity")
		}
	}
	path += "/changes"
	if change != "" {
		path += "/" + change
	}
	if version != "" {
		path += "/versions/" + version
	}
	return path, nil
}

func (c *NativeClient) Changes(ctx context.Context, item tracker.NativeWorkItemID) ([]tracker.ChangeRequest, error) {
	var result []tracker.ChangeRequest
	path, err := changePath(item, "", "")
	if err != nil {
		return nil, err
	}
	err = c.client.request(ctx, http.MethodGet, c.base()+path, nil, &result)
	return result, err
}

func (c *NativeClient) Change(ctx context.Context, item tracker.NativeWorkItemID, id string) (tracker.ChangeDetail, error) {
	var result tracker.ChangeDetail
	path, err := changePath(item, id, "")
	if err != nil || id == "" {
		return result, errors.New("valid Change Request identity is required")
	}
	err = c.client.request(ctx, http.MethodGet, c.base()+path, nil, &result)
	return result, err
}

func (c *NativeClient) runtimeChange(ctx context.Context, item tracker.NativeWorkItemID, id string) (tracker.ChangeDetail, error) {
	path, err := changePath(item, id, "")
	if err != nil || id == "" {
		return tracker.ChangeDetail{}, errors.New("valid Change Request identity is required")
	}
	read := func(params url.Values) (operatortool.ChangeResult, error) {
		var page operatortool.ChangeResult
		if err := c.client.request(ctx, http.MethodGet, c.base()+path+"?"+params.Encode(), nil, &page); err != nil {
			return page, err
		}
		if page.OrganizationID != string(c.organization) || page.ProjectID != string(c.project) || page.WorkItemID != string(item) || page.Detail == nil || page.Read == nil || page.Detail.Change.ID != id || page.Detail.Change.ProjectID != c.project || page.Detail.Change.WorkItemID != item || page.Read.VersionID != page.Detail.Change.CurrentVersion {
			return page, fmt.Errorf("%w: bounded Change read has incomplete or mismatched identity", ErrUnavailable)
		}
		return page, nil
	}
	first, err := read(url.Values{"view": {"bounded"}})
	if err != nil {
		return tracker.ChangeDetail{}, err
	}
	detail := *first.Detail
	for _, section := range []string{"versions", "reviews", "checks", "discussion"} {
		page, ok := first.Read.Sections[section]
		seen := map[string]bool{}
		var record []byte
		var identity tracker.NativeRecoveryPage
		for {
			if !ok {
				return tracker.ChangeDetail{}, fmt.Errorf("%w: bounded Change section is missing", ErrUnavailable)
			}
			if page.Complete {
				if len(record) != 0 {
					return tracker.ChangeDetail{}, fmt.Errorf("%w: retained Change review is incomplete", ErrUnavailable)
				}
				break
			}
			if page.NextCursor == "" || seen[page.NextCursor] {
				return tracker.ChangeDetail{}, fmt.Errorf("%w: bounded Change cursor did not progress", ErrUnavailable)
			}
			seen[page.NextCursor] = true
			next, err := read(url.Values{"view": {"bounded"}, "section": {section}, "cursor": {page.NextCursor}, "version_id": {first.Read.VersionID}})
			if err != nil {
				return tracker.ChangeDetail{}, err
			}
			if next.Detail.Change.CurrentVersion != detail.Change.CurrentVersion {
				return tracker.ChangeDetail{}, fmt.Errorf("%w: bounded Change head changed during traversal", ErrUnavailable)
			}
			if fragment := next.Fragment; fragment != nil {
				if len(record) == 0 {
					identity = *fragment
				}
				if section != "reviews" || fragment.Section != section || fragment.OrganizationID != c.organization || fragment.ProjectID != c.project || fragment.WorkItemID != item || fragment.VersionID != first.Read.VersionID || fragment.RecordID == "" || fragment.Offset != int64(len(record)) || len(fragment.Data) == 0 || len(fragment.Data) > 64<<10 || fragment.TotalBytes <= fragment.Offset || int64(len(fragment.Data)) > fragment.TotalBytes-fragment.Offset || len(fragment.SHA256) != 64 || fragment.RecordID != identity.RecordID || fragment.VersionID != identity.VersionID || fragment.TotalBytes != identity.TotalBytes || fragment.SHA256 != identity.SHA256 || len(next.Detail.Reviews) != 0 {
					return tracker.ChangeDetail{}, fmt.Errorf("%w: retained Change review fragment is invalid", ErrUnavailable)
				}
				record = append(record, fragment.Data...)
				if int64(len(record)) == fragment.TotalBytes {
					digest := sha256.Sum256(record)
					var review tracker.ChangeReview
					if hex.EncodeToString(digest[:]) != fragment.SHA256 || json.Unmarshal(record, &review) != nil || review.ID == "" || review.VersionID != fragment.VersionID || slices.ContainsFunc(detail.Reviews, func(previous tracker.ChangeReview) bool { return previous.ID == review.ID }) {
						return tracker.ChangeDetail{}, fmt.Errorf("%w: retained Change review digest or identity is invalid", ErrUnavailable)
					}
					next.Detail.Reviews = append(next.Detail.Reviews, review)
					record = nil
				}
			} else if len(record) != 0 {
				return tracker.ChangeDetail{}, fmt.Errorf("%w: retained Change review fragment is missing", ErrUnavailable)
			}
			switch section {
			case "versions":
				detail.Versions = append(detail.Versions, next.Detail.Versions...)
				detail.PublicationPushes = append(detail.PublicationPushes, next.Detail.PublicationPushes...)
			case "reviews":
				detail.Reviews = append(detail.Reviews, next.Detail.Reviews...)
			case "checks":
				detail.Checks = append(detail.Checks, next.Detail.Checks...)
			case "discussion":
				detail.Discussion = append(detail.Discussion, next.Detail.Discussion...)
			}
			page, ok = next.Read.Sections[section]
		}
	}
	slices.SortFunc(detail.Versions, func(a, b tracker.ChangeVersion) int { return cmp.Compare(a.Number, b.Number) })
	return detail, nil
}

func (c *NativeClient) currentChange(ctx context.Context, item tracker.NativeWorkItemID) (tracker.ChangeDetail, error) {
	changes, err := c.Changes(ctx, item)
	if err != nil {
		return tracker.ChangeDetail{}, fmt.Errorf("list changes: %w", err)
	}
	if len(changes) == 0 {
		return tracker.ChangeDetail{}, nil
	}
	detail, err := c.runtimeChange(ctx, item, changes[len(changes)-1].ID)
	if err != nil {
		return tracker.ChangeDetail{}, fmt.Errorf("read change: %w", err)
	}
	return detail, nil
}

func (c *NativeClient) CreateChange(ctx context.Context, item tracker.NativeWorkItemID, request tracker.CreateChange) (tracker.ChangeRequest, error) {
	var result tracker.ChangeRequest
	path, err := changePath(item, "", "")
	if err != nil {
		return result, err
	}
	request.Mutation = c.fencedMutation(ctx, item, request.Mutation)
	err = c.client.request(ctx, http.MethodPost, c.base()+path, request, &result)
	return result, err
}

func (c *NativeClient) PublishChangeVersion(ctx context.Context, item tracker.NativeWorkItemID, id string, request tracker.PublishChangeVersion) (tracker.ChangeVersion, error) {
	var result tracker.ChangeVersion
	path, err := changePath(item, id, "")
	if err != nil || id == "" {
		return result, errors.New("valid Change Request identity is required")
	}
	request.Mutation = c.fencedMutation(ctx, item, request.Mutation)
	err = c.client.request(ctx, http.MethodPost, c.base()+path+"/versions", request, &result)
	return result, err
}

func (c *NativeClient) ReviewChange(ctx context.Context, item tracker.NativeWorkItemID, id, version string, request tracker.ReviewChange) (tracker.ChangeReview, error) {
	var result tracker.ChangeReview
	path, err := changePath(item, id, version)
	if err != nil || id == "" || version == "" {
		return result, errors.New("valid Change Request and version identities are required")
	}
	err = c.client.request(ctx, http.MethodPost, c.base()+path+"/reviews", request, &result)
	return result, err
}

func (c *NativeClient) ChangeViewedFiles(ctx context.Context, item tracker.NativeWorkItemID, id, version string) ([]tracker.ChangeViewedFile, error) {
	var result []tracker.ChangeViewedFile
	path, err := changePath(item, id, version)
	if err != nil || id == "" || version == "" {
		return nil, errors.New("valid Change Request and version identities are required")
	}
	err = c.client.request(ctx, http.MethodGet, c.base()+path+"/viewed-files", nil, &result)
	return result, err
}

func (c *NativeClient) ViewChangeFile(ctx context.Context, item tracker.NativeWorkItemID, id, version string, request tracker.ViewChangeFile) (tracker.ChangeViewedFile, error) {
	var result tracker.ChangeViewedFile
	path, err := changePath(item, id, version)
	if err != nil || id == "" || version == "" {
		return result, errors.New("valid Change Request and version identities are required")
	}
	err = c.client.request(ctx, http.MethodPost, c.base()+path+"/viewed-files", request, &result)
	return result, err
}

func (c *NativeClient) SubmitChangeCheck(ctx context.Context, item tracker.NativeWorkItemID, id, version string, request tracker.SubmitChangeCheck) (tracker.ChangeCheck, error) {
	var result tracker.ChangeCheck
	path, err := changePath(item, id, version)
	if err != nil || id == "" || version == "" {
		return result, errors.New("valid Change Request and version identities are required")
	}
	err = c.client.request(ctx, http.MethodPost, c.base()+path+"/checks", request, &result)
	return result, err
}

func (c *NativeClient) DiscussChange(ctx context.Context, item tracker.NativeWorkItemID, id string, request tracker.DiscussChange) (tracker.ChangeDiscussion, error) {
	var result tracker.ChangeDiscussion
	path, err := changePath(item, id, "")
	if err != nil || id == "" {
		return result, errors.New("valid Change Request identity is required")
	}
	request.Mutation = c.fencedMutation(ctx, item, request.Mutation)
	err = c.client.request(ctx, http.MethodPost, c.base()+path+"/discussion", request, &result)
	return result, err
}

func (c *NativeClient) ApproveChangeReviewPolicy(ctx context.Context, request tracker.ApproveChangeReviewPolicy) (tracker.ChangeReviewPolicy, error) {
	var result tracker.ChangeReviewPolicy
	err := c.client.request(ctx, http.MethodPut, c.base()+"/change-review-policy", request, &result)
	return result, err
}

func (c *NativeConnector) FetchChanges(ctx context.Context, item tracker.NativeWorkItemID) ([]tracker.ChangeRequest, error) {
	return c.client.Changes(ctx, item)
}

func (c *NativeConnector) FetchChange(ctx context.Context, item tracker.NativeWorkItemID, id string) (tracker.ChangeDetail, error) {
	return c.client.Change(ctx, item, id)
}

func (c *NativeConnector) FetchNativeAttempts(ctx context.Context, item tracker.NativeWorkItemID) ([]tracker.NativeAttempt, error) {
	result := []tracker.NativeAttempt{}
	for cursor := ""; ; {
		page, err := c.client.Attempts(ctx, item, cursor)
		if err != nil {
			return nil, err
		}
		result = append(result, page.Items...)
		if page.NextCursor == "" {
			return result, nil
		}
		if page.NextCursor == cursor {
			return nil, errors.New("hub repeated attempt cursor")
		}
		cursor = page.NextCursor
	}
}

// LandChangeVersion reports the merge commit a reviewed version landed as.
// The mutation is fenced by the lease the landing run holds.
func (c *NativeClient) LandChangeVersion(ctx context.Context, item tracker.NativeWorkItemID, id, version string, request tracker.LandChangeVersion) (tracker.ChangeRequest, error) {
	var result tracker.ChangeRequest
	path, err := changePath(item, id, version)
	if err != nil || id == "" || version == "" {
		return result, errors.New("valid Change Request and version identities are required")
	}
	request.Mutation = c.fencedMutation(ctx, item, request.Mutation)
	err = c.client.request(ctx, http.MethodPost, c.base()+path+"/landing", request, &result)
	return result, err
}

func (c *NativeClient) AssociateReleasePick(ctx context.Context, item tracker.NativeWorkItemID, request tracker.AssociateReleasePick) (tracker.ReleaseDelivery, error) {
	var result tracker.ReleaseDelivery
	path, err := nativeItemPath(item)
	if err != nil {
		return result, err
	}
	err = c.client.request(ctx, http.MethodPost, c.base()+path+"/release-pick", request, &result)
	return result, err
}
