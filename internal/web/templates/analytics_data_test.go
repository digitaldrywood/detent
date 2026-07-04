package templates

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

func analyticsTestData() DashboardData {
	now := time.Date(2026, 7, 4, 16, 41, 52, 0, time.UTC)
	heartbeat := now.Add(-2 * time.Second)
	return DashboardData{
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			WorkAttempts: []telemetry.WorkAttempt{
				{
					AttemptID:     71,
					ProjectID:     "gopher-ai",
					Identifier:    "gopherguides/gopher-ai#185",
					Status:        "running",
					Phase:         "implementation",
					StatusMessage: "session started",
					WorkerHost:    "corys-mac-studio",
					StartedAt:     now.Add(-time.Minute),
					HeartbeatAt:   &heartbeat,
				},
				{
					AttemptID:  72,
					ProjectID:  "detent",
					Identifier: "digitaldrywood/detent#92",
					Status:     "running",
					Stale:      true,
					StartedAt:  now.Add(-2 * time.Hour),
				},
			},
			Events: []telemetry.ActivityEvent{
				{At: now.Add(-30 * time.Second), Event: "workspace_reap_succeeded", Message: "workspace reaped reason=cancelled"},
			},
		},
	}
}

func TestAnalyticsViewMergesAndSortsNewestFirst(t *testing.T) {
	view := analyticsViewFromDashboard(analyticsTestData())
	if len(view.Rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(view.Rows))
	}
	for i := 1; i < len(view.Rows); i++ {
		if view.Rows[i].At.After(view.Rows[i-1].At) {
			t.Fatalf("rows not newest-first: %v after %v", view.Rows[i].At, view.Rows[i-1].At)
		}
	}
	if view.Summary != "3 events in the current snapshot." {
		t.Fatalf("summary = %q", view.Summary)
	}
}

func TestAnalyticsAttemptDecisions(t *testing.T) {
	tests := []struct {
		name     string
		attempt  telemetry.WorkAttempt
		wantKind primitives.Kind
		wantText string
	}{
		{name: "stale wins", attempt: telemetry.WorkAttempt{Status: "running", Stale: true}, wantKind: primitives.KindWarn, wantText: "stale"},
		{name: "error is failed", attempt: telemetry.WorkAttempt{ErrorMessage: "boom"}, wantKind: primitives.KindErr, wantText: "failed"},
		{name: "terminal done completed", attempt: telemetry.WorkAttempt{TerminalState: "Done"}, wantKind: primitives.KindOK, wantText: "completed"},
		{name: "running dispatched", attempt: telemetry.WorkAttempt{Status: "running"}, wantKind: primitives.KindOK, wantText: "dispatched"},
		{name: "skipped stays neutral", attempt: telemetry.WorkAttempt{Status: "skipped"}, wantKind: primitives.KindNeutral, wantText: "skipped"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, text := analyticsAttemptDecision(tt.attempt)
			if kind != tt.wantKind || text != tt.wantText {
				t.Fatalf("decision = %q %q, want %q %q", kind, text, tt.wantKind, tt.wantText)
			}
		})
	}
}

func TestAnalyticsKindFilter(t *testing.T) {
	data := analyticsTestData()
	data.AnalyticsKind = "activity"
	view := analyticsViewFromDashboard(data)
	if len(view.Rows) != 1 || !view.Filtered {
		t.Fatalf("activity filter rows = %d filtered = %v", len(view.Rows), view.Filtered)
	}
	if view.Rows[0].Event != "workspace reap succeeded" {
		t.Fatalf("activity event label = %q", view.Rows[0].Event)
	}

	data.AnalyticsKind = "attempts"
	view = analyticsViewFromDashboard(data)
	if len(view.Rows) != 2 {
		t.Fatalf("attempts filter rows = %d", len(view.Rows))
	}

	data.AnalyticsKind = "garbage"
	view = analyticsViewFromDashboard(data)
	if view.Filtered || len(view.Rows) != 3 {
		t.Fatalf("unknown filter should show everything, rows = %d", len(view.Rows))
	}
}

func TestAnalyticsAttemptDetailReadsAsSentence(t *testing.T) {
	view := analyticsViewFromDashboard(analyticsTestData())
	var detail string
	for _, row := range view.Rows {
		if row.Ref == "#185" {
			detail = row.Detail
		}
	}
	for _, want := range []string{"phase implementation", "session started", "on corys-mac-studio"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail %q missing %q", detail, want)
		}
	}
}
