package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
)

func TestProbeOnboardingRepositoryFixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		setup          func(t *testing.T, root string)
		wantLanguage   string
		wantCommand    string
		wantManifest   string
		wantCIWorkflow string
	}{
		{
			name: "go",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeOnboardingWorkflowBuilderFile(t, filepath.Join(root, "go.mod"), "module example.com/api\n\ngo 1.26\ntoolchain go1.26.4\n")
				writeOnboardingWorkflowBuilderFile(t, filepath.Join(root, "Makefile"), ".PHONY: check\ncheck:\n\tgo test ./...\n")
				writeOnboardingWorkflowBuilderFile(t, filepath.Join(root, ".github", "workflows", "ci.yml"), "name: ci\n")
			},
			wantLanguage:   "Go",
			wantCommand:    "make check",
			wantManifest:   "go.mod",
			wantCIWorkflow: ".github/workflows/ci.yml",
		},
		{
			name: "node",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeOnboardingWorkflowBuilderFile(t, filepath.Join(root, "package.json"), `{"scripts":{"test":"node --test"},"engines":{"node":">=22"}}`)
				writeOnboardingWorkflowBuilderFile(t, filepath.Join(root, "package-lock.json"), "{}")
			},
			wantLanguage: "Node",
			wantCommand:  "npm test",
			wantManifest: "package.json",
		},
		{
			name: "empty unknown",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeOnboardingWorkflowBuilderFile(t, filepath.Join(root, "README.md"), "unknown\n")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			tt.setup(t, root)

			got, err := probeOnboardingRepository(root)
			if err != nil {
				t.Fatalf("probeOnboardingRepository() error = %v", err)
			}
			if !got.ReadOnly {
				t.Fatal("ReadOnly = false, want true")
			}
			if tt.wantLanguage != "" && !onboardingStringSliceContains(got.Languages, tt.wantLanguage) {
				t.Fatalf("Languages = %#v, want %s", got.Languages, tt.wantLanguage)
			}
			if tt.wantCommand != "" && got.ValidationCommand != tt.wantCommand {
				t.Fatalf("ValidationCommand = %q, want %q", got.ValidationCommand, tt.wantCommand)
			}
			if tt.wantManifest != "" && !onboardingStringSliceContains(got.Manifests, tt.wantManifest) {
				t.Fatalf("Manifests = %#v, want %s", got.Manifests, tt.wantManifest)
			}
			if tt.wantCIWorkflow != "" && !onboardingStringSliceContains(got.CIWorkflows, tt.wantCIWorkflow) {
				t.Fatalf("CIWorkflows = %#v, want %s", got.CIWorkflows, tt.wantCIWorkflow)
			}
			if tt.wantLanguage == "" && (len(got.Languages) != 0 || got.ValidationCommand != "" || got.Monorepo) {
				t.Fatalf("unknown probe = %#v, want no language, command, or monorepo", got)
			}
		})
	}
}

func TestProbeOnboardingRepositoryDetectsMonorepo(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeOnboardingWorkflowBuilderFile(t, filepath.Join(root, "go.work"), "go 1.26\n\nuse ./services/api\n")
	writeOnboardingWorkflowBuilderFile(t, filepath.Join(root, "services", "api", "go.mod"), "module example.com/api\n\ngo 1.26\n")
	writeOnboardingWorkflowBuilderFile(t, filepath.Join(root, "apps", "web", "package.json"), `{"scripts":{"test":"node --test"}}`)

	got, err := probeOnboardingRepository(root)
	if err != nil {
		t.Fatalf("probeOnboardingRepository() error = %v", err)
	}
	if !got.Monorepo {
		t.Fatalf("Monorepo = false, evidence = %#v", got.MonorepoEvidence)
	}
	if len(got.MonorepoEvidence) == 0 {
		t.Fatal("MonorepoEvidence is empty")
	}
}

