package webhooks

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"github.com/google/uuid"
	"io"
	"strings"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

func PayloadKey(deliveryID, digest string) string {
	return attachment.GitHubWebhookPrefix + base64.RawURLEncoding.EncodeToString([]byte(deliveryID)) + "/" + digest
}

func ValidPayloadKey(deliveryID, digest, key string) bool {
	if deliveryID == "" || len(deliveryID) > 256 || !artifact.ValidHash(digest, 64) {
		return false
	}
	base := PayloadKey(deliveryID, digest)
	if key == base {
		return true
	}
	revision, ok := strings.CutPrefix(key, base+"/")
	id, err := uuid.Parse(revision)
	return ok && err == nil && id.String() == revision
}

func SweepReleasedPayload(ctx context.Context, store attachment.Storage, released func(context.Context, string, string, string) (bool, error), key string) error {
	suffix, ok := strings.CutPrefix(key, attachment.GitHubWebhookPrefix)
	if !ok {
		return nil
	}
	encoded, digest, ok := strings.Cut(suffix, "/")
	if !ok || len(encoded) > base64.RawURLEncoding.EncodedLen(256) {
		return nil
	}
	digest, _, _ = strings.Cut(digest, "/")
	id, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || !ValidPayloadKey(string(id), digest, key) {
		return nil
	}
	done, err := released(ctx, string(id), digest, key)
	if err != nil || !done {
		return err
	}
	return store.Delete(ctx, key)
}

func StorePayload(ctx context.Context, store attachment.Storage, deliveryID, digest, key string, payload []byte) error {
	if store == nil {
		return artifact.ErrStorage
	}
	if !ValidPayloadKey(deliveryID, digest, key) || len(payload) > cloudassert.MaxBodyBytes || artifact.Digest(payload) != digest {
		return artifact.ErrIntegrity
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, err := writer.Write(payload)
	if err := errors.Join(err, writer.Close()); err != nil {
		return err
	}
	data := compressed.Bytes()
	if err := store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), "application/gzip", artifact.Digest(data)); err != nil {
		_, readErr := ReadPayload(ctx, store, deliveryID, digest, key)
		if readErr != nil {
			return errors.Join(err, readErr)
		}
	}
	return nil
}

func ReadPayload(ctx context.Context, store attachment.Storage, deliveryID, digest, key string) ([]byte, error) {
	if store == nil {
		return nil, artifact.ErrStorage
	}
	if !ValidPayloadKey(deliveryID, digest, key) {
		return nil, artifact.ErrIntegrity
	}
	body, err := store.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	const maxCompressed = cloudassert.MaxBodyBytes + 64<<10
	compressed, err := io.ReadAll(io.LimitReader(body, maxCompressed+1))
	if err := errors.Join(err, body.Close()); err != nil {
		return nil, err
	}
	if len(compressed) > maxCompressed {
		return nil, artifact.ErrIntegrity
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, artifact.ErrIntegrity
	}
	data, err := io.ReadAll(io.LimitReader(reader, cloudassert.MaxBodyBytes+1))
	if err := errors.Join(err, reader.Close()); err != nil {
		return nil, artifact.ErrIntegrity
	}
	if len(data) > cloudassert.MaxBodyBytes || artifact.Digest(data) != digest {
		return nil, artifact.ErrIntegrity
	}
	return data, nil
}
