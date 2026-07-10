package templates

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type SettingsData struct {
	Title            string
	ApplicationName  string
	InstanceName     string
	Version          string
	Build            buildinfo.Info
	StoreName        string
	Snapshot         telemetry.Snapshot
	Global           SettingsGlobal
	Projects         []SettingsProject
	Runtime          SettingsRuntime
	Assets           AssetPaths
	SidebarProjects  []ProjectSmallMultiple
	ActiveNav        string
	ProjectID        string
	ProjectName      string
	SidebarCollapsed bool
	Theme            string
	Density          string
}

type SettingsGlobal struct {
	ConfigPath string
	PathRule   string
}

type SettingsProject struct {
	ID                    string
	WorkflowPath          string
	Workdir               string
	WorktreeRoot          string
	Weight                int
	Priority              int
	Paused                bool
	TrackerKind           string
	TrackerProject        string
	DependencyAutoUnblock string
}

type SettingsRuntime struct {
	DBPath        string
	LogPath       string
	ServerAddress string
}

func settingsPageTitle(data SettingsData) string {
	if strings.TrimSpace(data.Title) != "" {
		return data.Title
	}
	return "Detent settings"
}

func settingsDashboardShellData(data SettingsData) DashboardShellData {
	activeNav := strings.TrimSpace(data.ActiveNav)
	if activeNav == "" {
		activeNav = "settings"
	}
	return DashboardShellData{
		Title:            settingsPageTitle(data),
		ApplicationName:  data.ApplicationName,
		InstanceName:     data.InstanceName,
		Version:          data.Version,
		Snapshot:         data.Snapshot,
		Projects:         data.SidebarProjects,
		Assets:           data.Assets,
		ActiveNav:        activeNav,
		ProjectID:        data.ProjectID,
		ProjectName:      data.ProjectName,
		SidebarCollapsed: data.SidebarCollapsed,
		Theme:            data.Theme,
		Density:          data.Density,
	}
}

func settingsVersionLabel(data SettingsData) string {
	version := strings.TrimSpace(data.Version)
	if version == "" {
		return "dev"
	}
	return version
}

// settingsBuildFooter is the one place the full build string lives; every
// other page shows at most the short version.
func settingsBuildFooter(data SettingsData) string {
	parts := make([]string, 0, 3)
	version := strings.TrimSpace(data.Build.Version)
	if version == "" {
		version = settingsVersionLabel(data)
	}
	build := "Build " + version
	if commit := strings.TrimSpace(data.Build.Commit); commit != "" {
		if len(commit) > 7 {
			commit = commit[:7]
		}
		build += " (" + commit + ")"
	}
	if date := strings.TrimSpace(data.Build.Date); date != "" {
		build += " " + date
	}
	parts = append(parts, build)
	if store := strings.TrimSpace(data.StoreName); store != "" {
		parts = append(parts, "store: "+store)
	}
	return strings.Join(parts, " · ")
}

func settingsProjectTabsVisible(data SettingsData) bool {
	return strings.TrimSpace(data.ActiveNav) == "configuration" && strings.TrimSpace(data.ProjectID) != ""
}

func settingsRowClass(last bool) string {
	base := "group grid grid-cols-[minmax(0,1fr)_44px] items-center gap-x-3.5 gap-y-1 px-4 py-2.5 md:grid-cols-[220px_minmax(0,1fr)_60px] md:gap-y-0"
	if !last {
		return base + " border-b border-line"
	}
	return base
}

func settingsThemeChecked(data SettingsData, theme string) bool {
	if strings.TrimSpace(data.Theme) == "light" {
		return theme == "light"
	}
	return theme == "dark"
}

func settingsDensityChecked(data SettingsData, density string) bool {
	if strings.TrimSpace(data.Density) == "cozy" {
		return density == "cozy"
	}
	return density == "compact"
}

func settingsText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "n/a"
	}
	return value
}

func hasSettingsValue(value string) bool {
	return strings.TrimSpace(value) != ""
}

func settingsPathRule(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unavailable"
	}
	return value
}

func settingsInt(value int) string {
	return formatInt(int64(value))
}

func settingsBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
