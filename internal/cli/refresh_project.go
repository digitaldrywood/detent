package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

const refreshManagedDefaultPrefix = "detent-refresh: managed-default sha256="

type projectRefreshSetting struct {
	Path           string `json:"path"`
	Existing       string `json:"existing,omitempty"`
	CurrentDefault string `json:"current_default"`
	Effect         string `json:"effect"`
}

type projectRefreshFeature struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Effect string `json:"effect"`
	Status string `json:"status"`
}

type projectRefreshFile struct {
	Path   string `json:"path"`
	Action string `json:"action"`
}

type projectRefreshResult struct {
	Status            string                  `json:"status"`
	Project           string                  `json:"project"`
	Preset            string                  `json:"preset"`
	WorkflowPath      string                  `json:"workflow_path"`
	ConfigPath        string                  `json:"config_path"`
	AgentsPath        string                  `json:"agents_path"`
	Files             []projectRefreshFile    `json:"files"`
	PreservedSettings []projectRefreshSetting `json:"preserved_settings"`
	DefaultUpdates    []projectRefreshSetting `json:"default_updates"`
	OptInFeatures     []projectRefreshFeature `json:"opt_in_features"`
	Diff              string                  `json:"diff"`
	Noop              bool                    `json:"noop"`
	Applied           bool                    `json:"applied"`
}

type projectRefreshConfig struct {
	ConfigPath string
	ProjectID  string
	Options    options
}

type projectRefreshPlan struct {
	Result  projectRefreshResult
	changes []projectRefreshChange
}

type projectRefreshChange struct {
	path    string
	before  []byte
	after   []byte
	mode    fs.FileMode
	existed bool
}

type projectRefreshStagedChange struct {
	target  string
	temp    string
	backup  string
	existed bool
	applied bool
}

type projectRefreshFeatureDescriptor struct {
	name   string
	path   string
	effect string
}

var projectRefreshFeatureDescriptors = []projectRefreshFeatureDescriptor{
	{
		name:   "out-of-scope follow-ups",
		path:   "agent.followups.enabled",
		effect: "allows agents to preserve meaningful out-of-scope discoveries as governed tracker items",
	},
	{
		name:   "backlog admission",
		path:   "backlog_admission.enabled",
		effect: "evaluates backlog work against project-owned criteria and proposes eligible items for dispatch",
	},
	{
		name:   "scheduled routines",
		path:   "routines",
		effect: "runs bounded recurring agent prompts such as repository maintenance reviews",
	},
	{
		name:   "scheduled source intake",
		path:   "intake.sources",
		effect: "turns configured repository scans into deduplicated backlog items",
	},
	{
		name:   "validator agent",
		path:   "gate.validator.enabled",
		effect: "adds an automated validation opinion before promotion",
	},
	{
		name:   "skill draft creation",
		path:   "agent.skills.creation.enabled",
		effect: "allows completed runs to propose one reusable project skill for review",
	},
}

func newRefreshProjectCommand(configPath *string, opts options) *cobra.Command {
	var confirmed bool
	cmd := &cobra.Command{
		Use:          "refresh-project PROJECT_ID",
		Short:        "Propose current onboarding updates for a registered project",
		Long:         "Regenerate a registered project's Detent files against the current onboarding feature set, preserving configured values and YAML comments. The default is a read-only diff; --yes applies the reviewed proposal.",
		Example:      "detent refresh-project api\n  detent refresh-project api --yes",
		Args:         ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			plan, err := planProjectRefresh(cmd.Context(), projectRefreshConfig{
				ConfigPath: derefString(configPath),
				ProjectID:  args[0],
				Options:    opts,
			})
			if err != nil {
				return err
			}
			if confirmed && !plan.Result.Noop {
				if err := applyProjectRefresh(plan); err != nil {
					return err
				}
				plan.Result.Applied = true
			}
			return out.Write(func(writer io.Writer) error {
				return writeProjectRefreshPretty(writer, plan.Result)
			}, plan.Result)
		},
	}
	cmd.Flags().BoolVar(&confirmed, "yes", false, "apply the displayed refresh proposal")
	return cmd
}

