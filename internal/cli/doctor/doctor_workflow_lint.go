package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/gate"
)

var jobLabelConditionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)github\.event\.label\.name\s*==\s*'([^']*)'`),
	regexp.MustCompile(`(?i)github\.event\.label\.name\s*==\s*"([^"]*)"`),
	regexp.MustCompile(`(?i)'([^']*)'\s*==\s*github\.event\.label\.name`),
	regexp.MustCompile(`(?i)"([^"]*)"\s*==\s*github\.event\.label\.name`),
	regexp.MustCompile(`(?i)contains\(\s*github\.event\.pull_request\.labels\.\*\.name\s*,\s*'([^']*)'\s*\)`),
	regexp.MustCompile(`(?i)contains\(\s*github\.event\.pull_request\.labels\.\*\.name\s*,\s*"([^"]*)"\s*\)`),
}

type ShipSkill struct {
	Version string
	Path    string
}

func CheckWorkflowLint(ctx context.Context, projectID string, project globalconfig.Project, cfg workflowconfig.Config, prompt string, workflowTokenThreshold int, deps Deps) []Check {
	workflowPath := workflowLintPath(project)
	checks := make([]Check, 0)
	if sizeCheck, ok := checkWorkflowPromptSize(projectID, workflowPath, prompt, cfg.Agent.Skills.Path, workflowTokenThreshold); ok {
		checks = append(checks, sizeCheck)
	}
	if len(cfg.ConfiguredSubsettings("budget")) == 0 {
		if budgetCheck, ok := checkDisabledBudgetCaps(projectID, cfg.Budget); ok {
			budgetCheck.Detail = workflowPath + " " + budgetCheck.Detail
			budgetCheck.Hint = fmt.Sprintf("Set budget.enabled: true in %s, or remove the configured budget caps.", workflowPath)
			checks = append(checks, budgetCheck)
		}
	}
	checks = append(checks, checkInertWorkflowBlocks(projectID, workflowPath, cfg)...)
	checks = append(checks, checkRequiredCheckTriggers(projectID, project, cfg)...)
	checks = append(checks, checkGateCommand(ctx, projectID, workflowPath, project, cfg, deps)...)
	checks = append(checks, checkShipSkill(projectID, workflowPath, cfg, deps)...)
	return checks
}

type requiredCheckTrigger struct {
	matched    bool
	safe       bool
	labelGated bool
	risks      []string
}

