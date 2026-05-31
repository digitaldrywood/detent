package web

import (
	"strings"

	"github.com/labstack/echo/v4"

	workflowconfig "github.com/digitaldrywood/symphony/internal/config"
	"github.com/digitaldrywood/symphony/internal/project"
	"github.com/digitaldrywood/symphony/internal/web/templates"
)

func (s *Server) settings(c echo.Context) error {
	return render(c, templates.Settings(s.settingsData()))
}

func (s *Server) settingsData() templates.SettingsData {
	return templates.SettingsData{
		Title:   "Symphony settings",
		Version: s.version,
		Global: templates.SettingsGlobal{
			ConfigPath: s.globalConfig.Path,
			PathRule:   string(s.configRule),
		},
		Projects: settingsProjects(s.registry),
		Runtime: templates.SettingsRuntime{
			DBPath:        s.dbPath,
			LogPath:       s.logPath,
			ServerAddress: s.serverAddr,
		},
		Assets: s.assets.templatePaths(),
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
			ID:             string(trackedProject.ID()),
			WorkflowPath:   cfg.Workflow,
			Workdir:        cfg.Workdir,
			WorktreeRoot:   workflow.Workspace.Root,
			Weight:         cfg.Weight,
			Priority:       cfg.Priority,
			Paused:         cfg.Paused,
			TrackerKind:    trackerKind(workflow),
			TrackerProject: trackerProject(workflow),
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