func planProjectRefresh(ctx context.Context, cfg projectRefreshConfig) (projectRefreshPlan, error) {
	id := strings.TrimSpace(cfg.ProjectID)
	if id == "" {
		return projectRefreshPlan{}, WrapValidation(errors.New("PROJECT_ID is required"))
	}
	resolution, err := resolveConfigPathResolution(cfg.ConfigPath, cfg.Options)
	if err != nil {
		return projectRefreshPlan{}, err
	}
	readProject := cfg.Options.readProject
	if readProject == nil {
		readProject = func(path string, projectID string) (globalconfig.Config, []string, error) {
			return globalconfig.ReadProject(path, projectID)
		}
	}
	global, skipped, err := readProject(resolution.Path, id)
	if err != nil {
		return projectRefreshPlan{}, err
	}
	if len(global.Projects) != 1 {
		available := make([]globalconfig.Project, 0, len(skipped))
		for _, projectID := range skipped {
			available = append(available, globalconfig.Project{ID: projectID})
		}
		return projectRefreshPlan{}, projectNotFoundError(id, available)
	}
	registered := global.Projects[0]
	if strings.TrimSpace(registered.WorkflowRef) != "" {
		return projectRefreshPlan{}, NewValidationError(
			"refresh-project cannot update a workflow_ref-backed project",
			"Refresh the workflow definition in its source ref, update projects[].workflow_ref, then rerun refresh-project.",
			nil,
		)
	}
	workflowPath, err := projectRefreshWorkflowPath(registered)
	if err != nil {
		return projectRefreshPlan{}, err
	}
	configPath := workflowconfig.DefinitionPath(workflowPath)
	agentsPath := filepath.Join(filepath.Dir(workflowPath), "AGENTS.md")

	workflowRaw, workflowMode, err := readRequiredProjectRefreshFile(workflowPath)
	if err != nil {
		return projectRefreshPlan{}, err
	}
	if refreshWorkflowHasFrontmatter(workflowRaw) {
		return projectRefreshPlan{}, NewValidationError(
			"refresh-project requires the split WORKFLOW.md and detent.yaml layout",
			"Run detent fix workflow-layout --workflow "+workflowPath+" --yes, review that migration, then rerun refresh-project.",
			nil,
		)
	}
	configRaw, configMode, err := readRequiredProjectRefreshFile(configPath)
	if err != nil {
		return projectRefreshPlan{}, err
	}
	agentsRaw, agentsMode, agentsExists, err := readOptionalProjectRefreshFile(agentsPath)
	if err != nil {
		return projectRefreshPlan{}, err
	}

	existingRoot, err := parseProjectRefreshYAML(configRaw, configPath)
	if err != nil {
		return projectRefreshPlan{}, err
	}
	existingConfig, err := parseProjectRefreshConfig(configRaw, workflowRaw)
	if err != nil {
		return projectRefreshPlan{}, fmt.Errorf("parse current project config: %w", err)
	}
	presetName, err := projectRefreshPreset(existingConfig)
	if err != nil {
		return projectRefreshPlan{}, err
	}
	sourceRoot := projectRefreshSourceRoot(registered, workflowPath)
	probe, err := probeOnboardingRepository(sourceRoot)
	if err != nil {
		return projectRefreshPlan{}, err
	}
	answers := projectRefreshAnswers(id, presetName, existingConfig, existingRoot, workflowRaw)
	preset, err := selectOnboardingWorkflowPreset(presetName, answers)
	if err != nil {
		return projectRefreshPlan{}, err
	}
	validation := onboardingAnswersValidationResult{
		DetentProjectID:  id,
		TargetRepository: projectRefreshRepository(existingConfig, id),
		TargetSourceRoot: sourceRoot,
	}
	desiredConfig, generatedWorkflow, decisions, err := renderOnboardingWorkflow(ctx, preset, answers, validation, probe)
	if err != nil {
		return projectRefreshPlan{}, err
	}
	desiredRoot, err := parseProjectRefreshYAML([]byte(desiredConfig), "current onboarding template")
	if err != nil {
		return projectRefreshPlan{}, err
	}

	decisionByPath := make(map[string]onboardingWorkflowDecision, len(decisions))
	for _, decision := range decisions {
		decisionByPath[decision.Path] = decision
	}
	result := projectRefreshResult{
		Status:            "ok",
		Project:           id,
		Preset:            presetName,
		WorkflowPath:      workflowPath,
		ConfigPath:        configPath,
		AgentsPath:        agentsPath,
		Files:             []projectRefreshFile{},
		PreservedSettings: []projectRefreshSetting{},
		DefaultUpdates:    []projectRefreshSetting{},
	}
	result.OptInFeatures = projectRefreshFeatures(existingRoot, desiredRoot)
	configChanged := mergeProjectRefreshYAML(existingRoot, desiredRoot, nil, decisionByPath, &result)
	refreshedConfig := configRaw
	if configChanged {
		refreshedConfig, err = marshalProjectRefreshYAML(existingRoot, configRaw)
		if err != nil {
			return projectRefreshPlan{}, err
		}
	}
	refreshedWorkflow := refreshProjectWorkflow(string(workflowRaw), generatedWorkflow, existingConfig)
	refreshedAgents, err := renderProjectRefreshAgentGuidance(string(agentsRaw), answers)
	if err != nil {
		return projectRefreshPlan{}, err
	}
	if err := validateProjectRefreshCandidate(workflowPath, []byte(refreshedWorkflow), configPath, refreshedConfig, agentsPath, []byte(refreshedAgents)); err != nil {
		return projectRefreshPlan{}, err
	}

	sort.Slice(result.PreservedSettings, func(i int, j int) bool {
		return result.PreservedSettings[i].Path < result.PreservedSettings[j].Path
	})
	sort.Slice(result.DefaultUpdates, func(i int, j int) bool {
		return result.DefaultUpdates[i].Path < result.DefaultUpdates[j].Path
	})

	changes := make([]projectRefreshChange, 0, 3)
	changes = appendProjectRefreshChange(changes, workflowPath, workflowRaw, []byte(refreshedWorkflow), workflowMode, true)
	changes = appendProjectRefreshChange(changes, configPath, configRaw, refreshedConfig, configMode, true)
	changes = appendProjectRefreshChange(changes, agentsPath, agentsRaw, []byte(refreshedAgents), agentsMode, agentsExists)
	sort.Slice(changes, func(i int, j int) bool { return changes[i].path < changes[j].path })
	for _, change := range changes {
		action := "update"
		if !change.existed {
			action = "create"
		}
		result.Files = append(result.Files, projectRefreshFile{Path: change.path, Action: action})
	}
	result.Diff = projectRefreshDiff(changes)
	result.Noop = len(changes) == 0
	return projectRefreshPlan{Result: result, changes: changes}, nil
}

func projectRefreshWorkflowPath(project globalconfig.Project) (string, error) {
	workflowPath := strings.TrimSpace(project.Workflow)
	if workflowPath == "" {
		return "", errors.New("registered project workflow path is empty")
	}
	if filepath.IsAbs(workflowPath) {
		return filepath.Clean(workflowPath), nil
	}
	workdir := strings.TrimSpace(project.Workdir)
	if workdir == "" {
		return "", errors.New("registered project workdir is empty")
	}
	return filepath.Clean(filepath.Join(workdir, workflowPath)), nil
}

func projectRefreshSourceRoot(project globalconfig.Project, workflowPath string) string {
	workdir := strings.TrimSpace(project.Workdir)
	if workdir != "" && filepath.IsAbs(workdir) {
		if info, err := os.Stat(workdir); err == nil && info.IsDir() {
			return filepath.Clean(workdir)
		}
	}
	return filepath.Dir(workflowPath)
}

