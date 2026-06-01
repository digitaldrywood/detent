package templates

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	webchart "github.com/digitaldrywood/detent/internal/web/chart"
)

const (
	throughputTrendWindow   = 10 * time.Minute
	defaultThroughputWindow = time.Minute
)

type DashboardData struct {
	Title         string
	Version       string
	DashboardURL  string
	ConnectorName string
	Snapshot      telemetry.Snapshot
	Assets        AssetPaths
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

type boardStateRow struct {
	State      string
	Count      int
	CountLabel string
	Percent    string
	DotClass   string
}

type budgetHistoryBar struct {
	Style string
	Title string
}

type budgetBurnDownViewModel struct {
	Available       bool
	EmptyTitle      string
	EmptyDetail     string
	PeriodLabel     string
	CurrentLabel    string
	CapLabel        string
	ProjectionLabel string
	Chart           BudgetProjectionChartData
}

type agentTimelineRow struct {
	Identifier   string
	Title        string
	State        string
	StartedAt    string
	EndedAt      string
	Duration     string
	StartPercent string
	EndPercent   string
	Segments     []agentTimelineSegment
}

type agentTimelineSegment struct {
	Label string
	Class string
	Style string
	Title string
	Width string
}

type agentTimelineEntry struct {
	issue   telemetry.Issue
	state   string
	start   time.Time
	end     time.Time
	running bool
}

type runningActivityRow struct {
	At      string
	Event   string
	Message string
}

type prPipelineLane struct {
	ID          string
	Title       string
	CountLabel  string
	DotClass    string
	EmptyTitle  string
	EmptyDetail string
	Cards       []prPipelineCard
}

type prPipelineCard struct {
	IssueNumber      string
	Identifier       string
	Title            string
	URL              string
	CIStatus         string
	CIClass          string
	CodexReviewState string
	CodexReviewClass string
	TimeInStage      string
	TimeInStageTitle string
	Stage            string
}

func pageTitle(data DashboardData) string {
	if data.Title != "" {
		return data.Title
	}
	return "Detent"
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

func issueDescriptionPreview(issue telemetry.Issue) string {
	description := strings.Join(strings.Fields(issue.Description), " ")
	if description == "" {
		return ""
	}

	const limit = 180
	runes := []rune(description)
	if len(runes) <= limit {
		return description
	}
	return string(runes[:limit-3]) + "..."
}

func issueDetailURL(issue telemetry.Issue) string {
	identifier := issueIdentifier(issue)
	if identifier == "" || identifier == "unknown" {
		return ""
	}
	return "/api/v1/" + url.PathEscape(identifier)
}

func issuePopoverID(prefix string, index int) string {
	return prefix + "-issue-popover-" + strconv.Itoa(index)
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
	return formatDuration(runningRuntimeSeconds(row, generatedAt)) + " / " + formatInt(int64(row.TurnCount)) + " turns"
}

func runningRuntimeSeconds(row telemetry.Running, generatedAt time.Time) float64 {
	seconds := row.RuntimeSeconds
	if seconds <= 0 && !row.StartedAt.IsZero() && !generatedAt.IsZero() {
		seconds = generatedAt.Sub(row.StartedAt).Seconds()
	}
	return seconds
}

func runningRuntimeOnly(row telemetry.Running, generatedAt time.Time) string {
	return formatDuration(runningRuntimeSeconds(row, generatedAt))
}

func runningTurnLabel(row telemetry.Running) string {
	if row.TurnCount <= 0 {
		return "Turn n/a"
	}
	return "Turn " + formatInt(int64(row.TurnCount))
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

func runningActivityID(prefix string, index int) string {
	return prefix + "-activity-" + strconv.Itoa(index)
}

func runningActivityRows(row telemetry.Running) []runningActivityRow {
	events := row.RecentEvents
	if len(events) == 0 && row.LastEventAt != nil {
		events = []telemetry.ActivityEvent{
			{
				At:      *row.LastEventAt,
				Event:   row.LastEvent,
				Message: lastCodexUpdate(row),
			},
		}
	}
	if len(events) == 0 {
		return nil
	}

	start := 0
	if len(events) > 5 {
		start = len(events) - 5
	}
	rows := make([]runningActivityRow, 0, len(events)-start)
	for i := len(events) - 1; i >= start; i-- {
		event := events[i]
		rows = append(rows, runningActivityRow{
			At:      activityTimeLabel(event.At),
			Event:   activityValue(event.Event, "event"),
			Message: activityValue(event.Message, "No message recorded."),
		})
	}
	return rows
}

func activityTimeLabel(at time.Time) string {
	if at.IsZero() {
		return "n/a"
	}
	return at.UTC().Format("15:04:05 UTC")
}

func activityValue(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func prPipelineLanes(snapshot telemetry.Snapshot) []prPipelineLane {
	cardsByLane := map[string][]prPipelineCard{
		"human-review": {},
		"merging":      {},
		"done-today":   {},
	}
	seen := map[string]struct{}{}
	now := pipelineNow(snapshot)

	for _, issue := range snapshot.Pipeline {
		appendPRPipelineCard(cardsByLane, seen, issue, issue.State, pipelineIssueStageTime(issue), now)
	}
	for _, row := range snapshot.Running {
		appendPRPipelineCard(cardsByLane, seen, row.Issue, issueState(row.Issue, "Running"), row.StartedAt, now)
	}
	for _, row := range snapshot.Queue {
		stageAt := time.Time{}
		if row.DueAt != nil {
			stageAt = *row.DueAt
		}
		appendPRPipelineCard(cardsByLane, seen, row.Issue, issueState(row.Issue, "Todo"), stageAt, now)
	}
	for _, row := range snapshot.Blocked {
		stageAt := time.Time{}
		if row.BlockedAt != nil {
			stageAt = *row.BlockedAt
		}
		appendPRPipelineCard(cardsByLane, seen, row.Issue, issueState(row.Issue, "Blocked"), stageAt, now)
	}
	for _, row := range snapshot.Completed {
		appendPRPipelineCard(cardsByLane, seen, row.Issue, completedState(row), row.CompletedAt, now)
	}

	return []prPipelineLane{
		{
			ID:          "human-review",
			Title:       "Human Review",
			CountLabel:  formatCount(len(cardsByLane["human-review"])),
			DotClass:    "bg-success",
			EmptyTitle:  "No PRs waiting for review.",
			EmptyDetail: "Ready pull requests will appear here after Detent hands them to reviewers.",
			Cards:       cardsByLane["human-review"],
		},
		{
			ID:          "merging",
			Title:       "Merging",
			CountLabel:  formatCount(len(cardsByLane["merging"])),
			DotClass:    "bg-accent",
			EmptyTitle:  "Nothing is merging.",
			EmptyDetail: "Approved pull requests enter this lane while the final integration run is active.",
			Cards:       cardsByLane["merging"],
		},
		{
			ID:          "done-today",
			Title:       "Done today",
			CountLabel:  formatCount(len(cardsByLane["done-today"])),
			DotClass:    "bg-muted-foreground",
			EmptyTitle:  "No PRs finished today.",
			EmptyDetail: "Merged pull requests land here for the current UTC day.",
			Cards:       cardsByLane["done-today"],
		},
	}
}

func prPipelineTotalLabel(snapshot telemetry.Snapshot) string {
	total := 0
	for _, lane := range prPipelineLanes(snapshot) {
		total += len(lane.Cards)
	}
	return formatCount(total)
}

func appendPRPipelineCard(
	cardsByLane map[string][]prPipelineCard,
	seen map[string]struct{},
	issue telemetry.Issue,
	state string,
	stageAt time.Time,
	now time.Time,
) {
	laneID := prPipelineLaneID(state)
	if laneID == "" {
		return
	}
	if laneID == "done-today" && !pipelineSameUTCDay(stageAt, now) {
		return
	}

	key := laneID + ":" + issueIdentifier(issue)
	if issue.ID != "" {
		key = laneID + ":" + issue.ID
	}
	if _, ok := seen[key]; ok {
		return
	}
	seen[key] = struct{}{}

	cardsByLane[laneID] = append(cardsByLane[laneID], prPipelineCardForIssue(issue, state, laneID, stageAt, now))
}

func prPipelineLaneID(state string) string {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(state), " ", "")) {
	case "humanreview", "review", "inreview":
		return "human-review"
	case "merging":
		return "merging"
	case "done", "complete", "completed", "closed", "cancelled", "canceled":
		return "done-today"
	default:
		return ""
	}
}

func prPipelineCardForIssue(issue telemetry.Issue, state string, laneID string, stageAt time.Time, now time.Time) prPipelineCard {
	ciStatus := prPipelineCIStatus(issue, laneID)
	codexReview := prPipelineCodexReviewState(issue)
	return prPipelineCard{
		IssueNumber:      issueNumber(issue),
		Identifier:       issueIdentifier(issue),
		Title:            issueTitle(issue),
		URL:              prPipelineURL(issue),
		CIStatus:         ciStatus,
		CIClass:          prPipelineCIClass(ciStatus),
		CodexReviewState: codexReview,
		CodexReviewClass: prPipelineCodexReviewClass(codexReview),
		TimeInStage:      prPipelineAge(stageAt, now),
		TimeInStageTitle: prPipelineAgeTitle(state, stageAt, now),
		Stage:            chartText(state, "n/a"),
	}
}

func pipelineNow(snapshot telemetry.Snapshot) time.Time {
	if !snapshot.GeneratedAt.IsZero() {
		return snapshot.GeneratedAt.UTC()
	}
	latest := time.Time{}
	for _, issue := range snapshot.Pipeline {
		if issue.UpdatedAt != nil && issue.UpdatedAt.After(latest) {
			latest = *issue.UpdatedAt
		}
	}
	for _, row := range snapshot.Running {
		if row.StartedAt.After(latest) {
			latest = row.StartedAt
		}
	}
	for _, row := range snapshot.Completed {
		if row.CompletedAt.After(latest) {
			latest = row.CompletedAt
		}
	}
	return latest.UTC()
}

func pipelineIssueStageTime(issue telemetry.Issue) time.Time {
	if issue.UpdatedAt != nil && !issue.UpdatedAt.IsZero() {
		return issue.UpdatedAt.UTC()
	}
	if issue.CreatedAt != nil && !issue.CreatedAt.IsZero() {
		return issue.CreatedAt.UTC()
	}
	return time.Time{}
}

func pipelineSameUTCDay(stageAt time.Time, now time.Time) bool {
	if stageAt.IsZero() || now.IsZero() {
		return true
	}
	stageAt = stageAt.UTC()
	now = now.UTC()
	return stageAt.Year() == now.Year() && stageAt.YearDay() == now.YearDay()
}

func prPipelineCIStatus(issue telemetry.Issue, laneID string) string {
	if issue.PullRequest != nil {
		switch strings.ToLower(strings.TrimSpace(issue.PullRequest.CIStatus)) {
		case "pass", "passed", "success", "green":
			return "pass"
		case "fail", "failed", "failure", "error", "red":
			return "fail"
		case "pending", "expected", "queued", "waiting", "in_progress", "in progress":
			return "pending"
		}
		if strings.EqualFold(issue.PullRequest.State, "MERGED") {
			return "pass"
		}
	}
	if laneID == "done-today" {
		return "pass"
	}
	return "pending"
}

func prPipelineCodexReviewState(issue telemetry.Issue) string {
	if issue.PullRequest != nil {
		switch strings.ToUpper(strings.TrimSpace(issue.PullRequest.CodexReviewState)) {
		case "P1":
			return "P1"
		case "P2":
			return "P2"
		case "CLEAN":
			return "clean"
		}
	}
	for _, label := range issue.Labels {
		switch strings.ToUpper(strings.TrimSpace(label)) {
		case "P1", "CODEX:P1", "CODEX-REVIEW:P1":
			return "P1"
		case "P2", "CODEX:P2", "CODEX-REVIEW:P2":
			return "P2"
		}
	}
	return "clean"
}

func prPipelineCIClass(status string) string {
	switch status {
	case "pass":
		return "border-success-soft bg-success-soft text-success"
	case "fail":
		return "border-danger-soft bg-danger-soft text-danger"
	default:
		return "border-warning-soft bg-warning-soft text-warning"
	}
}

func prPipelineCodexReviewClass(state string) string {
	switch state {
	case "P1":
		return "border-danger-soft bg-danger-soft text-danger"
	case "P2":
		return "border-warning-soft bg-warning-soft text-warning"
	default:
		return "border-success-soft bg-success-soft text-success"
	}
}

func prPipelineAge(stageAt time.Time, now time.Time) string {
	if stageAt.IsZero() || now.IsZero() {
		return "n/a"
	}
	if now.Before(stageAt) {
		return "0s"
	}
	return formatDuration(now.Sub(stageAt).Seconds())
}

func prPipelineAgeTitle(state string, stageAt time.Time, now time.Time) string {
	if stageAt.IsZero() {
		return "Stage start is unavailable."
	}
	return chartText(state, "Stage") + " since " + timeLabel(stageAt) + " (" + prPipelineAge(stageAt, now) + ")"
}

func issueNumber(issue telemetry.Issue) string {
	if issue.PullRequest != nil && issue.PullRequest.Number > 0 {
		return "#" + strconv.Itoa(issue.PullRequest.Number)
	}
	identifier := issueIdentifier(issue)
	index := strings.LastIndex(identifier, "#")
	if index >= 0 && index < len(identifier)-1 {
		return identifier[index:]
	}
	return identifier
}

func prPipelineURL(issue telemetry.Issue) string {
	if issue.PullRequest != nil && strings.TrimSpace(issue.PullRequest.URL) != "" {
		return strings.TrimSpace(issue.PullRequest.URL)
	}
	return issue.URL
}

func queuedDueLabel(row telemetry.Queued) string {
	if row.DueAt != nil {
		return timeLabel(*row.DueAt)
	}
	if row.DueInMillis > 0 {
		return "in " + formatDuration(float64(row.DueInMillis)/1000)
	}
	return "n/a"
}

func rowError(value string) string {
	if strings.TrimSpace(value) == "" {
		return "n/a"
	}
	return value
}

func blockedAtLabel(row telemetry.Blocked) string {
	if row.BlockedAt == nil {
		return "n/a"
	}
	return timeLabel(*row.BlockedAt)
}

func blockedLastUpdate(row telemetry.Blocked) string {
	if row.LastMessage != "" {
		return row.LastMessage
	}
	if row.LastEvent != "" {
		return row.LastEvent
	}
	return "n/a"
}

func blockedLastUpdateMeta(row telemetry.Blocked) string {
	if row.LastEvent == "" && row.LastEventAt == nil {
		return "n/a"
	}
	parts := make([]string, 0, 2)
	if row.LastEvent != "" {
		parts = append(parts, row.LastEvent)
	}
	if row.LastEventAt != nil {
		parts = append(parts, timeLabel(*row.LastEventAt))
	}
	return strings.Join(parts, " / ")
}

func completedAtLabel(row telemetry.Completed) string {
	if row.CompletedAt.IsZero() {
		return "n/a"
	}
	return timeLabel(row.CompletedAt)
}

func completedRuntime(row telemetry.Completed) string {
	return formatDuration(row.RuntimeSeconds) + " / " + formatInt(int64(row.Turns)) + " turns"
}

func completedState(row telemetry.Completed) string {
	if strings.TrimSpace(row.FinalState) == "" {
		return "completed"
	}
	return row.FinalState
}

func boardStateRows(snapshot telemetry.Snapshot) []boardStateRow {
	counts := telemetry.BoardStateCounts(snapshot)
	total := boardStateTotal(counts)
	rows := make([]boardStateRow, 0, len(counts))
	for _, count := range counts {
		percent := "0%"
		if total > 0 {
			percent = fmt.Sprintf("%.0f%%", float64(count.Count)/float64(total)*100)
		}
		rows = append(rows, boardStateRow{
			State:      count.State,
			Count:      count.Count,
			CountLabel: formatCount(count.Count),
			Percent:    percent,
			DotClass:   boardStateDotClass(count.State),
		})
	}
	return rows
}

func boardStateTotal(counts []telemetry.BoardStateCount) int {
	total := 0
	for _, count := range counts {
		total += count.Count
	}
	return total
}

func boardStateTotalLabel(snapshot telemetry.Snapshot) string {
	return formatCount(boardStateTotal(telemetry.BoardStateCounts(snapshot)))
}

func boardDistributionChart(snapshot telemetry.Snapshot) TimelineChartData {
	counts := telemetry.BoardStateCounts(snapshot)
	segments := make([]TimelineSegment, 0, len(counts))
	for _, count := range counts {
		segments = append(segments, TimelineSegment{
			Label: count.State,
			Value: float64(count.Count),
			Class: boardStateTextClass(count.State),
		})
	}
	return TimelineChartData{
		Title:       "Board state distribution",
		AriaLabel:   "Board state distribution",
		Segments:    segments,
		ValueSuffix: "issues",
		Class:       "h-9",
		Height:      36,
	}
}

func boardProgressChart(snapshot telemetry.Snapshot) SeriesChartData {
	points := telemetry.BoardProgressPoints(snapshot)
	chartPoints := make([]webchart.Point, 0, len(points))
	for _, point := range points {
		chartPoints = append(chartPoints, webchart.Point{
			Label: point.Label,
			Value: float64(point.Count),
		})
	}
	return SeriesChartData{
		Title:       "Board cumulative flow",
		AriaLabel:   "Board cumulative flow",
		Points:      chartPoints,
		ValueSuffix: "issues",
		ColorClass:  "text-success",
	}
}

func boardProgressCount(snapshot telemetry.Snapshot) string {
	points := telemetry.BoardProgressPoints(snapshot)
	if len(points) == 0 {
		return "0"
	}
	return formatCount(points[len(points)-1].Count)
}

func boardStateDotClass(state string) string {
	switch normalizeTimelineState(state) {
	case "todo", "rework":
		return "bg-warning"
	case "review", "done":
		return "bg-success"
	case "blocked":
		return "bg-danger"
	case "backlog":
		return "bg-muted-foreground"
	default:
		return "bg-accent"
	}
}

func boardStateTextClass(state string) string {
	switch normalizeTimelineState(state) {
	case "todo", "rework":
		return "text-warning"
	case "review", "done":
		return "text-success"
	case "blocked":
		return "text-danger"
	case "backlog":
		return "text-muted-foreground"
	default:
		return "text-accent"
	}
}

func completedModel(row telemetry.Completed) string {
	if strings.TrimSpace(row.Model) == "" {
		return "n/a"
	}
	return row.Model
}

func timeLabel(value time.Time) string {
	if value.IsZero() {
		return "n/a"
	}
	return value.UTC().Format("Jan 2 15:04:05 UTC")
}

func agentTimelineRows(snapshot telemetry.Snapshot) []agentTimelineRow {
	entries := agentTimelineEntries(snapshot)
	if len(entries) == 0 {
		return nil
	}

	sortAgentTimelineEntries(entries)
	start, end := agentTimelineRange(entries)
	span := end.Sub(start).Seconds()
	if span <= 0 {
		span = 1
	}

	rows := make([]agentTimelineRow, 0, len(entries))
	for _, entry := range entries {
		startPercent := timelinePercent(entry.start, start, span)
		endPercent := timelinePercent(entry.end, start, span)
		width := endPercent - startPercent
		if width < 0 {
			width = 0
		}

		state := chartText(entry.state, "running")
		endLabel := timeLabel(entry.end)
		if entry.running {
			endLabel = "Live now"
		}

		identifier := issueIdentifier(entry.issue)
		title := issueTitle(entry.issue)
		segmentLabel := title
		if segmentLabel == "Untitled issue" {
			segmentLabel = identifier
		}
		segmentTitle := segmentLabel + ": " + state + " from " + timeLabel(entry.start) + " to " + endLabel

		rows = append(rows, agentTimelineRow{
			Identifier:   identifier,
			Title:        title,
			State:        state,
			StartedAt:    timeLabel(entry.start),
			EndedAt:      endLabel,
			Duration:     formatDuration(entry.end.Sub(entry.start).Seconds()),
			StartPercent: percentLabel(startPercent),
			EndPercent:   percentLabel(endPercent),
			Segments: []agentTimelineSegment{
				{
					Label: state,
					Class: agentTimelineStateClass(state),
					Style: "left: " + percentLabel(startPercent) + "; width: " + percentLabel(width) + ";",
					Title: segmentTitle,
					Width: percentLabel(width),
				},
			},
		})
	}

	return rows
}

func agentTimelineEntries(snapshot telemetry.Snapshot) []agentTimelineEntry {
	now, hasNow := agentTimelineNow(snapshot)
	entries := make([]agentTimelineEntry, 0, len(snapshot.Running)+len(snapshot.Completed))
	for _, row := range snapshot.Running {
		start, ok := agentTimelineStart(row.StartedAt, now, hasNow, row.RuntimeSeconds)
		if !ok {
			continue
		}

		end := now
		if !hasNow {
			end = start
			if row.RuntimeSeconds > 0 {
				end = start.Add(time.Duration(math.Round(row.RuntimeSeconds)) * time.Second)
			}
		}
		if end.Before(start) {
			end = start
		}

		entries = append(entries, agentTimelineEntry{
			issue:   row.Issue,
			state:   issueState(row.Issue, "Running"),
			start:   start.UTC(),
			end:     end.UTC(),
			running: true,
		})
	}

	for _, row := range snapshot.Completed {
		if row.CompletedAt.IsZero() {
			continue
		}
		end := row.CompletedAt.UTC()
		start := row.StartedAt
		if start.IsZero() && row.RuntimeSeconds > 0 {
			start = end.Add(-time.Duration(math.Round(row.RuntimeSeconds)) * time.Second)
		}
		if start.IsZero() {
			continue
		}
		if end.Before(start) {
			end = start
		}

		entries = append(entries, agentTimelineEntry{
			issue: row.Issue,
			state: completedState(row),
			start: start.UTC(),
			end:   end.UTC(),
		})
	}

	return entries
}

func agentTimelineNow(snapshot telemetry.Snapshot) (time.Time, bool) {
	if !snapshot.GeneratedAt.IsZero() {
		return snapshot.GeneratedAt.UTC(), true
	}

	var latest time.Time
	for _, row := range snapshot.Running {
		if row.LastEventAt != nil && row.LastEventAt.After(latest) {
			latest = *row.LastEventAt
		}
		if row.StartedAt.After(latest) {
			latest = row.StartedAt
		}
	}
	for _, row := range snapshot.Completed {
		if row.CompletedAt.After(latest) {
			latest = row.CompletedAt
		}
	}
	if latest.IsZero() {
		return time.Time{}, false
	}
	return latest.UTC(), true
}

func agentTimelineStart(start time.Time, now time.Time, hasNow bool, runtimeSeconds float64) (time.Time, bool) {
	if !start.IsZero() {
		return start.UTC(), true
	}
	if hasNow && runtimeSeconds > 0 {
		return now.Add(-time.Duration(math.Round(runtimeSeconds)) * time.Second).UTC(), true
	}
	return time.Time{}, false
}

func sortAgentTimelineEntries(entries []agentTimelineEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].start.Equal(entries[j].start) {
			return entries[i].start.Before(entries[j].start)
		}
		return issueIdentifier(entries[i].issue) < issueIdentifier(entries[j].issue)
	})
}

