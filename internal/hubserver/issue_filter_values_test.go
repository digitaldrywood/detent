package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestIssueFilterValues(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-project")
	type entry struct {
		fixture nativeFixture
		issue   tracker.NativeIssue
		id      tracker.WorkItemID
	}
	var entries [2]entry
	for index, fixture := range []nativeFixture{f, other} {
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		scope := nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
		issue, err := createNativeFixtureIssueTx(t.Context(), tx, scope, tracker.CreateIssue{
			Title: "Filterable issue", Body: issueContractTestSections, State: "Todo",
			Labels: []string{" Required ", "required"}, Assignees: []string{" Chosen ", "chosen"},
		}, f.service.config.now())
		if err != nil {
			t.Fatal(err)
		}
		var id tracker.WorkItemID
		if err := tx.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id=?", issue.WorkItemID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		entries[index] = entry{fixture, issue, id}
	}
	assertMatches := func(firstMatches bool) {
		t.Helper()
		for index, entry := range entries {
			scope := nativeScope{organization: entry.fixture.project.OrganizationID, project: entry.fixture.project.ID, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
			ids, err := nativeCandidateIDs(t.Context(), f.service.database.reader, claimCandidateQuery{NativeScope: &scope, Limit: 1}, nil, nil, nil, nil, []string{"chosen"}, []string{"required"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := []tracker.WorkItemID{entry.id}
			if index == 0 && !firstMatches {
				want = nil
			}
			if !slices.Equal(ids, want) {
				t.Fatalf("project %s candidates=%v want=%v", scope.project, ids, want)
			}
		}
	}
	assertMatches(true)
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "DROP TABLE issue_filter_values"); err != nil {
		t.Fatal(err)
	}
	if err := migrateIssueFilterValues(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertMatches(true)
	for index, values := range [][]string{{"Replacement"}, {}} {
		request := tracker.UpdateIssue{
			Mutation:         tracker.Mutation{IdempotencyKey: newNativeID("filter_update")},
			ExpectedRevision: entries[0].issue.Revision + tracker.Revision(index), Labels: &values, Assignees: &values,
		}
		response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(entries[0].issue.WorkItemID), f.token, request)
		requireNativeStatus(t, response, http.StatusOK)
		assertMatches(false)
		var count int
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issue_filter_values WHERE issue_id=? AND value='replacement'", entries[0].id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if want := 2 * (1 - index); count != want {
			t.Fatalf("updated filter values=%d want=%d", count, want)
		}
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_issue_page_filter_values WHERE issue_id=? AND value='Replacement'", entries[0].id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if want := 2 * (1 - index); count != want {
			t.Fatalf("updated exact filter values=%d want=%d", count, want)
		}
	}
}

func syncNativeIssueFixtureFacts(t *testing.T, q interface {
	nativeQueryer
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, items ...tracker.NativeWorkItemID) {
	t.Helper()
	if len(items) == 0 {
		items = []tracker.NativeWorkItemID{""}
	}
	for _, item := range items {
		rows, err := q.QueryContext(t.Context(), "SELECT id,actor_json,labels_json,assignees_json FROM issues WHERE ?='' OR native_id=?", item, item)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		type fact struct {
			id                int64
			actor             tracker.Actor
			labels, assignees []string
		}
		var facts []fact
		for rows.Next() {
			var f fact
			var actor, labels, assignees string
			if err := rows.Scan(&f.id, &actor, &labels, &assignees); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			for _, field := range []struct {
				raw    string
				target any
			}{{actor, &f.actor}, {labels, &f.labels}, {assignees, &f.assignees}} {
				if err := json.Unmarshal([]byte(field.raw), field.target); err != nil {
					rows.Close()
					t.Fatal(err)
				}
			}
			facts = append(facts, f)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		for _, f := range facts {
			if err := replaceIssuePageFilterValues(t.Context(), q, f.id, f.labels, f.assignees); err != nil {
				t.Fatal(err)
			}
			if _, err := q.ExecContext(t.Context(), "UPDATE issues SET actor_kind=?,actor_principal_id=? WHERE id=?", f.actor.Kind, f.actor.PrincipalID, f.id); err != nil {
				t.Fatal(err)
			}
			if _, err := q.ExecContext(t.Context(), "DELETE FROM issue_filter_values WHERE issue_id=?", f.id); err != nil {
				t.Fatal(err)
			}
			for _, group := range []struct {
				kind   string
				values []string
			}{{"label", f.labels}, {"assignee", f.assignees}} {
				for _, value := range normalizedQueryStrings(group.values) {
					if _, err := q.ExecContext(t.Context(), "INSERT INTO issue_filter_values(issue_id,kind,value) VALUES(?,?,?)", f.id, group.kind, value); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
}
