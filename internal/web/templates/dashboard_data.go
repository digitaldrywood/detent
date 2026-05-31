package templates

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/symphony/internal/telemetry"
	webchart "github.com/digitaldrywood/symphony/internal/web/chart"
)

type DashboardData struct {
	Title          string
	Version        string
	DashboardURL   string
	ConnectorName  string
	Snapshot       telemetry.Snapshot
	TokenSparkline []TokenSparklinePoint
}

type TokenSparklinePoint struct {
	Label string
	Value int64
}

type Budget = telemetry.Budget

type RateLimits = telemetry.RateLimits

type rateLimitRow struct {
	Name        string
	Remaining   string
	Used        string
	Limit       string
	Reset       string
	UsedPercent int
}

func pageTitle(data DashboardData) string {
	if data.Title != "" {
		return data.Title
	}
	return "Symphony"
}

func versionLabel(data DashboardData) string {
	version := strings.TrimSpace(data.Version)
	if version == "" {
		return "dev"
	}
	return version
}

func dashboardURL(data DashboardData) string {
	url := strings.TrimSpace(data.DashboardURL)
	if url == "" {
		return "http://localhost:4000"
	}
	return url
}

func dashboardURLLabel(data DashboardData) string {
	url := strings.TrimSpace(data.DashboardURL)
	if url == "" {
		return "http://localhost:4000"
	}
	return url
}

func connectorName(data DashboardData) string {
	if data.ConnectorName != "" {
		return data.ConnectorName
	}
	return "unknown"
}

func runningCount(snapshot telemetry.Snapshot) int {
	if snapshot.Counts.Running != 0 || len(snapshot.Running) == 0 {
		return snapshot.Counts.Running
	}
	return len(snapshot.Running)
}

func queueCount(snapshot telemetry.Snapshot) int {
	if snapshot.Counts.Queue != 0 || len(snapshot.Queue) == 0 {
		return snapshot.Counts.Queue
	}
	return len(snapshot.Queue)
}

func blockedCount(snapshot telemetry.Snapshot) int {
	if snapshot.Counts.Blocked != 0 || len(snapshot.Blocked) == 0 {
		return snapshot.Counts.Blocked
	}
	return len(snapshot.Blocked)
}

func completedCount(snapshot telemetry.Snapshot) int {
	if snapshot.Counts.Completed != 0 || len(snapshot.Completed) == 0 {
		return snapshot.Counts.Completed
	}
	return len(snapshot.Completed)
}

func generatedAtLabel(snapshot telemetry.Snapshot) string {
	if snapshot.GeneratedAt.IsZero() {
		return "Snapshot pending"
	}
	return "Updated " + snapshot.GeneratedAt.UTC().Format("Jan 2 15:04:05 UTC")
}

func issueIdentifier(issue telemetry.Issue) string {
	if issue.Identifier != "" {
		return issue.Identifier
	}
	if issue.ID != "" {
		return issue.ID
	}
	return "unknown"
}

func issueTitle(issue telemetry.Issue) string {
	if issue.Title != "" {
		return issue.Title
	}
	return "Untitled issue"
}

func issueState(issue telemetry.Issue, fallback string) string {
	if issue.State != "" {
		return issue.State
	}
	return fallback
}

func sessionLabel(sessionID string) string {
	if sessionID == "" {
		return "n/a"
	}
	if len(sessionID) <= 18 {
		return sessionID
	}
	return sessionID[:10] + "..." + sessionID[len(sessionID)-5:]
}

func runningRuntime(row telemetry.Running, generatedAt time.Time) string {
	seconds := row.RuntimeSeconds
	if seconds <= 0 && !row.StartedAt.IsZero() && !generatedAt.IsZero() {
		seconds = generatedAt.Sub(row.StartedAt).Seconds()
	}
	return formatDuration(seconds) + " / " + formatInt(int64(row.TurnCount)) + " turns"
}

func lastCodexUpdate(row telemetry.Running) string {
	if row.LastMessage != "" {
		return row.LastMessage
	}
	if row.LastEvent != "" {
		return row.LastEvent
	}
	return "No Codex update yet."
}

