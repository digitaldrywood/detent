package hubcontent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"
)

func ExpandStorageConversion(ctx context.Context, size int) error {
	return storageconversion.Expand(ctx, size)
}

func ResolveStorageJSON(ctx context.Context, raw []byte) ([]byte, error) {
	if bytes.HasPrefix(raw, []byte("orgs/")) {
		return ReadStoredPayload(ctx, string(raw))
	}
	if ExplanationRead(ctx) {
		return raw, nil
	}
	return ResolveStorageJSONWith(ctx, raw, ReadChangeContent)
}

func DecodeStorageJSON(ctx context.Context, raw []byte, result any) error {
	data, err := ResolveStorageJSON(ctx, raw)
	if err != nil {
		return fmt.Errorf("read converted content: %w", err)
	}
	if err := ExpandStorageConversion(ctx, len(data)); err != nil {
		return err
	}
	return json.Unmarshal(data, result)
}

func ResolveStorageJSONWith(ctx context.Context, raw []byte, read func(context.Context, string, attachment.ArchiveReference) ([]byte, error)) ([]byte, error) {
	return storageconversion.ResolveJSONWith(ctx, raw, read)
}
