package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/agentoverride"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
)

func checkDoctorProjects(ctx context.Context, cfg globalconfig.Config, deps doctorDeps, githubToken RuntimeSecret, allowWriteProbes bool) []doctorCheck {
	if len(cfg.Projects) == 0 {
		return []doctorCheck{
			{
				Name:   "Project workflows",
				Status: doctorWarn,
				Detail: "no projects configured",
				Hint:   "Run detent add-project to add a project.",
			},
		}
	}

	checks := make([]doctorCheck, 0, len(cfg.Projects)*2)
	for _, project := range cfg.Projects {
		checks = append(checks, checkDoctorProject(ctx, project, deps, githubToken, allowWriteProbes)...)
	}

	return checks
}

func doctorProjectCheckJobs(cfg globalconfig.Config, deps doctorDeps, githubToken RuntimeSecret, allowWriteProbes bool) []doctorCheckJob {
	if len(cfg.Projects) == 0 {
		return []doctorCheckJob{{
			Name: "Project workflows",
			Run: func(context.Context) []doctorCheck {
				return []doctorCheck{
					{
						Name:   "Project workflows",
						Status: doctorWarn,
						Detail: "no projects configured",
						Hint:   "Run detent add-project to add a project.",
					},
				}
			},
		}}
	}

	jobs := make([]doctorCheckJob, 0, len(cfg.Projects))
	for _, project := range cfg.Projects {
		id := doctorProjectID(project)
		progress := &doctorCheckProgress{}
		jobs = append(jobs, doctorCheckJob{
			Name:    "Project " + id + " checks",
			Current: progress.Current,
			Run: func(jobCtx context.Context) []doctorCheck {
				return checkDoctorProjectWithProgress(jobCtx, project, deps, githubToken, allowWriteProbes, progress.Set)
			},
		})
	}
	return jobs
}

func checkDoctorProject(ctx context.Context, project globalconfig.Project, deps doctorDeps, githubToken RuntimeSecret, allowWriteProbes bool) []doctorCheck {
	return checkDoctorProjectWithProgress(ctx, project, deps, githubToken, allowWriteProbes, nil)
}

