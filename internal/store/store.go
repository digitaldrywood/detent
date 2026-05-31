package store

import (
	"context"
	"fmt"
	"time"

	"github.com/digitaldrywood/symphony-go/internal/store/sqlc"
)

type Backend string

const BackendSQLite Backend = "sqlite"

const defaultBusyTimeout = 5 * time.Second

type Config struct {
	Backend     Backend
	Path        string
	BusyTimeout time.Duration
}

type Store interface {
	Queries() *sqlc.Queries
	Close() error
}

func Open(ctx context.Context, cfg Config) (Store, error) {
	switch cfg.Backend {
	case "", BackendSQLite:
		return openSQLite(ctx, cfg)
	default:
		return nil, fmt.Errorf("unsupported store backend %q", cfg.Backend)
	}
}

func busyTimeoutMillis(timeout time.Duration) int64 {
	if timeout <= 0 {
		return defaultBusyTimeout.Milliseconds()
	}

	millis := timeout.Milliseconds()
	if millis < 1 {
		return 1
	}
	return millis
}
