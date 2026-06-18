package web

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const (
	onboardingStepTracker     = "tracker"
	onboardingStepCredentials = "credentials"
	onboardingStepProject     = "project"
	onboardingStepAgent       = "agent"
	onboardingStepWrite       = "write"

	defaultGitHubAPIKey         = "$GITHUB_TOKEN"
	defaultWorkspaceRoot        = "~/code/detent-workspaces"
	defaultMaxConcurrentAgents  = "5"
	defaultMaxTurns             = "20"
	defaultMergingConcurrency   = "1"
	defaultDispatchPriorityText = "Merging\nRework"
)

var (
	errWorkflowExists        = errors.New("workflow file already exists")
	repoPattern              = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	defaultPollingIntervalMS = strconv.Itoa(config.DefaultPollingIntervalMS)
	minPollingIntervalMS     = strconv.Itoa(config.MinPollingIntervalMS)
)

func (s *Server) onboarding(c echo.Context) error {
	form := templates.OnboardingForm{
		Step:                         onboardingStepTracker,
		TrackerKind:                  config.TrackerGitHub,
		WorkspaceRoot:                defaultWorkspaceRoot,
		MaxConcurrentAgents:          defaultMaxConcurrentAgents,
		MaxTurns:                     defaultMaxTurns,
		PollingIntervalMS:            defaultPollingIntervalMS,
		MergingConcurrency:           defaultMergingConcurrency,
		DispatchPriorityState:        defaultDispatchPriorityText,
		DependencyAutoUnblockEnabled: "false",
	}
	return render(c, templates.OnboardingPage(s.onboardingData(form, nil, templates.OnboardingResult{})))
}

func (s *Server) onboardingTracker(c echo.Context) error {
	form := parseOnboardingForm(c)
	form.Step = onboardingStepTracker
	problems := validateTracker(form)
	if len(problems) > 0 {
		return s.renderOnboardingStep(c, form, problems, templates.OnboardingResult{})
	}

	applyTrackerDefaults(&form)
	form.Step = onboardingStepCredentials
	return s.renderOnboardingStep(c, form, nil, templates.OnboardingResult{})
}

func (s *Server) onboardingCredentials(c echo.Context) error {
	form := parseOnboardingForm(c)
	form.Step = onboardingStepCredentials
	problems := validateTracker(form)
	if len(problems) == 0 {
		applyTrackerDefaults(&form)
		problems = validateCredentials(form)
	}
	if len(problems) > 0 {
		return s.renderOnboardingStep(c, form, problems, templates.OnboardingResult{})
	}

	applyAgentDefaults(&form)
	form.Step = onboardingStepProject
	return s.renderOnboardingStep(c, form, nil, templates.OnboardingResult{})
}

func (s *Server) onboardingProject(c echo.Context) error {
	form := parseOnboardingForm(c)
	form.Step = onboardingStepProject
	problems := validateThroughProject(&form)
	if len(problems) > 0 {
		return s.renderOnboardingStep(c, form, problems, templates.OnboardingResult{})
	}

	applyAgentDefaults(&form)
	form.Step = onboardingStepAgent
	return s.renderOnboardingStep(c, form, nil, templates.OnboardingResult{})
}

func (s *Server) onboardingAgent(c echo.Context) error {
	form := parseOnboardingForm(c)
	form.Step = onboardingStepAgent
	problems := validateOnboardingForm(&form)
	if len(problems) > 0 {
		return s.renderOnboardingStep(c, form, problems, templates.OnboardingResult{})
	}

	form.Step = onboardingStepWrite
	return s.renderOnboardingStep(c, form, nil, templates.OnboardingResult{})
}

