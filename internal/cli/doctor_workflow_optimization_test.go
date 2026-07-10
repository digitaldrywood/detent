package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/lessons"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestDoctorWorkflowOptimizationFindsFixtureRules(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	paths := seedDoctorWorkflowOptimizationFixture(t)
	db, err := openDoctorSQLiteReadOnly(ctx, paths.db)
	if err != nil {
		t.Fatalf("openDoctorSQLiteReadOnly() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	global := doctorWorkflowOptimizationGlobal(paths)
	deps := successfulDoctorDeps()
	deps.loadWorkflow = workflowconfig.LoadWorkflow
	report, err := doctorWorkflowOptimization(ctx, db, paths.db, global, deps, "", doctorWorkflowOptimizationOptions{IncludeDiff: true})
	if err != nil {
		t.Fatalf("doctorWorkflowOptimization() error = %v", err)
	}

	if len(report.Findings) < 5 {
		t.Fatalf("findings len = %d, want at least 5: %#v", len(report.Findings), report.Findings)
	}
	if strings.TrimSpace(report.Diff) == "" {
		t.Fatal("Diff is blank, want proposed frontmatter patch")
	}
	var pretty bytes.Buffer
	if err := writeDoctorReport(&pretty, doctorReport{WorkflowOptimization: report}); err != nil {
		t.Fatalf("writeDoctorReport() error = %v", err)
	}
	for _, want := range []string{
		"Workflow Optimization",
		"runaway_session_tokens",
		"Evidence:",
		"Suggested patch: agent.max_session_tokens=40000",
		"@@ workflow frontmatter @@",
	} {
		if !strings.Contains(pretty.String(), want) {
			t.Fatalf("pretty report missing %q:\n%s", want, pretty.String())
		}
	}

	runaway := doctorWorkflowFindingByRule(t, report.Findings, doctorWorkflowRuleRunawaySessionTokens)
	if got := evidenceInt64(t, runaway, "max_session_tokens"); got != 300000 {
		t.Fatalf("runaway max_session_tokens = %d, want 300000", got)
	}
	if got := evidenceInt64(t, runaway, "median_session_tokens"); got != 10000 {
		t.Fatalf("runaway median_session_tokens = %d, want 10000", got)
	}
	if got := evidenceFloat64(t, runaway, "max_to_median_ratio"); got != 30 {
		t.Fatalf("runaway max_to_median_ratio = %v, want 30", got)
	}

	rework := doctorWorkflowFindingByRule(t, report.Findings, doctorWorkflowRuleReworkLaps)
	if got := evidenceInt64(t, rework, "max_rework_laps_per_issue"); got != 3 {
		t.Fatalf("rework laps = %d, want 3", got)
	}

	validator := doctorWorkflowFindingByRule(t, report.Findings, doctorWorkflowRuleValidatorModel)
	if got := validator.Patch[0].Value; got != doctorWorkflowValidatorModel {
		t.Fatalf("validator patch value = %#v, want %s", got, doctorWorkflowValidatorModel)
	}

	emptyModel := doctorWorkflowFindingByRule(t, report.Findings, doctorWorkflowRuleEmptyModelTelemetry)
	if got := evidenceInt64(t, emptyModel, "empty_model_recent_sessions"); got != 4 {
		t.Fatalf("empty model recent sessions = %d, want 4", got)
	}
	if got := evidenceFloat64(t, emptyModel, "empty_model_recent_fraction"); got != 0.8 {
		t.Fatalf("empty model fraction = %v, want 0.8", got)
	}

	if doctorWorkflowFindingExists(report.Findings, doctorWorkflowRuleBudgetEstimateDrift) {
		t.Fatalf("budget drift finding should not be emitted for cached-heavy raw token drift: %#v", report.Findings)
	}

	scheduler := doctorWorkflowFindingByRule(t, report.Findings, doctorWorkflowRuleSchedulerSkipRate)
	if got := evidenceFloat64(t, scheduler, "scheduler_skip_rate"); got != 0.67 {
		t.Fatalf("scheduler skip rate = %v, want 0.67", got)
	}
	if len(report.Projects) != 1 {
		t.Fatalf("projects len = %d, want 1", len(report.Projects))
	}
	metrics := report.Projects[0].Metrics
	if metrics.SessionCount != 5 || metrics.UsageEventCount != 5 {
		t.Fatalf("metrics counts = sessions %d usage %d, want 5 and 5", metrics.SessionCount, metrics.UsageEventCount)
	}
	if metrics.MaxSessionTokens != 300000 {
		t.Fatalf("MaxSessionTokens = %d, want 300000", metrics.MaxSessionTokens)
	}
	if metrics.P90SessionBillableTokens != 107320 {
		t.Fatalf("P90SessionBillableTokens = %d, want 107320", metrics.P90SessionBillableTokens)
	}
	if metrics.BudgetEstimateDriftRatio != 0.6313 {
		t.Fatalf("BudgetEstimateDriftRatio = %v, want 0.6313", metrics.BudgetEstimateDriftRatio)
	}
}

func TestDoctorWorkflowOptimizationProposesGovernedSelfImprovement(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	paths := seedDoctorWorkflowOptimizationFixture(t)
	seedDoctorWorkflowRepeatedValidatorFindings(t, paths.db)
	seedDoctorWorkflowRepeatedLessons(t, paths.dir)

	db, err := openDoctorSQLiteReadOnly(ctx, paths.db)
	if err != nil {
		t.Fatalf("openDoctorSQLiteReadOnly() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	global := doctorWorkflowOptimizationGlobal(paths)
	deps := successfulDoctorDeps()
	deps.loadWorkflow = workflowconfig.LoadWorkflow
	report, err := doctorWorkflowOptimization(ctx, db, paths.db, global, deps, "", doctorWorkflowOptimizationOptions{ProposalThreshold: 2})
	if err != nil {
		t.Fatalf("doctorWorkflowOptimization() error = %v", err)
	}

	rework := doctorWorkflowProposalBySignal(t, report.Proposals, "doctor_finding", doctorWorkflowRuleReworkLaps)
	if rework.TargetKind != "workflow" || rework.TargetPath != "agent.auto_promote.rework_limit" || rework.Count != 3 {
		t.Fatalf("rework proposal = %#v", rework)
	}
	if !strings.Contains(rework.Governance, "must not self-apply") || !strings.Contains(rework.IssueBody, "status: pending") {
		t.Fatalf("rework proposal missing governance/outcome:\n%#v\n%s", rework, rework.IssueBody)
	}
	if !strings.Contains(rework.IssueMarker, rework.ID) || !strings.Contains(rework.IssueBody, rework.IssueMarker) {
		t.Fatalf("proposal marker not embedded: marker=%q body=\n%s", rework.IssueMarker, rework.IssueBody)
	}
	var pretty bytes.Buffer
	if err := writeDoctorWorkflowOptimizationPretty(&pretty, doctorWorkflowOptimizationReport{
		Proposals: []doctorWorkflowImprovementProposal{rework},
	}); err != nil {
		t.Fatalf("writeDoctorWorkflowOptimizationPretty() error = %v", err)
	}
	if !strings.Contains(pretty.String(), "Governed Self-Improvement Proposals") || !strings.Contains(pretty.String(), rework.ID) {
		t.Fatalf("proposal-only pretty report missing proposal details:\n%s", pretty.String())
	}

	validator := doctorWorkflowProposalBySignal(t, report.Proposals, "validator_finding", "p1|internal/runner/prompt.go|missing rollback coverage.")
	if validator.TargetKind != "gate" || validator.Count != 2 {
		t.Fatalf("validator proposal = %#v", validator)
	}

	lesson := doctorWorkflowProposalBySignal(t, report.Proposals, "lesson_failure_kind", "token_ceiling_exceeded")
	if lesson.TargetKind != "skill" || lesson.TargetPath != ".detent/skills" || lesson.Count != 2 {
		t.Fatalf("lesson proposal = %#v", lesson)
	}
}

func TestDoctorWorkflowOptimizationCreatesProposalIssuesWithMemoryTracker(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	paths := seedDoctorWorkflowOptimizationFixture(t)
	db, err := openDoctorSQLiteReadOnly(ctx, paths.db)
	if err != nil {
		t.Fatalf("openDoctorSQLiteReadOnly() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	global := doctorWorkflowOptimizationGlobal(paths)
	deps := successfulDoctorDeps()
	deps.loadWorkflow = workflowconfig.LoadWorkflow
	report, err := doctorWorkflowOptimization(ctx, db, paths.db, global, deps, "", doctorWorkflowOptimizationOptions{
		ProposalThreshold: 2,
		ProposeIssues:     true,
	})
	if err != nil {
		t.Fatalf("doctorWorkflowOptimization() error = %v", err)
	}
	if len(report.Proposals) == 0 {
		t.Fatal("proposals len = 0, want governed proposals")
	}
	if len(report.CreatedProposalIssues) != len(report.Proposals) {
		t.Fatalf("created proposal issues len = %d, want %d", len(report.CreatedProposalIssues), len(report.Proposals))
	}
	for _, created := range report.CreatedProposalIssues {
		if created.ProposalID == "" || created.ProjectID != "detent" || created.IssueID == "" || created.Reused {
			t.Fatalf("created proposal issue = %#v", created)
		}
	}
}

func TestDoctorWorkflowOptimizationContinuesWhenProposalStateUpdateBlocked(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerMemory
	proposal := doctorWorkflowFinalizeProposal(doctorWorkflowImprovementProposal{
		ProjectID:       "detent",
		SignalKind:      "doctor_finding",
		Pattern:         "blocked-state-update",
		Count:           2,
		Title:           "Improve blocked state update handling",
		Detail:          "blocked update proposal",
		TargetKind:      "workflow",
		SuggestedChange: "Review blocked update handling.",
	})
	projectConnector := &blockedDoctorProposalConnector{
		Connector: memory.New(memory.Config{Stateful: true}),
	}
	deps := successfulDoctorDeps()
	deps.proposalConnector = func(workflowconfig.Config) (doctorWorkflowProposalConnector, error) {
		return projectConnector, nil
	}

	created, err := createDoctorWorkflowImprovementProposalIssues(ctx, "detent", cfg, deps, []doctorWorkflowImprovementProposal{proposal})
	if err != nil {
		t.Fatalf("createDoctorWorkflowImprovementProposalIssues() error = %v", err)
	}
	if len(created) != 1 || created[0].ProposalID != proposal.ID || created[0].IssueID == "" {
		t.Fatalf("created proposal issue = %#v, want one created issue for %q", created, proposal.ID)
	}
	if projectConnector.updateCalls != 1 {
		t.Fatalf("UpdateIssueState calls = %d, want 1", projectConnector.updateCalls)
	}
}

func TestDoctorWorkflowOptimizationReusesProposalIssueOutsideBacklog(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerMemory
	proposal := doctorWorkflowImprovementProposal{
		ID:          "detent-self-improve-existing",
		ProjectID:   "detent",
		Title:       "Improve repeated finding handling",
		IssueMarker: "<!-- detent:self-improvement proposal_id=detent-self-improve-existing -->",
		IssueBody: "<!-- detent:self-improvement proposal_id=detent-self-improve-existing -->\n\n" +
			"## Outcome\n\n- status: accepted\n",
	}
	existing := connector.Issue{
		ID:          "issue-42",
		Identifier:  "digitaldrywood/detent#42",
		State:       "In Progress",
		Description: proposal.IssueBody,
	}
	var events []memory.Event
	deps := successfulDoctorDeps()
	deps.proposalConnector = func(workflowconfig.Config) (doctorWorkflowProposalConnector, error) {
		return memory.New(memory.Config{
			Issues:    []connector.Issue{existing},
			Stateful:  true,
			EventSink: func(event memory.Event) { events = append(events, event) },
		}), nil
	}

	created, err := createDoctorWorkflowImprovementProposalIssues(ctx, "detent", cfg, deps, []doctorWorkflowImprovementProposal{proposal})
	if err != nil {
		t.Fatalf("createDoctorWorkflowImprovementProposalIssues() error = %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("created len = %d, want 1", len(created))
	}
	if !created[0].Reused || created[0].IssueID != existing.ID || created[0].OutcomeStatus != "accepted" {
		t.Fatalf("created proposal issue = %#v", created[0])
	}
	for _, event := range events {
		if event.Kind == memory.EventKindIssueCreate {
			t.Fatalf("created duplicate proposal issue event: %#v", event)
		}
	}
}

func TestDoctorWorkflowOptimizationFindingsTokenImpactIsAvoidable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ruleID  string
		metrics doctorWorkflowOptimizationMetrics
	}{
		{
			name:   "empty model telemetry does not charge historical usage",
			ruleID: doctorWorkflowRuleEmptyModelTelemetry,
			metrics: doctorWorkflowOptimizationMetrics{
				TotalTokens:              4_720_170_000,
				RecentSessionCount:       50,
				EmptyModelRecentSessions: 48,
				EmptyModelRecentFraction: 0.96,
			},
		},
		{
			name:   "scheduler skips do not charge full agent sessions",
			ruleID: doctorWorkflowRuleSchedulerSkipRate,
			metrics: doctorWorkflowOptimizationMetrics{
				MedianSessionTokens:       7_710_440,
				SchedulerDecisionCount:    1_704,
				SchedulerSkippedDecisions: 1_136,
				SchedulerSkipRate:         0.67,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := workflowconfig.Config{}
			if tt.ruleID == doctorWorkflowRuleEmptyModelTelemetry {
				cfg.Budget.Enabled = true
			}
			findings := doctorWorkflowOptimizationFindings("detent", "WORKFLOW.md", cfg, tt.metrics)
			finding := doctorWorkflowFindingByRule(t, findings, tt.ruleID)
			if finding.EstimatedTokenImpact != 0 {
				t.Fatalf("EstimatedTokenImpact = %d, want 0", finding.EstimatedTokenImpact)
			}
		})
	}
}

func TestDoctorWorkflowOptimizationReviewFlowFindings(t *testing.T) {
	t.Parallel()

	autopilot := workflowconfig.Default()
	autopilot.Agent.AutoPromote.Enabled = true
	autopilot.Agent.AutoPromote.QuietSeconds = 0
	autopilot.Agent.AutoPromote.GateWaitState = workflowconfig.AutoPromoteGateWaitStateSource

	reviewGate := workflowconfig.Default()
	reviewGate.Agent.AutoPromote.Enabled = true
	reviewGate.Agent.AutoPromote.QuietSeconds = 600
	reviewGate.Agent.AutoPromote.GateWaitState = workflowconfig.AutoPromoteGateWaitStateReview

	tests := []struct {
		name      string
		cfg       workflowconfig.Config
		metrics   doctorWorkflowOptimizationMetrics
		wantRules []string
	}{
		{
			name: "autopilot clean",
			cfg:  autopilot,
			metrics: doctorWorkflowOptimizationMetrics{
				ReviewEntryCount: 1,
			},
		},
		{
			name: "autopilot repeated review entry mismatch",
			cfg:  autopilot,
			metrics: doctorWorkflowOptimizationMetrics{
				ReviewEntryCount:      2,
				ReviewEntryIssue:      "digitaldrywood/detent#981",
				ReviewEntryIssueCount: 2,
			},
			wantRules: []string{doctorWorkflowRuleReviewFlowBehaviorMismatch},
		},
		{
			name: "review gate review entries are normal",
			cfg:  reviewGate,
			metrics: doctorWorkflowOptimizationMetrics{
				ReviewEntryCount:      3,
				ReviewEntryIssue:      "digitaldrywood/detent#415",
				ReviewEntryIssueCount: 3,
			},
		},
		{
			name: "invalid workpad status recurrence",
			cfg:  autopilot,
			metrics: doctorWorkflowOptimizationMetrics{
				InvalidWorkpadStatusDecisions:  2,
				InvalidWorkpadStatusIssue:      "digitaldrywood/detent#981",
				InvalidWorkpadStatusIssueCount: 2,
			},
			wantRules: []string{doctorWorkflowRuleInvalidWorkpadStatusRecurrence},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			findings := doctorWorkflowOptimizationFindings("detent", "WORKFLOW.md", tt.cfg, tt.metrics)
			for _, rule := range tt.wantRules {
				finding := doctorWorkflowFindingByRule(t, findings, rule)
				if got := finding.Evidence["review_flow"]; got != doctorReviewFlowChoice(tt.cfg) {
					t.Fatalf("%s review_flow evidence = %#v, want %s", rule, got, doctorReviewFlowChoice(tt.cfg))
				}
			}
			for _, finding := range findings {
				if finding.RuleID != doctorWorkflowRuleReviewFlowBehaviorMismatch &&
					finding.RuleID != doctorWorkflowRuleInvalidWorkpadStatusRecurrence {
					continue
				}
				if !slices.Contains(tt.wantRules, finding.RuleID) {
					t.Fatalf("unexpected review-flow finding %s in %#v", finding.RuleID, findings)
				}
			}
		})
	}
}

func TestDoctorReviewFlowPhraseMatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		text    string
		phrases []string
		want    int
	}{
		{
			name:    "affirmative enter review instruction",
			text:    "When done, move the issue to Human Review.",
			phrases: doctorReviewFlowEnterReviewPhrases("Human Review"),
			want:    1,
		},
		{
			name:    "do not enter review instruction",
			text:    "Do NOT move the issue to Human Review.",
			phrases: doctorReviewFlowEnterReviewPhrases("Human Review"),
		},
		{
			name:    "never enter review instruction",
			text:    "Never move the issue to Human Review.",
			phrases: doctorReviewFlowEnterReviewPhrases("Human Review"),
		},
		{
			name:    "negated skip review instruction",
			text:    "Do not skip Human Review.",
			phrases: doctorReviewFlowSkipReviewPhrases("Human Review"),
		},
		{
			name:    "negated direct promotion instruction",
			text:    "Do not promote the issue directly to Merging.",
			phrases: doctorReviewFlowDirectPromotePhrases("Merging"),
		},
		{
			name: "mixed document",
			text: "Do not move the issue to Human Review.\n" +
				"When explicitly requested, move the issue to Human Review.",
			phrases: doctorReviewFlowEnterReviewPhrases("Human Review"),
			want:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := doctorReviewFlowPhraseMatches(tt.text, tt.phrases); got != tt.want {
				t.Fatalf("doctorReviewFlowPhraseMatches() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDoctorWorkflowReviewFlowTelemetryCountsImmediateReviewEntries(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "detent.db")
	backend, err := store.Open(ctx, store.Config{
		Backend: store.BackendSQLite,
		Path:    dbPath,
	})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}

	base := time.Date(2026, 7, 9, 16, 0, 0, 0, time.UTC)
	issue := "digitaldrywood/detent#981"
	for index := range 2 {
		startedAt := base.Add(time.Duration(index) * time.Hour)
		completedAt := startedAt.Add(5 * time.Minute)
		sessionID, err := backend.StartSession(ctx, store.SessionStart{
			Identifier: issue,
			StartedAt:  startedAt,
			Model:      "gpt-5-codex",
		})
		if err != nil {
			t.Fatalf("StartSession() error = %v", err)
		}
		if err := backend.FinishSession(ctx, sessionID, store.SessionFinish{
			CompletedAt:       completedAt,
			Turns:             1,
			InputTokens:       900,
			CachedInputTokens: 450,
			OutputTokens:      100,
			TotalTokens:       1000,
			RuntimeSeconds:    300,
			FinalState:        "completed",
			Model:             "gpt-5-codex",
		}); err != nil {
			t.Fatalf("FinishSession() error = %v", err)
		}
		if _, err := backend.RecordUsageEvent(ctx, store.UsageEvent{
			ProjectID:         "detent",
			SessionID:         sessionID,
			Identifier:        issue,
			Model:             "gpt-5-codex",
			InputTokens:       900,
			CachedInputTokens: 450,
			OutputTokens:      100,
			TotalTokens:       1000,
			RuntimeSeconds:    300,
			StartedAt:         startedAt,
			FinishedAt:        completedAt,
			Outcome:           "completed",
		}); err != nil {
			t.Fatalf("RecordUsageEvent() error = %v", err)
		}
		if _, err := backend.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
			ProjectID:         "detent",
			Identifier:        issue,
			PhaseType:         store.WorkflowPhaseTypeLane,
			PhaseName:         "Human Review",
			PreviousPhaseName: "In Progress",
			Status:            "entered",
			StartedAt:         completedAt.Add(2 * time.Minute),
			Reason:            "completed_active_review_transition",
		}); err != nil {
			t.Fatalf("RecordWorkflowPhaseEvent(review) error = %v", err)
		}
		if _, err := backend.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
			ProjectID:         "detent",
			Identifier:        issue,
			PhaseType:         store.WorkflowPhaseTypeLane,
			PhaseName:         "Rework",
			PreviousPhaseName: "Human Review",
			Status:            "entered",
			StartedAt:         completedAt.Add(4 * time.Minute),
			Reason:            "workpad_status_invalid",
		}); err != nil {
			t.Fatalf("RecordWorkflowPhaseEvent(invalid) error = %v", err)
		}
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	db, err := openDoctorSQLiteReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatalf("openDoctorSQLiteReadOnly() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	cfg := workflowconfig.Default()
	cfg.Agent.AutoPromote.Enabled = true
	cfg.Agent.AutoPromote.QuietSeconds = 0
	cfg.Agent.AutoPromote.GateWaitState = workflowconfig.AutoPromoteGateWaitStateSource
	metrics, err := doctorWorkflowOptimizationMetricsForProject(ctx, db, "detent", cfg)
	if err != nil {
		t.Fatalf("doctorWorkflowOptimizationMetricsForProject() error = %v", err)
	}
	if metrics.ReviewEntryCount != 2 || metrics.ReviewEntryIssue != issue {
		t.Fatalf("review entry metrics = count %d issue %q, want 2/%s", metrics.ReviewEntryCount, metrics.ReviewEntryIssue, issue)
	}
	if metrics.InvalidWorkpadStatusDecisions != 2 || metrics.InvalidWorkpadStatusIssue != issue {
		t.Fatalf("invalid status metrics = count %d issue %q, want 2/%s", metrics.InvalidWorkpadStatusDecisions, metrics.InvalidWorkpadStatusIssue, issue)
	}
	findings := doctorWorkflowOptimizationFindings("detent", "WORKFLOW.md", cfg, metrics)
	doctorWorkflowFindingByRule(t, findings, doctorWorkflowRuleReviewFlowBehaviorMismatch)
	doctorWorkflowFindingByRule(t, findings, doctorWorkflowRuleInvalidWorkpadStatusRecurrence)
}

func TestDoctorWorkflowOptimizationRunawaySessionTokensRespectsConfiguredCap(t *testing.T) {
	t.Parallel()

	metrics := doctorWorkflowOptimizationMetrics{
		SessionCount:        3,
		MedianSessionTokens: 10000,
		P90SessionTokens:    120000,
		MaxSessionTokens:    300000,
	}
	tests := []struct {
		name          string
		configuredCap int64
		wantFinding   bool
	}{
		{
			name:        "uncapped",
			wantFinding: true,
		},
		{
			name:          "configured cap above tolerance",
			configuredCap: 51000,
			wantFinding:   true,
		},
		{
			name:          "configured cap within median drift tolerance",
			configuredCap: 50000,
		},
		{
			name:          "configured cap equals suggested cap",
			configuredCap: 40000,
		},
		{
			name:          "configured cap below suggested cap",
			configuredCap: 30000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := workflowconfig.Default()
			cfg.Agent.MaxSessionTokens = tt.configuredCap

			findings := doctorWorkflowOptimizationFindings("detent", "WORKFLOW.md", cfg, metrics)
			gotFinding := doctorWorkflowFindingExists(findings, doctorWorkflowRuleRunawaySessionTokens)
			if gotFinding != tt.wantFinding {
				t.Fatalf("runaway finding exists = %v, want %v: %#v", gotFinding, tt.wantFinding, findings)
			}
			if !tt.wantFinding {
				return
			}

			runaway := doctorWorkflowFindingByRule(t, findings, doctorWorkflowRuleRunawaySessionTokens)
			if got := runaway.Patch[0].Value; got != int64(40000) {
				t.Fatalf("runaway patch value = %#v, want 40000", got)
			}
		})
	}
}

func TestDoctorWorkflowOptimizationFlagsMissingSessionBrake(t *testing.T) {
	t.Parallel()

	findings := doctorWorkflowOptimizationFindings("detent", "WORKFLOW.md", workflowconfig.Config{}, doctorWorkflowOptimizationMetrics{
		SessionCount:        3,
		MedianSessionTokens: 10_000,
	})
	if !doctorWorkflowFindingExists(findings, "no_session_token_brake") {
		t.Fatalf("findings = %#v, want no_session_token_brake", findings)
	}
}

func TestDoctorWorkflowOptimizationModelAndSessionGuardConfigurations(t *testing.T) {
	t.Parallel()

	pinnedConfig := func(model string) workflowconfig.Config {
		cfg := workflowconfig.Config{}
		cfg.Agent.MaxSessionTokens = 2_000_000
		cfg.Agents.Routes = []workflowconfig.AgentRoute{{
			Name:    "default",
			Backend: workflowconfig.DefaultAgentBackendID,
			Model:   model,
			Default: true,
		}}
		return cfg
	}
	unpinnedConfig := func() workflowconfig.Config {
		cfg := workflowconfig.Config{}
		cfg.Agent.MaxSessionTokens = 2_000_000
		return cfg
	}
	observed := doctorWorkflowObservedDefaultModel{Model: "gpt-5.6-sol", SessionCount: 6, Major: 5, Minor: 6}
	tests := []struct {
		name         string
		cfg          workflowconfig.Config
		metrics      doctorWorkflowOptimizationMetrics
		wantMode     string
		wantRules    []string
		wantNoRules  []string
		wantDetail   []string
		wantSeverity map[string]string
	}{
		{
			name:        "pinned healthy",
			cfg:         pinnedConfig("gpt-5.6-sol"),
			wantMode:    "pinned",
			wantNoRules: []string{doctorWorkflowRulePinnedWorkerModelStale, doctorWorkflowRuleNoSessionTokenBrake},
		},
		{
			name:       "pinned stale",
			cfg:        pinnedConfig("gpt-5.5"),
			wantMode:   "pinned",
			wantRules:  []string{doctorWorkflowRulePinnedWorkerModelStale},
			wantDetail: []string{"gpt-5.5", "gpt-5.6-sol", "keep, update, or remove"},
		},
		{
			name: "unpinned",
			cfg: func() workflowconfig.Config {
				cfg := unpinnedConfig()
				cfg.Budget.Enabled = true
				return cfg
			}(),
			metrics: doctorWorkflowOptimizationMetrics{
				RecentSessionCount:       5,
				EmptyModelRecentSessions: 2,
				EmptyModelRecentFraction: 0.4,
			},
			wantMode:     "provider_default",
			wantRules:    []string{doctorWorkflowRuleEmptyModelTelemetry},
			wantNoRules:  []string{doctorWorkflowRulePinnedWorkerModelStale},
			wantSeverity: map[string]string{doctorWorkflowRuleEmptyModelTelemetry: "info"},
		},
		{
			name: "multiplier kills",
			cfg: func() workflowconfig.Config {
				cfg := unpinnedConfig()
				cfg.Agent.MaxSessionContextMultiplier = 4
				return cfg
			}(),
			metrics: doctorWorkflowOptimizationMetrics{SessionMultiplierKills: []doctorWorkflowSessionGuardIncident{
				{IssueIdentifier: "gopherguides/gopher-ai#200", AttemptCount: 5, CeilingTokens: 1_032_000, ContextMultiplier: 4},
				{IssueIdentifier: "gopherguides/gopher-ai#196", AttemptCount: 2, CeilingTokens: 1_032_000, ContextMultiplier: 4},
			}},
			wantMode:   "provider_default",
			wantRules:  []string{doctorWorkflowRuleSessionMultiplierKills},
			wantDetail: []string{"max_session_context_multiplier=4", "1032000", "gopherguides/gopher-ai#200 x5", "gopherguides/gopher-ai#196 x2", "max_session_tokens"},
		},
		{
			name:      "no brake",
			cfg:       workflowconfig.Config{},
			metrics:   doctorWorkflowOptimizationMetrics{SessionCount: 3, MedianSessionTokens: 10_000},
			wantMode:  "provider_default",
			wantRules: []string{doctorWorkflowRuleNoSessionTokenBrake},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := doctorWorkflowWorkerModelChoice(tt.cfg).Mode; got != tt.wantMode {
				t.Fatalf("model choice = %q, want %q", got, tt.wantMode)
			}
			findings := doctorWorkflowOptimizationFindings("detent", "WORKFLOW.md", tt.cfg, tt.metrics)
			project := doctorWorkflowAnalyzedProject{projectID: "detent", workflowPath: "WORKFLOW.md", config: tt.cfg, metrics: tt.metrics}
			if finding, ok := doctorWorkflowStalePinnedModelFinding(project, observed); ok {
				findings = append(findings, finding)
			}
			for _, ruleID := range tt.wantRules {
				finding := doctorWorkflowFindingByRule(t, findings, ruleID)
				for _, detail := range tt.wantDetail {
					if !strings.Contains(finding.Detail, detail) {
						t.Fatalf("%s detail = %q, want containing %q", ruleID, finding.Detail, detail)
					}
				}
				if severity := tt.wantSeverity[ruleID]; severity != "" && finding.Severity != severity {
					t.Fatalf("%s severity = %q, want %q", ruleID, finding.Severity, severity)
				}
				if ruleID == doctorWorkflowRuleSessionMultiplierKills || ruleID == doctorWorkflowRuleNoSessionTokenBrake {
					proposals := doctorWorkflowProposalsForFindings("detent", []doctorWorkflowOptimizationFinding{finding}, 2)
					proposal := doctorWorkflowProposalBySignal(t, proposals, "doctor_finding", ruleID)
					if proposal.TargetKind != "workflow" || proposal.TargetPath == "" {
						t.Fatalf("%s proposal = %#v, want concrete workflow correction", ruleID, proposal)
					}
				}
			}
			for _, ruleID := range tt.wantNoRules {
				if doctorWorkflowFindingExists(findings, ruleID) {
					t.Fatalf("findings = %#v, do not want %s", findings, ruleID)
				}
			}
		})
	}
}

