package hubserver

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptDispositionMigration(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "hub.db"), defaultBusyTimeout))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE native_attempts(id TEXT PRIMARY KEY,organization_id TEXT,project_id TEXT,work_item_id TEXT,fencing_token INTEGER,data_json TEXT)`); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id, item, project string
		fence             int
		disposition       *tracker.NativeDisposition
	}{
		{"first-blocked", "wi_first", "prj_first", 1, &tracker.NativeDisposition{Status: "blocked", HumanAction: true}},
		{"first-later", "wi_first", "prj_first", 2, &tracker.NativeDisposition{Status: "in_progress"}},
		{"second-blocked", "wi_second", "prj_second", 1, &tracker.NativeDisposition{Status: "blocked", HumanAction: true}},
		{"second-cleared", "wi_second", "prj_second", 2, &tracker.NativeDisposition{Status: "blocked"}},
		{"second-running", "wi_second", "prj_second", 3, nil},
	}
	for _, c := range cases {
		raw, err := json.Marshal(tracker.NativeRunData{Disposition: c.disposition})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,fencing_token,data_json) VALUES(?,'org_example',?,?,?,?)`, c.id, c.project, c.item, c.fence, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := migrateAttemptDispositions(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	service := &Service{database: &database{reader: db}}
	for _, c := range []struct {
		project, item string
		wantAnswer    bool
	}{
		{"prj_first", "wi_first", true}, {"prj_second", "wi_second", false},
	} {
		scope := nativeScope{organization: "org_example", project: tracker.ProjectID(c.project)}
		issue := tracker.NativeIssue{State: "Blocked"}
		issue.WorkItemID = tracker.NativeWorkItemID(c.item)
		err := service.validateCoordinatorMoveAnswer(t.Context(), scope, issue, "Todo", "")
		if (err != nil) != c.wantAnswer {
			t.Fatalf("%s answer required=%v err=%v", c.item, c.wantAnswer, err)
		}
		if err := service.validateCoordinatorMoveAnswer(t.Context(), scope, issue, "Todo", "Use the compatible option."); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range cases {
		var status string
		var human bool
		if err := db.QueryRowContext(t.Context(), "SELECT disposition_status,disposition_human_action FROM native_attempts WHERE id=?", c.id).Scan(&status, &human); err != nil {
			t.Fatal(err)
		}
		wantStatus, wantHuman := "", false
		if c.disposition != nil {
			wantStatus, wantHuman = c.disposition.Status, c.disposition.HumanAction
		}
		if status != wantStatus || human != wantHuman {
			t.Fatalf("%s disposition=%s/%v want=%s/%v", c.id, status, human, wantStatus, wantHuman)
		}
	}
}
