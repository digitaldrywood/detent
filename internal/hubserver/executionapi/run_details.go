package executionapi

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func DecodeRunHistory(ctx context.Context, q hubapi.NativeQueryer, raw string, result *tracker.CollaborationData) error {
	ctx = hubrecords.PayloadQueryContext(ctx, q)
	if err := hubcontent.DecodeStorageJSON(ctx, []byte(raw), result); err != nil {
		return err
	}
	if result.Run == nil {
		return nil
	}
	var envelope struct {
		Run json.RawMessage `json:"run"`
	}
	if err := hubcontent.DecodeStorageJSON(ctx, []byte(raw), &envelope); err != nil {
		return err
	}
	return hubrecords.DecodeRunDetail(ctx, q, string(envelope.Run), result.Run)
}
