package templates

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

func TestHealthReleaseRow(t *testing.T) {
	t.Parallel()

	next := time.Date(2026, time.July, 11, 12, 0, 0, 0, time.UTC)
	row := healthReleaseRow(telemetry.Release{
		ProjectID:        "detent",
		Enabled:          true,
		State:            "release_pending",
		LastRelease:      "v1.2.3",
		UnreleasedMerges: 4,
		NextTriggerAt:    &next,
	})
	if row.Component != "Auto-release · detent" || row.Kind != primitives.KindWarn || row.Status != "release pending" {
		t.Fatalf("healthReleaseRow() = %#v", row)
	}
	if !strings.Contains(row.Detail, "v1.2.3") || !strings.Contains(row.Detail, "4 unreleased merges") {
		t.Fatalf("healthReleaseRow() detail = %q", row.Detail)
	}
}

func TestReportsReleaseView(t *testing.T) {
	t.Parallel()

	next := time.Date(2026, time.July, 11, 12, 0, 0, 0, time.UTC)
	view := reportsReleaseView(ReportsData{ProjectID: "detent", Snapshot: telemetry.Snapshot{Releases: []telemetry.Release{
		{ProjectID: "other", Enabled: true, LastRelease: "v9.0.0"},
		{ProjectID: "detent", Enabled: true, State: "waiting", LastRelease: "v1.2.3", UnreleasedMerges: 3, NextTriggerAt: &next},
	}}})
	if !view.Available || view.Last != "v1.2.3" || view.Merges != "3" || view.Next != localTimeToken(next, LocalDateTime) || view.State != "waiting" {
		t.Fatalf("reportsReleaseView() = %#v", view)
	}
}
