package templates

import (
	"testing"
	"time"

	"github.com/digitaldrywood/symphony/internal/telemetry"
)

func TestCurrentThroughputPerMinute(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		snapshot telemetry.Snapshot
		want     float64
	}{
		{
			name: "empty snapshot",
		},
		{
			name: "rolling five minute window",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				Completed: []telemetry.Completed{
					completedAt(now.Add(-30 * time.Second)),
					completedAt(now.Add(-4 * time.Minute)),
					completedAt(now.Add(-6 * time.Minute)),
					completedAt(now.Add(time.Minute)),
				},
			},
			want: 0.4,
		},
		{
			name: "latest completion anchors missing generated time",
			snapshot: telemetry.Snapshot{
				Completed: []telemetry.Completed{
					completedAt(now.Add(-2 * time.Minute)),
					completedAt(now),
				},
			},
			want: 0.4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := currentThroughputPerMinute(tt.snapshot)
			if got != tt.want {
				t.Fatalf("currentThroughputPerMinute() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestThroughputTrendPoints(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 15, 0, 30, 0, time.UTC)
	points := throughputTrendPoints(telemetry.Snapshot{
		GeneratedAt: now,
		Completed: []telemetry.Completed{
			completedAt(now.Add(-8 * time.Minute)),
			completedAt(now.Add(-2 * time.Minute)),
			completedAt(now.Add(-30 * time.Second)),
			completedAt(now.Add(10 * time.Second)),
		},
	})

	if len(points) != throughputTrendBuckets {
		t.Fatalf("throughputTrendPoints() len = %d, want %d", len(points), throughputTrendBuckets)
	}

	wantValues := map[string]float64{
		"14:52": 1,
		"14:58": 1,
		"15:00": 1,
	}
	for _, point := range points {
		want := wantValues[point.Label]
		if point.Value != want {
			t.Fatalf("point %s = %v, want %v; points = %#v", point.Label, point.Value, want, points)
		}
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

func completedAt(completed time.Time) telemetry.Completed {
	return telemetry.Completed{CompletedAt: completed}
}