func projectRefreshPreset(cfg workflowconfig.Config) (string, error) {
	switch cfg.Tracker.Kind {
	case workflowconfig.TrackerGitHub:
		switch cfg.Tracker.GitHubStatusSource {
		case workflowconfig.GitHubStatusSourceProjectV2:
			return "project_v2", nil
		case workflowconfig.GitHubStatusSourceIssueField:
			return "issue_field", nil
		case workflowconfig.GitHubStatusSourceLabel:
			return "label", nil
		default:
			return "", NewValidationError(
				"registered GitHub project has no supported github_status_source",
				"Configure project_v2, issue_field, or label before refreshing.",
				nil,
			)
		}
	case workflowconfig.TrackerGitHubLocal:
		return "github_local", nil
	case workflowconfig.TrackerLocalSQLite:
		if cfg.Workspace.Kind == workflowconfig.WorkspaceFilesystem || cfg.Deliverable.Kind == workflowconfig.DeliverableArtifact {
			return "non_code_artifact", nil
		}
	}
	return "", NewValidationError(
		"registered project does not map to a current onboarding preset",
		"Refresh supports project_v2, issue_field, label, github_local, and non_code_artifact projects.",
		nil,
	)
}

func projectRefreshRepository(cfg workflowconfig.Config, projectID string) string {
	if repository := strings.TrimSpace(cfg.Tracker.Repository); repository != "" {
		return repository
	}
	return "local/" + strings.TrimSpace(projectID)
}

func projectRefreshAnswers(
	projectID string,
	preset string,
	cfg workflowconfig.Config,
	root *yaml.Node,
	workflow []byte,
) onboardingAnswers {
	values := map[string]string{
		"DETENT_PROJECT_ID":                      projectID,
		"WORKFLOW_PRESET":                        preset,
		"DELIVERY_PROFILE":                       "review_gate",
		"INTAKE_PROFILE":                         "manual_intake",
		"AUTO_PROMOTE_ENABLED":                   strconv.FormatBool(cfg.Agent.AutoPromote.Enabled),
		"AUTO_PROMOTE_QUIET_SECONDS":             strconv.Itoa(cfg.Agent.AutoPromote.QuietSeconds),
		"AUTO_PROMOTE_GATE_WAIT_STATE":           cfg.Agent.AutoPromote.GateWaitState,
		"AUTO_PROMOTE_GATE_WAIT_TIMEOUT_SECONDS": strconv.Itoa(cfg.Agent.AutoPromote.GateWaitTimeoutSeconds),
		"WORKER_MODEL_MODE":                      onboardingWorkflowWorkerModelProviderDefault,
		"FOLLOWUPS_ENABLED":                      "false",
	}
	if values["AUTO_PROMOTE_GATE_WAIT_STATE"] == "" {
		values["AUTO_PROMOTE_GATE_WAIT_STATE"] = workflowconfig.AutoPromoteGateWaitStateReview
	}
	if cfg.Agent.AutoPromote.GateWaitTimeoutSeconds <= 0 {
		values["AUTO_PROMOTE_GATE_WAIT_TIMEOUT_SECONDS"] = strconv.Itoa(workflowconfig.DefaultAutoPromoteGateWaitTimeoutSeconds)
	}
	if projectRefreshYAMLPathExists(root, "agent.followups.enabled") {
		values["FOLLOWUPS_ENABLED"] = strconv.FormatBool(cfg.Agent.Followups.Enabled)
	}
	if projectRefreshYAMLPathExists(root, "backlog_admission") && cfg.BacklogAdmission.Enabled {
		values["BACKLOG_ADMISSION_ENABLED"] = "true"
		values["BACKLOG_ADMISSION_SCHEDULE"] = cfg.BacklogAdmission.Schedule
		values["BACKLOG_ADMISSION_TARGET_STATE"] = cfg.BacklogAdmission.TargetState
		values["BACKLOG_ADMISSION_CRITERIA_SECTION"] = cfg.BacklogAdmission.CriteriaSection
		values["BACKLOG_ADMISSION_MAX_CANDIDATES_PER_RUN"] = strconv.Itoa(cfg.BacklogAdmission.MaxCandidatesPerRun)
		values["BACKLOG_ADMISSION_MAX_PROPOSALS_PER_RUN"] = strconv.Itoa(cfg.BacklogAdmission.MaxProposalsPerRun)
		values["BACKLOG_ADMISSION_MAX_OPEN_PROPOSALS"] = strconv.Itoa(cfg.BacklogAdmission.MaxOpenProposals)
		values["BACKLOG_ADMISSION_PROPOSAL_EXPIRY_DAYS"] = strconv.Itoa(cfg.BacklogAdmission.ProposalExpiryDays)
		values["BACKLOG_ADMISSION_AUTO_ADMIT"] = strconv.FormatBool(cfg.BacklogAdmission.AutoAdmit)
		values["BACKLOG_ADMISSION_AUTO_ADMIT_MIN_CONFIDENCE"] = strconv.FormatFloat(cfg.BacklogAdmission.AutoAdmitMinConfidence, 'f', -1, 64)
		if len(cfg.BacklogAdmission.Sources.States) > 0 {
			values["BACKLOG_ADMISSION_SOURCE_STATE"] = cfg.BacklogAdmission.Sources.States[0]
		}
		if len(cfg.BacklogAdmission.Authors.AllowAssociation) > 0 {
			values["BACKLOG_ADMISSION_AUTHORS_ALLOW_ASSOCIATION"] = strings.Join(cfg.BacklogAdmission.Authors.AllowAssociation, ",")
		}
	} else {
		values["BACKLOG_ADMISSION_ENABLED"] = "false"
	}
	values["ROUTINES_ENABLED"] = strconv.FormatBool(projectRefreshYAMLPathHasItems(root, "routines"))
	values["STALE_TODOS_ENABLED"] = strconv.FormatBool(projectRefreshYAMLPathHasItems(root, "intake.sources"))
	if cfg.Tracker.ProjectSlug != "" {
		values["PROJECT_SLUG"] = cfg.Tracker.ProjectSlug
	}
	if cfg.Tracker.StatusField != "" {
		values["STATUS_FIELD_NAME"] = cfg.Tracker.StatusField
	}
	if cfg.Tracker.StatusLabelPrefix != "" {
		values["STATUS_LABEL_PREFIX"] = cfg.Tracker.StatusLabelPrefix
	}
	if cfg.Tracker.WriteProbeIssue != "" {
		values["WRITE_PROBE_ISSUE"] = cfg.Tracker.WriteProbeIssue
	}
	projectRefreshGuidanceAnswers(values, string(workflow))
	return onboardingAnswers{Values: values}
}