func agentTimelineRange(entries []agentTimelineEntry) (time.Time, time.Time) {
	start := entries[0].start
	end := entries[0].end
	for _, entry := range entries[1:] {
		if entry.start.Before(start) {
			start = entry.start
		}
		if entry.end.After(end) {
			end = entry.end
		}
	}
	if !end.After(start) {
		end = start.Add(time.Second)
	}
	return start, end
}

func timelinePercent(value time.Time, start time.Time, spanSeconds float64) float64 {
	if spanSeconds <= 0 {
		return 0
	}
	return clampPercent(value.Sub(start).Seconds() / spanSeconds * 100)
}

func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func percentLabel(value float64) string {
	return fmt.Sprintf("%.2f%%", clampPercent(value))
}

func agentTimelineStateClass(state string) string {
	switch normalizeTimelineState(state) {
	case "completed", "complete", "done", "human review":
		return "bg-success"
	case "blocked", "failed", "failure", "cancelled", "canceled":
		return "bg-danger"
	case "backlog", "queued", "queue", "retry", "retrying", "todo":
		return "bg-warning"
	default:
		return "bg-accent"
	}
}

func normalizeTimelineState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
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

func budgetSpendTodayLabel(budget telemetry.Budget) string {
	return formatUSD(budget.CurrentSpendUSD) + " / " + budgetDailyCapLabel(budget)
}

