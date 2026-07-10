package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/agentoverride"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/skills"
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
			checkDoctorIssueEffortGuidanceUnavailable(id, "WORKFLOW.md could not be loaded"),
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
			checkDoctorIssueEffortGuidanceUnavailable(id, "WORKFLOW.md is invalid"),
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
		setDoctorCurrentCheck("Project " + id + " issue effort guidance")
		checks = append(checks, checkDoctorIssueEffortGuidanceForSource(id, project, workflow.Config))
		setDoctorCurrentCheck("Project " + id + " skills")
		checks = append(checks, checkDoctorFilesystemProjectSkills(id, project, workflow.Config))
		return checks
	}

	sourceRepoCheckName := "Project " + id + " source repo"
	setDoctorCurrentCheck(sourceRepoCheckName)
	sourceRoot := projectSourceRoot(project, workflow.Config)
	if sourceRoot == "" {
		checks = append(checks, doctorCheck{
			Name:   sourceRepoCheckName,
			Status: doctorFail,
			Detail: "source root is not configured",
			Hint:   "Set workspace.source_root, project workdir, or workspace.root to an existing git checkout.",
		})
		checks = append(checks, checkDoctorIssueEffortGuidanceUnavailable(id, "source root is not configured"))
		return append(checks, checkDoctorProjectSkillsUnavailable(id, workflow.Config.Agent.Skills, "source root is not configured"))
	}
	expandedSourceRoot, err := expandDoctorWorkspacePath(sourceRoot)
	if err != nil {
		checks = append(checks, doctorCheck{
			Name:   sourceRepoCheckName,
			Status: doctorFail,
			Detail: fmt.Sprintf("%s: %v", sourceRoot, err),
			Hint:   "Set workspace.source_root or project workdir to an existing git checkout.",
		})
		checks = append(checks, checkDoctorIssueEffortGuidanceUnavailable(id, "source root could not be resolved"))
		return append(checks, checkDoctorProjectSkillsUnavailable(id, workflow.Config.Agent.Skills, "source root could not be resolved"))
	}
	if err := deps.gitWorkTree(ctx, expandedSourceRoot); err != nil {
		checks = append(checks, doctorCheck{
			Name:   sourceRepoCheckName,
			Status: doctorFail,
			Detail: fmt.Sprintf("%s: %v", expandedSourceRoot, err),
			Hint:   "Set workspace.source_root or project workdir to an existing git checkout.",
		})
		checks = append(checks, checkDoctorIssueEffortGuidanceUnavailable(id, "source repository is unavailable locally"))
		return append(checks, checkDoctorProjectSkillsUnavailable(id, workflow.Config.Agent.Skills, "source repository is unavailable locally"))
	}
	checks = append(checks, doctorCheck{
		Name:   sourceRepoCheckName,
		Status: doctorOK,
		Detail: expandedSourceRoot + " is a git worktree",
	})
	setDoctorCurrentCheck("Project " + id + " issue effort guidance")
	checks = append(checks, checkDoctorIssueEffortGuidance(id, expandedSourceRoot))
	setDoctorCurrentCheck("Project " + id + " skills")
	checks = append(checks, checkDoctorProjectSkills(id, expandedSourceRoot, workflow.Config.Agent.Skills))
	if doctorTrackerUsesGitHubReads(workflow.Config.Tracker.Kind) {
		setDoctorCurrentCheck("Project " + id + " GitHub readiness")
		checks = append(checks, checkDoctorGitHubReadiness(ctx, id, project, workflow.Config, deps, githubToken, expandedSourceRoot, allowWriteProbes)...)
	}
	return checks
}

func checkDoctorIssueEffortGuidanceForSource(id string, project globalconfig.Project, cfg workflowconfig.Config) doctorCheck {
	sourceRoot := projectSourceRoot(project, cfg)
	if sourceRoot == "" {
		return checkDoctorIssueEffortGuidanceUnavailable(id, "source root is not configured")
	}
	expandedSourceRoot, err := expandDoctorWorkspacePath(sourceRoot)
	if err != nil {
		return checkDoctorIssueEffortGuidanceUnavailable(id, "source root could not be resolved")
	}
	info, err := os.Stat(expandedSourceRoot)
	if err != nil || !info.IsDir() {
		return checkDoctorIssueEffortGuidanceUnavailable(id, "source repository is unavailable locally")
	}
	return checkDoctorIssueEffortGuidance(id, expandedSourceRoot)
}

