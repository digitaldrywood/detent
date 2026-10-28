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
	data, err := ReadArchiveObject(ctx, s.Storage, organization, ref)
	if err != nil {
		return nil, err
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, &ArchiveReadError{BytesRead: int64(len(data)), ExpectedSize: ref.CompressedSize, Err: errors.Join(artifact.ErrIntegrity, err)}
	}
	reader.Multistream(false)
	content, err := io.ReadAll(io.LimitReader(reader, ref.UncompressedSize+1))
	if err = errors.Join(err, reader.Close()); err != nil || int64(len(content)) != ref.UncompressedSize {
		return nil, &ArchiveReadError{BytesRead: int64(len(data)), ExpectedSize: ref.CompressedSize, Err: errors.Join(artifact.ErrIntegrity, err)}
	}
	return content, nil
}
