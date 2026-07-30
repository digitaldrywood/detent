package cli

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/budget"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

const (
	doctorWorkflowOptimizationCheckName = "Workflow optimization"

	doctorWorkflowRuleRunawaySessionTokens           = "runaway_session_tokens"
	doctorWorkflowRuleReworkLaps                     = "rework_laps"
	doctorWorkflowRuleValidatorModel                 = "validator_model"
	doctorWorkflowRuleEmptyModelTelemetry            = "empty_model_telemetry"
	doctorWorkflowRulePinnedRouteModelRejected       = "pinned_route_model_rejected"
	doctorWorkflowRulePinnedWorkerModelStale         = "pinned_worker_model_stale"
	doctorWorkflowRuleSessionMultiplierKills         = "session_multiplier_ceiling_kills"
	doctorWorkflowRuleSpendProgressBelowMedian       = "spend_progress_below_median_session_cost"
	doctorWorkflowRuleNoSessionTokenBrake            = "no_session_token_brake"
	doctorWorkflowRuleBudgetEstimateDrift            = "budget_estimate_drift"
	doctorWorkflowRuleSchedulerSkipRate              = "scheduler_skip_rate"
	doctorWorkflowRuleInvalidWorkpadStatusRecurrence = "invalid_workpad_status_recurrence"
	doctorWorkflowRuleOrphanRecoveryFallback         = "orphan_recovery_fallback"
	doctorWorkflowRuleOrphansNeverRecovered          = "orphans_never_recovered"
	doctorWorkflowRuleReviewFlowBehaviorMismatch     = "review_flow_behavior_mismatch"
	doctorWorkflowRuleReviewFlowProseMismatch        = "review_flow_prose_mismatch"

	doctorWorkflowRunawayMedianMultiplier      = 4.0
	doctorWorkflowReworkLapThreshold           = 1
	doctorWorkflowRecentSessionLimit           = 50
	doctorWorkflowRecentSessionWindow          = 24 * time.Hour
	doctorWorkflowEmptyModelMinFraction        = 0.20
	doctorWorkflowBudgetEstimateInput          = int64(150_000)
	doctorWorkflowBudgetEstimateOutput         = int64(20_000)
	doctorWorkflowBudgetEstimateTotal          = doctorWorkflowBudgetEstimateInput + doctorWorkflowBudgetEstimateOutput
	doctorWorkflowBudgetEstimateBillable       = doctorWorkflowBudgetEstimateInput + doctorWorkflowBudgetEstimateOutput
	doctorWorkflowBudgetDriftRatio             = 1.50
	doctorWorkflowSchedulerMinDecisions        = int64(5)
	doctorWorkflowSchedulerHighSkipRate        = 0.50
	doctorWorkflowReviewFlowMismatchMinEntries = int64(2)
	doctorWorkflowReviewFlowRecentWindow       = 30 * 24 * time.Hour
	doctorWorkflowInvalidWorkpadStatusMinCount = int64(2)
	doctorWorkflowValidatorModel               = "gpt-5.4-mini"
	doctorWorkflowRunawayCapTolerance          = 1.25
	doctorWorkflowCapRetryCostAssumption       = "one ceiling retry re-burns the full configured cap"
)

var errDoctorTelemetryStoreUnavailable = errors.New("telemetry store unavailable")

type doctorWorkflowOptimizationReport struct {
	StorePath             string                                    `json:"store_path,omitempty"`
	Projects              []doctorWorkflowOptimizationProjectReport `json:"projects,omitempty"`
	Findings              []doctorWorkflowOptimizationFinding       `json:"findings,omitempty"`
	Proposals             []doctorWorkflowImprovementProposal       `json:"proposals,omitempty"`
	CreatedProposalIssues []doctorWorkflowCreatedProposalIssue      `json:"created_proposal_issues,omitempty"`
	Diff                  string                                    `json:"diff,omitempty"`
	Written               []string                                  `json:"written,omitempty"`
}

type doctorWorkflowOptimizationOptions struct {
	IncludeDiff       bool
	ProposalThreshold int
	ProposeIssues     bool
}

type doctorWorkflowOptimizationProjectReport struct {
	ProjectID      string                            `json:"project_id"`
	WorkflowPath   string                            `json:"workflow_path,omitempty"`
	ModelChoice    doctorWorkflowModelChoice         `json:"model_choice"`
	SessionGuard   doctorWorkflowSessionGuard        `json:"session_guard"`
	OrphanRecovery doctorOrphanRecoveryConfig        `json:"orphan_recovery"`
	Metrics        doctorWorkflowOptimizationMetrics `json:"metrics"`
	Retro          doctorWorkflowRetroStatus         `json:"retro"`
	Error          string                            `json:"error,omitempty"`
}

type doctorWorkflowRetroStatus struct {
	Enabled       bool       `json:"enabled"`
	LastRun       *time.Time `json:"last_run,omitempty"`
	Trigger       string     `json:"trigger,omitempty"`
	Findings      int64      `json:"findings"`
	FiledIssues   int64      `json:"filed_issues"`
	UpdatedIssues int64      `json:"updated_issues"`
	Error         string     `json:"error,omitempty"`
}

type doctorWorkflowModelChoice struct {
	Mode   string `json:"mode"`
	Model  string `json:"model,omitempty"`
	Source string `json:"source,omitempty"`
}

type doctorWorkflowSessionGuard struct {
	MaxSessionTokens            int64   `json:"max_session_tokens"`
	MaxSessionContextMultiplier float64 `json:"max_session_context_multiplier"`
}

type doctorOrphanRecoveryConfig struct {
	ResumeOrphanedSessions   bool `json:"resume_orphaned_sessions"`
	ExperimentalThreadResume bool `json:"experimental_thread_resume"`
}

type doctorOrphanRecoveryMetrics struct {
	Detected                int64            `json:"detected"`
	Reattached              int64            `json:"reattached"`
	FreshContinuations      int64            `json:"fresh_continuations"`
	ReattachFailures        int64            `json:"reattach_failures"`
	ResumedInputTokens      int64            `json:"resumed_input_tokens"`
	ResumedCachedTokens     int64            `json:"resumed_cached_tokens"`
	ResumedCachedInputShare float64          `json:"resumed_cached_input_share"`
	FallbackReasons         map[string]int64 `json:"fallback_reasons,omitempty"`
}

type doctorWorkflowOptimizationMetrics struct {
	SessionCount                     int64                                `json:"session_count"`
	UsageEventCount                  int64                                `json:"usage_event_count"`
	InputTokens                      int64                                `json:"input_tokens"`
	CachedInputTokens                int64                                `json:"cached_input_tokens"`
	OutputTokens                     int64                                `json:"output_tokens"`
	TotalTokens                      int64                                `json:"total_tokens"`
	InputOutputRatio                 float64                              `json:"input_output_ratio"`
	CacheReadFraction                float64                              `json:"cache_read_fraction"`
	MedianSessionTokens              int64                                `json:"median_session_tokens"`
	P90SessionTokens                 int64                                `json:"p90_session_tokens"`
	MaxSessionTokens                 int64                                `json:"max_session_tokens"`
	MaxSessionsPerIssue              int64                                `json:"max_sessions_per_issue"`
	MaxSessionsIssue                 string                               `json:"max_sessions_issue,omitempty"`
	RecentSessionCount               int64                                `json:"recent_session_count"`
	EmptyModelRecentSessions         int64                                `json:"empty_model_recent_sessions"`
	EmptyModelRecentFraction         float64                              `json:"empty_model_recent_fraction"`
	RecentModels                     []doctorWorkflowModelTelemetry       `json:"recent_models,omitempty"`
	RecentDefaultModels              []doctorWorkflowModelTelemetry       `json:"recent_default_models,omitempty"`
	SessionMultiplierKills           []doctorWorkflowSessionGuardIncident `json:"session_multiplier_kills,omitempty"`
	MaxReworkLapsPerIssue            int64                                `json:"max_rework_laps_per_issue"`
	MaxReworkLapsIssue               string                               `json:"max_rework_laps_issue,omitempty"`
	FailureTokens                    int64                                `json:"failure_tokens"`
	SchedulerDecisionCount           int64                                `json:"scheduler_decision_count"`
	SchedulerSkippedDecisions        int64                                `json:"scheduler_skipped_decisions"`
	SchedulerSkipRate                float64                              `json:"scheduler_skip_rate"`
	LaneEventCount                   int64                                `json:"lane_event_count"`
	LaneDwellP90Seconds              int64                                `json:"lane_dwell_p90_seconds"`
	SlowestLane                      string                               `json:"slowest_lane,omitempty"`
	SlowestLaneP90Seconds            int64                                `json:"slowest_lane_p90_seconds"`
	BudgetEstimateTokens             int64                                `json:"budget_estimate_tokens"`
	BudgetEstimateBillableTokens     int64                                `json:"budget_estimate_billable_tokens"`
	P90SessionBillableTokens         int64                                `json:"p90_session_billable_tokens"`
	BudgetEstimateBillableDriftRatio float64                              `json:"budget_estimate_billable_drift_ratio"`
	BudgetEstimateDriftRatio         float64                              `json:"budget_estimate_drift_ratio"`
	ReviewEntryCount                 int64                                `json:"review_entry_count"`
	ReviewEntryIssue                 string                               `json:"review_entry_issue,omitempty"`
	ReviewEntryIssueCount            int64                                `json:"review_entry_issue_count"`
	ReviewFlowBoundaryAt             string                               `json:"review_flow_boundary_at,omitempty"`
	ReviewFlowBoundaryType           string                               `json:"review_flow_boundary_type,omitempty"`
	InvalidWorkpadStatusDecisions    int64                                `json:"invalid_workpad_status_decisions"`
	InvalidWorkpadStatusIssue        string                               `json:"invalid_workpad_status_issue,omitempty"`
	InvalidWorkpadStatusIssueCount   int64                                `json:"invalid_workpad_status_issue_count"`
	OrphanRecovery                   doctorOrphanRecoveryMetrics          `json:"orphan_recovery"`
	MedianSessionCostUSDByEffort     map[string]float64                   `json:"median_session_cost_usd_by_effort,omitempty"`
}

type doctorWorkflowModelTelemetry struct {
	Model        string `json:"model"`
	SessionCount int64  `json:"session_count"`
}

type doctorWorkflowSessionGuardIncident struct {
	IssueIdentifier   string  `json:"issue_identifier"`
	AttemptCount      int64   `json:"attempt_count"`
	CeilingTokens     int64   `json:"ceiling_tokens"`
	ContextMultiplier float64 `json:"context_multiplier"`
}

type doctorWorkflowOptimizationFinding struct {
	RuleID               string                            `json:"rule_id"`
	ProjectID            string                            `json:"project_id,omitempty"`
	WorkflowPath         string                            `json:"workflow_path,omitempty"`
	Severity             string                            `json:"severity"`
	Title                string                            `json:"title"`
	Detail               string                            `json:"detail"`
	EstimatedTokenImpact int64                             `json:"estimated_token_impact"`
	Evidence             map[string]any                    `json:"evidence"`
	Patch                []doctorWorkflowOptimizationPatch `json:"patch,omitempty"`
}

type doctorWorkflowOptimizationPatch struct {
	Path  string `json:"path"`
	Value any    `json:"value"`
}

type doctorWorkflowSessionMetrics struct {
	count                    int64
	inputTokens              int64
	cachedInputTokens        int64
	outputTokens             int64
	totalTokens              int64
	totalTokensBySession     []int64
	billableTokensBySession  []int64
	issueSessionCounts       map[string]int64
	recentSessionCount       int64
	emptyModelRecentSessions int64
	recentModelCounts        map[string]int64
	recentDefaultModelCounts map[string]int64
	failureTokens            int64
	costUSDByEffort          map[string][]float64
}

type doctorWorkflowAnalyzedProject struct {
	projectID    string
	workflowPath string
	config       workflowconfig.Config
	metrics      doctorWorkflowOptimizationMetrics
}

type doctorWorkflowObservedDefaultModel struct {
	Model        string
	SessionCount int64
	Major        int
	Minor        int
}

type doctorWorkflowUsageMetrics struct {
	count             int64
	inputTokens       int64
	cachedInputTokens int64
	outputTokens      int64
	totalTokens       int64
	failureTokens     int64
}

type doctorWorkflowLaneMetrics struct {
	count        int64
	durations    []int64
	laneDuration map[string][]int64
	reworkLaps   map[string]int64
}

type doctorWorkflowSchedulerMetrics struct {
	count   int64
	skipped int64
}

type doctorWorkflowReviewFlowMetrics struct {
	reviewEntryCount               int64
	reviewEntryIssue               string
	reviewEntryIssueCount          int64
	boundaryAt                     time.Time
	boundaryType                   string
	invalidWorkpadStatusDecisions  int64
	invalidWorkpadStatusIssue      string
	invalidWorkpadStatusIssueCount int64
}

type doctorWorkflowCompletedSession struct {
	issueKey    string
	completedAt time.Time
}

type doctorWorkflowReviewEntryEvent struct {
	issueKey  string
	startedAt time.Time
}

func (r *doctorWorkflowOptimizationReport) Merge(next doctorWorkflowOptimizationReport) {
	if next.StorePath != "" {
		r.StorePath = next.StorePath
	}
	r.Projects = append(r.Projects, next.Projects...)
	r.Findings = append(r.Findings, next.Findings...)
	r.Proposals = append(r.Proposals, next.Proposals...)
	r.CreatedProposalIssues = append(r.CreatedProposalIssues, next.CreatedProposalIssues...)
	if r.Diff == "" {
		r.Diff = next.Diff
	} else if strings.TrimSpace(next.Diff) != "" {
		r.Diff = strings.TrimRight(r.Diff, "\n") + "\n" + strings.TrimLeft(next.Diff, "\n")
	}
	r.Written = append(r.Written, next.Written...)
}

