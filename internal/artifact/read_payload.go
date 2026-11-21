package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

func ReadPayload(ctx context.Context, grant Grant, ref Reference) ([]byte, error) {
	if !ValidOrigin(grant.Origin) || grant.SHA256 != ref.SHA256 || ref.Bytes <= 0 || ref.Bytes > MaxArtifactBytes {
		return nil, ErrIntegrity
	}
	httpClient := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	read := func(path string, limit int64) ([]byte, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, grant.Origin+path, nil)
		if err != nil {
			return nil, ErrInvalid
		}
		request.Header.Set("Authorization", "Bearer "+grant.Token)
		response, err := httpClient.Do(request)
		if err != nil {
			return nil, ErrStorage
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, ErrStorage
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
		if err != nil || int64(len(data)) > limit {
			return nil, ErrIntegrity
		}
		return data, nil
	}
	path := fmt.Sprintf("/v1/artifacts/%s/manifests/%d", ref.ArtifactID, ref.Revision)
	raw, err := read(path, MaxManifestBytes)
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if Digest(raw) != ref.SHA256 || json.Unmarshal(raw, &manifest) != nil || manifest.Validate() != nil || manifest.Scope != ref.Scope || manifest.ArtifactID != ref.ArtifactID || manifest.Revision != ref.Revision || manifest.Kind != "log" || manifest.State != "complete" || manifest.TotalBytes != ref.Bytes {
		return nil, ErrIntegrity
	}
	var content []byte
	for _, object := range manifest.Objects {
		data, err := read(path+"/objects/"+object.ID, object.Size)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != object.Size || Digest(data) != object.SHA256 {
			return nil, ErrIntegrity
		}
		content = append(content, data...)
	}
	return content, nil
}