func TestProbeOnboardingRepositoryIgnoresNestedNodeTestScriptForRootGate(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeOnboardingWorkflowBuilderFile(t, filepath.Join(root, "apps", "web", "package.json"), `{"scripts":{"test":"node --test"}}`)
	writeOnboardingWorkflowBuilderFile(t, filepath.Join(root, "apps", "web", "pnpm-lock.yaml"), "")

	got, err := probeOnboardingRepository(root)
	if err != nil {
		t.Fatalf("probeOnboardingRepository() error = %v", err)
	}
	if got.NodeTestScript {
		t.Fatal("NodeTestScript = true, want false for nested package.json")
	}
	if got.PackageManager != "" {
		t.Fatalf("PackageManager = %q, want empty for nested lockfile", got.PackageManager)
	}
	if got.ValidationCommand != "" {
		t.Fatalf("ValidationCommand = %q, want empty without a root package test script", got.ValidationCommand)
	}
}

func TestBuildOnboardingWorkflowPreservesArtifactPresetGate(t *testing.T) {
	t.Parallel()

	root := initOnboardingWorkflowBuilderGitRepository(t, "https://github.com/acme/artifacts.git")
	answersPath := writeOnboardingWorkflowBuilderAnswers(t, strings.Join([]string{
		"CUSTOMER_ID=acme",
		"DETENT_PROJECT_ID=artifact-production",
		"TARGET_REPOSITORY=acme/artifacts",
		"TARGET_SOURCE_ROOT=" + root,
		"REFERENCE_REPOSITORIES=digitaldrywood/detent",
		"DETENT_ONBOARDING_MODE=add-project",
		"IDENTITY_CONFIRMED=true",
		"WORKFLOW_PRESET=non_code_artifact",
		"",
	}, "\n"))

	result, err := buildOnboardingWorkflow(context.Background(), onboardingBuildWorkflowConfig{
		AnswersPath: answersPath,
		Write:       false,
	})
	if err != nil {
		t.Fatalf("buildOnboardingWorkflow() error = %v", err)
	}
	workflow, err := workflowconfig.ParseWorkflow([]byte(result.Workflow))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v\n%s", err, result.Workflow)
	}
	if workflow.Config.Gate.Kind != gate.KindArtifact {
		t.Fatalf("Gate.Kind = %q, want artifact", workflow.Config.Gate.Kind)
	}
	if workflow.Config.Gate.Run != "" {
		t.Fatalf("Gate.Run = %q, want empty for artifact preset", workflow.Config.Gate.Run)
	}
	if workflow.Config.Gate.Artifact.StatusField != "render_status" {
		t.Fatalf("Gate.Artifact.StatusField = %q, want render_status", workflow.Config.Gate.Artifact.StatusField)
	}
	assertOnboardingWorkflowDecision(t, result.Decisions, "gate.kind", "preset")
	assertOnboardingWorkflowDecision(t, result.Decisions, "gate.validator.model", "preset")
	assertOnboardingWorkflowDecision(t, result.Decisions, "budget.per_issue_max_usd", "preset")
}

