package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	workflowtemplates "github.com/digitaldrywood/detent/docs/templates"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	onboardingprofile "github.com/digitaldrywood/detent/internal/onboarding"
)

const (
	onboardingWorkflowWorkerModelProviderDefault  = "provider_default"
	onboardingWorkflowWorkerModelPinned           = "pinned"
	onboardingWorkflowDefaultSessionTokens        = int64(2000000)
	onboardingWorkflowDefaultSessionOverrideLabel = "allow-large-session"
)

type onboardingBuildWorkflowConfig struct {
	AnswersPath      string
	OutputPath       string
	TargetSourceRoot string
	Preset           string
	Write            bool
}

type onboardingBuildWorkflowResult struct {
	Status     string                       `json:"status"`
	Preset     string                       `json:"preset"`
	Path       string                       `json:"path"`
	ConfigPath string                       `json:"config_path"`
	Written    bool                         `json:"written"`
	Probe      onboardingRepoProbe          `json:"probe"`
	Decisions  []onboardingWorkflowDecision `json:"decisions"`
	Workflow   string                       `json:"workflow"`
	Config     string                       `json:"config"`
}

type onboardingWorkflowDecision struct {
	Path       string `json:"path"`
	Value      string `json:"value"`
	Provenance string `json:"provenance"`
	Why        string `json:"why"`
}

type onboardingWorkflowPreset struct {
	Name       string
	ConfigRaw  []byte
	PromptRaw  []byte
	Provenance string
	Why        string
}

type onboardingWorkflowWorkerModel struct {
	Mode       string
	Model      string
	Command    string
	Provenance string
	Why        string
	Comment    string
}

type onboardingWorkflowReviewFlow string

const (
	onboardingWorkflowReviewGateFlow onboardingWorkflowReviewFlow = "review_gate"
	onboardingWorkflowAutopilotFlow  onboardingWorkflowReviewFlow = "autopilot"
)

type onboardingWorkflowDecisionRecorder struct {
	decisions []onboardingWorkflowDecision
}

func newOnboardingBuildWorkflowCommand() *cobra.Command {
	var answersPath string
	var outputPath string
	var preset string
	var targetSourceRoot string
	var write bool

	cmd := &cobra.Command{
		Use:          "build-workflow",
		Short:        "Build detent.yaml and WORKFLOW.md from onboarding answers and repository probes",
		Example:      `detent onboarding build-workflow --answers "$ONBOARDING_DIR/answers.env" --output WORKFLOW.md --write`,
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			result, err := buildOnboardingWorkflow(cmd.Context(), onboardingBuildWorkflowConfig{
				AnswersPath:      answersPath,
				OutputPath:       outputPath,
				TargetSourceRoot: targetSourceRoot,
				Preset:           preset,
				Write:            write,
			})
			if err != nil {
				return err
			}
			return out.Write(func(w io.Writer) error {
				return writeOnboardingBuildWorkflowPretty(w, result)
			}, result)
		},
	}
	cmd.Flags().StringVar(&answersPath, "answers", "answers.env", "path to onboarding answers.env")
	cmd.Flags().StringVar(&outputPath, "output", "WORKFLOW.md", "WORKFLOW.md anchor path; detent.yaml is written beside it")
	cmd.Flags().StringVar(&targetSourceRoot, "target-source-root", "", "explicit target repository checkout root")
	cmd.Flags().StringVar(&preset, "preset", "", "workflow preset: project_v2, issue_field, label, github_local, or non_code_artifact")
	cmd.Flags().BoolVar(&write, "write", false, "write the generated detent.yaml and WORKFLOW.md")
	return cmd
}

func buildOnboardingWorkflow(ctx context.Context, cfg onboardingBuildWorkflowConfig) (onboardingBuildWorkflowResult, error) {
	answers, validation, err := readOnboardingWorkflowBuilderAnswers(ctx, cfg.AnswersPath)
	if err != nil {
		return onboardingBuildWorkflowResult{}, err
	}
	sourceRoot := strings.TrimSpace(cfg.TargetSourceRoot)
	if sourceRoot == "" {
		sourceRoot = validation.TargetSourceRoot
	}
	sourceRoot, err = resolveOnboardingGateSourceRoot(sourceRoot)
	if err != nil {
		return onboardingBuildWorkflowResult{}, err
	}
	probe, err := probeOnboardingRepository(sourceRoot)
	if err != nil {
		return onboardingBuildWorkflowResult{}, err
	}
	preset, err := selectOnboardingWorkflowPreset(cfg.Preset, answers)
	if err != nil {
		return onboardingBuildWorkflowResult{}, err
	}

	projectConfig, workflow, decisions, err := renderOnboardingWorkflow(ctx, preset, answers, validation, probe)
	if err != nil {
		return onboardingBuildWorkflowResult{}, err
	}

	outputPath := strings.TrimSpace(cfg.OutputPath)
	if outputPath == "" {
		outputPath = filepath.Join(sourceRoot, defaultWorkflowFile)
	}
	if !filepath.IsAbs(outputPath) {
		outputPath = filepath.Join(sourceRoot, outputPath)
	}
	outputPath = filepath.Clean(outputPath)
	projectConfigPath := workflowconfig.DefinitionPath(outputPath)
	if _, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{
		WorkflowPath: outputPath,
		Workflow:     []byte(workflow),
		ConfigPath:   projectConfigPath,
		Config:       []byte(projectConfig),
		HasConfig:    true,
	}); err != nil {
		return onboardingBuildWorkflowResult{}, fmt.Errorf("parse generated project definition: %w", err)
	}
	if cfg.Write {
		if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
			return onboardingBuildWorkflowResult{}, fmt.Errorf("create workflow directory %s: %w", filepath.Dir(outputPath), err)
		}
		if err := os.WriteFile(outputPath, []byte(workflow), 0o600); err != nil {
			return onboardingBuildWorkflowResult{}, fmt.Errorf("write workflow %s: %w", outputPath, err)
		}
		if err := os.WriteFile(projectConfigPath, []byte(projectConfig), 0o600); err != nil {
			return onboardingBuildWorkflowResult{}, fmt.Errorf("write project config %s: %w", projectConfigPath, err)
		}
	}

	return onboardingBuildWorkflowResult{
		Status:     "ok",
		Preset:     preset.Name,
		Path:       outputPath,
		ConfigPath: projectConfigPath,
		Written:    cfg.Write,
		Probe:      probe,
		Decisions:  decisions,
		Workflow:   workflow,
		Config:     projectConfig,
	}, nil
}

