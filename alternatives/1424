package hubclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *NativeClient) PullRequestDetails(ctx context.Context, item tracker.NativeWorkItemID) ([]tracker.PullRequestView, error) {
	var result []tracker.PullRequestView
	path, err := nativeItemPath(item)
	if err != nil {
		return result, err
	}
	err = c.client.request(ctx, http.MethodGet, c.base()+path+"/pull-requests", nil, &result)
	return result, err
}

func (c *NativeClient) StoredDiff(ctx context.Context, item tracker.NativeWorkItemID, attempt, source string, sequence int64) (*tracker.AttemptDiff, error) {
	path, err := nativeItemPath(item)
	if err != nil {
		return nil, err
	}
	path += "/diff"
	if attempt != "" {
		path, err = nativeAttemptDiffPath(attempt)
		if err != nil {
			return nil, err
		}
	}
	params := url.Values{}
	if source != "" {
		params.Set("source", source)
	}
	if sequence > 0 {
		params.Set("at", strconv.FormatInt(sequence, 10))
	}
	path += "?" + params.Encode()
	if attempt != "" {
		owned, err := c.NativeAttempt(ctx, item, attempt)
		if err != nil {
			return nil, err
		}
		var result tracker.AttemptDiff
		err = c.client.request(ctx, http.MethodGet, c.base()+path, nil, &result)
		// An attempt-addressed endpoint still needs the work-item ownership
		// match. Compare with the member-authorized attempt resource.
		if err != nil {
			return nil, err
		}
		if owned.AttemptID != attempt || result.AttemptID != attempt {
			return nil, operatortool.ErrAccessDenied
		}
		return &result, nil
	}
	var result tracker.WorkItemDiff
	err = c.client.request(ctx, http.MethodGet, c.base()+path, nil, &result)
	return result.Diff, err
}

func (c *NativeClient) BindArtifactService(ctx context.Context, binding artifact.Binding, requestID string) (artifact.Binding, error) {
	var result artifact.Binding
	request := struct {
		artifact.Binding
		tracker.Mutation
	}{binding, tracker.Mutation{IdempotencyKey: requestID}}
	err := c.client.request(ctx, http.MethodPut, c.base()+"/artifact-services/"+binding.ServiceID, request, &result)
	return result, err
}

func (c *NativeClient) NativeAttempt(ctx context.Context, item tracker.NativeWorkItemID, attempt string) (tracker.NativeAttempt, error) {
	var result tracker.NativeAttempt
	path, err := nativeItemPath(item)
	if err != nil {
		return result, err
	}
	if !strings.HasPrefix(attempt, "attempt_") || strings.ContainsAny(attempt, "/\\?#%") {
		return result, operatortool.ErrInvalidArguments
	}
	err = c.client.request(ctx, http.MethodGet, c.base()+path+"/attempts/"+attempt, nil, &result)
	return result, err
}

func (c *NativeClient) ChangeReviewPolicy(ctx context.Context) (tracker.ChangeReviewPolicy, error) {
	var result tracker.ChangeReviewPolicy
	err := c.client.request(ctx, http.MethodGet, c.base()+"/change-review-policy", nil, &result)
	return result, err
}
func (c *NativeClient) ArtifactServices(ctx context.Context) ([]artifact.Binding, error) {
	var result []artifact.Binding
	err := c.client.request(ctx, http.MethodGet, c.base()+"/artifact-services", nil, &result)
	return result, err
}

// ArtifactDownload reuses the same reference ownership and ephemeral read grant
// as dashboard downloads. No caller-provided origin or producer identity is used.
func (c *NativeClient) ArtifactDownload(ctx context.Context, item tracker.NativeWorkItemID, id string, revision int64, sha256 string) (operatortool.ArtifactDownload, error) {
	ref, err := c.ArtifactReference(ctx, item, id, revision)
	if err != nil {
		return operatortool.ArtifactDownload{}, err
	}
	if sha256 != "" && ref.SHA256 != sha256 || ref.Availability != "available" {
		return operatortool.ArtifactDownload{}, operatortool.ErrAccessDenied
	}
	grant, err := c.ArtifactGrant(ctx, item, id, revision)
	if err != nil {
		return operatortool.ArtifactDownload{}, err
	}
	if grant.ArtifactID != id || grant.Revision != revision || sha256 != "" && grant.SHA256 != sha256 {
		return operatortool.ArtifactDownload{}, operatortool.ErrAccessDenied
	}
	return operatortool.DownloadResult(grant)
}

func (c *NativeClient) ArtifactReference(ctx context.Context, item tracker.NativeWorkItemID, id string, revision int64) (artifact.Reference, error) {
	var result artifact.Reference
	path, err := nativeItemPath(item)
	if err != nil || !artifact.ValidID(id, "artifact") || revision < 1 {
		return result, artifact.ErrInvalid
	}
	err = c.client.request(ctx, http.MethodGet, c.base()+path+"/artifacts/"+id+"/revisions/"+strconv.FormatInt(revision, 10), nil, &result)
	return result, err
}
