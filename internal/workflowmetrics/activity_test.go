package workflowmetrics

import (
	"testing"
	"time"
)

func TestActivityBreakdownPartitionsObservedWallTime(t *testing.T) {
	at := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	span := func(id, parent, kind string, start, end int) ActivitySpan {
		return ActivitySpan{ID: id, ParentID: parent, Kind: kind, StartedAt: at.Add(time.Duration(start) * time.Second), FinishedAt: at.Add(time.Duration(end) * time.Second), Outcome: "completed"}
	}
	for _, tt := range []struct {
		name                          string
		spans                         []ActivitySpan
		observed, concurrent, waiting float64
	}{
		{"overlap", []ActivitySpan{span("a", "turn", "local_validation", 2, 8), span("b", "turn", "review", 4, 10)}, 8, 4, 0},
		{"nested wait", []ActivitySpan{span("a", "turn", "local_validation", 2, 10), span("wait", "a", "waiting", 3, 6)}, 8, 0, 3},
		{"missing completion", []ActivitySpan{{ID: "a", Kind: "local_validation", StartedAt: at.Add(time.Second), Outcome: "running"}}, 0, 0, 0},
		{"out of order", []ActivitySpan{span("a", "turn", "review", 8, 2)}, 0, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			profile := ActivityProfile{StartedAt: at, AsOf: at.Add(12 * time.Second), Spans: tt.spans}
			b := profile.Breakdown()
			if b.ObservedSeconds != tt.observed || b.UnknownSeconds != 12-tt.observed || b.ConcurrentSeconds != tt.concurrent || b.ByKind["waiting"] != tt.waiting {
				t.Fatalf("breakdown = %+v", b)
			}
			gapSeconds := 0.0
			for _, gap := range profile.Gaps() {
				gapSeconds += gap.FinishedAt.Sub(gap.StartedAt).Seconds()
			}
			if gapSeconds != b.UnknownSeconds {
				t.Fatalf("gaps=%v, unknown=%v", gapSeconds, b.UnknownSeconds)
			}
			sum := 0.0
			for _, v := range b.ByKind {
				sum += v
			}
			if sum != 12 {
				t.Fatalf("partition sums to %v", sum)
			}
		})
	}
}