func (s *Server) onboardingWrite(c echo.Context) error {
	form := parseOnboardingForm(c)
	form.Step = onboardingStepWrite
	problems := validateOnboardingForm(&form)
	if len(problems) > 0 {
		return s.renderOnboardingStep(c, form, problems, templates.OnboardingResult{})
	}

	content := renderWorkflow(form, workflowSourceRoot(s.workflow))
	workflow, err := config.ParseWorkflow([]byte(content))
	if err == nil {
		err = workflow.Config.Validate()
	}
	if err != nil {
		result := templates.OnboardingResult{
			Kind:    "error",
			Message: "generated workflow is invalid: " + err.Error(),
		}
		return s.renderOnboardingStep(c, form, nil, result)
	}

	err = writeWorkflowFile(s.workflow, content, c.FormValue("replace") == "true")
	switch {
	case err == nil:
		result := templates.OnboardingResult{
			Kind:    "success",
			Message: "Wrote " + workflowDisplayPath(s.workflow) + ".",
		}
		return s.renderOnboardingStep(c, form, nil, result)
	case errors.Is(err, errWorkflowExists):
		result := templates.OnboardingResult{
			Kind:    "exists",
			Message: workflowDisplayPath(s.workflow) + " already exists.",
		}
		return s.renderOnboardingStep(c, form, nil, result)
	default:
		result := templates.OnboardingResult{
			Kind:    "error",
			Message: fmt.Sprintf("Failed to write %s: %v", workflowDisplayPath(s.workflow), err),
		}
		return s.renderOnboardingStep(c, form, nil, result)
	}
}

func (s *Server) renderOnboardingStep(c echo.Context, form templates.OnboardingForm, problems []string, result templates.OnboardingResult) error {
	return render(c, templates.OnboardingStep(s.onboardingData(form, problems, result)))
}

func (s *Server) onboardingData(form templates.OnboardingForm, problems []string, result templates.OnboardingResult) templates.OnboardingData {
	instanceName := s.instanceName()
	return templates.OnboardingData{
		Title:           instancePageTitle(instanceName, "Detent onboarding"),
		ApplicationName: applicationName(instanceName),
		InstanceName:    instanceName,
		WorkflowPath:    workflowDisplayPath(s.workflow),
		Step:            form.Step,
		Form:            form,
		Errors:          problems,
		Result:          result,
		Assets:          s.assets.templatePaths(),
		Polling:         templates.PollingData{MinIntervalMS: minPollingIntervalMS},
	}
}

func parseOnboardingForm(c echo.Context) templates.OnboardingForm {
	return templates.OnboardingForm{
		TrackerKind:                  strings.TrimSpace(c.FormValue("tracker_kind")),
		Endpoint:                     strings.TrimSpace(c.FormValue("endpoint")),
		APIKey:                       strings.TrimSpace(c.FormValue("api_key")),
		ProjectSlug:                  strings.TrimSpace(c.FormValue("project_slug")),
		Repo:                         strings.TrimSpace(c.FormValue("repo")),
		WorkspaceRoot:                strings.TrimSpace(c.FormValue("workspace_root")),
		MaxConcurrentAgents:          strings.TrimSpace(c.FormValue("max_concurrent_agents")),
		MaxTurns:                     strings.TrimSpace(c.FormValue("max_turns")),
		PollingIntervalMS:            strings.TrimSpace(c.FormValue("polling_interval_ms")),
		MergingConcurrency:           strings.TrimSpace(c.FormValue("merging_concurrency")),
		DispatchPriorityState:        strings.TrimSpace(c.FormValue("dispatch_priority_by_state")),
		DispatchPriorityLabel:        strings.TrimSpace(c.FormValue("dispatch_priority_by_label")),
		DependencyAutoUnblockEnabled: onboardingBoolValue(c.FormValue("dependency_auto_unblock_enabled")),
	}
}

func validateThroughProject(form *templates.OnboardingForm) []string {
	problems := validateTracker(*form)
	if len(problems) > 0 {
		return problems
	}
	applyTrackerDefaults(form)

	problems = append(problems, validateCredentials(*form)...)
	problems = append(problems, validateProject(*form)...)
	return problems
}

func validateOnboardingForm(form *templates.OnboardingForm) []string {
	problems := validateThroughProject(form)
	if len(problems) > 0 {
		return problems
	}
	applyAgentDefaults(form)

	return append(problems, validateAgent(*form)...)
}