func projectRefreshGuidanceAnswers(values map[string]string, workflow string) {
	criteriaFallbacks := map[string]string{
		"Alignment":    "Admit work only when it advances a documented project goal or repairs an operator-visible defect.",
		"Readiness":    "Admit work only when its expected behavior, dependencies, and validation are checkable.",
		"Size":         "Admit work only when implementation and validation fit within one agent run.",
		"Safety Gates": "Do not admit work that needs unavailable credentials, destructive authority, or unresolved external dependencies.",
	}
	for _, field := range onboardingAdmissionGuidanceFields() {
		value := projectRefreshMarkdownSubsection(workflow, "### "+field.Heading)
		if value == "" {
			value = criteriaFallbacks[field.Heading]
		}
		values[field.Key] = value
	}
	effortFallbacks := map[string]string{
		"medium": "Small, mechanical, tightly specified work with complete acceptance criteria.",
		"high":   "A standard feature or fix with some ambiguity or cross-cutting impact.",
		"xhigh":  "A new subsystem or tricky state, concurrency, restart, recovery, or interaction work.",
		"max":    "Exceptional operator-designated work; never select this effort automatically.",
	}
	for _, field := range onboardingEffortGuidanceFields() {
		values[field.Key] = effortFallbacks[field.Heading]
	}
}

func renderProjectRefreshAgentGuidance(existing string, answers onboardingAnswers) (string, error) {
	if hasProjectRefreshEffortGuidance(existing) {
		return existing, nil
	}
	return renderOnboardingAgentGuidance(existing, answers)
}

func hasProjectRefreshEffortGuidance(text string) bool {
	heading := "## " + onboardingEffortRubricHeading
	remaining := text
	for {
		index := strings.Index(remaining, heading)
		if index < 0 {
			return false
		}
		candidate := remaining[index:]
		if hasOnboardingEffortGuidance(candidate) {
			return true
		}
		remaining = candidate[len(heading):]
	}
}

func projectRefreshMarkdownSubsection(markdown string, heading string) string {
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	start := -1
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if start < 0 {
			if trimmed == heading {
				start = index + 1
			}
			continue
		}
		if strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "### ") {
			return strings.TrimSpace(strings.Join(lines[start:index], "\n"))
		}
	}
	if start < 0 {
		return ""
	}
	return strings.TrimSpace(strings.Join(lines[start:], "\n"))
}

func parseProjectRefreshConfig(configRaw []byte, workflowRaw []byte) (workflowconfig.Config, error) {
	combined := make([]byte, 0, len(configRaw)+len(workflowRaw)+10)
	combined = append(combined, "---\n"...)
	combined = append(combined, configRaw...)
	if len(combined) > 0 && combined[len(combined)-1] != '\n' {
		combined = append(combined, '\n')
	}
	combined = append(combined, "---\n"...)
	combined = append(combined, workflowRaw...)
	workflow, err := workflowconfig.ParseWorkflow(combined)
	if err != nil {
		return workflowconfig.Config{}, err
	}
	return workflow.Config, nil
}

func parseProjectRefreshYAML(raw []byte, path string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse %s: project config must be a YAML mapping", path)
	}
	return doc.Content[0], nil
}

func marshalProjectRefreshYAML(root *yaml.Node, original []byte) ([]byte, error) {
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(projectRefreshYAMLIndent(original))
	if err := encoder.Encode(root); err != nil {
		return nil, fmt.Errorf("marshal refreshed project config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("close refreshed project config encoder: %w", err)
	}
	return output.Bytes(), nil
}

func projectRefreshYAMLIndent(raw []byte) int {
	indent := 0
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		spaces := len(line) - len(strings.TrimLeft(line, " "))
		if spaces > 0 && (indent == 0 || spaces < indent) {
			indent = spaces
		}
	}
	if indent < 2 {
		return 2
	}
	return indent
}

func mergeProjectRefreshYAML(
	existing *yaml.Node,
	desired *yaml.Node,
	path []string,
	decisions map[string]onboardingWorkflowDecision,
	result *projectRefreshResult,
) bool {
	if existing == nil || desired == nil || existing.Kind != yaml.MappingNode || desired.Kind != yaml.MappingNode {
		return false
	}
	changed := false
	for index := 0; index+1 < len(desired.Content); index += 2 {
		desiredKey := desired.Content[index]
		desiredValue := desired.Content[index+1]
		currentPath := append(append([]string(nil), path...), desiredKey.Value)
		pathText := strings.Join(currentPath, ".")
		existingIndex := projectRefreshYAMLKeyIndex(existing, desiredKey.Value)
		if existingIndex < 0 {
			key := cloneProjectRefreshYAMLNode(desiredKey)
			if desiredValue.Kind == yaml.MappingNode && len(desiredValue.Content) > 0 {
				value := cloneProjectRefreshYAMLNode(desiredValue)
				value.Content = nil
				existing.Content = append(existing.Content, key, value)
				mergeProjectRefreshYAML(value, desiredValue, currentPath, decisions, result)
				changed = true
				continue
			}
			value := cloneProjectRefreshYAMLNode(desiredValue)
			projectRefreshSetManagedDefault(key, value)
			existing.Content = append(existing.Content, key, value)
			result.DefaultUpdates = append(result.DefaultUpdates, newProjectRefreshSetting(pathText, "", value, decisions))
			changed = true
			continue
		}

		existingKey := existing.Content[existingIndex]
		existingValue := existing.Content[existingIndex+1]
		if existingValue.Kind == yaml.MappingNode && desiredValue.Kind == yaml.MappingNode {
			if mergeProjectRefreshYAML(existingValue, desiredValue, currentPath, decisions, result) {
				changed = true
			}
			continue
		}
		if bytes.Equal(projectRefreshYAMLBytes(existingValue), projectRefreshYAMLBytes(desiredValue)) {
			continue
		}
		managedHash, managed := projectRefreshManagedDefaultHash(existingKey)
		if managed && managedHash == projectRefreshYAMLHash(existingValue) {
			replacement := cloneProjectRefreshYAMLNode(desiredValue)
			preserveProjectRefreshYAMLComments(existingValue, replacement)
			existing.Content[existingIndex+1] = replacement
			projectRefreshSetManagedDefault(existingKey, existing.Content[existingIndex+1])
			result.DefaultUpdates = append(result.DefaultUpdates, newProjectRefreshSetting(pathText, projectRefreshYAMLValue(existingValue), desiredValue, decisions))
			changed = true
			continue
		}
		if managed {
			comment := projectRefreshStripManagedDefault(existingKey.HeadComment)
			if comment != existingKey.HeadComment {
				existingKey.HeadComment = comment
				changed = true
			}
		}
		result.PreservedSettings = append(result.PreservedSettings, newProjectRefreshSetting(pathText, projectRefreshYAMLValue(existingValue), desiredValue, decisions))
	}
	return changed
}

