package templates

import (
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type APIKeysData struct {
	Title                   string
	ApplicationName         string
	InstanceName            string
	Version                 string
	Build                   buildinfo.Info
	Snapshot                telemetry.Snapshot
	Assets                  AssetPaths
	SidebarProjects         []ProjectSmallMultiple
	ActiveNav               string
	ProjectID               string
	ProjectName             string
	SidebarCollapsed        bool
	Theme                   string
	Density                 string
	Search                  string
	Sort                    string
	ActiveRows              []APIKeyRow
	InactiveRows            []APIKeyRow
	ProjectOptions          []APIKeyProjectOption
	ShowStaticOnlyBanner    bool
	StaticTokenConfigured   bool
	WorkItemExampleProject  string
	WorkItemExampleEndpoint string
}

type APIKeyRow struct {
	ID               string
	Name             string
	PrefixLast4      string
	Scope            string
	ProjectLabels    []string
	Status           string
	StatusClass      string
	LastUsed         string
	LastUsedClass    string
	UnusedBadge      bool
	Created          string
	SortCreated      string
	SortLastUsed     string
	AdminConfirmName bool
}

type APIKeyProjectOption struct {
	ID   string
	Name string
}

func apiKeysShellData(data APIKeysData) DashboardShellData {
	activeNav := strings.TrimSpace(data.ActiveNav)
	if activeNav == "" {
		activeNav = "api-keys"
	}
	return DashboardShellData{
		Title:            apiKeysPageTitle(data),
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

func apiKeysPageTitle(data APIKeysData) string {
	if strings.TrimSpace(data.Title) != "" {
		return data.Title
	}
	return "Detent API Keys"
}

func apiKeyScopeClass(scope string) string {
	base := "inline-flex items-center rounded-chip px-2 py-0.5 text-2xs font-medium "
	switch strings.TrimSpace(scope) {
	case "admin":
		return base + "bg-err/15 text-err"
	case "write":
		return base + "bg-accent/15 text-accent"
	default:
		return base + "bg-elev text-sec"
	}
}

func apiKeyStatusClass(row APIKeyRow) string {
	if strings.TrimSpace(row.StatusClass) != "" {
		return row.StatusClass
	}
	return "text-sec"
}

func apiKeyTableEmpty(data APIKeysData) bool {
	return len(data.ActiveRows) == 0 && len(data.InactiveRows) == 0
}

func apiKeyMaskedToken(token string) string {
	if strings.TrimSpace(token) == "" {
		return ""
	}
	return "detent_••••…"
}

func apiKeyCurlSnippet(data APIKeysData, token string) string {
	endpoint := strings.TrimSpace(data.WorkItemExampleEndpoint)
	if endpoint == "" {
		projectID := strings.TrimSpace(data.WorkItemExampleProject)
		if projectID == "" {
			projectID = "PROJECT_ID"
		}
		endpoint = "/api/v1/projects/" + projectID + "/work-items"
	}
	return fmt.Sprintf(`curl -X POST %q \
  -H "Authorization: Bearer %s" \
  -H "Content-Type: application/json" \
  --data '{"title":"Dispatch work item","description":"Full brief in markdown"}'`, endpoint, token)
}

func apiKeyPromptSnippet(data APIKeysData, token string) string {
	endpoint := strings.TrimSpace(data.WorkItemExampleEndpoint)
	if endpoint == "" {
		projectID := strings.TrimSpace(data.WorkItemExampleProject)
		if projectID == "" {
			projectID = "PROJECT_ID"
		}
		endpoint = "/api/v1/projects/" + projectID + "/work-items"
	}
	return "Use Detent's work-item submission API with this bearer token: " + token + "\n\n" +
		"Endpoint: POST " + endpoint + "\n" +
		"Headers: Authorization: Bearer <token>, Content-Type: application/json\n" +
		"Required fields: title, description\n" +
		"Optional fields: state, labels, fields, priority, model_override, deliverable, identifier\n" +
		"Errors: 401 unauthorized/token_revoked/token_expired, 403 forbidden, 404 project_not_found, 409 duplicate identifier, 422 invalid request, 501 unsupported tracker."
}

func apiKeyMaskedSnippet(snippet string, token string) string {
	return strings.ReplaceAll(snippet, token, apiKeyMaskedToken(token))
}

func ifThen(condition bool, value string, fallback string) string {
	if condition {
		return value
	}
	return fallback
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