func validateTracker(form templates.OnboardingForm) []string {
	switch form.TrackerKind {
	case "":
		return []string{"tracker is required"}
	case config.TrackerGitHub, config.TrackerMemory:
		return nil
	default:
		return []string{"tracker must be github or memory"}
	}
}

func validateCredentials(form templates.OnboardingForm) []string {
	if form.TrackerKind == config.TrackerMemory {
		return nil
	}

	var problems []string
	if strings.TrimSpace(form.Endpoint) == "" {
		problems = append(problems, "endpoint is required")
	} else if _, err := parseHTTPURL(form.Endpoint); err != nil {
		problems = append(problems, "endpoint must be an absolute HTTP URL")
	}
	if strings.TrimSpace(form.APIKey) == "" {
		problems = append(problems, "api key is required")
	}
	problems = append(problems, rejectNewlines("api key", form.APIKey)...)
	return problems
}

func validateProject(form templates.OnboardingForm) []string {
	if form.TrackerKind == config.TrackerMemory {
		return nil
	}

	var problems []string
	if strings.TrimSpace(form.ProjectSlug) == "" {
		problems = append(problems, "project is required")
	}
	if !repoPattern.MatchString(form.Repo) {
		problems = append(problems, "repo must look like owner/name")
	}
	problems = append(problems, rejectNewlines("project", form.ProjectSlug)...)
	return problems
}

func validateAgent(form templates.OnboardingForm) []string {
	var problems []string
	if strings.TrimSpace(form.WorkspaceRoot) == "" {
		problems = append(problems, "workspace root is required")
	}
	problems = append(problems, rejectNewlines("workspace root", form.WorkspaceRoot)...)
	problems = append(problems, positiveIntegerProblem("max concurrent agents", form.MaxConcurrentAgents)...)
	problems = append(problems, positiveIntegerProblem("max turns", form.MaxTurns)...)
	problems = append(problems, positiveIntegerProblem("polling interval", form.PollingIntervalMS)...)
	problems = append(problems, minimumIntegerProblem("polling interval", form.PollingIntervalMS, config.MinPollingIntervalMS)...)
	problems = append(problems, positiveIntegerProblem("merging concurrency", form.MergingConcurrency)...)
	for _, state := range dispatchPriorityStates(form) {
		problems = append(problems, rejectNewlines("dispatch priority state", state)...)
		if strings.TrimSpace(state) == "" {
			problems = append(problems, "dispatch priority states must not be blank")
		}
	}
	for _, label := range dispatchPriorityLabels(form) {
		if strings.TrimSpace(label) == "" {
			problems = append(problems, "dispatch priority labels must not be blank")
		}
	}
	return problems
}

func positiveIntegerProblem(field string, value string) []string {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return []string{field + " must be positive"}
	}
	return nil
}

func minimumIntegerProblem(field string, value string, minimum int) []string {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return nil
	}
	if parsed < minimum {
		return []string{fmt.Sprintf("%s must be at least %d", field, minimum)}
	}
	return nil
}

func rejectNewlines(field string, value string) []string {
	if strings.ContainsAny(value, "\r\n") {
		return []string{field + " must be a single line"}
	}
	return nil
}

func parseHTTPURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("unsupported scheme")
	}
	if parsed.Host == "" {
		return nil, errors.New("missing host")
	}
	return parsed, nil
}

func applyTrackerDefaults(form *templates.OnboardingForm) {
	switch form.TrackerKind {
	case config.TrackerGitHub:
		if form.Endpoint == "" {
			form.Endpoint = "https://api.github.com/graphql"
		}
		if form.APIKey == "" {
			form.APIKey = defaultGitHubAPIKey
		}
	case config.TrackerMemory:
		form.Endpoint = ""
		form.APIKey = ""
	}
}