func checkDoctorWorkflowOptimization(
	ctx context.Context,
	resolution globalconfig.PathResolution,
	cfg globalconfig.Config,
	deps doctorDeps,
	githubToken RuntimeSecret,
	options doctorWorkflowOptimizationOptions,
) doctorCheck {
	if strings.TrimSpace(resolution.Path) == "" {
		return doctorCheck{
			Name:   doctorWorkflowOptimizationCheckName,
			Status: doctorOK,
			Detail: "skipped because global config path is unavailable",
		}
	}
	if len(cfg.Projects) == 0 {
		return doctorCheck{
			Name:   doctorWorkflowOptimizationCheckName,
			Status: doctorOK,
			Detail: "skipped because no projects are configured",
		}
	}

	deps = deps.withDefaults()
	storePath := filepath.Join(filepath.Dir(resolution.Path), "detent.db")
	db, err := deps.openSQLiteReadOnly(ctx, storePath)
	if err != nil {
		if errors.Is(err, errDoctorTelemetryStoreUnavailable) {
			return doctorCheck{
				Name:   doctorWorkflowOptimizationCheckName,
				Status: doctorOK,
				Detail: storePath + " has no telemetry yet",
			}
		}
		return doctorCheck{
			Name:   doctorWorkflowOptimizationCheckName,
			Status: doctorWarn,
			Detail: fmt.Sprintf("%s could not be read: %v", storePath, err),
			Hint:   "Run detent after current migrations are applied, then rerun detent doctor.",
		}
	}
	report, err := doctorWorkflowOptimization(ctx, db, storePath, cfg, deps, runtimeGlobalGitHubToken(githubToken), options)
	closeErr := db.Close()
	if err != nil {
		return doctorCheck{
			Name:   doctorWorkflowOptimizationCheckName,
			Status: doctorWarn,
			Detail: fmt.Sprintf("%s telemetry could not be analyzed: %v", storePath, err),
			Hint:   "Confirm the runtime database is migrated through cached-token telemetry columns.",
		}
	}
	if closeErr != nil {
		return doctorCheck{
			Name:   doctorWorkflowOptimizationCheckName,
			Status: doctorWarn,
			Detail: fmt.Sprintf("%s telemetry could not be closed: %v", storePath, closeErr),
		}
	}

	check := doctorCheck{
		Name:                 doctorWorkflowOptimizationCheckName,
		Status:               doctorOK,
		Detail:               fmt.Sprintf("0 findings and %d proposal(s) across %d project(s)", len(report.Proposals), len(report.Projects)),
		WorkflowOptimization: report,
	}
	if len(report.Findings) == 0 && len(report.Proposals) == 0 {
		return check
	}

	check.Status = doctorWarn
	check.Detail = fmt.Sprintf("%d finding(s) and %d proposal(s) across %d project(s); estimated token impact %d", len(report.Findings), len(report.Proposals), len(report.Projects), doctorWorkflowEstimatedImpact(report.Findings))
	check.Hint = "Review detent doctor output; use --propose-issues to file governed backlog proposals, or --diff/--write for confirmed frontmatter patches."
	return check
}

func doctorWorkflowOptimization(
	ctx context.Context,
	db doctorTelemetryStore,
	storePath string,
	cfg globalconfig.Config,
	deps doctorDeps,
	runtimeGitHubToken string,
	options doctorWorkflowOptimizationOptions,
) (doctorWorkflowOptimizationReport, error) {
	options = doctorWorkflowOptimizationOptionsWithDefaults(options)
	report := doctorWorkflowOptimizationReport{StorePath: storePath}
	analyzedProjects := make([]doctorWorkflowAnalyzedProject, 0, len(cfg.Projects))
	for _, project := range cfg.Projects {
		projectID := doctorProjectID(project)
		workflowPath, workflowPathErr := doctorWorkflowOptimizationWorkflowPath(project)
		workflow, err := loadDoctorProjectWorkflow(ctx, project, deps)
		if err != nil {
			report.Projects = append(report.Projects, doctorWorkflowOptimizationProjectReport{
				ProjectID:    projectID,
				WorkflowPath: workflowPath,
				Error:        err.Error(),
			})
			continue
		}
		workflow.Config = doctorWorkflowConfigWithRuntimeGitHubToken(workflow.Config, runtimeGitHubToken)
		if err := workflow.Config.Validate(); err != nil {
			report.Projects = append(report.Projects, doctorWorkflowOptimizationProjectReport{
				ProjectID:    projectID,
				WorkflowPath: workflowPath,
				Error:        err.Error(),
			})
			continue
		}
		if workflowPathErr != nil {
			workflowPath = ""
		}

		reviewFlowBoundaryAt, reviewFlowBoundaryType := doctorWorkflowReviewFlowBoundary(project, deps.now())
		metrics, err := doctorWorkflowOptimizationMetricsForProject(ctx, db, projectID, workflow.Config, reviewFlowBoundaryAt, reviewFlowBoundaryType)
		if err != nil {
			return doctorWorkflowOptimizationReport{}, err
		}
		retroStatus, err := doctorWorkflowRetroStatusForProject(ctx, db, projectID, workflow.Config.Retro.Enabled)
		if err != nil {
			return doctorWorkflowOptimizationReport{}, err
		}
		report.Projects = append(report.Projects, doctorWorkflowOptimizationProjectReport{
			ProjectID:      projectID,
			WorkflowPath:   workflowPath,
			ModelChoice:    doctorWorkflowWorkerModelChoice(workflow.Config),
			SessionGuard:   doctorWorkflowSessionGuardConfig(workflow.Config),
			OrphanRecovery: doctorOrphanRecoveryConfig{ResumeOrphanedSessions: workflow.Config.Agent.ResumeOrphanedSessions, ExperimentalThreadResume: workflow.Config.Agent.ExperimentalThreadResume},
			Metrics:        metrics,
			Retro:          retroStatus,
		})
		analyzedProjects = append(analyzedProjects, doctorWorkflowAnalyzedProject{
			projectID:    projectID,
			workflowPath: workflowPath,
			config:       workflow.Config,
			metrics:      metrics,
		})
		findings := doctorWorkflowOptimizationFindings(projectID, workflowPath, workflow.Config, metrics)
		report.Findings = append(report.Findings, findings...)
		proposals, err := doctorWorkflowImprovementProposals(ctx, db, project, workflow.Config, findings, options.ProposalThreshold)
		if err != nil {
			return doctorWorkflowOptimizationReport{}, err
		}
		report.Proposals = append(report.Proposals, proposals...)
		if options.ProposeIssues {
			created, err := createDoctorWorkflowImprovementProposalIssues(ctx, projectID, workflow.Config, deps, proposals)
			if err != nil {
				return doctorWorkflowOptimizationReport{}, err
			}
			report.CreatedProposalIssues = append(report.CreatedProposalIssues, created...)
		}
	}
	if observed, ok := doctorWorkflowObservedDefaultModelForProjects(analyzedProjects); ok {
		for _, project := range analyzedProjects {
			finding, stale := doctorWorkflowStalePinnedModelFinding(project, observed)
			if !stale {
				continue
			}
			report.Findings = append(report.Findings, finding)
			proposals := doctorWorkflowProposalsForFindings(project.projectID, []doctorWorkflowOptimizationFinding{finding}, options.ProposalThreshold)
			report.Proposals = append(report.Proposals, proposals...)
			if options.ProposeIssues {
				created, err := createDoctorWorkflowImprovementProposalIssues(ctx, project.projectID, project.config, deps, proposals)
				if err != nil {
					return doctorWorkflowOptimizationReport{}, err
				}
				report.CreatedProposalIssues = append(report.CreatedProposalIssues, created...)
			}
		}
	}

	sort.SliceStable(report.Findings, func(i, j int) bool {
		left := report.Findings[i]
		right := report.Findings[j]
		if left.EstimatedTokenImpact != right.EstimatedTokenImpact {
			return left.EstimatedTokenImpact > right.EstimatedTokenImpact
		}
		if left.ProjectID != right.ProjectID {
			return left.ProjectID < right.ProjectID
		}
		return left.RuleID < right.RuleID
	})
	sort.SliceStable(report.Proposals, func(i, j int) bool {
		left := report.Proposals[i]
		right := report.Proposals[j]
		if left.Count != right.Count {
			return left.Count > right.Count
		}
		if left.ProjectID != right.ProjectID {
			return left.ProjectID < right.ProjectID
		}
		return left.ID < right.ID
	})
	if options.IncludeDiff {
		report.Diff = doctorWorkflowOptimizationDiff(report)
	}
	return report, nil
}

func doctorWorkflowRetroStatusForProject(ctx context.Context, db doctorTelemetryStore, projectID string, enabled bool) (doctorWorkflowRetroStatus, error) {
	status := doctorWorkflowRetroStatus{Enabled: enabled}
	var completedAt string
	err := db.QueryRowContext(ctx, `
SELECT completed_at, trigger, findings_count, filed_count, updated_count, COALESCE(error, '')
FROM retro_runs
WHERE project_id = ?
ORDER BY completed_at DESC, id DESC
LIMIT 1`, strings.TrimSpace(projectID)).Scan(
		&completedAt, &status.Trigger, &status.Findings, &status.FiledIssues, &status.UpdatedIssues, &status.Error,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return status, nil
	}
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such table: retro_runs") {
		return status, nil
	}
	if err != nil {
		return doctorWorkflowRetroStatus{}, fmt.Errorf("read retro status for project %s: %w", projectID, err)
	}
	lastRun, err := doctorWorkflowSessionTimestamp(completedAt)
	if err != nil {
		return doctorWorkflowRetroStatus{}, err
	}
	status.LastRun = &lastRun
	return status, nil
}

func doctorWorkflowReviewFlowBoundary(project globalconfig.Project, now time.Time) (time.Time, string) {
	now = now.UTC()
	recentBoundary := now.Add(-doctorWorkflowReviewFlowRecentWindow)
	modifiedAt := doctorWorkflowModifiedAt(project)
	if modifiedAt.After(now) {
		modifiedAt = now
	}
	if modifiedAt.After(recentBoundary) {
		return modifiedAt, "workflow_modified_at"
	}
	return recentBoundary, "recent_window"
}

