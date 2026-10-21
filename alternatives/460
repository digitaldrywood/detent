package attachment

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"

	"github.com/digitaldrywood/detent/internal/artifact"
)

type ArchiveContentStore interface {
	PublishContent(context.Context, string, []byte) (ArchiveReference, error)
	ReadContent(context.Context, string, ArchiveReference) ([]byte, error)
}

type ArchiveContents struct {
	Storage ArchiveStorage
}

func (s ArchiveContents) PublishContent(ctx context.Context, organization string, content []byte) (ArchiveReference, error) {
	if s.Storage == nil {
		return ArchiveReference{}, artifact.ErrStorage
	}
	ref, data, err := PrepareArchive(organization, 1, bytes.NewReader(content))
	if err != nil {
		return ArchiveReference{}, err
	}
	if err := StoreArchive(ctx, s.Storage, organization, ref, data); err != nil {
		return ArchiveReference{}, err
	}
	return ref, nil
}

func (s ArchiveContents) ReadContent(ctx context.Context, organization string, ref ArchiveReference) ([]byte, error) {
	if s.Storage == nil || ref.Validate(organization) != nil {
		return nil, ErrInvalid
	}
	body, err := s.Storage.Open(ctx, ref.Key)
	if err != nil {
		return nil, artifact.ErrStorage
	}
	data, err := io.ReadAll(io.LimitReader(body, ref.CompressedSize+1))
	if err = errors.Join(err, body.Close()); err != nil {
		return nil, artifact.ErrStorage
	}
	if int64(len(data)) != ref.CompressedSize || artifact.Digest(data) != ref.SHA256 {
		return nil, artifact.ErrIntegrity
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, artifact.ErrIntegrity
	}
	reader.Multistream(false)
	content, err := io.ReadAll(io.LimitReader(reader, ref.UncompressedSize+1))
	if err = errors.Join(err, reader.Close()); err != nil || int64(len(content)) != ref.UncompressedSize {
		return nil, artifact.ErrIntegrity
	}
	return content, nil
}
