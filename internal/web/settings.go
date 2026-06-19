package web

import (
	"context"
	"strings"

	"github.com/labstack/echo/v4"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func (s *Server) settings(c echo.Context) error {
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok {
		return s.demoSettings(c, scenario, c.QueryParam("project"))
	}
	data := s.settingsData(c.Request().Context(), c.QueryParam("project"))
	data.SidebarCollapsed = dashboardSidebarCollapsed(c.Request())
	return render(c, templates.Settings(data))
}

func (s *Server) settingsData(ctx context.Context, selectedProjectID string) templates.SettingsData {
	instanceName := s.instanceName()
	globalConfig := s.currentGlobalConfig()
	snapshot := s.latestSnapshot(ctx)
	sidebarProjects := s.projectSmallMultiples(ctx, snapshot)
	projectID, projectName, _ := s.sidebarProjectContext(selectedProjectID, sidebarProjects, snapshot)
	return templates.SettingsData{
		Title:           instancePageTitle(instanceName, "Detent settings"),
		ApplicationName: applicationName(instanceName),
		InstanceName:    instanceName,
		Version:         s.version,
		Global: templates.SettingsGlobal{
			ConfigPath: globalConfig.Path,
			PathRule:   string(s.configRule),
		},
		Projects: settingsProjects(s.registry),
		Runtime: templates.SettingsRuntime{
			DBPath:        s.dbPath,
			LogPath:       s.logPath,
			ServerAddress: s.serverAddr,
		},
		Assets:          s.assets.templatePaths(),
		SidebarProjects: sidebarProjects,
		ActiveNav:       "settings",
		ProjectID:       projectID,
		ProjectName:     projectName,
	}
}

func settingsProjects(registry *project.Registry) []templates.SettingsProject {
	if registry == nil {
		return nil
	}

	projects := registry.List()
	out := make([]templates.SettingsProject, 0, len(projects))
	for _, trackedProject := range projects {
		if trackedProject == nil {
			continue
		}
		cfg := trackedProject.Config()
		workflow := trackedProject.Workflow().Config
		out = append(out, templates.SettingsProject{
			ID:                    string(trackedProject.ID()),
			WorkflowPath:          cfg.Workflow,
			Workdir:               cfg.Workdir,
			WorktreeRoot:          workflow.Workspace.Root,
			Weight:                cfg.Weight,
			Priority:              cfg.Priority,
			Paused:                cfg.Paused,
			TrackerKind:           trackerKind(workflow),
			TrackerProject:        trackerProject(workflow),
			DependencyAutoUnblock: dependencyAutoUnblockPolicy(workflow),
		})
	}
	return out
}

func trackerKind(cfg workflowconfig.Config) string {
	if kind := strings.TrimSpace(cfg.Tracker.Kind); kind != "" {
		return kind
	}
	return "unknown"
}

func trackerProject(cfg workflowconfig.Config) string {
	return strings.TrimSpace(cfg.Tracker.ProjectSlug)
}

func dependencyAutoUnblockPolicy(cfg workflowconfig.Config) string {
	policy := cfg.Tracker.DependencyAutoUnblock
	status := "disabled"
	if policy.Enabled {
		status = "enabled"
	}
	sourceStates := strings.Join(policy.SourceStates, ", ")
	if strings.TrimSpace(sourceStates) == "" {
		sourceStates = "n/a"
	}
	targetState := strings.TrimSpace(policy.TargetState)
	if targetState == "" {
		targetState = "n/a"
	}
	readiness := strings.TrimSpace(policy.Readiness)
	if readiness == "" {
		readiness = workflowconfig.DependencyReadinessTerminalOrMerged
	}
	return status + ": " + sourceStates + " -> " + targetState + " when " + readiness
}
