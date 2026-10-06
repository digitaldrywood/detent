package hubserver

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthDetectorActorMigration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, kind, principal, state string
	}{
		{"active health finding", "system", "health_detector", "Todo"},
		{"terminal health finding", "system", "health_detector", "Done"},
		{"human unchanged", "human", "health_detector", "Todo"},
		{"runner unchanged", "runner", "health_detector", "Todo"},
		{"integration unchanged", "integration", "health_detector", "Todo"},
		{"other principal unchanged", "system", "another-system", "Todo"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t, Config{})
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID,
				sourceActor: &tracker.Actor{Kind: test.kind, PrincipalID: test.principal}}
			now := f.service.config.now()
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			issue, err := createNativeIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Title: test.name, Body: "Retained evidence", State: test.state}, now)
			if err != nil {
				t.Fatal(err)
			}
			comment, err := insertNativeComment(t.Context(), tx, scope, issue, "Retained occurrence", nil, now)
			if err != nil {
				t.Fatal(err)
			}
			comment.EditedBy = scope.sourceActor
			comment.Revision++
			editor, err := marshalNative(comment.EditedBy)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), "UPDATE native_comments SET edited_by_json=?, revision=? WHERE id=?", editor, comment.Revision, comment.ID); err != nil {
				t.Fatal(err)
			}
			if err := recordNativeChange(t.Context(), tx, scope, comment, string(issue.WorkItemID), comment.Revision, "comment.edited", tracker.CollaborationData{CommentID: comment.ID, Revision: comment.Revision}, now); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			before := readWorkItem(t, f, issue.WorkItemID, "")
			comment, err = readNativeComment(t.Context(), f.service.database.db, scope, string(issue.WorkItemID), comment.ID)
			if err != nil {
				t.Fatal(err)
			}
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/history"
			var history tracker.Page[tracker.CollaborationEvent]
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil), &history)
			readVersions := func() []map[string]any {
				t.Helper()
				rows, err := f.service.database.db.QueryContext(t.Context(), "SELECT record_json FROM collaboration_versions WHERE work_item_id=? ORDER BY record_id,revision", issue.WorkItemID)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var records []map[string]any
				for rows.Next() {
					var raw string
					if err := rows.Scan(&raw); err != nil {
						t.Fatal(err)
					}
					var record map[string]any
					if err := json.Unmarshal([]byte(raw), &record); err != nil {
						t.Fatal(err)
					}
					records = append(records, record)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				return records
			}
			versions := readVersions()
			if len(versions) != 3 || len(history.Items) != 3 {
				t.Fatalf("versions/history = %d/%d, want 3/3", len(versions), len(history.Items))
			}
			wantActor := *scope.sourceActor
			if test.kind == "system" && test.principal == "health_detector" {
				wantActor.Kind = "integration"
			}
			before.Actor, comment.Actor, comment.EditedBy = wantActor, wantActor, &wantActor
			for i := range history.Items {
				history.Items[i].Actor = wantActor
			}
			for _, record := range versions {
				for _, key := range []string{"actor", "edited_by"} {
					if actor, ok := record[key].(map[string]any); ok {
						actor["kind"] = wantActor.Kind
					}
				}
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hub_schema_version WHERE version_id=20261006192328"); err != nil {
				t.Fatal(err)
			}
			for _, step := range []string{"repair", "restart"} {
				t.Run(step, func(t *testing.T) {
					if _, err := runMigrations(t.Context(), f.service.database.db, discardLogger()); err != nil {
						t.Fatal(err)
					}
					after := readWorkItem(t, f, issue.WorkItemID, "")
					if !reflect.DeepEqual(before, after) {
						t.Fatalf("issue changed beyond actor kind: before=%+v after=%+v", before, after)
					}
					gotComment, err := readNativeComment(t.Context(), f.service.database.db, scope, string(issue.WorkItemID), comment.ID)
					if err != nil || !reflect.DeepEqual(comment, gotComment) {
						t.Fatalf("comment changed beyond actor kind: got=%+v want=%+v error=%v", gotComment, comment, err)
					}
					var gotHistory tracker.Page[tracker.CollaborationEvent]
					decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil), &gotHistory)
					if !reflect.DeepEqual(history, gotHistory) || !reflect.DeepEqual(versions, readVersions()) {
						t.Fatal("migration changed history or saved revisions beyond actor kind")
					}
				})
			}
			for _, table := range []string{"collaboration_events", "collaboration_versions"} {
				for _, statement := range []string{"UPDATE " + table + " SET work_item_id=work_item_id WHERE work_item_id=?", "DELETE FROM " + table + " WHERE work_item_id=?"} {
					if _, err := f.service.database.db.ExecContext(t.Context(), statement, issue.WorkItemID); err == nil {
						t.Fatalf("history immutability lost: %s", statement)
					}
				}
			}
			var page tracker.NativeIssuePage
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?archived=all", f.token, nil), &page)
			if !slices.ContainsFunc(page.Items, func(item tracker.NativeIssue) bool {
				return item.WorkItemID == issue.WorkItemID && item.Actor == wantActor
			}) {
				t.Fatal("repaired actor missing from Work list")
			}
		})
	}
}