func budgetDailyCapLabel(budget telemetry.Budget) string {
	if !budget.Enabled {
		return "off"
	}
	return optionalUSD(budget.PerDayMaxUSD)
}

func budgetDailyUsageStyle(budget telemetry.Budget) string {
	if budget.PerDayMaxUSD == nil || *budget.PerDayMaxUSD <= 0 {
		return percentStyle(0)
	}
	return percentStyle(int(math.Round(budget.CurrentSpendUSD / *budget.PerDayMaxUSD * 100)))
}

func budgetBurnDownView(snapshot telemetry.Snapshot) budgetBurnDownViewModel {
	budget := snapshot.Budget
	if !budget.Enabled {
		return budgetBurnDownViewModel{
			EmptyTitle:      "Budget disabled.",
			EmptyDetail:     "Enable a daily budget cap to show spend burn-down.",
			CurrentLabel:    formatUSD(budget.CurrentSpendUSD),
			CapLabel:        optionalUSD(budget.PerDayMaxUSD),
			ProjectionLabel: formatUSD(budget.ProjectedCostUSD),
		}
	}

	now := snapshot.GeneratedAt.UTC()
	if now.IsZero() {
		now = latestBudgetPointAt(budget.SpendPoints)
	}
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Second)
	}

	periodStart, periodEnd := budgetPeriod(budget, now)
	currentSpend := budgetCurrentSpendUSD(budget)
	projectedSpend := budget.ProjectedSpendUSD
	if projectedSpend <= 0 {
		projectedSpend = budgetProjectedSpendUSD(periodStart, periodEnd, now, currentSpend)
	}

	actualPoints := budgetActualPoints(budget.SpendPoints, periodStart, periodEnd, now, currentSpend)
	if currentSpend <= 0 && len(actualPoints) <= 1 {
		return budgetBurnDownViewModel{
			EmptyTitle:      "No budget spend yet.",
			EmptyDetail:     "Cumulative spend and projection will appear after usage is recorded.",
			CurrentLabel:    formatUSD(currentSpend),
			CapLabel:        optionalUSD(budget.PerDayMaxUSD),
			ProjectionLabel: formatUSD(projectedSpend),
		}
	}

	lastActual := actualPoints[len(actualPoints)-1]
	return budgetBurnDownViewModel{
		Available:       true,
		PeriodLabel:     budgetPeriodLabel(periodStart, periodEnd),
		CurrentLabel:    formatUSD(currentSpend),
		CapLabel:        optionalUSD(budget.PerDayMaxUSD),
		ProjectionLabel: formatUSD(projectedSpend),
		Chart: BudgetProjectionChartData{
			Title:        "Cost burn-down",
			AriaLabel:    "Cumulative cost burn-down with budget cap and projected period-end spend",
			ActualPoints: actualPoints,
			ProjectionPoints: []BudgetProjectionPoint{
				{
					Label: "Current spend",
					At:    lastActual.At,
					Value: lastActual.Value,
				},
				{
					Label: "Projected period end",
					At:    periodEnd,
					Value: projectedSpend,
				},
			},
			PeriodStart: periodStart,
			PeriodEnd:   periodEnd,
			Cap:         budgetCapValue(budget.PerDayMaxUSD),
		},
	}
}