func checkDoctorProjectWithProgress(
	ctx context.Context,
	project globalconfig.Project,
	deps doctorDeps,
	githubToken RuntimeSecret,
	allowWriteProbes bool,
	setCurrent func(string),
) []doctorCheck {
	id := doctorProjectID(project)
	setDoctorCurrentCheck := func(name string) {
		if setCurrent != nil {
			setCurrent(name)
		}
	}
	workflowCheckName := "Project " + id + " workflow"
	setDoctorCurrentCheck(workflowCheckName)
	workflow, err := loadDoctorProjectWorkflow(ctx, project, deps)
	if err != nil {
		return []doctorCheck{
			{
				Name:   workflowCheckName,
				Status: doctorFail,
				Detail: fmt.Sprintf("%s: %v", project.Workflow, err),
				Hint:   "Fix the WORKFLOW.md path or YAML frontmatter.",
			},
			{
				Name:   "Project " + id + " source repo",
				Status: doctorWarn,
				Detail: "skipped because WORKFLOW.md could not be loaded",
				Hint:   "Fix the workflow file, then rerun detent doctor.",
			},
		}
	}
	workflow.Config = doctorWorkflowConfigWithRuntimeGitHubToken(workflow.Config, runtimeGlobalGitHubToken(githubToken))
	if err := workflow.Config.Validate(); err != nil {
		return []doctorCheck{
			{
				Name:   workflowCheckName,
				Status: doctorFail,
				Detail: fmt.Sprintf("%s: %v", project.Workflow, err),
				Hint:   "Fix invalid WORKFLOW.md frontmatter.",
			},
			{
				Name:   "Project " + id + " source repo",
				Status: doctorWarn,
				Detail: "skipped because WORKFLOW.md is invalid",
				Hint:   "Fix the workflow file, then rerun detent doctor.",
			},
		}
	}

	workflowCheck := doctorCheck{
		Name:   workflowCheckName,
		Status: doctorOK,
		Detail: doctorWorkflowDetail(project.Workflow, project, workflow.Config),
	}
	if workflowPath, err := doctorWorkflowOptimizationWorkflowPath(project); err == nil {
		findings := doctorReviewFlowWorkflowFindings(id, workflowPath, workflow.Config, workflow.Prompt)
		if len(findings) > 0 {
			workflowCheck.Status = doctorWarn
			workflowCheck.Detail += "; review-flow prose mismatch: " + doctorWorkflowFindingDetails(findings)
			workflowCheck.Hint = "Align WORKFLOW.md handoff prose with the configured review-flow choice, or adjust the frontmatter if the configured choice is not intended."
			workflowCheck.WorkflowOptimization = doctorWorkflowOptimizationReport{
				Findings:  findings,
				Proposals: doctorWorkflowProposalsForFindings(id, findings, 1),
			}
		}
	}
	checks := []doctorCheck{workflowCheck}
	setDoctorCurrentCheck("Project " + id + " pinned route models")
	checks = append(checks, checkDoctorRouteModels(ctx, id, project, workflow.Config, deps))
	if doctorTrackerUsesGitHubReads(workflow.Config.Tracker.Kind) {
		setDoctorCurrentCheck("Project " + id + " issue agent models")
		checks = append(checks, checkDoctorIssueAgentModels(ctx, id, project, workflow.Config, deps))
	}
	if workflow.Config.Agent.AutoPromote.Enabled {
		setDoctorCurrentCheck("Project " + id + " auto-promote")
		checks = append(checks, checkDoctorAutoPromote(ctx, id, workflow.Config, deps, time.Now()))
	}
	if doctorTrackerUsesGitHubReads(workflow.Config.Tracker.Kind) {
		if workflow.Config.Tracker.GitHubStatusSource == workflowconfig.GitHubStatusSourceLabel {
			setDoctorCurrentCheck("Project " + id + " label status drift")
			checks = append(checks, checkDoctorLabelStatusDrift(ctx, id, workflow.Config, deps))
		}
		setDoctorCurrentCheck("Project " + id + " dependency auto-unblock")
		checks = append(checks, checkDoctorDependencyAutoUnblock(ctx, id, workflow.Config, deps))
		setDoctorCurrentCheck("Project " + id + " blocked recovery")
		checks = append(checks, checkDoctorBlockedRecovery(ctx, id, workflow.Config, deps))
	}
	if workflow.Config.Tracker.Kind == workflowconfig.TrackerLocalSQLite || workflow.Config.Tracker.Kind == workflowconfig.TrackerGitHubLocal {
		setDoctorCurrentCheck("Project " + id + " local SQLite tracker")
		checks = append(checks, checkDoctorLocalSQLiteTracker(ctx, id, project, workflow.Config, deps))
	}
	if workflow.Config.Workspace.Kind == workflowconfig.WorkspaceFilesystem {
		setDoctorCurrentCheck("Project " + id + " filesystem workspace")
		checks = append(checks, checkDoctorFilesystemWorkspace(id, workflow.Config))
		return checks
	}

	sourceRepoCheckName := "Project " + id + " source repo"
	setDoctorCurrentCheck(sourceRepoCheckName)
	sourceRoot := projectSourceRoot(project, workflow.Config)
	if sourceRoot == "" {
		return append(checks, doctorCheck{
			Name:   sourceRepoCheckName,
			Status: doctorFail,
			Detail: "source root is not configured",
			Hint:   "Set workspace.source_root, project workdir, or workspace.root to an existing git checkout.",
		})
	}
	expandedSourceRoot, err := expandDoctorWorkspacePath(sourceRoot)
	if err != nil {
		return append(checks, doctorCheck{
			Name:   sourceRepoCheckName,
			Status: doctorFail,
			Detail: fmt.Sprintf("%s: %v", sourceRoot, err),
			Hint:   "Set workspace.source_root or project workdir to an existing git checkout.",
		})
	}
	if err := deps.gitWorkTree(ctx, expandedSourceRoot); err != nil {
		return append(checks, doctorCheck{
			Name:   sourceRepoCheckName,
			Status: doctorFail,
			Detail: fmt.Sprintf("%s: %v", expandedSourceRoot, err),
			Hint:   "Set workspace.source_root or project workdir to an existing git checkout.",
		})
	}
	checks = append(checks, doctorCheck{
		Name:   sourceRepoCheckName,
		Status: doctorOK,
		Detail: expandedSourceRoot + " is a git worktree",
	})
	if doctorTrackerUsesGitHubReads(workflow.Config.Tracker.Kind) {
		setDoctorCurrentCheck("Project " + id + " GitHub readiness")
		checks = append(checks, checkDoctorGitHubReadiness(ctx, id, project, workflow.Config, deps, githubToken, expandedSourceRoot, allowWriteProbes)...)
	}
	return checks
}

type doctorRouteModelProbeRequest struct {
	ProjectID    string
	Workspace    string
	WorkflowPath string
	RouteIndex   int
	RouteName    string
	RouteRole    string
	Model        string
	Backend      workflowconfig.AgentBackend
}

