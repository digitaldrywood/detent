package store

import (
	"context"
	"time"
)

type RuntimeHistoryMaintainer interface {
	PruneRuntimeHistory(context.Context, time.Time) error
}
