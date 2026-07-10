package templates

import (
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

// appNavItem is one text link in the redesigned sidebar.
type appNavItem struct {
	ID        string
	Label     string
	Href      string
	Active    bool
	HealthDot bool
}

type appNavGroup struct {
	ID    string
	Label string
	Items []appNavItem
}

func appShellNavGroups(data DashboardShellData) []appNavGroup {
	active := appShellActiveNav(data)
	return []appNavGroup{
		{
			ID: "primary",
			Items: []appNavItem{
				{ID: "board", Label: "Board", Href: "/", Active: active == "board"},
			},
		},
		{
			ID:    "monitor",
			Label: "Monitor",
			Items: []appNavItem{
				{ID: "fleet", Label: "Fleet", Href: "/fleet", Active: active == "fleet"},
				{ID: "health", Label: "Health", Href: "/health/ui", Active: active == "health", HealthDot: true},
			},
		},
		{
			ID:    "insights",
			Label: "Insights",
			Items: []appNavItem{
				{ID: "reports", Label: "Reports", Href: "/reports", Active: active == "reports"},
				{ID: "library", Label: "Library", Href: "/library", Active: active == "library"},
			},
		},
		{
			ID:    "system",
			Label: "System",
			Items: []appNavItem{
				{ID: "analytics", Label: "Analytics", Href: "/analytics", Active: active == "analytics"},
				{ID: "api-keys", Label: "API Keys", Href: "/api-keys", Active: active == "api-keys"},
				{ID: "settings", Label: "Settings", Href: "/settings", Active: active == "settings"},
			},
		},
	}
}

// appShellActiveNav normalizes legacy ActiveNav values onto the redesign's
// top-level views. Project-scoped pages activate no top-level link;
// the project row in the sidebar carries selection instead.
func appShellActiveNav(data DashboardShellData) string {
	nav := strings.TrimSpace(data.ActiveNav)
	if strings.TrimSpace(data.ProjectID) != "" {
		return ""
	}
	switch nav {
	case "", "board", "kanban":
		return "board"
	case "fleet":
		return "fleet"
	case "library", "reports", "analytics", "health", "api-keys", "settings":
		return nav
	}
	return ""
}

func appShellHealthKind(data DashboardShellData) primitives.Kind {
	switch gitHubAPIHealth(data.Snapshot).State {
	case gitHubAPIHealthStateHealthy, gitHubAPIHealthStateAtRest:
		return primitives.KindOK
	case gitHubAPIHealthStateWarning:
		return primitives.KindWarn
	case gitHubAPIHealthStateBackoff, gitHubAPIHealthStateExhausted:
		return primitives.KindErr
	}
	return primitives.KindNeutral
}

// appShellProject is one sidebar project row: a status dot, the name, and
// at most one count. Counts render only when they demand attention —
// blocked (err) wins over running; idle projects show no number at all.
type appShellProject struct {
	ID       string
	Name     string
	Href     string
	Kind     primitives.Kind
	Pulse    bool
	Count    string
	CountErr bool
	Active   bool
}

func appShellProjects(data DashboardShellData) []appShellProject {
	projects := append([]ProjectSmallMultiple(nil), data.Projects...)
	sortProjectSmallMultiples(projects)
	items := make([]appShellProject, 0, len(projects))
	for _, project := range projects {
		id := strings.TrimSpace(project.ID)
		if id == "" {
			continue
		}
		item := appShellProject{
			ID:     id,
			Name:   projectSmallMultipleName(project),
			Href:   projectOpenPath(id),
			Kind:   primitives.KindNeutral,
			Active: strings.TrimSpace(data.ProjectID) == id,
		}
		switch {
		case project.Blocked > 0:
			item.Kind = primitives.KindErr
			item.Count = strconv.Itoa(project.Blocked)
			item.CountErr = true
		case project.Running > 0:
			item.Kind = primitives.KindOK
			item.Pulse = true
			item.Count = strconv.Itoa(project.Running)
		case project.QueueCount > 0:
			item.Count = strconv.Itoa(project.QueueCount)
		}
		items = append(items, item)
	}
	return items
}

func appShellVersionLabel(data DashboardShellData) string {
	version := strings.TrimSpace(data.Version)
	if version == "" {
		return ""
	}
	if strings.HasPrefix(version, "v") {
		return version
	}
	return "v" + version
}

func appShellClockLabel(now time.Time) string {
	return localTimeToken(now, LocalTimeWithSeconds)
}

func appShellSnapshotClock(data DashboardShellData) string {
	if data.Snapshot.GeneratedAt.IsZero() {
		return "--:--:--"
	}
	return appShellClockLabel(data.Snapshot.GeneratedAt)
}

func appShellRailValue(data DashboardShellData) string {
	if data.SidebarCollapsed {
		return "true"
	}
	return "false"
}

func appShellTopbarTitle(data DashboardShellData) string {
	if name := strings.TrimSpace(data.ProjectName); name != "" {
		return name
	}
	if active := appShellActiveNav(data); active != "" {
		for _, group := range appShellNavGroups(data) {
			for _, item := range group.Items {
				if item.ID == active {
					return item.Label
				}
			}
		}
	}
	return applicationName(data.ApplicationName)
}

func appNavLinkClass(item appNavItem) string {
	base := "flex items-center justify-between gap-2 rounded-card px-2.5 py-1.5 text-sm group-data-[rail=true]/rail:justify-center group-data-[rail=true]/rail:px-0 group-data-[rail=true]/rail:py-2 "
	if item.Active {
		return base + "bg-elev font-medium text-text"
	}
	return base + "text-sec hover:bg-elev/60 hover:text-text"
}

func appNavInitial(item appNavItem) string {
	if item.Label == "" {
		return ""
	}
	return item.Label[:1]
}

func appProjectLinkClass(project appShellProject) string {
	base := "flex items-center gap-2 rounded-card px-2.5 py-1.5 text-sm group-data-[rail=true]/rail:justify-center group-data-[rail=true]/rail:px-0 "
	if project.Active {
		return base + "bg-elev font-medium text-text"
	}
	return base + "text-text hover:bg-elev/60"
}

func appProjectCountClass(project appShellProject) string {
	base := "font-mono text-2xs font-medium tabular-nums group-data-[rail=true]/rail:hidden "
	if project.CountErr {
		return base + "text-err"
	}
	return base + "text-sec"
}

func appDensityButtonClass(data DashboardShellData, choice string) string {
	base := "px-2.5 py-1 text-2xs "
	if appShellDensity(data) == choice {
		return base + "bg-elev font-medium text-text"
	}
	return base + "text-sec hover:text-text"
}

func appDensityPressed(data DashboardShellData, choice string) string {
	if appShellDensity(data) == choice {
		return "true"
	}
	return "false"
}

func appShellDensity(data DashboardShellData) string {
	if strings.TrimSpace(data.Density) == "cozy" {
		return "cozy"
	}
	return "compact"
}

func appShellHTMLAttributes(data DashboardShellData) templ.Attributes {
	attrs := templ.Attributes{"lang": "en"}
	if strings.TrimSpace(data.Theme) == "light" {
		attrs["data-theme"] = "light"
	}
	if strings.TrimSpace(data.Density) == "cozy" {
		attrs["data-density"] = "cozy"
	}
	return attrs
}