func doctorWorkflowTelemetryBoundaryValue(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func doctorWorkflowOptimizationMetricsForProject(
	ctx context.Context,
	db doctorTelemetryStore,
	projectID string,
	cfg workflowconfig.Config,
	reviewFlowBoundaryAt time.Time,
	reviewFlowBoundaryType string,
) (doctorWorkflowOptimizationMetrics, error) {
	pricing, err := budget.PricingForConfig(budget.Config{PricingPath: cfg.Budget.PricingPath})
	if err != nil {
		return doctorWorkflowOptimizationMetrics{}, fmt.Errorf("load budget pricing: %w", err)
	}
	sessions, err := doctorWorkflowSessionTelemetry(ctx, db, projectID, pricing)
	if err != nil {
		return doctorWorkflowOptimizationMetrics{}, err
	}
	usage, err := doctorWorkflowUsageTelemetry(ctx, db, projectID)
	if err != nil {
		return doctorWorkflowOptimizationMetrics{}, err
	}
	lanes, err := doctorWorkflowLaneTelemetry(ctx, db, projectID, cfg.Agent.AutoPromote.ReworkState)
	if err != nil {
		return doctorWorkflowOptimizationMetrics{}, err
	}
	scheduler, err := doctorWorkflowSchedulerTelemetry(ctx, db, projectID)
	if err != nil {
		return doctorWorkflowOptimizationMetrics{}, err
	}
	reviewFlow, err := doctorWorkflowReviewFlowTelemetry(ctx, db, projectID, cfg.Agent.AutoPromote.SourceState, reviewFlowBoundaryAt, reviewFlowBoundaryType)
	if err != nil {
		return doctorWorkflowOptimizationMetrics{}, err
	}
	sessionMultiplierKills, err := doctorWorkflowSessionGuardTelemetry(ctx, db, projectID)
	if err != nil {
		return doctorWorkflowOptimizationMetrics{}, err
	}
	orphanRecovery, err := doctorOrphanRecoveryTelemetry(ctx, db, projectID)
	if err != nil {
		return doctorWorkflowOptimizationMetrics{}, err
	}

	metrics := doctorWorkflowOptimizationMetrics{
		SessionCount:                   sessions.count,
		UsageEventCount:                usage.count,
		InputTokens:                    sessions.inputTokens,
		CachedInputTokens:              sessions.cachedInputTokens,
		OutputTokens:                   sessions.outputTokens,
		TotalTokens:                    sessions.totalTokens,
		MedianSessionTokens:            doctorPercentileInt64(sessions.totalTokensBySession, 0.50),
		P90SessionTokens:               doctorPercentileInt64(sessions.totalTokensBySession, 0.90),
		P90SessionBillableTokens:       doctorPercentileInt64(sessions.billableTokensBySession, 0.90),
		MaxSessionTokens:               doctorMaxInt64(sessions.totalTokensBySession),
		RecentSessionCount:             sessions.recentSessionCount,
		EmptyModelRecentSessions:       sessions.emptyModelRecentSessions,
		RecentModels:                   doctorWorkflowRecentModelTelemetry(sessions.recentModelCounts),
		RecentDefaultModels:            doctorWorkflowRecentModelTelemetry(sessions.recentDefaultModelCounts),
		SessionMultiplierKills:         sessionMultiplierKills,
		MaxSessionsPerIssue:            doctorMaxMapValue(sessions.issueSessionCounts),
		MaxSessionsIssue:               doctorMaxMapKey(sessions.issueSessionCounts),
		FailureTokens:                  sessions.failureTokens,
		SchedulerDecisionCount:         scheduler.count,
		SchedulerSkippedDecisions:      scheduler.skipped,
		LaneEventCount:                 lanes.count,
		LaneDwellP90Seconds:            doctorPercentileInt64(lanes.durations, 0.90),
		MaxReworkLapsPerIssue:          doctorMaxMapValue(lanes.reworkLaps),
		MaxReworkLapsIssue:             doctorMaxMapKey(lanes.reworkLaps),
		BudgetEstimateTokens:           doctorWorkflowBudgetEstimateTotal,
		BudgetEstimateBillableTokens:   doctorWorkflowBudgetEstimateBillable,
		ReviewEntryCount:               reviewFlow.reviewEntryCount,
		ReviewEntryIssue:               reviewFlow.reviewEntryIssue,
		ReviewEntryIssueCount:          reviewFlow.reviewEntryIssueCount,
		ReviewFlowBoundaryAt:           doctorWorkflowTelemetryBoundaryValue(reviewFlow.boundaryAt),
		ReviewFlowBoundaryType:         reviewFlow.boundaryType,
		InvalidWorkpadStatusDecisions:  reviewFlow.invalidWorkpadStatusDecisions,
		InvalidWorkpadStatusIssue:      reviewFlow.invalidWorkpadStatusIssue,
		InvalidWorkpadStatusIssueCount: reviewFlow.invalidWorkpadStatusIssueCount,
		OrphanRecovery:                 orphanRecovery,
		MedianSessionCostUSDByEffort:   doctorWorkflowMedianSessionCostUSDByEffort(sessions.costUSDByEffort),
	}
	if usage.count > 0 {
		metrics.InputTokens = usage.inputTokens
		metrics.CachedInputTokens = usage.cachedInputTokens
		metrics.OutputTokens = usage.outputTokens
		metrics.TotalTokens = usage.totalTokens
		metrics.FailureTokens = usage.failureTokens
	}
	metrics.InputOutputRatio = doctorRatio(metrics.InputTokens, metrics.OutputTokens)
	metrics.CacheReadFraction = doctorRatio(metrics.CachedInputTokens, metrics.InputTokens)
	metrics.EmptyModelRecentFraction = doctorRatio(metrics.EmptyModelRecentSessions, metrics.RecentSessionCount)
	metrics.SchedulerSkipRate = doctorRatio(metrics.SchedulerSkippedDecisions, metrics.SchedulerDecisionCount)
	metrics.SlowestLane, metrics.SlowestLaneP90Seconds = doctorSlowestLane(lanes.laneDuration)
	if metrics.P90SessionBillableTokens > 0 && metrics.BudgetEstimateBillableTokens > 0 {
		metrics.BudgetEstimateBillableDriftRatio = doctorRoundedFloat(float64(metrics.P90SessionBillableTokens)/float64(metrics.BudgetEstimateBillableTokens), 4)
		metrics.BudgetEstimateDriftRatio = metrics.BudgetEstimateBillableDriftRatio
	}
	return metrics, nil
}

func doctorWorkflowReviewFlowTelemetry(ctx context.Context, db doctorTelemetryStore, projectID string, reviewState string, boundaryAt time.Time, boundaryType string) (doctorWorkflowReviewFlowMetrics, error) {
	sessions, err := doctorWorkflowCompletedSessionTelemetry(ctx, db, projectID, reviewState, boundaryAt)
	if err != nil {
		return doctorWorkflowReviewFlowMetrics{}, err
	}
	entries, err := doctorWorkflowReviewEntryTelemetry(ctx, db, projectID, reviewState, boundaryAt)
	if err != nil {
		return doctorWorkflowReviewFlowMetrics{}, err
	}
	metrics := doctorWorkflowReviewFlowMetrics{boundaryAt: boundaryAt, boundaryType: boundaryType}
	reviewEntryCounts := map[string]int64{}
	for _, entry := range entries {
		if !doctorWorkflowReviewEntryFollowsCompletedSession(entry, sessions[entry.issueKey]) {
			continue
		}
		reviewEntryCounts[entry.issueKey]++
		metrics.reviewEntryCount++
	}
	metrics.reviewEntryIssue = doctorMaxMapKey(reviewEntryCounts)
	metrics.reviewEntryIssueCount = doctorMaxMapValue(reviewEntryCounts)

	invalidCounts, err := doctorWorkflowInvalidWorkpadStatusTelemetry(ctx, db, projectID, boundaryAt)
	if err != nil {
		return doctorWorkflowReviewFlowMetrics{}, err
	}
	for _, count := range invalidCounts {
		metrics.invalidWorkpadStatusDecisions += count
	}
	metrics.invalidWorkpadStatusIssue = doctorMaxMapKey(invalidCounts)
	metrics.invalidWorkpadStatusIssueCount = doctorMaxMapValue(invalidCounts)
	return metrics, nil
}

func doctorWorkflowCompletedSessionTelemetry(ctx context.Context, db doctorTelemetryStore, projectID string, reviewState string, boundaryAt time.Time) (map[string][]doctorWorkflowCompletedSession, error) {
	reviewState = strings.ToLower(strings.TrimSpace(reviewState))
	boundary := doctorWorkflowTelemetryBoundaryValue(boundaryAt)
	rows, err := db.QueryContext(ctx, `
SELECT
  COALESCE(NULLIF(s.identifier, ''), NULLIF(s.issue_id, ''), NULLIF(s.issue_url, ''), 'unassigned'),
  COALESCE(s.completed_at, '')
FROM codex_sessions s
WHERE s.completed_at IS NOT NULL
  AND lower(trim(COALESCE(s.final_state, ''))) IN ('completed', ?)
  AND (? = '' OR s.completed_at >= ?)
  AND (
    ? = ''
    OR s.id IN (
      SELECT DISTINCT session_id
      FROM usage_events
      WHERE session_id IS NOT NULL
        AND project_id = ?
    )
  )
ORDER BY s.completed_at DESC, s.id DESC
LIMIT 500`, reviewState, boundary, boundary, projectID, projectID)
	if err != nil {
		return nil, fmt.Errorf("read completed session review-flow telemetry: %w", err)
	}
	defer rows.Close()

	sessions := map[string][]doctorWorkflowCompletedSession{}
	for rows.Next() {
		var issueKey string
		var completedAtRaw string
		if err := rows.Scan(&issueKey, &completedAtRaw); err != nil {
			return nil, err
		}
		completedAt, err := doctorWorkflowSessionTimestamp(completedAtRaw)
		if err != nil {
			return nil, err
		}
		issueKey = strings.TrimSpace(issueKey)
		sessions[issueKey] = append(sessions[issueKey], doctorWorkflowCompletedSession{
			issueKey:    issueKey,
			completedAt: completedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sessions, nil
}

func doctorWorkflowReviewEntryTelemetry(ctx context.Context, db doctorTelemetryStore, projectID string, reviewState string, boundaryAt time.Time) ([]doctorWorkflowReviewEntryEvent, error) {
	reviewState = strings.ToLower(strings.TrimSpace(reviewState))
	if reviewState == "" {
		reviewState = "human review"
	}
	boundary := doctorWorkflowTelemetryBoundaryValue(boundaryAt)
	rows, err := db.QueryContext(ctx, `
SELECT
  COALESCE(NULLIF(identifier, ''), NULLIF(issue_id, ''), NULLIF(issue_url, ''), 'unassigned'),
  COALESCE(started_at, '')
FROM workflow_phase_events
WHERE phase_type = 'lane'
  AND lower(trim(COALESCE(status, ''))) = 'entered'
  AND lower(trim(phase_name)) = ?
  AND (? = '' OR started_at >= ?)
  AND (? = '' OR project_id = ?)
ORDER BY started_at DESC, id DESC
LIMIT 500`, reviewState, boundary, boundary, projectID, projectID)
	if err != nil {
		return nil, fmt.Errorf("read review-state entry telemetry: %w", err)
	}
	defer rows.Close()

	entries := []doctorWorkflowReviewEntryEvent{}
	for rows.Next() {
		var issueKey string
		var startedAtRaw string
		if err := rows.Scan(&issueKey, &startedAtRaw); err != nil {
			return nil, err
		}
		startedAt, err := doctorWorkflowSessionTimestamp(startedAtRaw)
		if err != nil {
			return nil, err
		}
		entries = append(entries, doctorWorkflowReviewEntryEvent{
			issueKey:  strings.TrimSpace(issueKey),
			startedAt: startedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func doctorWorkflowReviewEntryFollowsCompletedSession(entry doctorWorkflowReviewEntryEvent, sessions []doctorWorkflowCompletedSession) bool {
	for _, session := range sessions {
		if session.completedAt.IsZero() || entry.startedAt.IsZero() {
			continue
		}
		if entry.startedAt.Before(session.completedAt) {
			continue
		}
		if entry.startedAt.Sub(session.completedAt) <= 10*time.Minute {
			return true
		}
	}
	return false
}

func doctorWorkflowInvalidWorkpadStatusTelemetry(ctx context.Context, db doctorTelemetryStore, projectID string, boundaryAt time.Time) (map[string]int64, error) {
	boundary := doctorWorkflowTelemetryBoundaryValue(boundaryAt)
	rows, err := db.QueryContext(ctx, `
SELECT
  COALESCE(NULLIF(identifier, ''), NULLIF(issue_id, ''), NULLIF(issue_url, ''), 'unassigned'),
  COUNT(*)
FROM workflow_phase_events
WHERE phase_type = 'lane'
  AND lower(trim(COALESCE(status, ''))) = 'entered'
  AND lower(trim(COALESCE(reason, ''))) = 'workpad_status_invalid'
  AND (? = '' OR started_at >= ?)
  AND (? = '' OR project_id = ?)
GROUP BY COALESCE(NULLIF(identifier, ''), NULLIF(issue_id, ''), NULLIF(issue_url, ''), 'unassigned')`, boundary, boundary, projectID, projectID)
	if err != nil {
		return nil, fmt.Errorf("read invalid workpad status telemetry: %w", err)
	}
	defer rows.Close()

	counts := map[string]int64{}
	for rows.Next() {
		var issueKey string
		var count int64
		if err := rows.Scan(&issueKey, &count); err != nil {
			return nil, err
		}
		counts[strings.TrimSpace(issueKey)] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}

func doctorWorkflowSessionTelemetry(ctx context.Context, db doctorTelemetryStore, projectID string, pricing budget.PricingTable) (doctorWorkflowSessionMetrics, error) {
	rows, err := db.QueryContext(ctx, `
SELECT
  COALESCE(s.total_tokens, 0),
  COALESCE(s.input_tokens, 0),
  COALESCE(s.cached_input_tokens, 0),
  COALESCE(s.output_tokens, 0),
  COALESCE(s.model, ''),
  COALESCE(s.requested_model, ''),
  COALESCE(s.reasoning_effort, ''),
  COALESCE(s.final_state, ''),
  COALESCE(NULLIF(s.identifier, ''), NULLIF(s.issue_id, ''), NULLIF(s.issue_url, ''), 'unassigned'),
  COALESCE(s.completed_at, '')
FROM codex_sessions s
WHERE s.completed_at IS NOT NULL
  AND (
    ? = ''
    OR s.id IN (
      SELECT DISTINCT session_id
      FROM usage_events
      WHERE session_id IS NOT NULL
        AND project_id = ?
    )
  )
ORDER BY s.completed_at DESC, s.id DESC`, projectID, projectID)
	if err != nil {
		return doctorWorkflowSessionMetrics{}, fmt.Errorf("read codex session telemetry: %w", err)
	}
	defer rows.Close()

	metrics := doctorWorkflowSessionMetrics{
		issueSessionCounts:       map[string]int64{},
		recentModelCounts:        map[string]int64{},
		recentDefaultModelCounts: map[string]int64{},
		costUSDByEffort:          map[string][]float64{},
	}
	var recentCutoff time.Time
	for rows.Next() {
		var totalTokens int64
		var inputTokens int64
		var cachedInputTokens int64
		var outputTokens int64
		var model string
		var requestedModel string
		var reasoningEffort string
		var finalState string
		var issueKey string
		var completedAtRaw string
		if err := rows.Scan(&totalTokens, &inputTokens, &cachedInputTokens, &outputTokens, &model, &requestedModel, &reasoningEffort, &finalState, &issueKey, &completedAtRaw); err != nil {
			return doctorWorkflowSessionMetrics{}, err
		}
		completedAt, err := doctorWorkflowSessionTimestamp(completedAtRaw)
		if err != nil {
			return doctorWorkflowSessionMetrics{}, err
		}
		metrics.count++
		metrics.inputTokens += inputTokens
		metrics.cachedInputTokens += cachedInputTokens
		metrics.outputTokens += outputTokens
		metrics.totalTokens += totalTokens
		if totalTokens > 0 {
			metrics.totalTokensBySession = append(metrics.totalTokensBySession, totalTokens)
		}
		if billableTokens := doctorWorkflowBillableTokens(inputTokens, cachedInputTokens, outputTokens, totalTokens, model, pricing); billableTokens > 0 {
			metrics.billableTokensBySession = append(metrics.billableTokensBySession, billableTokens)
		}
		reasoningEffort = strings.ToLower(strings.TrimSpace(reasoningEffort))
		costModel := doctorFirstNonEmptyString(model, requestedModel)
		if reasoningEffort != "" {
			if costUSD, ok := budget.UsageCostUSD(pricing, costModel, inputTokens, cachedInputTokens, outputTokens); ok && costUSD > 0 {
				metrics.costUSDByEffort[reasoningEffort] = append(metrics.costUSDByEffort[reasoningEffort], costUSD)
			}
		}
		metrics.issueSessionCounts[issueKey]++
		if metrics.count == 1 {
			recentCutoff = completedAt.Add(-doctorWorkflowRecentSessionWindow)
		}
		if metrics.recentSessionCount < doctorWorkflowRecentSessionLimit && !completedAt.Before(recentCutoff) {
			metrics.recentSessionCount++
			model = strings.TrimSpace(model)
			if model == "" {
				metrics.emptyModelRecentSessions++
			} else {
				metrics.recentModelCounts[model]++
				if strings.TrimSpace(requestedModel) == "" {
					metrics.recentDefaultModelCounts[model]++
				}
			}
		}
		if doctorWorkflowSessionFailed(finalState) {
			metrics.failureTokens += totalTokens
		}
	}
	if err := rows.Err(); err != nil {
		return doctorWorkflowSessionMetrics{}, err
	}
	return metrics, nil
}

func doctorWorkflowRecentModelTelemetry(counts map[string]int64) []doctorWorkflowModelTelemetry {
	models := make([]doctorWorkflowModelTelemetry, 0, len(counts))
	for model, count := range counts {
		models = append(models, doctorWorkflowModelTelemetry{Model: model, SessionCount: count})
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].SessionCount != models[j].SessionCount {
			return models[i].SessionCount > models[j].SessionCount
		}
		return models[i].Model < models[j].Model
	})
	return models
}

func doctorWorkflowMedianSessionCostUSDByEffort(costs map[string][]float64) map[string]float64 {
	medians := make(map[string]float64, len(costs))
	for effort, values := range costs {
		if median := doctorPercentileFloat64(values, 0.50); median > 0 {
			medians[effort] = doctorRoundedFloat(median, 4)
		}
	}
	return medians
}

func doctorWorkflowSpendProgressLimitFinding(
	projectID string,
	workflowPath string,
	cfg workflowconfig.Config,
	medianCostByEffort map[string]float64,
) (doctorWorkflowOptimizationFinding, bool) {
	configuredLimit := cfg.Agent.NoProgressSpendLimitUSD
	if cfg.Budget.EffectiveBillingMode() != workflowconfig.BillingModeMetered ||
		configuredLimit <= 0 ||
		len(medianCostByEffort) == 0 {
		return doctorWorkflowOptimizationFinding{}, false
	}
	efforts := make([]string, 0, len(medianCostByEffort))
	for effort := range medianCostByEffort {
		efforts = append(efforts, effort)
	}
	slices.Sort(efforts)
	offendingMedians := map[string]float64{}
	effectiveLimits := map[string]float64{}
	details := make([]string, 0, len(efforts))
	recommendedBase := configuredLimit
	for _, effort := range efforts {
		median := medianCostByEffort[effort]
		effective := workflowconfig.EffectiveNoProgressSpendLimitUSD(configuredLimit, effort)
		if median <= 0 || effective >= median {
			continue
		}
		offendingMedians[effort] = median
		effectiveLimits[effort] = effective
		details = append(details, fmt.Sprintf("%s (%s < %s)", effort, budget.FormatUSD(effective), budget.FormatUSD(median)))
		candidate := median * 1.5 / workflowconfig.NoProgressSpendLimitMultiplier(effort)
		recommendedBase = math.Max(recommendedBase, candidate)
	}
	if len(offendingMedians) == 0 {
		return doctorWorkflowOptimizationFinding{}, false
	}
	recommendedBase = math.Ceil(recommendedBase*100) / 100
	return doctorWorkflowFinding(
		projectID,
		workflowPath,
		doctorWorkflowRuleSpendProgressBelowMedian,
		"Spend-progress breaker is below observed session cost",
		"effective breaker limit is below observed p50 per-session cost for effort tier(s): "+strings.Join(details, ", "),
		0,
		map[string]any{
			"configured_base_limit_usd":             configuredLimit,
			"effective_limit_usd_by_effort":         effectiveLimits,
			"observed_p50_session_cost_by_effort":   offendingMedians,
			"recommended_retry_cost_headroom_ratio": 1.5,
		},
		doctorWorkflowOptimizationPatch{Path: "agent.no_progress_spend_limit_usd", Value: recommendedBase},
	), true
}

func doctorWorkflowSessionGuardTelemetry(ctx context.Context, db doctorTelemetryStore, projectID string) ([]doctorWorkflowSessionGuardIncident, error) {
	rows, err := db.QueryContext(ctx, `
SELECT
  COALESCE(NULLIF(identifier, ''), NULLIF(issue_id, ''), NULLIF(issue_url, ''), 'unassigned'),
  COALESCE(error_message, '')
FROM work_attempts
WHERE completed_at IS NOT NULL
  AND (? = '' OR project_id = ?)
  AND error_message LIKE '%session token ceiling exceeded%'
  AND error_message LIKE '%source=max_session_context_multiplier%'
ORDER BY completed_at DESC, id DESC`, projectID, projectID)
	if err != nil {
		return nil, fmt.Errorf("read session guard telemetry: %w", err)
	}
	defer rows.Close()

	byIssue := map[string]*doctorWorkflowSessionGuardIncident{}
	for rows.Next() {
		var issueIdentifier string
		var errorMessage string
		if err := rows.Scan(&issueIdentifier, &errorMessage); err != nil {
			return nil, err
		}
		issueIdentifier = strings.TrimSpace(issueIdentifier)
		incident, ok := byIssue[issueIdentifier]
		if !ok {
			incident = &doctorWorkflowSessionGuardIncident{IssueIdentifier: issueIdentifier}
			byIssue[issueIdentifier] = incident
		}
		incident.AttemptCount++
		if incident.CeilingTokens == 0 {
			incident.CeilingTokens = doctorWorkflowErrorInt64(errorMessage, "ceiling_tokens")
		}
		if incident.ContextMultiplier == 0 {
			incident.ContextMultiplier = doctorWorkflowErrorFloat64(errorMessage, "context_multiplier")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	incidents := make([]doctorWorkflowSessionGuardIncident, 0, len(byIssue))
	for _, incident := range byIssue {
		incidents = append(incidents, *incident)
	}
	sort.Slice(incidents, func(i, j int) bool {
		if incidents[i].AttemptCount != incidents[j].AttemptCount {
			return incidents[i].AttemptCount > incidents[j].AttemptCount
		}
		return incidents[i].IssueIdentifier < incidents[j].IssueIdentifier
	})
	return incidents, nil
}

func doctorOrphanRecoveryTelemetry(ctx context.Context, db doctorTelemetryStore, projectID string) (doctorOrphanRecoveryMetrics, error) {
	rows, err := db.QueryContext(ctx, `
SELECT
  COALESCE(s.final_state, ''),
  COALESCE(s.orphan_recovery_outcome, ''),
  COALESCE(s.orphan_recovery_fallback_reason, ''),
  COALESCE(s.input_tokens, 0),
  COALESCE(s.cached_input_tokens, 0)
FROM codex_sessions s
LEFT JOIN work_attempts w ON w.id = s.work_attempt_id
WHERE datetime(s.started_at) >= datetime('now', '-24 hours')
  AND (? = '' OR w.project_id = ?)
  AND (s.final_state = 'orphaned' OR COALESCE(s.orphan_recovery_outcome, '') != '')
ORDER BY s.started_at DESC, s.id DESC`, projectID, projectID)
	if err != nil {
		return doctorOrphanRecoveryMetrics{}, fmt.Errorf("read orphan recovery telemetry: %w", err)
	}
	defer rows.Close()

	metrics := doctorOrphanRecoveryMetrics{FallbackReasons: map[string]int64{}}
	for rows.Next() {
		var finalState, outcome, reason string
		var inputTokens, cachedInputTokens int64
		if err := rows.Scan(&finalState, &outcome, &reason, &inputTokens, &cachedInputTokens); err != nil {
			return doctorOrphanRecoveryMetrics{}, err
		}
		if finalState == "orphaned" {
			metrics.Detected++
		}
		switch outcome {
		case "resumed":
			metrics.Reattached++
			metrics.ResumedInputTokens += inputTokens
			metrics.ResumedCachedTokens += cachedInputTokens
		case "fresh":
			metrics.FreshContinuations++
			metrics.ReattachFailures++
			reason = doctorOrphanFallbackReason(reason)
			metrics.FallbackReasons[reason]++
		}
	}
	if err := rows.Err(); err != nil {
		return doctorOrphanRecoveryMetrics{}, err
	}
	if metrics.ResumedInputTokens > 0 {
		metrics.ResumedCachedInputShare = doctorRoundedFloat(float64(metrics.ResumedCachedTokens)/float64(metrics.ResumedInputTokens), 4)
	}
	return metrics, nil
}

func doctorOrphanFallbackReason(reason string) string {
	reason = strings.TrimSpace(reason)
	lower := strings.ToLower(reason)
	switch {
	case strings.Contains(lower, "rollout") && strings.Contains(lower, "not found"):
		return "rollout file not found"
	case strings.Contains(lower, "payload") && (strings.Contains(lower, "too large") || strings.Contains(lower, "oversized")):
		return "resume payload too large"
	case reason == "":
		return "unknown"
	default:
		return reason
	}
}

func doctorDominantFallbackReason(reasons map[string]int64) (string, int64) {
	var dominant string
	var count int64
	for reason, candidate := range reasons {
		if candidate > count || candidate == count && reason < dominant {
			dominant, count = reason, candidate
		}
	}
	return dominant, count
}

func doctorWorkflowErrorInt64(message string, key string) int64 {
	value := doctorWorkflowErrorValue(message, key)
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

func doctorWorkflowErrorFloat64(message string, key string) float64 {
	value := doctorWorkflowErrorValue(message, key)
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	return parsed
}

func doctorWorkflowErrorValue(message string, key string) string {
	prefix := strings.TrimSpace(key) + "="
	for _, field := range strings.Fields(message) {
		field = strings.Trim(field, ",;()[]{}")
		if value, ok := strings.CutPrefix(field, prefix); ok {
			return strings.Trim(value, ",;()[]{}")
		}
	}
	return ""
}

func doctorWorkflowSessionTimestamp(value string) (time.Time, error) {
	completedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, fmt.Errorf("parse codex session completed_at: %w", err)
	}
	return completedAt, nil
}

func doctorWorkflowUsageTelemetry(ctx context.Context, db doctorTelemetryStore, projectID string) (doctorWorkflowUsageMetrics, error) {
	rows, err := db.QueryContext(ctx, `
SELECT
  COALESCE(input_tokens, 0),
  COALESCE(cached_input_tokens, 0),
  COALESCE(output_tokens, 0),
  COALESCE(total_tokens, 0),
  COALESCE(outcome, '')
FROM usage_events
WHERE (? = '' OR project_id = ?)`, projectID, projectID)
	if err != nil {
		return doctorWorkflowUsageMetrics{}, fmt.Errorf("read usage telemetry: %w", err)
	}
	defer rows.Close()

	var metrics doctorWorkflowUsageMetrics
	for rows.Next() {
		var inputTokens int64
		var cachedInputTokens int64
		var outputTokens int64
		var totalTokens int64
		var outcome string
		if err := rows.Scan(&inputTokens, &cachedInputTokens, &outputTokens, &totalTokens, &outcome); err != nil {
			return doctorWorkflowUsageMetrics{}, err
		}
		metrics.count++
		metrics.inputTokens += inputTokens
		metrics.cachedInputTokens += cachedInputTokens
		metrics.outputTokens += outputTokens
		metrics.totalTokens += totalTokens
		if doctorWorkflowUsageFailed(outcome) {
			metrics.failureTokens += totalTokens
		}
	}
	if err := rows.Err(); err != nil {
		return doctorWorkflowUsageMetrics{}, err
	}
	return metrics, nil
}

func doctorWorkflowLaneTelemetry(ctx context.Context, db doctorTelemetryStore, projectID string, reworkState string) (doctorWorkflowLaneMetrics, error) {
	reworkState = strings.ToLower(strings.TrimSpace(reworkState))
	if reworkState == "" {
		reworkState = "rework"
	}
	rows, err := db.QueryContext(ctx, `
SELECT
  COALESCE(phase_name, ''),
  COALESCE(duration_seconds, 0),
  COALESCE(NULLIF(identifier, ''), NULLIF(issue_id, ''), NULLIF(issue_url, ''), 'unassigned')
FROM workflow_phase_events
WHERE phase_type = 'lane'
  AND finished_at IS NOT NULL
  AND (? = '' OR project_id = ?)`, projectID, projectID)
	if err != nil {
		return doctorWorkflowLaneMetrics{}, fmt.Errorf("read workflow lane telemetry: %w", err)
	}
	defer rows.Close()

	metrics := doctorWorkflowLaneMetrics{
		laneDuration: map[string][]int64{},
		reworkLaps:   map[string]int64{},
	}
	for rows.Next() {
		var phaseName string
		var durationSeconds int64
		var issueKey string
		if err := rows.Scan(&phaseName, &durationSeconds, &issueKey); err != nil {
			return doctorWorkflowLaneMetrics{}, err
		}
		metrics.count++
		if durationSeconds > 0 {
			metrics.durations = append(metrics.durations, durationSeconds)
			metrics.laneDuration[phaseName] = append(metrics.laneDuration[phaseName], durationSeconds)
		}
		if strings.ToLower(strings.TrimSpace(phaseName)) == reworkState {
			metrics.reworkLaps[issueKey]++
		}
	}
	if err := rows.Err(); err != nil {
		return doctorWorkflowLaneMetrics{}, err
	}
	return metrics, nil
}

func doctorWorkflowSchedulerTelemetry(ctx context.Context, db doctorTelemetryStore, projectID string) (doctorWorkflowSchedulerMetrics, error) {
	rows, err := db.QueryContext(ctx, `
SELECT COALESCE(result, ''), COALESCE(selected, 0)
FROM scheduler_decisions
WHERE (? = '' OR project_id = ?)`, projectID, projectID)
	if err != nil {
		return doctorWorkflowSchedulerMetrics{}, fmt.Errorf("read scheduler decisions: %w", err)
	}
	defer rows.Close()

	var metrics doctorWorkflowSchedulerMetrics
	for rows.Next() {
		var result string
		var selected int64
		if err := rows.Scan(&result, &selected); err != nil {
			return doctorWorkflowSchedulerMetrics{}, err
		}
		metrics.count++
		if strings.EqualFold(strings.TrimSpace(result), "skipped") || selected == 0 {
			metrics.skipped++
		}
	}
	if err := rows.Err(); err != nil {
		return doctorWorkflowSchedulerMetrics{}, err
	}
	return metrics, nil
}

func doctorWorkflowOptimizationFindings(
	projectID string,
	workflowPath string,
	cfg workflowconfig.Config,
	metrics doctorWorkflowOptimizationMetrics,
) []doctorWorkflowOptimizationFinding {
	var findings []doctorWorkflowOptimizationFinding
	recovery := metrics.OrphanRecovery
	if cfg.Agent.ExperimentalThreadResume && recovery.FreshContinuations > 0 && recovery.Reattached == 0 {
		reason, count := doctorDominantFallbackReason(recovery.FallbackReasons)
		findings = append(findings, doctorWorkflowFinding(projectID, workflowPath, doctorWorkflowRuleOrphanRecoveryFallback,
			"Orphan thread reattach consistently falls back",
			fmt.Sprintf("all %d recovery attempt(s) used a fresh continuation; dominant failure reason (%d): %s", recovery.FreshContinuations, count, reason),
			0,
			map[string]any{"fresh_continuations": recovery.FreshContinuations, "reattached": recovery.Reattached, "dominant_failure_reason": reason, "dominant_failure_count": count},
		))
	}
	if recovery.Detected > 0 && recovery.Reattached+recovery.FreshContinuations == 0 {
		findings = append(findings, doctorWorkflowFinding(projectID, workflowPath, doctorWorkflowRuleOrphansNeverRecovered,
			"Orphaned sessions were not recovered",
			fmt.Sprintf("%d orphaned session(s) were detected without a recovery outcome in the recent window", recovery.Detected),
			0,
			map[string]any{"detected": recovery.Detected, "reattached": recovery.Reattached, "fresh_continuations": recovery.FreshContinuations},
		))
	}
	if metrics.SessionCount >= 3 && metrics.MedianSessionTokens > 0 {
		ratio := float64(metrics.MaxSessionTokens) / float64(metrics.MedianSessionTokens)
		if ratio >= doctorWorkflowRunawayMedianMultiplier {
			value := doctorRoundUpInt64(int64(float64(metrics.MedianSessionTokens)*doctorWorkflowRunawayMedianMultiplier), 1000)
			configuredCapLimit := int64(math.Ceil(float64(value) * doctorWorkflowRunawayCapTolerance))
			if cfg.Agent.MaxSessionTokens <= 0 || cfg.Agent.MaxSessionTokens > configuredCapLimit {
				findings = append(findings, doctorWorkflowFinding(projectID, workflowPath, doctorWorkflowRuleRunawaySessionTokens,
					"Runaway session tail",
					fmt.Sprintf("max session tokens are %.1fx the median; sizing assumes %s", ratio, doctorWorkflowCapRetryCostAssumption),
					metrics.MaxSessionTokens-metrics.MedianSessionTokens,
					map[string]any{
						"session_count":         metrics.SessionCount,
						"median_session_tokens": metrics.MedianSessionTokens,
						"p90_session_tokens":    metrics.P90SessionTokens,
						"max_session_tokens":    metrics.MaxSessionTokens,
						"max_to_median_ratio":   doctorRoundedFloat(ratio, 2),
						"retry_cost_assumption": doctorWorkflowCapRetryCostAssumption,
					},
					doctorWorkflowOptimizationPatch{Path: "agent.max_session_tokens", Value: value},
				))
			}
		}
	}
	if cfg.Agent.MaxSessionTokens <= 0 && cfg.Agent.MaxSessionContextMultiplier <= 0 {
		patches := []doctorWorkflowOptimizationPatch{}
		if metrics.MedianSessionTokens > 0 {
			patches = append(patches, doctorWorkflowOptimizationPatch{
				Path:  "agent.max_session_tokens",
				Value: doctorRoundUpInt64(int64(float64(metrics.MedianSessionTokens)*doctorWorkflowRunawayMedianMultiplier), 1000),
			})
		}
		findings = append(findings, doctorWorkflowFinding(projectID, workflowPath, doctorWorkflowRuleNoSessionTokenBrake,
			"Session token brake is disabled",
			"neither agent.max_session_tokens nor agent.max_session_context_multiplier is configured; wall-clock, turn, and no-progress brakes do not cap token consumption, and any generated token cap assumes "+doctorWorkflowCapRetryCostAssumption,
			0,
			map[string]any{
				"max_session_tokens":             cfg.Agent.MaxSessionTokens,
				"max_session_context_multiplier": cfg.Agent.MaxSessionContextMultiplier,
				"unbraked_session_count":         metrics.SessionCount,
				"retry_cost_assumption":          doctorWorkflowCapRetryCostAssumption,
			},
			patches...,
		))
	}
	if finding, ok := doctorWorkflowSpendProgressLimitFinding(projectID, workflowPath, cfg, metrics.MedianSessionCostUSDByEffort); ok {
		findings = append(findings, finding)
	}
	if incidents := doctorWorkflowRepeatedSessionGuardIncidents(metrics.SessionMultiplierKills); len(incidents) > 0 {
		attemptCounts := make(map[string]int64, len(incidents))
		killedIssues := make([]string, 0, len(incidents))
		ceilingTokens := make([]int64, 0, len(incidents))
		contextMultipliers := make([]float64, 0, len(incidents))
		var killCount int64
		for _, incident := range incidents {
			attemptCounts[incident.IssueIdentifier] = incident.AttemptCount
			killCount += incident.AttemptCount
			killedIssues = append(killedIssues, fmt.Sprintf("%s x%d", incident.IssueIdentifier, incident.AttemptCount))
			ceilingTokens = append(ceilingTokens, incident.CeilingTokens)
			multiplier := incident.ContextMultiplier
			if multiplier <= 0 {
				multiplier = cfg.Agent.MaxSessionContextMultiplier
			}
			contextMultipliers = append(contextMultipliers, multiplier)
		}
		patches := []doctorWorkflowOptimizationPatch{}
		if cfg.Agent.MaxSessionTokens > 0 && cfg.Agent.MaxSessionContextMultiplier > 0 {
			patches = append(patches, doctorWorkflowOptimizationPatch{Path: "agent.max_session_context_multiplier", Value: 0})
		}
		findings = append(findings, doctorWorkflowFinding(projectID, workflowPath, doctorWorkflowRuleSessionMultiplierKills,
			"Session context multiplier repeatedly killed work",
			fmt.Sprintf("%s produced %d session token ceiling kills at ceiling(s) %s for %s; remove or recalibrate the multiplier and rely on an intentional max_session_tokens cap", doctorWorkflowMultiplierList(contextMultipliers), killCount, doctorWorkflowInt64List(ceilingTokens), strings.Join(killedIssues, ", ")),
			0,
			map[string]any{
				"configured_max_session_tokens":             cfg.Agent.MaxSessionTokens,
				"configured_max_session_context_multiplier": cfg.Agent.MaxSessionContextMultiplier,
				"context_multipliers":                       contextMultipliers,
				"ceiling_tokens":                            ceilingTokens,
				"killed_issues":                             killedIssues,
				"attempt_counts":                            attemptCounts,
				"session_multiplier_kill_count":             killCount,
			},
			patches...,
		))
	}
	if metrics.MaxReworkLapsPerIssue > doctorWorkflowReworkLapThreshold && (cfg.Agent.AutoPromote.ReworkLimit == 0 || metrics.MaxReworkLapsPerIssue > int64(cfg.Agent.AutoPromote.ReworkLimit)) {
		value := int(math.Max(1, math.Min(float64(metrics.MaxReworkLapsPerIssue-1), 2)))
		findings = append(findings, doctorWorkflowFinding(projectID, workflowPath, doctorWorkflowRuleReworkLaps,
			"Repeated rework laps",
			fmt.Sprintf("issue %s entered rework %d times", metrics.MaxReworkLapsIssue, metrics.MaxReworkLapsPerIssue),
			metrics.MedianSessionTokens*metrics.MaxReworkLapsPerIssue,
			map[string]any{
				"max_rework_laps_per_issue": metrics.MaxReworkLapsPerIssue,
				"max_rework_laps_issue":     metrics.MaxReworkLapsIssue,
				"configured_rework_limit":   cfg.Agent.AutoPromote.ReworkLimit,
			},
			doctorWorkflowOptimizationPatch{Path: "agent.auto_promote.rework_limit", Value: value},
		))
	}
	if cfg.Gate.Validator.Enabled && strings.TrimSpace(cfg.Gate.Validator.Model) == "" {
		findings = append(findings, doctorWorkflowFinding(projectID, workflowPath, doctorWorkflowRuleValidatorModel,
			"Validator model is implicit",
			"gate.validator.enabled=true without a gate.validator.model override",
			doctorMaxInt64([]int64{metrics.FailureTokens, metrics.MedianSessionTokens}),
			map[string]any{
				"validator_enabled": true,
				"validator_model":   "",
				"failure_tokens":    metrics.FailureTokens,
			},
			doctorWorkflowOptimizationPatch{Path: "gate.validator.model", Value: doctorWorkflowValidatorModel},
		))
	}
	if cfg.Budget.Enabled && metrics.RecentSessionCount > 0 && metrics.EmptyModelRecentSessions > 0 && metrics.EmptyModelRecentFraction >= doctorWorkflowEmptyModelMinFraction {
		detail := fmt.Sprintf("%d of %d recent sessions have an empty model", metrics.EmptyModelRecentSessions, metrics.RecentSessionCount)
		evidence := map[string]any{
			"recent_session_count":        metrics.RecentSessionCount,
			"empty_model_recent_sessions": metrics.EmptyModelRecentSessions,
			"empty_model_recent_fraction": doctorRoundedFloat(metrics.EmptyModelRecentFraction, 2),
		}
		if modelConfig, ok := doctorWorkflowDefaultRouteModelConfig(cfg); ok {
			detail += "; workflow model is configured via " + modelConfig.Source
			evidence["configured_model_source"] = modelConfig.Source
			if modelConfig.Model != "" {
				evidence["configured_model"] = modelConfig.Model
			}
		}
		finding := doctorWorkflowFinding(projectID, workflowPath, doctorWorkflowRuleEmptyModelTelemetry,
			"Session model telemetry is incomplete",
			detail,
			0,
			evidence,
		)
		finding.Severity = "info"
		findings = append(findings, finding)
	}
	if metrics.P90SessionBillableTokens > 0 && metrics.BudgetEstimateDriftRatio >= doctorWorkflowBudgetDriftRatio {
		findings = append(findings, doctorWorkflowFinding(projectID, workflowPath, doctorWorkflowRuleBudgetEstimateDrift,
			"Budget estimate is below observed p90",
			fmt.Sprintf("observed p90 billable session tokens are %.2fx the default dispatch estimate", metrics.BudgetEstimateDriftRatio),
			metrics.P90SessionBillableTokens-metrics.BudgetEstimateBillableTokens,
			map[string]any{
				"budget_estimate_billable_tokens":      metrics.BudgetEstimateBillableTokens,
				"budget_estimate_tokens":               doctorWorkflowBudgetEstimateTotal,
				"observed_p90_billable_session_tokens": metrics.P90SessionBillableTokens,
				"observed_p90_session_tokens":          metrics.P90SessionTokens,
				"drift_ratio":                          metrics.BudgetEstimateDriftRatio,
			},
		))
	}
	if metrics.SchedulerDecisionCount >= doctorWorkflowSchedulerMinDecisions && metrics.SchedulerSkipRate >= doctorWorkflowSchedulerHighSkipRate {
		value := cfg.Polling.IntervalMS * 2
		if value < workflowconfig.DefaultPollingIntervalMS {
			value = workflowconfig.DefaultPollingIntervalMS
		}
		if value > 600_000 {
			value = 600_000
		}
		findings = append(findings, doctorWorkflowFinding(projectID, workflowPath, doctorWorkflowRuleSchedulerSkipRate,
			"Scheduler skip rate is high",
			fmt.Sprintf("%.0f%% of recent scheduler decisions skipped dispatch", metrics.SchedulerSkipRate*100),
			0,
			map[string]any{
				"scheduler_decision_count":    metrics.SchedulerDecisionCount,
				"scheduler_skipped_decisions": metrics.SchedulerSkippedDecisions,
				"scheduler_skip_rate":         doctorRoundedFloat(metrics.SchedulerSkipRate, 2),
				"current_polling_interval_ms": cfg.Polling.IntervalMS,
			},
			doctorWorkflowOptimizationPatch{Path: "polling.interval_ms", Value: value},
		))
	}
	if doctorReviewFlowChoice(cfg) == doctorReviewFlowAutopilot && metrics.ReviewEntryCount >= doctorWorkflowReviewFlowMismatchMinEntries {
		findings = append(findings, doctorWorkflowFinding(projectID, workflowPath, doctorWorkflowRuleReviewFlowBehaviorMismatch,
			"Review-flow behavior mismatch",
			fmt.Sprintf("autopilot is configured but completed work entered %s %d times", cfg.Agent.AutoPromote.SourceState, metrics.ReviewEntryCount),
			0,
			map[string]any{
				"review_flow":              doctorReviewFlowAutopilot,
				"review_state":             cfg.Agent.AutoPromote.SourceState,
				"review_entry_count":       metrics.ReviewEntryCount,
				"review_entry_issue":       metrics.ReviewEntryIssue,
				"review_entry_issue_count": metrics.ReviewEntryIssueCount,
				"telemetry_boundary_at":    metrics.ReviewFlowBoundaryAt,
				"telemetry_boundary_type":  metrics.ReviewFlowBoundaryType,
				"auto_promote_enabled":     cfg.Agent.AutoPromote.Enabled,
				"quiet_seconds":            cfg.Agent.AutoPromote.QuietSeconds,
				"gate_wait_state":          doctorReviewFlowGateWaitState(cfg),
			},
		))
	}
	if metrics.InvalidWorkpadStatusDecisions >= doctorWorkflowInvalidWorkpadStatusMinCount {
		findings = append(findings, doctorWorkflowFinding(projectID, workflowPath, doctorWorkflowRuleInvalidWorkpadStatusRecurrence,
			"Invalid Workpad status recurrence",
			fmt.Sprintf("auto-promote recorded %d workpad_status_invalid decision(s)", metrics.InvalidWorkpadStatusDecisions),
			0,
			map[string]any{
				"invalid_workpad_status_decisions":   metrics.InvalidWorkpadStatusDecisions,
				"invalid_workpad_status_issue":       metrics.InvalidWorkpadStatusIssue,
				"invalid_workpad_status_issue_count": metrics.InvalidWorkpadStatusIssueCount,
				"review_flow":                        doctorReviewFlowChoice(cfg),
			},
		))
	}
	return findings
}

func doctorReviewFlowWorkflowFindings(projectID string, workflowPath string, cfg workflowconfig.Config, prompt string) []doctorWorkflowOptimizationFinding {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil
	}
	choice := doctorReviewFlowChoice(cfg)
	reviewState := doctorReviewFlowConfiguredState(cfg.Agent.AutoPromote.SourceState, "Human Review")
	passState := doctorReviewFlowConfiguredState(cfg.Agent.AutoPromote.PassState, "Merging")
	switch choice {
	case doctorReviewFlowAutopilot:
		phrases := doctorReviewFlowPhraseMatches(prompt, doctorReviewFlowEnterReviewPhrases(reviewState))
		if phrases == 0 {
			return nil
		}
		return []doctorWorkflowOptimizationFinding{doctorWorkflowFinding(projectID, workflowPath, doctorWorkflowRuleReviewFlowProseMismatch,
			"Review-flow prose contradicts autopilot",
			"autopilot is configured but WORKFLOW.md instructs agents to enter the review state",
			0,
			map[string]any{
				"review_flow":           choice,
				"review_state":          reviewState,
				"matching_phrase_count": int64(phrases),
				"auto_promote_enabled":  cfg.Agent.AutoPromote.Enabled,
				"quiet_seconds":         cfg.Agent.AutoPromote.QuietSeconds,
				"gate_wait_state":       doctorReviewFlowGateWaitState(cfg),
			},
		)}
	case doctorReviewFlowReviewGate:
		phrases := doctorReviewFlowPhraseMatches(prompt, append(
			doctorReviewFlowSkipReviewPhrases(reviewState),
			doctorReviewFlowDirectPromotePhrases(passState)...,
		))
		if phrases == 0 {
			return nil
		}
		return []doctorWorkflowOptimizationFinding{doctorWorkflowFinding(projectID, workflowPath, doctorWorkflowRuleReviewFlowProseMismatch,
			"Review-flow prose contradicts review-gate",
			"review-gate is configured but WORKFLOW.md promises direct review-state skipping",
			0,
			map[string]any{
				"review_flow":           choice,
				"review_state":          reviewState,
				"pass_state":            passState,
				"matching_phrase_count": int64(phrases),
				"auto_promote_enabled":  cfg.Agent.AutoPromote.Enabled,
				"quiet_seconds":         cfg.Agent.AutoPromote.QuietSeconds,
				"gate_wait_state":       doctorReviewFlowGateWaitState(cfg),
			},
		)}
	default:
		return nil
	}
}

func doctorReviewFlowConfiguredState(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func doctorReviewFlowEnterReviewPhrases(reviewState string) []string {
	phrases := []string{}
	for _, state := range doctorReviewFlowStatePhraseVariants(reviewState) {
		phrases = append(phrases,
			"move the issue to "+state,
			"move this issue to "+state,
			"move it to "+state,
			"move the card to "+state,
			"move the work item to "+state,
			"move back to "+state,
		)
	}
	if label := doctorReviewFlowStateLabel(reviewState); label != "" {
		phrases = append(phrases, "detent:"+label)
	}
	return phrases
}

func doctorReviewFlowSkipReviewPhrases(reviewState string) []string {
	phrases := []string{
		"leave the issue in `in progress`",
		"leave the issue in in progress",
	}
	for _, state := range doctorReviewFlowStatePhraseVariants(reviewState) {
		phrases = append(phrases,
			"do not move it to "+state,
			"do not move the issue to "+state,
			"do not move the work item to "+state,
			"never move the issue to "+state,
			"never move the work item to "+state,
			"skips "+state,
			"skip "+state,
		)
	}
	return phrases
}

func doctorReviewFlowDirectPromotePhrases(passState string) []string {
	phrases := []string{}
	for _, state := range doctorReviewFlowStatePhraseVariants(passState) {
		phrases = append(phrases,
			"promotes the issue directly to "+state,
			"promote the issue directly to "+state,
			"promotes the work item directly to "+state,
			"promote the work item directly to "+state,
			"promote directly to "+state,
		)
	}
	return phrases
}

func doctorReviewFlowStatePhraseVariants(state string) []string {
	state = strings.TrimSpace(state)
	if state == "" {
		return nil
	}
	return []string{"`" + state + "`", state}
}

func doctorReviewFlowStateLabel(state string) string {
	state = strings.ToLower(strings.Join(strings.Fields(state), "-"))
	state = strings.TrimSpace(state)
	if state == "" {
		return ""
	}
	var b strings.Builder
	lastHyphen := false
	for _, r := range state {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			lastHyphen = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case r == '-' && !lastHyphen:
			b.WriteRune(r)
			lastHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func doctorReviewFlowPhraseMatches(text string, phrases []string) int {
	count := 0
	for _, phrase := range phrases {
		phrase = strings.ToLower(strings.Join(strings.Fields(phrase), " "))
		if doctorReviewFlowPhraseMatch(text, phrase) {
			count++
		}
	}
	return count
}

func doctorReviewFlowPhraseMatch(text string, phrase string) bool {
	segments := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return r == '.' || r == '!' || r == '?' || r == ';'
	})
	for _, segment := range segments {
		segment = strings.Join(strings.Fields(segment), " ")
		for offset := 0; offset < len(segment); {
			match := strings.Index(segment[offset:], phrase)
			if match < 0 {
				break
			}
			match += offset
			if !doctorReviewFlowPhraseNegated(segment[:match], segment[match+len(phrase):]) {
				return true
			}
			offset = match + len(phrase)
		}
	}
	return false
}

func doctorReviewFlowPhraseNegated(prefix string, suffix string) bool {
	words := strings.Fields(prefix)
	if len(words) > 5 {
		words = words[len(words)-5:]
	}
	window := " " + strings.Join(words, " ") + " "
	negated := strings.Contains(window, " not ") ||
		strings.Contains(window, " never ") ||
		strings.Contains(window, " don't ") ||
		strings.Contains(window, " avoid ") ||
		strings.Contains(window, " instead of ")
	if !negated {
		return false
	}

	suffix = " " + strings.Join(strings.Fields(suffix), " ") + " "
	return !strings.Contains(suffix, " until ") &&
		!strings.Contains(suffix, " unless ") &&
		!strings.Contains(suffix, " except ")
}

func doctorWorkflowFinding(
	projectID string,
	workflowPath string,
	ruleID string,
	title string,
	detail string,
	impact int64,
	evidence map[string]any,
	patches ...doctorWorkflowOptimizationPatch,
) doctorWorkflowOptimizationFinding {
	return doctorWorkflowOptimizationFinding{
		RuleID:               ruleID,
		ProjectID:            projectID,
		WorkflowPath:         workflowPath,
		Severity:             "warning",
		Title:                title,
		Detail:               detail,
		EstimatedTokenImpact: impact,
		Evidence:             evidence,
		Patch:                patches,
	}
}

func doctorWorkflowHasDefaultRouteModel(cfg workflowconfig.Config) bool {
	_, ok := doctorWorkflowDefaultRouteModelConfig(cfg)
	return ok
}

type doctorWorkflowModelConfig struct {
	Model  string
	Source string
}

func doctorWorkflowWorkerModelChoice(cfg workflowconfig.Config) doctorWorkflowModelChoice {
	modelConfig, ok := doctorWorkflowDefaultRouteModelConfig(cfg)
	if ok && strings.TrimSpace(modelConfig.Model) != "" {
		return doctorWorkflowModelChoice{
			Mode:   "pinned",
			Model:  strings.TrimSpace(modelConfig.Model),
			Source: modelConfig.Source,
		}
	}
	choice := doctorWorkflowModelChoice{Mode: "provider_default"}
	if ok {
		choice.Source = modelConfig.Source
	}
	return choice
}

func doctorWorkflowSessionGuardConfig(cfg workflowconfig.Config) doctorWorkflowSessionGuard {
	return doctorWorkflowSessionGuard{
		MaxSessionTokens:            cfg.Agent.MaxSessionTokens,
		MaxSessionContextMultiplier: cfg.Agent.MaxSessionContextMultiplier,
	}
}

func doctorWorkflowObservedDefaultModelForProjects(projects []doctorWorkflowAnalyzedProject) (doctorWorkflowObservedDefaultModel, bool) {
	counts := map[string]int64{}
	for _, project := range projects {
		if doctorWorkflowWorkerModelChoice(project.config).Mode != "provider_default" {
			continue
		}
		for _, telemetry := range project.metrics.RecentDefaultModels {
			counts[strings.TrimSpace(telemetry.Model)] += telemetry.SessionCount
		}
	}

	var observed doctorWorkflowObservedDefaultModel
	found := false
	for model, count := range counts {
		major, minor, ok := doctorWorkflowModelGeneration(model)
		if !ok {
			continue
		}
		candidate := doctorWorkflowObservedDefaultModel{
			Model:        model,
			SessionCount: count,
			Major:        major,
			Minor:        minor,
		}
		if !found || doctorWorkflowObservedModelLess(observed, candidate) {
			observed = candidate
			found = true
		}
	}
	return observed, found
}

func doctorWorkflowObservedModelLess(left doctorWorkflowObservedDefaultModel, right doctorWorkflowObservedDefaultModel) bool {
	if left.Major != right.Major {
		return left.Major < right.Major
	}
	if left.Minor != right.Minor {
		return left.Minor < right.Minor
	}
	if left.SessionCount != right.SessionCount {
		return left.SessionCount < right.SessionCount
	}
	return left.Model < right.Model
}

func doctorWorkflowStalePinnedModelFinding(project doctorWorkflowAnalyzedProject, observed doctorWorkflowObservedDefaultModel) (doctorWorkflowOptimizationFinding, bool) {
	choice := doctorWorkflowWorkerModelChoice(project.config)
	if choice.Mode != "pinned" {
		return doctorWorkflowOptimizationFinding{}, false
	}
	major, minor, ok := doctorWorkflowModelGeneration(choice.Model)
	if !ok || major > observed.Major || major == observed.Major && minor >= observed.Minor {
		return doctorWorkflowOptimizationFinding{}, false
	}
	return doctorWorkflowFinding(project.projectID, project.workflowPath, doctorWorkflowRulePinnedWorkerModelStale,
		"Pinned worker model trails the observed provider default",
		fmt.Sprintf("pinned worker model %s via %s is generation-behind observed default model %s from %d unpinned session(s); keep, update, or remove the pin according to this project's intended model policy", choice.Model, choice.Source, observed.Model, observed.SessionCount),
		0,
		map[string]any{
			"model_choice":              choice.Mode,
			"pinned_model":              choice.Model,
			"configured_model_source":   choice.Source,
			"observed_default_model":    observed.Model,
			"observed_default_sessions": observed.SessionCount,
		},
	), true
}

func doctorWorkflowModelGeneration(model string) (int, int, bool) {
	version, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(model)), "gpt-")
	if !ok {
		return 0, 0, false
	}
	version, _, _ = strings.Cut(version, "-")
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

func doctorWorkflowDefaultRouteModelConfig(cfg workflowconfig.Config) (doctorWorkflowModelConfig, bool) {
	backends := doctorWorkflowBackendConfigsByID(cfg)
	for _, route := range cfg.AgentRouteConfigs() {
		role := strings.ToLower(strings.TrimSpace(route.Role))
		if role != "" && role != "code" {
			continue
		}
		if !route.Default {
			continue
		}
		if model := strings.TrimSpace(route.Model); model != "" {
			return doctorWorkflowModelConfig{
				Model:  model,
				Source: "agents.routes.model",
			}, true
		}
		backend, ok := backends[strings.TrimSpace(route.Backend)]
		if ok {
			if model := doctorWorkflowBackendCommandModel(backend); model != "" {
				return doctorWorkflowModelConfig{
					Model:  model,
					Source: "agents.backends.command",
				}, true
			}
		}
		if strings.TrimSpace(route.ModelField) != "" {
			return doctorWorkflowModelConfig{
				Source: "agents.routes.model_field",
			}, true
		}
	}
	return doctorWorkflowModelConfig{}, false
}

func doctorWorkflowBackendConfigsByID(cfg workflowconfig.Config) map[string]workflowconfig.AgentBackend {
	backends := cfg.AgentBackendConfigs()
	byID := make(map[string]workflowconfig.AgentBackend, len(backends))
	for _, backend := range backends {
		byID[strings.TrimSpace(backend.ID)] = backend
	}
	return byID
}

func doctorWorkflowBackendCommandModel(backend workflowconfig.AgentBackend) string {
	if strings.TrimSpace(backend.Kind) != workflowconfig.AgentBackendCodex {
		return ""
	}
	return doctorWorkflowCommandConfigModel(backend.Command)
}

func doctorWorkflowCommandConfigModel(command string) string {
	fields := doctorWorkflowCommandFields(command)
	for index, field := range fields {
		field = strings.TrimSpace(field)
		if field == "--config" {
			if index+1 >= len(fields) {
				continue
			}
			if model := doctorWorkflowConfigModel(fields[index+1]); model != "" {
				return model
			}
			continue
		}
		if value, ok := strings.CutPrefix(field, "--config="); ok {
			if model := doctorWorkflowConfigModel(value); model != "" {
				return model
			}
		}
	}
	return ""
}

func doctorWorkflowConfigModel(value string) string {
	key, model, ok := strings.Cut(strings.TrimSpace(value), "=")
	if !ok || strings.TrimSpace(key) != "model" {
		return ""
	}
	return strings.Trim(strings.TrimSpace(model), `"'`)
}

func doctorWorkflowCommandFields(command string) []string {
	var fields []string
	var field strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if field.Len() == 0 {
			return
		}
		fields = append(fields, field.String())
		field.Reset()
	}
	for _, r := range command {
		if escaped {
			field.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if quote == 0 && unicode.IsSpace(r) {
			flush()
			continue
		}
		switch {
		case quote == 0 && (r == '\'' || r == '"'):
			quote = r
		case quote != 0 && r == quote:
			quote = 0
		default:
			field.WriteRune(r)
		}
	}
	if escaped {
		field.WriteRune('\\')
	}
	flush()
	return fields
}

func doctorWorkflowOptimizationWorkflowPath(project globalconfig.Project) (string, error) {
	if strings.TrimSpace(project.WorkflowRef) != "" {
		return "", errors.New("workflow_ref-backed workflow cannot be patched locally")
	}
	return resolveDoctorProjectPath(project, project.Workflow)
}

func doctorWorkflowEstimatedImpact(findings []doctorWorkflowOptimizationFinding) int64 {
	var total int64
	for _, finding := range findings {
		if finding.EstimatedTokenImpact > 0 {
			total += finding.EstimatedTokenImpact
		}
	}
	return total
}

func writeDoctorWorkflowOptimizationPretty(out io.Writer, report doctorWorkflowOptimizationReport) error {
	if out == nil || len(report.Findings) == 0 && len(report.Proposals) == 0 && len(report.CreatedProposalIssues) == 0 && strings.TrimSpace(report.Diff) == "" && len(report.Written) == 0 && !doctorReportHasOrphanRecovery(report) && !doctorWorkflowHasRetroStatus(report.Projects) {
		return nil
	}
	if _, err := fmt.Fprintln(out); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, "Workflow Optimization"); err != nil {
		return err
	}
	if strings.TrimSpace(report.StorePath) != "" {
		if _, err := fmt.Fprintf(out, "Store: %s\n", report.StorePath); err != nil {
			return err
		}
	}
	for _, project := range report.Projects {
		recovery := project.Metrics.OrphanRecovery
		if recovery.Detected+recovery.Reattached+recovery.FreshContinuations > 0 {
			if _, err := fmt.Fprintf(out, "Orphan recovery [%s]: detected=%d, reattached=%d, fresh_continuations=%d, reattach_failures=%d, resumed_cached_input_share=%.1f%%\n", project.ProjectID, recovery.Detected, recovery.Reattached, recovery.FreshContinuations, recovery.ReattachFailures, recovery.ResumedCachedInputShare*100); err != nil {
				return err
			}
		}
		lastRun := "never"
		if project.Retro.LastRun != nil {
			lastRun = project.Retro.LastRun.UTC().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(out, "Retro %s: enabled=%t last_run=%s findings=%d filed_issues=%d updated_issues=%d", project.ProjectID, project.Retro.Enabled, lastRun, project.Retro.Findings, project.Retro.FiledIssues, project.Retro.UpdatedIssues); err != nil {
			return err
		}
		if project.Retro.Error != "" {
			if _, err := fmt.Fprintf(out, " error=%s", project.Retro.Error); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
	}
	for index, finding := range report.Findings {
		if _, err := fmt.Fprintf(out, "%d. [%s] %s", index+1, finding.RuleID, finding.Title); err != nil {
			return err
		}
		if finding.EstimatedTokenImpact > 0 {
			if _, err := fmt.Fprintf(out, " (impact: %d tokens)", finding.EstimatedTokenImpact); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
		if finding.ProjectID != "" {
			if _, err := fmt.Fprintf(out, "   Project: %s\n", finding.ProjectID); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(out, "   Detail: %s\n", finding.Detail); err != nil {
			return err
		}
		if evidence := doctorWorkflowEvidenceLine(finding.Evidence); evidence != "" {
			if _, err := fmt.Fprintf(out, "   Evidence: %s\n", evidence); err != nil {
				return err
			}
		}
		if patch := doctorWorkflowPatchLine(finding.Patch); patch != "" {
			if _, err := fmt.Fprintf(out, "   Suggested patch: %s\n", patch); err != nil {
				return err
			}
		}
	}
	if len(report.Proposals) > 0 {
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, "Governed Self-Improvement Proposals"); err != nil {
			return err
		}
		for index, proposal := range report.Proposals {
			if _, err := fmt.Fprintf(out, "%d. [%s] %s\n", index+1, proposal.ID, proposal.Title); err != nil {
				return err
			}
			if proposal.ProjectID != "" {
				if _, err := fmt.Fprintf(out, "   Project: %s\n", proposal.ProjectID); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(out, "   Signal: %s count=%d pattern=%s\n", proposal.SignalKind, proposal.Count, proposal.Pattern); err != nil {
				return err
			}
			target := proposal.TargetKind
			if proposal.TargetPath != "" {
				target += " " + proposal.TargetPath
			}
			if strings.TrimSpace(target) != "" {
				if _, err := fmt.Fprintf(out, "   Target: %s\n", target); err != nil {
					return err
				}
			}
			if proposal.SuggestedChange != "" {
				if _, err := fmt.Fprintf(out, "   Suggested change: %s\n", proposal.SuggestedChange); err != nil {
					return err
				}
			}
			if proposal.Governance != "" {
				if _, err := fmt.Fprintf(out, "   Governance: %s\n", proposal.Governance); err != nil {
					return err
				}
			}
		}
	}
	if len(report.CreatedProposalIssues) > 0 {
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, "Created Proposal Issues"); err != nil {
			return err
		}
		for _, issue := range report.CreatedProposalIssues {
			reused := ""
			if issue.Reused {
				reused = " reused"
			}
			if _, err := fmt.Fprintf(out, "- %s -> %s%s\n", issue.ProposalID, doctorFirstNonEmptyString(issue.URL, issue.Identifier, issue.IssueID), reused); err != nil {
				return err
			}
		}
	}
	if diff := strings.TrimSpace(report.Diff); diff != "" {
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, "Proposed WORKFLOW.md Frontmatter Patch"); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, diff); err != nil {
			return err
		}
	}
	if len(report.Written) > 0 {
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "Updated WORKFLOW.md files: %s\n", strings.Join(report.Written, ", ")); err != nil {
			return err
		}
	}
	return nil
}

func doctorReportHasOrphanRecovery(report doctorWorkflowOptimizationReport) bool {
	for _, project := range report.Projects {
		recovery := project.Metrics.OrphanRecovery
		if recovery.Detected+recovery.Reattached+recovery.FreshContinuations > 0 {
			return true
		}
	}
	return false
}

func doctorWorkflowHasRetroStatus(projects []doctorWorkflowOptimizationProjectReport) bool {
	return len(projects) > 0
}

func doctorWorkflowEvidenceLine(evidence map[string]any) string {
	if len(evidence) == 0 {
		return ""
	}
	keys := make([]string, 0, len(evidence))
	for key := range evidence {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", key, evidence[key]))
	}
	return strings.Join(parts, ", ")
}

func doctorFirstNonEmptyString(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func doctorWorkflowPatchLine(patches []doctorWorkflowOptimizationPatch) string {
	if len(patches) == 0 {
		return ""
	}
	parts := make([]string, 0, len(patches))
	for _, patch := range patches {
		parts = append(parts, fmt.Sprintf("%s=%v", patch.Path, patch.Value))
	}
	return strings.Join(parts, ", ")
}

func doctorWorkflowSessionFailed(finalState string) bool {
	switch strings.ToLower(strings.TrimSpace(finalState)) {
	case "failed", "failure", "cancelled", "canceled", "token_ceiling_exceeded":
		return true
	default:
		return false
	}
}

func doctorWorkflowUsageFailed(outcome string) bool {
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "failed", "failure", "cancelled", "canceled", "token_ceiling_exceeded", "error":
		return true
	default:
		return false
	}
}

func doctorWorkflowRepeatedSessionGuardIncidents(incidents []doctorWorkflowSessionGuardIncident) []doctorWorkflowSessionGuardIncident {
	repeated := make([]doctorWorkflowSessionGuardIncident, 0, len(incidents))
	for _, incident := range incidents {
		if incident.AttemptCount >= 2 {
			repeated = append(repeated, incident)
		}
	}
	return repeated
}

func doctorWorkflowInt64List(values []int64) string {
	unique := map[int64]struct{}{}
	for _, value := range values {
		if value > 0 {
			unique[value] = struct{}{}
		}
	}
	sorted := make([]int64, 0, len(unique))
	for value := range unique {
		sorted = append(sorted, value)
	}
	slices.Sort(sorted)
	if len(sorted) == 0 {
		return "unknown"
	}
	parts := make([]string, 0, len(sorted))
	for _, value := range sorted {
		parts = append(parts, strconv.FormatInt(value, 10))
	}
	return strings.Join(parts, ", ")
}

func doctorWorkflowMultiplierList(values []float64) string {
	unique := map[string]struct{}{}
	for _, value := range values {
		if value > 0 {
			unique[strconv.FormatFloat(value, 'g', -1, 64)] = struct{}{}
		}
	}
	formatted := make([]string, 0, len(unique))
	for value := range unique {
		formatted = append(formatted, "max_session_context_multiplier="+value)
	}
	slices.Sort(formatted)
	if len(formatted) == 0 {
		return "max_session_context_multiplier=unknown"
	}
	return strings.Join(formatted, ", ")
}

func doctorWorkflowBillableTokens(inputTokens int64, cachedInputTokens int64, outputTokens int64, totalTokens int64, model string, pricing budget.PricingTable) int64 {
	inputTokens = doctorNonNegativeInt64(inputTokens)
	cachedInputTokens = min(doctorNonNegativeInt64(cachedInputTokens), inputTokens)
	outputTokens = doctorNonNegativeInt64(outputTokens)
	if inputTokens == 0 && cachedInputTokens == 0 && outputTokens == 0 {
		return doctorNonNegativeInt64(totalTokens)
	}
	observedCost, observedOK := budget.UsageCostUSD(pricing, model, inputTokens, cachedInputTokens, outputTokens)
	estimateCost, estimateOK := budget.UsageCostUSD(pricing, model, doctorWorkflowBudgetEstimateInput, 0, doctorWorkflowBudgetEstimateOutput)
	if observedOK && estimateOK && estimateCost > 0 {
		return int64(math.Round((observedCost / estimateCost) * float64(doctorWorkflowBudgetEstimateBillable)))
	}
	return inputTokens - cachedInputTokens + outputTokens
}

func doctorNonNegativeInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func doctorRatio(numerator int64, denominator int64) float64 {
	if denominator <= 0 {
		return 0
	}
	return doctorRoundedFloat(float64(numerator)/float64(denominator), 4)
}

func doctorRoundedFloat(value float64, places int) float64 {
	scale := math.Pow10(places)
	return math.Round(value*scale) / scale
}

func doctorPercentileInt64(values []int64, percentile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(math.Ceil(percentile*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func doctorPercentileFloat64(values []float64, percentile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	index := int(math.Ceil(percentile*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func doctorMaxInt64(values []int64) int64 {
	var max int64
	for _, value := range values {
		if value > max {
			max = value
		}
	}
	return max
}

func doctorMaxMapValue(values map[string]int64) int64 {
	var max int64
	for _, value := range values {
		if value > max {
			max = value
		}
	}
	return max
}

func doctorMaxMapKey(values map[string]int64) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out string
	var max int64
	for _, key := range keys {
		if values[key] > max {
			out = key
			max = values[key]
		}
	}
	return out
}

func doctorSlowestLane(values map[string][]int64) (string, int64) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var lane string
	var p90 int64
	for _, key := range keys {
		candidate := doctorPercentileInt64(values[key], 0.90)
		if candidate > p90 {
			lane = key
			p90 = candidate
		}
	}
	return lane, p90
}

func doctorRoundUpInt64(value int64, nearest int64) int64 {
	if nearest <= 0 || value <= 0 {
		return value
	}
	return ((value + nearest - 1) / nearest) * nearest
}

func openDoctorSQLiteReadOnly(ctx context.Context, path string) (doctorTelemetryStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("%w: sqlite path is required", errDoctorTelemetryStoreUnavailable)
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", errDoctorTelemetryStoreUnavailable, path)
		}
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%w: %s is a directory", errDoctorTelemetryStoreUnavailable, path)
	}

	db, err := sql.Open("sqlite", doctorSQLiteReadOnlyDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		return nil, doctorSQLitePingError(fmt.Errorf("enable query_only: %w", err), db.Close())
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, doctorSQLitePingError(err, db.Close())
	}
	return db, nil
}

func doctorSQLiteReadOnlyDSN(path string) string {
	values := url.Values{}
	values.Set("mode", "ro")
	values.Set("cache", "shared")
	return "file:" + doctorEscapeSQLiteURIPath(doctorSQLiteURIPath(path)) + "?" + values.Encode()
}

func doctorSQLiteURIPath(path string) string {
	cleaned := filepath.Clean(path)
	uriPath := filepath.ToSlash(cleaned)
	if doctorWindowsDrivePath(uriPath) {
		uriPath = strings.ReplaceAll(cleaned, `\`, "/")
		if !strings.HasPrefix(uriPath, "/") {
			uriPath = "/" + uriPath
		}
	}
	return uriPath
}

func doctorWindowsDrivePath(path string) bool {
	return len(path) >= 2 && path[1] == ':' &&
		(path[0] >= 'A' && path[0] <= 'Z' || path[0] >= 'a' && path[0] <= 'z')
}

func doctorEscapeSQLiteURIPath(path string) string {
	parts := strings.Split(path, "/")
	for index, part := range parts {
		parts[index] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func confirmDoctorWorkflowOptimizationWrite(cmd *cobra.Command, report doctorWorkflowOptimizationReport) error {
	patches := doctorWorkflowOptimizationPatchesByWorkflow(report)
	if len(patches) == 0 {
		return nil
	}
	total := 0
	for _, patchSet := range patches {
		total += len(patchSet)
	}
	if cmd == nil {
		return errors.New("workflow optimization write confirmation requires a command")
	}
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Apply %d workflow optimization patch(es) to %d WORKFLOW.md file(s)? Type yes to continue: ", total, len(patches)); err != nil {
		return err
	}
	answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.ToLower(strings.TrimSpace(answer)) != "yes" {
		return errors.New("workflow optimization write cancelled")
	}
	return nil
}

func writeDoctorWorkflowOptimizationPatches(report doctorWorkflowOptimizationReport) ([]string, error) {
	patches := doctorWorkflowOptimizationPatchesByWorkflow(report)
	if len(patches) == 0 {
		return nil, nil
	}

	paths := make([]string, 0, len(patches))
	for path := range patches {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	written := make([]string, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read workflow %s: %w", path, err)
		}
		updated, err := doctorApplyWorkflowOptimizationPatches(raw, patches[path])
		if err != nil {
			return nil, fmt.Errorf("patch workflow %s: %w", path, err)
		}
		workflow, err := workflowconfig.ParseWorkflow(updated)
		if err != nil {
			return nil, fmt.Errorf("validate patched workflow %s: %w", path, err)
		}
		if err := workflow.Config.Validate(); err != nil {
			return nil, fmt.Errorf("validate patched workflow %s: %w", path, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("stat workflow %s: %w", path, err)
		}
		if bytes.Equal(raw, updated) {
			continue
		}
		if err := os.WriteFile(path, updated, info.Mode().Perm()); err != nil { // #nosec G703 -- path comes from the operator-selected workflow files already read and validated above.
			return nil, fmt.Errorf("write workflow %s: %w", path, err)
		}
		written = append(written, path)
	}
	return written, nil
}

func doctorWorkflowOptimizationPatchesByWorkflow(report doctorWorkflowOptimizationReport) map[string][]doctorWorkflowOptimizationPatch {
	out := map[string][]doctorWorkflowOptimizationPatch{}
	seen := map[string]map[string]struct{}{}
	for _, finding := range report.Findings {
		workflowPath := strings.TrimSpace(finding.WorkflowPath)
		if workflowPath == "" {
			continue
		}
		if seen[workflowPath] == nil {
			seen[workflowPath] = map[string]struct{}{}
		}
		for _, patch := range finding.Patch {
			patch.Path = strings.TrimSpace(patch.Path)
			if patch.Path == "" {
				continue
			}
			if _, ok := seen[workflowPath][patch.Path]; ok {
				continue
			}
			out[workflowPath] = append(out[workflowPath], patch)
			seen[workflowPath][patch.Path] = struct{}{}
		}
	}
	return out
}

func doctorWorkflowOptimizationDiff(report doctorWorkflowOptimizationReport) string {
	patches := doctorWorkflowOptimizationPatchesByWorkflow(report)
	if len(patches) == 0 {
		return ""
	}
	paths := make([]string, 0, len(patches))
	for path := range patches {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	var builder strings.Builder
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		updated, err := doctorApplyWorkflowOptimizationPatches(raw, patches[path])
		if err != nil || bytes.Equal(raw, updated) {
			continue
		}
		before, _, err := doctorSplitWorkflowFrontmatter(raw)
		if err != nil {
			continue
		}
		after, _, err := doctorSplitWorkflowFrontmatter(updated)
		if err != nil {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(doctorFrontmatterDiff(path, before, after))
	}
	return strings.TrimRight(builder.String(), "\n")
}

func doctorFrontmatterDiff(path string, before []byte, after []byte) string {
	var builder strings.Builder
	builder.WriteString("--- ")
	builder.WriteString(path)
	builder.WriteByte('\n')
	builder.WriteString("+++ ")
	builder.WriteString(path)
	builder.WriteByte('\n')
	builder.WriteString("@@ workflow frontmatter @@\n")
	for _, line := range strings.Split(strings.TrimRight(string(before), "\n"), "\n") {
		if line == "" {
			continue
		}
		builder.WriteByte('-')
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	for _, line := range strings.Split(strings.TrimRight(string(after), "\n"), "\n") {
		if line == "" {
			continue
		}
		builder.WriteByte('+')
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	return builder.String()
}

func doctorApplyWorkflowOptimizationPatches(raw []byte, patches []doctorWorkflowOptimizationPatch) ([]byte, error) {
	frontmatter, prompt, err := doctorSplitWorkflowFrontmatter(raw)
	if err != nil {
		return nil, err
	}

	root, err := doctorWorkflowFrontmatterRoot(frontmatter)
	if err != nil {
		return nil, err
	}
	for _, patch := range patches {
		if err := doctorApplyWorkflowOptimizationPatch(root, patch); err != nil {
			return nil, err
		}
	}

	frontmatter, err = yaml.Marshal(root)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString("---\n")
	out.Write(bytes.TrimRight(frontmatter, "\n"))
	out.WriteString("\n---\n")
	out.Write(prompt)
	return out.Bytes(), nil
}

func doctorSplitWorkflowFrontmatter(raw []byte) ([]byte, []byte, error) {
	normalized := strings.ReplaceAll(strings.TrimPrefix(string(raw), "\ufeff"), "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return nil, nil, errors.New("missing YAML frontmatter")
	}
	body := normalized[len("---\n"):]
	if strings.HasPrefix(body, "---\n") {
		return []byte{}, []byte(body[len("---\n"):]), nil
	}
	if body == "---" {
		return []byte{}, []byte{}, nil
	}
	before, after, ok := strings.Cut(body, "\n---\n")
	if ok {
		return []byte(before), []byte(after), nil
	}
	if before, ok := strings.CutSuffix(body, "\n---"); ok {
		return []byte(before), []byte{}, nil
	}
	return nil, nil, errors.New("unterminated YAML frontmatter")
}

func doctorWorkflowFrontmatterRoot(frontmatter []byte) (*yaml.Node, error) {
	if len(bytes.TrimSpace(frontmatter)) == 0 {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(frontmatter, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, errors.New("workflow frontmatter must be a YAML document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("workflow frontmatter must be a mapping")
	}
	return root, nil
}

func doctorApplyWorkflowOptimizationPatch(root *yaml.Node, patch doctorWorkflowOptimizationPatch) error {
	path := strings.TrimSpace(patch.Path)
	switch path {
	case "agent.max_session_tokens", "agent.max_session_context_multiplier", "agent.auto_promote.rework_limit", "gate.validator.model", "polling.interval_ms", "budget.per_issue_max_usd":
		return doctorSetSimpleYAMLPath(root, strings.Split(patch.Path, "."), patch.Value)
	case "agents.routes.default.model":
		return doctorSetDefaultRouteModel(root, fmt.Sprint(patch.Value))
	default:
		if index, ok := doctorWorkflowRouteModelPatchIndex(path); ok {
			return doctorSetRouteModel(root, index, fmt.Sprint(patch.Value))
		}
		return fmt.Errorf("unsupported workflow optimization patch path %q", patch.Path)
	}
}

func doctorSetSimpleYAMLPath(root *yaml.Node, path []string, value any) error {
	if len(path) == 0 {
		return errors.New("empty YAML path")
	}
	current := root
	for _, key := range path[:len(path)-1] {
		current = doctorEnsureYAMLMapping(current, key)
	}
	doctorSetYAMLMappingValue(current, path[len(path)-1], doctorYAMLScalar(value))
	return nil
}

func doctorSetDefaultRouteModel(root *yaml.Node, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("default route model is required")
	}
	agents := doctorEnsureYAMLMapping(root, "agents")
	routes := doctorYAMLMappingValue(agents, "routes")
	if routes == nil || routes.Kind != yaml.SequenceNode {
		routes = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		doctorSetYAMLMappingValue(agents, "routes", routes)
	}
	for _, route := range routes.Content {
		if route.Kind != yaml.MappingNode {
			continue
		}
		if doctorYAMLRouteIsDefault(route) {
			doctorSetYAMLMappingValue(route, "model", doctorYAMLScalar(model))
			return nil
		}
	}
	routes.Content = append(routes.Content, doctorYAMLMapping(map[string]any{
		"name":    "default",
		"backend": workflowconfig.DefaultAgentBackendID,
		"default": true,
		"model":   model,
	}))
	return nil
}

func doctorWorkflowRouteModelPatchIndex(path string) (int, bool) {
	path = strings.TrimSpace(path)
	const prefix = "agents.routes["
	const suffix = "].model"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return 0, false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	index, err := strconv.Atoi(value)
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

func doctorSetRouteModel(root *yaml.Node, index int, model string) error {
	agents := doctorYAMLMappingValue(root, "agents")
	if agents == nil || agents.Kind != yaml.MappingNode {
		return errors.New("agents routes are not configured")
	}
	routes := doctorYAMLMappingValue(agents, "routes")
	if routes == nil || routes.Kind != yaml.SequenceNode {
		return errors.New("agents.routes are not configured")
	}
	if index < 0 || index >= len(routes.Content) {
		return fmt.Errorf("agents.routes[%d] does not exist", index)
	}
	route := routes.Content[index]
	if route.Kind != yaml.MappingNode {
		return fmt.Errorf("agents.routes[%d] is not a mapping", index)
	}
	doctorSetYAMLMappingValue(route, "model", doctorYAMLScalar(strings.TrimSpace(model)))
	return nil
}

func doctorYAMLRouteIsDefault(route *yaml.Node) bool {
	defaultValue := doctorYAMLMappingValue(route, "default")
	if defaultValue == nil || defaultValue.Kind != yaml.ScalarNode || defaultValue.Value != "true" {
		return false
	}
	role := doctorYAMLMappingValue(route, "role")
	return role == nil || strings.TrimSpace(role.Value) == "" || strings.EqualFold(strings.TrimSpace(role.Value), "code")
}

func doctorEnsureYAMLMapping(parent *yaml.Node, key string) *yaml.Node {
	value := doctorYAMLMappingValue(parent, key)
	if value != nil && value.Kind == yaml.MappingNode {
		return value
	}
	value = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	doctorSetYAMLMappingValue(parent, key, value)
	return value
}

func doctorYAMLMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func doctorSetYAMLMappingValue(mapping *yaml.Node, key string, value *yaml.Node) {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, doctorYAMLScalar(key), value)
}

func doctorYAMLMapping(values map[string]any) *yaml.Node {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, key := range keys {
		node.Content = append(node.Content, doctorYAMLScalar(key), doctorYAMLScalar(values[key]))
	}
	return node
}

func doctorYAMLScalar(value any) *yaml.Node {
	node := &yaml.Node{Kind: yaml.ScalarNode}
	switch typed := value.(type) {
	case bool:
		node.Tag = "!!bool"
		node.Value = strconv.FormatBool(typed)
	case int:
		node.Tag = "!!int"
		node.Value = strconv.Itoa(typed)
	case int64:
		node.Tag = "!!int"
		node.Value = strconv.FormatInt(typed, 10)
	case float64:
		node.Tag = "!!float"
		node.Value = strconv.FormatFloat(typed, 'f', -1, 64)
	case string:
		node.Tag = "!!str"
		node.Value = typed
	default:
		node.Tag = "!!str"
		node.Value = fmt.Sprint(typed)
	}
	return node
}