func checkRequiredCheckTriggers(projectID string, project globalconfig.Project, cfg workflowconfig.Config) []Check {
	required := UniqueStrings(cfg.Gate.RequiredStatusChecks)
	ciTriggerLabel := gate.Effective(cfg.Gate).CITriggerLabel
	if len(required) == 0 || cfg.Deliverable.Kind != workflowconfig.DeliverablePullRequest {
		return nil
	}
	root, err := ExpandWorkspacePath(ProjectSourceRoot(project, cfg))
	if err != nil {
		return nil
	}
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	triggers := make(map[string]requiredCheckTrigger, len(required))
	for _, name := range required {
		triggers[name] = requiredCheckTrigger{}
	}
	for _, entry := range entries {
		if entry.IsDir() || !workflowYAMLFile(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var document yaml.Node
		if err := yaml.Unmarshal(raw, &document); err != nil || len(document.Content) == 0 {
			continue
		}
		rootNode := document.Content[0]
		on := YAMLMapValue(rootNode, "on")
		workflowRisks := requiredCheckTriggerRisks(on)
		jobs := YAMLMapValue(rootNode, "jobs")
		if jobs == nil || jobs.Kind != yaml.MappingNode {
			continue
		}
		for index := 0; index+1 < len(jobs.Content); index += 2 {
			jobID := strings.TrimSpace(jobs.Content[index].Value)
			jobNode := jobs.Content[index+1]
			jobName := YAMLScalarValue(YAMLMapValue(jobNode, "name"))
			labelGated := requiredCheckLabelGated(on, jobNode)
			risks := append([]string(nil), workflowRisks...)
			jobRequiresLabelEvent := jobRequiresLabelEvent(jobNode)
			jobUsesTriggerLabel := jobUsesTriggerLabel(jobNode)
			jobMatchesTriggerLabel := !jobUsesTriggerLabel || ciTriggerLabel == "" || jobConditionMatchesLabel(jobNode, ciTriggerLabel)
			if jobUsesTriggerLabel {
				if jobRequiresLabelEvent {
					risks = append(risks, "job condition requires labeled event")
				} else {
					risks = append(risks, "job condition requires trigger label")
				}
				if !jobMatchesTriggerLabel {
					risks = append(risks, "job condition does not match gate.ci_trigger_label")
				}
			}
			for _, requiredName := range required {
				if !RequiredCheckMatchesJob(requiredName, jobID, jobName) {
					continue
				}
				trigger := triggers[requiredName]
				trigger.matched = true
				trigger.labelGated = trigger.labelGated || labelGated
				if len(risks) == 0 {
					trigger.safe = true
				} else if labelGated && ciTriggerLabel != "" && jobMatchesTriggerLabel && onlyLabelGateRisk(risks) {
					trigger.safe = true
				} else {
					relativePath, pathErr := filepath.Rel(root, path)
					if pathErr != nil {
						relativePath = path
					}
					for _, risk := range risks {
						trigger.risks = append(trigger.risks, filepath.ToSlash(relativePath)+": "+risk)
					}
				}
				triggers[requiredName] = trigger
			}
		}
	}
	warnings := make([]string, 0)
	for _, name := range required {
		trigger := triggers[name]
		if !trigger.matched || trigger.safe {
			continue
		}
		if trigger.labelGated && ciTriggerLabel == "" {
			warnings = append(warnings, name+" (label-gated: "+strings.Join(UniqueStrings(trigger.risks), "; ")+")")
			continue
		}
		warnings = append(warnings, name+" ("+strings.Join(UniqueStrings(trigger.risks), "; ")+")")
	}
	if len(warnings) == 0 {
		return nil
	}
	detail := "required checks are not produced on every pull-request head: " + strings.Join(warnings, ", ")
	hint := "Remove pull_request path filters and include the synchronize activity for required-check workflows; skip non-applicable work inside the job while still reporting the required check."
	if strings.Contains(detail, "does not match gate.ci_trigger_label") {
		hint = "Set gate.ci_trigger_label to a label explicitly accepted by the required job condition, or update the workflow condition to accept the configured label."
	} else if ciTriggerLabel == "" && strings.Contains(detail, "label-gated:") {
		hint = "Configure gate.ci_trigger_label with the workflow's label so Detent re-applies it after every PR-head push, or include the synchronize activity in the required-check workflow."
	}
	return []Check{{
		Name:   "Project " + projectID + " workflow lint required check triggers",
		Status: Warn,
		Detail: detail,
		Hint:   hint,
	}}
}

func requiredCheckLabelGated(on *yaml.Node, job *yaml.Node) bool {
	if on == nil || on.Kind != yaml.MappingNode {
		return false
	}
	pullRequest := YAMLMapValue(on, "pull_request")
	if pullRequest == nil || pullRequest.Kind != yaml.MappingNode {
		return false
	}
	types := YAMLMapValue(pullRequest, "types")
	if !yAMLContainsScalar(types, "labeled") {
		return false
	}
	return !yAMLContainsScalar(types, "synchronize") || jobUsesTriggerLabel(job)
}

func onlyLabelGateRisk(risks []string) bool {
	if len(risks) == 0 {
		return false
	}
	for _, risk := range risks {
		switch risk {
		case "pull_request types exclude synchronize", "job condition requires labeled event", "job condition requires trigger label":
		default:
			return false
		}
	}
	return true
}

func jobRequiresLabelEvent(job *yaml.Node) bool {
	condition := strings.ToLower(YAMLScalarValue(YAMLMapValue(job, "if")))
	return strings.Contains(condition, "github.event.label") ||
		strings.Contains(condition, "github.event.action") && strings.Contains(condition, "labeled")
}

func jobUsesTriggerLabel(job *yaml.Node) bool {
	condition := strings.ToLower(YAMLScalarValue(YAMLMapValue(job, "if")))
	return jobRequiresLabelEvent(job) || strings.Contains(condition, "github.event.pull_request.labels")
}

func jobConditionMatchesLabel(job *yaml.Node, label string) bool {
	condition := YAMLScalarValue(YAMLMapValue(job, "if"))
	lowerCondition := strings.ToLower(condition)
	if !strings.Contains(lowerCondition, "github.event.label") && !strings.Contains(lowerCondition, "github.event.pull_request.labels") {
		return true
	}
	label = strings.TrimSpace(label)
	for _, pattern := range jobLabelConditionPatterns {
		for _, match := range pattern.FindAllStringSubmatch(condition, -1) {
			if len(match) > 1 && strings.EqualFold(strings.TrimSpace(match[1]), label) {
				return true
			}
		}
	}
	return false
}

func workflowYAMLFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yml", ".yaml":
		return true
	default:
		return false
	}
}

