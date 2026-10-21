package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestValidationHistoryReport(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		empty     bool
		local     bool
		ambiguous bool
	}{
		{"overlap midnight incomplete and unattributed", false, false, false}, {"missing observations", true, false, false}, {"local attempts without worker host", false, true, false}, {"ambiguous attempts", false, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`CREATE TABLE validation_events(payload TEXT);
CREATE TABLE validation_report_params(host TEXT, worker_host TEXT, day TEXT);
INSERT INTO validation_report_params VALUES('host','worker','2020-01-02');
CREATE TABLE work_attempts(id INTEGER, identifier TEXT, started_at TEXT, completed_at TEXT, heartbeat_at TEXT, worker_host TEXT, worker_type TEXT);
INSERT INTO work_attempts VALUES
(1,'repo#1','2020-01-02T00:00:00Z','2020-01-02T00:02:00Z',NULL,'worker','agent'),
(2,'repo#2','2020-01-02T00:00:00Z','2020-01-02T00:02:00Z',NULL,'worker','agent'),
(3,'repo#3','2020-01-01T23:59:00Z','2020-01-02T00:00:30Z',NULL,'worker','agent'),
(4,'repo#1','2020-01-02T00:00:00Z','2020-01-02T00:02:00Z',NULL,'other','agent');`)
			if tt.ambiguous {
				exec("INSERT INTO work_attempts SELECT 5, identifier, started_at, completed_at, heartbeat_at, worker_host, worker_type FROM work_attempts WHERE id = 1")
			}
			if tt.local {
				exec("UPDATE work_attempts SET worker_host = NULL WHERE worker_host = 'worker'")
				exec("UPDATE validation_report_params SET worker_host = ''")
			}
			if !tt.empty {
				base := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)
				for _, run := range []struct {
					id, issue, phase string
					start, wait      int
				}{
					{"a", "repo#1", "passed", 10, 40}, {"b", "repo#1", "failed", 30, 50},
					{"c", "repo#2", "wait_failed", 60, 40}, {"d", "repo#3", "passed", -10, 20},
					{"e", "repo#1", "queued", 110, 5}, {"f", "unknown", "passed", 200, 10},
				} {
					event := validationEvent{RunID: run.id, Host: "host", Identifier: run.issue, Phase: run.phase, StartedAt: base.Add(time.Duration(run.start) * time.Second), At: base.Add(time.Duration(run.start+run.wait) * time.Second), WaitSeconds: float64(run.wait), HoldSeconds: 2, QueueSize: 8}
					data, err := json.Marshal(event)
					if err != nil {
						t.Fatal(err)
					}
					exec("INSERT INTO validation_events VALUES(?)", string(data))
					// Re-imported records must not multiply durations.
					exec("INSERT INTO validation_events VALUES(?)", string(data))
				}
				exec(`INSERT INTO validation_events VALUES('broken'), ('{"phase":"passed"}')`)
			}
			script, err := os.ReadFile("../../scripts/validation-report.sql")
			if err != nil {
				t.Fatal(err)
			}
			sections := strings.Split(string(script), "-- Output ")
			exec(sections[0])
			var host, day string
			var malformed, legacy, runs, incomplete, unattributed int
			var dispatched, waiting float64
			var fraction sql.NullFloat64
			first := sections[1][strings.Index(sections[1], "SELECT "):]
			if err := db.QueryRowContext(t.Context(), first).Scan(&host, &day, &malformed, &legacy, &runs, &incomplete, &unattributed, &dispatched, &waiting, &fraction); err != nil {
				t.Fatal(err)
			}
			wantDispatched, wantWaiting, wantUnattributed := 270.0, 125.0, 1
			if tt.ambiguous {
				wantDispatched, wantWaiting, wantUnattributed = 390, 50, 4
			}
			if dispatched != wantDispatched {
				t.Fatalf("dispatched = %v", dispatched)
			}
			if tt.empty {
				if runs != 0 || waiting != 0 || fraction.Valid {
					t.Fatalf("empty report: runs=%d wait=%v", runs, waiting)
				}
			} else if malformed != 1 || legacy != 1 || runs != 6 || incomplete != 1 || unattributed != wantUnattributed || waiting != wantWaiting || !fraction.Valid || fraction.Float64 != wantWaiting/wantDispatched {
				t.Fatalf("report: malformed=%d legacy=%d runs=%d incomplete=%d unattributed=%d wait=%v fraction=%v", malformed, legacy, runs, incomplete, unattributed, waiting, fraction)
			}
			second := sections[2][strings.Index(sections[2], "WITH ranked"):]
			var busy, mean, median, p90, maxDepth, observedDepth sql.NullFloat64
			if err := db.QueryRowContext(t.Context(), second).Scan(&busy, &mean, &median, &p90, &maxDepth, &observedDepth); err != nil {
				t.Fatal(err)
			}
			if tt.empty && maxDepth.Valid || !tt.empty && (maxDepth.Float64 != 2 || observedDepth.Float64 != 8 || busy.Float64 != 115 || mean.Float64 != 155.0/115 || median.Float64 != 1 || p90.Float64 != 2) {
				t.Fatalf("queue: busy=%v mean=%v median=%v p90=%v max=%v", busy, mean, median, p90, maxDepth)
			}
			third := sections[3][strings.Index(sections[3], "SELECT "):]
			rows, err := db.QueryContext(t.Context(), third)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			count := 0
			for rows.Next() {
				count++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if count != runs {
				t.Fatalf("detail count = %d, runs = %d", count, runs)
			}
		})
	}
}
