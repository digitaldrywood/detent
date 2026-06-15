package web

import (
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const (
	defaultTimeSeriesWindow = 10 * time.Minute
	defaultTimeSeriesBucket = time.Minute
	maxTimeSeriesWindow     = 24 * time.Hour
	maxTimeSeriesBuckets    = 240
)

type timeSeriesAPIResponse struct {
	GeneratedAt     time.Time           `json:"generated_at"`
	Scope           string              `json:"scope"`
	ProjectID       string              `json:"project_id,omitempty"`
	Labels          []string            `json:"labels"`
	RunningAgents   []timeSeriesDataset `json:"running_agents"`
	TokensPerSecond timeSeriesDataset   `json:"tokens_per_second"`
	Completions     []timeSeriesDataset `json:"completions"`
	TokenSpend      timeSeriesDataset   `json:"token_spend"`
	BoardFlow       []timeSeriesDataset `json:"board_flow,omitempty"`
}

type timeSeriesDataset struct {
	ProjectID string    `json:"project_id,omitempty"`
	Label     string    `json:"label"`
	Data      []float64 `json:"data"`
}

func projectScopedSnapshot(snapshot telemetry.Snapshot, selectedProjectID string) (telemetry.Snapshot, bool) {
	selectedProjectID = strings.TrimSpace(selectedProjectID)
	if selectedProjectID == "" {
		return snapshot, true
	}
	projectSnapshot, ok := projectSnapshotForID(snapshot, selectedProjectID)
	if ok {
		return projectScopedSnapshotForProject(snapshot, projectSnapshot.Project), true
	}
	if telemetryProjectMatches(snapshot.Project, selectedProjectID) {
		return projectScopedSnapshotForProject(snapshot, snapshot.Project), true
	}
	return telemetry.Snapshot{}, false
}

func projectScopedSnapshotForProject(snapshot telemetry.Snapshot, selectedProject telemetry.Project) telemetry.Snapshot {
	selectedProjectID := projectID(selectedProject)
	if selectedProjectID == "" {
		return snapshot
	}

	sourceProject, hasSourceProject := projectSnapshotForID(snapshot, selectedProjectID)
	fallbackProjectID := ""
	if len(snapshot.Projects) == 0 && telemetryProjectMatches(snapshot.Project, selectedProjectID) {
		fallbackProjectID = selectedProjectID
	}

	out := snapshot
	out.Project = selectedProject
	out.Projects = nil
	out.BoardIssues = scopedIssues(snapshot.BoardIssues, selectedProjectID, fallbackProjectID)
	out.Pipeline = scopedIssues(snapshot.Pipeline, selectedProjectID, fallbackProjectID)
	out.Running = scopedRunning(snapshot.Running, selectedProjectID, fallbackProjectID)
	out.Queue = scopedQueue(snapshot.Queue, selectedProjectID, fallbackProjectID)
	out.Blocked = scopedBlocked(snapshot.Blocked, selectedProjectID, fallbackProjectID)
	out.Completed = scopedCompleted(snapshot.Completed, selectedProjectID, fallbackProjectID)
	if hasSourceProject {
		out.Counts = sourceProject.Counts
		out.Tokens = sourceProject.Tokens
		out.Throughput = sourceProject.Throughput
	} else {
		out.Counts = telemetry.Counts{
			Running:   len(out.Running),
			Queue:     len(out.Queue),
			Blocked:   len(out.Blocked),
			Completed: len(out.Completed),
		}
		out.Tokens = scopedTokens(out.Running, out.Completed)
		out.Throughput = telemetry.TokenThroughput{}
	}
	if len(snapshot.Projects) > 0 {
		out.TokenTrend = nil
	}
	return out
}

func projectSnapshotForID(snapshot telemetry.Snapshot, selectedProjectID string) (telemetry.ProjectSnapshot, bool) {
	for _, projectSnapshot := range snapshot.Projects {
		if telemetryProjectMatches(projectSnapshot.Project, selectedProjectID) {
			return projectSnapshot, true
		}
	}
	return telemetry.ProjectSnapshot{}, false
}

func telemetryProjectMatches(project telemetry.Project, selectedProjectID string) bool {
	selectedProjectID = strings.TrimSpace(selectedProjectID)
	return selectedProjectID != "" && projectID(project) == selectedProjectID
}

func issueMatchesProject(issue telemetry.Issue, selectedProjectID string, fallbackProjectID string) bool {
	projectID := strings.TrimSpace(issue.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(fallbackProjectID)
	}
	return projectID == selectedProjectID
}

func scopedIssues(entries []telemetry.Issue, selectedProjectID string, fallbackProjectID string) []telemetry.Issue {
	out := make([]telemetry.Issue, 0, len(entries))
	for _, entry := range entries {
		if issueMatchesProject(entry, selectedProjectID, fallbackProjectID) {
			out = append(out, entry)
		}
	}
	return out
}

func scopedRunning(entries []telemetry.Running, selectedProjectID string, fallbackProjectID string) []telemetry.Running {
	out := make([]telemetry.Running, 0, len(entries))
	for _, entry := range entries {
		if issueMatchesProject(entry.Issue, selectedProjectID, fallbackProjectID) {
			out = append(out, entry)
		}
	}
	return out
}

func scopedQueue(entries []telemetry.Queued, selectedProjectID string, fallbackProjectID string) []telemetry.Queued {
	out := make([]telemetry.Queued, 0, len(entries))
	for _, entry := range entries {
		if issueMatchesProject(entry.Issue, selectedProjectID, fallbackProjectID) {
			out = append(out, entry)
		}
	}
	return out
}

func scopedBlocked(entries []telemetry.Blocked, selectedProjectID string, fallbackProjectID string) []telemetry.Blocked {
	out := make([]telemetry.Blocked, 0, len(entries))
	for _, entry := range entries {
		if issueMatchesProject(entry.Issue, selectedProjectID, fallbackProjectID) {
			out = append(out, entry)
		}
	}
	return out
}

func scopedCompleted(entries []telemetry.Completed, selectedProjectID string, fallbackProjectID string) []telemetry.Completed {
	out := make([]telemetry.Completed, 0, len(entries))
	for _, entry := range entries {
		if issueMatchesProject(entry.Issue, selectedProjectID, fallbackProjectID) {
			out = append(out, entry)
		}
	}
	return out
}

func scopedTokens(running []telemetry.Running, completed []telemetry.Completed) telemetry.Tokens {
	tokens := telemetry.Tokens{}
	for _, row := range running {
		tokens.Input += row.Tokens.Input
		tokens.Output += row.Tokens.Output
		tokens.Total += row.Tokens.Total
		tokens.RuntimeSeconds += row.Tokens.RuntimeSeconds
	}
	for _, row := range completed {
		tokens.Input += row.Tokens.Input
		tokens.Output += row.Tokens.Output
		tokens.Total += row.Tokens.Total
		tokens.RuntimeSeconds += row.Tokens.RuntimeSeconds
	}
	return tokens
}

func projectTimeSeriesResponse(
	projects []templates.ProjectSmallMultiple,
	selectedProjectID string,
	now time.Time,
	window time.Duration,
	bucket time.Duration,
) timeSeriesAPIResponse {
	now = normalizedSeriesNow(now)
	window, bucket = cappedTimeSeriesWindow(window, bucket)
	buckets := timeSeriesBuckets(now, window, bucket)
	labels := timeSeriesLabels(buckets)
	selectedProjectID = strings.TrimSpace(selectedProjectID)

	seriesProjects := append([]templates.ProjectSmallMultiple(nil), projects...)
	if selectedProjectID != "" {
		seriesProjects = seriesProjects[:0]
		for _, project := range projects {
			if strings.TrimSpace(project.ID) == selectedProjectID {
				seriesProjects = append(seriesProjects, project)
			}
		}
	}
	sort.SliceStable(seriesProjects, func(i, j int) bool {
		return timeSeriesProjectName(seriesProjects[i]) < timeSeriesProjectName(seriesProjects[j])
	})

	response := timeSeriesAPIResponse{
		GeneratedAt: now,
		Scope:       "fleet",
		Labels:      labels,
		TokensPerSecond: timeSeriesDataset{
			Label: "Tokens/sec",
			Data:  make([]float64, len(labels)),
		},
		TokenSpend: timeSeriesDataset{
			Label: "Spend",
			Data:  make([]float64, len(labels)),
		},
	}
	if selectedProjectID != "" {
		response.Scope = "project"
		response.ProjectID = selectedProjectID
		response.BoardFlow = []timeSeriesDataset{
			{Label: "Running", Data: make([]float64, len(labels))},
			{Label: "Queued", Data: make([]float64, len(labels))},
			{Label: "Blocked", Data: make([]float64, len(labels))},
			{Label: "Completed", Data: make([]float64, len(labels))},
		}
	}

	for _, project := range seriesProjects {
		projectID := strings.TrimSpace(project.ID)
		name := timeSeriesProjectName(project)
		running := timeSeriesDataset{ProjectID: projectID, Label: name, Data: make([]float64, len(labels))}
		completed := timeSeriesDataset{ProjectID: projectID, Label: name, Data: make([]float64, len(labels))}
		for _, sample := range timeSeriesProjectSamples(project) {
			index, ok := timeSeriesBucketIndex(sample.At, buckets, now, bucket)
			if !ok {
				continue
			}
			running.Data[index] = float64(sample.Running)
			completed.Data[index] = float64(sample.Completed)
			response.TokensPerSecond.Data[index] += sample.ThroughputTokensPerSecond
			response.TokenSpend.Data[index] += sample.SpendUSD
			if selectedProjectID != "" && len(response.BoardFlow) == 4 {
				response.BoardFlow[0].Data[index] = float64(sample.Running)
				response.BoardFlow[1].Data[index] = float64(sample.QueueDepth)
				response.BoardFlow[2].Data[index] = float64(sample.Blocked)
				response.BoardFlow[3].Data[index] = float64(sample.Completed)
			}
		}
		response.RunningAgents = append(response.RunningAgents, running)
		response.Completions = append(response.Completions, completed)
	}

	return response
}

func timeSeriesProjectName(project templates.ProjectSmallMultiple) string {
	name := strings.TrimSpace(project.Name)
	if name != "" {
		return name
	}
	id := strings.TrimSpace(project.ID)
	if id != "" {
		return id
	}
	return "unknown project"
}

func timeSeriesProjectSamples(project templates.ProjectSmallMultiple) []templates.ProjectSmallMultipleSample {
	if len(project.Samples) > 0 {
		return append([]templates.ProjectSmallMultipleSample(nil), project.Samples...)
	}
	return []templates.ProjectSmallMultipleSample{
		{
			Running:                   project.Running,
			TotalTokens:               project.TotalTokens,
			ThroughputTokensPerSecond: project.ThroughputTokensPerSecond,
			SpendUSD:                  project.CurrentSpendUSD,
			QueueDepth:                project.QueueCount,
			Blocked:                   project.Blocked,
			Completed:                 project.Completed,
		},
	}
}

func normalizedSeriesNow(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC().Truncate(time.Second)
	}
	return now.UTC()
}

