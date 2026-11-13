package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/reportdata"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func decodeActivityRun(ctx context.Context, q hubapi.NativeQueryer, raw string, data *tracker.NativeRunData) error {
	return reportdata.DecodeRun(ctx, raw, data, hubcontent.DecodeStorageJSON, func(ctx context.Context, raw string, data *tracker.NativeRunData) error {
		return hubrecords.DecodeRunDetail(ctx, q, raw, data)
	})
}
