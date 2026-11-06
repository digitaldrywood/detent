package nativechange

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubclient/evidence"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) retainChangeValidation(ctx context.Context, input *tracker.ChangeVersionInput) error {
	a := &e.artifacts
	a.mu.Lock()
	defer a.mu.Unlock()
	var uploader evidence.Uploader
	if a.client != nil {
		uploader = a.client
	}
	return evidence.Retain(ctx, input, uploader, a.reservation, e.data.AttemptID)
}