func checkDoctorRouteModels(ctx context.Context, id string, project globalconfig.Project, cfg workflowconfig.Config, deps doctorDeps) doctorCheck {
	if deps.modelProbe == nil {
		deps.modelProbe = defaultDoctorRouteModelProbe
	}
	name := "Project " + id + " pinned route models"
	backends := doctorWorkflowBackendConfigsByID(cfg)
	workflowPath, workflowPathErr := doctorWorkflowOptimizationWorkflowPath(project)
	workspacePath := projectSourceRoot(project, cfg)
	if expanded, err := expandDoctorWorkspacePath(workspacePath); err == nil {
		workspacePath = expanded
	}

	var probed int
	var skipped int
	var failures []string
	var findings []doctorWorkflowOptimizationFinding
	probeModel := func(index int, route workflowconfig.AgentRoute, backend workflowconfig.AgentBackend, model string, source string) {
		probed++
		routeName := doctorRouteModelName(route, index)
		err := deps.modelProbe(ctx, doctorRouteModelProbeRequest{
			ProjectID:    id,
			Workspace:    workspacePath,
			WorkflowPath: workflowPath,
			RouteIndex:   index,
			RouteName:    routeName,
			RouteRole:    strings.TrimSpace(route.Role),
			Model:        model,
			Backend:      backend,
		})
		if err == nil {
			return
		}
		detail := fmt.Sprintf("project %s route %s model %s via %s rejected by backend: %v", id, routeName, model, source, err)
		failures = append(failures, detail)
		if workflowPathErr != nil {
			return
		}
		findings = append(findings, doctorWorkflowOptimizationFinding{
			RuleID:       doctorWorkflowRulePinnedRouteModelRejected,
			ProjectID:    id,
			WorkflowPath: workflowPath,
			Severity:     "error",
			Title:        "Pinned worker model rejected",
			Detail:       detail,
			Evidence: map[string]any{
				"backend":                 backend.ID,
				"configured_model_source": source,
				"error":                   err.Error(),
				"model":                   model,
				"route":                   routeName,
			},
		})
	}
	for index, route := range cfg.AgentRouteConfigs() {
		model := strings.TrimSpace(route.Model)
		if model == "" {
			continue
		}
		backend, ok := backends[strings.TrimSpace(route.Backend)]
		if !ok || strings.TrimSpace(backend.Kind) != workflowconfig.AgentBackendCodex {
			skipped++
			continue
		}
		probeModel(index, route, backend, model, "agents.routes.model")
	}
	probedCommandBackends := map[string]struct{}{}
	for index, route := range cfg.AgentRouteConfigs() {
		backendID := strings.TrimSpace(route.Backend)
		if _, ok := probedCommandBackends[backendID]; ok {
			continue
		}
		backend, ok := backends[backendID]
		if !ok || strings.TrimSpace(backend.Kind) != workflowconfig.AgentBackendCodex {
			continue
		}
		model := doctorWorkflowBackendCommandModel(backend)
		if model == "" {
			continue
		}
		probedCommandBackends[backendID] = struct{}{}
		probeModel(index, route, backend, model, "agents.backends.command")
	}

	if len(failures) == 0 {
		detail := fmt.Sprintf("validated %d pinned Codex route model(s)", probed)
		if skipped > 0 {
			detail += fmt.Sprintf("; skipped %d non-Codex pinned route model(s)", skipped)
		}
		return doctorCheck{Name: name, Status: doctorOK, Detail: detail}
	}
	report := doctorWorkflowOptimizationReport{
		Findings:  findings,
		Proposals: doctorWorkflowProposalsForFindings(id, findings, 1),
	}
	return doctorCheck{
		Name:                 name,
		Status:               doctorFail,
		Detail:               strings.Join(failures, "; "),
		Hint:                 "Confirm the project's intended model policy, then update the pin to a backend-supported model or remove it to inherit the provider default.",
		WorkflowOptimization: report,
	}
}

func doctorRouteModelName(route workflowconfig.AgentRoute, index int) string {
	if name := strings.TrimSpace(route.Name); name != "" {
		return name
	}
	return fmt.Sprintf("routes[%d]", index)
}

func defaultDoctorRouteModelProbe(ctx context.Context, req doctorRouteModelProbeRequest) error {
	backend, err := buildAgentBackend(req.Backend)
	if err != nil {
		return err
	}
	provider, ok := backend.(runnerpkg.AgentModelCatalogProvider)
	if !ok {
		return errors.New("backend does not advertise a model catalog")
	}
	models, err := provider.ListModels(ctx)
	if err != nil {
		return err
	}
	return validateDoctorModelCatalog(models, req.Model)
}

