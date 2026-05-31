package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/symphony-go/internal/store/sqlc"
)

type Backend string

const BackendSQLite Backend = "sqlite"

const defaultBusyTimeout = 5 * time.Second

var ErrNotFound = errors.New("store record not found")

type Config struct {
	Backend     Backend
	Path        string
	BusyTimeout time.Duration
}

type Store interface {
	StatsStore
	Queries() *sqlc.Queries
	Close() error
}

type StatsStore interface {
	StartRun(context.Context, RunStart) (int64, error)
	UpdateRun(context.Context, int64, RunUpdate) error
	StopRun(context.Context, int64, RunStop) error
	StartSession(context.Context, SessionStart) (int64, error)
	FinishSession(context.Context, int64, SessionFinish) error
	DailyTokenSpend(context.Context, time.Time) (TokenSpend, error)
}

type RunStart struct {
	StartedAt            time.Time
	PeakConcurrentAgents int64
	SessionsLaunched     int64
	InputTokens          int64
	OutputTokens         int64
	TotalTokens          int64
	RuntimeSeconds       int64
}

type RunUpdate struct {
	PeakConcurrentAgents int64
	SessionsLaunched     int64
	InputTokens          int64
	OutputTokens         int64
	TotalTokens          int64
	RuntimeSeconds       int64
}

type RunStop struct {
	StoppedAt            time.Time
	RestartReason        string
	PeakConcurrentAgents int64
	SessionsLaunched     int64
	InputTokens          int64
	OutputTokens         int64
	TotalTokens          int64
	RuntimeSeconds       int64
}

type SessionStart struct {
	RunID      int64
	IssueID    string
	Identifier string
	IssueURL   string
	StartedAt  time.Time
	Model      string
}

type SessionFinish struct {
	CompletedAt    time.Time
	Turns          int64
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	RuntimeSeconds int64
	FinalState     string
	Model          string
}

type TokenSpend struct {
	Date         string
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	Sessions     int64
	ByModel      []ModelTokenSpend
}

type ModelTokenSpend struct {
	Model        string
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	Sessions     int64
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
