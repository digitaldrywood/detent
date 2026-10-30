package hubanalytics

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestAnalyticsBackfillItemsBoundedByEvents(t *testing.T) {
	for _, test := range []struct {
		name     string
		sequence int64
		want     int
	}{
		{"one small item per transaction", 1, 1},
		{"large items stop at the event budget", BackfillEvents/2 + 1, 1},
		{"one oversized item still progresses", BackfillEvents * 2, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "backfill.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			if _, err := db.ExecContext(t.Context(), `CREATE TABLE issues(organization_id TEXT,project_id TEXT,native_id TEXT,event_sequence INTEGER);CREATE TABLE analytics_rollup_positions(organization_id TEXT,project_id TEXT,work_item_id TEXT);`); err != nil {
				t.Fatal(err)
			}
			for _, project := range []string{"project_a", "project_b"} {
				for i := range 3 {
					if _, err := db.ExecContext(t.Context(), "INSERT INTO issues VALUES('org_example',?,?,?)", project, project+strconv.Itoa(i), test.sequence); err != nil {
						t.Fatal(err)
					}
				}
			}
			tx, err := db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for _, project := range []string{"project_a", "project_b"} {
				ids, err := analyticsBackfillItems(t.Context(), tx, "org_example", project)
				if err != nil {
					t.Fatal(err)
				}
				if len(ids) > 0 && !strings.HasPrefix(ids[0], project) {
					t.Fatalf("cross-project backfill: %v", ids)
				}
				if len(ids) != test.want {
					t.Fatalf("batch=%d want %d", len(ids), test.want)
				}
			}
		})
	}
}
