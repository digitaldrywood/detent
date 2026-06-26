package web

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const maxWorkflowOldestCards = 8

func (s *Server) snapshotWorkflowMetrics(ctx context.Context, snapshot telemetry.Snapshot) telemetry.WorkflowMetrics {
	if s.store == nil {
		return telemetry.WorkflowMetrics{}
	}

	now := snapshot.GeneratedAt
	if now.IsZero() {
		now = time.Now()
	}

	projectID := strings.TrimSpace(snapshot.Project.ID)
	windows := []struct {
		label    string
		duration time.Duration
	}{
		{label: "24h", duration: 24 * time.Hour},
		{label: "7d", duration: 7 * 24 * time.Hour},
		{label: "30d", duration: 30 * 24 * time.Hour},
	}

	out := telemetry.WorkflowMetrics{
		Available:        true,
		OldestCards:      workflowOldestCards(snapshot),
		ActiveBottleneck: workflowActiveBottleneck(snapshot, now),
	}
	for _, window := range windows {
		from := now.Add(-window.duration)
		report, err := s.store.WorkflowMetricsReport(ctx, store.WorkflowMetricsQuery{
			ProjectID: projectID,
			From:      from,
			To:        now,
		})
		if err != nil {
			s.logger.Warn("workflow metrics report failed", slog.Any("error", err))
			return telemetry.WorkflowMetrics{DegradedReason: "workflow metrics query failed"}
		}
		out.Windows = append(out.Windows, telemetry.WorkflowMetricsWindow{
			Label:     window.label,
			From:      from,
			To:        now,
			Lanes:     workflowPhaseMetricsFromStore(report.Lanes),
			SubPhases: workflowPhaseMetricsFromStore(report.SubPhases),
		})
	}
	return out
}

func workflowPhaseMetricsFromStore(metrics []store.WorkflowPhaseMetric) []telemetry.WorkflowPhaseMetric {
	out := make([]telemetry.WorkflowPhaseMetric, 0, len(metrics))
	for _, metric := range metrics {
		out = append(out, telemetry.WorkflowPhaseMetric{
			ProjectID:      metric.ProjectID,
			PhaseType:      metric.PhaseType,
			PhaseName:      metric.PhaseName,
			Count:          metric.Count,
			TotalSeconds:   metric.TotalSeconds,
			AverageSeconds: metric.AverageSeconds,
			P50Seconds:     metric.P50Seconds,
			P90Seconds:     metric.P90Seconds,
			P95Seconds:     metric.P95Seconds,
			InputTokens:    metric.InputTokens,
			OutputTokens:   metric.OutputTokens,
			TotalTokens:    metric.TotalTokens,
			Turns:          metric.Turns,
			EndpointFamily: metric.EndpointFamily,
		})
	}
	return out
}

func workflowOldestCards(snapshot telemetry.Snapshot) []telemetry.WorkflowLaneAge {
	cards := workflowLaneAgeCards(snapshot)
	sort.SliceStable(cards, func(i int, j int) bool {
		if cards[i].AgeSeconds == cards[j].AgeSeconds {
			return cards[i].Identifier < cards[j].Identifier
		}
		return cards[i].AgeSeconds > cards[j].AgeSeconds
	})
	if len(cards) > maxWorkflowOldestCards {
		return append([]telemetry.WorkflowLaneAge(nil), cards[:maxWorkflowOldestCards]...)
	}
	return cards
}

func workflowLaneAgeCards(snapshot telemetry.Snapshot) []telemetry.WorkflowLaneAge {
	var issues []telemetry.Issue
	issues = append(issues, snapshot.BoardIssues...)
	issues = append(issues, snapshot.Pipeline...)
	for _, row := range snapshot.Running {
		issues = append(issues, row.Issue)
	}
	for _, row := range snapshot.Queue {
		issues = append(issues, row.Issue)
	}
	for _, row := range snapshot.Blocked {
		issues = append(issues, row.Issue)
	}
	for _, row := range snapshot.Completed {
		issues = append(issues, row.Issue)
	}

	seen := map[string]struct{}{}
	out := make([]telemetry.WorkflowLaneAge, 0, len(issues))
	for _, issue := range issues {
		key := strings.TrimSpace(issue.ProjectID) + "\x00" + strings.TrimSpace(issue.ID) + "\x00" + strings.TrimSpace(issue.State)
		if strings.TrimSpace(issue.ID) == "" {
			key = strings.TrimSpace(issue.ProjectID) + "\x00" + strings.TrimSpace(issue.Identifier) + "\x00" + strings.TrimSpace(issue.State)
		}
		if key == "\x00\x00" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, telemetry.WorkflowLaneAge{
			ProjectID:     issue.ProjectID,
			IssueID:       issue.ID,
			Identifier:    issue.Identifier,
			URL:           issue.URL,
			Title:         issue.Title,
			State:         issue.State,
			EnteredAt:     issue.CurrentLaneEnteredAt,
			AgeSeconds:    issue.CurrentLaneAgeSeconds,
			BottleneckKey: workflowIssueBottleneckKey(issue),
		})
	}
	return out
}

