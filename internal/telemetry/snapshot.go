package telemetry

import "time"

type Snapshot struct {
	GeneratedAt    time.Time         `json:"generated_at"`
	Project        Project           `json:"project"`
	DashboardURL   string            `json:"dashboard_url,omitempty"`
	Refresh        Refresh           `json:"refresh"`
	Counts         Counts            `json:"counts"`
	Running        []Running         `json:"running"`
	Queue          []Queued          `json:"queue"`
	Blocked        []Blocked         `json:"blocked"`
	Completed      []Completed       `json:"completed"`
	Budget         Budget            `json:"budget"`
	RateLimits     *RateLimits       `json:"rate_limits"`
	Tokens         Tokens            `json:"tokens"`
	Throughput     TokenThroughput   `json:"throughput"`
	LifetimeTotals LifetimeTotals    `json:"lifetime_totals"`
	TokenTrend     []TokenTrendPoint `json:"token_trend,omitempty"`
}

type Project struct {
	DisplayName string `json:"display_name,omitempty"`
	URL         string `json:"url,omitempty"`
}

type Refresh struct {
	PollIntervalSeconds int64      `json:"poll_interval_seconds,omitempty"`
	LastRefreshAt       *time.Time `json:"last_refresh_at,omitempty"`
	NextRefreshAt       *time.Time `json:"next_refresh_at,omitempty"`
}

type Counts struct {
	Running   int `json:"running"`
	Queue     int `json:"queue"`
	Blocked   int `json:"blocked"`
	Completed int `json:"completed"`
}

type Issue struct {
	ID          string `json:"issue_id"`
	Identifier  string `json:"identifier,omitempty"`
	URL         string `json:"url,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	State       string `json:"state,omitempty"`
}

type ActivityEvent struct {
	At      time.Time `json:"at"`
	Event   string    `json:"event,omitempty"`
	Message string    `json:"message,omitempty"`
}

type Running struct {
	Issue
	WorkerHost      string          `json:"worker_host,omitempty"`
	ProcessIdentity string          `json:"process_identity,omitempty"`
	WorkspacePath   string          `json:"workspace_path,omitempty"`
	SessionID       string          `json:"session_id,omitempty"`
	TurnCount       int             `json:"turn_count"`
	StartedAt       time.Time       `json:"started_at"`
	LastEventAt     *time.Time      `json:"last_event_at,omitempty"`
	LastEvent       string          `json:"last_event,omitempty"`
	LastMessage     string          `json:"last_message,omitempty"`
	RecentEvents    []ActivityEvent `json:"recent_events,omitempty"`
	RuntimeSeconds  float64         `json:"runtime_seconds"`
	DiffAdded       int             `json:"diff_added"`
	DiffRemoved     int             `json:"diff_removed"`
	DiffFiles       int             `json:"diff_files"`
	DiffStatus      string          `json:"diff_status,omitempty"`
	Tokens          Tokens          `json:"tokens"`
}

type Queued struct {
	Issue
	Attempt        int        `json:"attempt"`
	DueAt          *time.Time `json:"due_at,omitempty"`
	DueInMillis    int64      `json:"due_in_ms,omitempty"`
	Error          string     `json:"error,omitempty"`
	WorkerHost     string     `json:"worker_host,omitempty"`
	WorkspacePath  string     `json:"workspace_path,omitempty"`
	ProjectedSpend float64    `json:"projected_spend_usd,omitempty"`
}

type Blocked struct {
	Issue
	WorkerHost    string     `json:"worker_host,omitempty"`
	WorkspacePath string     `json:"workspace_path,omitempty"`
	SessionID     string     `json:"session_id,omitempty"`
	Error         string     `json:"error,omitempty"`
	BlockedAt     *time.Time `json:"blocked_at,omitempty"`
	LastEventAt   *time.Time `json:"last_event_at,omitempty"`
	LastEvent     string     `json:"last_event,omitempty"`
	LastMessage   string     `json:"last_message,omitempty"`
}

type Completed struct {
	Issue
	SessionID      string    `json:"session_id,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	CompletedAt    time.Time `json:"completed_at"`
	Turns          int       `json:"turns"`
	RuntimeSeconds float64   `json:"runtime_seconds"`
	FinalState     string    `json:"final_state,omitempty"`
	Model          string    `json:"model,omitempty"`
	Tokens         Tokens    `json:"tokens"`
}

type Budget struct {
	Enabled          bool            `json:"enabled"`
	PerDayMaxUSD     *float64        `json:"per_day_max_usd"`
	PerIssueMaxUSD   *float64        `json:"per_issue_max_usd"`
	CurrentSpendUSD  float64         `json:"current_spend_usd"`
	ProjectedCostUSD float64         `json:"projected_cost_usd"`
	Days             []BudgetDay     `json:"days,omitempty"`
	Refusals         []BudgetRefusal `json:"refusals,omitempty"`
}

type BudgetDay struct {
	Date     string  `json:"date"`
	SpendUSD float64 `json:"spend_usd"`
}

type BudgetRefusal struct {
	IssueID          string     `json:"issue_id"`
	Identifier       string     `json:"identifier,omitempty"`
	Code             string     `json:"code"`
	Message          string     `json:"message"`
	CurrentSpendUSD  float64    `json:"current_spend_usd"`
	ProjectedCostUSD float64    `json:"projected_cost_usd"`
	MaxUSD           *float64   `json:"max_usd"`
	ResetAt          *time.Time `json:"reset_at,omitempty"`
	RefusedAt        time.Time  `json:"refused_at"`
}

type RateLimits struct {
	LimitID   string           `json:"limit_id,omitempty"`
	LimitName string           `json:"limit_name,omitempty"`
	Primary   *RateLimitBucket `json:"primary,omitempty"`
	Secondary *RateLimitBucket `json:"secondary,omitempty"`
	Credits   *RateLimitBucket `json:"credits,omitempty"`
}

type RateLimitBucket struct {
	Remaining      int64      `json:"remaining,omitempty"`
	Used           int64      `json:"used,omitempty"`
	Limit          int64      `json:"limit,omitempty"`
	ResetAt        *time.Time `json:"reset_at,omitempty"`
	ResetInSeconds int64      `json:"reset_in_seconds,omitempty"`
	HasCredits     bool       `json:"has_credits,omitempty"`
	Unlimited      bool       `json:"unlimited,omitempty"`
	Balance        string     `json:"balance,omitempty"`
}

type Tokens struct {
	Input          int64   `json:"input_tokens"`
	Output         int64   `json:"output_tokens"`
	Total          int64   `json:"total_tokens"`
	RuntimeSeconds float64 `json:"seconds_running,omitempty"`
}

type TokenThroughput struct {
	TokensPerSecond float64 `json:"tokens_per_second"`
	WindowSeconds   int64   `json:"window_seconds"`
	Tokens          int64   `json:"tokens"`
}

type LifetimeTotals struct {
	Available      bool   `json:"available"`
	DegradedReason string `json:"degraded_reason,omitempty"`
	InputTokens    int64  `json:"input_tokens"`
	OutputTokens   int64  `json:"output_tokens"`
	TotalTokens    int64  `json:"total_tokens"`
	RuntimeSeconds int64  `json:"runtime_seconds"`
	Sessions       int64  `json:"sessions"`
	Runs           int64  `json:"runs"`
}

type TokenTrendPoint struct {
	At     time.Time `json:"at"`
	Input  int64     `json:"input_tokens"`
	Output int64     `json:"output_tokens"`
	Total  int64     `json:"total_tokens"`
}