func TestDoctorWorkflowSessionGuardTelemetryGroupsMultiplierKills(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "detent.db")
	backend, err := store.Open(ctx, store.Config{Backend: store.BackendSQLite, Path: dbPath})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	kills := []struct {
		identifier string
		count      int
		source     string
	}{
		{identifier: "gopherguides/gopher-ai#200", count: 5, source: "max_session_context_multiplier"},
		{identifier: "gopherguides/gopher-ai#196", count: 2, source: "max_session_context_multiplier"},
		{identifier: "gopherguides/gopher-ai#201", count: 1, source: "max_session_context_multiplier"},
		{identifier: "gopherguides/gopher-ai#202", count: 3, source: "max_session_tokens"},
	}
	for _, kill := range kills {
		for attempt := 1; attempt <= kill.count; attempt++ {
			startedAt := now.Add(time.Duration(attempt) * time.Minute)
			attemptID, err := backend.StartWorkAttempt(ctx, store.WorkAttemptStart{
				ProjectID:     "gopher-ai",
				Identifier:    kill.identifier,
				WorkerType:    "implement",
				AttemptNumber: attempt,
				StartedAt:     startedAt,
			})
			if err != nil {
				t.Fatalf("StartWorkAttempt(%s, %d) error = %v", kill.identifier, attempt, err)
			}
			errorMessage := fmt.Sprintf("session token ceiling exceeded: total_tokens=1033000 ceiling_tokens=1032000 source=%s model_context_window=258000 context_multiplier=4", kill.source)
			if err := backend.CompleteWorkAttempt(ctx, store.WorkAttemptCompletion{
				AttemptID:     attemptID,
				CompletedAt:   startedAt.Add(time.Minute),
				TerminalState: store.WorkAttemptTerminalFailure,
				ErrorClass:    "runner_error",
				ErrorMessage:  errorMessage,
			}); err != nil {
				t.Fatalf("CompleteWorkAttempt(%s, %d) error = %v", kill.identifier, attempt, err)
			}
		}
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	db, err := openDoctorSQLiteReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatalf("openDoctorSQLiteReadOnly() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	incidents, err := doctorWorkflowSessionGuardTelemetry(ctx, db, "gopher-ai")
	if err != nil {
		t.Fatalf("doctorWorkflowSessionGuardTelemetry() error = %v", err)
	}
	if len(incidents) != 3 {
		t.Fatalf("incidents = %#v, want three multiplier-sourced issues", incidents)
	}
	if incidents[0].IssueIdentifier != "gopherguides/gopher-ai#200" || incidents[0].AttemptCount != 5 || incidents[0].CeilingTokens != 1_032_000 || incidents[0].ContextMultiplier != 4 {
		t.Fatalf("first incident = %#v, want #200 x5 at 4x/1032000", incidents[0])
	}
	if incidents[1].IssueIdentifier != "gopherguides/gopher-ai#196" || incidents[1].AttemptCount != 2 {
		t.Fatalf("second incident = %#v, want #196 x2", incidents[1])
	}
}

func TestDoctorWorkflowOptimizationFlagsPinBehindObservedDefault(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	pinnedWorkflow := filepath.Join(dir, "PINNED_WORKFLOW.md")
	defaultWorkflow := filepath.Join(dir, "DEFAULT_WORKFLOW.md")
	if err := os.WriteFile(pinnedWorkflow, []byte(`---
tracker:
  kind: memory
agent:
  max_session_tokens: 2000000
agents:
  routes:
    - name: default
      backend: codex
      model: gpt-5.5
      default: true
---
Prompt
`), 0o600); err != nil {
		t.Fatalf("WriteFile(pinned workflow) error = %v", err)
	}
	if err := os.WriteFile(defaultWorkflow, []byte(`---
tracker:
  kind: memory
agent:
  max_session_tokens: 2000000
---
Prompt
`), 0o600); err != nil {
		t.Fatalf("WriteFile(default workflow) error = %v", err)
	}

	dbPath := filepath.Join(dir, "detent.db")
	backend, err := store.Open(ctx, store.Config{Backend: store.BackendSQLite, Path: dbPath})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	recordSession := func(projectID string, identifier string, requestedModel string, model string, completedAt time.Time) {
		t.Helper()
		sessionID, err := backend.StartSession(ctx, store.SessionStart{Identifier: identifier, StartedAt: completedAt.Add(-time.Minute), Model: requestedModel})
		if err != nil {
			t.Fatalf("StartSession(%s) error = %v", identifier, err)
		}
		if err := backend.FinishSession(ctx, sessionID, store.SessionFinish{CompletedAt: completedAt, Turns: 1, InputTokens: 900, OutputTokens: 100, TotalTokens: 1000, FinalState: "completed", Model: model}); err != nil {
			t.Fatalf("FinishSession(%s) error = %v", identifier, err)
		}
		if _, err := backend.RecordUsageEvent(ctx, store.UsageEvent{ProjectID: projectID, SessionID: sessionID, Identifier: identifier, Model: model, InputTokens: 900, OutputTokens: 100, TotalTokens: 1000, Outcome: "completed", StartedAt: completedAt.Add(-time.Minute), FinishedAt: completedAt}); err != nil {
			t.Fatalf("RecordUsageEvent(%s) error = %v", identifier, err)
		}
	}
	now := time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC)
	recordSession("pinned", "example/pinned#1", "gpt-5.5", "gpt-5.5", now)
	recordSession("default", "example/default#1", "", "gpt-5.6-sol", now.Add(time.Minute))
	recordSession("default", "example/default#2", "", "gpt-5.6-sol", now.Add(2*time.Minute))
	recordSession("default", "example/default#3", "gpt-5.7-preview", "gpt-5.7-preview", now.Add(3*time.Minute))
	if err := backend.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	db, err := openDoctorSQLiteReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatalf("openDoctorSQLiteReadOnly() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	global := globalconfig.Config{Projects: []globalconfig.Project{
		{ID: "pinned", Workflow: pinnedWorkflow, Workdir: dir},
		{ID: "default", Workflow: defaultWorkflow, Workdir: dir},
	}}
	deps := successfulDoctorDeps()
	deps.loadWorkflow = workflowconfig.LoadWorkflow
	report, err := doctorWorkflowOptimization(ctx, db, dbPath, global, deps, "", doctorWorkflowOptimizationOptions{})
	if err != nil {
		t.Fatalf("doctorWorkflowOptimization() error = %v", err)
	}
	if len(report.Projects) != 2 || report.Projects[0].ModelChoice.Model != "gpt-5.5" || report.Projects[1].ModelChoice.Mode != "provider_default" {
		t.Fatalf("project model reports = %#v", report.Projects)
	}
	stale := doctorWorkflowFindingByRule(t, report.Findings, doctorWorkflowRulePinnedWorkerModelStale)
	if stale.ProjectID != "pinned" || evidenceInt64(t, stale, "observed_default_sessions") != 2 {
		t.Fatalf("stale finding = %#v, want pinned project against two default sessions", stale)
	}
	proposal := doctorWorkflowProposalBySignal(t, report.Proposals, "doctor_finding", doctorWorkflowRulePinnedWorkerModelStale)
	if !strings.Contains(proposal.SuggestedChange, "keep, update, or remove") {
		t.Fatalf("stale proposal is not model-choice neutral: %#v", proposal)
	}
}

