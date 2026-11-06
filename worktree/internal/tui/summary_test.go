package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestModelExitSummary(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 12, 20, 30, 45, 0, time.UTC)
	busy := busySnapshot()
	busy.DashboardURL = "\x1b[31mhttp://localhost:4101\x1b[0m"
	busy.Blocked[0].Error = "\x1b[31mdependency unavailable\x1b[0m"
	tests := []struct {
		name    string
		model   Model
		want    []string
		notWant []string
	}{
		{
			name: "empty snapshot",
			model: Model{
				now:     func() time.Time { return now },
				logPath: "/var/log/detent.log",
			},
			want: []string{
				"Timestamp: 2026-07-12T20:30:45Z",
				"Counts: ● 0 running | ◐ 0 queued | ✗ 0 blocked | ✓ 0 completed",
				"Completed:\n  none",
				"Blocked:\n  none",
				"Dashboard: http://localhost:4000",
				"Logs: /var/log/detent.log",
			},
		},
		{
			name: "busy snapshot",
			model: Model{
				snapshot: busy,
				now:      func() time.Time { return now },
				logPath:  "\x1b[2m/var/log/detent.log\x1b[0m",
			},
			want: []string{
				"Counts: ● 7 running | ◐ 5 queued | ✗ 5 blocked | ✓ 10 completed",
				"✓ digitaldrywood/detent#450 — Done",
				"✓ digitaldrywood/detent#446 — Done",
				"✗ digitaldrywood/detent#460 — dependency unavailable",
				"Dashboard: http://localhost:4101",
				"Logs: /var/log/detent.log",
			},
			notWant: []string{"digitaldrywood/detent#445"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.model.ExitSummary()
			if ansi.Strip(got) != got {
				t.Fatalf("ExitSummary() contains ANSI escapes: %q", got)
			}
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("ExitSummary() missing %q:\n%s", want, got)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(got, notWant) {
					t.Fatalf("ExitSummary() unexpectedly contains %q:\n%s", notWant, got)
				}
			}
		})
	}
}

func TestSummaryTextRemovesControlStyling(t *testing.T) {
	t.Parallel()

	if got := summaryText("\x1b[31mblocked\x1b[0m\nnow"); got != "blocked now" {
		t.Fatalf("summaryText() = %q, want %q", got, "blocked now")
	}
}
