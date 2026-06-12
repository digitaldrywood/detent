package templates

import "strings"

type SettingsData struct {
	Title           string
	ApplicationName string
	InstanceName    string
	Version         string
	Global          SettingsGlobal
	Projects        []SettingsProject
	Runtime         SettingsRuntime
	Assets          AssetPaths
}

type SettingsGlobal struct {
	ConfigPath string
	PathRule   string
}

type SettingsProject struct {
	ID             string
	WorkflowPath   string
	Workdir        string
	WorktreeRoot   string
	Weight         int
	Priority       int
	Paused         bool
	TrackerKind    string
	TrackerProject string
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

func settingsVersionLabel(data SettingsData) string {
	version := strings.TrimSpace(data.Version)
	if version == "" {
		return "dev"
	}
	return version
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