func readOnboardingWorkflowBuilderAnswers(ctx context.Context, path string) (onboardingAnswers, onboardingAnswersValidationResult, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return onboardingAnswers{}, onboardingAnswersValidationResult{}, NewValidationError(
			"--answers is required",
			`Run detent onboarding draft-answers --answers "$ONBOARDING_DIR/answers.env" --write, confirm identity, then rerun build-workflow.`,
			nil,
		)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return onboardingAnswers{}, onboardingAnswersValidationResult{}, NewValidationError(
				"answers file not found: "+path,
				`Run detent onboarding draft-answers --answers "$ONBOARDING_DIR/answers.env" --write, confirm identity, then rerun build-workflow.`,
				nil,
			)
		}
		return onboardingAnswers{}, onboardingAnswersValidationResult{}, fmt.Errorf("read onboarding answers %s: %w", path, err)
	}
	answers := parseOnboardingAnswers(raw)
	validation := onboardingAnswersValidationResult{
		Status: "ok",
		Path:   path,
		Phase:  onboardingAnswersPhaseIdentity,
	}
	problems := append([]string(nil), answers.Problems...)
	problems = append(problems, validateOnboardingIdentityAnswersWithContext(ctx, answers, &validation)...)
	if len(problems) > 0 {
		return onboardingAnswers{}, onboardingAnswersValidationResult{}, NewValidationError(
			strings.Join(problems, "; "),
			onboardingAnswersValidationHint(onboardingAnswersPhaseIdentity),
			nil,
		)
	}
	return answers, validation, nil
}

func selectOnboardingWorkflowPreset(override string, answers onboardingAnswers) (onboardingWorkflowPreset, error) {
	value := strings.TrimSpace(override)
	provenance := "answer"
	why := "selected from --preset"
	if value == "" {
		if answer, ok := onboardingWorkflowAnswer(answers, "WORKFLOW_PRESET"); ok {
			value = answer
			why = "selected from WORKFLOW_PRESET"
		}
	}
	if value == "" {
		if answer, ok := onboardingWorkflowAnswer(answers, "GITHUB_MODE"); ok {
			value = answer
			why = "selected from existing GITHUB_MODE answer"
		}
	}
	if value == "" {
		return onboardingWorkflowPreset{}, NewValidationError(
			"WORKFLOW_PRESET or GITHUB_MODE is required",
			"Record WORKFLOW_PRESET=github_local, WORKFLOW_PRESET=label, WORKFLOW_PRESET=issue_field, or WORKFLOW_PRESET=project_v2 in answers.env, or pass --preset.",
			nil,
		)
	}
	name, ok := normalizeOnboardingWorkflowPreset(value)
	if !ok {
		return onboardingWorkflowPreset{}, NewValidationError(
			"workflow preset must be project_v2, issue_field, label, github_local, or non_code_artifact",
			"Choose one of the docs/templates/WORKFLOW.*.md presets.",
			nil,
		)
	}
	promptRaw, err := fs.ReadFile(workflowtemplates.FS, "WORKFLOW."+name+".md")
	if err != nil {
		return onboardingWorkflowPreset{}, fmt.Errorf("read workflow preset %s: %w", name, err)
	}
	configRaw, err := fs.ReadFile(workflowtemplates.FS, "detent."+name+".yaml")
	if err != nil {
		return onboardingWorkflowPreset{}, fmt.Errorf("read project config preset %s: %w", name, err)
	}
	return onboardingWorkflowPreset{
		Name:       name,
		ConfigRaw:  configRaw,
		PromptRaw:  promptRaw,
		Provenance: provenance,
		Why:        why,
	}, nil
}

func normalizeOnboardingWorkflowPreset(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case workflowconfig.GitHubStatusSourceProjectV2, "projectv2", "project":
		return "project_v2", true
	case workflowconfig.GitHubStatusSourceIssueField, "issuefield", "issue-field":
		return "issue_field", true
	case workflowconfig.GitHubStatusSourceLabel, "labels", "issue_label", "issue-label":
		return "label", true
	case workflowconfig.TrackerGitHubLocal, "github-local", "local_github", "local-github":
		return "github_local", true
	case "non_code_artifact", "non-code-artifact", "artifact":
		return "non_code_artifact", true
	default:
		return "", false
	}
}

func renderOnboardingWorkflow(
	_ context.Context,
	preset onboardingWorkflowPreset,
	answers onboardingAnswers,
	validation onboardingAnswersValidationResult,
	probe onboardingRepoProbe,
) (string, string, []onboardingWorkflowDecision, error) {
	root, err := parseOnboardingWorkflowFrontmatter(preset.ConfigRaw)
	if err != nil {
		return "", "", nil, err
	}
	decisions := &onboardingWorkflowDecisionRecorder{}
	decisions.add("workflow.preset", preset.Name, preset.Provenance, preset.Why)

	reviewFlow, err := applyOnboardingWorkflowDecisions(root, preset.Name, answers, validation, probe, decisions)
	if err != nil {
		return "", "", nil, err
	}
	renderedPrompt := renderOnboardingWorkflowPrompt(preset.Name, preset.PromptRaw, reviewFlow)

	rawConfig, err := yaml.Marshal(root)
	if err != nil {
		return "", "", nil, fmt.Errorf("marshal project config: %w", err)
	}
	projectConfig := strings.TrimSpace(string(rawConfig)) + "\n"
	workflow := strings.TrimLeft(renderedPrompt, "\n")
	if !strings.HasSuffix(workflow, "\n") {
		workflow += "\n"
	}
	return projectConfig, workflow, decisions.decisions, nil
}

func parseOnboardingWorkflowFrontmatter(raw []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse workflow template frontmatter: %w", err)
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0], nil
	}
	return &yaml.Node{Kind: yaml.MappingNode}, nil
}

func onboardingWorkflowTemplateGateKind(root *yaml.Node) string {
	gateNode := onboardingYAMLMappingValue(root, "gate")
	if gateNode == nil {
		return gate.KindCommand
	}
	kindNode := onboardingYAMLMappingValue(gateNode, "kind")
	if kindNode == nil {
		return gate.KindCommand
	}
	switch strings.TrimSpace(kindNode.Value) {
	case gate.KindArtifact:
		return gate.KindArtifact
	default:
		return gate.KindCommand
	}
}

