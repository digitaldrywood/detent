package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
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
	report, err := doctorWorkflowOptimization(ctx, db, paths.db, global, deps, true)
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

	budget := doctorWorkflowFindingByRule(t, report.Findings, doctorWorkflowRuleBudgetEstimateDrift)
	if got := evidenceInt64(t, budget, "observed_p90_session_tokens"); got != 300000 {
		t.Fatalf("budget p90 = %d, want 300000", got)
	}

	scheduler := doctorWorkflowFindingByRule(t, report.Findings, doctorWorkflowRuleSchedulerSkipRate)
	if got := evidenceFloat64(t, scheduler, "scheduler_skip_rate"); got != 0.67 {
		t.Fatalf("scheduler skip rate = %v, want 0.67", got)
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
			Findings  []struct {
				RuleID string         `json:"rule_id"`
				Detail string         `json:"detail"`
				Values map[string]any `json:"evidence"`
			} `json:"findings"`
		} `json:"workflow_optimization"`
	}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal() error = %v\n%s", err, output.String())
	}
	if got.Result != "FAIL" {
		t.Fatalf("Result = %q, want FAIL for nonzero findings", got.Result)
	}
	if got.WorkflowOptimization.StorePath != "/tmp/detent.db" {
		t.Fatalf("StorePath = %q, want /tmp/detent.db", got.WorkflowOptimization.StorePath)
	}
	if len(got.WorkflowOptimization.Findings) != 1 || got.WorkflowOptimization.Findings[0].RuleID != doctorWorkflowRuleRunawaySessionTokens {
		t.Fatalf("workflow optimization findings = %#v", got.WorkflowOptimization.Findings)
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

	err := cmd.Execute()
	if !errors.Is(err, ErrDoctorFailed) {
		t.Fatalf("Execute() error = %v, want ErrDoctorFailed for nonzero findings\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
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
	if workflow.Config.Budget.PerIssueMaxUSD != 8.82 {
		t.Fatalf("Budget.PerIssueMaxUSD = %v, want 8.82", workflow.Config.Budget.PerIssueMaxUSD)
	}
	if !doctorWorkflowHasDefaultRouteModel(workflow.Config) {
		t.Fatalf("workflow missing default route model after write: %#v", workflow.Config.Agents.Routes)
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
polling:
  interval_ms: 60000
agent:
  auto_promote:
    enabled: true
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
