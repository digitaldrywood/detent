package web

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func TestDailyDigestWindowsFollowClientTimezoneDST(t *testing.T) {
	t.Parallel()

	location, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	tests := []struct {
		name       string
		now        time.Time
		wantDate   string
		wantLength time.Duration
	}{
		{name: "spring forward", now: time.Date(2026, 3, 8, 18, 0, 0, 0, time.UTC), wantDate: "2026-03-08", wantLength: 23 * time.Hour},
		{name: "fall back", now: time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC), wantDate: "2026-11-01", wantLength: 25 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			windows := dailyDigestWindows(tt.now, location, 2)
			got := windows[1]
			if got.Date != tt.wantDate || got.To.Sub(got.From) != tt.wantLength {
				t.Fatalf("window = %#v (%s), want %s lasting %s", got, got.To.Sub(got.From), tt.wantDate, tt.wantLength)
			}
		})
	}
}

func TestPopulateDailyDigestTrackerDeduplicatesProjectsAndReleases(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 7, 10, 5, 0, 0, 0, time.UTC)
	filedAt := from.Add(time.Hour)
	shippedAt := from.Add(2 * time.Hour)
	releasedAt := from.Add(3 * time.Hour)
	issue := telemetry.Issue{ID: "issue-1", ProjectID: "detent", State: "Done", CreatedAt: &filedAt, StageUpdatedAt: &shippedAt}
	release := telemetry.Release{ProjectID: "detent", LastRelease: "v1.2.3", LastReleaseAt: &releasedAt}
	snapshot := telemetry.Snapshot{
		BoardIssues: []telemetry.Issue{issue},
		Pipeline:    []telemetry.Issue{issue},
		Release:     release,
		Releases:    []telemetry.Release{release},
	}
	day := templates.DailyDigestDayData{From: from, To: from.Add(24 * time.Hour)}

	populateDailyDigestTracker(&day, snapshot, map[string]string{"detent": "Detent"})

	if day.IssuesFiled != 1 || day.IssuesShipped != 1 || day.ReleasesTagged != 1 {
		t.Fatalf("tracker totals = %#v, want filed/shipped/releases 1/1/1", day)
	}
	if len(day.Projects) != 1 || day.Projects[0].Name != "Detent" || day.Projects[0].Filed != 1 || day.Projects[0].Shipped != 1 || day.Projects[0].Releases != 1 {
		t.Fatalf("project totals = %#v, want one reconciled Detent row", day.Projects)
	}
}

func TestReportsTimezoneRejectsInvalidNames(t *testing.T) {
	t.Parallel()

	if _, err := reportsTimezone("not/a-zone"); err == nil {
		t.Fatal("reportsTimezone() error = nil, want invalid timezone")
	}
	location, err := reportsTimezone("America/Chicago")
	if err != nil || location.String() != "America/Chicago" {
		t.Fatalf("reportsTimezone() = %v, %v, want America/Chicago", location, err)
	}
}