func lastCodexMeta(row telemetry.Running) string {
	if row.LastEvent == "" && row.LastEventAt == nil {
		return "n/a"
	}
	parts := make([]string, 0, 2)
	if row.LastEvent != "" {
		parts = append(parts, row.LastEvent)
	}
	if row.LastEventAt != nil {
		parts = append(parts, row.LastEventAt.UTC().Format("15:04:05 UTC"))
	}
	return strings.Join(parts, " / ")
}

func formatDiffStat(row telemetry.Running) string {
	if row.DiffStatus == "ok" {
		return "+" + formatInt(int64(row.DiffAdded)) + " -" + formatInt(int64(row.DiffRemoved)) + " (" + formatInt(int64(row.DiffFiles)) + " files)"
	}
	if row.DiffStatus != "" {
		return row.DiffStatus
	}
	return "pending"
}

func formatCount(value int) string {
	return formatInt(int64(value))
}

func formatTokens(tokens telemetry.Tokens) string {
	return formatInt(tokens.Total)
}

func formatTokenBreakdown(tokens telemetry.Tokens) string {
	return "In " + formatInt(tokens.Input) + " / Out " + formatInt(tokens.Output)
}

func formatUSD(value float64) string {
	return fmt.Sprintf("$%.2f", value)
}

func optionalUSD(value *float64) string {
	if value == nil {
		return "off"
	}
	return formatUSD(*value)
}

func budgetStatus(budget telemetry.Budget) string {
	if budget.Enabled {
		return "Budget enabled"
	}
	return "Budget disabled"
}

func rateLimitRows(limits *telemetry.RateLimits) []rateLimitRow {
	if limits == nil {
		return nil
	}

	rows := make([]rateLimitRow, 0, 3)
	appendBucket := func(name string, bucket *telemetry.RateLimitBucket) {
		if bucket == nil {
			return
		}
		rows = append(rows, rateLimitRow{
			Name:        name,
			Remaining:   formatInt(bucket.Remaining) + " left",
			Used:        formatInt(bucket.Used) + " used",
			Limit:       formatLimit(bucket.Limit) + " limit",
			Reset:       resetLabel(bucket),
			UsedPercent: usedPercent(bucket),
		})
	}

	appendBucket("Primary", limits.Primary)
	appendBucket("Secondary", limits.Secondary)
	if limits.Credits != nil {
		rows = append(rows, creditRateLimitRow(limits.Credits))
	}
	return rows
}

func creditRateLimitRow(bucket *telemetry.RateLimitBucket) rateLimitRow {
	row := rateLimitRow{
		Name:        "Credits",
		Remaining:   formatInt(bucket.Remaining) + " left",
		Used:        formatInt(bucket.Used) + " used",
		Limit:       formatLimit(bucket.Limit) + " limit",
		Reset:       resetLabel(bucket),
		UsedPercent: usedPercent(bucket),
	}

	switch {
	case bucket.Unlimited:
		row.Remaining = "unlimited credits"
		row.Used = "available"
		row.Limit = "n/a limit"
		row.UsedPercent = 0
	case bucket.HasCredits && strings.TrimSpace(bucket.Balance) != "":
		row.Remaining = strings.TrimSpace(bucket.Balance) + " credits"
		row.Used = "available"
		row.Limit = "n/a limit"
		row.UsedPercent = 0
	case bucket.HasCredits:
		row.Remaining = "available credits"
		row.Used = "available"
		row.Limit = "n/a limit"
		row.UsedPercent = 0
	case bucket.Limit == 0 && bucket.Remaining == 0 && bucket.Used == 0:
		row.Remaining = "no credits"
		row.Used = "unavailable"
		row.Limit = "n/a limit"
		row.UsedPercent = 0
	}

	return row
}

func rateLimitName(limits *telemetry.RateLimits) string {
	if limits == nil || limits.LimitName == "" {
		return "Latest snapshot"
	}
	return limits.LimitName
}

func percentStyle(percent int) string {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	return fmt.Sprintf("width: %d%%;", percent)
}

