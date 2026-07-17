package templates

import (
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/efficiency"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type ReportsData struct {
	Title            string
	ApplicationName  string
	InstanceName     string
	ConnectorName    string
	Snapshot         telemetry.Snapshot
	GeneratedAt      time.Time
	Day              UsageReportData
	Project          UsageReportData
	Issue            UsageReportData
	PR               UsageReportData
	Model            UsageReportData
	Digest           DailyDigestData
	Efficiency       efficiency.Rollup
	CostPerOutcome   efficiency.CostPerOutcomeReport
	OutcomeWindow    string
	Assets           AssetPaths
	Projects         []ProjectSmallMultiple
	ActiveNav        string
	ProjectID        string
	ProjectName      string
	SidebarCollapsed bool
	Theme            string
	Density          string
}

type DailyDigestData struct {
	Timezone string
	Days     []DailyDigestDayData
}

type DailyDigestDayData struct {
	Date                 string
	From                 time.Time
	To                   time.Time
	Sessions             int64
	InputTokens          int64
	CachedInputTokens    int64
	OutputTokens         int64
	TotalTokens          int64
	SpendUSD             float64
	OrphanResumed        int64
	OrphanFresh          int64
	CapacityOutages      int64
	CapacitySeconds      int64
	CapacityRecoveryMode string
	BreakerTrips         int64
	FailedSessions       int64
	DominantErrorClass   string
	IssuesFiled          int64
	IssuesShipped        int64
	ReleasesTagged       int64
	Efficiency           efficiency.RollupWindow
	Projects             []DailyDigestProjectData
}

type DailyDigestProjectData struct {
	ID       string
	Name     string
	Filed    int64
	Shipped  int64
	Releases int64
}

type UsageReportData struct {
	By         string
	From       string
	To         string
	Totals     UsageTotalsData
	Series     []UsageBucketData
	Breakdowns []UsageBucketData
}

type UsageTotalsData struct {
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
	ModelContextWindow    int64
	RuntimeSeconds        int64
	Events                int64
	SpendUSD              float64
	CacheReadFraction     float64
	Models                []UsageModelData
}

type UsageBucketData struct {
	Bucket                string
	Label                 string
	Date                  string
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
	ModelContextWindow    int64
	RuntimeSeconds        int64
	Events                int64
	SpendUSD              float64
	CacheReadFraction     float64
	Models                []UsageModelData
}

type UsageModelData struct {
	Model                 string
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
	ModelContextWindow    int64
	RuntimeSeconds        int64
	Events                int64
	SpendUSD              float64
	CacheReadFraction     float64
}

func reportsPageTitle(data ReportsData) string {
	if strings.TrimSpace(data.Title) != "" {
		return data.Title
	}
	return "Detent reports"
}

func reportsDashboardShellData(data ReportsData) DashboardShellData {
	activeNav := strings.TrimSpace(data.ActiveNav)
	if activeNav == "" {
		activeNav = "reports"
	}
	return DashboardShellData{
		Title:            reportsPageTitle(data),
		ApplicationName:  data.ApplicationName,
		InstanceName:     data.InstanceName,
		ConnectorName:    data.ConnectorName,
		Snapshot:         data.Snapshot,
		Projects:         data.Projects,
		Assets:           data.Assets,
		ActiveNav:        activeNav,
		ProjectID:        data.ProjectID,
		ProjectName:      data.ProjectName,
		SidebarCollapsed: data.SidebarCollapsed,
		Theme:            data.Theme,
		Density:          data.Density,
	}
}

func reportsHasUsage(data ReportsData) bool {
	return data.Day.Totals.Events > 0 || data.Day.Totals.TotalTokens > 0 || data.Day.Totals.SpendUSD > 0 || data.Efficiency.Current.Issues > 0 || len(data.CostPerOutcome.Projects) > 0
}

func reportBucketLabel(row UsageBucketData) string {
	if strings.TrimSpace(row.Label) != "" {
		return row.Label
	}
	if strings.TrimSpace(row.Bucket) != "" {
		return row.Bucket
	}
	return "unassigned"
}

func reportCacheReadFraction(fraction float64) string {
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	return formatDecimal(fraction*100) + "%"
}
