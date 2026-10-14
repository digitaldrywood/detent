package hubserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestImportedGitHubReferenceMapping(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	const sourceURL = "https://github.com/digitaldrywood/detent/issues/2199"
	for index, test := range []struct {
		name, body, provider, wantURL string
	}{
		{"migration section", "Body\n\n## Migration context\nImported from " + sourceURL, "github", sourceURL},
		{"migration line", "Body\nMigration context: Imported from [GitHub](" + sourceURL + ").", "github", sourceURL},
		{"canonical URL", "## Migration context\nhttps://github.com/DigitalDryWood/Detent/issues/2199/", "github", sourceURL},
		{"body mention is not identity", "Discuss " + sourceURL, "github", ""},
		{"later section is not identity", "## Migration context\nUnknown\n## Related work\n" + sourceURL, "github", ""},
		{"ambiguous sources", "## Migration context\n" + sourceURL + "\nhttps://github.com/digitaldrywood/detent/issues/2308", "github", ""},
		{"malformed identity", "## Migration context\nhttps://github.com/digitaldrywood/detent/issues/2199?unexpected=1", "github", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := fmt.Sprintf("I_mapping_%d", index)
			now := time.Now().UTC().Add(-time.Hour)
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: id}, Title: test.name, Body: test.body, State: "Todo", Provenance: &tracker.Provenance{Provider: test.provider, ExternalID: id, AuthorID: "source-author", CreatedAt: now, UpdatedAt: now, ObservedAt: now}}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
			requireNativeStatus(t, response, http.StatusOK)
			var created tracker.NativeIssue
			decodeHubResponse(t, response, &created)
			want := tracker.ExternalReference{Provider: test.provider, Kind: "issue", ID: id}
			if test.wantURL != "" {
				want = tracker.GitHubIssueSourceReference(id, test.wantURL)
			}
			read := readWorkItem(t, f, created.WorkItemID, "")
			if !reflect.DeepEqual(created.ExternalReferences, []tracker.ExternalReference{want}) || !reflect.DeepEqual(read.ExternalReferences, created.ExternalReferences) {
				t.Fatalf("create/read references = %+v / %+v, want %+v", created.ExternalReferences, read.ExternalReferences, want)
			}
			body := "Edited native body"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(created.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: id + "-edit"}, ExpectedRevision: created.Revision, Body: &body}), http.StatusOK)
			read = readWorkItem(t, f, created.WorkItemID, "")
			if !reflect.DeepEqual(read.ExternalReferences, created.ExternalReferences) {
				t.Fatalf("body edit lost source identity: %+v", read.ExternalReferences)
			}
		})
	}
}

func TestImportedGitHubReferenceBackfill(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	other := newNativeFixture(t, f.service, "", "other-imports")
	const sourceURL = "https://github.com/digitaldrywood/detent/issues/2199"
	for index, test := range []struct {
		name, body, storedURL, recordURL, recordNode, wantURL string
		originalBody, otherProject                            bool
	}{
		{name: "resolved migration context", body: "## Migration context\nImported from " + sourceURL, wantURL: sourceURL},
		{name: "resolved original body", body: "## Migration context\n" + sourceURL, originalBody: true, wantURL: sourceURL},
		{name: "resolved import record", recordURL: sourceURL, wantURL: sourceURL},
		{name: "resolved other project", body: "Migration context: " + sourceURL, otherProject: true, wantURL: sourceURL},
		{name: "already complete", storedURL: sourceURL, recordURL: "https://github.com/digitaldrywood/detent/issues/2308", wantURL: sourceURL},
		{name: "unresolvable", body: "Unrelated mention: " + sourceURL},
		{name: "malformed source", recordURL: "https://github.com/digitaldrywood/detent/issues/2199?query=1"},
		{name: "different node", recordURL: sourceURL, recordNode: "I_other"},
		{name: "conflicting sources", body: "## Migration context\n" + sourceURL, recordURL: "https://github.com/digitaldrywood/detent/issues/2308"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := f
			if test.otherProject {
				fixture = other
			}
			id := fmt.Sprintf("I_backfill_%d", index)
			now := time.Now().UTC().Add(-time.Hour)
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: id}, Title: test.name, Body: test.body, State: "Todo", Provenance: &tracker.Provenance{Provider: "github", ExternalID: id, AuthorID: "source-author", CreatedAt: now, UpdatedAt: now, ObservedAt: now}}
			response := performHubAPIRequest(t, fixture.service, http.MethodPost, fixture.base+"/work-items", fixture.token, request)
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			body := test.body
			if test.originalBody {
				body = "Native edits removed migration context"
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET url = ?, github_number = NULL, body = ? WHERE native_id = ?", test.storedURL, body, issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			if test.recordURL != "" {
				node := id
				if test.recordNode != "" {
					node = test.recordNode
				}
				record := GitHubImportRecord{Kind: "issue", SourceKey: "issue:" + node, Data: json.RawMessage(fmt.Sprintf(`{"node_id":%q,"html_url":%q}`, node, test.recordURL)), Provenance: tracker.Provenance{Provider: "github", ExternalID: node}}
				raw, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO github_imports(id,project_id,issue_number,work_item_id,observed_at) VALUES (?,?,?,?,?)", id, fixture.project.ID, index+1, issue.WorkItemID, testTimestamp); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO github_import_records(import_id,source_key,kind,record_json,observed_at) VALUES (?,?,'issue',?,?)", id, record.SourceKey, string(raw), testTimestamp); err != nil {
					t.Fatal(err)
				}
			}
			before := readWorkItem(t, fixture, issue.WorkItemID, "")
			if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hub_schema_version WHERE version_id = 20261006023000"); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			if _, err := runMigrations(t.Context(), f.service.database.db, logger); err != nil {
				t.Fatal(err)
			}
			after := readWorkItem(t, fixture, issue.WorkItemID, "")
			want := tracker.GitHubIssueSourceReference(id, test.wantURL)
			if !reflect.DeepEqual(after.ExternalReferences, []tracker.ExternalReference{want}) {
				t.Fatalf("backfilled references = %+v, want %+v", after.ExternalReferences, want)
			}
			if test.wantURL == "" && (!strings.Contains(logs.String(), string(issue.WorkItemID)) || !reflect.DeepEqual(before, after)) {
				t.Fatalf("unresolved reference changed or was not logged: %s", logs.String())
			}
			after.ExternalReferences = before.ExternalReferences
			if !reflect.DeepEqual(before, after) {
				t.Fatal("backfill changed native content, revision, or workflow")
			}
			if test.wantURL != "" {
				scope := nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID}
				sources, err := readNativeSourceReferences(t.Context(), f.service.database.db, scope, string(issue.WorkItemID))
				if err != nil || tracker.AppendGitHubIssueClosingReferences("Landing", sources) != "Landing\n\nCloses digitaldrywood/detent#2199" {
					t.Fatalf("landing references = %+v, error = %v", sources, err)
				}
			}
			logs.Reset()
			if _, err := runMigrations(t.Context(), f.service.database.db, logger); err != nil || strings.Contains(logs.String(), "Backfilled") {
				t.Fatalf("migration repeated on restart: %s, error = %v", logs.String(), err)
			}
		})
	}
}
