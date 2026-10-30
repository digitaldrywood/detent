package api

import (
	"context"
	"net/http"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *NativeClient) Artifacts(ctx context.Context, item tracker.NativeWorkItemID) ([]artifact.Reference, error) {
	var refs []artifact.Reference
	path, err := NativeItemPath(item)
	if err != nil {
		return nil, err
	}
	err = c.client.request(ctx, http.MethodGet, c.base()+path+"/artifacts", nil, &refs)
	return refs, err
}

func (c *NativeClient) ArtifactGrant(ctx context.Context, item tracker.NativeWorkItemID, id string, revision int64) (artifact.Grant, error) {
	var grant artifact.Grant
	path, err := NativeItemPath(item)
	if err != nil || !artifact.ValidID(id, "artifact") {
		return grant, artifact.ErrInvalid
	}
	err = c.client.request(ctx, http.MethodPost, c.base()+path+"/artifacts/"+id+"/access", struct {
		Revision int64 `json:"revision"`
	}{revision}, &grant)
	return grant, err
}

func (c *NativeConnector) FetchArtifacts(ctx context.Context, item tracker.NativeWorkItemID) ([]artifact.Reference, error) {
	return c.client.Artifacts(ctx, item)
}

func (c *NativeClient) ArtifactReader(token string) *NativeClient {
	return &NativeClient{organization: c.organization, project: c.project, client: &Client{baseURL: c.client.baseURL, httpClient: c.client.httpClient, tokenSource: func() string { return token }}}
}

func (c *Client) ArtifactTransport() http.RoundTripper { return c.httpClient.Transport }