func TestBuildOnboardingWorkflowWorkerModelFixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		answers     []string
		wantCommand string
		wantComment string
		wantModel   string
	}{
		{
			name:        "provider default",
			answers:     []string{"WORKER_MODEL_MODE=provider_default"},
			wantCommand: "codex app-server",
			wantComment: "Provider default: upgrades automatically and avoids retirement breakage.",
		},
		{
			name: "pinned",
			answers: []string{
				"WORKER_MODEL_MODE=pinned",
				"WORKER_MODEL=gpt-5.6-sol",
			},
			wantCommand: `codex app-server --config 'model="gpt-5.6-sol"'`,
			wantComment: "Pinned for reproducibility or cost control; update before retirement.",
			wantModel:   "gpt-5.6-sol",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := initOnboardingWorkflowBuilderGitRepository(t, "https://github.com/acme/api.git")
			answersPath := writeOnboardingWorkflowBuilderAnswers(t, onboardingWorkflowBuilderVariantAnswers(root, "github_local", "review_gate", tt.answers...))

			result, err := buildOnboardingWorkflow(context.Background(), onboardingBuildWorkflowConfig{
				AnswersPath: answersPath,
				Write:       false,
			})
			if err != nil {
				t.Fatalf("buildOnboardingWorkflow() error = %v", err)
			}
			workflow, err := workflowconfig.ParseWorkflow([]byte(result.Workflow))
			if err != nil {
				t.Fatalf("ParseWorkflow() error = %v\n%s", err, result.Workflow)
			}
			if workflow.Config.Codex.Command != tt.wantCommand {
				t.Fatalf("Codex.Command = %q, want %q", workflow.Config.Codex.Command, tt.wantCommand)
			}
			assertOnboardingWorkflowContains(t, result.Workflow, tt.wantComment)
			assertOnboardingWorkflowContains(t, result.Workflow, "Optional model_reasoning_effort is unset because not every model accepts it.")
			assertOnboardingWorkflowDecision(t, result.Decisions, "answers.worker_model_mode", "answer")
			assertOnboardingWorkflowDecision(t, result.Decisions, "codex.command", "answer")
			if tt.wantModel != "" {
				assertOnboardingWorkflowDecision(t, result.Decisions, "answers.worker_model", "answer")
			}
		})
	}
}

func TestBuildOnboardingWorkflowRejectsInvalidWorkerModelAnswers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		answers   []string
		wantError string
	}{
		{
			name:      "unknown mode",
			answers:   []string{"WORKER_MODEL_MODE=automatic"},
			wantError: "WORKER_MODEL_MODE must be provider_default or pinned",
		},
		{
			name:      "pinned without model",
			answers:   []string{"WORKER_MODEL_MODE=pinned"},
			wantError: "WORKER_MODEL is required when WORKER_MODEL_MODE=pinned",
		},
		{
			name: "provider default with model",
			answers: []string{
				"WORKER_MODEL_MODE=provider_default",
				"WORKER_MODEL=gpt-5.6-sol",
			},
			wantError: "WORKER_MODEL must be omitted when WORKER_MODEL_MODE=provider_default",
		},
		{
			name: "unsafe pinned model",
			answers: []string{
				"WORKER_MODEL_MODE=pinned",
				"WORKER_MODEL=gpt-5.6-sol';echo",
			},
			wantError: "WORKER_MODEL contains unsupported characters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := initOnboardingWorkflowBuilderGitRepository(t, "https://github.com/acme/api.git")
			answersPath := writeOnboardingWorkflowBuilderAnswers(t, onboardingWorkflowBuilderVariantAnswers(root, "github_local", "review_gate", tt.answers...))

			_, err := buildOnboardingWorkflow(context.Background(), onboardingBuildWorkflowConfig{
				AnswersPath: answersPath,
				Write:       false,
			})
			if err == nil {
				t.Fatal("buildOnboardingWorkflow() error = nil, want validation error")
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("buildOnboardingWorkflow() error = %v, want %q", err, tt.wantError)
			}
		})
	}
}