func TestDoctorWorkflowOptimizationJSONReportSchema(t *testing.T) {
	t.Parallel()

	report := doctorReport{
		Checks: []doctorCheck{{
			Name:   doctorWorkflowOptimizationCheckName,
			Status: doctorWarn,
			Detail: "1 finding",
		}},
		WorkflowOptimization: doctorWorkflowOptimizationReport{
			StorePath: "/tmp/detent.db",
			Projects: []doctorWorkflowOptimizationProjectReport{{
				ProjectID:   "detent",
				ModelChoice: doctorWorkflowModelChoice{Mode: "pinned", Model: "gpt-5.5", Source: "agents.routes.model"},
				SessionGuard: doctorWorkflowSessionGuard{
					MaxSessionTokens:            2_000_000,
					MaxSessionContextMultiplier: 4,
				},
			}},
			Findings: []doctorWorkflowOptimizationFinding{{
				RuleID:               doctorWorkflowRuleRunawaySessionTokens,
				ProjectID:            "detent",
				Severity:             "warning",
				Title:                "Runaway session tail",
				Detail:               "max session tokens are high",
				EstimatedTokenImpact: 290000,
				Evidence: map[string]any{
					"max_session_tokens": int64(300000),
				},
			}},
		},
	}

	var output bytes.Buffer
	if err := writeDoctorReport(&output, report, OutputFormatJSON); err != nil {
		t.Fatalf("writeDoctorReport() error = %v", err)
	}

	var got struct {
		Result               string `json:"result"`
		WorkflowOptimization struct {
			StorePath string `json:"store_path"`
			Projects  []struct {
				ProjectID    string                     `json:"project_id"`
				ModelChoice  doctorWorkflowModelChoice  `json:"model_choice"`
				SessionGuard doctorWorkflowSessionGuard `json:"session_guard"`
			} `json:"projects"`
			Findings []struct {
				RuleID string         `json:"rule_id"`
				Detail string         `json:"detail"`
				Values map[string]any `json:"evidence"`
			} `json:"findings"`
		} `json:"workflow_optimization"`
	}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal() error = %v\n%s", err, output.String())
	}
	if got.Result != "PASS" {
		t.Fatalf("Result = %q, want PASS for advisory findings", got.Result)
	}
	if got.WorkflowOptimization.StorePath != "/tmp/detent.db" {
		t.Fatalf("StorePath = %q, want /tmp/detent.db", got.WorkflowOptimization.StorePath)
	}
	if len(got.WorkflowOptimization.Projects) != 1 || got.WorkflowOptimization.Projects[0].ModelChoice.Model != "gpt-5.5" || got.WorkflowOptimization.Projects[0].SessionGuard.MaxSessionTokens != 2_000_000 {
		t.Fatalf("workflow optimization projects = %#v, want model and session guard report", got.WorkflowOptimization.Projects)
	}
	if len(got.WorkflowOptimization.Findings) != 1 || got.WorkflowOptimization.Findings[0].RuleID != doctorWorkflowRuleRunawaySessionTokens {
		t.Fatalf("workflow optimization findings = %#v", got.WorkflowOptimization.Findings)
	}
}

