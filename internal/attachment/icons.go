package attachment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
)

type IconStore interface {
	PutIcon(context.Context, string, string, []byte) error
}

type Icons struct{ Storage Storage }

func (s Icons) PutIcon(ctx context.Context, organization, id string, content []byte) error {
	key, err := Key(organization, id)
	if err != nil || s.Storage == nil || len(content) == 0 || len(content) > 256*1024 {
		return ErrInvalid
	}
	digest := sha256.Sum256(content)
	return s.Storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "image/png", hex.EncodeToString(digest[:]))
}
