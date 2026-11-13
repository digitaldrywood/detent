package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/reportdata"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func decodeActivityRun(ctx context.Context, q nativeQueryer, raw string, data *tracker.NativeRunData) error {
	return reportdata.DecodeRun(ctx, raw, data, decodeStorageJSON, func(ctx context.Context, raw string, data *tracker.NativeRunData) error {
		return decodeRunDetail(ctx, q, raw, data)
	})
}