func TestDoctorWorkflowOptimizationBudgetDriftFindingIsAdvisory(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Default()
	metrics := doctorWorkflowOptimizationMetrics{
		P90SessionTokens:                 500000,
		P90SessionBillableTokens:         300000,
		BudgetEstimateTokens:             doctorWorkflowBudgetEstimateTotal,
		BudgetEstimateBillableTokens:     doctorWorkflowBudgetEstimateBillable,
		BudgetEstimateDriftRatio:         doctorRoundedFloat(float64(300000)/float64(doctorWorkflowBudgetEstimateBillable), 4),
		BudgetEstimateBillableDriftRatio: doctorRoundedFloat(float64(300000)/float64(doctorWorkflowBudgetEstimateBillable), 4),
	}
	findings := doctorWorkflowOptimizationFindings("detent", "/tmp/WORKFLOW.md", cfg, metrics)
	budget := doctorWorkflowFindingByRule(t, findings, doctorWorkflowRuleBudgetEstimateDrift)
	if len(budget.Patch) != 0 {
		t.Fatalf("budget drift patches = %#v, want advisory finding with no patches", budget.Patch)
	}
	if got := evidenceInt64(t, budget, "observed_p90_billable_session_tokens"); got != 300000 {
		t.Fatalf("billable p90 = %d, want 300000", got)
	}
	if got := evidenceInt64(t, budget, "budget_estimate_billable_tokens"); got != doctorWorkflowBudgetEstimateBillable {
		t.Fatalf("budget estimate billable tokens = %d, want %d", got, doctorWorkflowBudgetEstimateBillable)
	}
	if _, ok := budget.Evidence["current_per_issue_max_usd"]; ok {
		t.Fatalf("budget drift evidence should not point at per_issue_max_usd: %#v", budget.Evidence)
	}
}