func validateDoctorModelCatalog(models []runnerpkg.AgentModel, requested string) error {
	want := strings.TrimSpace(requested)
	for _, model := range models {
		if strings.TrimSpace(model.ID) == want || strings.TrimSpace(model.Model) == want {
			if upgrade := strings.TrimSpace(model.Upgrade); upgrade != "" {
				return fmt.Errorf("model %q is retired; use %q", want, upgrade)
			}
			return nil
		}
	}
	return fmt.Errorf("model %q is not available from the backend", want)
}

func checkDoctorIssueAgentModels(ctx context.Context, id string, project globalconfig.Project, cfg workflowconfig.Config, deps doctorDeps) doctorCheck {
	name := "Project " + id + " issue agent models"
	backend, ok := doctorDefaultCodexBackend(cfg)
	if !ok {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "detent-agent model validation skipped because no Codex backend is configured"}
	}
	if deps.autoPromoteConnector == nil {
		deps.autoPromoteConnector = defaultDoctorAutoPromoteConnector
	}
	projectConnector, err := deps.autoPromoteConnector(cfg)
	if err != nil {
		return doctorCheck{Name: name, Status: doctorFail, Detail: fmt.Sprintf("create issue model diagnostic connector: %v", err), Hint: "Fix tracker credentials and rerun detent doctor."}
	}
	if projectConnector == nil {
		return doctorCheck{Name: name, Status: doctorFail, Detail: "create issue model diagnostic connector: connector is nil", Hint: "Fix tracker configuration and rerun detent doctor."}
	}

	states := append(append([]string(nil), cfg.Tracker.ActiveStates...), cfg.Tracker.ObservedStates...)
	issues, fetchErr := projectConnector.FetchIssuesByStates(ctx, states)
	closeErr := closeDoctorAutoPromoteConnector(projectConnector)
	if fetchErr != nil {
		return doctorCheck{Name: name, Status: doctorFail, Detail: fmt.Sprintf("fetch issue model overrides: %v", fetchErr), Hint: "Fix tracker connectivity and rerun detent doctor."}
	}

	if deps.modelProbe == nil {
		deps.modelProbe = defaultDoctorRouteModelProbe
	}
	workspacePath := projectSourceRoot(project, cfg)
	if expanded, err := expandDoctorWorkspacePath(workspacePath); err == nil {
		workspacePath = expanded
	}
	workflowPath, workflowPathErr := doctorWorkflowOptimizationWorkflowPath(project)
	if workflowPathErr != nil {
		workflowPath = strings.TrimSpace(project.Workflow)
	}
	probed := 0
	failures := []string{}
	for _, issue := range issues {
		override, found, err := agentoverride.FromIssueBody(issue.Description)
		if !found {
			continue
		}
		identifier := strings.TrimSpace(issue.Identifier)
		if identifier == "" {
			identifier = strings.TrimSpace(issue.ID)
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("issue %s has invalid detent-agent block: %v", identifier, err))
			continue
		}
		if override.Model == "" {
			continue
		}
		probed++
		err = deps.modelProbe(ctx, doctorRouteModelProbeRequest{
			ProjectID:    id,
			Workspace:    workspacePath,
			WorkflowPath: workflowPath,
			RouteName:    identifier,
			Model:        override.Model,
			Backend:      backend,
		})
		if err != nil {
			failures = append(failures, fmt.Sprintf("issue %s detent-agent model %s rejected by backend: %v", identifier, override.Model, err))
		}
	}

	if len(failures) > 0 {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: strings.Join(failures, "; "),
			Hint:   "Update rejected detent-agent model values in the original issue bodies or remove the model key to inherit the project default.",
		}
	}
	detail := fmt.Sprintf("validated %d detent-agent model override(s)", probed)
	check := doctorCheck{Name: name, Status: doctorOK, Detail: detail}
	if closeErr != nil {
		check.Status = doctorWarn
		check.Detail += "; connector close failed: " + closeErr.Error()
		check.Hint = "Rerun detent doctor and check local network resources."
	}
	return check
}

func doctorDefaultCodexBackend(cfg workflowconfig.Config) (workflowconfig.AgentBackend, bool) {
	backends := doctorWorkflowBackendConfigsByID(cfg)
	for _, route := range cfg.AgentRouteConfigs() {
		if !route.Default || (strings.TrimSpace(route.Role) != "" && !strings.EqualFold(strings.TrimSpace(route.Role), runnerpkg.RoleCode)) {
			continue
		}
		backend, ok := backends[strings.TrimSpace(route.Backend)]
		if ok && backend.Kind == workflowconfig.AgentBackendCodex {
			return backend, true
		}
	}
	for _, backend := range cfg.AgentBackendConfigs() {
		if backend.Kind == workflowconfig.AgentBackendCodex {
			return backend, true
		}
	}
	return workflowconfig.AgentBackend{}, false
}