func budgetProjectedSpendUSD(periodStart time.Time, periodEnd time.Time, now time.Time, currentSpend float64) float64 {
	if currentSpend <= 0 {
		return 0
	}
	if periodStart.IsZero() || !periodEnd.After(periodStart) {
		return currentSpend
	}
	elapsed := now.Sub(periodStart).Seconds()
	if elapsed <= 0 {
		return currentSpend
	}
	total := periodEnd.Sub(periodStart).Seconds()
	if total <= 0 {
		return currentSpend
	}
	projected := currentSpend * total / elapsed
	if projected < currentSpend {
		return currentSpend
	}
	return projected
}

func budgetPeriod(budget telemetry.Budget, now time.Time) (time.Time, time.Time) {
	start := budget.PeriodStart.UTC()
	end := budget.PeriodEnd.UTC()
	if !start.IsZero() && end.After(start) {
		return start, end
	}
	year, month, day := now.UTC().Date()
	start = time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 0, 1)
}

func budgetCurrentSpendUSD(budget telemetry.Budget) float64 {
	current := budget.CurrentSpendUSD
	for _, point := range budget.SpendPoints {
		if point.SpendUSD > current {
			current = point.SpendUSD
		}
	}
	if current < 0 {
		return 0
	}
	return current
}

func budgetActualPoints(points []telemetry.BudgetSpendPoint, periodStart time.Time, periodEnd time.Time, now time.Time, currentSpend float64) []BudgetProjectionPoint {
	filtered := make([]telemetry.BudgetSpendPoint, 0, len(points))
	for _, point := range points {
		at := point.At.UTC()
		if at.IsZero() || at.Before(periodStart) || !at.Before(periodEnd) {
			continue
		}
		if point.SpendUSD < 0 {
			continue
		}
		filtered = append(filtered, telemetry.BudgetSpendPoint{At: at, SpendUSD: point.SpendUSD})
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		return filtered[i].At.Before(filtered[j].At)
	})

	out := []BudgetProjectionPoint{
		{
			Label: "Period start",
			At:    periodStart,
			Value: 0,
		},
	}
	lastSpend := 0.0
	for _, point := range filtered {
		spend := point.SpendUSD
		if spend < lastSpend {
			spend = lastSpend
		}
		lastSpend = spend
		out = append(out, BudgetProjectionPoint{
			Label: budgetPointLabel(point.At),
			At:    point.At,
			Value: spend,
		})
	}

	if currentSpend < lastSpend {
		currentSpend = lastSpend
	}
	if currentSpend > lastSpend {
		at := now.UTC()
		if at.Before(periodStart) {
			at = periodStart
		}
		if !at.Before(periodEnd) {
			at = periodEnd
		}
		out = append(out, BudgetProjectionPoint{
			Label: "Current spend",
			At:    at,
			Value: currentSpend,
		})
	}
	return out
}