func newProjectRefreshSetting(
	path string,
	existing string,
	desired *yaml.Node,
	decisions map[string]onboardingWorkflowDecision,
) projectRefreshSetting {
	effect := "current onboarding template default"
	if decision, ok := decisions[path]; ok && strings.TrimSpace(decision.Why) != "" {
		effect = strings.TrimSpace(decision.Why)
	}
	return projectRefreshSetting{
		Path:           path,
		Existing:       existing,
		CurrentDefault: projectRefreshYAMLValue(desired),
		Effect:         effect,
	}
}

func projectRefreshFeatures(existing *yaml.Node, desired *yaml.Node) []projectRefreshFeature {
	features := make([]projectRefreshFeature, 0, len(projectRefreshFeatureDescriptors))
	for _, descriptor := range projectRefreshFeatureDescriptors {
		if projectRefreshYAMLPathExists(existing, descriptor.path) {
			if enabled, ok := projectRefreshYAMLPathBool(existing, descriptor.path); ok && !enabled {
				features = append(features, projectRefreshFeature{
					Name:   descriptor.name,
					Path:   descriptor.path,
					Effect: descriptor.effect,
					Status: "explicitly disabled; preserved",
				})
			}
			continue
		}
		status := "available; not included by the preserved intake profile"
		if projectRefreshYAMLPathExists(desired, descriptor.path) {
			status = "proposed; applied only with --yes"
		}
		features = append(features, projectRefreshFeature{
			Name:   descriptor.name,
			Path:   descriptor.path,
			Effect: descriptor.effect,
			Status: status,
		})
	}
	return features
}

func refreshProjectWorkflow(existing string, generated string, cfg workflowconfig.Config) string {
	refreshed := existing
	if projectRefreshYAMLTextSectionRequired(cfg.BacklogAdmission.Enabled, cfg.BacklogAdmission.CriteriaSection) {
		refreshed = appendProjectRefreshMarkdownSection(refreshed, generated, cfg.BacklogAdmission.CriteriaSection)
	}
	if cfg.BacklogAdmission.RequireEffort &&
		cfg.BacklogAdmission.EffortFile != workflowconfig.BacklogAdmissionEffortFileAgents &&
		strings.TrimSpace(cfg.BacklogAdmission.EffortSection) != "" {
		section := strings.TrimSpace(cfg.BacklogAdmission.EffortSection)
		if _, found := onboardingMarkdownSection(refreshed, "## "+section); !found {
			refreshed = strings.TrimRight(refreshed, "\n") + "\n\n" + projectRefreshEffortSection(section)
		}
	}
	if !strings.HasSuffix(refreshed, "\n") {
		refreshed += "\n"
	}
	return refreshed
}

func projectRefreshYAMLTextSectionRequired(configured bool, section string) bool {
	return configured && strings.TrimSpace(section) != ""
}

func appendProjectRefreshMarkdownSection(existing string, generated string, section string) string {
	section = strings.TrimSpace(section)
	if section == "" {
		return existing
	}
	generatedSection, found := projectRefreshMarkdownSection(generated, "## "+section)
	if !found {
		return existing
	}
	if existingSection, found := onboardingMarkdownSection(existing, "## "+section); found {
		if strings.TrimSpace(existingSection) != "" {
			return existing
		}
		return replaceProjectRefreshMarkdownSection(existing, "## "+section, generatedSection)
	}
	return strings.TrimRight(existing, "\n") + "\n\n" + generatedSection
}

func replaceProjectRefreshMarkdownSection(existing string, heading string, replacement string) string {
	lines := strings.Split(strings.ReplaceAll(existing, "\r\n", "\n"), "\n")
	start := -1
	end := len(lines)
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if start < 0 {
			if trimmed == heading {
				start = index
			}
			continue
		}
		if strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "## ") {
			end = index
			break
		}
	}
	if start < 0 {
		return existing
	}
	parts := []string{strings.TrimRight(strings.Join(lines[:start], "\n"), "\n"), strings.TrimRight(replacement, "\n"), strings.TrimLeft(strings.Join(lines[end:], "\n"), "\n")}
	kept := parts[:0]
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "\n\n") + "\n"
}

func projectRefreshMarkdownSection(markdown string, heading string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	start := -1
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if start < 0 {
			if trimmed == heading {
				start = index
			}
			continue
		}
		if index > start && (strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "## ")) {
			return strings.TrimRight(strings.Join(lines[start:index], "\n"), "\n") + "\n", true
		}
	}
	if start < 0 {
		return "", false
	}
	return strings.TrimRight(strings.Join(lines[start:], "\n"), "\n") + "\n", true
}