func tokenSparklineChart(data DashboardData) SeriesChartData {
	points := tokenSparklinePoints(data)
	chartPoints := make([]webchart.Point, 0, len(points))
	for _, point := range points {
		chartPoints = append(chartPoints, webchart.Point{
			Label: point.Label,
			Value: float64(point.Value),
		})
	}
	return SeriesChartData{
		Title:       "Token sparkline",
		AriaLabel:   "Token sparkline",
		Points:      chartPoints,
		ValueSuffix: "tokens",
	}
}

func tokenRate(snapshot telemetry.Snapshot) string {
	if snapshot.Tokens.Total <= 0 || snapshot.Tokens.RuntimeSeconds <= 0 {
		return "n/a"
	}
	perMinute := int64(math.Round(float64(snapshot.Tokens.Total) / snapshot.Tokens.RuntimeSeconds * 60))
	return formatInt(perMinute) + " tokens/min"
}

func formatDuration(seconds float64) string {
	if seconds <= 0 {
		return "0s"
	}

	duration := time.Duration(math.Round(seconds)) * time.Second
	hours := int(duration / time.Hour)
	duration -= time.Duration(hours) * time.Hour
	minutes := int(duration / time.Minute)
	duration -= time.Duration(minutes) * time.Minute
	secs := int(duration / time.Second)

	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, secs)
	}
	return fmt.Sprintf("%ds", secs)
}

func formatInt(value int64) string {
	sign := ""
	if value < 0 {
		sign = "-"
		value = -value
	}

	raw := strconv.FormatInt(value, 10)
	if len(raw) <= 3 {
		return sign + raw
	}

	first := len(raw) % 3
	if first == 0 {
		first = 3
	}

	var out strings.Builder
	out.Grow(len(sign) + len(raw) + (len(raw)-1)/3)
	out.WriteString(sign)
	out.WriteString(raw[:first])
	for i := first; i < len(raw); i += 3 {
		out.WriteByte(',')
		out.WriteString(raw[i : i+3])
	}
	return out.String()
}

func formatLimit(value int64) string {
	if value <= 0 {
		return "n/a"
	}
	return formatInt(value)
}

func resetLabel(bucket *telemetry.RateLimitBucket) string {
	if bucket.ResetAt != nil {
		return bucket.ResetAt.UTC().Format("15:04 UTC")
	}
	if bucket.ResetInSeconds > 0 {
		return formatDuration(float64(bucket.ResetInSeconds))
	}
	return "n/a"
}

func usedPercent(bucket *telemetry.RateLimitBucket) int {
	if bucket.Limit > 0 {
		return int(math.Round(float64(bucket.Used) / float64(bucket.Limit) * 100))
	}
	total := bucket.Used + bucket.Remaining
	if total > 0 {
		return int(math.Round(float64(bucket.Used) / float64(total) * 100))
	}
	return 0
}

func tokenSparklinePoints(data DashboardData) []TokenSparklinePoint {
	if len(data.TokenSparkline) > 0 {
		points := make([]TokenSparklinePoint, 0, len(data.TokenSparkline))
		for i, point := range data.TokenSparkline {
			if point.Label == "" {
				point.Label = "Point " + strconv.Itoa(i+1)
			}
			points = append(points, point)
		}
		return points
	}

	if len(data.Snapshot.Running) > 0 {
		points := make([]TokenSparklinePoint, 0, len(data.Snapshot.Running))
		for _, row := range data.Snapshot.Running {
			if row.Tokens.Total <= 0 {
				continue
			}
			points = append(points, TokenSparklinePoint{
				Label: issueIdentifier(row.Issue),
				Value: row.Tokens.Total,
			})
		}
		if len(points) > 0 {
			return points
		}
	}

	if data.Snapshot.Tokens.Total > 0 {
		return []TokenSparklinePoint{
			{Label: "Input", Value: data.Snapshot.Tokens.Input},
			{Label: "Output", Value: data.Snapshot.Tokens.Output},
			{Label: "Total", Value: data.Snapshot.Tokens.Total},
		}
	}

	return nil
}