func TestDoctorWorkflowBillableTokensWeightsCachedInputCost(t *testing.T) {
	t.Parallel()

	pricing := budget.PricingTable{
		"gpt-test": {
			USDPerInputToken:       0.01,
			USDPerCachedInputToken: 0.001,
			USDPerOutputToken:      0.02,
		},
	}

	got := doctorWorkflowBillableTokens(1_000_000, 900_000, 20_000, 1_020_000, "gpt-test", pricing)
	if got != 205789 {
		t.Fatalf("doctorWorkflowBillableTokens() = %d, want 205789", got)
	}
	if got <= doctorWorkflowBudgetEstimateBillable {
		t.Fatalf("doctorWorkflowBillableTokens() = %d, want over default budget estimate", got)
	}
}

func TestDoctorSQLiteReadOnlyDSNFormatsWindowsDrivePath(t *testing.T) {
	t.Parallel()

	dsn := doctorSQLiteReadOnlyDSN(`C:\Users\RUNNER~1\AppData\Local\Temp\detent db.sqlite`)
	want := `file:/C:/Users/RUNNER~1/AppData/Local/Temp/detent%20db.sqlite?cache=shared&mode=ro`
	if dsn != want {
		t.Fatalf("doctorSQLiteReadOnlyDSN() = %q, want %q", dsn, want)
	}
	if strings.Contains(dsn, "file://C:") || strings.Contains(dsn, "%5C") {
		t.Fatalf("doctorSQLiteReadOnlyDSN() = %q, want no URI authority or escaped backslashes", dsn)
	}
}

