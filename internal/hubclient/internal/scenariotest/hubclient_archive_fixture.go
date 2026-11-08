package scenariotest

import (
	"bytes"
	"context"
	"io"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

type hubclientArchiveStorage struct {
	attachment.Storage
	details *archivedRunDetails
}

func (s hubclientArchiveStorage) Open(_ context.Context, key string) (io.ReadCloser, error) {
	s.details.mu.Lock()
	data, ok := s.details.archives[key]
	data = bytes.Clone(data)
	s.details.mu.Unlock()
	if !ok {
		return nil, artifact.ErrMissing
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *archivedRunDetails) ReadCanonicalDetail(ctx context.Context, organization, project, key string) ([]byte, error) {
	return attachment.ReadCanonicalArchive(ctx, hubclientArchiveStorage{details: s}, organization, project, key)
}