func checkDoctorIssueEffortGuidance(id string, sourceRoot string) doctorCheck {
	name := "Project " + id + " issue effort guidance"
	paths := []string{"AGENTS.md", "CLAUDE.md"}
	for _, path := range paths {
		content, err := os.ReadFile(filepath.Join(sourceRoot, path))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return doctorCheck{
				Name:   name,
				Status: doctorOK,
				Detail: fmt.Sprintf("skipped because %s could not be read: %v", path, err),
			}
		}
		if strings.Contains(strings.ToLower(string(content)), "detent-agent") {
			return doctorCheck{
				Name:   name,
				Status: doctorOK,
				Detail: path + " mentions detent-agent effort guidance",
			}
		}
	}
	return doctorCheck{
		Name:   name,
		Status: doctorWarn,
		Detail: "AGENTS.md and CLAUDE.md contain no detent-agent guidance",
		Hint:   "Add a project-specific effort-selection rubric; see docs/ONBOARDING.md#per-issue-agent-overrides.",
	}
}

func checkDoctorIssueEffortGuidanceUnavailable(id string, reason string) doctorCheck {
	return doctorCheck{
		Name:   "Project " + id + " issue effort guidance",
		Status: doctorOK,
		Detail: "skipped because " + reason,
	}
}

func checkDoctorProjectSkills(id string, sourceRoot string, cfg workflowconfig.Skills) doctorCheck {
	name := "Project " + id + " skills"
	detail := doctorSkillsConfigDetail(cfg)
	if !cfg.Enabled {
		return doctorCheck{Name: name, Status: doctorOK, Detail: detail + "; loaded=0; dropped=0"}
	}

	result, err := skills.Load(sourceRoot, skills.Options{
		Path:              cfg.Path,
		MaxSkillsInPrompt: cfg.MaxSkillsInPrompt,
	})
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorWarn,
			Detail: detail + "; inspection failed: " + err.Error(),
			Hint:   "Fix agent.skills.path or permissions, then rerun detent doctor.",
		}
	}

	detail += fmt.Sprintf("; loaded=%d; dropped=%d", len(result.Skills), len(result.Dropped))
	if len(result.Dropped) == 0 {
		return doctorCheck{Name: name, Status: doctorOK, Detail: detail}
	}

	drops := make([]string, 0, len(result.Dropped))
	for _, drop := range result.Dropped {
		path := drop.Path
		if relative, err := filepath.Rel(sourceRoot, drop.Path); err == nil {
			path = relative
		}
		drops = append(drops, fmt.Sprintf("%s (%s: %s)", path, drop.Reason, drop.Message))
	}
	return doctorCheck{
		Name:   name,
		Status: doctorWarn,
		Detail: detail + "; drops: " + strings.Join(drops, "; "),
		Hint:   "Fix invalid or duplicate skill files, or raise agent.skills.max_skills_in_prompt.",
	}
}

func checkDoctorProjectSkillsUnavailable(id string, cfg workflowconfig.Skills, reason string) doctorCheck {
	if !cfg.Enabled {
		return checkDoctorProjectSkills(id, "", cfg)
	}
	return doctorCheck{
		Name:   "Project " + id + " skills",
		Status: doctorWarn,
		Detail: doctorSkillsConfigDetail(cfg) + "; skipped because " + reason,
		Hint:   "Make the source repository available locally, then rerun detent doctor.",
	}
}

func checkDoctorFilesystemProjectSkills(id string, project globalconfig.Project, cfg workflowconfig.Config) doctorCheck {
	sourceRoot := projectSourceRoot(project, cfg)
	if sourceRoot == "" {
		return checkDoctorProjectSkillsUnavailable(id, cfg.Agent.Skills, "source root is not configured")
	}
	expandedSourceRoot, err := expandDoctorWorkspacePath(sourceRoot)
	if err != nil {
		return checkDoctorProjectSkillsUnavailable(id, cfg.Agent.Skills, "source root could not be resolved")
	}
	info, err := os.Stat(expandedSourceRoot)
	if err != nil || !info.IsDir() {
		return checkDoctorProjectSkillsUnavailable(id, cfg.Agent.Skills, "source repository is unavailable locally")
	}
	return checkDoctorProjectSkills(id, expandedSourceRoot, cfg.Agent.Skills)
}

