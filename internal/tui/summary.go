package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

const exitSummaryCompletedLimit = 5

func (m Model) ExitSummary() string {
	now := m.now
	if now == nil {
		now = time.Now
	}
	snapshot := m.snapshot
	lines := []string{
		"Detent exit summary",
		"Timestamp: " + formatTimestamp(now()),
		fmt.Sprintf(
			"Counts: ● %d running | ◐ %d queued | ✗ %d blocked | ✓ %d completed",
			countOrLen(snapshot.Counts.Running, len(snapshot.Running)),
			countOrLen(snapshot.Counts.Queue, len(snapshot.Queue)),
			countOrLen(snapshot.Counts.Blocked, len(snapshot.Blocked)),
			countOrLen(snapshot.Counts.Completed, len(snapshot.Completed)),
		),
	}

	completed := append([]telemetry.Completed(nil), snapshot.Completed...)
	sort.SliceStable(completed, func(i int, j int) bool {
		return completed[i].CompletedAt.After(completed[j].CompletedAt)
	})
	lines = append(lines, "Completed:")
	if len(completed) == 0 {
		lines = append(lines, "  none")
	} else {
		for _, row := range completed[:min(len(completed), exitSummaryCompletedLimit)] {
			state := defaultString(row.FinalState, row.State)
			if state == "" {
				state = "completed"
			}
			lines = append(lines, fmt.Sprintf("  ✓ %s — %s", summaryText(issueLabel(row.Issue)), summaryText(state)))
		}
	}

	lines = append(lines, "Blocked:")
	if len(snapshot.Blocked) == 0 {
		lines = append(lines, "  none")
	} else {
		blocked := append([]telemetry.Blocked(nil), snapshot.Blocked...)
		sort.SliceStable(blocked, func(i int, j int) bool {
			return issueLabel(blocked[i].Issue) < issueLabel(blocked[j].Issue)
		})
		for _, row := range blocked {
			reason := row.Error
			if strings.TrimSpace(reason) == "" {
				reason = row.LastMessage
			}
			if strings.TrimSpace(reason) == "" {
				reason = "blocked"
			}
			lines = append(lines, fmt.Sprintf("  ✗ %s — %s", summaryText(issueLabel(row.Issue)), summaryText(reason)))
		}
	}

	lines = append(lines,
		"Dashboard: "+summaryText(formatDashboardURL(snapshot)),
		"Logs: "+summaryText(defaultString(m.logPath, "n/a")),
	)
	return strings.Join(lines, "\n")
}

func summaryText(value string) string {
	return cleanInline(ansi.Strip(value))
}
