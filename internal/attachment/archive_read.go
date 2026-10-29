package attachment

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/digitaldrywood/detent/internal/artifact"
)

type ArchiveReadError struct {
	BytesRead    int64
	ExpectedSize int64
	Err          error
}

func (e *ArchiveReadError) Error() string { return fmt.Sprintf("read archive: %v", e.Err) }
func (e *ArchiveReadError) Unwrap() error { return e.Err }

func ReadArchiveObject(ctx context.Context, storage Storage, organization string, ref ArchiveReference) ([]byte, error) {
	if ref.Validate(organization) != nil {
		return nil, ErrInvalid
	}
	failure := func(size int, reason, cause error) ([]byte, error) {
		return nil, &ArchiveReadError{BytesRead: int64(size), ExpectedSize: ref.CompressedSize, Err: errors.Join(reason, cause)}
	}
	body, err := storage.Open(ctx, ref.Key)
	if err != nil {
		return failure(0, artifact.ErrStorage, fmt.Errorf("open object: %w", errors.Join(err, ctx.Err())))
	}
	data, err := io.ReadAll(io.LimitReader(body, ref.CompressedSize+1))
	err = errors.Join(err, body.Close(), ctx.Err())
	if err != nil {
		return failure(len(data), artifact.ErrStorage, fmt.Errorf("read object: %w", err))
	}
	if int64(len(data)) != ref.CompressedSize || artifact.Digest(data) != ref.SHA256 {
		return failure(len(data), artifact.ErrIntegrity, nil)
	}
	return data, nil
}
