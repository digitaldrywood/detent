package store

import (
	"context"
)

type TypedProjectionMaintainer interface {
	BackfillTypedProjections(context.Context) (int, error)
}