func projectRefreshEffortSection(section string) string {
	return markdownLines(
		"## "+section,
		"",
		"- `medium` — Small, mechanical, tightly specified work with complete acceptance criteria.",
		"- `high` — A standard feature or fix with some ambiguity or cross-cutting impact.",
		"- `xhigh` — A new subsystem or tricky state, concurrency, restart, recovery, or interaction work.",
		"- `max` — Exceptional operator-designated work; never select this effort automatically.",
	)
}

func validateProjectRefreshCandidate(workflowPath string, workflow []byte, configPath string, config []byte, agentsPath string, agents []byte) error {
	sources := workflowconfig.ProjectDefinitionSources{
		WorkflowPath: workflowPath,
		Workflow:     workflow,
		ConfigPath:   configPath,
		Config:       config,
		HasConfig:    true,
		AgentsPath:   agentsPath,
		Agents:       agents,
		HasAgents:    true,
	}
	localWorkflowPath := workflowconfig.LocalWorkflowPath(workflowPath)
	localWorkflow, _, localWorkflowExists, err := readOptionalProjectRefreshFile(localWorkflowPath)
	if err != nil {
		return err
	}
	sources.LocalWorkflowPath = localWorkflowPath
	sources.LocalWorkflow = localWorkflow
	sources.HasLocalWorkflow = localWorkflowExists
	localConfigPath := workflowconfig.LocalDefinitionPath(workflowPath)
	localConfig, _, localConfigExists, err := readOptionalProjectRefreshFile(localConfigPath)
	if err != nil {
		return err
	}
	sources.LocalConfigPath = localConfigPath
	sources.LocalConfig = localConfig
	sources.HasLocalConfig = localConfigExists
	if _, err := workflowconfig.ParseProjectDefinition(sources); err != nil {
		return fmt.Errorf("validate refresh proposal: %w", err)
	}
	return nil
}

func appendProjectRefreshChange(
	changes []projectRefreshChange,
	path string,
	before []byte,
	after []byte,
	mode fs.FileMode,
	existed bool,
) []projectRefreshChange {
	if bytes.Equal(before, after) {
		return changes
	}
	if mode == 0 {
		mode = 0o600
	}
	return append(changes, projectRefreshChange{
		path:    path,
		before:  append([]byte(nil), before...),
		after:   append([]byte(nil), after...),
		mode:    mode.Perm(),
		existed: existed,
	})
}

func applyProjectRefresh(plan projectRefreshPlan) error {
	return applyProjectRefreshWithRename(plan, os.Rename)
}

func applyProjectRefreshWithRename(plan projectRefreshPlan, rename func(string, string) error) error {
	for _, change := range plan.changes {
		current, _, exists, err := readOptionalProjectRefreshFile(change.path)
		if err != nil {
			return err
		}
		if exists != change.existed || !bytes.Equal(current, change.before) {
			return fmt.Errorf("%s changed since the refresh proposal was prepared; rerun detent refresh-project", change.path)
		}
	}

	staged := make([]projectRefreshStagedChange, 0, len(plan.changes))
	cleanup := func() error {
		var cleanupErrors []error
		for _, change := range staged {
			if change.temp == "" {
				continue
			}
			if err := os.Remove(change.temp); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("remove staged refresh file %s: %w", change.temp, err))
			}
		}
		return errors.Join(cleanupErrors...)
	}
	for _, change := range plan.changes {
		file, err := os.CreateTemp(filepath.Dir(change.path), ".detent-refresh-*")
		if err != nil {
			return errors.Join(fmt.Errorf("stage refreshed file %s: %w", change.path, err), cleanup())
		}
		tempPath := file.Name()
		if err := file.Chmod(change.mode); err != nil {
			return errors.Join(
				fmt.Errorf("set refreshed file mode %s: %w", change.path, err),
				discardProjectRefreshTemp(file),
				cleanup(),
			)
		}
		if _, err := file.Write(change.after); err != nil {
			return errors.Join(
				fmt.Errorf("stage refreshed file %s: %w", change.path, err),
				discardProjectRefreshTemp(file),
				cleanup(),
			)
		}
		if err := file.Close(); err != nil {
			removeErr := os.Remove(tempPath)
			if errors.Is(removeErr, os.ErrNotExist) {
				removeErr = nil
			}
			return errors.Join(fmt.Errorf("close staged refresh file %s: %w", change.path, err), removeErr, cleanup())
		}
		staged = append(staged, projectRefreshStagedChange{target: change.path, temp: tempPath, existed: change.existed})
	}
	for index, change := range staged {
		if change.existed {
			backup, err := reserveProjectRefreshBackupPath(change.target)
			if err != nil {
				return errors.Join(err, rollbackProjectRefresh(staged[:index], rename), cleanup())
			}
			if err := rename(change.target, backup); err != nil {
				return errors.Join(
					fmt.Errorf("preserve current file %s: %w", change.target, err),
					rollbackProjectRefresh(staged[:index], rename),
					cleanup(),
				)
			}
			staged[index].backup = backup
		}
		if err := rename(change.temp, change.target); err != nil {
			var restoreErr error
			if change.existed {
				restoreErr = rename(staged[index].backup, change.target)
				if restoreErr == nil {
					staged[index].backup = ""
				} else {
					restoreErr = fmt.Errorf("restore current file %s from %s: %w", change.target, staged[index].backup, restoreErr)
				}
			}
			return errors.Join(
				fmt.Errorf("apply refreshed file %s: %w", change.target, err),
				restoreErr,
				rollbackProjectRefresh(staged[:index], rename),
				cleanup(),
			)
		}
		staged[index].temp = ""
		staged[index].applied = true
	}
	var cleanupErrors []error
	for index := range staged {
		if staged[index].backup == "" {
			continue
		}
		if err := os.Remove(staged[index].backup); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove refresh backup %s: %w", staged[index].backup, err))
		}
	}
	return errors.Join(cleanupErrors...)
}

func reserveProjectRefreshBackupPath(target string) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(target), ".detent-refresh-backup-*")
	if err != nil {
		return "", fmt.Errorf("reserve refresh backup for %s: %w", target, err)
	}
	path := file.Name()
	if err := discardProjectRefreshTemp(file); err != nil {
		return "", fmt.Errorf("reserve refresh backup for %s: %w", target, err)
	}
	return path, nil
}