func latestBudgetPointAt(points []telemetry.BudgetSpendPoint) time.Time {
	var latest time.Time
	for _, point := range points {
		at := point.At.UTC()
		if at.After(latest) {
			latest = at
		}
	}
	return latest
}

func budgetPeriodLabel(periodStart time.Time, periodEnd time.Time) string {
	return periodStart.UTC().Format("Jan 2 15:04") + " - " + periodEnd.UTC().Format("Jan 2 15:04 UTC")
}

func budgetPointLabel(at time.Time) string {
	if at.IsZero() {
		return "Spend"
	}
	at = at.UTC()
	if at.Second() == 0 {
		return at.Format("15:04")
	}
	return at.Format("15:04:05")
}

func budgetCapValue(cap *float64) float64 {
	if cap == nil || *cap <= 0 {
		return 0
	}
	return *cap
}

func budgetHistoryBars(budget telemetry.Budget) []budgetHistoryBar {
	days := budgetHistoryDays(budget.Days)
	if len(days) == 0 {
		return nil
	}

	maxSpend := 0.0
	for _, day := range days {
		if day.SpendUSD > maxSpend {
			maxSpend = day.SpendUSD
		}
	}

	bars := make([]budgetHistoryBar, 0, len(days))
	for _, day := range days {
		bars = append(bars, budgetHistoryBar{
			Style: budgetHistoryHeightStyle(day.SpendUSD, maxSpend),
			Title: budgetDayLabel(day) + ": " + formatUSD(day.SpendUSD),
		})
	}
	return bars
}