func TestBuildOnboardingWorkflowSessionContextMultiplierIsOptIn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		answers        []string
		wantMultiplier float64
		wantPresent    bool
		wantProvenance string
	}{
		{
			name:           "omitted by default",
			wantProvenance: "preset",
		},
		{
			name:           "explicit coarse ceiling",
			answers:        []string{"MAX_SESSION_CONTEXT_MULTIPLIER=12"},
			wantMultiplier: 12,
			wantPresent:    true,
			wantProvenance: "answer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := initOnboardingWorkflowBuilderGitRepository(t, "https://github.com/acme/api.git")
			answersPath := writeOnboardingWorkflowBuilderAnswers(t, onboardingWorkflowBuilderVariantAnswers(root, "github_local", "review_gate", tt.answers...))

			result, err := buildOnboardingWorkflow(context.Background(), onboardingBuildWorkflowConfig{
				AnswersPath: answersPath,
				Write:       false,
			})
			if err != nil {
				t.Fatalf("buildOnboardingWorkflow() error = %v", err)
			}
			workflow, err := workflowconfig.ParseWorkflow([]byte(result.Workflow))
			if err != nil {
				t.Fatalf("ParseWorkflow() error = %v\n%s", err, result.Workflow)
			}
			if workflow.Config.Agent.MaxSessionContextMultiplier != tt.wantMultiplier {
				t.Fatalf("MaxSessionContextMultiplier = %v, want %v", workflow.Config.Agent.MaxSessionContextMultiplier, tt.wantMultiplier)
			}
			gotPresent := strings.Contains(result.Workflow, "max_session_context_multiplier:")
			if gotPresent != tt.wantPresent {
				t.Fatalf("max_session_context_multiplier presence = %t, want %t\n%s", gotPresent, tt.wantPresent, result.Workflow)
			}
			assertOnboardingWorkflowDecision(t, result.Decisions, "agent.max_session_context_multiplier", tt.wantProvenance)
		})
	}
}