func rollbackProjectRefresh(staged []projectRefreshStagedChange, rename func(string, string) error) error {
	var rollbackErrors []error
	for index := len(staged) - 1; index >= 0; index-- {
		change := &staged[index]
		if !change.applied {
			continue
		}
		if err := os.Remove(change.target); err != nil && !errors.Is(err, os.ErrNotExist) {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("remove refreshed file %s during rollback: %w", change.target, err))
			continue
		}
		if !change.existed {
			change.applied = false
			continue
		}
		if err := rename(change.backup, change.target); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restore original file %s from %s: %w", change.target, change.backup, err))
			continue
		}
		change.backup = ""
		change.applied = false
	}
	return errors.Join(rollbackErrors...)
}

func discardProjectRefreshTemp(file *os.File) error {
	var cleanupErrors []error
	path := filepath.Clean(file.Name())
	if err := file.Close(); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		cleanupErrors = append(cleanupErrors, err)
	}
	return errors.Join(cleanupErrors...)
}

func writeProjectRefreshPretty(writer io.Writer, result projectRefreshResult) error {
	if _, err := fmt.Fprintf(writer, "Project refresh: %s (%s)\n", result.Project, result.Preset); err != nil {
		return err
	}
	if len(result.PreservedSettings) > 0 {
		if _, err := fmt.Fprintln(writer, "Preserved operator settings:"); err != nil {
			return err
		}
		for _, setting := range result.PreservedSettings {
			if _, err := fmt.Fprintf(writer, "- %s=%s (current default: %s; %s)\n", setting.Path, setting.Existing, setting.CurrentDefault, setting.Effect); err != nil {
				return err
			}
		}
	}
	if len(result.DefaultUpdates) > 0 {
		if _, err := fmt.Fprintln(writer, "Generated default additions and updates:"); err != nil {
			return err
		}
		for _, setting := range result.DefaultUpdates {
			if _, err := fmt.Fprintf(writer, "- %s=%s (%s)\n", setting.Path, setting.CurrentDefault, setting.Effect); err != nil {
				return err
			}
		}
	}
	if len(result.OptInFeatures) > 0 {
		if _, err := fmt.Fprintln(writer, "Opt-in feature additions:"); err != nil {
			return err
		}
		for _, feature := range result.OptInFeatures {
			if _, err := fmt.Fprintf(writer, "- %s (%s): %s; %s\n", feature.Name, feature.Path, feature.Effect, feature.Status); err != nil {
				return err
			}
		}
	}
	if result.Noop {
		_, err := fmt.Fprintln(writer, "No changes needed; the project already matches this refresh proposal.")
		return err
	}
	if _, err := fmt.Fprintln(writer, "Diff:"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(writer, result.Diff); err != nil {
		return err
	}
	if result.Applied {
		_, err := fmt.Fprintln(writer, "Refresh applied.")
		return err
	}
	_, err := fmt.Fprintf(writer, "Preview only; no files changed. Re-run detent refresh-project %s --yes to apply this exact proposal after review.\n", result.Project)
	return err
}

func readRequiredProjectRefreshFile(path string) ([]byte, fs.FileMode, error) {
	raw, mode, exists, err := readOptionalProjectRefreshFile(path)
	if err != nil {
		return nil, 0, err
	}
	if !exists {
		return nil, 0, fmt.Errorf("read project file %s: %w", path, os.ErrNotExist)
	}
	return raw, mode, nil
}

func readOptionalProjectRefreshFile(path string) ([]byte, fs.FileMode, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0o600, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("read project file %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, false, fmt.Errorf("stat project file %s: %w", path, err)
	}
	return raw, info.Mode().Perm(), true, nil
}

func refreshWorkflowHasFrontmatter(raw []byte) bool {
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	return bytes.HasPrefix(raw, []byte("---\n")) || bytes.HasPrefix(raw, []byte("---\r\n"))
}

func projectRefreshYAMLPathExists(root *yaml.Node, path string) bool {
	return projectRefreshYAMLPathNode(root, path) != nil
}

func projectRefreshYAMLPathNode(root *yaml.Node, path string) *yaml.Node {
	current := root
	for _, part := range strings.Split(path, ".") {
		if current == nil || current.Kind != yaml.MappingNode {
			return nil
		}
		index := projectRefreshYAMLKeyIndex(current, part)
		if index < 0 {
			return nil
		}
		current = current.Content[index+1]
	}
	return current
}

func projectRefreshYAMLPathBool(root *yaml.Node, path string) (bool, bool) {
	node := projectRefreshYAMLPathNode(root, path)
	if node == nil || node.Kind != yaml.ScalarNode {
		return false, false
	}
	value, err := strconv.ParseBool(strings.TrimSpace(node.Value))
	return value, err == nil
}

func projectRefreshYAMLPathHasItems(root *yaml.Node, path string) bool {
	current := root
	for _, part := range strings.Split(path, ".") {
		if current == nil || current.Kind != yaml.MappingNode {
			return false
		}
		index := projectRefreshYAMLKeyIndex(current, part)
		if index < 0 {
			return false
		}
		current = current.Content[index+1]
	}
	return current != nil && current.Kind == yaml.SequenceNode && len(current.Content) > 0
}

func projectRefreshYAMLKeyIndex(node *yaml.Node, key string) int {
	if node == nil || node.Kind != yaml.MappingNode {
		return -1
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return index
		}
	}
	return -1
}

func cloneProjectRefreshYAMLNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	clone := *node
	clone.Content = make([]*yaml.Node, len(node.Content))
	for index, child := range node.Content {
		clone.Content[index] = cloneProjectRefreshYAMLNode(child)
	}
	if node.Alias != nil {
		clone.Alias = cloneProjectRefreshYAMLNode(node.Alias)
	}
	return &clone
}

