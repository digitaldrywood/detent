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
	onboardingprofile "github.com/digitaldrywood/detent/internal/onboarding"
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
	defaultStatusField          = "Status"
	defaultStatusLabelPrefix    = "detent:"
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
		GitHubStatusSource:           config.GitHubStatusSourceProjectV2,
		StatusField:                  defaultStatusField,
		StatusLabelPrefix:            defaultStatusLabelPrefix,
		WorkspaceRoot:                defaultWorkspaceRoot,
		MaxConcurrentAgents:          defaultMaxConcurrentAgents,
		MaxTurns:                     defaultMaxTurns,
		PollingIntervalMS:            defaultPollingIntervalMS,
		MergingConcurrency:           defaultMergingConcurrency,
		DispatchPriorityState:        defaultDispatchPriorityText,
		DeliveryProfile:              onboardingprofile.DeliveryProfileConservativeReview,
		DependencyAutoUnblockEnabled: "false",
		KanbanMode:                   config.KanbanModeIntegration,
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
	beforeCloseout, beforeCloseoutOK := s.onboardingCloseoutSnapshot()

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
		closeout := s.runOnboardingCloseout(c.Request().Context(), form, beforeCloseout, beforeCloseoutOK)
		result := templates.OnboardingResult{
			Kind:    "success",
			Message: "Wrote " + workflowDisplayPath(s.workflow) + ".",
			Details: closeout.details,
		}
		if !closeout.ok {
			result.Kind = "warning"
			result.Message = "Wrote " + workflowDisplayPath(s.workflow) + ". Closeout verifier needs attention."
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
	trackerKind := strings.TrimSpace(c.FormValue("tracker_kind"))
	gitHubStatusSource := strings.TrimSpace(c.FormValue("github_status_source"))
	if choice := strings.TrimSpace(c.FormValue("tracker_choice")); choice != "" {
		trackerKind, gitHubStatusSource = parseTrackerChoice(choice)
	}

	return templates.OnboardingForm{
		TrackerKind:                  trackerKind,
		GitHubStatusSource:           gitHubStatusSource,
		Endpoint:                     strings.TrimSpace(c.FormValue("endpoint")),
		APIKey:                       strings.TrimSpace(c.FormValue("api_key")),
		ProjectSlug:                  strings.TrimSpace(c.FormValue("project_slug")),
		Repo:                         strings.TrimSpace(c.FormValue("repo")),
		StatusField:                  strings.TrimSpace(c.FormValue("status_field")),
		StatusLabelPrefix:            strings.TrimSpace(c.FormValue("status_label_prefix")),
		WorkspaceRoot:                strings.TrimSpace(c.FormValue("workspace_root")),
		MaxConcurrentAgents:          strings.TrimSpace(c.FormValue("max_concurrent_agents")),
		MaxTurns:                     strings.TrimSpace(c.FormValue("max_turns")),
		PollingIntervalMS:            strings.TrimSpace(c.FormValue("polling_interval_ms")),
		MergingConcurrency:           strings.TrimSpace(c.FormValue("merging_concurrency")),
		DispatchPriorityState:        strings.TrimSpace(c.FormValue("dispatch_priority_by_state")),
		DispatchPriorityLabel:        strings.TrimSpace(c.FormValue("dispatch_priority_by_label")),
		DeliveryProfile:              strings.TrimSpace(c.FormValue("delivery_profile")),
		DependencyAutoUnblockEnabled: onboardingBoolValue(c.FormValue("dependency_auto_unblock_enabled")),
		KanbanMode:                   strings.TrimSpace(c.FormValue("kanban_mode")),
	}
}

