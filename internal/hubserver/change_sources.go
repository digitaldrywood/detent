package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readChangeSourceBundle(ctx context.Context, query nativeQueryer, version tracker.ChangeVersion) ([]byte, error) {
	ref := contentReference()
	var organization string
	var bundle []byte
	if err := query.QueryRowContext(ctx, `SELECT s.bundle,s.object_key,s.object_sha256,s.object_bytes,s.content_bytes,c.organization_id FROM change_sources s JOIN change_versions v ON v.id=s.version_id JOIN change_requests c ON c.id=v.change_id WHERE s.version_id=?`, version.ID).Scan(&bundle, &ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize, &organization); err != nil {
		return nil, err
	}
	if ref.Key != "" {
		return readChangeContent(ctx, organization, ref)
	}
	decoded, err := decodeChangeSource(bundle, version.Source.Bytes)
	if err != nil {
		return nil, artifact.ErrIntegrity
	}
	return decoded, nil
}
