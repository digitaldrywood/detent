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

	doctorWorkflowRuleRunawaySessionTokens = "runaway_session_tokens"
	doctorWorkflowRuleReworkLaps           = "rework_laps"
	doctorWorkflowRuleValidatorModel       = "validator_model"
	doctorWorkflowRuleEmptyModelTelemetry  = "empty_model_telemetry"
	doctorWorkflowRuleBudgetEstimateDrift  = "budget_estimate_drift"
	doctorWorkflowRuleSchedulerSkipRate    = "scheduler_skip_rate"

	doctorWorkflowRunawayMedianMultiplier = 4.0
	doctorWorkflowReworkLapThreshold      = 1
	doctorWorkflowRecentSessionLimit      = 50
	doctorWorkflowRecentSessionWindow     = 24 * time.Hour
	doctorWorkflowEmptyModelMinFraction   = 0.20
	doctorWorkflowBudgetEstimateInput     = int64(150_000)
	doctorWorkflowBudgetEstimateOutput    = int64(20_000)
	doctorWorkflowBudgetEstimateTotal     = doctorWorkflowBudgetEstimateInput + doctorWorkflowBudgetEstimateOutput
	doctorWorkflowBudgetEstimateBillable  = doctorWorkflowBudgetEstimateInput + doctorWorkflowBudgetEstimateOutput
	doctorWorkflowBudgetDriftRatio        = 1.50
	doctorWorkflowSchedulerMinDecisions   = int64(5)
	doctorWorkflowSchedulerHighSkipRate   = 0.50
	doctorWorkflowValidatorModel          = "gpt-5.4-mini"
)

var errDoctorTelemetryStoreUnavailable = errors.New("telemetry store unavailable")

type doctorWorkflowOptimizationReport struct {
	StorePath string                                    `json:"store_path,omitempty"`
	Projects  []doctorWorkflowOptimizationProjectReport `json:"projects,omitempty"`
	Findings  []doctorWorkflowOptimizationFinding       `json:"findings,omitempty"`
	Diff      string                                    `json:"diff,omitempty"`
	Written   []string                                  `json:"written,omitempty"`
}

type doctorWorkflowOptimizationProjectReport struct {
	ProjectID    string                            `json:"project_id"`
	WorkflowPath string                            `json:"workflow_path,omitempty"`
	Metrics      doctorWorkflowOptimizationMetrics `json:"metrics"`
	Error        string                            `json:"error,omitempty"`
}

type doctorWorkflowOptimizationMetrics struct {
	SessionCount                     int64   `json:"session_count"`
	UsageEventCount                  int64   `json:"usage_event_count"`
	InputTokens                      int64   `json:"input_tokens"`
	CachedInputTokens                int64   `json:"cached_input_tokens"`
	OutputTokens                     int64   `json:"output_tokens"`
	TotalTokens                      int64   `json:"total_tokens"`
	InputOutputRatio                 float64 `json:"input_output_ratio"`
	CacheReadFraction                float64 `json:"cache_read_fraction"`
	MedianSessionTokens              int64   `json:"median_session_tokens"`
	P90SessionTokens                 int64   `json:"p90_session_tokens"`
	MaxSessionTokens                 int64   `json:"max_session_tokens"`
	MaxSessionsPerIssue              int64   `json:"max_sessions_per_issue"`
	MaxSessionsIssue                 string  `json:"max_sessions_issue,omitempty"`
	RecentSessionCount               int64   `json:"recent_session_count"`
	EmptyModelRecentSessions         int64   `json:"empty_model_recent_sessions"`
	EmptyModelRecentFraction         float64 `json:"empty_model_recent_fraction"`
	MaxReworkLapsPerIssue            int64   `json:"max_rework_laps_per_issue"`
	MaxReworkLapsIssue               string  `json:"max_rework_laps_issue,omitempty"`
	FailureTokens                    int64   `json:"failure_tokens"`
	SchedulerDecisionCount           int64   `json:"scheduler_decision_count"`
	SchedulerSkippedDecisions        int64   `json:"scheduler_skipped_decisions"`
	SchedulerSkipRate                float64 `json:"scheduler_skip_rate"`
	LaneEventCount                   int64   `json:"lane_event_count"`
	LaneDwellP90Seconds              int64   `json:"lane_dwell_p90_seconds"`
	SlowestLane                      string  `json:"slowest_lane,omitempty"`
	SlowestLaneP90Seconds            int64   `json:"slowest_lane_p90_seconds"`
	BudgetEstimateTokens             int64   `json:"budget_estimate_tokens"`
	BudgetEstimateBillableTokens     int64   `json:"budget_estimate_billable_tokens"`
	P90SessionBillableTokens         int64   `json:"p90_session_billable_tokens"`
	BudgetEstimateBillableDriftRatio float64 `json:"budget_estimate_billable_drift_ratio"`
	BudgetEstimateDriftRatio         float64 `json:"budget_estimate_drift_ratio"`
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
	failureTokens            int64
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

func (r *doctorWorkflowOptimizationReport) Merge(next doctorWorkflowOptimizationReport) {
	if next.StorePath != "" {
		r.StorePath = next.StorePath
	}
	r.Projects = append(r.Projects, next.Projects...)
	r.Findings = append(r.Findings, next.Findings...)
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
	includeDiff bool,
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
	report, err := doctorWorkflowOptimization(ctx, db, storePath, cfg, deps, runtimeGlobalGitHubToken(githubToken), includeDiff)
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
		Detail:               fmt.Sprintf("0 findings across %d project(s)", len(report.Projects)),
		WorkflowOptimization: report,
	}
	if len(report.Findings) == 0 {
		return check
	}

	check.Status = doctorWarn
	check.Detail = fmt.Sprintf("%d finding(s) across %d project(s); estimated token impact %d", len(report.Findings), len(report.Projects), doctorWorkflowEstimatedImpact(report.Findings))
	check.Hint = "Review detent doctor --diff, then rerun with --write and confirm to apply frontmatter patches."
	return check
}

