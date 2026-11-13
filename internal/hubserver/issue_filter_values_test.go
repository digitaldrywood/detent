package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestIssueFilterValues(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-project")
	type entry struct {
		fixture nativeFixture
		issue   tracker.NativeIssue
		id      tracker.WorkItemID
	}
	var entries [2]entry
	var sources []CoreWorkItemImportSource
	var projectSources []*coreNeutralFixture
	for index, fixture := range []nativeFixture{f, other} {
		source := coreWorkItemNeutralFixture(t, fixture.project.OrganizationID, fixture.project.ID, false)
		for _, suffix := range []string{"", "_second"} {
			labels, assignees := []string{"control"}, []string{"other"}
			if suffix == "" {
				labels, assignees = []string{" Required ", "required"}, []string{" Chosen ", "chosen"}
			}
			source.set("issues", map[string]string{"native_id": "wi_core_import" + suffix}, map[string]any{"labels_json": string(historyTestJSON(t, labels)), "assignees_json": string(historyTestJSON(t, assignees))})
			for rowIndex, row := range source.records["collaboration_versions"] {
				var record, raw string
				if err := row.Decode(map[string]any{"record_id": &record}); err != nil {
					t.Fatal(err)
				}
				if record != "wi_core_import"+suffix {
					continue
				}
				var revision int64
				if err := row.Decode(map[string]any{"revision": &revision}); err != nil {
					t.Fatal(err)
				}
				base := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: fixture.project.OrganizationID, ProjectID: fixture.project.ID, WorkItemID: tracker.NativeWorkItemID(record), Number: 1, Revision: tracker.Revision(revision), Profile: "native"}, Title: "Initial title", Body: coreImportBody(), State: "Todo", Labels: labels, Assignees: assignees, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}, CreatedAt: parseTestTime(t, "2026-10-01T00:00:00Z"), UpdatedAt: parseTestTime(t, "2026-10-01T00:00:00Z"), LastActivityAt: parseTestTime(t, "2026-10-01T00:00:00Z")}
				if suffix != "" {
					base.Number = 3
				}
				if revision == 2 {
					base.Title = "Updated title"
					base.UpdatedAt, base.LastActivityAt = parseTestTime(t, "2026-10-02T00:00:00Z"), parseTestTime(t, "2026-10-02T00:00:00Z")
				}
				raw = string(historyTestJSON(t, base))
				cell := row.Values["encoding"]
				cell.Null, cell.Encoding, cell.Data = false, "utf8", []byte("snapshot")
				row.Values["encoding"] = cell
				cell.Data = []byte(raw)
				row.Values["record_json"] = cell
				source.records["collaboration_versions"][rowIndex] = row
			}
		}
		for table, rows := range source.records {
			for rowIndex, row := range rows {
				for name, cell := range row.Values {
					if index > 0 && ((table == "issues" && name == "id") || (table == "queue_entries" && (name == "id" || name == "issue_id"))) {
						numeric, err := strconv.Atoi(string(cell.Data))
						if err != nil {
							t.Fatal(err)
						}
						cell.Data = []byte(strconv.Itoa(numeric + 100))
						if name == "id" {
							row.SourceRowID = int64(numeric + 100)
						}
					}
					for _, marker := range []string{"wi_core_import", "comment_core_import", "event_core_import"} {
						cell.Data = []byte(strings.ReplaceAll(string(cell.Data), marker, string(fixture.project.ID)+"_"+marker))
					}
					row.Values[name] = cell
				}
				for keyIndex, key := range row.Key {
					if index > 0 && (table == "issues" || table == "queue_entries") {
						if numeric, err := strconv.Atoi(key); err == nil {
							key = strconv.Itoa(numeric + 100)
						}
					}
					for _, marker := range []string{"wi_core_import", "comment_core_import", "event_core_import"} {
						key = strings.ReplaceAll(key, marker, string(fixture.project.ID)+"_"+marker)
					}
					row.Key[keyIndex] = key
				}
				source.records[table][rowIndex] = row
			}
		}
		projectSources = append(projectSources, source)
	}
	bundle := mergeCoreNeutralProjectSources(t, projectSources...).write()
	for index, fixture := range []nativeFixture{f, other} {
		input := CoreWorkItemImportSource{BundlePath: bundle, OrganizationID: fixture.project.OrganizationID, ProjectID: fixture.project.ID, Objects: f.service.runDetailStore()}
		if count, err := ImportCoreWorkItems(t.Context(), f.service.database.postgres, input); err != nil || count != 2 {
			t.Fatalf("filter source import count=%d error=%v", count, err)
		}
		sources = append(sources, input)
		credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), fixture.project.OrganizationID, apikey.HashToken(fixture.token), "", "")
		if err != nil || status != http.StatusOK {
			t.Fatalf("fixture authentication=%d %v", status, err)
		}
		scope := hubscope.NativeScope{Organization: fixture.project.OrganizationID, Project: fixture.project.ID, Credential: credential}
		issue, err := f.service.readWorkItem(t.Context(), scope, string(fixture.project.ID)+"_wi_core_import")
		if err != nil {
			t.Fatal(err)
		}
		var id tracker.WorkItemID
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			item, err := r.WorkItem(t.Context(), scope.Project, issue.WorkItemID)
			id = tracker.WorkItemID(item.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		entries[index] = entry{fixture, issue, id}
	}
	candidates := func(fixture nativeFixture, assignees, labels []string) []tracker.WorkItemID {
		t.Helper()
		var ids []tracker.WorkItemID
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), fixture.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			page, err := r.CandidatePage(t.Context(), claimstore.CandidateQuery{ProjectID: fixture.project.ID, Limit: 1, Assignees: assignees, IncludeLabels: labels, AvailableAt: f.service.config.now()})
			for _, candidate := range page.Items {
				ids = append(ids, tracker.WorkItemID(candidate.ID))
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	assertMatches := func(firstMatches bool) {
		t.Helper()
		for index, entry := range entries {
			ids := candidates(entry.fixture, []string{"chosen"}, []string{"required"})
			want := []tracker.WorkItemID{entry.id}
			if index == 0 && !firstMatches {
				want = nil
			}
			if !slices.Equal(ids, want) {
				t.Fatalf("project %s candidates=%v want=%v", entry.fixture.project.ID, ids, want)
			}
		}
	}
	assertMatches(true)
	for _, source := range sources {
		if count, err := ImportCoreWorkItems(t.Context(), f.service.database.postgres, source); err != nil || count != 2 {
			t.Fatalf("filter source replay count=%d error=%v", count, err)
		}
	}
	assertMatches(true)
	for index, values := range [][]string{{"Replacement"}, {}} {
		request := tracker.UpdateIssue{
			Mutation:         tracker.Mutation{IdempotencyKey: hubapi.NewID("filter_update")},
			ExpectedRevision: entries[0].issue.Revision + tracker.Revision(index), Labels: &values, Assignees: &values,
		}
		response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(entries[0].issue.WorkItemID), f.token, request)
		requireNativeStatus(t, response, http.StatusOK)
		assertMatches(false)
		count := len(candidates(f, []string{"replacement"}, nil)) + len(candidates(f, nil, []string{"replacement"}))
		if want := 2 * (1 - index); count != want {
			t.Fatalf("updated filter values=%d want=%d", count, want)
		}
		issue := readWorkItem(t, f, entries[0].issue.WorkItemID, "")
		count = 0
		for _, value := range append(slices.Clone(issue.Labels), issue.Assignees...) {
			if value == "Replacement" {
				count++
			}
		}
		if want := 2 * (1 - index); count != want {
			t.Fatalf("updated exact filter values=%d want=%d", count, want)
		}
	}
}

func syncNativeIssueFixtureFacts(t *testing.T, q interface {
	hubapi.NativeQueryer
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