func applyAgentDefaults(form *templates.OnboardingForm) {
	if form.WorkspaceRoot == "" {
		form.WorkspaceRoot = defaultWorkspaceRoot
	}
	if form.MaxConcurrentAgents == "" {
		form.MaxConcurrentAgents = defaultMaxConcurrentAgents
	}
	if form.MaxTurns == "" {
		form.MaxTurns = defaultMaxTurns
	}
	if form.PollingIntervalMS == "" {
		form.PollingIntervalMS = defaultPollingIntervalMS
	}
	if form.MergingConcurrency == "" {
		form.MergingConcurrency = defaultMergingConcurrency
	}
	if form.DispatchPriorityState == "" {
		form.DispatchPriorityState = defaultDispatchPriorityText
	}
	if form.DependencyAutoUnblockEnabled == "" {
		form.DependencyAutoUnblockEnabled = "false"
	}
}

func renderWorkflow(form templates.OnboardingForm, sourceRoot string) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("tracker:\n")
	writeScalar(&b, "  ", "kind", form.TrackerKind)
	if form.Endpoint != "" {
		writeScalar(&b, "  ", "endpoint", form.Endpoint)
	}
	if form.APIKey != "" {
		writeScalar(&b, "  ", "api_key", form.APIKey)
	}
	if form.ProjectSlug != "" {
		writeScalar(&b, "  ", "project_slug", form.ProjectSlug)
	}
	if form.TrackerKind == config.TrackerMemory {
		b.WriteString("  issues:\n")
		b.WriteString("    - id: memory-onboarding-1\n")
		b.WriteString("      identifier: MEM-1\n")
		b.WriteString("      title: Verify Detent onboarding\n")
		b.WriteString("      description: Run the generated memory workflow through one local dispatch.\n")
		b.WriteString("      priority: 3\n")
		b.WriteString("      state: Todo\n")
		b.WriteString("      labels:\n")
		b.WriteString("        - onboarding\n")
		b.WriteString("      assigned_to_worker: true\n")
	}
	writeList(&b, "  ", "active_states", []string{"Todo", "In Progress", "Rework", "Merging"})
	writeList(&b, "  ", "observed_states", []string{"Backlog", "Human Review", "Blocked"})
	writeList(&b, "  ", "terminal_states", []string{"Done", "Cancelled"})
	b.WriteString("  state_map:\n")
	b.WriteString("    Cancelled: Done\n")
	b.WriteString("  priority_map:\n")
	b.WriteString("    Urgent: 1\n")
	b.WriteString("    High: 2\n")
	b.WriteString("    Medium: 3\n")
	b.WriteString("    Low: 4\n")
	b.WriteString("    No priority: null\n")
	b.WriteString("  dependency_auto_unblock:\n")
	writeScalar(&b, "    ", "enabled", form.DependencyAutoUnblockEnabled)
	writeList(&b, "    ", "source_states", []string{"Blocked"})
	b.WriteString("    target_state: Todo\n")
	b.WriteString("    readiness: terminal_or_merged\n")
	b.WriteString("polling:\n")
	writeScalar(&b, "  ", "interval_ms", form.PollingIntervalMS)
	b.WriteString("workspace:\n")
	writeScalar(&b, "  ", "root", form.WorkspaceRoot)
	writeScalar(&b, "  ", "source_root", sourceRoot)
	b.WriteString("  auto_branch: true\n")
	b.WriteString("agent:\n")
	writeScalar(&b, "  ", "max_concurrent_agents", form.MaxConcurrentAgents)
	writeScalar(&b, "  ", "max_turns", form.MaxTurns)
	b.WriteString("  max_concurrent_agents_by_state:\n")
	writeScalar(&b, "    ", "Merging", form.MergingConcurrency)
	writeList(&b, "  ", "dispatch_priority_by_state", dispatchPriorityStates(form))
	writeList(&b, "  ", "dispatch_priority_by_label", dispatchPriorityLabels(form))
	b.WriteString("  auto_promote:\n")
	b.WriteString("    enabled: false\n")
	b.WriteString("    quiet_seconds: 600\n")
	b.WriteString("    optout_label: requires-human-review\n")
	b.WriteString("    allowed_issue_labels: []\n")
	b.WriteString("  skills:\n")
	b.WriteString("    enabled: true\n")
	b.WriteString("    path: .detent/skills\n")
	b.WriteString("    max_skills_in_prompt: 50\n")
	b.WriteString("codex:\n")
	b.WriteString("  command: codex app-server\n")
	b.WriteString("  approval_policy: never\n")
	b.WriteString("  thread_sandbox: workspace-write\n")
	b.WriteString("  turn_sandbox_policy:\n")
	b.WriteString("    type: workspaceWrite\n")
	b.WriteString("    networkAccess: true\n")
	b.WriteString("gate:\n")
	b.WriteString("  kind: command\n")
	b.WriteString("  run: make check\n")
	b.WriteString("  ci_failure_action: skip\n")
	b.WriteString("hooks:\n")
	b.WriteString("  timeout_ms: 60000\n")
	b.WriteString("---\n\n")
	b.WriteString(renderWorkflowPrompt(form))
	return b.String()
}

