package pgcore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func Affected(ctx context.Context, rows int64, err error) error {
	if err != nil {
		return MapError(ctx, err)
	}
	if rows == 0 {
		return corestore.ErrNotFound
	}
	return nil
}

func Updated(ctx context.Context, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return corestore.ErrRevisionConflict
	}
	return MapError(ctx, err)
}

func MapError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil && (errors.Is(err, pgx.ErrTxClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return fmt.Errorf("PostgreSQL operation: %w", ctx.Err())
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		var domain error
		switch state.SQLState() {
		case "23505":
			domain = corestore.ErrUniqueViolation
		case "40001", "40P01":
			domain = corestore.ErrSerializationFailure
		case "57014":
			domain = ctx.Err()
			if domain == nil {
				domain = context.Canceled
			}
		}
		return &storageError{cause: err, domain: domain, state: state.SQLState()}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return &storageError{cause: err, domain: corestore.ErrNotFound}
	}
	return err
}

type storageError struct {
	cause  error
	domain error
	state  string
}

func (e *storageError) Error() string {
	if e.domain != nil {
		return e.domain.Error()
	}
	return "PostgreSQL operation failed with SQLSTATE " + e.state
}

func RetryableConflict(err error) bool {
	var stored *storageError
	return errors.As(err, &stored) && (stored.state == "40001" || stored.state == "40P01")
}

func (e *storageError) Unwrap() error {
	return e.cause
}

func (e *storageError) Is(target error) bool {
	return errors.Is(e.domain, target)
}
