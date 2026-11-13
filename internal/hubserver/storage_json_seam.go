package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"
)

type storageJSONReference = storageconversion.JSONReference
type storageJSONPayload = storageconversion.JSONPayload

func resolveStorageJSON(ctx context.Context, raw []byte) ([]byte, error) {
	if bytes.HasPrefix(raw, []byte("orgs/")) {
		return readStoredPayload(ctx, string(raw))
	}
	if explanationRead(ctx) {
		return raw, nil
	}
	return resolveStorageJSONWith(ctx, raw, readChangeContent)
}

func decodeStorageJSON(ctx context.Context, raw []byte, result any) error {
	data, err := resolveStorageJSON(ctx, raw)
	if err != nil {
		return fmt.Errorf("read converted content: %w", err)
	}
	if err := expandStorageConversion(ctx, len(data)); err != nil {
		return err
	}
	return json.Unmarshal(data, result)
}

func resolveStorageJSONWith(ctx context.Context, raw []byte, read func(context.Context, string, attachment.ArchiveReference) ([]byte, error)) ([]byte, error) {
	return storageconversion.ResolveJSONWith(ctx, raw, read)
}
