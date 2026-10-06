package web

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/coordination"
	"github.com/digitaldrywood/detent/internal/efficiency"
	"github.com/digitaldrywood/detent/internal/store"
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

func TestPopulateDailyDigestTrackerUsesDurableShippedProjects(t *testing.T) {
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

	populateDailyDigestTracker(&day, snapshot, map[string]string{"detent": "Detent"}, map[string]int64{"detent": 42})

	if day.IssuesFiled != 1 || day.IssuesShipped != 0 || day.ReleasesTagged != 0 {
		t.Fatalf("tracker totals = %#v, want snapshot filed/shipped/releases 1/0/0", day)
	}
	if len(day.Projects) != 1 || day.Projects[0].Name != "Detent" || day.Projects[0].Filed != 1 || day.Projects[0].Shipped != 42 || day.Projects[0].Releases != 0 {
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

func TestEfficiencyReportRangeUsesInclusiveToDate(t *testing.T) {
	t.Parallel()

	day := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		from     time.Time
		to       time.Time
		wantFrom time.Time
		wantTo   time.Time
	}{
		{name: "same day", from: day, to: day, wantFrom: day, wantTo: day.AddDate(0, 0, 1)},
		{name: "open end", from: day, wantFrom: day},
		{name: "to only", to: day, wantTo: day.AddDate(0, 0, 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			from, to := efficiencyReportRange(tt.from, tt.to)
			if !from.Equal(tt.wantFrom) || !to.Equal(tt.wantTo) {
				t.Fatalf("efficiencyReportRange() = %s, %s, want %s, %s", from, to, tt.wantFrom, tt.wantTo)
			}
		})
	}
}

// Replays the September 30 audit: 42 verified receipts, only 13 visible Done cards.
func TestDailyDigestDurableShippedCohort(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	ctx := context.Background()
	location, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	for _, date := range []struct {
		name  string
		month time.Month
		day   int
	}{
		{"September audit", time.September, 30}, {"spring DST", time.March, 8}, {"fall DST", time.November, 1},
	} {
		t.Run(date.name, func(t *testing.T) {
			from := time.Date(2026, date.month, date.day, 0, 0, 0, 0, location)
			now := from.Add(14 * time.Hour)
			path := filepath.Join(t.TempDir(), "digest.db")
			backend, err := store.Open(ctx, store.Config{Backend: store.BackendSQLite, Path: path})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := backend.Close(); err != nil {
					t.Error(err)
				}
			}()
			ledger := backend.(store.LaneLedgerStore)
			write := func(id, reason, result string, at time.Time) {
				t.Helper()
				row, err := ledger.PrepareLaneWrite(ctx, "detent", coordination.LaneWrite{Issue: id, From: "Merging", To: "Done", Reason: reason, WrittenAt: at})
				if err != nil {
					t.Fatal(err)
				}
				if err := ledger.ResolveLaneWrite(ctx, row.FenceToken, result); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := telemetry.Snapshot{}
			for i := range 42 {
				id := fmt.Sprintf("issue-%d", i)
				completed := from.Add(time.Duration(i) * time.Minute)
				reason := "merge_worker_programmatic_merge"
				if i == 0 {
					reason = "closed_completed_running_done"
				}
				write(id, reason, "applied", completed)
				write(id, reason, "applied", completed.Add(time.Hour))
				if i == 0 {
					started := completed.Add(-615282 * time.Second)
					if _, err := backend.StartWorkAttempt(ctx, store.WorkAttemptStart{ProjectID: "detent", IssueID: id, WorkerType: "agent", Lane: "In Progress", AttemptNumber: 1, StartedAt: started}); err != nil {
						t.Fatal(err)
					}
					// A minute of measured lane history; the rest is an attribution gap.
					if _, err := backend.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{ProjectID: "detent", IssueID: id, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "In Progress", Status: "exited", StartedAt: started, FinishedAt: started.Add(time.Minute)}); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := backend.CompleteEfficiencyReceipt(ctx, efficiency.Completion{ProjectID: "detent", IssueID: id, CompletedAt: completed}); err != nil {
					t.Fatal(err)
				}
				if i < 13 {
					snapshot.BoardIssues = append(snapshot.BoardIssues, telemetry.Issue{ID: id, ProjectID: "detent", State: "Done", StageUpdatedAt: &completed})
				}
			}
			for _, tt := range []struct {
				id, reason, result string
				at                 time.Time
			}{
				{"import", "closed_completed_observed_done", "applied", now},
				{"abandoned", "issue_closed_not_planned", "applied", now},
				{"failed", "pull_request_merged", "failed", now},
				{"prior", "pull_request_merged", "applied", from.Add(-time.Second)},
				{"next", "pull_request_merged", "applied", from.AddDate(0, 0, 1)},
			} {
				write(tt.id, tt.reason, tt.result, tt.at)
				if _, err := backend.CompleteEfficiencyReceipt(ctx, efficiency.Completion{ProjectID: "detent", IssueID: tt.id, CompletedAt: tt.at}); err != nil {
					t.Fatal(err)
				}
			}
			write("prior", "pull_request_merged", "applied", now)
			// A successful finalized receipt alone is not shipping evidence.
			if _, err := backend.CompleteEfficiencyReceipt(ctx, efficiency.Completion{ProjectID: "detent", IssueID: "success-only", CompletedAt: now}); err != nil {
				t.Fatal(err)
			}
			for _, pruned := range []bool{false, true} {
				if pruned {
					snapshot = telemetry.Snapshot{}
					if err := backend.Close(); err != nil {
						t.Fatal(err)
					}
					backend, err = store.Open(ctx, store.Config{Backend: store.BackendSQLite, Path: path})
					if err != nil {
						t.Fatal(err)
					}
				}
				server := &Server{store: backend}
				digest, err := server.dailyDigestData(ctx, snapshot, nil, location, now)
				if err != nil {
					t.Fatal(err)
				}
				day := digest.Days[len(digest.Days)-1]
				if day.IssuesShipped != 42 || day.Efficiency.Issues != 42 {
					t.Fatalf("pruned=%v shipped/cohort=%d/%d, want 42/42", pruned, day.IssuesShipped, day.Efficiency.Issues)
				}
				if day.Efficiency.Dwell.WorkingSeconds != 60 || day.UnknownDwellSeconds != 615222 {
					t.Fatalf("dwell=%#v unknown=%d, want recorded minute plus lifetime gap", day.Efficiency.Dwell, day.UnknownDwellSeconds)
				}
			}

		})
	}
}