func TestBuildOnboardingWorkflowRendersReviewFlowVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		preset              string
		profile             string
		wantAutopilot       bool
		wantReviewPhrase    string
		wantAutopilotPhrase string
	}{
		{
			preset:              "project_v2",
			profile:             "review_gate",
			wantReviewPhrase:    "Move the issue to `Human Review` only after",
			wantAutopilotPhrase: "do not move the issue to `Human Review`",
		},
		{
			preset:              "project_v2",
			profile:             "full_autopilot",
			wantAutopilot:       true,
			wantReviewPhrase:    "Move the issue to `Human Review` only after",
			wantAutopilotPhrase: "do not move the issue to `Human Review`",
		},
		{
			preset:              "issue_field",
			profile:             "review_gate",
			wantReviewPhrase:    "Move the issue to `Human Review` only after",
			wantAutopilotPhrase: "do not move the issue to `Human Review`",
		},
		{
			preset:              "issue_field",
			profile:             "full_autopilot",
			wantAutopilot:       true,
			wantReviewPhrase:    "Move the issue to `Human Review` only after",
			wantAutopilotPhrase: "do not move the issue to `Human Review`",
		},
		{
			preset:              "label",
			profile:             "review_gate",
			wantReviewPhrase:    "Move the issue to `Human Review` only after",
			wantAutopilotPhrase: "do not move the issue to `Human Review`",
		},
		{
			preset:              "label",
			profile:             "full_autopilot",
			wantAutopilot:       true,
			wantReviewPhrase:    "Move the issue to `Human Review` only after",
			wantAutopilotPhrase: "do not move the issue to `Human Review`",
		},
		{
			preset:              "github_local",
			profile:             "review_gate",
			wantReviewPhrase:    "Move the local issue to `Human Review` only after",
			wantAutopilotPhrase: "do not move the local issue to `Human Review`",
		},
		{
			preset:              "github_local",
			profile:             "full_autopilot",
			wantAutopilot:       true,
			wantReviewPhrase:    "Move the local issue to `Human Review` only after",
			wantAutopilotPhrase: "do not move the local issue to `Human Review`",
		},
		{
			preset:              "non_code_artifact",
			profile:             "review_gate",
			wantReviewPhrase:    "move the work item to\n   `Review`",
			wantAutopilotPhrase: "do not move it to `Review`",
		},
		{
			preset:              "non_code_artifact",
			profile:             "full_autopilot",
			wantAutopilot:       true,
			wantReviewPhrase:    "move the work item to\n   `Review`",
			wantAutopilotPhrase: "do not move it to `Review`",
		},
	}

	for _, tt := range tests {
		t.Run(tt.preset+"/"+tt.profile, func(t *testing.T) {
			t.Parallel()

			root := initOnboardingWorkflowBuilderGitRepository(t, "https://github.com/acme/api.git")
			answersPath := writeOnboardingWorkflowBuilderAnswers(t, onboardingWorkflowBuilderVariantAnswers(root, tt.preset, tt.profile))

			result, err := buildOnboardingWorkflow(context.Background(), onboardingBuildWorkflowConfig{
				AnswersPath: answersPath,
				Write:       false,
			})
			if err != nil {
				t.Fatalf("buildOnboardingWorkflow() error = %v", err)
			}
			workflow, err := workflowconfig.ParseWorkflow([]byte(result.Workflow))
			if err != nil {
				t.Fatalf("ParseWorkflow() error = %v\n%s", err, result.Workflow)
			}

			cfg := workflow.Config
			if cfg.Agent.AutoPromote.Enabled != tt.wantAutopilot {
				t.Fatalf("AutoPromote.Enabled = %t, want %t\n%s", cfg.Agent.AutoPromote.Enabled, tt.wantAutopilot, result.Workflow)
			}
			if tt.wantAutopilot {
				if cfg.Agent.AutoPromote.QuietSeconds != 0 {
					t.Fatalf("QuietSeconds = %d, want 0", cfg.Agent.AutoPromote.QuietSeconds)
				}
				if cfg.Agent.AutoPromote.GateWaitState != workflowconfig.AutoPromoteGateWaitStateSource {
					t.Fatalf("GateWaitState = %q, want source", cfg.Agent.AutoPromote.GateWaitState)
				}
				assertOnboardingWorkflowContains(t, result.Workflow, tt.wantAutopilotPhrase)
				if strings.Contains(result.Workflow, tt.wantReviewPhrase) {
					t.Fatalf("autopilot workflow contains review handoff phrase %q:\n%s", tt.wantReviewPhrase, result.Workflow)
				}
			} else {
				if cfg.Agent.AutoPromote.QuietSeconds != 600 {
					t.Fatalf("QuietSeconds = %d, want 600", cfg.Agent.AutoPromote.QuietSeconds)
				}
				if cfg.Agent.AutoPromote.GateWaitState != workflowconfig.AutoPromoteGateWaitStateReview {
					t.Fatalf("GateWaitState = %q, want review", cfg.Agent.AutoPromote.GateWaitState)
				}
				assertOnboardingWorkflowContains(t, result.Workflow, tt.wantReviewPhrase)
				if strings.Contains(result.Workflow, tt.wantAutopilotPhrase) {
					t.Fatalf("review-gate workflow contains autopilot phrase %q:\n%s", tt.wantAutopilotPhrase, result.Workflow)
				}
			}

			assertOnboardingWorkflowContainsWords(t, result.Workflow, "`status` must be one of `in_progress`, `blocked`, or `complete`")
			for _, want := range []string{
				"status: in_progress",
				"status: blocked",
				"status: complete",
			} {
				assertOnboardingWorkflowContains(t, result.Workflow, want)
			}
		})
	}
}

func TestBuildOnboardingWorkflowRejectsMalformedAnswers(t *testing.T) {
	t.Parallel()

	root := initOnboardingWorkflowBuilderGitRepository(t, "https://github.com/acme/api.git")
	answersPath := writeOnboardingWorkflowBuilderAnswers(t, strings.Join([]string{
		"CUSTOMER_ID=acme",
		"DETENT_PROJECT_ID=api",
		"TARGET_REPOSITORY=acme/api",
		"TARGET_SOURCE_ROOT=" + root,
		"REFERENCE_REPOSITORIES=digitaldrywood/detent",
		"DETENT_ONBOARDING_MODE=add-project",
		"IDENTITY_CONFIRMED=true",
		"WORKFLOW_PRESET=github_local",
		"GATE_RUN make test",
		"",
	}, "\n"))

	_, err := buildOnboardingWorkflow(context.Background(), onboardingBuildWorkflowConfig{
		AnswersPath: answersPath,
		Write:       false,
	})
	if err == nil {
		t.Fatal("buildOnboardingWorkflow() error = nil, want malformed answer validation error")
	}
	if !strings.Contains(err.Error(), "line 9 must be KEY=VALUE") {
		t.Fatalf("buildOnboardingWorkflow() error = %v, want malformed answer problem", err)
	}
}

