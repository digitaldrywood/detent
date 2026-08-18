package cli

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
)

func TestCheckDoctorHistoricalThroughputReportsRecordedFloorAndRedispatch(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 18, 18, 0, 0, 0, time.UTC)
	dbPath := filepath.Join(t.TempDir(), "detent.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE work_attempts (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  issue_id TEXT,
  identifier TEXT,
  issue_url TEXT,
  started_at TEXT NOT NULL,
  lease_expires_at TEXT,
  heartbeat_at TEXT,
  completed_at TEXT
);
CREATE TABLE scheduler_decisions (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  result TEXT NOT NULL,
  wait_reason TEXT,
  decision_at TEXT NOT NULL
);`); err != nil {
		t.Fatalf("create history tables: %v", err)
	}
	for index := range 10 {
		hour := index % 7
		minute := index / 7 * 30
		start := now.Add(-time.Duration(8-hour) * time.Hour).Add(time.Duration(minute) * time.Minute)
		end := start.Add(20 * time.Minute)
		if _, err := db.ExecContext(
			t.Context(),
			`INSERT INTO work_attempts (project_id, issue_id, identifier, started_at, completed_at) VALUES (?, ?, ?, ?, ?)`,
			"alpha", "issue-1886", "alpha#1886", start.Format(time.RFC3339), end.Format(time.RFC3339),
		); err != nil {
			t.Fatalf("insert attempt %d: %v", index, err)
		}
	}
	for hour := range 7 {
		at := now.Add(-time.Duration(8-hour) * time.Hour).Add(25 * time.Minute)
		if _, err := db.ExecContext(
			t.Context(),
			`INSERT INTO scheduler_decisions (project_id, result, wait_reason, decision_at) VALUES (?, 'skipped', ?, ?)`,
			"alpha", doctorRateWindowBackpressureWaitName, at.Format(time.RFC3339),
		); err != nil {
			t.Fatalf("insert decision %d: %v", hour, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close() error = %v", err)
	}

	checks := checkDoctorHistoricalThroughput(t.Context(), "alpha", dbPath, 5, doctorDeps{
		openSQLiteReadOnly: openDoctorSQLiteReadOnly,
		now:                func() time.Time { return now },
	})
	if len(checks) != 2 {
		t.Fatalf("checks len = %d, want 2", len(checks))
	}
	concurrency := checks[0]
	if concurrency.Status != doctorWarn || len(concurrency.ConcurrencyHistory) != 2 {
		t.Fatalf("concurrency check = %#v, want two warnings", concurrency)
	}
	if !strings.Contains(concurrency.Detail, "effective ceiling at 1 for 7 consecutive hours") ||
		!strings.Contains(concurrency.Detail, "configured concurrency 5 was unreachable") {
		t.Fatalf("concurrency detail = %q", concurrency.Detail)
	}
	redispatch := checks[1]
	if redispatch.Status != doctorWarn || len(redispatch.RedispatchLoops) != 1 || redispatch.RedispatchLoops[0].Dispatches != 10 {
		t.Fatalf("redispatch check = %#v, want alpha#1886 with 10 dispatches", redispatch)
	}
}

func TestDoctorConcurrencyCheckRequiresRecordedBackpressureSignature(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 18, 18, 0, 0, 0, time.UTC)
	buckets := make([]store.ConcurrencyBucket, 0, 8)
	for index := range 8 {
		start := now.Add(-time.Duration(8-index) * time.Hour)
		buckets = append(buckets, store.ConcurrencyBucket{Start: start, End: start.Add(time.Hour), Median: 1, P90: 1, Max: 1, ActiveSeconds: 3600})
	}
	report := store.ConcurrencyReport{
		From:   now.Add(-8 * time.Hour),
		To:     now,
		Bucket: time.Hour,
		Series: []store.ConcurrencySeries{{ProjectID: "alpha", Buckets: buckets}},
	}

	check := doctorConcurrencyCheck("historical concurrency", "alpha", 5, report, doctorDispatchPressure{})
	if check.Status != doctorOK || len(check.ConcurrencyHistory) != 0 {
		t.Fatalf("check = %#v, want no causal attribution without recorded scheduler pressure", check)
	}
}
