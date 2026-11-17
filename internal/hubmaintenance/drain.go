package hubmaintenance

import (
	"context"
	"errors"
)

func Drain[T any](ctx context.Context, limit int, after *T, step func(T, []T) (T, bool, bool, error)) (Outcome, error) {
	var failures []error
	attempted := make([]T, 0, limit)
	for len(attempted) < limit {
		next, processed, required, err := step(*after, attempted)
		*after = next
		if err != nil {
			failures = append(failures, err)
		}
		if !processed {
			break
		}
		attempted = append(attempted, next)
		if !required || ctx.Err() != nil {
			break
		}
	}
	return Outcome{Progress: len(attempted) > 0, Items: len(attempted)}, errors.Join(failures...)
}