func parseTrackerChoice(choice string) (string, string) {
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "github_project_v2":
		return config.TrackerGitHub, config.GitHubStatusSourceProjectV2
	case "github_issue_field":
		return config.TrackerGitHub, config.GitHubStatusSourceIssueField
	case "github_label":
		return config.TrackerGitHub, config.GitHubStatusSourceLabel
	case config.TrackerMemory:
		return config.TrackerMemory, ""
	default:
		return choice, ""
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
	case config.TrackerGitHub:
		switch normalizedOnboardingGitHubStatusSource(form.GitHubStatusSource) {
		case config.GitHubStatusSourceProjectV2, config.GitHubStatusSourceIssueField, config.GitHubStatusSourceLabel:
			return nil
		default:
			return []string{"github status source must be project_v2, issue_field, or label"}
		}
	case config.TrackerMemory:
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
	switch normalizedOnboardingGitHubStatusSource(form.GitHubStatusSource) {
	case config.GitHubStatusSourceProjectV2:
		if strings.TrimSpace(form.ProjectSlug) == "" {
			problems = append(problems, "project is required")
		}
		if strings.TrimSpace(form.Repo) != "" && !repoPattern.MatchString(form.Repo) {
			problems = append(problems, "repo must look like owner/name")
		}
		problems = append(problems, rejectNewlines("project", form.ProjectSlug)...)
	case config.GitHubStatusSourceIssueField:
		if !repoPattern.MatchString(form.Repo) {
			problems = append(problems, "repo must look like owner/name")
		}
		problems = append(problems, rejectNewlines("status field", form.StatusField)...)
	case config.GitHubStatusSourceLabel:
		if !repoPattern.MatchString(form.Repo) {
			problems = append(problems, "repo must look like owner/name")
		}
		problems = append(problems, rejectNewlines("status label prefix", form.StatusLabelPrefix)...)
	}
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
	if _, ok := normalizedOnboardingKanbanMode(form.KanbanMode); !ok {
		problems = append(problems, "kanban mode must be read_only or integration")
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
		form.GitHubStatusSource = normalizedOnboardingGitHubStatusSource(form.GitHubStatusSource)
		if form.Endpoint == "" {
			form.Endpoint = "https://api.github.com/graphql"
		}
		if form.APIKey == "" {
			form.APIKey = defaultGitHubAPIKey
		}
		if form.StatusField == "" {
			form.StatusField = defaultStatusField
		}
		if form.StatusLabelPrefix == "" {
			form.StatusLabelPrefix = defaultStatusLabelPrefix
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
	applyDeliveryProfile(form)
	if strings.TrimSpace(form.KanbanMode) == "" {
		form.KanbanMode = recommendedOnboardingKanbanMode(*form)
	}
}

func applyDeliveryProfile(form *templates.OnboardingForm) {
	profile := strings.TrimSpace(form.DeliveryProfile)
	if profile == "" {
		return
	}
	settings, ok := onboardingprofile.DeliveryProfile(profile)
	if !ok {
		return
	}
	form.DeliveryProfile = settings.ID
	form.MergingConcurrency = strconv.Itoa(settings.MergingConcurrency)
	form.DependencyAutoUnblockEnabled = onboardingBool(settings.DependencyAutoUnblockEnabled)
}

func renderWorkflow(form templates.OnboardingForm, sourceRoot string) string {
	settings, hasDeliveryProfile := explicitDeliveryProfile(form.DeliveryProfile)
	if hasDeliveryProfile {
		form.MergingConcurrency = strconv.Itoa(settings.MergingConcurrency)
		form.DependencyAutoUnblockEnabled = onboardingBool(settings.DependencyAutoUnblockEnabled)
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("tracker:\n")
	writeScalar(&b, "  ", "kind", form.TrackerKind)
	switch form.TrackerKind {
	case config.TrackerGitHub:
		writeScalar(&b, "  ", "endpoint", form.Endpoint)
		writeScalar(&b, "  ", "api_key", form.APIKey)
		statusSource := normalizedOnboardingGitHubStatusSource(form.GitHubStatusSource)
		writeScalar(&b, "  ", "github_status_source", statusSource)
		switch statusSource {
		case config.GitHubStatusSourceProjectV2:
			writeScalar(&b, "  ", "project_slug", form.ProjectSlug)
		case config.GitHubStatusSourceIssueField:
			writeScalar(&b, "  ", "repository", form.Repo)
			writeQuotedScalar(&b, "  ", "status_field", form.StatusField)
		case config.GitHubStatusSourceLabel:
			writeScalar(&b, "  ", "repository", form.Repo)
			writeQuotedScalar(&b, "  ", "status_label_prefix", form.StatusLabelPrefix)
		}
	case config.TrackerMemory:
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
	if hasDeliveryProfile {
		writeScalar(&b, "    ", "enabled", onboardingBool(settings.AutoPromoteEnabled))
		writeScalar(&b, "    ", "quiet_seconds", strconv.Itoa(settings.AutoPromoteQuietSeconds))
	} else {
		b.WriteString("    enabled: false\n")
		b.WriteString("    quiet_seconds: 600\n")
	}
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
	if hasDeliveryProfile {
		writeScalar(&b, "  ", "require_automated_review", onboardingBool(settings.GateRequireAutomatedReview))
	}
	b.WriteString("  ci_failure_action: skip\n")
	b.WriteString("  validator:\n")
	b.WriteString("    enabled: false\n")
	b.WriteString("    model: \"\"\n")
	b.WriteString("    min_score: 0.8\n")
	b.WriteString("    block_on:\n")
	b.WriteString("      - p1\n")
	b.WriteString("server:\n")
	if hasDeliveryProfile {
		b.WriteString("  host: 127.0.0.1\n")
	}
	b.WriteString("  kanban:\n")
	writeScalar(&b, "    ", "mode", onboardingKanbanMode(form))
	b.WriteString("    # Integration is recommended for operator-owned local/private installs after mutation authorization and detent doctor --allow-write-probes passes.\n")
	b.WriteString("    # Use read_only for observer/shared dashboards or until write probes pass.\n")
	b.WriteString("hooks:\n")
	b.WriteString("  timeout_ms: 60000\n")
	b.WriteString("---\n\n")
	b.WriteString(renderWorkflowPrompt(form))
	return b.String()
}

func onboardingKanbanMode(form templates.OnboardingForm) string {
	if mode, ok := normalizedOnboardingKanbanMode(form.KanbanMode); ok {
		return mode
	}
	return recommendedOnboardingKanbanMode(form)
}

func recommendedOnboardingKanbanMode(form templates.OnboardingForm) string {
	if form.TrackerKind == config.TrackerGitHub {
		return config.KanbanModeIntegration
	}
	return config.KanbanModeReadOnly
}

func normalizedOnboardingKanbanMode(value string) (string, bool) {
	mode := strings.ToLower(strings.TrimSpace(value))
	switch mode {
	case config.KanbanModeReadOnly, config.KanbanModeIntegration:
		return mode, true
	default:
		return "", false
	}
}

func onboardingBoolValue(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "true") {
		return "true"
	}
	return "false"
}

func onboardingBool(value bool) string {
	if value {
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
	profilePrompt := deliveryProfileWorkflowPrompt(form)
	switch form.TrackerKind {
	case config.TrackerMemory:
		return "You are working on a memory tracker issue `{{ issue.identifier }}`.\n\n" + commonWorkflowPrompt() + profilePrompt
	default:
		switch normalizedOnboardingGitHubStatusSource(form.GitHubStatusSource) {
		case config.GitHubStatusSourceIssueField:
			return "You are working on GitHub issue `{{ issue.identifier }}` with issue-field status in `" + form.Repo + "`.\n\n" + commonWorkflowPrompt() + profilePrompt
		case config.GitHubStatusSourceLabel:
			return "You are working on GitHub issue `{{ issue.identifier }}` with status labels in `" + form.Repo + "`.\n\n" + commonWorkflowPrompt() + profilePrompt
		default:
			return "You are working on GitHub issue `{{ issue.identifier }}` in ProjectV2 `" + form.ProjectSlug + "`.\n\n" + commonWorkflowPrompt() + profilePrompt
		}
	}
}

func deliveryProfileWorkflowPrompt(form templates.OnboardingForm) string {
	settings, ok := explicitDeliveryProfile(form.DeliveryProfile)
	if !ok {
		return ""
	}
	switch settings.ID {
	case onboardingprofile.DeliveryProfileAutonomousDelivery:
		return "\n\nAutonomous delivery still requires linked PRs, green CI, and clear gates.\nUse live reload or a project-scoped refresh after onboarding changes; do not restart Detent or interrupt running agents unless the operator explicitly authorizes it."
	default:
		return "\n\nConservative review mode keeps Detent parked at Human Review until the operator chooses promotion or merge."
	}
}

func explicitDeliveryProfile(value string) (onboardingprofile.DeliveryProfileSettings, bool) {
	if strings.TrimSpace(value) == "" {
		return onboardingprofile.DeliveryProfileSettings{}, false
	}
	return onboardingprofile.DeliveryProfile(value)
}

func normalizedOnboardingGitHubStatusSource(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", config.GitHubStatusSourceProjectV2, "projectv2", "project":
		return config.GitHubStatusSourceProjectV2
	case config.GitHubStatusSourceIssueField, "issuefield", "issues":
		return config.GitHubStatusSourceIssueField
	case config.GitHubStatusSourceLabel, "labels", "issue_label", "issue_labels":
		return config.GitHubStatusSourceLabel
	default:
		return strings.ToLower(strings.TrimSpace(value))
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
validation evidence, blockers, and handoff notes.

## Required Execution Flow

Use the current Detent status above as the source of truth for which section
applies.

### For Todo

1. Move the issue to In Progress.
2. Create or update the persistent ## Codex Workpad comment with the plan,
   acceptance criteria, validation plan, and blockers.
3. Fetch current origin/main, confirm this worktree is based on it, and confirm
   every Depends on: or Blocked by: issue or pull request is merged or otherwise
   terminal before coding.
4. Reproduce or confirm the reported behavior before changing code when the
   issue is a bug.
5. Implement the smallest complete change that satisfies the issue.
6. Run focused tests for touched packages, then run the configured validation
   gate.
7. Commit and push the branch.
8. Open or update a pull request that references the issue.
9. Re-check pull request comments, inline review comments, and CI after the
   latest push.
10. Move the issue to Human Review only after the pull request is open, not a
    draft, references the issue, validation is green, and no actionable review
    comments remain.

### For In Progress

1. Re-read the issue, pull request, comments, and ## Codex Workpad.
2. Continue from the current repository and tracker state.
3. If implementation is complete, run the full pre-review gate and move the
   issue to Human Review only when the gate passes.

### For Rework

1. Re-read all human and bot feedback.
2. Move the issue to In Progress.
3. Fix the requested changes.
4. Push updates to the pull request.
5. Run the full pre-review gate again.
6. Move the issue back to Human Review only when the gate passes.

### For Merging

1. Confirm $go-workflow:ship is available in the Codex environment. If it is
   unavailable, keep the issue in Merging and record the missing ship workflow
   as an external blocker in the ## Codex Workpad.
2. Invoke and follow $go-workflow:ship.
3. Do not call gh pr merge directly outside the ship workflow.
4. End with exactly one terminal outcome:
   - pull request merged and issue moved to Done;
   - issue moved to Rework with an actionable defect;
   - issue remains in Merging with a concrete external blocker recorded in the
     ## Codex Workpad.
5. Move the issue to Done only after the pull request is merged.`
}

func writeScalar(b *strings.Builder, indent string, key string, value string) {
	b.WriteString(indent)
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteByte('\n')
}

func writeQuotedScalar(b *strings.Builder, indent string, key string, value string) {
	writeScalar(b, indent, key, strconv.Quote(value))
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
