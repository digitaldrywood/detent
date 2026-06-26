package telemetry

import (
	"strings"
	"time"
)

type Snapshot struct {
	GeneratedAt        time.Time           `json:"generated_at"`
	Project            Project             `json:"project"`
	Instance           Instance            `json:"instance"`
	Projects           []ProjectSnapshot   `json:"projects,omitempty"`
	DashboardURL       string              `json:"dashboard_url,omitempty"`
	Auth               AuthHealth          `json:"auth,omitzero"`
	Shutdown           Shutdown            `json:"shutdown"`
	Refresh            Refresh             `json:"refresh"`
	Events             []ActivityEvent     `json:"events,omitempty"`
	Counts             Counts              `json:"counts"`
	BoardIssues        []Issue             `json:"board_issues,omitempty"`
	Pipeline           []Issue             `json:"pipeline,omitempty"`
	Running            []Running           `json:"running"`
	WorkAttempts       []WorkAttempt       `json:"work_attempts,omitempty"`
	SchedulerDecisions []SchedulerDecision `json:"scheduler_decisions,omitempty"`
	Queue              []Queued            `json:"queue"`
	Blocked            []Blocked           `json:"blocked"`
	Completed          []Completed         `json:"completed"`
	Budget             Budget              `json:"budget"`
	RateLimits         *RateLimits         `json:"rate_limits"`
	Tokens             Tokens              `json:"tokens"`
	Throughput         TokenThroughput     `json:"throughput"`
	LifetimeTotals     LifetimeTotals      `json:"lifetime_totals"`
	CycleTime          CycleTimeReport     `json:"cycle_time"`
	WorkflowMetrics    WorkflowMetrics     `json:"workflow_metrics"`
	TokenTrend         []TokenTrendPoint   `json:"token_trend,omitempty"`
}

type Shutdown struct {
	Status            string     `json:"status,omitempty"`
	Draining          bool       `json:"draining"`
	SessionsRemaining int        `json:"sessions_remaining"`
	RequestedAt       *time.Time `json:"requested_at,omitempty"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	Result            string     `json:"result,omitempty"`
}

type Project struct {
	ID          string `json:"id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	URL         string `json:"url,omitempty"`
	Color       string `json:"color,omitempty"`
}

type Instance struct {
	Name                    string `json:"name,omitempty"`
	GitHubLogin             string `json:"github_login,omitempty"`
	AuthorizationScope      string `json:"authorization_scope,omitempty"`
	AuthorizationConfigured bool   `json:"authorization_configured"`
}

type ProjectSnapshot struct {
	Project    Project         `json:"project"`
	Counts     Counts          `json:"counts"`
	Tokens     Tokens          `json:"tokens"`
	Throughput TokenThroughput `json:"throughput"`
	Auth       AuthHealth      `json:"auth,omitzero"`
	Refresh    Refresh         `json:"refresh,omitzero"`
}

type AuthStatus string

const (
	AuthStatusStale     AuthStatus = "stale"
	AuthStatusRecovered AuthStatus = "recovered"
)

