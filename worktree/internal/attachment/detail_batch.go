package attachment

import (
	"context"
	"errors"
	"sync"

	"github.com/digitaldrywood/detent/internal/artifact"
)

const MaxDetailBatchReferences = 200

type DetailReadResult struct {
	Reference ArchiveReference
	Data      []byte
	ErrorCode string
}
type DetailBatchStore interface {
	ReadDetails(context.Context, string, []ArchiveReference) ([]DetailReadResult, error)
}

func ValidateDetailBatch(organization string, refs []ArchiveReference) error {
	if len(refs) == 0 || len(refs) > MaxDetailBatchReferences {
		return ErrInvalid
	}
	var compressed, content int64
	for _, ref := range refs {
		if ref.Validate(organization) != nil {
			return ErrInvalid
		}
		compressed += ref.CompressedSize
		content += ref.UncompressedSize
		if compressed > MaxArchiveBytes || content > MaxArchiveBytes {
			return ErrInvalid
		}
	}
	return nil
}
func (r DetailReadResult) Bytes() ([]byte, error) {
	switch r.ErrorCode {
	case "":
		if int64(len(r.Data)) != r.Reference.UncompressedSize {
			return nil, artifact.ErrIntegrity
		}
		return r.Data, nil
	case "missing":
		return nil, artifact.ErrMissing
	case "integrity":
		return nil, artifact.ErrIntegrity
	case "invalid":
		return nil, ErrInvalid
	case "deadline":
		return nil, context.DeadlineExceeded
	case "canceled":
		return nil, context.Canceled
	default:
		return nil, errors.New("run detail storage unavailable")
	}
}
func ReadDetailBatch(ctx context.Context, organization string, refs []ArchiveReference, read func(context.Context, string, ArchiveReference) ([]byte, error)) ([]DetailReadResult, error) {
	if ValidateDetailBatch(organization, refs) != nil || read == nil {
		return nil, ErrInvalid
	}
	results := make([]DetailReadResult, len(refs))
	var mu sync.Mutex
	next := 0
	var workers sync.WaitGroup
	for range min(8, len(refs)) {
		workers.Go(func() {
			for {
				mu.Lock()
				if next == len(refs) {
					mu.Unlock()
					return
				}
				index := next
				next++
				mu.Unlock()
				ref := refs[index]
				result := DetailReadResult{Reference: ref}
				var err error
				if ctx.Err() != nil {
					err = ctx.Err()
				} else {
					result.Data, err = read(ctx, organization, ref)
				}
				switch {
				case errors.Is(err, context.DeadlineExceeded):
					result.ErrorCode = "deadline"
				case errors.Is(err, context.Canceled):
					result.ErrorCode = "canceled"
				case errors.Is(err, artifact.ErrMissing):
					result.ErrorCode = "missing"
				case errors.Is(err, artifact.ErrIntegrity):
					result.ErrorCode = "integrity"
				case errors.Is(err, ErrInvalid):
					result.ErrorCode = "invalid"
				case err != nil:
					result.ErrorCode = "unavailable"
				}
				if err == nil && int64(len(result.Data)) != ref.UncompressedSize {
					result.Data = nil
					result.ErrorCode = "integrity"
				}
				if err != nil {
					result.Data = nil
				}
				results[index] = result
			}
		})
	}
	workers.Wait()
	return results, nil
}