func applyOnboardingWorkflowDecisions(
	root *yaml.Node,
	preset string,
	answers onboardingAnswers,
	validation onboardingAnswersValidationResult,
	probe onboardingRepoProbe,
	decisions *onboardingWorkflowDecisionRecorder,
) (onboardingWorkflowReviewFlow, error) {
	profile, profileProvenance, profileWhy, err := onboardingWorkflowDeliveryProfile(answers)
	if err != nil {
		return "", err
	}
	profileAnswers, _ := onboardingprofile.DeliveryProfileAnswerExpansion(profile)
	decisions.add("answers.delivery_profile", profile, profileProvenance, profileWhy)

	templateGateKind := onboardingWorkflowTemplateGateKind(root)
	sourceRoot := probe.SourceRoot
	worktreeRoot, worktreeProvenance, worktreeWhy := onboardingWorkflowStringDecision(
		answers,
		nil,
		"WORKTREE_ROOT",
		filepath.Join(sourceRoot, ".detent", "workspaces"),
		"preset",
		"recommended isolated worktree root under the target checkout",
	)
	gateRun, gateRunProvenance, gateRunWhy := onboardingWorkflowStringDecision(
		answers,
		nil,
		"GATE_RUN",
		probe.ValidationCommand,
		"probe",
		probe.ValidationCommandSource,
	)
	if gateRun == "" {
		gateRun = "make check"
		gateRunProvenance = "preset"
		gateRunWhy = "template command gate default"
	}

	kanbanMode, kanbanProvenance, kanbanWhy := onboardingWorkflowStringDecision(answers, profileAnswers, "KANBAN_MODE", "integration", "preset", "recommended interactive Kanban mode")
	autoPromote, autoPromoteProvenance, autoPromoteWhy, err := onboardingWorkflowBoolDecision(answers, profileAnswers, "AUTO_PROMOTE_ENABLED", false, "preset", "review gate preset stops in Human Review")
	if err != nil {
		return "", err
	}
	quietSeconds, quietProvenance, quietWhy, err := onboardingWorkflowIntDecision(answers, profileAnswers, "AUTO_PROMOTE_QUIET_SECONDS", 600, "preset", "recommended quiet window before automated promotion")
	if err != nil {
		return "", err
	}
	gateWaitFallback := workflowconfig.AutoPromoteGateWaitStateReview
	gateWaitFallbackWhy := "review-gate handoff parks completed work in the review state"
	if autoPromote && quietSeconds == 0 {
		gateWaitFallback = workflowconfig.AutoPromoteGateWaitStateSource
		gateWaitFallbackWhy = "zero-quiet auto-promote keeps completed work in the active state while gates finish"
	}
	gateWaitState, gateWaitProvenance, gateWaitWhy := onboardingWorkflowStringDecision(
		answers,
		profileAnswers,
		"AUTO_PROMOTE_GATE_WAIT_STATE",
		gateWaitFallback,
		"preset",
		gateWaitFallbackWhy,
	)
	gateWaitState, ok := normalizeOnboardingWorkflowGateWaitState(gateWaitState)
	if !ok {
		return "", NewValidationError(
			"AUTO_PROMOTE_GATE_WAIT_STATE must be source or review",
			"Use source for zero-quiet autopilot or review for review-state handoff.",
			nil,
		)
	}
	gateWaitTimeout, gateWaitTimeoutProvenance, gateWaitTimeoutWhy, err := onboardingWorkflowIntDecision(
		answers,
		profileAnswers,
		"AUTO_PROMOTE_GATE_WAIT_TIMEOUT_SECONDS",
		workflowconfig.DefaultAutoPromoteGateWaitTimeoutSeconds,
		"preset",
		"bounds active-lane gate waits before moving to the review state",
	)
	if err != nil {
		return "", err
	}
	requireAutomatedReview, reviewProvenance, reviewWhy, err := onboardingWorkflowBoolDecision(answers, profileAnswers, "GATE_REQUIRE_AUTOMATED_REVIEW", true, "preset", "requires automated review unless profile says otherwise")
	if err != nil {
		return "", err
	}
	dependencyAutoUnblock, dependencyProvenance, dependencyWhy, err := onboardingWorkflowBoolDecision(answers, profileAnswers, "DEPENDENCY_AUTO_UNBLOCK_ENABLED", false, "preset", "dependency waits remain human-controlled by default")
	if err != nil {
		return "", err
	}
	mergingConcurrency, mergingProvenance, mergingWhy, err := onboardingWorkflowIntDecision(answers, profileAnswers, "MERGING_CONCURRENCY", 1, "preset", "Merging is serialized")
	if err != nil {
		return "", err
	}
	maxConcurrentAgents, maxConcurrentProvenance, maxConcurrentWhy, err := onboardingWorkflowIntDecision(answers, nil, "MAX_CONCURRENT_AGENTS", 5, "preset", "balanced default project concurrency")
	if err != nil {
		return "", err
	}
	maxTurns, maxTurnsProvenance, maxTurnsWhy, err := onboardingWorkflowIntDecision(answers, nil, "MAX_TURNS", 20, "preset", "session turn ceiling guardrail")
	if err != nil {
		return "", err
	}
	dayBudget, dayBudgetProvenance, dayBudgetWhy, err := onboardingWorkflowFloatDecision(answers, "BUDGET_PER_DAY_MAX_USD", 50.0, "preset", "recommended daily spend cap")
	if err != nil {
		return "", err
	}
	issueBudget, issueBudgetProvenance, issueBudgetWhy, err := onboardingWorkflowFloatDecision(answers, "BUDGET_PER_ISSUE_MAX_USD", 5.0, "preset", "recommended per-issue spend cap")
	if err != nil {
		return "", err
	}
	validatorEnabled, validatorEnabledProvenance, validatorEnabledWhy, err := onboardingWorkflowBoolDecision(answers, nil, "VALIDATOR_ENABLED", false, "preset", "validator is explicit opt-in")
	if err != nil {
		return "", err
	}
	validatorModel, validatorModelProvenance, validatorModelWhy := onboardingWorkflowStringDecision(answers, nil, "VALIDATOR_MODEL", "", "preset", "inherits the validator route or provider default unless explicitly pinned")
	validatorMinScore, validatorScoreProvenance, validatorScoreWhy, err := onboardingWorkflowFloatDecision(answers, "VALIDATOR_MIN_SCORE", 0.8, "preset", "recommended validator confidence threshold")
	if err != nil {
		return "", err
	}
	validatorBlockOn, validatorBlockProvenance, validatorBlockWhy := onboardingWorkflowListDecision(answers, "VALIDATOR_BLOCK_ON", []string{"p1"}, "preset", "block promotion on P1 validator findings")
	workerModel, err := onboardingWorkflowWorkerModelDecision(answers)
	if err != nil {
		return "", err
	}
	sessionTokens, sessionTokensProvenance, sessionTokensWhy, err := onboardingWorkflowInt64Decision(answers, "MAX_SESSION_TOKENS", onboardingWorkflowDefaultSessionTokens, "preset", "recommended per-session token ceiling")
	if err != nil {
		return "", err
	}
	sessionMultiplierRaw, sessionMultiplierSet := onboardingWorkflowAnswer(answers, "MAX_SESSION_CONTEXT_MULTIPLIER")
	var sessionMultiplier float64
	if sessionMultiplierSet {
		sessionMultiplier, err = strconv.ParseFloat(sessionMultiplierRaw, 64)
		if err != nil {
			return "", NewValidationError("MAX_SESSION_CONTEXT_MULTIPLIER must be a number", "Use a numeric value only when explicitly opting into the coarse context multiplier.", nil)
		}
	}
	sessionOverride, sessionOverrideProvenance, sessionOverrideWhy := onboardingWorkflowStringDecision(answers, nil, "MAX_SESSION_TOKEN_OVERRIDE_LABEL", onboardingWorkflowDefaultSessionOverrideLabel, "preset", "explicit per-issue escape hatch for large sessions")

	decisions.set(root, "tracker.repository", validation.TargetRepository, "answer", "TARGET_REPOSITORY")
	decisions.set(root, "workspace.source_root", sourceRoot, "answer", "TARGET_SOURCE_ROOT")
	decisions.set(root, "workspace.root", worktreeRoot, worktreeProvenance, worktreeWhy)
	if preset != "non_code_artifact" {
		decisions.set(root, "deliverable.merge_method", workflowconfig.MergeMethodSquash, "preset", "default pull request merge strategy")
	}
	decisions.set(root, "agent.max_concurrent_agents", maxConcurrentAgents, maxConcurrentProvenance, maxConcurrentWhy)
	decisions.set(root, "agent.max_turns", maxTurns, maxTurnsProvenance, maxTurnsWhy)
	decisions.set(root, "agent.max_retry_backoff_ms", 300000, "preset", "recommended retry backoff ceiling")
	decisions.set(root, "agent.max_session_tokens", sessionTokens, sessionTokensProvenance, sessionTokensWhy)
	if sessionMultiplierSet {
		decisions.set(root, "agent.max_session_context_multiplier", sessionMultiplier, "answer", "MAX_SESSION_CONTEXT_MULTIPLIER explicitly opts into a coarse context ceiling")
	} else {
		deleteOnboardingYAMLPath(root, []string{"agent", "max_session_context_multiplier"})
		decisions.add("agent.max_session_context_multiplier", "omitted", "preset", "max_session_tokens is the default runaway brake; the coarse context multiplier is opt-in")
	}
	decisions.set(root, "agent.max_session_token_override_label", sessionOverride, sessionOverrideProvenance, sessionOverrideWhy)
	decisions.set(root, "agent.max_concurrent_agents_by_state.Merging", mergingConcurrency, mergingProvenance, mergingWhy)
	decisions.set(root, "agent.auto_promote.enabled", autoPromote, autoPromoteProvenance, autoPromoteWhy)
	decisions.set(root, "agent.auto_promote.quiet_seconds", quietSeconds, quietProvenance, quietWhy)
	decisions.set(root, "agent.auto_promote.gate_wait_state", gateWaitState, gateWaitProvenance, gateWaitWhy)
	decisions.set(root, "agent.auto_promote.gate_wait_timeout_seconds", gateWaitTimeout, gateWaitTimeoutProvenance, gateWaitTimeoutWhy)
	decisions.set(root, "tracker.dependency_auto_unblock.enabled", dependencyAutoUnblock, dependencyProvenance, dependencyWhy)
	if templateGateKind == gate.KindArtifact {
		decisions.add("gate.kind", gate.KindArtifact, "preset", "artifact validation gate from selected preset")
	} else {
		decisions.set(root, "gate.kind", gate.KindCommand, "preset", "command validation gate")
		decisions.set(root, "gate.run", gateRun, gateRunProvenance, gateRunWhy)
		decisions.set(root, "gate.require_automated_review", requireAutomatedReview, reviewProvenance, reviewWhy)
	}
	decisions.set(root, "gate.ci_failure_action", "rework", "preset", "failed current-head CI routes to Rework")
	decisions.set(root, "gate.transient_ci_retry_limit", 2, "preset", "limits transient CI rework retries")
	decisions.set(root, "gate.validator.enabled", validatorEnabled, validatorEnabledProvenance, validatorEnabledWhy)
	decisions.set(root, "gate.validator.model", validatorModel, validatorModelProvenance, validatorModelWhy)
	decisions.set(root, "gate.validator.min_score", validatorMinScore, validatorScoreProvenance, validatorScoreWhy)
	decisions.set(root, "gate.validator.block_on", validatorBlockOn, validatorBlockProvenance, validatorBlockWhy)
	decisions.add("answers.worker_model_mode", workerModel.Mode, workerModel.Provenance, workerModel.Why)
	if workerModel.Model != "" {
		decisions.add("answers.worker_model", workerModel.Model, "answer", "WORKER_MODEL")
	}
	decisions.setWithComments(
		root,
		"codex.command",
		workerModel.Command,
		workerModel.Provenance,
		workerModel.Why,
		"Optional model_reasoning_effort is unset because not every model accepts it.",
		workerModel.Comment,
	)
	decisions.set(root, "plan.enabled", false, "preset", "direct implementation dispatch by default")
	decisions.set(root, "plan.review", "human", "preset", "human plan approval if plan mode is enabled later")
	decisions.set(root, "server.kanban.mode", kanbanMode, kanbanProvenance, kanbanWhy)
	decisions.set(root, "budget.billing_mode", workflowconfig.BillingModeMetered, "preset", "enforce USD guardrails for metered API billing")
	decisions.set(root, "budget.enabled", true, "preset", "enable spend guardrails at onboarding")
	decisions.set(root, "budget.per_day_max_usd", dayBudget, dayBudgetProvenance, dayBudgetWhy)
	decisions.set(root, "budget.per_issue_max_usd", issueBudget, issueBudgetProvenance, issueBudgetWhy)
	decisions.set(root, "budget.refusal_cooldown_seconds", 3600, "preset", "cool down budget refusals before retry")
	decisions.set(root, "budget.pricing_path", "priv/pricing/models.yaml", "preset", "default pricing table path")

	switch preset {
	case "project_v2":
		projectSlug, ok := onboardingWorkflowAnswer(answers, "PROJECT_SLUG", "PROJECT_NODE_ID")
		if !ok {
			return "", NewValidationError(
				"PROJECT_SLUG is required for WORKFLOW_PRESET=project_v2",
				"Record the GitHub ProjectV2 node id as PROJECT_SLUG in answers.env or choose a boardless preset.",
				nil,
			)
		}
		decisions.set(root, "tracker.github_status_source", workflowconfig.GitHubStatusSourceProjectV2, "answer", "WORKFLOW_PRESET/GITHUB_MODE selected ProjectV2")
		decisions.set(root, "tracker.project_slug", projectSlug, "answer", "PROJECT_SLUG")
	case "issue_field":
		statusField, statusFieldProvenance, statusFieldWhy := onboardingWorkflowStringDecision(answers, nil, "STATUS_FIELD_NAME", "Status", "preset", "default GitHub issue field name")
		decisions.set(root, "tracker.github_status_source", workflowconfig.GitHubStatusSourceIssueField, "answer", "WORKFLOW_PRESET/GITHUB_MODE selected issue_field")
		decisions.set(root, "tracker.status_field", statusField, statusFieldProvenance, statusFieldWhy)
	case "label":
		labelPrefix, labelPrefixProvenance, labelPrefixWhy := onboardingWorkflowStringDecision(answers, nil, "STATUS_LABEL_PREFIX", "detent:", "preset", "default Detent status label prefix")
		decisions.set(root, "tracker.github_status_source", workflowconfig.GitHubStatusSourceLabel, "answer", "WORKFLOW_PRESET/GITHUB_MODE selected label")
		decisions.set(root, "tracker.status_label_prefix", labelPrefix, labelPrefixProvenance, labelPrefixWhy)
	case "github_local":
		decisions.set(root, "tracker.kind", workflowconfig.TrackerGitHubLocal, "answer", "WORKFLOW_PRESET selected github_local")
		decisions.set(root, "tracker.local_sqlite.project_id", validation.DetentProjectID, "answer", "DETENT_PROJECT_ID")
	case "non_code_artifact":
		decisions.set(root, "tracker.local_sqlite.project_id", validation.DetentProjectID, "answer", "DETENT_PROJECT_ID")
		decisions.set(root, "workspace.kind", workflowconfig.WorkspaceFilesystem, "preset", "artifact preset uses filesystem workspaces")
	}
	if writeProbeIssue, ok := onboardingWorkflowAnswer(answers, "WRITE_PROBE_ISSUE"); ok {
		decisions.set(root, "tracker.write_probe_issue", writeProbeIssue, "answer", "WRITE_PROBE_ISSUE")
	} else if preset != "github_local" && preset != "non_code_artifact" {
		deleteOnboardingYAMLPath(root, []string{"tracker", "write_probe_issue"})
		decisions.add("tracker.write_probe_issue", "omitted", "preset", "legacy/deep issue-object probes require an explicit issue answer")
	}
	return onboardingWorkflowReviewFlowFor(autoPromote, quietSeconds, gateWaitState), nil
}

