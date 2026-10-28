package attachment

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"

	"github.com/digitaldrywood/detent/internal/artifact"
)

type DetailStore interface {
	StoreDetail(context.Context, string, []byte) (ArchiveReference, error)
	ReadDetail(context.Context, string, ArchiveReference) ([]byte, error)
}

func ReadArchive(ctx context.Context, storage Storage, organization string, ref ArchiveReference) ([]byte, error) {
	if ref.Validate(organization) != nil {
		return nil, ErrInvalid
	}
	compressed, err := ReadArchiveObject(ctx, storage, organization, ref)
	if err != nil {
		return nil, err
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, &ArchiveReadError{BytesRead: int64(len(compressed)), ExpectedSize: ref.CompressedSize, Err: errors.Join(artifact.ErrIntegrity, err)}
	}
	data, err := io.ReadAll(io.LimitReader(reader, ref.UncompressedSize+1))
	if err = errors.Join(err, reader.Close()); err != nil || int64(len(data)) != ref.UncompressedSize {
		return nil, &ArchiveReadError{BytesRead: int64(len(compressed)), ExpectedSize: ref.CompressedSize, Err: errors.Join(artifact.ErrIntegrity, err)}
	}
	return data, nil
}