func budgetHistoryDays(days []telemetry.BudgetDay) []telemetry.BudgetDay {
	const maxBudgetHistoryDays = 7
	if len(days) <= maxBudgetHistoryDays {
		return days
	}
	return days[len(days)-maxBudgetHistoryDays:]
}

func budgetHistoryCount(budget telemetry.Budget) string {
	count := len(budgetHistoryDays(budget.Days))
	switch count {
	case 0:
		return "No history"
	case 1:
		return "1 day"
	default:
		return formatInt(int64(count)) + " days"
	}
}

func budgetHistoryHeightStyle(spend float64, maxSpend float64) string {
	percent := 12
	if spend > 0 && maxSpend > 0 {
		percent = int(math.Round(spend / maxSpend * 100))
		if percent < 12 {
			percent = 12
		}
	}
	if percent > 100 {
		percent = 100
	}
	return fmt.Sprintf("height: %d%%;", percent)
}

func budgetDayLabel(day telemetry.BudgetDay) string {
	date := strings.TrimSpace(day.Date)
	if date == "" {
		return "n/a"
	}
	return date
}

func runtimeStatusLabel(snapshot telemetry.Snapshot) string {
	if snapshot.GeneratedAt.IsZero() {
		return "Offline"
	}
	return "Live"
}

