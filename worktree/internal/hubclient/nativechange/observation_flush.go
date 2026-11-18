package nativechange

import (
	"context"
	"slices"
)

func (e *nativeExecution) flushObservations(ctx context.Context) error {
	if err := e.flush(ctx); err != nil {
		return err
	}
	if len(e.observations) == 0 {
		return nil
	}
	last := len(e.observations) - 1
	event := e.observations[last]
	event.PriorEvents = slices.Clone(e.observations[:last])
	e.pending = &event
	return e.flush(ctx)
}