func onboardingWorkflowDeliveryProfile(answers onboardingAnswers) (string, string, string, error) {
	value, ok := onboardingWorkflowAnswer(answers, "DELIVERY_PROFILE")
	if !ok {
		return onboardingprofile.DeliveryProfileReviewGate, "preset", "review_gate is the conservative workflow-builder default", nil
	}
	profile := onboardingprofile.NormalizeDeliveryProfile(value)
	if _, ok := onboardingprofile.DeliveryProfile(profile); !ok {
		return "", "", "", NewValidationError(
			"DELIVERY_PROFILE must be full_autopilot, review_gate, or conservative_manual",
			"Record a valid DELIVERY_PROFILE or omit it to use the builder default.",
			nil,
		)
	}
	return profile, "answer", "DELIVERY_PROFILE", nil
}

func onboardingWorkflowWorkerModelDecision(answers onboardingAnswers) (onboardingWorkflowWorkerModel, error) {
	rawMode, provenance, why := onboardingWorkflowStringDecision(
		answers,
		nil,
		"WORKER_MODEL_MODE",
		onboardingWorkflowWorkerModelProviderDefault,
		"preset",
		"provider default follows upgrades and avoids retirement breakage",
	)
	mode, ok := normalizeOnboardingWorkflowWorkerModelMode(rawMode)
	if !ok {
		return onboardingWorkflowWorkerModel{}, NewValidationError(
			"WORKER_MODEL_MODE must be provider_default or pinned",
			"Use provider_default to follow provider upgrades, or pinned with WORKER_MODEL for reproducibility or cost control.",
			nil,
		)
	}
	model, hasModel := onboardingWorkflowAnswer(answers, "WORKER_MODEL")
	switch mode {
	case onboardingWorkflowWorkerModelProviderDefault:
		if hasModel {
			return onboardingWorkflowWorkerModel{}, NewValidationError(
				"WORKER_MODEL must be omitted when WORKER_MODEL_MODE=provider_default",
				"Remove WORKER_MODEL to inherit the provider default, or set WORKER_MODEL_MODE=pinned.",
				nil,
			)
		}
		return onboardingWorkflowWorkerModel{
			Mode:       mode,
			Command:    "codex app-server",
			Provenance: provenance,
			Why:        why,
			Comment:    "Provider default: upgrades automatically and avoids retirement breakage.",
		}, nil
	case onboardingWorkflowWorkerModelPinned:
		if !hasModel {
			return onboardingWorkflowWorkerModel{}, NewValidationError(
				"WORKER_MODEL is required when WORKER_MODEL_MODE=pinned",
				"Record the exact provider-supported model identifier, and plan to update it before retirement.",
				nil,
			)
		}
		if !validOnboardingWorkflowModel(model) {
			return onboardingWorkflowWorkerModel{}, NewValidationError(
				"WORKER_MODEL contains unsupported characters",
				"Use a model identifier containing only letters, digits, dot, underscore, colon, slash, or hyphen.",
				nil,
			)
		}
		return onboardingWorkflowWorkerModel{
			Mode:       mode,
			Model:      model,
			Command:    fmt.Sprintf(`codex app-server --config 'model="%s"'`, model),
			Provenance: provenance,
			Why:        why,
			Comment:    "Pinned for reproducibility or cost control; update before retirement.",
		}, nil
	default:
		return onboardingWorkflowWorkerModel{}, errors.New("unreachable worker model mode")
	}
}