func RequiredCheckMatchesJob(required string, jobID string, jobName string) bool {
	required = strings.TrimSpace(required)
	if index := strings.LastIndex(required, " / "); index >= 0 {
		required = strings.TrimSpace(required[index+3:])
	}
	return strings.EqualFold(required, strings.TrimSpace(jobID)) || strings.EqualFold(required, strings.TrimSpace(jobName))
}

func requiredCheckTriggerRisks(on *yaml.Node) []string {
	if on == nil {
		return []string{"no pull_request or push trigger"}
	}
	switch on.Kind {
	case yaml.ScalarNode:
		if requiredCheckHeadEvent(on.Value) {
			return nil
		}
		return []string{"no pull_request or push trigger"}
	case yaml.SequenceNode:
		for _, event := range on.Content {
			if requiredCheckHeadEvent(event.Value) {
				return nil
			}
		}
		return []string{"no pull_request or push trigger"}
	case yaml.MappingNode:
		risks := make([]string, 0, 4)
		pullRequest := YAMLMapValue(on, "pull_request")
		if pullRequest != nil {
			pullRequestRisks := pullRequestEventRisks(pullRequest)
			if len(pullRequestRisks) == 0 {
				return nil
			}
			risks = append(risks, pullRequestRisks...)
		}
		push := YAMLMapValue(on, "push")
		if push != nil {
			pushRisks := pushEventRisks(push)
			if len(pushRisks) == 0 {
				return nil
			}
			risks = append(risks, pushRisks...)
		}
		if pullRequest == nil && push == nil {
			return []string{"no pull_request or push trigger"}
		}
		return risks
	default:
		return []string{"no pull_request or push trigger"}
	}
}

func requiredCheckHeadEvent(event string) bool {
	event = strings.ToLower(strings.TrimSpace(event))
	return event == "pull_request" || event == "push"
}

func pullRequestEventRisks(event *yaml.Node) []string {
	if event.Kind != yaml.MappingNode {
		return nil
	}
	risks := make([]string, 0, 2)
	if YAMLMapValue(event, "paths") != nil || YAMLMapValue(event, "paths-ignore") != nil {
		risks = append(risks, "pull_request paths filter")
	}
	if types := YAMLMapValue(event, "types"); types != nil && !yAMLContainsScalar(types, "synchronize") {
		risks = append(risks, "pull_request types exclude synchronize")
	}
	return risks
}

func pushEventRisks(event *yaml.Node) []string {
	if event.Kind != yaml.MappingNode {
		return nil
	}
	risks := make([]string, 0, 2)
	if YAMLMapValue(event, "paths") != nil || YAMLMapValue(event, "paths-ignore") != nil {
		risks = append(risks, "push paths filter")
	}
	if YAMLMapValue(event, "branches") != nil || YAMLMapValue(event, "branches-ignore") != nil {
		risks = append(risks, "push branch filter")
	}
	return risks
}

func YAMLMapValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if strings.EqualFold(strings.TrimSpace(node.Content[index].Value), key) {
			return node.Content[index+1]
		}
	}
	return nil
}

func YAMLScalarValue(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return strings.TrimSpace(node.Value)
}

func yAMLContainsScalar(node *yaml.Node, value string) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.ScalarNode {
		return strings.EqualFold(strings.TrimSpace(node.Value), value)
	}
	for _, child := range node.Content {
		if yAMLContainsScalar(child, value) {
			return true
		}
	}
	return false
}

func workflowLintPath(project globalconfig.Project) string {
	if ref := strings.TrimSpace(project.WorkflowRef); ref != "" {
		return ref + ":" + strings.TrimSpace(project.Workflow)
	}
	if path, err := ResolveProjectPath(project, project.Workflow); err == nil {
		return path
	}
	return strings.TrimSpace(project.Workflow)
}

func checkInertWorkflowBlocks(projectID string, workflowPath string, cfg workflowconfig.Config) []Check {
	blocks := []struct {
		path    string
		enabled bool
	}{
		{path: "budget", enabled: cfg.Budget.Enabled},
		{path: "agent.budget", enabled: cfg.Agent.Budget.Enabled},
		{path: "gate.validator", enabled: cfg.Gate.Validator.Enabled},
		{path: "agent.lessons", enabled: cfg.Agent.Lessons.Enabled},
		{path: "agent.skills", enabled: cfg.Agent.Skills.Enabled},
		{path: "agent.knowledge", enabled: cfg.Agent.Knowledge.Enabled},
	}
	checks := make([]Check, 0)
	for _, block := range blocks {
		fields := cfg.ConfiguredSubsettings(block.path)
		if block.enabled || len(fields) == 0 {
			continue
		}
		checks = append(checks, Check{
			Name:   "Project " + projectID + " workflow lint inert " + block.path,
			Status: Warn,
			Detail: fmt.Sprintf("%s configures %s while %s.enabled=false, so the block is inert", workflowPath, strings.Join(fields, ", "), block.path),
			Hint:   fmt.Sprintf("Set %s.enabled: true in %s, or remove the configured %s sub-settings.", block.path, workflowPath, block.path),
		})
	}
	return checks
}

func checkGateCommand(ctx context.Context, projectID string, workflowPath string, project globalconfig.Project, cfg workflowconfig.Config, deps Deps) []Check {
	if gate.NormalizeKind(cfg.Gate.Kind) != gate.KindCommand {
		return nil
	}
	command := strings.TrimSpace(cfg.Gate.Run)
	if (command == "true" || command == ":") && cfg.Gate.RequiredStatusChecks == nil {
		return []Check{workflowGateWarning(projectID, workflowPath, command, "the local command is a no-op but aggregate CI is still required; a head with no CI producer cannot advance", "Configure CI, or explicitly set gate.required_status_checks: [] to use only native branch requirements.")}
	}
	if deps.ResolveCommandInDir == nil {
		return nil
	}
	fields := WorkflowCommandFields(workflowCommandPrefix(command))
	if len(fields) == 0 {
		return []Check{workflowGateWarning(projectID, workflowPath, command, "no executable was found", "Set gate.run to a command available on PATH.")}
	}
	executableIndex := workflowExecutableIndex(fields)
	if executableIndex < 0 || executableIndex >= len(fields) {
		return []Check{workflowGateWarning(projectID, workflowPath, command, "no executable was found", "Set gate.run to a command available on PATH.")}
	}
	executable := fields[executableIndex]
	environment := append([]string(nil), fields[:executableIndex]...)
	workdir := ProjectSourceRoot(project, cfg)
	expandedWorkdir, err := ExpandWorkspacePath(workdir)
	if err != nil {
		return []Check{workflowGateWarning(projectID, workflowPath, command, fmt.Sprintf("project workdir %q cannot be resolved: %v", workdir, err), "Set workspace.source_root or the project workdir to the repository containing the gate target.")}
	}
	resolved, err := deps.ResolveCommandInDir(ctx, expandedWorkdir, environment, executable)
	if err != nil {
		return []Check{workflowGateWarning(projectID, workflowPath, command, fmt.Sprintf("command -v %q failed in %s: %v", executable, expandedWorkdir, err), "Install the executable, fix its project-relative path, or replace gate.run with an available command.")}
	}
	if !makeExecutable(executable) || deps.RunCommandInDir == nil {
		return nil
	}
	commandArgs := []string{}
	if executableIndex+1 < len(fields) {
		commandArgs = fields[executableIndex+1:]
	}
	args := makeDryRunArgs(commandArgs)
	if err := deps.RunCommandInDir(ctx, expandedWorkdir, environment, resolved, args...); err != nil {
		probe := strings.Join(append([]string{executable}, args...), " ")
		return []Check{workflowGateWarning(projectID, workflowPath, command, fmt.Sprintf("dry-run %q failed in %s: %v", probe, expandedWorkdir, err), "Add or fix the make target in the project workdir, or update gate.run to a passing command.")}
	}
	return nil
}

