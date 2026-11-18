package transport

import (
	"context"
	"errors"
)

var (
	ErrUnavailable          = errors.New("hub unavailable")
	ErrRejected             = errors.New("Hub request rejected")
	ErrAuthorityUnavailable = errors.New("execution authority unavailable")
)

func DeferObservation(ctx context.Context, err error) bool {
	return errors.Is(err, ErrUnavailable) && ctx.Err() == nil && !errors.Is(err, ErrAuthorityUnavailable) && !errors.Is(err, ErrRejected) && !errors.Is(err, context.Canceled)
}

func ObservationError(ctx context.Context, err error) error {
	if DeferObservation(ctx, err) {
		return nil
	}
	return err
}
