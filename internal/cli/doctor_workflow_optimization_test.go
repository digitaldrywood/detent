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

	"github.com/digitaldrywood/detent/internal/budget"
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
	report, err := doctorWorkflowOptimization(ctx, db, paths.db, global, deps, "", true)
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
	if metrics.P90SessionBillableTokens != 150500 {
		t.Fatalf("P90SessionBillableTokens = %d, want 150500", metrics.P90SessionBillableTokens)
	}
	if metrics.BudgetEstimateDriftRatio != 0.8853 {
		t.Fatalf("BudgetEstimateDriftRatio = %v, want 0.8853", metrics.BudgetEstimateDriftRatio)
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

			findings := doctorWorkflowOptimizationFindings("detent", "WORKFLOW.md", workflowconfig.Config{}, tt.metrics)
			finding := doctorWorkflowFindingByRule(t, findings, tt.ruleID)
			if finding.EstimatedTokenImpact != 0 {
				t.Fatalf("EstimatedTokenImpact = %d, want 0", finding.EstimatedTokenImpact)
			}
		})
	}
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
			name:          "configured cap above suggested cap",
			configuredCap: 50000,
			wantFinding:   true,
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
	if got.Result != "PASS" {
		t.Fatalf("Result = %q, want PASS for advisory findings", got.Result)
	}
	if got.WorkflowOptimization.StorePath != "/tmp/detent.db" {
		t.Fatalf("StorePath = %q, want /tmp/detent.db", got.WorkflowOptimization.StorePath)
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
		name      string
		raw       string
		wantOK    bool
		wantModel string
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
			wantOK:    true,
			wantModel: "gpt-5-route",
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
			wantOK:    true,
			wantModel: "gpt-5.5",
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
			wantOK: true,
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
	report, err := doctorWorkflowOptimization(ctx, db, dbPath, global, deps, "runtime-token", false)
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
