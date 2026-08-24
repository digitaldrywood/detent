package templates

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/a-h/templ"

	"github.com/digitaldrywood/detent/internal/web/ui/components/icon"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

type appNavItem struct {
	ID        string
	Label     string
	Href      string
	Icon      string
	Active    bool
	HealthDot bool
}

type appNavGroup struct {
	ID    string
	Label string
	Items []appNavItem
}

type SSEFingerprint [sha256.Size]byte

type appSidebarFingerprint struct {
	NavGroups  []appNavGroup
	HealthKind primitives.Kind
	Projects   []appShellProject
}

type gitHubAPIHealthSidebarFingerprint struct {
	State  gitHubAPIHealthState
	Label  string
	Active bool
}

func AppSidebarFingerprint(data DashboardShellData) (SSEFingerprint, error) {
	projects := appShellProjects(data)
	if len(projects) <= 1 {
		projects = nil
	}
	fingerprint, err := sseFingerprint(appSidebarFingerprint{
		NavGroups:  appShellNavGroups(data),
		HealthKind: appShellHealthKind(data),
		Projects:   projects,
	})
	if err != nil {
		return SSEFingerprint{}, fmt.Errorf("fingerprint app sidebar: %w", err)
	}
	return fingerprint, nil
}

func GitHubAPIHealthSidebarFingerprint(data DashboardShellData) (SSEFingerprint, error) {
	health := gitHubAPIHealth(data.Snapshot)
	fingerprint, err := sseFingerprint(gitHubAPIHealthSidebarFingerprint{
		State:  health.State,
		Label:  health.Label,
		Active: sidebarStaticNavActive(data, "health"),
	})
	if err != nil {
		return SSEFingerprint{}, fmt.Errorf("fingerprint GitHub API health sidebar item: %w", err)
	}
	return fingerprint, nil
}

func sseFingerprint(value any) (SSEFingerprint, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return SSEFingerprint{}, fmt.Errorf("marshal component view: %w", err)
	}
	return sha256.Sum256(encoded), nil
}

