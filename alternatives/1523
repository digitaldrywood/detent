package hubserver

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) changeSource(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	change, err := readChange(ctx, s.database.reader, nativeRequestScope(c), c.Param("item"), c.Param("change"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	version, err := readChangeVersion(ctx, s.database.reader, change.ID, c.Param("version"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if version.Source == nil {
		return s.nativeAPIError(c, nativeNotFound())
	}
	bundle, err := readChangeSourceBundle(ctx, s.database.reader, version)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := version.Source.Validate(version.BaseSHA, version.HeadSHA, bundle); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.Blob(http.StatusOK, "application/x-git-bundle", bundle)
}

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