func doctorSkillsConfigDetail(cfg workflowconfig.Skills) string {
	return fmt.Sprintf(
		"enabled=%t; creation_enabled=%t; max_drafts_per_run=%d; path=%s; max_skills_in_prompt=%d",
		cfg.Enabled,
		cfg.Creation.Enabled,
		cfg.Creation.MaxDraftsPerRun,
		cfg.Path,
		cfg.MaxSkillsInPrompt,
	)
}

type doctorRouteModelProbeRequest struct {
	ProjectID    string
	Workspace    string
	WorkflowPath string
	RouteIndex   int
	RouteName    string
	RouteRole    string
	Model        string
	Effort       string
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
	model := strings.TrimSpace(req.Model)
	if model == "" && strings.TrimSpace(req.Effort) != "" {
		defaultProvider, ok := backend.(runnerpkg.AgentDefaultModelProvider)
		if !ok {
			return errors.New("backend does not advertise its effective default model")
		}
		model, err = defaultProvider.DefaultModel(ctx, req.Workspace)
		if err != nil {
			return fmt.Errorf("effective default model unavailable: %w", err)
		}
	}
	if err := validateDoctorModelCatalog(models, model); err != nil {
		return err
	}
	if strings.TrimSpace(req.Effort) == "" {
		return nil
	}
	return validateDoctorEffortCatalog(models, model, req.Effort)
}

func validateDoctorModelCatalog(models []runnerpkg.AgentModel, requested string) error {
	_, err := doctorCatalogModel(models, requested)
	return err
}

func validateDoctorEffortCatalog(models []runnerpkg.AgentModel, requestedModel string, requestedEffort string) error {
	model, err := doctorCatalogModel(models, requestedModel)
	if err != nil {
		return err
	}
	want := strings.TrimSpace(requestedEffort)
	for _, effort := range model.SupportedReasoningEfforts {
		if strings.EqualFold(strings.TrimSpace(effort), want) {
			return nil
		}
	}
	supported := make([]string, 0, len(model.SupportedReasoningEfforts))
	for _, effort := range model.SupportedReasoningEfforts {
		if effort = strings.TrimSpace(effort); effort != "" {
			supported = append(supported, effort)
		}
	}
	if len(supported) == 0 {
		supported = append(supported, "none")
	}
	return fmt.Errorf("effort %q is not supported by model %q; supported efforts: %s", want, doctorCatalogModelName(model, requestedModel), strings.Join(supported, ", "))
}

func doctorCatalogModel(models []runnerpkg.AgentModel, requested string) (runnerpkg.AgentModel, error) {
	want := strings.TrimSpace(requested)
	for _, model := range models {
		if strings.TrimSpace(model.ID) != want && strings.TrimSpace(model.Model) != want {
			continue
		}
		if upgrade := strings.TrimSpace(model.Upgrade); upgrade != "" {
			return runnerpkg.AgentModel{}, fmt.Errorf("model %q is retired; use %q", want, upgrade)
		}
		return model, nil
	}
	return runnerpkg.AgentModel{}, fmt.Errorf("model %q is not available from the backend", want)
}

func doctorCatalogModelName(model runnerpkg.AgentModel, fallback string) string {
	if value := strings.TrimSpace(model.Model); value != "" {
		return value
	}
	if value := strings.TrimSpace(model.ID); value != "" {
		return value
	}
	return strings.TrimSpace(fallback)
}

