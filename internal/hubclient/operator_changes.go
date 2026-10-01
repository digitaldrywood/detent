package hubclient

import (
	"context"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"net/http"
	"strconv"
)

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
	if sha256 != "" && grant.SHA256 != sha256 {
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