type AuthHealth struct {
	Status          AuthStatus `json:"status,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	LastErrorAt     *time.Time `json:"last_error_at,omitempty"`
	LastRecoveredAt *time.Time `json:"last_recovered_at,omitempty"`
}

func (h AuthHealth) IsZero() bool {
	return h.Status == "" &&
		strings.TrimSpace(h.LastError) == "" &&
		h.LastErrorAt == nil &&
		h.LastRecoveredAt == nil
}

type RefreshStatus string

const (
	RefreshStatusInitializing RefreshStatus = "initializing"
	RefreshStatusReady        RefreshStatus = "ready"
	RefreshStatusDegraded     RefreshStatus = "degraded"
)

type RefreshAttemptStatus string

const (
	RefreshAttemptStatusInProgress RefreshAttemptStatus = "in_progress"
	RefreshAttemptStatusCoalesced  RefreshAttemptStatus = "coalesced"
	RefreshAttemptStatusSucceeded  RefreshAttemptStatus = "succeeded"
	RefreshAttemptStatusFailed     RefreshAttemptStatus = "failed"
)

type Refresh struct {
	PollIntervalSeconds int64           `json:"poll_interval_seconds,omitempty"`
	Status              RefreshStatus   `json:"status,omitempty"`
	LastRefreshAt       *time.Time      `json:"last_refresh_at,omitempty"`
	NextRefreshAt       *time.Time      `json:"next_refresh_at,omitempty"`
	LastError           string          `json:"last_error,omitempty"`
	LastErrorAt         *time.Time      `json:"last_error_at,omitempty"`
	Manual              *RefreshAttempt `json:"manual,omitempty"`
}

type RefreshAttempt struct {
	ID          string               `json:"id,omitempty"`
	Status      RefreshAttemptStatus `json:"status,omitempty"`
	RequestedAt *time.Time           `json:"requested_at,omitempty"`
	StartedAt   *time.Time           `json:"started_at,omitempty"`
	CompletedAt *time.Time           `json:"completed_at,omitempty"`
	Operations  []string             `json:"operations,omitempty"`
	Coalesced   bool                 `json:"coalesced,omitempty"`
	LastError   string               `json:"last_error,omitempty"`
	LastErrorAt *time.Time           `json:"last_error_at,omitempty"`
}

func (a RefreshAttempt) IsZero() bool {
	return strings.TrimSpace(a.ID) == "" &&
		a.Status == "" &&
		a.RequestedAt == nil &&
		a.StartedAt == nil &&
		a.CompletedAt == nil &&
		len(a.Operations) == 0 &&
		!a.Coalesced &&
		strings.TrimSpace(a.LastError) == "" &&
		a.LastErrorAt == nil
}

func (r Refresh) ReadinessStatus() RefreshStatus {
	switch RefreshStatus(strings.TrimSpace(string(r.Status))) {
	case RefreshStatusInitializing:
		return RefreshStatusInitializing
	case RefreshStatusReady:
		return RefreshStatusReady
	case RefreshStatusDegraded:
		return RefreshStatusDegraded
	}
	if strings.TrimSpace(r.LastError) != "" || r.LastErrorAt != nil {
		return RefreshStatusDegraded
	}
	if r.LastRefreshAt != nil {
		return RefreshStatusReady
	}
	return RefreshStatusInitializing
}

func (r Refresh) Ready() bool {
	return r.ReadinessStatus() == RefreshStatusReady
}

func (r Refresh) Initializing() bool {
	return r.ReadinessStatus() == RefreshStatusInitializing
}

func (r Refresh) Degraded() bool {
	return r.ReadinessStatus() == RefreshStatusDegraded
}

type Counts struct {
	Running   int `json:"running"`
	Queue     int `json:"queue"`
	Blocked   int `json:"blocked"`
	Completed int `json:"completed"`
}

type Issue struct {
	ID                    string         `json:"issue_id"`
	Identifier            string         `json:"identifier,omitempty"`
	ProjectID             string         `json:"project_id,omitempty"`
	URL                   string         `json:"url,omitempty"`
	Title                 string         `json:"title,omitempty"`
	Description           string         `json:"description,omitempty"`
	State                 string         `json:"state,omitempty"`
	Labels                []string       `json:"labels,omitempty"`
	Assignees             []string       `json:"assignees,omitempty"`
	Comments              []IssueComment `json:"comments,omitempty"`
	BlockedBy             []BlockedRef   `json:"blocked_by,omitempty"`
	PullRequest           *PullRequest   `json:"pull_request,omitempty"`
	MergeTiming           *MergeTiming   `json:"merge_timing,omitempty"`
	Owner                 string         `json:"owner,omitempty"`
	LeaseRenewedAt        *time.Time     `json:"lease_renewed_at,omitempty"`
	LeaseExpiresAt        *time.Time     `json:"lease_expires_at,omitempty"`
	LeaseStale            bool           `json:"lease_stale,omitempty"`
	CreatedAt             *time.Time     `json:"created_at,omitempty"`
	UpdatedAt             *time.Time     `json:"updated_at,omitempty"`
	StageUpdatedAt        *time.Time     `json:"stage_updated_at,omitempty"`
	CurrentLaneEnteredAt  *time.Time     `json:"current_lane_entered_at,omitempty"`
	CurrentLaneAgeSeconds int64          `json:"current_lane_age_seconds,omitempty"`
}

type IssueComment struct {
	Body string `json:"body,omitempty"`
	URL  string `json:"url,omitempty"`
}

type BlockedRef struct {
	ID         string `json:"id,omitempty"`
	Identifier string `json:"identifier"`
	State      string `json:"state,omitempty"`
}

type PullRequest struct {
	Number                     int                `json:"number,omitempty"`
	URL                        string             `json:"url,omitempty"`
	BranchName                 string             `json:"branch_name,omitempty"`
	State                      string             `json:"state,omitempty"`
	MergeableState             string             `json:"mergeable_state,omitempty"`
	HeadSHA                    string             `json:"head_sha,omitempty"`
	BaseSHA                    string             `json:"base_sha,omitempty"`
	HydrationUnavailableReason string             `json:"hydration_unavailable_reason,omitempty"`
	HydrationDegradedReason    string             `json:"hydration_degraded_reason,omitempty"`
	HydrationNextRetryAt       *time.Time         `json:"hydration_next_retry_at,omitempty"`
	CIStatus                   string             `json:"ci_status,omitempty"`
	CheckRunCount              int                `json:"check_run_count,omitempty"`
	StatusContextCount         int                `json:"status_context_count,omitempty"`
	CIQueueSeconds             int64              `json:"ci_queue_seconds,omitempty"`
	CIDurationSeconds          int64              `json:"ci_duration_seconds,omitempty"`
	QuietWaitSeconds           int64              `json:"quiet_wait_seconds,omitempty"`
	SlowChecks                 []PullRequestCheck `json:"slow_checks,omitempty"`
	RunningChecks              []string           `json:"running_checks,omitempty"`
	CodexReviewState           string             `json:"codex_review_state,omitempty"`
}

type PullRequestCheck struct {
	Name            string `json:"name,omitempty"`
	Status          string `json:"status,omitempty"`
	Conclusion      string `json:"conclusion,omitempty"`
	QueueSeconds    int64  `json:"queue_seconds,omitempty"`
	DurationSeconds int64  `json:"duration_seconds,omitempty"`
}

type MergeTiming struct {
	EnteredMergingAt           *time.Time `json:"entered_merging_at,omitempty"`
	MergeWorkerSlotAcquiredAt  *time.Time `json:"merge_worker_slot_acquired_at,omitempty"`
	MergeStartedAt             *time.Time `json:"merge_started_at,omitempty"`
	BaseRefreshStartedAt       *time.Time `json:"base_refresh_started_at,omitempty"`
	BaseRefreshFinishedAt      *time.Time `json:"base_refresh_finished_at,omitempty"`
	CIWaitStartedAt            *time.Time `json:"ci_wait_started_at,omitempty"`
	CIWaitFinishedAt           *time.Time `json:"ci_wait_finished_at,omitempty"`
	MergedAt                   *time.Time `json:"merged_at,omitempty"`
	MergeFailedAt              *time.Time `json:"merge_failed_at,omitempty"`
	MergeFailureReason         string     `json:"merge_failure_reason,omitempty"`
	QueueWaitSeconds           int64      `json:"queue_wait_seconds,omitempty"`
	ActiveMergeDurationSeconds int64      `json:"active_merge_duration_seconds,omitempty"`
	TotalMergingSeconds        int64      `json:"total_merging_seconds,omitempty"`
	Repository                 string     `json:"repository,omitempty"`
	PullRequestNumber          int        `json:"pull_request_number,omitempty"`
	IssueNumber                int        `json:"issue_number,omitempty"`
	HeadSHA                    string     `json:"head_sha,omitempty"`
	BaseSHA                    string     `json:"base_sha,omitempty"`
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

type WorkAttempt struct {
	AttemptID              int64      `json:"attempt_id"`
	ProjectID              string     `json:"project_id,omitempty"`
	IssueID                string     `json:"issue_id,omitempty"`
	Identifier             string     `json:"identifier,omitempty"`
	IssueURL               string     `json:"issue_url,omitempty"`
	PRNumber               *int64     `json:"pr_number,omitempty"`
	Repo                   string     `json:"repo,omitempty"`
	WorkerType             string     `json:"worker_type,omitempty"`
	WorkerHost             string     `json:"worker_host,omitempty"`
	Lane                   string     `json:"lane,omitempty"`
	AttemptNumber          int        `json:"attempt_number,omitempty"`
	Status                 string     `json:"status,omitempty"`
	StartedAt              time.Time  `json:"started_at,omitzero"`
	LeaseExpiresAt         *time.Time `json:"lease_expires_at,omitempty"`
	HeartbeatAt            *time.Time `json:"heartbeat_at,omitempty"`
	CompletedAt            *time.Time `json:"completed_at,omitempty"`
	TerminalState          string     `json:"terminal_state,omitempty"`
	ErrorClass             string     `json:"error_class,omitempty"`
	ErrorMessage           string     `json:"error_message,omitempty"`
	Phase                  string     `json:"phase,omitempty"`
	StatusMessage          string     `json:"status_message,omitempty"`
	CurrentCommand         string     `json:"current_command,omitempty"`
	WaitReason             string     `json:"wait_reason,omitempty"`
	GitHubRateSnapshotJSON string     `json:"github_rate_snapshot_json,omitempty"`
	CIState                string     `json:"ci_state,omitempty"`
	CapacitySnapshotJSON   string     `json:"capacity_snapshot_json,omitempty"`
	MetricsJSON            string     `json:"metrics_json,omitempty"`
	NextAction             string     `json:"next_action,omitempty"`
	Stale                  bool       `json:"stale,omitempty"`
}

type SchedulerDecision struct {
	ID                     int64     `json:"id,omitempty"`
	ProjectID              string    `json:"project_id,omitempty"`
	IssueID                string    `json:"issue_id,omitempty"`
	Identifier             string    `json:"identifier,omitempty"`
	IssueURL               string    `json:"issue_url,omitempty"`
	PRNumber               *int64    `json:"pr_number,omitempty"`
	Repo                   string    `json:"repo,omitempty"`
	Lane                   string    `json:"lane,omitempty"`
	QueuePosition          int       `json:"queue_position,omitempty"`
	Result                 string    `json:"result,omitempty"`
	Reason                 string    `json:"reason,omitempty"`
	Selected               bool      `json:"selected,omitempty"`
	Retry                  bool      `json:"retry,omitempty"`
	AttemptNumber          int       `json:"attempt_number,omitempty"`
	WorkerHost             string    `json:"worker_host,omitempty"`
	DecisionAt             time.Time `json:"decision_at,omitzero"`
	WaitReason             string    `json:"wait_reason,omitempty"`
	CapacitySnapshotJSON   string    `json:"capacity_snapshot_json,omitempty"`
	GitHubRateSnapshotJSON string    `json:"github_rate_snapshot_json,omitempty"`
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
	WorkerHost     string     `json:"worker_host,omitempty"`
	WorkspacePath  string     `json:"workspace_path,omitempty"`
	SessionID      string     `json:"session_id,omitempty"`
	Error          string     `json:"error,omitempty"`
	RecoveryReason string     `json:"recovery_reason,omitempty"`
	RecoveryTarget string     `json:"recovery_target,omitempty"`
	BlockedAt      *time.Time `json:"blocked_at,omitempty"`
	LastEventAt    *time.Time `json:"last_event_at,omitempty"`
	LastEvent      string     `json:"last_event,omitempty"`
	LastMessage    string     `json:"last_message,omitempty"`
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
	Enabled           bool               `json:"enabled"`
	DegradedReason    string             `json:"degraded_reason,omitempty"`
	PerDayMaxUSD      *float64           `json:"per_day_max_usd"`
	PerIssueMaxUSD    *float64           `json:"per_issue_max_usd"`
	CurrentSpendUSD   float64            `json:"current_spend_usd"`
	ProjectedCostUSD  float64            `json:"projected_cost_usd"`
	ProjectedSpendUSD float64            `json:"projected_spend_usd,omitempty"`
	PeriodStart       time.Time          `json:"period_start,omitzero"`
	PeriodEnd         time.Time          `json:"period_end,omitzero"`
	SpendPoints       []BudgetSpendPoint `json:"spend_points,omitempty"`
	Days              []BudgetDay        `json:"days,omitempty"`
	Refusals          []BudgetRefusal    `json:"refusals,omitempty"`
}

type BudgetSpendPoint struct {
	At       time.Time `json:"at"`
	SpendUSD float64   `json:"spend_usd"`
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
	LimitID       string           `json:"limit_id,omitempty"`
	LimitName     string           `json:"limit_name,omitempty"`
	Primary       *RateLimitBucket `json:"primary,omitempty"`
	Secondary     *RateLimitBucket `json:"secondary,omitempty"`
	Credits       *RateLimitBucket `json:"credits,omitempty"`
	GitHubGraphQL *RateLimitBucket `json:"github_graphql,omitempty"`
	GitHubREST    *RateLimitBucket `json:"github_rest,omitempty"`
	GraphQLCost   *GraphQLCost     `json:"graphql_cost,omitempty"`
	RESTUsage     *RESTUsage       `json:"rest_usage,omitempty"`
}

type RateLimitBucket struct {
	Remaining      int64      `json:"remaining,omitempty"`
	Used           int64      `json:"used,omitempty"`
	Limit          int64      `json:"limit,omitempty"`
	Cost           int64      `json:"cost,omitempty"`
	ResetAt        *time.Time `json:"reset_at,omitempty"`
	ResetInSeconds int64      `json:"reset_in_seconds,omitempty"`
	HasCredits     bool       `json:"has_credits,omitempty"`
	Unlimited      bool       `json:"unlimited,omitempty"`
	Balance        string     `json:"balance,omitempty"`
}

type GraphQLCost struct {
	TotalQueries int64                    `json:"total_queries,omitempty"`
	TotalCost    int64                    `json:"total_cost,omitempty"`
	Contributors []GraphQLCostContributor `json:"contributors,omitempty"`
}

type GraphQLCostContributor struct {
	QueryType string `json:"query_type"`
	Count     int64  `json:"count"`
	Cost      int64  `json:"cost"`
}

type RESTUsage struct {
	TotalRequests int64                  `json:"total_requests,omitempty"`
	RateLimited   bool                   `json:"rate_limited,omitempty"`
	BackoffUntil  *time.Time             `json:"backoff_until,omitempty"`
	Contributors  []RESTUsageContributor `json:"contributors,omitempty"`
}

type RESTUsageContributor struct {
	EndpointFamily string     `json:"endpoint_family"`
	Count          int64      `json:"count"`
	Remaining      int64      `json:"remaining,omitempty"`
	Limit          int64      `json:"limit,omitempty"`
	Resource       string     `json:"resource,omitempty"`
	ResetAt        *time.Time `json:"reset_at,omitempty"`
	RetryAfterMS   int64      `json:"retry_after_ms,omitempty"`
	RateLimited    bool       `json:"rate_limited,omitempty"`
	LastStatus     int        `json:"last_status,omitempty"`
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

type CycleTimeReport struct {
	Available      bool              `json:"available"`
	DegradedReason string            `json:"degraded_reason,omitempty"`
	Issues         []CycleTimeIssue  `json:"issues,omitempty"`
	Buckets        []CycleTimeBucket `json:"buckets,omitempty"`
	AverageSeconds int64             `json:"average_seconds"`
}

type CycleTimeIssue struct {
	Key             string    `json:"key"`
	StartedAt       time.Time `json:"started_at"`
	CompletedAt     time.Time `json:"completed_at"`
	DurationSeconds int64     `json:"duration_seconds"`
	Sessions        int64     `json:"sessions"`
}

type CycleTimeBucket struct {
	Label      string `json:"label"`
	MinSeconds int64  `json:"min_seconds"`
	MaxSeconds int64  `json:"max_seconds,omitempty"`
	Count      int    `json:"count"`
}

type WorkflowMetrics struct {
	Available        bool                    `json:"available"`
	DegradedReason   string                  `json:"degraded_reason,omitempty"`
	Windows          []WorkflowMetricsWindow `json:"windows,omitempty"`
	OldestCards      []WorkflowLaneAge       `json:"oldest_cards,omitempty"`
	ActiveBottleneck WorkflowBottleneck      `json:"active_bottleneck,omitzero"`
}

type WorkflowMetricsWindow struct {
	Label     string                `json:"label"`
	From      time.Time             `json:"from"`
	To        time.Time             `json:"to"`
	Lanes     []WorkflowPhaseMetric `json:"lanes,omitempty"`
	SubPhases []WorkflowPhaseMetric `json:"sub_phases,omitempty"`
}

type WorkflowPhaseMetric struct {
	ProjectID      string                    `json:"project_id,omitempty"`
	PhaseType      string                    `json:"phase_type"`
	PhaseName      string                    `json:"phase_name"`
	Count          int64                     `json:"count"`
	TotalSeconds   int64                     `json:"total_seconds"`
	AverageSeconds int64                     `json:"average_seconds"`
	P50Seconds     int64                     `json:"p50_seconds"`
	P90Seconds     int64                     `json:"p90_seconds"`
	P95Seconds     int64                     `json:"p95_seconds"`
	InputTokens    int64                     `json:"input_tokens,omitempty"`
	OutputTokens   int64                     `json:"output_tokens,omitempty"`
	TotalTokens    int64                     `json:"total_tokens,omitempty"`
	Turns          int64                     `json:"turns,omitempty"`
	EndpointFamily string                    `json:"endpoint_family,omitempty"`
	Bottleneck     bool                      `json:"bottleneck,omitempty"`
	Comparison     *WorkflowMetricComparison `json:"comparison,omitempty"`
}

type WorkflowMetricComparison struct {
	Label                  string    `json:"label"`
	PreviousFrom           time.Time `json:"previous_from"`
	PreviousTo             time.Time `json:"previous_to"`
	PreviousCount          int64     `json:"previous_count"`
	PreviousAverageSeconds int64     `json:"previous_average_seconds,omitempty"`
	DeltaSeconds           int64     `json:"delta_seconds"`
	DeltaPercent           float64   `json:"delta_percent,omitempty"`
	Direction              string    `json:"direction"`
}

type WorkflowLaneAge struct {
	ProjectID     string     `json:"project_id,omitempty"`
	IssueID       string     `json:"issue_id,omitempty"`
	Identifier    string     `json:"identifier,omitempty"`
	URL           string     `json:"url,omitempty"`
	Title         string     `json:"title,omitempty"`
	State         string     `json:"state,omitempty"`
	EnteredAt     *time.Time `json:"entered_at,omitempty"`
	AgeSeconds    int64      `json:"age_seconds"`
	BottleneckKey string     `json:"bottleneck_key,omitempty"`
}

type WorkflowBottleneck struct {
	Kind       string     `json:"kind,omitempty"`
	Label      string     `json:"label,omitempty"`
	Detail     string     `json:"detail,omitempty"`
	ProjectID  string     `json:"project_id,omitempty"`
	IssueID    string     `json:"issue_id,omitempty"`
	Identifier string     `json:"identifier,omitempty"`
	Seconds    int64      `json:"seconds,omitempty"`
	Count      int        `json:"count,omitempty"`
	Until      *time.Time `json:"until,omitempty"`
}

func (b WorkflowBottleneck) IsZero() bool {
	return b.Kind == "" &&
		b.Label == "" &&
		b.Detail == "" &&
		b.ProjectID == "" &&
		b.IssueID == "" &&
		b.Identifier == "" &&
		b.Seconds == 0 &&
		b.Count == 0 &&
		b.Until == nil
}

type TokenTrendPoint struct {
	At     time.Time `json:"at"`
	Input  int64     `json:"input_tokens"`
	Output int64     `json:"output_tokens"`
	Total  int64     `json:"total_tokens"`
}