func TestBuildOnboardingWorkflowGeneratesParseablePausedProject(t *testing.T) {
	t.Parallel()

	root := initOnboardingWorkflowBuilderGitRepository(t, "https://github.com/acme/api.git")
	writeOnboardingWorkflowBuilderFile(t, filepath.Join(root, "go.mod"), "module example.com/api\n\ngo 1.26\n")
	writeOnboardingWorkflowBuilderFile(t, filepath.Join(root, "Makefile"), ".PHONY: check\ncheck:\n\tgo test ./...\n")
	answersPath := writeOnboardingWorkflowBuilderAnswers(t, strings.Join([]string{
		"CUSTOMER_ID=acme",
		"DETENT_PROJECT_ID=api",
		"TARGET_REPOSITORY=acme/api",
		"TARGET_SOURCE_ROOT=" + root,
		"REFERENCE_REPOSITORIES=digitaldrywood/detent",
		"DETENT_ONBOARDING_MODE=add-project",
		"IDENTITY_CONFIRMED=true",
		"WORKFLOW_PRESET=github_local",
		"DELIVERY_PROFILE=review_gate",
		"",
	}, "\n"))
	outputPath := filepath.Join(root, "WORKFLOW.md")

	result, err := buildOnboardingWorkflow(context.Background(), onboardingBuildWorkflowConfig{
		AnswersPath: answersPath,
		OutputPath:  outputPath,
		Write:       true,
	})
	if err != nil {
		t.Fatalf("buildOnboardingWorkflow() error = %v", err)
	}
	if !result.Written || result.Path != outputPath {
		t.Fatalf("result = %#v, want written workflow at %s", result, outputPath)
	}
	raw, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile(WORKFLOW.md) error = %v", err)
	}
	workflow, err := workflowconfig.ParseWorkflow(raw)
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v\n%s", err, string(raw))
	}
	if workflow.Config.Tracker.Kind != workflowconfig.TrackerGitHubLocal {
		t.Fatalf("Tracker.Kind = %q, want github_local", workflow.Config.Tracker.Kind)
	}
	if workflow.Config.Workspace.SourceRoot != root {
		t.Fatalf("Workspace.SourceRoot = %q, want %q", workflow.Config.Workspace.SourceRoot, root)
	}
	if workflow.Config.Gate.Run != "make check" {
		t.Fatalf("Gate.Run = %q, want make check", workflow.Config.Gate.Run)
	}
	if !workflow.Config.Budget.Enabled || workflow.Config.Budget.PerDayMaxUSD != 50 || workflow.Config.Budget.PerIssueMaxUSD != 5 {
		t.Fatalf("Budget = %#v, want enabled 50/day 5/issue", workflow.Config.Budget)
	}
	if workflow.Config.Gate.Validator.Model != "" {
		t.Fatalf("Gate.Validator.Model = %q, want provider or route default", workflow.Config.Gate.Validator.Model)
	}
	if strings.Contains(string(raw), "max_session_context_multiplier:") {
		t.Fatalf("WORKFLOW.md includes opt-in max_session_context_multiplier:\n%s", string(raw))
	}

	got, err := project.New(project.Config{
		Project: globalconfig.Project{
			ID:      "api",
			Workdir: root,
			Weight:  1,
			Paused:  true,
		},
		Workflow: workflow,
	}, project.Dependencies{
		GitHubToken: "test-token",
		Runner:      orchestrator.FakeRunner{},
		ConnectorFactory: func(workflowconfig.Config) (connector.Connector, error) {
			return memory.New(memory.Config{}), nil
		},
	})
	if err != nil {
		t.Fatalf("project.New() error = %v", err)
	}
	if err := got.Start(context.Background()); !errors.Is(err, project.ErrProjectPaused) {
		t.Fatalf("Start() error = %v, want ErrProjectPaused", err)
	}

	assertOnboardingWorkflowDecision(t, result.Decisions, "tracker.repository", "answer")
	assertOnboardingWorkflowDecision(t, result.Decisions, "gate.run", "probe")
	assertOnboardingWorkflowDecision(t, result.Decisions, "gate.validator.model", "preset")
	assertOnboardingWorkflowDecision(t, result.Decisions, "agent.max_session_tokens", "preset")
	assertOnboardingWorkflowDecision(t, result.Decisions, "agent.max_session_context_multiplier", "preset")
	assertOnboardingWorkflowDecision(t, result.Decisions, "answers.worker_model_mode", "preset")
	assertOnboardingWorkflowDecision(t, result.Decisions, "codex.command", "preset")
	for _, decision := range result.Decisions {
		switch decision.Provenance {
		case "answer", "probe", "preset":
		default:
			t.Fatalf("decision %#v has unsupported provenance", decision)
		}
		if strings.TrimSpace(decision.Why) == "" {
			t.Fatalf("decision %#v missing why", decision)
		}
	}
}