func normalizeOnboardingWorkflowWorkerModelMode(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case onboardingWorkflowWorkerModelProviderDefault, "provider-default", "default", "unpinned":
		return onboardingWorkflowWorkerModelProviderDefault, true
	case onboardingWorkflowWorkerModelPinned, "pin":
		return onboardingWorkflowWorkerModelPinned, true
	default:
		return "", false
	}
}

func validOnboardingWorkflowModel(model string) bool {
	if model == "" {
		return false
	}
	for _, char := range model {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("._:/-", char) {
			continue
		}
		return false
	}
	return true
}

func onboardingWorkflowAnswer(answers onboardingAnswers, keys ...string) (string, bool) {
	for _, key := range keys {
		value := strings.TrimSpace(answers.Values[key])
		if value != "" {
			return value, true
		}
	}
	return "", false
}

func onboardingWorkflowStringDecision(answers onboardingAnswers, profile map[string]string, key string, fallback string, fallbackProvenance string, fallbackWhy string) (string, string, string) {
	if value, ok := onboardingWorkflowAnswer(answers, key); ok {
		return value, "answer", key
	}
	if value := strings.TrimSpace(profile[key]); value != "" {
		return value, "answer", "DELIVERY_PROFILE expands " + key
	}
	return fallback, fallbackProvenance, fallbackWhy
}

