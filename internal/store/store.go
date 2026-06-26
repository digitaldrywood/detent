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
	BudgetCostStore
	WorkflowMetricsStore
	WorkAttemptStore
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
	CycleTimeReport(context.Context) (CycleTimeReport, error)
	LifetimeTotals(context.Context) (LifetimeTotals, error)
	DailyTokenSpend(context.Context, time.Time) (TokenSpend, error)
	IssueTokenSpend(context.Context, IssueIdentity) (TokenSpend, error)
}

type FairShareStore interface {
	ListFairShareUsage(context.Context) ([]FairShareUsage, error)
	RecordFairShareDispatch(context.Context, FairShareDispatch) error
}

type BudgetCostStore interface {
	BudgetCostEvents(context.Context, BudgetCostQuery) ([]BudgetCostEvent, error)
}

type WorkflowMetricsStore interface {
	RecordWorkflowPhaseEvent(context.Context, WorkflowPhaseEvent) (int64, error)
	WorkflowMetricsReport(context.Context, WorkflowMetricsQuery) (WorkflowMetricsReport, error)
	IssueWorkflowTimeline(context.Context, IssueIdentity) (WorkflowTimeline, error)
}

type WorkAttemptStore interface {
	StartWorkAttempt(context.Context, WorkAttemptStart) (int64, error)
	RecordWorkAttemptHeartbeat(context.Context, WorkAttemptHeartbeat) error
	CompleteWorkAttempt(context.Context, WorkAttemptCompletion) error
	ListActiveWorkAttempts(context.Context, WorkAttemptQuery) ([]WorkAttempt, error)
	TimeoutExpiredWorkAttempts(context.Context, WorkAttemptTimeout) ([]WorkAttempt, error)
	ReclaimActiveWorkAttempts(context.Context, WorkAttemptReclaim) ([]WorkAttempt, error)
	RecordSchedulerDecision(context.Context, SchedulerDecision) (int64, error)
	ListRecentSchedulerDecisions(context.Context, SchedulerDecisionQuery) ([]SchedulerDecision, error)
}

type WorkflowPhaseType string

const (
	WorkflowPhaseTypeLane          WorkflowPhaseType = "lane"
	WorkflowPhaseTypeAgentSession  WorkflowPhaseType = "agent_session"
	WorkflowPhaseTypeLocalCheck    WorkflowPhaseType = "local_check"
	WorkflowPhaseTypeCI            WorkflowPhaseType = "ci"
	WorkflowPhaseTypeGitHubBackoff WorkflowPhaseType = "github_backoff"
	WorkflowPhaseTypeReview        WorkflowPhaseType = "review"
	WorkflowPhaseTypeMergeQueue    WorkflowPhaseType = "merge_queue"
)

type WorkAttemptStatus string

const (
	WorkAttemptStatusActive   WorkAttemptStatus = "active"
	WorkAttemptStatusTerminal WorkAttemptStatus = "terminal"
)

type WorkAttemptTerminalState string

const (
	WorkAttemptTerminalSuccess    WorkAttemptTerminalState = "success"
	WorkAttemptTerminalFailure    WorkAttemptTerminalState = "failure"
	WorkAttemptTerminalCancelled  WorkAttemptTerminalState = "cancelled"
	WorkAttemptTerminalTimedOut   WorkAttemptTerminalState = "timed_out"
	WorkAttemptTerminalSuperseded WorkAttemptTerminalState = "superseded"
	WorkAttemptTerminalAbandoned  WorkAttemptTerminalState = "abandoned"
)

type SchedulerDecisionResult string

const (
	SchedulerDecisionResultSelected SchedulerDecisionResult = "selected"
	SchedulerDecisionResultSkipped  SchedulerDecisionResult = "skipped"
)

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

type WorkflowPhaseEvent struct {
	ID                int64
	ProjectID         string
	RunID             int64
	SessionID         int64
	IssueID           string
	Identifier        string
	IssueURL          string
	PRNumber          *int64
	PhaseType         WorkflowPhaseType
	PhaseName         string
	PreviousPhaseName string
	Reason            string
	Status            string
	StartedAt         time.Time
	FinishedAt        time.Time
	DurationSeconds   int64
	CommandName       string
	ExitCode          *int64
	Turns             int64
	InputTokens       int64
	OutputTokens      int64
	TotalTokens       int64
	EndpointFamily    string
	MetadataJSON      string
}