func workflowCommandPrefix(command string) string {
	var prefix strings.Builder
	var quote rune
	escaped := false
	for index, r := range command {
		if escaped {
			prefix.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			prefix.WriteRune(r)
			escaped = true
			continue
		}
		if quote != 0 {
			prefix.WriteRune(r)
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' {
			prefix.WriteRune(r)
			quote = r
			continue
		}
		if r == '$' && index+1 < len(command) && command[index+1] == '(' {
			return strings.TrimSpace(prefix.String())
		}
		if strings.ContainsRune("|&;<>(`\n", r) {
			commandPrefix := prefix.String()
			if r == '>' || r == '<' {
				commandPrefix = workflowTrimRedirectionDescriptor(commandPrefix)
			}
			return strings.TrimSpace(commandPrefix)
		}
		prefix.WriteRune(r)
	}
	return strings.TrimSpace(prefix.String())
}

func workflowTrimRedirectionDescriptor(command string) string {
	trimmed := strings.TrimRight(command, " \t")
	index := len(trimmed)
	for index > 0 && trimmed[index-1] >= '0' && trimmed[index-1] <= '9' {
		index--
	}
	if index == len(trimmed) || index > 0 && trimmed[index-1] != ' ' && trimmed[index-1] != '\t' {
		return command
	}
	return trimmed[:index]
}

func workflowExecutableIndex(fields []string) int {
	for index, field := range fields {
		if workflowEnvironmentAssignment(field) {
			continue
		}
		return index
	}
	return -1
}

func workflowEnvironmentAssignment(field string) bool {
	name, _, ok := strings.Cut(field, "=")
	if !ok || name == "" {
		return false
	}
	for index, r := range name {
		if r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || index > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func makeExecutable(executable string) bool {
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(executable, "\\", "/")))
	return base == "make" || base == "make.exe" || base == "gmake" || base == "gmake.exe"
}

func makeDryRunArgs(fields []string) []string {
	args := []string{"-n"}
	for _, field := range fields {
		switch field {
		case "&&", "||", ";", "|":
			return args
		case "-n", "--just-print", "--dry-run", "--recon":
			continue
		default:
			args = append(args, field)
		}
	}
	return args
}

func workflowGateWarning(projectID string, workflowPath string, command string, failure string, fix string) Check {
	return Check{
		Name:   "Project " + projectID + " workflow lint gate command",
		Status: Warn,
		Detail: fmt.Sprintf("%s gate.run %q cannot work: %s", workflowPath, command, failure),
		Hint:   fix + " Then rerun detent doctor.",
	}
}