func onboardingWorkflowBoolDecision(answers onboardingAnswers, profile map[string]string, key string, fallback bool, fallbackProvenance string, fallbackWhy string) (bool, string, string, error) {
	raw, provenance, why := onboardingWorkflowStringDecision(answers, profile, key, strconv.FormatBool(fallback), fallbackProvenance, fallbackWhy)
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, "", "", NewValidationError(key+" must be true or false", "Use true or false for boolean onboarding answers.", nil)
	}
	return value, provenance, why, nil
}

func onboardingWorkflowIntDecision(answers onboardingAnswers, profile map[string]string, key string, fallback int, fallbackProvenance string, fallbackWhy string) (int, string, string, error) {
	raw, provenance, why := onboardingWorkflowStringDecision(answers, profile, key, strconv.Itoa(fallback), fallbackProvenance, fallbackWhy)
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, "", "", NewValidationError(key+" must be an integer", "Use a whole number for integer onboarding answers.", nil)
	}
	return value, provenance, why, nil
}

func onboardingWorkflowInt64Decision(answers onboardingAnswers, key string, fallback int64, fallbackProvenance string, fallbackWhy string) (int64, string, string, error) {
	raw, provenance, why := onboardingWorkflowStringDecision(answers, nil, key, strconv.FormatInt(fallback, 10), fallbackProvenance, fallbackWhy)
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, "", "", NewValidationError(key+" must be an integer", "Use a whole number for integer onboarding answers.", nil)
	}
	return value, provenance, why, nil
}

func onboardingWorkflowFloatDecision(answers onboardingAnswers, key string, fallback float64, fallbackProvenance string, fallbackWhy string) (float64, string, string, error) {
	raw, provenance, why := onboardingWorkflowStringDecision(answers, nil, key, strconv.FormatFloat(fallback, 'f', -1, 64), fallbackProvenance, fallbackWhy)
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, "", "", NewValidationError(key+" must be a number", "Use a numeric value for budget and threshold onboarding answers.", nil)
	}
	return value, provenance, why, nil
}

func onboardingWorkflowListDecision(answers onboardingAnswers, key string, fallback []string, fallbackProvenance string, fallbackWhy string) ([]string, string, string) {
	if raw, ok := onboardingWorkflowAnswer(answers, key); ok {
		return splitOnboardingWorkflowList(raw), "answer", key
	}
	return append([]string(nil), fallback...), fallbackProvenance, fallbackWhy
}

func splitOnboardingWorkflowList(raw string) []string {
	var values []string
	for _, part := range strings.Split(raw, ",") {
		value := strings.TrimSpace(part)
		if value != "" {
			values = append(values, value)
		}
	}
	sort.Strings(values)
	return values
}

func normalizeOnboardingWorkflowGateWaitState(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case workflowconfig.AutoPromoteGateWaitStateSource:
		return workflowconfig.AutoPromoteGateWaitStateSource, true
	case workflowconfig.AutoPromoteGateWaitStateReview:
		return workflowconfig.AutoPromoteGateWaitStateReview, true
	default:
		return "", false
	}
}

func onboardingWorkflowReviewFlowFor(autoPromote bool, quietSeconds int, gateWaitState string) onboardingWorkflowReviewFlow {
	if autoPromote && quietSeconds == 0 && gateWaitState == workflowconfig.AutoPromoteGateWaitStateSource {
		return onboardingWorkflowAutopilotFlow
	}
	return onboardingWorkflowReviewGateFlow
}

