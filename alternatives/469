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
	body, err := storage.Open(ctx, ref.Key)
	if err != nil {
		return nil, artifact.ErrStorage
	}
	compressed, err := io.ReadAll(io.LimitReader(body, ref.CompressedSize+1))
	if err = errors.Join(err, body.Close()); err != nil {
		return nil, artifact.ErrStorage
	}
	if int64(len(compressed)) != ref.CompressedSize || artifact.Digest(compressed) != ref.SHA256 {
		return nil, artifact.ErrIntegrity
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, artifact.ErrIntegrity
	}
	data, err := io.ReadAll(io.LimitReader(reader, ref.UncompressedSize+1))
	if err = errors.Join(err, reader.Close()); err != nil || int64(len(data)) != ref.UncompressedSize {
		return nil, artifact.ErrIntegrity
	}
	return data, nil
}