func doctorProjectID(project globalconfig.Project) string {
	id := strings.TrimSpace(project.ID)
	if id == "" {
		return "project"
	}
	return id
}

func doctorWorkflowDetail(path string, project globalconfig.Project, cfg workflowconfig.Config) string {
	details := []string{path + " is valid"}
	if cfg.Identity.Configured() {
		details = append(details, "identity "+doctorIdentityDetail(cfg.Identity))
	}
	details = append(details, doctorReviewFlowConfigDetail(cfg))
	details = append(details, doctorWorkflowModelChoiceDetail(cfg))
	details = append(details, doctorWorkflowSessionGuardDetail(cfg))
	details = append(details, doctorAuthorizationDetail(project, cfg))
	return strings.Join(details, "; ")
}

func doctorWorkflowModelChoiceDetail(cfg workflowconfig.Config) string {
	choice := doctorWorkflowWorkerModelChoice(cfg)
	if choice.Mode == "pinned" {
		return fmt.Sprintf("worker-model=pinned %s via %s", choice.Model, choice.Source)
	}
	detail := "worker-model=provider-default"
	if choice.Source == "agents.routes.model_field" {
		detail += " with issue-field overrides"
	}
	return detail
}

func doctorWorkflowSessionGuardDetail(cfg workflowconfig.Config) string {
	tokens := "disabled"
	if cfg.Agent.MaxSessionTokens > 0 {
		tokens = strconv.FormatInt(cfg.Agent.MaxSessionTokens, 10)
	}
	multiplier := "disabled"
	if cfg.Agent.MaxSessionContextMultiplier > 0 {
		multiplier = strconv.FormatFloat(cfg.Agent.MaxSessionContextMultiplier, 'g', -1, 64)
	}
	return fmt.Sprintf("session-guard=max_session_tokens=%s, max_session_context_multiplier=%s", tokens, multiplier)
}

func doctorWorkflowFindingDetails(findings []doctorWorkflowOptimizationFinding) string {
	parts := make([]string, 0, len(findings))
	for _, finding := range findings {
		if strings.TrimSpace(finding.Detail) != "" {
			parts = append(parts, finding.Detail)
		}
	}
	return strings.Join(parts, "; ")
}

func doctorIdentityDetail(identity workflowconfig.Identity) string {
	identity.Normalize()
	if !identity.Configured() {
		return "not configured; ownership defaults to assignee"
	}

	details := []string{identity.Name}
	if identity.GitHubLogin != "" {
		details = append(details, "github_login "+identity.GitHubLogin)
	}
	switch identity.OwnershipMode {
	case workflowconfig.IdentityOwnershipField:
		details = append(details, "owner field "+identity.OwnerField)
	default:
		details = append(details, "owner "+identity.OwnershipMode)
	}
	return strings.Join(details, ", ")
}

func doctorAuthorizationDetail(project globalconfig.Project, cfg workflowconfig.Config) string {
	projectAuthorization := project.Authorization.Configured()
	workflowAuthorization := cfg.Tracker.Authorization.Configured()
	switch {
	case projectAuthorization && workflowAuthorization:
		return "authorization selectors from global.yaml and WORKFLOW.md"
	case projectAuthorization:
		return "authorization selector from global.yaml"
	case workflowAuthorization:
		return "authorization selector from WORKFLOW.md"
	default:
		return "authorization allows all issues"
	}
}

func projectSourceRoot(project globalconfig.Project, cfg workflowconfig.Config) string {
	if sourceRoot := strings.TrimSpace(cfg.Workspace.SourceRoot); sourceRoot != "" {
		return sourceRoot
	}
	if workdir := strings.TrimSpace(project.Workdir); workdir != "" {
		return workdir
	}
	if root := strings.TrimSpace(cfg.Workspace.Root); root != "" {
		return root
	}
	return ""
}

func loadDoctorProjectWorkflow(ctx context.Context, project globalconfig.Project, deps doctorDeps) (workflowconfig.Workflow, error) {
	if strings.TrimSpace(project.WorkflowRef) == "" {
		return deps.loadWorkflow(project.Workflow)
	}
	return projectpkg.LoadWorkflowContext(ctx, project)
}