func appShellNavGroups(data DashboardShellData) []appNavGroup {
	active := appShellActiveNav(data)
	return []appNavGroup{
		{
			ID: "primary",
			Items: []appNavItem{
				{ID: "board", Label: "Board", Href: "/", Icon: "kanban", Active: active == "board"},
			},
		},
		{
			ID:    "monitor",
			Label: "Monitor",
			Items: []appNavItem{
				{ID: "fleet", Label: "Fleet", Href: "/fleet", Icon: "network", Active: active == "fleet"},
				{ID: "diagnostics", Label: "Diagnostics", Href: "/diagnostics", Icon: "gauge", Active: active == "diagnostics"},
				{ID: "health", Label: "Health", Href: "/health/ui", Icon: "activity", Active: active == "health", HealthDot: true},
			},
		},
		{
			ID:    "insights",
			Label: "Insights",
			Items: []appNavItem{
				{ID: "reports", Label: "Reports", Href: "/reports", Icon: "file-chart-column", Active: active == "reports"},
				{ID: "library", Label: "Library", Href: "/library", Icon: "library", Active: active == "library"},
			},
		},
		{
			ID:    "system",
			Label: "System",
			Items: []appNavItem{
				{ID: "analytics", Label: "Analytics", Href: "/analytics", Icon: "chart-no-axes-combined", Active: active == "analytics"},
				{ID: "api-keys", Label: "API Keys", Href: "/api-keys", Icon: "key-round", Active: active == "api-keys"},
				{ID: "settings", Label: "Settings", Href: "/settings", Icon: "settings", Active: active == "settings"},
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
	case "library", "reports", "analytics", "diagnostics", "health", "api-keys", "settings":
		return nav
	}
	return ""
}

func appShellHealthKind(data DashboardShellData) primitives.Kind {
	if refreshSnapshotFailed(data.Snapshot) {
		return primitives.KindErr
	}
	if len(boardAlerts(data.Snapshot)) > 0 {
		return primitives.KindErr
	}
	switch gitHubAPIHealth(data.Snapshot).State {
	case gitHubAPIHealthStateHealthy, gitHubAPIHealthStateAtRest:
		return primitives.KindOK
	}
	if !data.Snapshot.GeneratedAt.IsZero() || diagnosticsSnapshotHasLoadedData(data.Snapshot) {
		return primitives.KindOK
	}
	return primitives.KindNeutral
}

func appLiveStatusKind(data DashboardShellData) primitives.Kind {
	return refreshFreshnessKind(data.Snapshot)
}

func appLiveStatusLabel(data DashboardShellData) string {
	if refreshSnapshotFailed(data.Snapshot) {
		return "Live · refresh failed"
	}
	if data.Snapshot.LastKnown {
		return "Live · last-known data"
	}
	if data.Snapshot.Refresh.Stale(data.Snapshot.GeneratedAt) || data.Snapshot.Refresh.Behind() {
		return "Live · data delayed"
	}
	if appLiveStatusKind(data) == primitives.KindOK {
		return "Live · data current"
	}
	return "Live · waiting for data"
}

func appLiveStatusTextClass(data DashboardShellData) string {
	if appLiveStatusKind(data) == primitives.KindErr {
		return "text-err"
	}
	if appLiveStatusKind(data) == primitives.KindWarn {
		return "text-warn"
	}
	return "text-sec"
}

func appLiveStatusAt(data DashboardShellData) time.Time {
	if at := refreshOldestSuccess(data.Snapshot.Refresh.Sources); !at.IsZero() {
		return at
	}
	if data.Snapshot.Refresh.LastRefreshAt != nil {
		return data.Snapshot.Refresh.LastRefreshAt.UTC()
	}
	if snapshotHasPriorTrackerSnapshot(data.Snapshot) {
		return data.Snapshot.GeneratedAt
	}
	return time.Time{}
}

type appShellProject struct {
	ID                         string
	Name                       string
	Initials                   string
	Href                       string
	Live                       bool
	Count                      string
	Todo                       int
	Active                     int
	Waiting                    int
	Blocked                    int
	Paused                     bool
	Selected                   bool
	ActiveHoursVisible         bool
	ActiveHoursCozyLabel       string
	ActiveHoursCompactLabel    string
	ActiveHoursHelpDescription string
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
			ID:       id,
			Name:     projectSmallMultipleName(project),
			Href:     projectOpenPath(id),
			Live:     project.Running > 0,
			Todo:     project.BoardTodo,
			Active:   project.BoardActive,
			Waiting:  project.BoardWaiting,
			Blocked:  project.BoardBlocked,
			Paused:   project.Paused,
			Selected: strings.TrimSpace(data.ProjectID) == id,
		}
		item.ActiveHoursVisible, item.ActiveHoursCozyLabel, item.ActiveHoursCompactLabel, item.ActiveHoursHelpDescription = appProjectActiveHoursIndicator(project)
		item.ActiveHoursVisible = item.ActiveHoursVisible && !item.Paused
		item.Initials = appProjectInitials(item.Name, item.ID)
		if project.Paused {
			item.Count = "paused"
		} else if project.BoardLoad > 0 || project.BoardBlocked > 0 {
			item.Count = strconv.Itoa(project.BoardLoad)
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
	base := "relative flex items-center justify-between gap-2 rounded-card px-2.5 py-1.5 text-sm group-data-[rail=true]/rail:justify-center group-data-[rail=true]/rail:px-0 group-data-[rail=true]/rail:py-2 "
	if item.Active {
		return base + "bg-elev font-medium text-text group-data-[rail=true]/rail:text-accent group-data-[rail=true]/rail:ring-1 group-data-[rail=true]/rail:ring-accent/40"
	}
	return base + "text-sec hover:bg-elev/60 hover:text-text"
}

func appNavIcon(item appNavItem) templ.Component {
	return icon.Icon(item.Icon)(icon.Props{Class: "size-4"})
}

func appProjectLinkClass(project appShellProject) string {
	base := "relative flex items-center gap-2 rounded-card px-2.5 py-1.5 text-sm group-data-[rail=true]/rail:justify-center group-data-[rail=true]/rail:px-0 "
	if project.Selected {
		return base + "bg-elev font-medium text-text group-data-[rail=true]/rail:text-accent group-data-[rail=true]/rail:ring-1 group-data-[rail=true]/rail:ring-accent/40"
	}
	return base + "text-text hover:bg-elev/60"
}

func appProjectCountClass(project appShellProject) string {
	base := "rounded-chip px-1.5 py-0.5 font-mono text-2xs font-medium tabular-nums group-data-[rail=true]/rail:absolute group-data-[rail=true]/rail:-bottom-0.5 group-data-[rail=true]/rail:-right-0.5 group-data-[rail=true]/rail:min-w-3 group-data-[rail=true]/rail:px-0.5 group-data-[rail=true]/rail:py-0 group-data-[rail=true]/rail:text-center group-data-[rail=true]/rail:text-[9px] group-data-[rail=true]/rail:ring-1 group-data-[rail=true]/rail:ring-page "
	if project.Paused {
		return base + "bg-warn/15 text-warn"
	}
	if project.Blocked > 0 {
		return base + "bg-err/15 text-err"
	}
	return base + "bg-elev text-sec"
}

func appProjectInitials(name string, id string) string {
	value := strings.TrimSpace(name)
	if value == "" {
		value = strings.TrimSpace(id)
	}
	words := strings.FieldsFunc(value, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	if len(words) == 0 {
		return "?"
	}
	initials := make([]rune, 0, 2)
	if len(words) > 1 {
		for _, word := range words[:2] {
			for _, r := range word {
				initials = append(initials, r)
				break
			}
		}
	} else {
		for _, r := range words[0] {
			initials = append(initials, r)
			if len(initials) == 2 {
				break
			}
		}
	}
	return strings.ToUpper(string(initials))
}

func appProjectDotKind(project appShellProject) primitives.Kind {
	if project.Paused {
		return primitives.KindWarn
	}
	if project.ActiveHoursVisible {
		return primitives.KindNeutral
	}
	if project.Live {
		return primitives.KindOK
	}
	return primitives.KindNeutral
}

func appProjectBreakdown(project appShellProject) string {
	return strings.Join([]string{
		strconv.Itoa(project.Todo) + " ready",
		strconv.Itoa(project.Active) + " active",
		strconv.Itoa(project.Waiting) + " waiting",
		strconv.Itoa(project.Blocked) + " blocked",
	}, " · ")
}

func appProjectStatus(project appShellProject) string {
	if project.Paused {
		return "paused"
	}
	if project.ActiveHoursVisible {
		return "off hours"
	}
	if project.Live {
		return "active"
	}
	return "idle"
}

func appProjectBadgeLabel(project appShellProject) string {
	if project.Paused {
		return "paused"
	}
	return project.Count + " board load"
}

func appProjectActiveHoursIndicator(project ProjectSmallMultiple) (bool, string, string, string) {
	visible, _, compact, detail := projectActiveHoursIndicator(project)
	if !visible {
		return false, "", "", ""
	}
	cozy := "Off hours"
	if project.ActiveHours.NextOpen != nil {
		cozy = "Off · " + compact
	}
	return true, cozy, compact, detail
}

func appProjectHelpScope(project appShellProject) string {
	if project.ActiveHoursVisible {
		return "project-active-hours"
	}
	return "project-load"
}

func appProjectHelpTerm(project appShellProject) string {
	if project.ActiveHoursVisible {
		return "sidebar-active-hours-" + project.ID
	}
	return "sidebar-project-" + project.ID
}

func appProjectHelpTitle(project appShellProject) string {
	if project.ActiveHoursVisible {
		return "Active hours · " + project.Name
	}
	return project.Name
}

func appProjectHelpDescription(project appShellProject) string {
	if project.ActiveHoursVisible {
		return project.ActiveHoursHelpDescription + " Board: " + appProjectBreakdown(project) + "."
	}
	return appProjectBreakdown(project)
}

func appProjectAriaLabel(project appShellProject) string {
	if project.ActiveHoursVisible {
		return project.Name + ", " + project.ActiveHoursCozyLabel
	}
	return project.Name
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
	switch strings.TrimSpace(data.Density) {
	case "compact", "comfy":
		return strings.TrimSpace(data.Density)
	default:
		return "cozy"
	}
}

func appShellHTMLAttributes(data DashboardShellData) templ.Attributes {
	attrs := templ.Attributes{
		"lang":                   "en",
		"data-detent-connection": "connecting",
	}
	if strings.TrimSpace(data.Theme) == "light" {
		attrs["data-theme"] = "light"
	}
	attrs["data-density"] = appShellDensity(data)
	return attrs
}