func TestDoctorWorkflowOptimizationWriteRoundTripsWorkflow(t *testing.T) {
	t.Parallel()

	paths := seedDoctorWorkflowOptimizationFixture(t)
	configPath := filepath.Join(paths.dir, "global.yaml")
	global := doctorWorkflowOptimizationGlobal(paths)
	global.Path = configPath

	deps := successfulDoctorDeps()
	deps.loadWorkflow = workflowconfig.LoadWorkflow
	deps.openSQLiteReadOnly = openDoctorSQLiteReadOnly

	configFlag := configPath
	envFlag := ""
	logLevelFlag := ""
	hostFlag := "127.0.0.1"
	portFlag := 0
	cmd := newDoctorCommandWithDeps(&configFlag, &envFlag, &logLevelFlag, &hostFlag, &portFlag, successfulDoctorOptionsWithConfig(configPath, global), deps)
	cmd.SetArgs([]string{"--project", "detent", "--write"})
	cmd.SetIn(strings.NewReader("yes\n"))
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v, want nil for advisory findings\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}

	workflow, err := workflowconfig.LoadWorkflow(paths.workflow)
	if err != nil {
		t.Fatalf("LoadWorkflow() error = %v", err)
	}
	if err := workflow.Config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if workflow.Config.Agent.MaxSessionTokens != 40000 {
		t.Fatalf("Agent.MaxSessionTokens = %d, want 40000", workflow.Config.Agent.MaxSessionTokens)
	}
	if workflow.Config.Agent.AutoPromote.ReworkLimit != 2 {
		t.Fatalf("Agent.AutoPromote.ReworkLimit = %d, want 2", workflow.Config.Agent.AutoPromote.ReworkLimit)
	}
	if workflow.Config.Gate.Validator.Model != doctorWorkflowValidatorModel {
		t.Fatalf("Gate.Validator.Model = %q, want %s", workflow.Config.Gate.Validator.Model, doctorWorkflowValidatorModel)
	}
	if workflow.Config.Polling.IntervalMS != 120000 {
		t.Fatalf("Polling.IntervalMS = %d, want 120000", workflow.Config.Polling.IntervalMS)
	}
	if workflow.Config.Budget.PerIssueMaxUSD != 5 {
		t.Fatalf("Budget.PerIssueMaxUSD = %v, want unchanged default 5", workflow.Config.Budget.PerIssueMaxUSD)
	}
	if doctorWorkflowHasDefaultRouteModel(workflow.Config) {
		t.Fatalf("workflow gained unexpected default route model after write: %#v", workflow.Config.Agents.Routes)
	}
}

