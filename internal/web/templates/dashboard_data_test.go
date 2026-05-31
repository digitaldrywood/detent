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

func completedAt(completed time.Time) telemetry.Completed {
	return telemetry.Completed{CompletedAt: completed}
}