func projectRefreshSetManagedDefault(key *yaml.Node, value *yaml.Node) {
	key.HeadComment = projectRefreshStripManagedDefault(key.HeadComment)
	marker := refreshManagedDefaultPrefix + projectRefreshYAMLHash(value)
	if strings.TrimSpace(key.HeadComment) == "" {
		key.HeadComment = marker
		return
	}
	key.HeadComment = strings.TrimRight(key.HeadComment, "\n") + "\n" + marker
}

func projectRefreshManagedDefaultHash(key *yaml.Node) (string, bool) {
	if key == nil {
		return "", false
	}
	for _, line := range strings.Split(key.HeadComment, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
		if strings.HasPrefix(line, refreshManagedDefaultPrefix) {
			value := strings.TrimSpace(strings.TrimPrefix(line, refreshManagedDefaultPrefix))
			return value, value != ""
		}
	}
	return "", false
}

func projectRefreshStripManagedDefault(comment string) string {
	lines := strings.Split(comment, "\n")
	kept := lines[:0]
	for _, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
		if strings.HasPrefix(trimmed, refreshManagedDefaultPrefix) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func projectRefreshYAMLHash(node *yaml.Node) string {
	clone := cloneProjectRefreshYAMLNode(node)
	stripProjectRefreshYAMLComments(clone)
	sum := sha256.Sum256(projectRefreshYAMLBytes(clone))
	return hex.EncodeToString(sum[:])
}

func stripProjectRefreshYAMLComments(node *yaml.Node) {
	if node == nil {
		return
	}
	node.HeadComment = ""
	node.LineComment = ""
	node.FootComment = ""
	for _, child := range node.Content {
		stripProjectRefreshYAMLComments(child)
	}
}

func preserveProjectRefreshYAMLComments(existing *yaml.Node, replacement *yaml.Node) {
	if existing == nil || replacement == nil {
		return
	}
	if strings.TrimSpace(existing.HeadComment) != "" {
		replacement.HeadComment = existing.HeadComment
	}
	if strings.TrimSpace(existing.LineComment) != "" {
		replacement.LineComment = existing.LineComment
	}
	if strings.TrimSpace(existing.FootComment) != "" {
		replacement.FootComment = existing.FootComment
	}
}

func projectRefreshYAMLBytes(node *yaml.Node) []byte {
	raw, err := yaml.Marshal(node)
	if err != nil {
		return []byte(fmt.Sprint(node))
	}
	return raw
}

func projectRefreshYAMLValue(node *yaml.Node) string {
	value := strings.Join(strings.Fields(strings.TrimSpace(string(projectRefreshYAMLBytes(node)))), " ")
	if len(value) > 160 {
		return value[:157] + "..."
	}
	return value
}

type projectRefreshDiffOp struct {
	kind byte
	line string
}

func projectRefreshDiff(changes []projectRefreshChange) string {
	var builder strings.Builder
	for index, change := range changes {
		if index > 0 {
			builder.WriteByte('\n')
		}
		beforePath := change.path
		if !change.existed {
			beforePath = "/dev/null"
		}
		fmt.Fprintf(&builder, "--- %s\n+++ %s\n", beforePath, change.path)
		builder.WriteString(projectRefreshUnifiedHunks(change.before, change.after))
	}
	return strings.TrimRight(builder.String(), "\n")
}

func projectRefreshUnifiedHunks(before []byte, after []byte) string {
	left := projectRefreshDiffLines(before)
	right := projectRefreshDiffLines(after)
	ops := projectRefreshDiffOperations(left, right)
	changeIndexes := make([]int, 0)
	for index, operation := range ops {
		if operation.kind != ' ' {
			changeIndexes = append(changeIndexes, index)
		}
	}
	if len(changeIndexes) == 0 {
		return ""
	}

	const contextLines = 3
	type hunk struct{ start, end int }
	hunks := make([]hunk, 0)
	for _, index := range changeIndexes {
		start := max(0, index-contextLines)
		end := min(len(ops), index+contextLines+1)
		if len(hunks) > 0 && start <= hunks[len(hunks)-1].end {
			hunks[len(hunks)-1].end = max(hunks[len(hunks)-1].end, end)
			continue
		}
		hunks = append(hunks, hunk{start: start, end: end})
	}

	var builder strings.Builder
	for _, current := range hunks {
		oldStart, newStart := 1, 1
		for _, operation := range ops[:current.start] {
			if operation.kind != '+' {
				oldStart++
			}
			if operation.kind != '-' {
				newStart++
			}
		}
		oldCount, newCount := 0, 0
		for _, operation := range ops[current.start:current.end] {
			if operation.kind != '+' {
				oldCount++
			}
			if operation.kind != '-' {
				newCount++
			}
		}
		if oldCount == 0 {
			oldStart--
		}
		if newCount == 0 {
			newStart--
		}
		fmt.Fprintf(&builder, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for _, operation := range ops[current.start:current.end] {
			builder.WriteByte(operation.kind)
			builder.WriteString(operation.line)
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}

func projectRefreshDiffLines(raw []byte) []string {
	if len(raw) == 0 {
		return []string{}
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return []string{""}
	}
	return strings.Split(text, "\n")
}

func projectRefreshDiffOperations(left []string, right []string) []projectRefreshDiffOp {
	dp := make([][]int, len(left)+1)
	for index := range dp {
		dp[index] = make([]int, len(right)+1)
	}
	for i := len(left) - 1; i >= 0; i-- {
		for j := len(right) - 1; j >= 0; j-- {
			if left[i] == right[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	ops := make([]projectRefreshDiffOp, 0, len(left)+len(right))
	for i, j := 0, 0; i < len(left) || j < len(right); {
		switch {
		case i < len(left) && j < len(right) && left[i] == right[j]:
			ops = append(ops, projectRefreshDiffOp{kind: ' ', line: left[i]})
			i++
			j++
		case j < len(right) && (i == len(left) || dp[i][j+1] >= dp[i+1][j]):
			ops = append(ops, projectRefreshDiffOp{kind: '+', line: right[j]})
			j++
		default:
			ops = append(ops, projectRefreshDiffOp{kind: '-', line: left[i]})
			i++
		}
	}
	return ops
}
