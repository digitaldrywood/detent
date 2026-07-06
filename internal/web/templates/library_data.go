package templates

import (
	"net/url"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type LibraryData struct {
	Title            string
	ApplicationName  string
	InstanceName     string
	Version          string
	Build            buildinfo.Info
	ConnectorName    string
	Snapshot         telemetry.Snapshot
	Assets           AssetPaths
	SidebarProjects  []ProjectSmallMultiple
	ActiveNav        string
	ProjectID        string
	ProjectName      string
	SidebarCollapsed bool
	Theme            string
	Density          string
	Filters          LibraryFilters
	Summary          LibrarySummary
	Rows             []LibraryRow
	ProjectOptions   []LibraryFilterOption
	KindOptions      []LibraryFilterOption
	StatusOptions    []LibraryFilterOption
	Warnings         []string
	UnfilteredCount  int
	FilteredCount    int
	HasActiveFilters bool
}

type LibraryFilters struct {
	ProjectID string
	Kind      string
	Status    string
	From      time.Time
	To        time.Time
	FromValue string
	ToValue   string
}

type LibrarySummary struct {
	Total        int
	Projects     int
	Artifacts    int
	PullRequests int
	Validated    int
}

type LibraryRow struct {
	ID               string
	SourceKind       string
	ProjectID        string
	ProjectName      string
	Kind             string
	ArtifactPath     string
	ValidationStatus string
	ReviewURL        string
	SourceURL        string
	SourceLabel      string
	PullRequestURL   string
	PullRequestLabel string
	Title            string
	State            string
	ExternalID       string
	Metadata         string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type LibraryFilterOption struct {
	Value    string
	Label    string
	Count    int
	Selected bool
}

func libraryShellData(data LibraryData) DashboardShellData {
	activeNav := strings.TrimSpace(data.ActiveNav)
	if activeNav == "" {
		activeNav = "library"
	}
	return DashboardShellData{
		Title:            libraryPageTitle(data),
		ApplicationName:  data.ApplicationName,
		InstanceName:     data.InstanceName,
		Version:          data.Version,
		Build:            data.Build,
		ConnectorName:    data.ConnectorName,
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

func libraryPageTitle(data LibraryData) string {
	if strings.TrimSpace(data.Title) != "" {
		return data.Title
	}
	return "Detent Library"
}

func librarySummaryFigures(data LibraryData) []reportsKPI {
	return []reportsKPI{
		{ID: "library-total", Value: formatInt(int64(data.Summary.Total)), Label: "deliverables"},
		{ID: "library-projects", Value: formatInt(int64(data.Summary.Projects)), Label: "projects"},
		{ID: "library-artifacts", Value: formatInt(int64(data.Summary.Artifacts)), Label: "artifacts"},
		{ID: "library-prs", Value: formatInt(int64(data.Summary.PullRequests)), Label: "PR records"},
		{ID: "library-validated", Value: formatInt(int64(data.Summary.Validated)), Label: "validated"},
	}
}

func libraryOptionLabel(option LibraryFilterOption) string {
	label := strings.TrimSpace(option.Label)
	if label == "" {
		label = strings.TrimSpace(option.Value)
	}
	if label == "" {
		label = "All"
	}
	return label + " (" + formatInt(int64(option.Count)) + ")"
}

func libraryStatusLabel(status string) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return "unvalidated"
	}
	return status
}

func libraryStatusClass(status string) string {
	base := "inline-flex max-w-full items-center rounded-chip px-2 py-0.5 text-2xs font-medium "
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "approved", "complete", "completed", "pass", "passed", "valid":
		return base + "bg-ok/15 text-ok"
	case "pending", "review", "reviewing", "waiting":
		return base + "bg-warn/15 text-warn"
	case "changes_requested", "failed", "invalid", "rework":
		return base + "bg-err/15 text-err"
	case "":
		return base + "bg-elev text-dim"
	default:
		return base + "bg-accent/15 text-accent"
	}
}

func libraryKindLabel(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "artifact"
	}
	return strings.ReplaceAll(kind, "_", " ")
}

func libraryKindClass(kind string) string {
	base := "inline-flex max-w-full items-center rounded-chip px-2 py-0.5 text-2xs font-medium "
	if strings.EqualFold(strings.TrimSpace(kind), "pull_request") {
		return base + "bg-accent/15 text-accent"
	}
	return base + "bg-elev text-sec"
}

func libraryPathLabel(row LibraryRow) string {
	if path := strings.TrimSpace(row.ArtifactPath); path != "" {
		return path
	}
	if strings.EqualFold(strings.TrimSpace(row.SourceKind), "pull_request") {
		return strings.TrimSpace(row.PullRequestLabel)
	}
	return "n/a"
}

func librarySourceTitle(row LibraryRow) string {
	parts := []string{}
	if label := strings.TrimSpace(row.SourceLabel); label != "" {
		parts = append(parts, label)
	}
	if state := strings.TrimSpace(row.State); state != "" {
		parts = append(parts, state)
	}
	return strings.Join(parts, " · ")
}

func libraryRowTitle(row LibraryRow) string {
	if title := strings.TrimSpace(row.Title); title != "" {
		return title
	}
	return librarySourceTitle(row)
}

func libraryTimeLabel(value time.Time) string {
	if value.IsZero() {
		return "n/a"
	}
	return value.UTC().Format("2006-01-02 15:04")
}

func libraryDateInputValue(value string, fallback time.Time) string {
	value = strings.TrimSpace(value)
	if value != "" {
		return value
	}
	if fallback.IsZero() {
		return ""
	}
	return fallback.UTC().Format("2006-01-02")
}

func libraryResultsLabel(data LibraryData) string {
	if !data.HasActiveFilters {
		return formatInt(int64(data.FilteredCount))
	}
	return formatInt(int64(data.FilteredCount)) + " of " + formatInt(int64(data.UnfilteredCount))
}

func libraryHasSafeURL(value string) bool {
	return librarySafeURLValue(value) != ""
}

func librarySafeURL(value string) templ.SafeURL {
	return templ.SafeURL(librarySafeURLValue(value))
}

func librarySafeURLValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") {
		return value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return ""
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return value
	default:
		return ""
	}
}

func libraryRowDetail(row LibraryRow) string {
	parts := []string{}
	if externalID := strings.TrimSpace(row.ExternalID); externalID != "" {
		parts = append(parts, externalID)
	}
	if metadata := strings.TrimSpace(row.Metadata); metadata != "" {
		parts = append(parts, metadata)
	}
	return strings.Join(parts, " · ")
}
