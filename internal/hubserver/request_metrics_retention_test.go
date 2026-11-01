package hubserver

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRequestMetricsRetention(t *testing.T) {
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	s := &Service{config: Config{now: func() time.Time { return now }}}
	for _, organization := range []tracker.OrganizationID{"org_one", "org_two"} {
		t.Run(string(organization), func(t *testing.T) {
			m := testRequestMetrics(t, filepath.Join(t.TempDir(), "metrics.sqlite"), organization)
			s.requestMetrics = m
			cases := []struct {
				table, column string
				retention     time.Duration
				unit          time.Duration
			}{
				{"request_minutes", "minute", 14 * 24 * time.Hour, time.Minute},
				{"request_hours", "hour", 90 * 24 * time.Hour, time.Hour},
				{"request_days", "day", 400 * 24 * time.Hour, 24 * time.Hour},
			}
			for _, test := range cases {
				for _, boundary := range []struct {
					name   string
					offset time.Duration
				}{{"expired", -test.unit}, {"exact", 0}, {"retained", test.unit}} {
					at := now.Add(-test.retention).Add(boundary.offset)
					seedRequestRetentionRow(t, m.db, test.table, at, "GET /"+test.table+"/"+boundary.name, "runner", "example", 2)
				}
			}
			seedRequestRetentionRow(t, m.db, "request_minutes", now.Add(-401*24*time.Hour), "GET /beyond-daily", "runner", "old-example", 2)
			m.record(now.Add(-time.Minute), "GET /current", "browser", "example-user", 200, 3*time.Millisecond)
			if err := m.flush(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			reportBefore, err := m.report(t.Context(), now)
			if err != nil {
				t.Fatal(err)
			}
			historyBefore, err := m.history(t.Context(), now.Add(-90*24*time.Hour), now, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			for _, tick := range []struct {
				name  string
				after time.Duration
			}{{"exact cutoff", 0}, {"fractional cutoff", time.Nanosecond}} {
				t.Run(tick.name, func(t *testing.T) {
					now = now.Add(tick.after)
					if err := s.expireRequestMetrics(t.Context(), s.config.now()); err != nil {
						t.Fatal(err)
					}
					for _, test := range cases {
						for _, boundary := range []struct {
							name string
							want int64
						}{{"expired", 0}, {"exact", 2}, {"retained", 2}} {
							var count int64
							var sum, maximum float64
							query := "SELECT coalesce(sum(count),0),coalesce(sum(sum_ms),0),coalesce(max(max_ms),0) FROM " + test.table + " WHERE route=?"
							if err := m.db.QueryRowContext(t.Context(), query, "GET /"+test.table+"/"+boundary.name).Scan(&count, &sum, &maximum); err != nil {
								t.Fatal(err)
							}
							wantMax := float64(0)
							if boundary.want > 0 {
								wantMax = 3
							}
							if count != boundary.want || sum != float64(boundary.want)*3 || maximum != wantMax {
								t.Errorf("%s %s: count=%d sum=%g max=%g", test.table, boundary.name, count, sum, maximum)
							}
							if boundary.want > 0 {
								var timestamp int64
								if err := m.db.QueryRowContext(t.Context(), "SELECT "+test.column+" FROM "+test.table+" WHERE route=?", "GET /"+test.table+"/"+boundary.name).Scan(&timestamp); err != nil {
									t.Fatal(err)
								}
								if boundary.name == "exact" && timestamp != now.Add(-test.retention).Unix() {
									t.Errorf("%s boundary timestamp=%d", test.table, timestamp)
								}
							}
						}
					}
				})
			}
			reportAfter, err := m.report(t.Context(), reportBefore.To)
			if err != nil || !reflect.DeepEqual(reportBefore, reportAfter) {
				t.Fatalf("current report changed: before=%+v after=%+v err=%v", reportBefore, reportAfter, err)
			}
			historyAfter, err := m.history(t.Context(), historyBefore.From, historyBefore.To, time.Hour)
			if err != nil || !reflect.DeepEqual(historyBefore, historyAfter) {
				t.Fatalf("retained history changed: before=%+v after=%+v err=%v", historyBefore, historyAfter, err)
			}
			var ancient int
			if err := m.db.QueryRowContext(t.Context(), `SELECT
(SELECT count(*) FROM request_minutes WHERE route='GET /beyond-daily')+
(SELECT count(*) FROM request_days WHERE route='GET /beyond-daily')`).Scan(&ancient); err != nil || ancient != 0 {
				t.Fatalf("detail beyond daily retention survived: rows=%d err=%v", ancient, err)
			}
		})
	}
}

func seedRequestRetentionRow(t *testing.T, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, table string, at time.Time, route, kind, caller string, count int64) {
	t.Helper()
	var query string
	args := []any{at.Unix(), route}
	switch table {
	case "request_minutes":
		query = "INSERT INTO request_minutes VALUES(?,?,?,?,2,?,?,3,3,3,'[]')"
		args = append(args, kind, caller, count, float64(count)*3)
	case "request_hours":
		query = "INSERT INTO request_hours VALUES(?,?,2,?,?,3,3,3,'[]')"
		args = append(args, count, float64(count)*3)
	case "request_days":
		query = "INSERT INTO request_days VALUES(?,?,?,2,?,?,3)"
		args = append(args, kind, count, float64(count)*3)
	default:
		t.Fatalf("unknown metrics table %q", table)
	}
	if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestRequestMetricsRetentionResumes(t *testing.T) {
	for _, test := range []struct {
		table string
		age   time.Duration
	}{
		{"request_minutes", 15 * 24 * time.Hour},
		{"request_hours", 91 * 24 * time.Hour},
		{"request_days", 401 * 24 * time.Hour},
	} {
		t.Run(test.table, func(t *testing.T) {
			now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
			s := &Service{config: Config{now: func() time.Time { return now }}}
			for index, organization := range []tracker.OrganizationID{"org_one", "org_two"} {
				t.Run(string(organization), func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "metrics.sqlite")
					m := testRequestMetrics(t, path, organization)
					s.requestMetrics = m
					kinds := []string{"runner", "browser", "mcp", "ci", "anonymous", "legacy-one", "legacy-two"}
					tx, err := m.db.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					for i := range requestRetentionBatch + 1 {
						route := fmt.Sprintf("GET /expired/%03d", i)
						kind := "runner"
						if test.table == "request_minutes" {
							route, kind = "GET /expired", kinds[i%len(kinds)]
						}
						seedRequestRetentionRow(t, tx, test.table, now.Add(-test.age), route, kind, fmt.Sprintf("caller-%d", i), int64(index+1))
					}
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}
					for pass, want := range []int{1, 0, 0} {
						if err := s.expireRequestMetrics(t.Context(), s.config.now()); err != nil {
							t.Fatal(err)
						}
						var remaining int
						if err := m.db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+test.table).Scan(&remaining); err != nil || remaining != want {
							t.Fatalf("pass %d remaining=%d want=%d err=%v", pass, remaining, want, err)
						}
						if pass == 0 {
							if err := m.db.Close(); err != nil {
								t.Fatal(err)
							}
							m = testRequestMetrics(t, path, organization)
							s = &Service{config: Config{now: func() time.Time { return now }}, requestMetrics: m}
						}
					}
					if test.table == "request_minutes" {
						var rows, count int64
						var sum, maximum float64
						if err := m.db.QueryRowContext(t.Context(), "SELECT count(*),sum(count),sum(sum_ms),max(max_ms) FROM request_days").Scan(&rows, &count, &sum, &maximum); err != nil {
							t.Fatal(err)
						}
						want := int64((requestRetentionBatch + 1) * (index + 1))
						if rows != 6 || count != want || sum != float64(want)*3 || maximum != 3 {
							t.Fatalf("compacted rows=%d count=%d sum=%g max=%g, want 6/%d/%d/3", rows, count, sum, maximum, want, want*3)
						}
					}
				})
			}
		})
	}
}