func cappedTimeSeriesWindow(window time.Duration, bucket time.Duration) (time.Duration, time.Duration) {
	if window <= 0 {
		window = defaultTimeSeriesWindow
	}
	if window > maxTimeSeriesWindow {
		window = maxTimeSeriesWindow
	}
	if bucket <= 0 {
		bucket = defaultTimeSeriesBucket
	}
	if bucket > window {
		bucket = window
	}
	if int(window/bucket)+1 > maxTimeSeriesBuckets {
		bucket = window / time.Duration(maxTimeSeriesBuckets-1)
		if bucket <= 0 {
			bucket = time.Second
		}
	}
	return window, bucket
}

func timeSeriesBuckets(now time.Time, window time.Duration, bucket time.Duration) []time.Time {
	start := now.Add(-window).Truncate(bucket)
	buckets := make([]time.Time, 0, int(window/bucket)+2)
	for at := start; !at.After(now); at = at.Add(bucket) {
		buckets = append(buckets, at.UTC())
	}
	if len(buckets) == 0 {
		return []time.Time{now.UTC()}
	}
	return buckets
}

func timeSeriesLabels(buckets []time.Time) []string {
	labels := make([]string, 0, len(buckets))
	for _, bucket := range buckets {
		labels = append(labels, bucket.UTC().Format("15:04"))
	}
	return labels
}

func timeSeriesBucketIndex(at time.Time, buckets []time.Time, now time.Time, bucket time.Duration) (int, bool) {
	if len(buckets) == 0 {
		return 0, false
	}
	if at.IsZero() {
		return len(buckets) - 1, true
	}
	at = at.UTC()
	if at.After(now) || at.Before(buckets[0]) {
		return 0, false
	}
	index := int(at.Sub(buckets[0]) / bucket)
	if index >= len(buckets) {
		index = len(buckets) - 1
	}
	return index, true
}
