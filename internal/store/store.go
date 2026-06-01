package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/store/sqlc"
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
	FairShareStore
	Queries() *sqlc.Queries
	Close() error
}

type StatsStore interface {
	StartRun(context.Context, RunStart) (int64, error)
	UpdateRun(context.Context, int64, RunUpdate) error
	StopRun(context.Context, int64, RunStop) error
	StartSession(context.Context, SessionStart) (int64, error)
	FinishSession(context.Context, int64, SessionFinish) error
	RecordUsageEvent(context.Context, UsageEvent) (int64, error)
	UsageReport(context.Context, UsageReportQuery) (UsageReport, error)
	LifetimeTotals(context.Context) (LifetimeTotals, error)
	DailyTokenSpend(context.Context, time.Time) (TokenSpend, error)
	IssueTokenSpend(context.Context, IssueIdentity) (TokenSpend, error)
}

type FairShareStore interface {
	ListFairShareUsage(context.Context) ([]FairShareUsage, error)
	RecordFairShareDispatch(context.Context, FairShareDispatch) error
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

type UsageEvent struct {
	ProjectID      string
	RunID          int64
	SessionID      int64
	IssueID        string
	Identifier     string
	PRNumber       *int64
	Model          string
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	CostUSD        float64
	RuntimeSeconds int64
	StartedAt      time.Time
	FinishedAt     time.Time
	Outcome        string
}

type UsageReportGroup string

const (
	UsageReportByDay     UsageReportGroup = "day"
	UsageReportByProject UsageReportGroup = "project"
	UsageReportByIssue   UsageReportGroup = "issue"
	UsageReportByPR      UsageReportGroup = "pr"
	UsageReportByModel   UsageReportGroup = "model"
)

type UsageReportQuery struct {
	By   UsageReportGroup
	From time.Time
	To   time.Time
}

type UsageReport struct {
	By     UsageReportGroup
	From   string
	To     string
	Totals UsageReportTotals
	Rows   []UsageReportRow
}

type UsageReportTotals struct {
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	RuntimeSeconds int64
	Events         int64
	Models         []UsageReportModel
}

type UsageReportRow struct {
	Key            string
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	RuntimeSeconds int64
	Events         int64
	Models         []UsageReportModel
}

type UsageReportModel struct {
	Model          string
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	RuntimeSeconds int64
	Events         int64
}

type IssueIdentity struct {
	IssueID    string
	Identifier string
	IssueURL   string
}

type TokenSpend struct {
	Date         string
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	Sessions     int64
	ByModel      []ModelTokenSpend
}

type LifetimeTotals struct {
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	RuntimeSeconds int64
	Sessions       int64
	Runs           int64
}

type ModelTokenSpend struct {
	Model        string
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	Sessions     int64
}

type FairShareUsage struct {
	ProjectID      string
	Weight         int
	Dispatches     int64
	RuntimeSeconds int64
	UpdatedAt      time.Time
}

type FairShareDispatch struct {
	ProjectID      string
	Weight         int
	RuntimeSeconds int64
	DispatchedAt   time.Time
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