func checkDoctorIssueAgentModels(ctx context.Context, id string, project globalconfig.Project, cfg workflowconfig.Config, deps doctorDeps) doctorCheck {
	name := "Project " + id + " issue agent models"
	backends := doctorWorkflowBackendConfigsByID(cfg)
	hasCodexBackend := false
	for _, backend := range backends {
		if strings.TrimSpace(backend.Kind) == workflowconfig.AgentBackendCodex {
			hasCodexBackend = true
			break
		}
	}
	if !hasCodexBackend {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "detent-agent override validation skipped because no Codex backend is configured"}
	}
	if deps.autoPromoteConnector == nil {
		deps.autoPromoteConnector = defaultDoctorAutoPromoteConnector
	}
	projectConnector, err := deps.autoPromoteConnector(cfg)
	if err != nil {
		return doctorCheck{Name: name, Status: doctorFail, Detail: fmt.Sprintf("create issue override diagnostic connector: %v", err), Hint: "Fix tracker credentials and rerun detent doctor."}
	}
	if projectConnector == nil {
		return doctorCheck{Name: name, Status: doctorFail, Detail: "create issue override diagnostic connector: connector is nil", Hint: "Fix tracker configuration and rerun detent doctor."}
	}
	selectorContext := selector.Context{Persona: cfg.Tracker.Assignee}
	if identifier, ok := projectConnector.(connector.InstanceIdentifier); ok {
		selectorContext.InstanceLogin = identifier.InstanceLogin()
	}

	states := append(append([]string(nil), cfg.Tracker.ActiveStates...), cfg.Tracker.ObservedStates...)
	issues, fetchErr := projectConnector.FetchIssuesByStates(ctx, states)
	closeErr := closeDoctorAutoPromoteConnector(projectConnector)
	if fetchErr != nil {
		return doctorCheck{Name: name, Status: doctorFail, Detail: fmt.Sprintf("fetch issue agent overrides: %v", fetchErr), Hint: "Fix tracker connectivity and rerun detent doctor."}
	}
	router, err := doctorIssueAgentRouter(cfg)
	if err != nil {
		return doctorCheck{Name: name, Status: doctorFail, Detail: fmt.Sprintf("create issue agent override router: %v", err), Hint: "Fix agent route configuration and rerun detent doctor."}
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
	probedModels := 0
	probedEfforts := 0
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
		if override.Model == "" && override.Effort == "" {
			continue
		}
		selection, err := router.Route(issue, selectorContext)
		if err != nil {
			failures = append(failures, fmt.Sprintf("issue %s detent-agent route selection failed: %v", identifier, err))
			continue
		}
		backend, ok := backends[strings.TrimSpace(selection.BackendID)]
		if !ok {
			failures = append(failures, fmt.Sprintf("issue %s detent-agent route %s references unavailable backend %s", identifier, selection.RouteName, selection.BackendID))
			continue
		}
		model := override.Model
		if model == "" {
			model = selection.Model
		}
		if override.Model != "" {
			probedModels++
		}
		if override.Effort != "" {
			probedEfforts++
		}
		err = deps.modelProbe(ctx, doctorRouteModelProbeRequest{
			ProjectID:    id,
			Workspace:    workspacePath,
			WorkflowPath: workflowPath,
			RouteName:    identifier,
			Model:        model,
			Effort:       override.Effort,
			Backend:      backend,
		})
		if err != nil {
			fields := []string{}
			if override.Model != "" {
				fields = append(fields, "model "+override.Model)
			}
			if override.Effort != "" {
				fields = append(fields, "effort "+override.Effort)
			}
			failures = append(failures, fmt.Sprintf("issue %s detent-agent %s rejected by backend: %v", identifier, strings.Join(fields, " "), err))
		}
	}

	if len(failures) > 0 {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: strings.Join(failures, "; "),
			Hint:   "Fix rejected detent-agent values in the original issue bodies; remove the model key to inherit the project default model, or remove the effort key to inherit the project default effort.",
		}
	}
	detail := fmt.Sprintf("validated %d detent-agent model override(s) and %d effort override(s)", probedModels, probedEfforts)
	check := doctorCheck{Name: name, Status: doctorOK, Detail: detail}
	if closeErr != nil {
		check.Status = doctorWarn
		check.Detail += "; connector close failed: " + closeErr.Error()
		check.Hint = "Rerun detent doctor and check local network resources."
	}
	return check
}

func doctorIssueAgentRouter(cfg workflowconfig.Config) (*runnerpkg.Router, error) {
	routes := cfg.AgentRouteConfigs()
	doctorRoutes := make([]runnerpkg.Route, 0, len(routes))
	for _, route := range routes {
		doctorRoutes = append(doctorRoutes, runnerpkg.Route{
			Name:       route.Name,
			Role:       route.Role,
			BackendID:  route.Backend,
			Model:      route.Model,
			ModelField: route.ModelField,
			Default:    route.Default,
			Selector:   route.Selector,
		})
	}
	return runnerpkg.NewRouter(doctorRoutes)
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