func doctorWorkflowOptimization(
	ctx context.Context,
	db doctorTelemetryStore,
	storePath string,
	cfg globalconfig.Config,
	deps doctorDeps,
	runtimeGitHubToken string,
	includeDiff bool,
) (doctorWorkflowOptimizationReport, error) {
	report := doctorWorkflowOptimizationReport{StorePath: storePath}
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

		metrics, err := doctorWorkflowOptimizationMetricsForProject(ctx, db, projectID, workflow.Config)
		if err != nil {
			return doctorWorkflowOptimizationReport{}, err
		}
		report.Projects = append(report.Projects, doctorWorkflowOptimizationProjectReport{
			ProjectID:    projectID,
			WorkflowPath: workflowPath,
			Metrics:      metrics,
		})
		report.Findings = append(report.Findings, doctorWorkflowOptimizationFindings(projectID, workflowPath, workflow.Config, metrics)...)
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
	if includeDiff {
		report.Diff = doctorWorkflowOptimizationDiff(report)
	}
	return report, nil
}

func doctorWorkflowOptimizationMetricsForProject(
	ctx context.Context,
	db doctorTelemetryStore,
	projectID string,
	cfg workflowconfig.Config,
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

	metrics := doctorWorkflowOptimizationMetrics{
		SessionCount:                 sessions.count,
		UsageEventCount:              usage.count,
		InputTokens:                  sessions.inputTokens,
		CachedInputTokens:            sessions.cachedInputTokens,
		OutputTokens:                 sessions.outputTokens,
		TotalTokens:                  sessions.totalTokens,
		MedianSessionTokens:          doctorPercentileInt64(sessions.totalTokensBySession, 0.50),
		P90SessionTokens:             doctorPercentileInt64(sessions.totalTokensBySession, 0.90),
		P90SessionBillableTokens:     doctorPercentileInt64(sessions.billableTokensBySession, 0.90),
		MaxSessionTokens:             doctorMaxInt64(sessions.totalTokensBySession),
		RecentSessionCount:           sessions.recentSessionCount,
		EmptyModelRecentSessions:     sessions.emptyModelRecentSessions,
		MaxSessionsPerIssue:          doctorMaxMapValue(sessions.issueSessionCounts),
		MaxSessionsIssue:             doctorMaxMapKey(sessions.issueSessionCounts),
		FailureTokens:                sessions.failureTokens,
		SchedulerDecisionCount:       scheduler.count,
		SchedulerSkippedDecisions:    scheduler.skipped,
		LaneEventCount:               lanes.count,
		LaneDwellP90Seconds:          doctorPercentileInt64(lanes.durations, 0.90),
		MaxReworkLapsPerIssue:        doctorMaxMapValue(lanes.reworkLaps),
		MaxReworkLapsIssue:           doctorMaxMapKey(lanes.reworkLaps),
		BudgetEstimateTokens:         doctorWorkflowBudgetEstimateTotal,
		BudgetEstimateBillableTokens: doctorWorkflowBudgetEstimateBillable,
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

func doctorWorkflowSessionTelemetry(ctx context.Context, db doctorTelemetryStore, projectID string, pricing budget.PricingTable) (doctorWorkflowSessionMetrics, error) {
	rows, err := db.QueryContext(ctx, `
SELECT
  COALESCE(s.total_tokens, 0),
  COALESCE(s.input_tokens, 0),
  COALESCE(s.cached_input_tokens, 0),
  COALESCE(s.output_tokens, 0),
  COALESCE(s.model, ''),
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
		issueSessionCounts: map[string]int64{},
	}
	var recentCutoff time.Time
	for rows.Next() {
		var totalTokens int64
		var inputTokens int64
		var cachedInputTokens int64
		var outputTokens int64
		var model string
		var finalState string
		var issueKey string
		var completedAtRaw string
		if err := rows.Scan(&totalTokens, &inputTokens, &cachedInputTokens, &outputTokens, &model, &finalState, &issueKey, &completedAtRaw); err != nil {
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
		metrics.issueSessionCounts[issueKey]++
		if metrics.count == 1 {
			recentCutoff = completedAt.Add(-doctorWorkflowRecentSessionWindow)
		}
		if metrics.recentSessionCount < doctorWorkflowRecentSessionLimit && !completedAt.Before(recentCutoff) {
			metrics.recentSessionCount++
			if strings.TrimSpace(model) == "" {
				metrics.emptyModelRecentSessions++
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
	if metrics.SessionCount >= 3 && metrics.MedianSessionTokens > 0 {
		ratio := float64(metrics.MaxSessionTokens) / float64(metrics.MedianSessionTokens)
		if ratio >= doctorWorkflowRunawayMedianMultiplier {
			value := doctorRoundUpInt64(int64(float64(metrics.MedianSessionTokens)*doctorWorkflowRunawayMedianMultiplier), 1000)
			findings = append(findings, doctorWorkflowFinding(projectID, workflowPath, doctorWorkflowRuleRunawaySessionTokens,
				"Runaway session tail",
				fmt.Sprintf("max session tokens are %.1fx the median", ratio),
				metrics.MaxSessionTokens-metrics.MedianSessionTokens,
				map[string]any{
					"session_count":         metrics.SessionCount,
					"median_session_tokens": metrics.MedianSessionTokens,
					"p90_session_tokens":    metrics.P90SessionTokens,
					"max_session_tokens":    metrics.MaxSessionTokens,
					"max_to_median_ratio":   doctorRoundedFloat(ratio, 2),
				},
				doctorWorkflowOptimizationPatch{Path: "agent.max_session_tokens", Value: value},
			))
		}
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
	if metrics.RecentSessionCount > 0 && metrics.EmptyModelRecentSessions > 0 && metrics.EmptyModelRecentFraction >= doctorWorkflowEmptyModelMinFraction {
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
		findings = append(findings, doctorWorkflowFinding(projectID, workflowPath, doctorWorkflowRuleEmptyModelTelemetry,
			"Session model telemetry is incomplete",
			detail,
			0,
			evidence,
		))
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
	return findings
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
		if strings.TrimSpace(route.ModelField) != "" {
			return doctorWorkflowModelConfig{
				Source: "agents.routes.model_field",
			}, true
		}
		backend, ok := backends[strings.TrimSpace(route.Backend)]
		if !ok {
			continue
		}
		if model := doctorWorkflowBackendCommandModel(backend); model != "" {
			return doctorWorkflowModelConfig{
				Model:  model,
				Source: "agents.backends.command",
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
	if out == nil || len(report.Findings) == 0 && strings.TrimSpace(report.Diff) == "" && len(report.Written) == 0 {
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
		if err := os.WriteFile(path, updated, info.Mode().Perm()); err != nil {
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
	switch strings.TrimSpace(patch.Path) {
	case "agent.max_session_tokens", "agent.auto_promote.rework_limit", "gate.validator.model", "polling.interval_ms", "budget.per_issue_max_usd":
		return doctorSetSimpleYAMLPath(root, strings.Split(patch.Path, "."), patch.Value)
	case "agents.routes.default.model":
		return doctorSetDefaultRouteModel(root, fmt.Sprint(patch.Value))
	default:
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