func assertOnboardingWorkflowDecision(t *testing.T, decisions []onboardingWorkflowDecision, path string, provenance string) {
	t.Helper()

	for _, decision := range decisions {
		if decision.Path == path {
			if decision.Provenance != provenance {
				t.Fatalf("decision %s provenance = %q, want %q", path, decision.Provenance, provenance)
			}
			return
		}
	}
	t.Fatalf("decision %s not found in %#v", path, decisions)
}

func assertOnboardingWorkflowContains(t *testing.T, text string, want string) {
	t.Helper()

	if !strings.Contains(text, want) {
		t.Fatalf("workflow missing %q:\n%s", want, text)
	}
}

func assertOnboardingWorkflowContainsWords(t *testing.T, text string, want string) {
	t.Helper()

	if !strings.Contains(strings.Join(strings.Fields(text), " "), strings.Join(strings.Fields(want), " ")) {
		t.Fatalf("workflow missing %q:\n%s", want, text)
	}
}

func onboardingWorkflowBuilderVariantAnswers(root string, preset string, profile string, extra ...string) string {
	lines := []string{
		"CUSTOMER_ID=acme",
		"DETENT_PROJECT_ID=api",
		"TARGET_REPOSITORY=acme/api",
		"TARGET_SOURCE_ROOT=" + root,
		"REFERENCE_REPOSITORIES=digitaldrywood/detent",
		"DETENT_ONBOARDING_MODE=add-project",
		"IDENTITY_CONFIRMED=true",
		"WORKFLOW_PRESET=" + preset,
		"DELIVERY_PROFILE=" + profile,
	}
	if preset == "project_v2" {
		lines = append(lines, "PROJECT_SLUG=PVT_test")
	}
	lines = append(lines, extra...)
	return strings.Join(append(lines, ""), "\n")
}

func initOnboardingWorkflowBuilderGitRepository(t *testing.T, remote string) string {
	t.Helper()

	dir := t.TempDir()
	runOnboardingWorkflowBuilderGit(t, dir, "init")
	runOnboardingWorkflowBuilderGit(t, dir, "remote", "add", "origin", remote)
	return dir
}

func runOnboardingWorkflowBuilderGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s error = %v\n%s", strings.Join(args, " "), err, string(output))
	}
}

func writeOnboardingWorkflowBuilderAnswers(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "answers.env")
	writeOnboardingWorkflowBuilderFile(t, path, content)
	return path
}

func writeOnboardingWorkflowBuilderFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}
