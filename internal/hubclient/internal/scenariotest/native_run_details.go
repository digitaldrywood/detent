package scenariotest

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

type archivedRunDetails struct {
	mu       sync.Mutex
	archives map[string][]byte
}

func (s *archivedRunDetails) ReadDetail(_ context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Validate(organization) != nil {
		return nil, attachment.ErrInvalid
	}
	s.mu.Lock()
	compressed := bytes.Clone(s.archives[ref.Key])
	s.mu.Unlock()
	if int64(len(compressed)) != ref.CompressedSize || artifact.Digest(compressed) != ref.SHA256 {
		return nil, artifact.ErrIntegrity
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(reader)
	if err := errors.Join(err, reader.Close()); err != nil {
		return nil, err
	}
	if int64(len(data)) != ref.UncompressedSize {
		return nil, artifact.ErrIntegrity
	}
	return data, nil
}

func (s *archivedRunDetails) StoreDetail(_ context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	ref, compressed, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		return ref, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.archives == nil {
		s.archives = make(map[string][]byte)
	}
	s.archives[ref.Key] = bytes.Clone(compressed)
	return ref, nil
}

func (s *archivedRunDetails) ReadCanonicalDetail(ctx context.Context, organization, project, key string) ([]byte, error) {
	if err := attachment.ValidateCanonicalDetailKey(organization, project, key); err != nil {
		return nil, err
	}
	s.mu.Lock()
	compressed := bytes.Clone(s.archives[key])
	s.mu.Unlock()
	if artifact.Digest(compressed) != key[strings.LastIndex(key, "/")+1:] {
		return nil, artifact.ErrIntegrity
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, attachment.MaxArchiveBytes+1))
	if err := errors.Join(err, reader.Close(), ctx.Err()); err != nil {
		return nil, err
	}
	if len(data) > attachment.MaxArchiveBytes {
		return nil, artifact.ErrIntegrity
	}
	return s.ReadDetail(ctx, organization, attachment.ArchiveReference{Key: key, SHA256: artifact.Digest(compressed), CompressedSize: int64(len(compressed)), UncompressedSize: int64(len(data)), Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention})
}