func onboardingBoolValue(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "true") {
		return "true"
	}
	return "false"
}

func workflowSourceRoot(workflowPath string) string {
	dir := filepath.Dir(strings.TrimSpace(workflowPath))
	if dir == "" {
		return "."
	}
	return filepath.Clean(dir)
}

func renderWorkflowPrompt(form templates.OnboardingForm) string {
	switch form.TrackerKind {
	case config.TrackerMemory:
		return "You are working on a memory tracker issue `{{ issue.identifier }}`.\n\n" + commonWorkflowPrompt()
	default:
		return "You are working on GitHub issue `{{ issue.identifier }}` in ProjectV2 `" + form.ProjectSlug + "`.\n\n" + commonWorkflowPrompt()
	}
}

func commonWorkflowPrompt() string {
	return `Issue context:
Identifier: {{ issue.identifier }}
Issue node id: {{ issue.id }}
Title: {{ issue.title }}
Current Detent status: {{ issue.state }}
Priority: {{ issue.priority }}
Labels: {{ issue.labels }}
URL: {{ issue.url }}

Description:
{% if issue.description %}
{{ issue.description }}
{% else %}
No description provided.
{% endif %}

Use a single persistent tracker comment headed ## Codex Workpad for the plan,
validation evidence, blockers, and handoff notes. Move Todo work to In Progress
before coding, move validated PRs to Human Review, and keep Merging serialized
until the PR is green and merged.`
}

func writeScalar(b *strings.Builder, indent string, key string, value string) {
	b.WriteString(indent)
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteByte('\n')
}

func writeList(b *strings.Builder, indent string, key string, values []string) {
	b.WriteString(indent)
	b.WriteString(key)
	b.WriteString(":\n")
	for _, value := range values {
		b.WriteString(indent)
		b.WriteString("  - ")
		b.WriteString(value)
		b.WriteByte('\n')
	}
}

func dispatchPriorityStates(form templates.OnboardingForm) []string {
	states := splitOnboardingList(form.DispatchPriorityState)
	if len(states) == 0 {
		return []string{"Merging", "Rework"}
	}
	return states
}

func dispatchPriorityLabels(form templates.OnboardingForm) []string {
	return splitOnboardingList(form.DispatchPriorityLabel)
}

func splitOnboardingList(value string) []string {
	raw := strings.NewReplacer(",", "\n", "\r\n", "\n", "\r", "\n").Replace(value)
	lines := strings.Split(raw, "\n")
	values := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			values = append(values, line)
		}
	}
	return values
}

func writeWorkflowFile(path string, content string, replace bool) error {
	if replace {
		return os.WriteFile(path, []byte(content), 0o600)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return errWorkflowExists
		}
		return err
	}
	defer func() {
		_ = file.Close()
	}()

	_, err = file.WriteString(content)
	return err
}

func workflowDisplayPath(path string) string {
	base := filepath.Base(path)
	if base == "." || base == string(filepath.Separator) {
		return path
	}
	return base
}