func TestRequestMetricsRetentionPreservesUncommittedTotals(t *testing.T) {
	for _, test := range []struct {
		name, trigger string
		cancel        bool
	}{
		{"daily write rejected", "CREATE TRIGGER reject_daily BEFORE INSERT ON request_days BEGIN SELECT RAISE(ABORT,'example failure'); END", false},
		{"minute delete rejected", "CREATE TRIGGER reject_delete BEFORE DELETE ON request_minutes BEGIN SELECT RAISE(ABORT,'example failure'); END", false},
		{"canceled retention", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
			m := testRequestMetrics(t, filepath.Join(t.TempDir(), "metrics.sqlite"), "org_one")
			seedRequestRetentionRow(t, m.db, "request_minutes", now.Add(-15*24*time.Hour), "GET /example", "runner", "example", 2)
			seedRequestRetentionRow(t, m.db, "request_days", now.Add(-15*24*time.Hour), "GET /example", "runner", "", 3)
			if !test.cancel {
				seedRequestRetentionRow(t, m.db, "request_hours", now.Add(-91*24*time.Hour), "GET /expired", "runner", "", 1)
				seedRequestRetentionRow(t, m.db, "request_days", now.Add(-401*24*time.Hour), "GET /expired", "runner", "", 1)
			}
			if test.trigger != "" {
				if _, err := m.db.ExecContext(t.Context(), test.trigger); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.cancel {
				cancel()
			}
			if err := (&Service{requestMetrics: m}).expireRequestMetrics(ctx, now); err == nil {
				t.Fatal("retention succeeded despite rejected transaction")
			}
			var hours int
			if err := m.db.QueryRowContext(t.Context(), "SELECT count(*) FROM request_hours").Scan(&hours); err != nil || hours != 0 {
				t.Fatalf("minute compaction prevented hourly expiry: rows=%d err=%v", hours, err)
			}
			for _, table := range []struct {
				name  string
				count int64
			}{{"request_minutes", 2}, {"request_days", 3}} {
				var count int64
				var sum float64
				if err := m.db.QueryRowContext(t.Context(), "SELECT sum(count),sum(sum_ms) FROM "+table.name).Scan(&count, &sum); err != nil || count != table.count || sum != float64(table.count)*3 {
					t.Fatalf("%s count=%d sum=%g err=%v", table.name, count, sum, err)
				}
			}
		})
	}
}

func TestRequestMinuteRetentionKeepsPaceWithIngestion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		callers int
	}{
		{"within one batch", 3},
		{"beyond one batch", requestRetentionBatch + 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			m := testRequestMetrics(t, filepath.Join(t.TempDir(), "metrics.sqlite"), "org_one")
			written := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			for i := range test.callers {
				m.record(written, "GET /items/:item", "mcp", fmt.Sprintf("key_%d", i), 200, time.Millisecond)
			}
			if err := m.flush(t.Context(), written.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			expired := written.Add(requestMinuteRetention + 2*time.Minute)
			if err := (&Service{requestMetrics: m}).expireRequestMetrics(t.Context(), expired); err != nil {
				t.Fatal(err)
			}
			var remaining int
			if err := m.db.QueryRowContext(t.Context(), "SELECT count(*) FROM request_minutes WHERE minute < ?", expired.Add(-requestMinuteRetention).Unix()).Scan(&remaining); err != nil {
				t.Fatal(err)
			}
			if remaining != 0 {
				t.Fatalf("expired request minutes left after one tick: %d of %d", remaining, test.callers)
			}
		})
	}
}