func checkShipSkill(projectID string, workflowPath string, cfg workflowconfig.Config, deps Deps) []Check {
	if cfg.Deliverable.Kind != workflowconfig.DeliverablePullRequest || !workflowHasCodexBackend(cfg) || deps.ShipSkillProbe == nil {
		return nil
	}
	codexHome, err := codexHome(deps.LookupEnv)
	if err == nil {
		_, err = deps.ShipSkillProbe(codexHome)
	}
	if err == nil {
		return nil
	}
	return []Check{{
		Name:   "Project " + projectID + " workflow lint ship skill",
		Status: Warn,
		Detail: fmt.Sprintf("%s uses deliverable.kind=pull_request with a Codex backend, but $go-workflow:ship does not resolve: %v", workflowPath, err),
		Hint:   "Enable go-workflow@gopher-ai in $CODEX_HOME/config.toml and install a cache version whose .codex-plugin/plugin.json version matches its directory and contains skills/ship/SKILL.md.",
	}}
}

func workflowHasCodexBackend(cfg workflowconfig.Config) bool {
	for _, backend := range cfg.AgentBackendConfigs() {
		if strings.EqualFold(strings.TrimSpace(backend.Kind), workflowconfig.AgentBackendCodex) {
			return true
		}
	}
	return false
}

func codexHome(LookupEnv func(string) string) (string, error) {
	if LookupEnv != nil {
		if root := strings.TrimSpace(LookupEnv("CODEX_HOME")); root != "" {
			return root, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve CODEX_HOME: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

func ProbeShipSkill(codexHome string) (ShipSkill, error) {
	configPath := filepath.Join(codexHome, "config.toml")
	pattern := filepath.Join(codexHome, "plugins", "cache", "*", "go-workflow", "*")
	candidates, globErr := filepath.Glob(pattern)
	if globErr != nil {
		return ShipSkill{}, globErr
	}
	invalid := make([]string, 0)
	for _, candidate := range candidates {
		provider := filepath.Base(filepath.Dir(filepath.Dir(candidate)))
		configured, configErr := codexPluginEnabled(configPath, "go-workflow", provider)
		manifestPath := filepath.Join(candidate, ".codex-plugin", "plugin.json")
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			invalid = append(invalid, manifestPath+" is missing")
			continue
		}
		var manifest struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(raw, &manifest); err != nil {
			invalid = append(invalid, manifestPath+" is invalid")
			continue
		}
		version := strings.TrimSpace(manifest.Version)
		if strings.TrimSpace(manifest.Name) != "go-workflow" || version == "" || version != filepath.Base(candidate) {
			invalid = append(invalid, fmt.Sprintf("%s version %q does not match cache directory %q", manifestPath, version, filepath.Base(candidate)))
			continue
		}
		skillPath := filepath.Join(candidate, "skills", "ship", "SKILL.md")
		if info, err := os.Stat(skillPath); err != nil || info.IsDir() {
			invalid = append(invalid, skillPath+" is missing")
			continue
		}
		if !configured {
			if configErr != nil {
				invalid = append(invalid, fmt.Sprintf("go-workflow cache version %s exists, but plugin enablement cannot be read: %v", version, configErr))
				continue
			}
			invalid = append(invalid, fmt.Sprintf("go-workflow@%s cache version %s exists, but the plugin is not enabled in %s", provider, version, configPath))
			continue
		}
		return ShipSkill{Version: version, Path: skillPath}, nil
	}
	if len(invalid) > 0 {
		return ShipSkill{}, errors.New(strings.Join(invalid, "; "))
	}
	return ShipSkill{}, fmt.Errorf("go-workflow cache is missing under %s", filepath.Join(codexHome, "plugins", "cache"))
}

func codexPluginEnabled(configPath string, plugin string, provider string) (bool, error) {
	var config struct {
		Plugins map[string]struct {
			Enabled bool `toml:"enabled"`
		} `toml:"plugins"`
	}
	if _, err := toml.DecodeFile(configPath, &config); err != nil {
		return false, err
	}
	for name, pluginConfig := range config.Plugins {
		if (name == plugin || name == plugin+"@"+provider) && pluginConfig.Enabled {
			return true, nil
		}
	}
	return false, nil
}