type WorkAttempt struct {
	ID                     int64
	ProjectID              string
	IssueID                string
	Identifier             string
	IssueURL               string
	PRNumber               *int64
	Repo                   string
	WorkerType             string
	WorkerHost             string
	Lane                   string
	AttemptNumber          int
	Status                 WorkAttemptStatus
	StartedAt              time.Time
	LeaseExpiresAt         time.Time
	HeartbeatAt            time.Time
	CompletedAt            time.Time
	TerminalState          WorkAttemptTerminalState
	ErrorClass             string
	ErrorMessage           string
	Phase                  string
	StatusMessage          string
	CurrentStep            *int64
	TotalSteps             *int64
	ProgressPercent        *int64
	CurrentCommand         string
	WaitReason             string
	GitHubRateSnapshotJSON string
	CIState                string
	CapacitySnapshotJSON   string
	WorkerMetadataJSON     string
	MetricsJSON            string
	NextAction             string
}

type WorkAttemptStart struct {
	ProjectID              string
	IssueID                string
	Identifier             string
	IssueURL               string
	PRNumber               *int64
	Repo                   string
	WorkerType             string
	WorkerHost             string
	Lane                   string
	AttemptNumber          int
	StartedAt              time.Time
	LeaseExpiresAt         time.Time
	Phase                  string
	StatusMessage          string
	CurrentStep            *int64
	TotalSteps             *int64
	ProgressPercent        *int64
	CurrentCommand         string
	WaitReason             string
	GitHubRateSnapshotJSON string
	CIState                string
	CapacitySnapshotJSON   string
	WorkerMetadataJSON     string
	MetricsJSON            string
	NextAction             string
}

type WorkAttemptHeartbeat struct {
	AttemptID              int64
	HeartbeatAt            time.Time
	LeaseExpiresAt         time.Time
	Phase                  string
	StatusMessage          string
	CurrentStep            *int64
	TotalSteps             *int64
	ProgressPercent        *int64
	CurrentCommand         string
	WaitReason             string
	GitHubRateSnapshotJSON string
	CIState                string
	CapacitySnapshotJSON   string
	MetricsJSON            string
	NextAction             string
	ErrorClass             string
	ErrorMessage           string
}

type WorkAttemptCompletion struct {
	AttemptID              int64
	CompletedAt            time.Time
	Status                 WorkAttemptStatus
	TerminalState          WorkAttemptTerminalState
	ErrorClass             string
	ErrorMessage           string
	Phase                  string
	StatusMessage          string
	WaitReason             string
	GitHubRateSnapshotJSON string
	CIState                string
	CapacitySnapshotJSON   string
	MetricsJSON            string
	NextAction             string
}

type WorkAttemptQuery struct {
	ProjectID string
}

type WorkAttemptTimeout struct {
	ProjectID     string
	Now           time.Time
	TerminalState WorkAttemptTerminalState
	ErrorClass    string
	ErrorMessage  string
}

type WorkAttemptReclaim struct {
	ProjectID     string
	Now           time.Time
	TerminalState WorkAttemptTerminalState
	ErrorClass    string
	ErrorMessage  string
}

type SchedulerDecision struct {
	ID                     int64
	ProjectID              string
	IssueID                string
	Identifier             string
	IssueURL               string
	PRNumber               *int64
	Repo                   string
	Lane                   string
	QueuePosition          int
	Result                 SchedulerDecisionResult
	Reason                 string
	Selected               bool
	Retry                  bool
	AttemptNumber          int
	WorkerHost             string
	DecisionAt             time.Time
	WaitReason             string
	CapacitySnapshotJSON   string
	GitHubRateSnapshotJSON string
	MetadataJSON           string
}

type SchedulerDecisionQuery struct {
	ProjectID string
	Limit     int
}

type WorkflowMetricsQuery struct {
	ProjectID string
	From      time.Time
	To        time.Time
}

type WorkflowMetricsReport struct {
	Lanes     []WorkflowPhaseMetric
	SubPhases []WorkflowPhaseMetric
}

type WorkflowPhaseMetric struct {
	ProjectID      string
	PhaseType      string
	PhaseName      string
	Count          int64
	TotalSeconds   int64
	AverageSeconds int64
	P50Seconds     int64
	P90Seconds     int64
	P95Seconds     int64
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	Turns          int64
	EndpointFamily string
}

type WorkflowTimeline struct {
	Events []WorkflowPhaseEvent
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

type CycleTimeReport struct {
	Issues         []CycleTimeIssue
	Buckets        []CycleTimeBucket
	AverageSeconds int64
}

type CycleTimeIssue struct {
	Key             string
	StartedAt       time.Time
	CompletedAt     time.Time
	DurationSeconds int64
	Sessions        int64
}

type CycleTimeBucket struct {
	Label      string
	MinSeconds int64
	MaxSeconds int64
	Count      int
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

type BudgetCostQuery struct {
	ProjectIDs []string
	From       time.Time
	To         time.Time
}

type BudgetCostEvent struct {
	ProjectID string
	At        time.Time
	CostUSD   float64
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