func workflowActiveBottleneck(snapshot telemetry.Snapshot, now time.Time) telemetry.WorkflowBottleneck {
	if backoff := workflowRESTBackoffBottleneck(snapshot.RateLimits, now); !backoff.IsZero() {
		return backoff
	}
	if ci := workflowCIBottleneck(snapshot); !ci.IsZero() {
		return ci
	}
	if len(snapshot.Running) > 0 {
		var seconds int64
		var issue telemetry.Issue
		for _, running := range snapshot.Running {
			runtimeSeconds := int64(running.RuntimeSeconds)
			if runtimeSeconds > seconds {
				seconds = runtimeSeconds
				issue = running.Issue
			}
		}
		return telemetry.WorkflowBottleneck{
			Kind:       "ai_active",
			Label:      "AI active",
			Detail:     "agent sessions currently running",
			ProjectID:  issue.ProjectID,
			IssueID:    issue.ID,
			Identifier: issue.Identifier,
			Seconds:    seconds,
			Count:      len(snapshot.Running),
		}
	}
	if mergeQueue := workflowMergeQueueBottleneck(snapshot); !mergeQueue.IsZero() {
		return mergeQueue
	}
	oldest := workflowOldestCards(snapshot)
	if len(oldest) == 0 {
		return telemetry.WorkflowBottleneck{}
	}
	card := oldest[0]
	return telemetry.WorkflowBottleneck{
		Kind:       "lane_age",
		Label:      "Oldest lane",
		Detail:     strings.TrimSpace(card.State),
		ProjectID:  card.ProjectID,
		IssueID:    card.IssueID,
		Identifier: card.Identifier,
		Seconds:    card.AgeSeconds,
		Count:      len(oldest),
	}
}

func workflowRESTBackoffBottleneck(limits *telemetry.RateLimits, now time.Time) telemetry.WorkflowBottleneck {
	if limits == nil || limits.RESTUsage == nil || limits.RESTUsage.BackoffUntil == nil {
		return telemetry.WorkflowBottleneck{}
	}
	until := *limits.RESTUsage.BackoffUntil
	if !until.After(now) {
		return telemetry.WorkflowBottleneck{}
	}
	return telemetry.WorkflowBottleneck{
		Kind:    "rate_limited",
		Label:   "Rate limited",
		Detail:  workflowRESTBackoffFamily(limits.RESTUsage.Contributors),
		Seconds: int64(until.Sub(now) / time.Second),
		Until:   &until,
	}
}

func workflowRESTBackoffFamily(contributors []telemetry.RESTUsageContributor) string {
	for _, contributor := range contributors {
		if contributor.RateLimited && strings.TrimSpace(contributor.EndpointFamily) != "" {
			return contributor.EndpointFamily
		}
	}
	return "GitHub REST backoff"
}

func workflowCIBottleneck(snapshot telemetry.Snapshot) telemetry.WorkflowBottleneck {
	var selected telemetry.Issue
	var seconds int64
	count := 0
	visit := func(issue telemetry.Issue) {
		if issue.PullRequest == nil || !workflowCIWaiting(issue.PullRequest) {
			return
		}
		count++
		wait := issue.PullRequest.CIQueueSeconds + issue.PullRequest.CIDurationSeconds
		if wait < issue.CurrentLaneAgeSeconds {
			wait = issue.CurrentLaneAgeSeconds
		}
		if wait > seconds {
			seconds = wait
			selected = issue
		}
	}
	for _, issue := range snapshot.Pipeline {
		visit(issue)
	}
	for _, issue := range snapshot.BoardIssues {
		visit(issue)
	}
	if count == 0 {
		return telemetry.WorkflowBottleneck{}
	}
	return telemetry.WorkflowBottleneck{
		Kind:       "ci_wait",
		Label:      "CI wait",
		Detail:     "pull requests waiting on checks",
		ProjectID:  selected.ProjectID,
		IssueID:    selected.ID,
		Identifier: selected.Identifier,
		Seconds:    seconds,
		Count:      count,
	}
}

func workflowCIWaiting(pr *telemetry.PullRequest) bool {
	status := strings.ToLower(strings.TrimSpace(pr.CIStatus))
	if status == "" {
		return len(pr.RunningChecks) > 0
	}
	switch status {
	case "pending", "queued", "in_progress", "waiting", "expected":
		return true
	default:
		return false
	}
}

func workflowMergeQueueBottleneck(snapshot telemetry.Snapshot) telemetry.WorkflowBottleneck {
	var selected telemetry.Issue
	var seconds int64
	count := 0
	visit := func(issue telemetry.Issue) {
		if !strings.EqualFold(strings.TrimSpace(issue.State), "Merging") {
			return
		}
		count++
		if issue.CurrentLaneAgeSeconds > seconds {
			seconds = issue.CurrentLaneAgeSeconds
			selected = issue
		}
	}
	for _, issue := range snapshot.Pipeline {
		visit(issue)
	}
	for _, issue := range snapshot.BoardIssues {
		visit(issue)
	}
	if count == 0 {
		return telemetry.WorkflowBottleneck{}
	}
	return telemetry.WorkflowBottleneck{
		Kind:       "merge_queue",
		Label:      "Merge queue",
		Detail:     "issues waiting or actively merging",
		ProjectID:  selected.ProjectID,
		IssueID:    selected.ID,
		Identifier: selected.Identifier,
		Seconds:    seconds,
		Count:      count,
	}
}

func workflowIssueBottleneckKey(issue telemetry.Issue) string {
	if issue.PullRequest != nil && workflowCIWaiting(issue.PullRequest) {
		return "ci_wait"
	}
	if strings.EqualFold(strings.TrimSpace(issue.State), "Merging") {
		return "merge_queue"
	}
	if issue.CurrentLaneAgeSeconds > 0 {
		return "lane_age"
	}
	return ""
}