func renderOnboardingWorkflowPrompt(preset string, prompt []byte, flow onboardingWorkflowReviewFlow) string {
	text := strings.TrimPrefix(string(prompt), "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimLeft(text, "\n")
	section := onboardingWorkflowExecutionFlow(preset, flow)
	if section == "" {
		return text
	}
	return replaceOnboardingWorkflowSection(text, "## Required Execution Flow", section)
}

func replaceOnboardingWorkflowSection(text string, heading string, replacement string) string {
	if strings.HasPrefix(text, heading+"\n") {
		return replacement
	}
	marker := "\n" + heading + "\n"
	index := strings.Index(text, marker)
	if index < 0 {
		return strings.TrimRight(text, "\n") + "\n\n" + replacement
	}
	return strings.TrimRight(text[:index], "\n") + "\n\n" + replacement
}

func onboardingWorkflowExecutionFlow(preset string, flow onboardingWorkflowReviewFlow) string {
	switch preset {
	case "github_local":
		return onboardingWorkflowGitHubLocalExecutionFlow(flow)
	case "non_code_artifact":
		return onboardingWorkflowArtifactExecutionFlow(flow)
	default:
		return onboardingWorkflowGitHubExecutionFlow(flow)
	}
}

func onboardingWorkflowGitHubExecutionFlow(flow onboardingWorkflowReviewFlow) string {
	if flow == onboardingWorkflowAutopilotFlow {
		return markdownLines(
			"## Required Execution Flow",
			"",
			"This workflow uses the autopilot handoff: `agent.auto_promote.enabled: true`,",
			"`quiet_seconds: 0`, and `gate_wait_state: source`. Completed agents leave",
			"issues in the active lane, set the Workpad `detent-status` block to",
			"`status: complete`, and let Detent promote eligible issues to `Merging`",
			"when the PR gate is green. Do not self-move issues to `Human Review`.",
			"",
			"Use the current Detent state as the source of truth for which section applies.",
			"",
			"### For Todo",
			"",
			"1. Move the issue to `In Progress`.",
			"2. Create or update the persistent `## Codex Workpad` comment with the plan,",
			"   acceptance criteria, validation plan, and the `in_progress`",
			"   `detent-status` block shown above.",
			"3. Fetch current `origin/main`, confirm this worktree is based on it, and",
			"   confirm every native dependency relation, `detent-status` blocker, and",
			"   issue-body `Depends on:` reference is merged or otherwise terminal before",
			"   coding.",
			"4. Reproduce or confirm the reported behavior before changing code when the",
			"   issue is a bug.",
			"5. Implement the smallest complete change that satisfies the issue.",
			"6. Run focused tests for touched packages, then run the configured validation",
			"   gate.",
			"7. Commit and push the branch.",
			"8. Open or update a pull request that references the issue.",
			"9. Re-check pull request comments, inline review comments, and CI after the",
			"   latest push.",
			"10. If the PR is open, not a draft, references the issue, validation is green,",
			"    and no actionable review comments remain, leave the issue in `In Progress`,",
			"    update the Workpad block to `status: complete` with `blockers: []` and",
			"    `human_action: null`, and do not move the issue to `Human Review`.",
			"",
			"### For In Progress",
			"",
			"1. Re-read the issue, pull request, comments, and `## Codex Workpad`, including",
			"   the `detent-status` block.",
			"2. Continue from the current repository and tracker state.",
			"3. If implementation is complete, run the full pre-review gate, update the",
			"   Workpad block to `status: complete` with `blockers: []` and",
			"   `human_action: null`, leave the issue in `In Progress`, and do not move the",
			"   issue to `Human Review`.",
			"",
			"### For Rework",
			"",
			"1. Re-read all human and bot feedback.",
			"2. Move the issue to `In Progress`.",
			"3. Fix the requested changes.",
			"4. Push updates to the pull request.",
			"5. Run the full pre-review gate again.",
			"6. When the gate passes, leave the issue in `In Progress`, update the Workpad",
			"   block to `status: complete`, and do not move the issue to `Human Review`.",
			"",
			"### For Merging",
			"",
			"1. Confirm `$go-workflow:ship` is available in the Codex environment. If it is",
			"   unavailable, keep the issue in `Merging` and record the missing ship workflow",
			"   as `human_action` in the `detent-status` block.",
			"2. Invoke and follow `$go-workflow:ship`.",
			"3. Do not call `gh pr merge` directly outside the ship workflow.",
			"4. End with exactly one terminal outcome:",
			"   - pull request merged and issue moved to `Done`;",
			"   - issue moved to `Rework` with an actionable defect;",
			"   - issue remains in `Merging` with a concrete external blocker recorded in",
			"     the `detent-status` block and described in the `## Codex Workpad`.",
			"5. Move the issue to `Done` only after the pull request is merged.",
		)
	}

	return markdownLines(
		"## Required Execution Flow",
		"",
		"This workflow uses the review-gate handoff: completed agents move issues to",
		"`Human Review` after the PR gate is ready, and a human or quiet-period",
		"auto-promote advances eligible issues to `Merging`.",
		"",
		"Use the current Detent state as the source of truth for which section applies.",
		"",
		"### For Todo",
		"",
		"1. Move the issue to `In Progress`.",
		"2. Create or update the persistent `## Codex Workpad` comment with the plan,",
		"   acceptance criteria, validation plan, and the `in_progress`",
		"   `detent-status` block shown above.",
		"3. Fetch current `origin/main`, confirm this worktree is based on it, and",
		"   confirm every native dependency relation, `detent-status` blocker, and",
		"   issue-body `Depends on:` reference is merged or otherwise terminal before",
		"   coding.",
		"4. Reproduce or confirm the reported behavior before changing code when the",
		"   issue is a bug.",
		"5. Implement the smallest complete change that satisfies the issue.",
		"6. Run focused tests for touched packages, then run the configured validation",
		"   gate.",
		"7. Commit and push the branch.",
		"8. Open or update a pull request that references the issue.",
		"9. Re-check pull request comments, inline review comments, and CI after the",
		"   latest push.",
		"10. Move the issue to `Human Review` only after the pull request is open, not a",
		"    draft, references the issue, validation is green, and no actionable review",
		"    comments remain.",
		"",
		"### For In Progress",
		"",
		"1. Re-read the issue, pull request, comments, and `## Codex Workpad`, including",
		"   the `detent-status` block.",
		"2. Continue from the current repository and tracker state.",
		"3. If implementation is complete, run the full pre-review gate, update the",
		"   Workpad block to `status: complete` with `blockers: []` and",
		"   `human_action: null`, and move the issue to `Human Review` only when the",
		"   gate passes.",
		"",
		"### For Rework",
		"",
		"1. Re-read all human and bot feedback.",
		"2. Move the issue to `In Progress`.",
		"3. Fix the requested changes.",
		"4. Push updates to the pull request.",
		"5. Run the full pre-review gate again.",
		"6. Move the issue back to `Human Review` only when the gate passes.",
		"",
		"### For Merging",
		"",
		"1. Confirm `$go-workflow:ship` is available in the Codex environment. If it is",
		"   unavailable, keep the issue in `Merging` and record the missing ship workflow",
		"   as `human_action` in the `detent-status` block.",
		"2. Invoke and follow `$go-workflow:ship`.",
		"3. Do not call `gh pr merge` directly outside the ship workflow.",
		"4. End with exactly one terminal outcome:",
		"   - pull request merged and issue moved to `Done`;",
		"   - issue moved to `Rework` with an actionable defect;",
		"   - issue remains in `Merging` with a concrete external blocker recorded in",
		"     the `detent-status` block and described in the `## Codex Workpad`.",
		"5. Move the issue to `Done` only after the pull request is merged.",
	)
}

func onboardingWorkflowGitHubLocalExecutionFlow(flow onboardingWorkflowReviewFlow) string {
	return strings.NewReplacer(
		"issues", "local issues",
		"issue", "local issue",
		"pull request", "pull request",
	).Replace(onboardingWorkflowGitHubExecutionFlow(flow))
}

func onboardingWorkflowArtifactExecutionFlow(flow onboardingWorkflowReviewFlow) string {
	if flow == onboardingWorkflowAutopilotFlow {
		return markdownLines(
			"## Required Execution Flow",
			"",
			"This workflow uses the artifact autopilot handoff: `agent.auto_promote.enabled:",
			"true`, `quiet_seconds: 0`, and `gate_wait_state: source`. Completed agents keep",
			"the work item in `Production`, set the Workpad `detent-status` block to",
			"`status: complete`, set `render_status` to `valid` when the artifact gate is",
			"satisfied, and let Detent promote the item to `Ready for Pickup`. Do not",
			"self-move work items to `Review`.",
			"",
			"### For Todo",
			"",
			"1. Move the work item to `Production`.",
			"2. Read the work item title, description, fields, metadata, and deliverable",
			"   data.",
			"3. Produce the artifact manifest under the configured output directory.",
			"4. When the artifact is ready and local validation passes, set `render_status`",
			"   to `valid`, update the Workpad block to `status: complete` with",
			"   `blockers: []` and `human_action: null`, leave the work item in",
			"   `Production`, and do not move it to `Review`.",
			"",
			"### For Production",
			"",
			"Continue production from the current filesystem state. When the artifact is",
			"ready and local validation passes, set `render_status` to `valid`, set the",
			"Workpad block to `status: complete`, and leave the work item in `Production`.",
			"",
			"### For Rework",
			"",
			"Move the work item to `Production`, address the requested changes, rerun the",
			"artifact validation gate, set `render_status` to `valid`, set the Workpad block",
			"to `status: complete`, and do not move the work item to `Review`.",
			"",
			"### For Review",
			"",
			"Review is reserved for explicit human opt-out or gate-wait timeout. Re-read the",
			"feedback, update the artifact, then follow the Rework flow.",
		)
	}

	return markdownLines(
		"## Required Execution Flow",
		"",
		"This workflow uses the review-gate handoff: completed artifact work moves to",
		"`Review`, and a human or external renderer marks the artifact `approved` or",
		"`valid` before Detent promotes it to `Ready for Pickup`.",
		"",
		"### For Todo",
		"",
		"1. Move the work item to `Production`.",
		"2. Read the work item title, description, fields, metadata, and deliverable",
		"   data.",
		"3. Produce the artifact manifest under the configured output directory.",
		"4. When the artifact is ready for review, set `render_status` to",
		"   `pending_review`, update the Workpad block to `status: complete` with",
		"   `blockers: []` and `human_action: null`, and move the work item to",
		"   `Review`.",
		"",
		"### For Production",
		"",
		"Continue production from the current filesystem state. When the artifact is",
		"ready for review, set `render_status` to `pending_review`, set the Workpad",
		"block to `status: complete`, and move the work item to `Review`.",
		"",
		"### For Rework",
		"",
		"Move the work item to `Production`, address the requested changes, rerun the",
		"artifact validation gate, set `render_status` to `pending_review`, set the",
		"Workpad block to `status: complete`, and move the work item back to `Review`.",
		"",
		"### For Review",
		"",
		"Do not continue production unless feedback asks for changes. When a human or",
		"external renderer marks `render_status` as `approved` or `valid`, Detent can",
		"promote the item to `Ready for Pickup`.",
	)
}

func markdownLines(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

func (r *onboardingWorkflowDecisionRecorder) set(root *yaml.Node, path string, value any, provenance string, why string) {
	setOnboardingYAMLPath(root, strings.Split(path, "."), value)
	r.add(path, value, provenance, why)
}

func (r *onboardingWorkflowDecisionRecorder) setWithComments(root *yaml.Node, path string, value any, provenance string, why string, headComment string, lineComment string) {
	parts := strings.Split(path, ".")
	setOnboardingYAMLPath(root, parts, value)
	setOnboardingYAMLPathComments(root, parts, headComment, lineComment)
	r.add(path, value, provenance, why)
}

func (r *onboardingWorkflowDecisionRecorder) add(path string, value any, provenance string, why string) {
	r.decisions = append(r.decisions, onboardingWorkflowDecision{
		Path:       path,
		Value:      onboardingWorkflowDecisionValue(value),
		Provenance: provenance,
		Why:        strings.TrimSpace(why),
	})
}

func onboardingWorkflowDecisionValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case []string:
		return strings.Join(typed, ",")
	default:
		return fmt.Sprint(value)
	}
}