func TestDoctorWorkflowOptimizationStrictFailsOnAdvisoryFindings(t *testing.T) {
	t.Parallel()

	paths := seedDoctorWorkflowOptimizationFixture(t)
	configPath := filepath.Join(paths.dir, "global.yaml")
	global := doctorWorkflowOptimizationGlobal(paths)
	global.Path = configPath

	deps := successfulDoctorDeps()
	deps.loadWorkflow = workflowconfig.LoadWorkflow
	deps.openSQLiteReadOnly = openDoctorSQLiteReadOnly

	configFlag := configPath
	envFlag := ""
	logLevelFlag := ""
	hostFlag := "127.0.0.1"
	portFlag := 0
	cmd := newDoctorCommandWithDeps(&configFlag, &envFlag, &logLevelFlag, &hostFlag, &portFlag, successfulDoctorOptionsWithConfig(configPath, global), deps)
	cmd.SetArgs([]string{"--project", "detent", "--strict"})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if !errors.Is(err, ErrDoctorFailed) {
		t.Fatalf("Execute() error = %v, want ErrDoctorFailed for strict advisory findings\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	for _, want := range []string{"Workflow Optimization", "Summary:", "0 FAIL", "Result: FAIL"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestDoctorWorkflowOptimizationEmptyModelTelemetryRespectsBackendCommandModel(t *testing.T) {
	t.Parallel()

	workflow, err := workflowconfig.ParseWorkflow([]byte(`---
tracker:
  kind: memory
agents:
  backends:
    - id: codex-main
      kind: codex
      command: codex --config 'model="gpt-5.5"' app-server
  routes:
    - name: default
      backend: codex-main
      default: true
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	if !doctorWorkflowHasDefaultRouteModel(workflow.Config) {
		t.Fatal("doctorWorkflowHasDefaultRouteModel() = false, want true for backend command model")
	}
	workflow.Config.Budget.Enabled = true

	findings := doctorWorkflowOptimizationFindings("detent", "WORKFLOW.md", workflow.Config, doctorWorkflowOptimizationMetrics{
		RecentSessionCount:       50,
		EmptyModelRecentSessions: 48,
		EmptyModelRecentFraction: 0.96,
		TotalTokens:              100_000,
	})
	emptyModel := doctorWorkflowFindingByRule(t, findings, doctorWorkflowRuleEmptyModelTelemetry)
	if len(emptyModel.Patch) != 0 {
		t.Fatalf("empty model telemetry patch = %#v, want none for configured backend command model", emptyModel.Patch)
	}
	if got := emptyModel.Evidence["configured_model"]; got != "gpt-5.5" {
		t.Fatalf("configured_model evidence = %#v, want gpt-5.5", got)
	}
}

func TestDoctorWorkflowOptimizationEmptyModelTelemetryHasNoHardcodedModelPatch(t *testing.T) {
	t.Parallel()

	workflow, err := workflowconfig.ParseWorkflow([]byte(`---
tracker:
  kind: memory
codex:
  command: codex app-server
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	workflow.Config.Budget.Enabled = true

	findings := doctorWorkflowOptimizationFindings("detent", "WORKFLOW.md", workflow.Config, doctorWorkflowOptimizationMetrics{
		RecentSessionCount:       50,
		EmptyModelRecentSessions: 48,
		EmptyModelRecentFraction: 0.96,
		TotalTokens:              100_000,
	})
	emptyModel := doctorWorkflowFindingByRule(t, findings, doctorWorkflowRuleEmptyModelTelemetry)
	if len(emptyModel.Patch) != 0 {
		t.Fatalf("empty model telemetry patch = %#v, want none when configured model is unknown", emptyModel.Patch)
	}
}

func TestDoctorWorkflowSessionTelemetryBoundsRecentEmptyModelsByLatestSessionWindow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "detent.db")
	backend, err := store.Open(ctx, store.Config{
		Backend: store.BackendSQLite,
		Path:    dbPath,
	})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}

	base := time.Date(2026, 7, 3, 18, 0, 0, 0, time.UTC)
	recordSession := func(completedAt time.Time, model string) {
		t.Helper()

		startedAt := completedAt.Add(-5 * time.Minute)
		sessionID, err := backend.StartSession(ctx, store.SessionStart{
			Identifier: "digitaldrywood/detent#890",
			StartedAt:  startedAt,
			Model:      model,
		})
		if err != nil {
			t.Fatalf("StartSession() error = %v", err)
		}
		if err := backend.FinishSession(ctx, sessionID, store.SessionFinish{
			CompletedAt:       completedAt,
			Turns:             1,
			InputTokens:       900,
			CachedInputTokens: 450,
			OutputTokens:      100,
			TotalTokens:       1000,
			RuntimeSeconds:    300,
			FinalState:        "completed",
			Model:             model,
		}); err != nil {
			t.Fatalf("FinishSession() error = %v", err)
		}
		if _, err := backend.RecordUsageEvent(ctx, store.UsageEvent{
			ProjectID:         "detent",
			SessionID:         sessionID,
			Identifier:        "digitaldrywood/detent#890",
			Model:             model,
			InputTokens:       900,
			CachedInputTokens: 450,
			OutputTokens:      100,
			TotalTokens:       1000,
			RuntimeSeconds:    300,
			StartedAt:         startedAt,
			FinishedAt:        completedAt,
			Outcome:           "completed",
		}); err != nil {
			t.Fatalf("RecordUsageEvent() error = %v", err)
		}
	}

	for index := range 48 {
		recordSession(base.Add(-doctorWorkflowRecentSessionWindow-time.Hour-time.Duration(index)*time.Minute), "")
	}
	recordSession(base.Add(-time.Minute), "gpt-5.5")
	recordSession(base, "gpt-5.5")

	if err := backend.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	db, err := openDoctorSQLiteReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatalf("openDoctorSQLiteReadOnly() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	metrics, err := doctorWorkflowSessionTelemetry(ctx, db, "detent", budget.DefaultPricingTable())
	if err != nil {
		t.Fatalf("doctorWorkflowSessionTelemetry() error = %v", err)
	}
	if metrics.count != 50 {
		t.Fatalf("count = %d, want 50", metrics.count)
	}
	if metrics.recentSessionCount != 2 {
		t.Fatalf("recentSessionCount = %d, want 2", metrics.recentSessionCount)
	}
	if metrics.emptyModelRecentSessions != 0 {
		t.Fatalf("emptyModelRecentSessions = %d, want 0", metrics.emptyModelRecentSessions)
	}
}

func TestDoctorWorkflowDefaultRouteModelConfigSources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		wantOK     bool
		wantModel  string
		wantSource string
	}{
		{
			name: "route model",
			raw: `---
tracker:
  kind: memory
agents:
  backends:
    - id: codex-main
      kind: codex
      command: codex app-server
  routes:
    - name: default
      backend: codex-main
      model: gpt-5-route
      default: true
---
Prompt
`,
			wantOK:     true,
			wantModel:  "gpt-5-route",
			wantSource: "agents.routes.model",
		},
		{
			name: "backend command config model",
			raw: `---
tracker:
  kind: memory
agents:
  backends:
    - id: codex-main
      kind: codex
      command: codex --config=model=\"gpt-5.5\" app-server
  routes:
    - name: default
      backend: codex-main
      default: true
---
Prompt
`,
			wantOK:     true,
			wantModel:  "gpt-5.5",
			wantSource: "agents.backends.command",
		},
		{
			name: "backend command config model with issue field overrides",
			raw: `---
tracker:
  kind: memory
agents:
  backends:
    - id: codex-main
      kind: codex
      command: codex --config=model="gpt-5.5" app-server
  routes:
    - name: default
      backend: codex-main
      model_field: Model
      default: true
---
Prompt
`,
			wantOK:     true,
			wantModel:  "gpt-5.5",
			wantSource: "agents.backends.command",
		},
		{
			name: "route model field",
			raw: `---
tracker:
  kind: memory
agents:
  backends:
    - id: codex-main
      kind: codex
      command: codex app-server
  routes:
    - name: default
      backend: codex-main
      model_field: Model
      default: true
---
Prompt
`,
			wantOK:     true,
			wantSource: "agents.routes.model_field",
		},
		{
			name: "no model source",
			raw: `---
tracker:
  kind: memory
codex:
  command: codex app-server
---
Prompt
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workflow, err := workflowconfig.ParseWorkflow([]byte(tt.raw))
			if err != nil {
				t.Fatalf("ParseWorkflow() error = %v", err)
			}
			got, ok := doctorWorkflowDefaultRouteModelConfig(workflow.Config)
			if ok != tt.wantOK {
				t.Fatalf("doctorWorkflowDefaultRouteModelConfig() ok = %v, want %v; config = %#v", ok, tt.wantOK, got)
			}
			if got.Model != tt.wantModel {
				t.Fatalf("doctorWorkflowDefaultRouteModelConfig() model = %q, want %q", got.Model, tt.wantModel)
			}
			if got.Source != tt.wantSource {
				t.Fatalf("doctorWorkflowDefaultRouteModelConfig() source = %q, want %q", got.Source, tt.wantSource)
			}
		})
	}
}

func TestDoctorWorkflowOptimizationUsesRuntimeGitHubToken(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "detent.db")
	backend, err := store.Open(ctx, store.Config{
		Backend: store.BackendSQLite,
		Path:    dbPath,
	})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte(`---
tracker:
  kind: github_local
  repository: digitaldrywood/detent
  local_sqlite:
    path: `+dbPath+`
---
Prompt
`), 0o600); err != nil {
		t.Fatalf("WriteFile(WORKFLOW.md) error = %v", err)
	}

	db, err := openDoctorSQLiteReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatalf("openDoctorSQLiteReadOnly() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	global := globalconfig.Config{
		Path:       filepath.Join(dir, "global.yaml"),
		APIVersion: globalconfig.APIVersion,
		Kind:       globalconfig.Kind,
		Projects: []globalconfig.Project{{
			ID:       "detent",
			Workflow: workflowPath,
			Workdir:  dir,
			Weight:   1,
		}},
	}
	deps := successfulDoctorDeps()
	deps.loadWorkflow = workflowconfig.LoadWorkflow
	report, err := doctorWorkflowOptimization(ctx, db, dbPath, global, deps, "runtime-token", doctorWorkflowOptimizationOptions{})
	if err != nil {
		t.Fatalf("doctorWorkflowOptimization() error = %v", err)
	}
	if len(report.Projects) != 1 {
		t.Fatalf("projects len = %d, want 1", len(report.Projects))
	}
	if report.Projects[0].Error != "" {
		t.Fatalf("project error = %q, want none", report.Projects[0].Error)
	}
}

type doctorWorkflowOptimizationFixturePaths struct {
	dir      string
	db       string
	workflow string
}

func seedDoctorWorkflowOptimizationFixture(t *testing.T) doctorWorkflowOptimizationFixturePaths {
	t.Helper()

	ctx := context.Background()
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte(`---
tracker:
  kind: memory
  local_sqlite:
    project_id: detent-local
  active_states:
    - Todo
    - In Progress
    - Rework
    - Merging
  observed_states:
    - Backlog
    - Human Review
    - Blocked
polling:
  interval_ms: 60000
budget:
  enabled: true
agent:
  auto_promote:
    enabled: true
    rework_limit: 0
gate:
  kind: command
  validator:
    enabled: true
---
Prompt
`), 0o600); err != nil {
		t.Fatalf("WriteFile(WORKFLOW.md) error = %v", err)
	}

	dbPath := filepath.Join(dir, "detent.db")
	backend, err := store.Open(ctx, store.Config{
		Backend: store.BackendSQLite,
		Path:    dbPath,
	})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}

	now := time.Date(2026, 7, 2, 15, 0, 0, 0, time.UTC)
	sessionTotals := []struct {
		issue      string
		total      int64
		model      string
		finalState string
	}{
		{issue: "digitaldrywood/detent#1", total: 10000, finalState: "completed"},
		{issue: "digitaldrywood/detent#1", total: 10000, finalState: "completed"},
		{issue: "digitaldrywood/detent#2", total: 10000, finalState: "completed"},
		{issue: "digitaldrywood/detent#3", total: 120000, finalState: "completed"},
		{issue: "digitaldrywood/detent#4", total: 300000, model: "gpt-5-codex", finalState: "failed"},
	}
	for index, session := range sessionTotals {
		startedAt := now.Add(time.Duration(index-10) * time.Minute)
		sessionID, err := backend.StartSession(ctx, store.SessionStart{
			Identifier: session.issue,
			StartedAt:  startedAt,
			Model:      session.model,
		})
		if err != nil {
			t.Fatalf("StartSession() error = %v", err)
		}
		outputTokens := int64(1000)
		if err := backend.FinishSession(ctx, sessionID, store.SessionFinish{
			CompletedAt:       startedAt.Add(5 * time.Minute),
			Turns:             1,
			InputTokens:       session.total - outputTokens,
			CachedInputTokens: (session.total - outputTokens) / 2,
			OutputTokens:      outputTokens,
			TotalTokens:       session.total,
			RuntimeSeconds:    300,
			FinalState:        session.finalState,
			Model:             session.model,
		}); err != nil {
			t.Fatalf("FinishSession() error = %v", err)
		}
		if _, err := backend.RecordUsageEvent(ctx, store.UsageEvent{
			ProjectID:         "detent",
			SessionID:         sessionID,
			Identifier:        session.issue,
			Model:             session.model,
			InputTokens:       session.total - outputTokens,
			CachedInputTokens: (session.total - outputTokens) / 2,
			OutputTokens:      outputTokens,
			TotalTokens:       session.total,
			RuntimeSeconds:    300,
			StartedAt:         startedAt,
			FinishedAt:        startedAt.Add(5 * time.Minute),
			Outcome:           session.finalState,
		}); err != nil {
			t.Fatalf("RecordUsageEvent() error = %v", err)
		}
	}

	otherStartedAt := now
	otherSessionID, err := backend.StartSession(ctx, store.SessionStart{
		Identifier: "digitaldrywood/other#99",
		StartedAt:  otherStartedAt,
	})
	if err != nil {
		t.Fatalf("StartSession(other) error = %v", err)
	}
	if err := backend.FinishSession(ctx, otherSessionID, store.SessionFinish{
		CompletedAt:       otherStartedAt.Add(5 * time.Minute),
		Turns:             1,
		InputTokens:       899000,
		CachedInputTokens: 449500,
		OutputTokens:      1000,
		TotalTokens:       900000,
		RuntimeSeconds:    300,
		FinalState:        "failed",
	}); err != nil {
		t.Fatalf("FinishSession(other) error = %v", err)
	}
	if _, err := backend.RecordUsageEvent(ctx, store.UsageEvent{
		ProjectID:         "other",
		SessionID:         otherSessionID,
		Identifier:        "digitaldrywood/other#99",
		InputTokens:       899000,
		CachedInputTokens: 449500,
		OutputTokens:      1000,
		TotalTokens:       900000,
		RuntimeSeconds:    300,
		StartedAt:         otherStartedAt,
		FinishedAt:        otherStartedAt.Add(5 * time.Minute),
		Outcome:           "failed",
	}); err != nil {
		t.Fatalf("RecordUsageEvent(other) error = %v", err)
	}

	for index := range 3 {
		if _, err := backend.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
			ProjectID:       "detent",
			Identifier:      "digitaldrywood/detent#2",
			PhaseType:       store.WorkflowPhaseTypeLane,
			PhaseName:       "Rework",
			Status:          "completed",
			StartedAt:       now.Add(time.Duration(index) * time.Hour),
			FinishedAt:      now.Add(time.Duration(index)*time.Hour + 30*time.Minute),
			DurationSeconds: int64((30 * time.Minute) / time.Second),
		}); err != nil {
			t.Fatalf("RecordWorkflowPhaseEvent(rework) error = %v", err)
		}
	}
	for index, duration := range []time.Duration{time.Hour, 3 * time.Hour, 6 * time.Hour} {
		if _, err := backend.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
			ProjectID:       "detent",
			Identifier:      "digitaldrywood/detent#3",
			PhaseType:       store.WorkflowPhaseTypeLane,
			PhaseName:       "In Progress",
			Status:          "completed",
			StartedAt:       now.Add(time.Duration(index+4) * time.Hour),
			FinishedAt:      now.Add(time.Duration(index+4)*time.Hour + duration),
			DurationSeconds: int64(duration / time.Second),
		}); err != nil {
			t.Fatalf("RecordWorkflowPhaseEvent(lane) error = %v", err)
		}
	}

	for index := range 6 {
		selected := index >= 4
		result := store.SchedulerDecisionResultSkipped
		if selected {
			result = store.SchedulerDecisionResultSelected
		}
		if _, err := backend.RecordSchedulerDecision(ctx, store.SchedulerDecision{
			ProjectID:  "detent",
			Identifier: "digitaldrywood/detent#5",
			Lane:       "Todo",
			Result:     result,
			Selected:   selected,
			DecisionAt: now.Add(time.Duration(index) * time.Second),
		}); err != nil {
			t.Fatalf("RecordSchedulerDecision() error = %v", err)
		}
	}

	if err := backend.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	return doctorWorkflowOptimizationFixturePaths{
		dir:      dir,
		db:       dbPath,
		workflow: workflowPath,
	}
}

func doctorWorkflowOptimizationGlobal(paths doctorWorkflowOptimizationFixturePaths) globalconfig.Config {
	return globalconfig.Config{
		Path:       filepath.Join(paths.dir, "global.yaml"),
		APIVersion: globalconfig.APIVersion,
		Kind:       globalconfig.Kind,
		Global: globalconfig.Settings{
			MaxConcurrentAgents: 1,
			Scheduling:          globalconfig.SchedulingWeighted,
		},
		Projects: []globalconfig.Project{{
			ID:       "detent",
			Workflow: paths.workflow,
			Workdir:  paths.dir,
			Weight:   1,
		}},
	}
}

func doctorWorkflowFindingByRule(t *testing.T, findings []doctorWorkflowOptimizationFinding, ruleID string) doctorWorkflowOptimizationFinding {
	t.Helper()

	for _, finding := range findings {
		if finding.RuleID == ruleID {
			return finding
		}
	}
	t.Fatalf("missing finding %q in %#v", ruleID, findings)
	return doctorWorkflowOptimizationFinding{}
}

func doctorWorkflowFindingExists(findings []doctorWorkflowOptimizationFinding, ruleID string) bool {
	for _, finding := range findings {
		if finding.RuleID == ruleID {
			return true
		}
	}
	return false
}

func evidenceInt64(t *testing.T, finding doctorWorkflowOptimizationFinding, key string) int64 {
	t.Helper()

	value, ok := finding.Evidence[key]
	if !ok {
		t.Fatalf("%s evidence missing %q: %#v", finding.RuleID, key, finding.Evidence)
	}
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	default:
		t.Fatalf("%s evidence %q = %#v (%T), want integer", finding.RuleID, key, value, value)
		return 0
	}
}

func evidenceFloat64(t *testing.T, finding doctorWorkflowOptimizationFinding, key string) float64 {
	t.Helper()

	value, ok := finding.Evidence[key]
	if !ok {
		t.Fatalf("%s evidence missing %q: %#v", finding.RuleID, key, finding.Evidence)
	}
	switch typed := value.(type) {
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case float64:
		return typed
	default:
		t.Fatalf("%s evidence %q = %#v (%T), want float", finding.RuleID, key, value, value)
		return 0
	}
}

func seedDoctorWorkflowRepeatedValidatorFindings(t *testing.T, dbPath string) {
	t.Helper()

	ctx := context.Background()
	backend, err := store.Open(ctx, store.Config{
		Backend: store.BackendSQLite,
		Path:    dbPath,
	})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	recordedAt := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	for index, headSHA := range []string{"head-a", "head-b"} {
		if err := backend.RecordValidatorVerdict(ctx, store.ValidatorVerdict{
			ProjectID:  "detent",
			IssueID:    "issue-validator-" + headSHA,
			HeadSHA:    headSHA,
			Identifier: "digitaldrywood/detent#" + strconv.Itoa(800+index),
			Submitted:  true,
			Verdict:    "rework",
			Findings: []store.ValidatorFinding{{
				Severity: "p1",
				Body:     "Missing rollback coverage.",
				Path:     "internal/runner/prompt.go",
				Line:     44,
			}},
			RecordedAt: recordedAt.Add(time.Duration(index) * time.Minute),
			UpdatedAt:  recordedAt.Add(time.Duration(index) * time.Minute),
		}); err != nil {
			t.Fatalf("RecordValidatorVerdict() error = %v", err)
		}
	}
}

func seedDoctorWorkflowRepeatedLessons(t *testing.T, dir string) {
	t.Helper()

	path := filepath.Join(dir, ".detent", "lessons.md")
	for index := range 2 {
		if err := lessons.Append(path, lessons.Entry{
			IssueNumber: strconv.Itoa(900 + index),
			Title:       "Token ceiling " + strconv.Itoa(index+1),
			FailureKind: "token_ceiling_exceeded",
			Symptom:     "session hit the configured ceiling",
			Hypothesis:  "task was too broad",
			Hint:        "split the task before retry",
		}, lessons.AppendOptions{Date: time.Date(2026, 7, 5+index, 0, 0, 0, 0, time.UTC)}); err != nil {
			t.Fatalf("lessons.Append() error = %v", err)
		}
	}
}

func doctorWorkflowProposalBySignal(t *testing.T, proposals []doctorWorkflowImprovementProposal, signalKind string, pattern string) doctorWorkflowImprovementProposal {
	t.Helper()

	for _, proposal := range proposals {
		if proposal.SignalKind == signalKind && proposal.Pattern == pattern {
			return proposal
		}
	}
	t.Fatalf("missing proposal signal=%q pattern=%q in %#v", signalKind, pattern, proposals)
	return doctorWorkflowImprovementProposal{}
}

type blockedDoctorProposalConnector struct {
	*memory.Connector
	updateCalls int
}

func (c *blockedDoctorProposalConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.updateCalls++
	return &connector.StateUpdateBlockedError{
		IssueID:      issueID,
		CurrentState: "Done",
		TargetState:  state,
	}
}