func runtimeStatusClass(snapshot telemetry.Snapshot) string {
	if snapshot.GeneratedAt.IsZero() {
		return "border-border bg-muted text-muted-foreground"
	}
	return "border-success-soft bg-success-soft text-success"
}

func statsStatusLabel(snapshot telemetry.Snapshot) string {
	if snapshot.GeneratedAt.IsZero() {
		return "Stats pending"
	}
	if snapshot.LifetimeTotals.Available {
		return "Stats healthy"
	}
	return "Stats degraded"
}

func statsStatusClass(snapshot telemetry.Snapshot) string {
	if snapshot.GeneratedAt.IsZero() {
		return "border-border bg-muted text-muted-foreground"
	}
	if snapshot.LifetimeTotals.Available {
		return "border-success-soft bg-success-soft text-success"
	}
	return "border-danger-soft bg-danger-soft text-danger"
}

func statsStatusTitle(snapshot telemetry.Snapshot) string {
	if snapshot.GeneratedAt.IsZero() {
		return "Waiting for the first telemetry snapshot."
	}
	if snapshot.LifetimeTotals.Available {
		return "Runtime statistics are available."
	}
	return lifetimeDegradedReason(snapshot.LifetimeTotals)
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

func tokenTrendChart(snapshot telemetry.Snapshot) SplitSeriesChartData {
	points := tokenTrendPoints(snapshot)
	chartPoints := make([]SplitSeriesPoint, 0, len(points))
	for _, point := range points {
		chartPoints = append(chartPoints, SplitSeriesPoint{
			Label:  tokenTrendLabel(point),
			Input:  float64(point.Input),
			Output: float64(point.Output),
		})
	}
	return SplitSeriesChartData{
		Title:       "Token trend",
		AriaLabel:   "Token trend",
		InputLabel:  "Input",
		OutputLabel: "Output",
		Points:      chartPoints,
		ValueSuffix: "tokens",
	}
}

func throughputTrendChart(data DashboardData) SeriesChartData {
	return SeriesChartData{
		Title:       "Token throughput trend",
		AriaLabel:   "Rolling token throughput trend",
		Points:      throughputTrendPoints(data.Snapshot),
		ValueSuffix: "tps",
		ColorClass:  "text-accent",
	}
}

func throughputRate(snapshot telemetry.Snapshot) string {
	return formatDecimal(snapshot.Throughput.TokensPerSecond) + " tps"
}

func throughputWindowLabel(snapshot telemetry.Snapshot) string {
	window := time.Duration(snapshot.Throughput.WindowSeconds) * time.Second
	if window <= 0 {
		window = defaultThroughputWindow
	}
	return "Last " + formatDurationWindow(window) + " token throughput"
}

func runtimeLabel(snapshot telemetry.Snapshot) string {
	return formatDuration(snapshot.Tokens.RuntimeSeconds)
}

func tokenRate(snapshot telemetry.Snapshot) string {
	if snapshot.Tokens.Total <= 0 || snapshot.Tokens.RuntimeSeconds <= 0 {
		return "n/a"
	}
	perMinute := int64(math.Round(float64(snapshot.Tokens.Total) / snapshot.Tokens.RuntimeSeconds * 60))
	return formatInt(perMinute) + " tokens/min"
}

func lifetimeStatus(totals telemetry.LifetimeTotals) string {
	if totals.Available {
		return "available"
	}
	return "unavailable"
}

func lifetimeDegradedReason(totals telemetry.LifetimeTotals) string {
	if strings.TrimSpace(totals.DegradedReason) != "" {
		return totals.DegradedReason
	}
	return "runtime store unavailable"
}

func lifetimeRuntime(totals telemetry.LifetimeTotals) string {
	return formatDuration(float64(totals.RuntimeSeconds))
}

func lifetimeSessions(totals telemetry.LifetimeTotals) string {
	return formatInt(totals.Sessions)
}

func lifetimeRuns(totals telemetry.LifetimeTotals) string {
	return formatInt(totals.Runs)
}

func throughputTrendPoints(snapshot telemetry.Snapshot) []webchart.Point {
	points := tokenTrendPoints(snapshot)
	if len(points) < 2 {
		return nil
	}

	latest := points[len(points)-1].At.UTC()
	windowStart := latest.Add(-throughputTrendWindow)
	chartPoints := make([]webchart.Point, 0, len(points)-1)
	for index := 1; index < len(points); index++ {
		previous := points[index-1]
		current := points[index]
		if current.At.IsZero() || previous.At.IsZero() || current.At.Before(windowStart) {
			continue
		}
		elapsed := current.At.Sub(previous.At).Seconds()
		if elapsed <= 0 {
			continue
		}
		tokens := current.Total - previous.Total
		if tokens <= 0 {
			continue
		}
		chartPoints = append(chartPoints, webchart.Point{
			Label: throughputTrendLabel(current.At),
			Value: float64(tokens) / elapsed,
		})
	}
	return chartPoints
}

func throughputTrendLabel(at time.Time) string {
	if at.IsZero() {
		return "Latest"
	}
	at = at.UTC()
	if at.Second() == 0 {
		return at.Format("15:04")
	}
	return at.Format("15:04:05")
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

func formatDurationWindow(duration time.Duration) string {
	if duration <= 0 {
		return "0s"
	}
	if duration%time.Hour == 0 {
		return formatInt(int64(duration/time.Hour)) + "h"
	}
	if duration%time.Minute == 0 {
		return formatInt(int64(duration/time.Minute)) + "m"
	}
	return formatDuration(duration.Seconds())
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

func formatDecimal(value float64) string {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return "0"
	}

	rounded := math.Round(value*10) / 10
	if math.Abs(rounded-math.Round(rounded)) < 0.000001 {
		return formatInt(int64(math.Round(rounded)))
	}
	return strconv.FormatFloat(rounded, 'f', 1, 64)
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

func tokenTrendPoints(snapshot telemetry.Snapshot) []telemetry.TokenTrendPoint {
	if len(snapshot.TokenTrend) > 0 {
		points := make([]telemetry.TokenTrendPoint, 0, len(snapshot.TokenTrend))
		for _, point := range snapshot.TokenTrend {
			if point.Input <= 0 && point.Output <= 0 && point.Total <= 0 {
				continue
			}
			if point.Total <= 0 {
				point.Total = point.Input + point.Output
			}
			points = append(points, point)
		}
		return points
	}

	if snapshot.Tokens.Input <= 0 && snapshot.Tokens.Output <= 0 && snapshot.Tokens.Total <= 0 {
		return nil
	}
	return []telemetry.TokenTrendPoint{
		{
			At:     snapshot.GeneratedAt,
			Input:  snapshot.Tokens.Input,
			Output: snapshot.Tokens.Output,
			Total:  snapshot.Tokens.Total,
		},
	}
}

func tokenTrendLabel(point telemetry.TokenTrendPoint) string {
	if point.At.IsZero() {
		return "Latest"
	}
	return point.At.UTC().Format("15:04")
}