func setOnboardingYAMLPath(root *yaml.Node, path []string, value any) {
	if len(path) == 0 {
		return
	}
	current := root
	for _, key := range path[:len(path)-1] {
		next := onboardingYAMLMappingValue(current, key)
		if next == nil || next.Kind != yaml.MappingNode {
			next = &yaml.Node{Kind: yaml.MappingNode}
			setOnboardingYAMLMappingValue(current, key, next)
		}
		current = next
	}
	setOnboardingYAMLMappingValue(current, path[len(path)-1], onboardingYAMLNode(value))
}

func setOnboardingYAMLPathComments(root *yaml.Node, path []string, headComment string, lineComment string) {
	if len(path) == 0 {
		return
	}
	current := root
	for _, key := range path[:len(path)-1] {
		current = onboardingYAMLMappingValue(current, key)
		if current == nil || current.Kind != yaml.MappingNode {
			return
		}
	}
	key := path[len(path)-1]
	for i := 0; i < len(current.Content); i += 2 {
		if current.Content[i].Value == key {
			current.Content[i].HeadComment = strings.TrimSpace(headComment)
			current.Content[i+1].LineComment = strings.TrimSpace(lineComment)
			return
		}
	}
}

func deleteOnboardingYAMLPath(root *yaml.Node, path []string) {
	if len(path) == 0 {
		return
	}
	current := root
	for _, key := range path[:len(path)-1] {
		current = onboardingYAMLMappingValue(current, key)
		if current == nil || current.Kind != yaml.MappingNode {
			return
		}
	}
	deleteOnboardingYAMLMappingValue(current, path[len(path)-1])
}

func onboardingYAMLMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func setOnboardingYAMLMappingValue(node *yaml.Node, key string, value *yaml.Node) {
	if node.Kind != yaml.MappingNode {
		node.Kind = yaml.MappingNode
		node.Content = nil
	}
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = value
			return
		}
	}
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func deleteOnboardingYAMLMappingValue(node *yaml.Node, key string) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
			return
		}
	}
}

func onboardingYAMLNode(value any) *yaml.Node {
	var doc yaml.Node
	if err := doc.Encode(value); err != nil {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fmt.Sprint(value)}
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return &doc
}

func writeOnboardingBuildWorkflowPretty(w io.Writer, result onboardingBuildWorkflowResult) error {
	lines := []string{
		"workflow builder: " + result.Status,
		"preset: " + result.Preset,
		"path: " + result.Path,
		"config_path: " + result.ConfigPath,
		"written: " + strconv.FormatBool(result.Written),
		"source_root: " + result.Probe.SourceRoot,
	}
	if len(result.Probe.Languages) > 0 {
		lines = append(lines, "languages: "+strings.Join(result.Probe.Languages, ","))
	}
	if result.Probe.ValidationCommand != "" {
		lines = append(lines, "validation_command: "+result.Probe.ValidationCommand)
	}
	lines = append(lines, "", "decisions and why:")
	for _, decision := range result.Decisions {
		lines = append(lines, fmt.Sprintf("- %s=%s (%s) %s", decision.Path, decision.Value, decision.Provenance, decision.Why))
	}
	if !result.Written {
		lines = append(lines,
			"",
			"detent.yaml preview:",
			strings.TrimSpace(result.Config),
			"",
			"WORKFLOW.md preview:",
			strings.TrimSpace(result.Workflow),
		)
	}
	_, err := fmt.Fprintln(w, strings.Join(lines, "\n"))
	return err
}
