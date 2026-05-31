package templates

import (
	"slices"
	"strings"
	"time"

	webchart "github.com/digitaldrywood/symphony/internal/web/chart"
)

const reportTopLimit = 5

type ReportsData struct {
	Title         string
	ConnectorName string
	GeneratedAt   time.Time
	Day           UsageReportData
	Project       UsageReportData
	Issue         UsageReportData
	PR            UsageReportData
	Model         UsageReportData
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
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	RuntimeSeconds int64
	Events         int64
	SpendUSD       float64
	Models         []UsageModelData
}

type UsageBucketData struct {
	Bucket         string
	Label          string
	Date           string
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	RuntimeSeconds int64
	Events         int64
	SpendUSD       float64
	Models         []UsageModelData
}

type UsageModelData struct {
	Model          string
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	RuntimeSeconds int64
	Events         int64
	SpendUSD       float64
}

func reportsPageTitle(data ReportsData) string {
	if strings.TrimSpace(data.Title) != "" {
		return data.Title
	}
	return "Symphony reports"
}

func reportsConnectorName(data ReportsData) string {
	if strings.TrimSpace(data.ConnectorName) != "" {
		return data.ConnectorName
	}
	return "unknown"
}

func reportsGeneratedAtLabel(data ReportsData) string {
	if data.GeneratedAt.IsZero() {
		return "Generated pending"
	}
	return "Generated " + data.GeneratedAt.UTC().Format("Jan 2 15:04:05 UTC")
}

func reportsWindowLabel(data ReportsData) string {
	from := strings.TrimSpace(data.Day.From)
	to := strings.TrimSpace(data.Day.To)
	switch {
	case from != "" && to != "":
		return from + " to " + to
	case from != "":
		return "Since " + from
	case to != "":
		return "Through " + to
	default:
		return "All time"
	}
}

func reportsHasUsage(data ReportsData) bool {
	return data.Day.Totals.Events > 0 || data.Day.Totals.TotalTokens > 0 || data.Day.Totals.SpendUSD > 0
}

func reportsModelCount(data ReportsData) string {
	return formatInt(int64(len(data.Model.Breakdowns)))
}

func reportsSpendTrendChart(data ReportsData) SeriesChartData {
	return SeriesChartData{
		Title:       "Spend trend",
		AriaLabel:   "Spend over time",
		Points:      reportSpendPoints(data.Day.Series),
		ValueSuffix: "USD",
		ColorClass:  "text-accent",
		Height:      128,
	}
}

func reportsTokenTrendChart(data ReportsData) SplitSeriesChartData {
	points := make([]SplitSeriesPoint, 0, len(data.Day.Series))
	for _, row := range data.Day.Series {
		points = append(points, SplitSeriesPoint{
			Label:  reportBucketLabel(row),
			Input:  float64(row.InputTokens),
			Output: float64(row.OutputTokens),
		})
	}
	return SplitSeriesChartData{
		Title:       "Token trend",
		AriaLabel:   "Token trend over time",
		InputLabel:  "Input",
		OutputLabel: "Output",
		Points:      points,
		ValueSuffix: "tokens",
		Height:      128,
	}
}

func reportsProjectChart(data ReportsData) BarChartData {
	return reportBarChart("Per-project breakdown", "Project tokens", topReportBuckets(data.Project.Breakdowns, 0), "text-accent")
}

func reportsIssueChart(data ReportsData) BarChartData {
	return reportBarChart("Top issues by tokens", "Issue tokens", topReportBuckets(data.Issue.Breakdowns, reportTopLimit), "text-success")
}

func reportsPRChart(data ReportsData) BarChartData {
	return reportBarChart("Top PRs by tokens", "PR tokens", topReportBuckets(data.PR.Breakdowns, reportTopLimit), "text-warning")
}

func reportsModelSplitChart(data ReportsData) TimelineChartData {
	segments := make([]TimelineSegment, 0, len(data.Model.Breakdowns))
	for index, row := range topReportBuckets(data.Model.Breakdowns, reportTopLimit) {
		segments = append(segments, TimelineSegment{
			Label: reportBucketLabel(row),
			Value: float64(row.TotalTokens),
			Class: reportColorClass(index),
		})
	}
	return TimelineChartData{
		Title:       "Model split",
		AriaLabel:   "Model token split",
		Segments:    segments,
		ValueSuffix: "tokens",
	}
}

func topReportBuckets(rows []UsageBucketData, limit int) []UsageBucketData {
	sorted := append([]UsageBucketData(nil), rows...)
	slices.SortFunc(sorted, func(a UsageBucketData, b UsageBucketData) int {
		if a.TotalTokens > b.TotalTokens {
			return -1
		}
		if a.TotalTokens < b.TotalTokens {
			return 1
		}
		return strings.Compare(reportBucketLabel(a), reportBucketLabel(b))
	})
	if limit > 0 && len(sorted) > limit {
		return sorted[:limit]
	}
	return sorted
}

func reportSpendPoints(rows []UsageBucketData) []webchart.Point {
	points := make([]webchart.Point, 0, len(rows))
	for _, row := range rows {
		points = append(points, webchart.Point{
			Label: reportBucketLabel(row),
			Value: row.SpendUSD,
		})
	}
	return points
}

func reportBarChart(title string, ariaLabel string, rows []UsageBucketData, colorClass string) BarChartData {
	points := make([]webchart.Point, 0, len(rows))
	for _, row := range rows {
		points = append(points, webchart.Point{
			Label: reportBucketLabel(row),
			Value: float64(row.TotalTokens),
		})
	}
	return BarChartData{
		Title:       title,
		AriaLabel:   ariaLabel,
		Bars:        points,
		ValueSuffix: "tokens",
		ColorClass:  colorClass,
	}
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

func reportInputOutputLabel(row UsageBucketData) string {
	return "In " + formatInt(row.InputTokens) + " / Out " + formatInt(row.OutputTokens)
}

func reportInputOutputTotals(totals UsageTotalsData) string {
	return "In " + formatInt(totals.InputTokens) + " / Out " + formatInt(totals.OutputTokens)
}

func reportTokenShareStyle(row UsageBucketData, total int64) string {
	if total <= 0 || row.TotalTokens <= 0 {
		return percentStyle(0)
	}
	return percentStyle(int(float64(row.TotalTokens) / float64(total) * 100))
}

func reportTokenShareLabel(row UsageBucketData, total int64) string {
	if total <= 0 || row.TotalTokens <= 0 {
		return "0%"
	}
	return formatDecimal(float64(row.TotalTokens)/float64(total)*100) + "%"
}

func reportModelRows(data ReportsData) []UsageBucketData {
	return topReportBuckets(data.Model.Breakdowns, reportTopLimit)
}

func reportColorClass(index int) string {
	classes := []string{"text-accent", "text-success", "text-warning", "text-danger"}
	return classes[index%len(classes)]
}
