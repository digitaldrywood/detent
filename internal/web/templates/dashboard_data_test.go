package templates

import (
	"testing"
	"time"

	"github.com/digitaldrywood/symphony/internal/telemetry"
)

func TestThroughputRateFormatsRollingTokenTPS(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		snapshot telemetry.Snapshot
		want     string
	}{
		{
			name: "empty throughput",
			want: "0 tps",
		},
		{
			name: "integer tps",
			snapshot: telemetry.Snapshot{
				Throughput: telemetry.TokenThroughput{TokensPerSecond: 42},
			},
			want: "42 tps",
		},
		{
			name: "decimal tps",
			snapshot: telemetry.Snapshot{
				Throughput: telemetry.TokenThroughput{TokensPerSecond: 7.25},
			},
			want: "7.3 tps",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := throughputRate(tt.snapshot)
			if got != tt.want {
				t.Fatalf("throughputRate() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestThroughputTrendPoints(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 15, 0, 30, 0, time.UTC)
	points := throughputTrendPoints(telemetry.Snapshot{
		TokenTrend: []telemetry.TokenTrendPoint{
			{At: now.Add(-8 * time.Minute), Total: 120},
			{At: now.Add(-2 * time.Minute), Total: 480},
			{At: now.Add(-30 * time.Second), Total: 570},
			{At: now.Add(10 * time.Second), Total: 690},
		},
	})

	if len(points) != 3 {
		t.Fatalf("throughputTrendPoints() len = %d, want 3", len(points))
	}

	wantValues := map[string]float64{
		"14:58:30": 1,
		"15:00":    1,
		"15:00:40": 3,
	}
	for _, point := range points {
		want := wantValues[point.Label]
		if point.Value != want {
			t.Fatalf("point %s = %v, want %v; points = %#v", point.Label, point.Value, want, points)
		}
	}
}

func TestRunningActivityRowsUseRecentEventsNewestFirst(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 15, 0, 5, 0, time.UTC)
	row := telemetry.Running{
		RecentEvents: []telemetry.ActivityEvent{
			{At: now.Add(-5 * time.Second), Event: "process_started", Message: "process 4242 started"},
			{At: now.Add(-4 * time.Second), Event: "turn_started", Message: "turn started"},
			{At: now.Add(-3 * time.Second), Event: "agent_message_delta", Message: "editing dashboard"},
			{At: now.Add(-2 * time.Second), Event: "token_usage", Message: "tokens updated"},
			{At: now.Add(-time.Second), Event: "rate_limits", Message: "rate snapshot"},
			{At: now, Event: "turn_completed", Message: "turn completed"},
		},
	}

	rows := runningActivityRows(row)
	if len(rows) != 5 {
		t.Fatalf("runningActivityRows() len = %d, want 5", len(rows))
	}

	want := []struct {
		event   string
		message string
		at      string
	}{
		{event: "turn_completed", message: "turn completed", at: "15:00:05 UTC"},
		{event: "rate_limits", message: "rate snapshot", at: "15:00:04 UTC"},
		{event: "token_usage", message: "tokens updated", at: "15:00:03 UTC"},
		{event: "agent_message_delta", message: "editing dashboard", at: "15:00:02 UTC"},
		{event: "turn_started", message: "turn started", at: "15:00:01 UTC"},
	}
	for i, wantRow := range want {
		if rows[i].Event != wantRow.event || rows[i].Message != wantRow.message || rows[i].At != wantRow.at {
			t.Fatalf("row %d = %#v, want %#v", i, rows[i], wantRow)
		}
	}
}

func TestRunningActivityRowsFallBackToLatestEvent(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 5, 31, 15, 3, 4, 0, time.UTC)
	rows := runningActivityRows(telemetry.Running{
		LastEventAt: &at,
		LastEvent:   "agent_message_delta",
		LastMessage: "working through review feedback",
	})

	if len(rows) != 1 {
		t.Fatalf("runningActivityRows() len = %d, want 1", len(rows))
	}
	if rows[0].At != "15:03:04 UTC" || rows[0].Event != "agent_message_delta" || rows[0].Message != "working through review feedback" {
		t.Fatalf("runningActivityRows()[0] = %#v", rows[0])
	}
}

func TestAgentTimelineRows(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	snapshot := telemetry.Snapshot{
		GeneratedAt: now,
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:         "issue-2",
					Identifier: "DD-2",
					Title:      "Second running issue",
					State:      "Merging",
				},
				StartedAt: now.Add(-4 * time.Minute),
			},
			{
				Issue: telemetry.Issue{
					ID:         "issue-1",
					Identifier: "DD-1",
					Title:      "First running issue",
					State:      "In Progress",
				},
				StartedAt: now.Add(-8 * time.Minute),
			},
		},
		Completed: []telemetry.Completed{
			{
				Issue: telemetry.Issue{
					ID:         "issue-3",
					Identifier: "DD-3",
					Title:      "Completed issue",
				},
				StartedAt:   now.Add(-10 * time.Minute),
				CompletedAt: now.Add(-2 * time.Minute),
				FinalState:  "completed",
			},
			{
				Issue: telemetry.Issue{
					ID:         "issue-4",
					Identifier: "DD-4",
				},
				StartedAt:   now.Add(-12 * time.Minute),
				CompletedAt: now.Add(-11 * time.Minute),
				FinalState:  "failed",
			},
		},
	}

	rows := agentTimelineRows(snapshot)
	if len(rows) != 4 {
		t.Fatalf("agentTimelineRows() len = %d, want 4", len(rows))
	}

	wantOrder := []string{"DD-4", "DD-3", "DD-1", "DD-2"}
	for i, want := range wantOrder {
		if rows[i].Identifier != want {
			t.Fatalf("row %d identifier = %q, want %q; rows = %#v", i, rows[i].Identifier, want, rows)
		}
	}

	tests := []struct {
		name      string
		row       agentTimelineRow
		wantState string
		wantStart string
		wantEnd   string
		wantWidth string
		wantClass string
	}{
		{
			name:      "failed completed row",
			row:       rows[0],
			wantState: "failed",
			wantStart: "0.00%",
			wantEnd:   "8.33%",
			wantWidth: "8.33%",
			wantClass: "bg-danger",
		},
		{
			name:      "completed row",
			row:       rows[1],
			wantState: "completed",
			wantStart: "16.67%",
			wantEnd:   "83.33%",
			wantWidth: "66.67%",
			wantClass: "bg-success",
		},
		{
			name:      "running row",
			row:       rows[3],
			wantState: "Merging",
			wantStart: "66.67%",
			wantEnd:   "100.00%",
			wantWidth: "33.33%",
			wantClass: "bg-accent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.row.State != tt.wantState {
				t.Fatalf("State = %q, want %q", tt.row.State, tt.wantState)
			}
			if tt.row.StartPercent != tt.wantStart {
				t.Fatalf("StartPercent = %q, want %q", tt.row.StartPercent, tt.wantStart)
			}
			if tt.row.EndPercent != tt.wantEnd {
				t.Fatalf("EndPercent = %q, want %q", tt.row.EndPercent, tt.wantEnd)
			}
			if len(tt.row.Segments) != 1 {
				t.Fatalf("Segments len = %d, want 1", len(tt.row.Segments))
			}
			segment := tt.row.Segments[0]
			if segment.Width != tt.wantWidth {
				t.Fatalf("segment.Width = %q, want %q", segment.Width, tt.wantWidth)
			}
			if segment.Class != tt.wantClass {
				t.Fatalf("segment.Class = %q, want %q", segment.Class, tt.wantClass)
			}
		})
	}
}
