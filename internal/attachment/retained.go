package attachment

import (
	"context"
	"errors"
	"io"

	"github.com/digitaldrywood/detent/internal/artifact"
)

type RetainedReference struct {
	ID     string
	SHA256 string
	Size   int64
}

func (r RetainedReference) Validate(organization string) error {
	if _, err := Key(organization, r.ID); err != nil || !artifact.ValidHash(r.SHA256, 64) || r.Size <= 0 || r.Size > MaxBytes {
		return ErrInvalid
	}
	return nil
}

func ReadRetained(ctx context.Context, store Storage, organization string, ref RetainedReference) ([]byte, error) {
	if store == nil || ref.Validate(organization) != nil {
		return nil, ErrInvalid
	}
	key, err := Key(organization, ref.ID)
	if err != nil {
		return nil, err
	}
	object, err := store.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(object, ref.Size+1))
	if err = errors.Join(err, object.Close()); err != nil {
		return nil, err
	}
	if int64(len(raw)) != ref.Size || artifact.Digest(raw) != ref.SHA256 {
		return nil, artifact.ErrIntegrity
	}
	return raw, nil
}
