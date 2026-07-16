package web

import (
	"context"
	"net/url"
	"strconv"
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
	applySettingsPreferences(c.Request(), &data)
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
		Build:           s.build,
		StoreName:       s.storeName(),
		Snapshot:        snapshot,
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

func (s *Server) storeName() string {
	if strings.TrimSpace(s.dbPath) != "" {
		return "sqlite"
	}
	return "memory"
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
			WorkflowDetailsURL:    workflowDetailsURL(workflow.Tracker.ProjectSlug),
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

func workflowDetailsURL(projectURL string) string {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(projectURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}

	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i := 0; i+3 < len(segments); i++ {
		if (segments[i] != "orgs" && segments[i] != "users") || segments[i+2] != "projects" {
			continue
		}
		projectNumber, err := strconv.Atoi(segments[i+3])
		if err != nil || projectNumber <= 0 {
			return ""
		}
		parsed.Path = "/" + strings.Join(segments[:i+4], "/") + "/workflows"
		parsed.RawPath = ""
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String()
	}
	return ""
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
