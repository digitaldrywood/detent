package coreimport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestImportedGitHubReferenceMapping(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubserver.Config{})
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
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: id}, Title: test.name, Body: test.body + hubfixture.IssueContractSections, State: "Todo", Provenance: &tracker.Provenance{Provider: test.provider, ExternalID: id, AuthorID: "source-author", CreatedAt: now, UpdatedAt: now, ObservedAt: now}}
			response := createIndependentNativeIssue(t, request, func(request tracker.CreateIssue) *httptest.ResponseRecorder {
				return f.Hub.Request(t, http.MethodPost, f.Base+"/work-items", f.Token, request)
			})
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var created tracker.NativeIssue
			hubfixture.Decode(t, response, &created)
			want := tracker.ExternalReference{Provider: test.provider, Kind: "issue", ID: id}
			if test.wantURL != "" {
				want = tracker.GitHubIssueSourceReference(id, test.wantURL)
			}
			read := hubfixture.ReadWorkItem(t, f, created.WorkItemID, "")
			if !reflect.DeepEqual(created.ExternalReferences, []tracker.ExternalReference{want}) || !reflect.DeepEqual(read.ExternalReferences, created.ExternalReferences) {
				t.Fatalf("create/read references = %+v / %+v, want %+v", created.ExternalReferences, read.ExternalReferences, want)
			}
			body := "Edited native body" + hubfixture.IssueContractSections
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPatch, f.Base+"/work-items/"+string(created.WorkItemID), f.Token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: id + "-edit"}, ExpectedRevision: created.Revision, Body: &body}), http.StatusOK)
			read = hubfixture.ReadWorkItem(t, f, created.WorkItemID, "")
			if !reflect.DeepEqual(read.ExternalReferences, created.ExternalReferences) {
				t.Fatalf("body edit lost source identity: %+v", read.ExternalReferences)
			}
		})
	}
}

func TestImportedGitHubReferenceBackfill(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewNative(t, hubfixture.OpenDeps(t, hubfixture.Config{}, hubdeps.Deps{RunMaintenance: func(ctx context.Context, _ *hubmaintenance.Scheduler, _ func(error)) error {
		<-ctx.Done()
		return ctx.Err()
	}}), "", "Example project")
	other := hubfixture.NewNative(t, f.Hub, "", "other-imports")
	const sourceURL = "https://github.com/digitaldrywood/detent/issues/2199"
	tests := []struct {
		name, body, storedURL, recordURL, recordNode, wantURL string
		originalBody, otherProject, typedProvenance           bool
	}{
		{name: "resolved migration context", body: "## Migration context\nImported from " + sourceURL, wantURL: sourceURL},
		{name: "typed provenance excludes legacy payload", body: "## Migration context\nImported from " + sourceURL, typedProvenance: true},
		{name: "resolved original body", body: "## Migration context\n" + sourceURL, originalBody: true, wantURL: sourceURL},
		{name: "resolved import record", recordURL: sourceURL, wantURL: sourceURL},
		{name: "resolved other project", body: "Migration context: " + sourceURL, otherProject: true, wantURL: sourceURL},
		{name: "already complete", storedURL: sourceURL, recordURL: "https://github.com/digitaldrywood/detent/issues/2308", wantURL: sourceURL},
		{name: "unresolvable", body: "Unrelated mention: " + sourceURL},
		{name: "malformed source", recordURL: "https://github.com/digitaldrywood/detent/issues/2199?query=1"},
		{name: "different node", recordURL: sourceURL, recordNode: "I_other"},
		{name: "conflicting sources", body: "## Migration context\n" + sourceURL, recordURL: "https://github.com/digitaldrywood/detent/issues/2308"},
		{name: "foreign repository", recordURL: "https://github.com/example-org/foreign/issues/2199"},
		{name: "typed number remains authoritative", body: "## Migration context\nhttps://github.com/digitaldrywood/detent/issues/2308", recordURL: "https://github.com/digitaldrywood/detent/issues/2308", wantURL: sourceURL},
		{name: "canonical reference remains authoritative", storedURL: sourceURL, recordURL: "https://github.com/digitaldrywood/detent/issues/2308", wantURL: sourceURL},
	}
	organization := f.Project.OrganizationID
	source := newCoreNeutralFixture(t, organization, "issues", "workflow_states", "repositories", "queue_entries", "native_comments", "collaboration_versions", "collaboration_events", "change_content_objects", "attachments", "github_imports", "github_import_records")
	source.Add("workflow_states", map[string]any{"id": 1, "project_id": f.Project.ID, "detent_state": "Todo"})
	source.Add("workflow_states", map[string]any{"id": 2, "project_id": other.Project.ID, "detent_state": "Todo"})
	source.Add("repositories", map[string]any{"id": 1, "github_owner": "digitaldrywood", "github_name": "detent", "github_node_id": "R_source"})
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	type importedCase struct {
		fixture  hubfixture.Native
		issue    tracker.NativeIssue
		original string
		numeric  int64
	}
	cases := make([]importedCase, len(tests))
	for index, test := range tests {
		fixture, state := f, 1
		if test.otherProject {
			fixture, state = other, 2
		}
		node := fmt.Sprintf("I_backfill_%d", index)
		id := tracker.NativeWorkItemID(fmt.Sprintf("wi_backfill_%d", index))
		actor := tracker.Actor{Kind: "human", PrincipalID: "source-author"}
		provenance := &tracker.Provenance{Provider: "github", ExternalID: node, AuthorID: "source-author", CreatedAt: now, UpdatedAt: now.Add(time.Hour), ObservedAt: now.Add(time.Hour)}
		body := test.body
		if test.originalBody {
			body = "Native edits removed migration context"
		}
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: fixture.Project.ID, WorkItemID: id, Number: index + 1, Revision: 2, Profile: "native"}, Title: test.name, Body: body, State: "Todo", Actor: actor, Provenance: provenance, CreatedAt: now, UpdatedAt: now.Add(time.Hour), LastActivityAt: now.Add(time.Hour), ExternalReferences: []tracker.ExternalReference{tracker.GitHubIssueSourceReference(node, test.storedURL)}}
		numeric := int64(1000 + index)
		row := map[string]any{"id": numeric, "native_id": id, "project_id": fixture.Project.ID, "number": index + 1, "revision": 2, "event_sequence": 2, "title": issue.Title, "body": body, "workflow_state_id": state, "repository_id": 1, "url": test.storedURL, "github_number": nil, "actor_json": string(historyTestJSON(t, actor)), "provenance_json": string(historyTestJSON(t, provenance)), "archived": 0, "created_at": now.Format(time.RFC3339Nano), "updated_at": issue.UpdatedAt.Format(time.RFC3339Nano), "native_created_at": now.Format(time.RFC3339Nano), "native_updated_at": issue.UpdatedAt.Format(time.RFC3339Nano), "last_activity_at": issue.LastActivityAt.Format(time.RFC3339Nano), "labels_json": "[]", "assignees_json": "[]"}
		if test.name == "typed number remains authoritative" {
			row["github_number"] = 2199
		}
		if test.name == "canonical reference remains authoritative" {
			row["url"] = ""
		}
		if test.typedProvenance {
			row["provenance_provider"] = "other"
			row["provenance_external_id"] = node
		}
		source.Add("issues", row)
		original := issue
		original.Revision, original.UpdatedAt, original.Body = 1, now, test.body
		originalRaw := string(historyTestJSON(t, original))
		source.Add("collaboration_versions", map[string]any{"project_id": fixture.Project.ID, "work_item_id": id, "record_id": id, "revision": 1, "encoding": "snapshot", "record_json": originalRaw})
		source.Add("collaboration_versions", map[string]any{"project_id": fixture.Project.ID, "work_item_id": id, "record_id": id, "revision": 2, "encoding": "snapshot", "record_json": string(historyTestJSON(t, issue))})
		for sequence, kind := range []string{"issue.created", "issue.updated"} {
			source.Add("collaboration_events", map[string]any{"id": fmt.Sprintf("evt_backfill_%d_%d", index, sequence+1), "project_id": fixture.Project.ID, "work_item_id": id, "sequence": sequence + 1, "type": kind, "schema_version": 1, "actor_json": string(historyTestJSON(t, actor)), "data_json": string(historyTestJSON(t, tracker.CollaborationData{Revision: tracker.Revision(sequence + 1)})), "event_revision": sequence + 1, "recorded_at": now.Add(time.Duration(sequence) * time.Hour).Format(time.RFC3339Nano)})
		}
		if test.recordURL != "" {
			recordNode := node
			if test.recordNode != "" {
				recordNode = test.recordNode
			}
			record := hubserver.GitHubImportRecord{Kind: "issue", SourceKey: "issue:" + recordNode, Data: json.RawMessage(fmt.Sprintf(`{"node_id":%q,"html_url":%q}`, recordNode, test.recordURL)), Provenance: tracker.Provenance{Provider: "github", ExternalID: recordNode}}
			source.Add("github_imports", map[string]any{"id": node, "project_id": fixture.Project.ID, "issue_number": index + 1, "work_item_id": id, "observed_at": testTimestamp})
			source.Add("github_import_records", map[string]any{"sequence": index + 1, "import_id": node, "source_key": record.SourceKey, "kind": "issue", "record_json": string(historyTestJSON(t, record)), "observed_at": testTimestamp})
		}
		if test.name == "unresolvable" {
			foreign := hubserver.GitHubImportRecord{Kind: "issue", SourceKey: "issue:" + node, Data: json.RawMessage(fmt.Sprintf(`{"node_id":%q,"html_url":%q}`, node, sourceURL)), Provenance: tracker.Provenance{Provider: "github", ExternalID: node}}
			source.Add("github_imports", map[string]any{"id": "foreign_" + node, "project_id": other.Project.ID, "issue_number": index + 1, "work_item_id": id, "observed_at": testTimestamp})
			source.Add("github_import_records", map[string]any{"sequence": 100 + index, "import_id": "foreign_" + node, "source_key": foreign.SourceKey, "kind": "issue", "record_json": string(historyTestJSON(t, foreign)), "observed_at": testTimestamp})
		}
		if test.name == "resolved import record" {
			foreign := hubserver.GitHubImportRecord{Kind: "issue", SourceKey: "issue:typed_other", Data: json.RawMessage(fmt.Sprintf(`{"node_id":%q,"html_url":%q}`, node, "https://github.com/digitaldrywood/detent/issues/2308")), Provenance: tracker.Provenance{Provider: "github", ExternalID: node}}
			source.Add("github_import_records", map[string]any{"sequence": 200 + index, "import_id": node, "source_key": foreign.SourceKey, "kind": "issue", "record_json": string(historyTestJSON(t, foreign)), "provenance_external_id": "I_other", "observed_at": testTimestamp})
		}

		cases[index] = importedCase{fixture: fixture, issue: issue, original: originalRaw, numeric: numeric}
		hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodGet, fixture.Base+"/work-items/"+string(id), fixture.Token, nil), http.StatusNotFound)
	}
	path := source.Write()
	repository := f.Hub.Config.TenantRepository.(*postgres.Repository)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	projectSequences := make(map[tracker.ProjectID]int64)
	for _, fixture := range []hubfixture.Native{f, other} {
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			sequence, err := r.ProjectEventSequence(t.Context(), fixture.Project.ID)
			projectSequences[fixture.Project.ID] = sequence
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, fixture := range []hubfixture.Native{f, other} {
		count, err := coreimport.ImportCoreWorkItems(t.Context(), repository, coreimport.CoreWorkItemImportSource{BundlePath: path, OrganizationID: organization, ProjectID: fixture.Project.ID, Objects: f.Hub.Config.RunDetails, Logger: logger})
		want := int64(len(tests) - 1)
		if fixture.Project.ID == other.Project.ID {
			want = 1
		}
		if err != nil || count != want {
			t.Fatalf("historical reference import=%d want=%d error=%v", count, want, err)
		}
	}
	for _, fixture := range []hubfixture.Native{f, other} {
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			sequence, err := r.ProjectEventSequence(t.Context(), fixture.Project.ID)
			if err == nil && (sequence <= projectSequences[fixture.Project.ID] || sequence < 2) {
				return fmt.Errorf("backfill regressed project change sequence")
			}
			projectSequences[fixture.Project.ID] = sequence
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	receipts := make(map[tracker.NativeWorkItemID]eventstore.Receipt)
	public := make(map[tracker.NativeWorkItemID]tracker.NativeIssue)
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := cases[index]
			after := hubfixture.ReadWorkItem(t, value.fixture, value.issue.WorkItemID, "")
			want := tracker.GitHubIssueSourceReference(value.issue.Provenance.ExternalID, test.wantURL)
			if !reflect.DeepEqual(after.ExternalReferences, []tracker.ExternalReference{want}) {
				t.Fatalf("backfilled references=%+v want=%+v", after.ExternalReferences, want)
			}
			if test.wantURL == "" && !test.typedProvenance && !strings.Contains(logs.String(), string(value.issue.WorkItemID)) {
				t.Fatalf("unresolved reference was not logged: %s", logs.String())
			}
			if after.Body != value.issue.Body || after.Title != value.issue.Title || after.Revision != value.issue.Revision || after.State != value.issue.State {
				t.Fatal("backfill changed native content, revision, or workflow")
			}
			var version, original eventstore.CollaborationVersion
			var receipt eventstore.Receipt
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				version, err = r.CollaborationVersion(t.Context(), string(value.issue.WorkItemID), 2)
				if err != nil {
					return err
				}
				original, err = r.CollaborationVersion(t.Context(), string(value.issue.WorkItemID), 1)
				if err != nil {
					return err
				}
				receipt, err = r.Receipt(t.Context(), "production-import", "core-work-item", strconv.FormatInt(value.numeric, 10))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			content := eventstore.NewContent(f.Hub.Config.RunDetails)
			raw, err := content.CollaborationVersion(t.Context(), version)
			if err != nil {
				t.Fatal(err)
			}
			var canonical tracker.NativeIssue
			if err := json.Unmarshal(raw, &canonical); err != nil {
				t.Fatal(err)
			}
			canonical.ExternalReferences = value.issue.ExternalReferences
			if !reflect.DeepEqual(canonical, value.issue) {
				t.Fatal("backfill changed canonical native content, revision, or workflow")
			}
			raw, err = content.CollaborationVersion(t.Context(), original)
			if err != nil || string(raw) != value.original {
				t.Fatalf("backfill changed original immutable issue version: %s error=%v", raw, err)
			}
			if test.wantURL != "" && tracker.AppendGitHubIssueClosingReferences("Landing", after.ExternalReferences) != "Landing\n\nCloses digitaldrywood/detent#2199" {
				t.Fatalf("landing references=%+v", after.ExternalReferences)
			}
			receipts[value.issue.WorkItemID], public[value.issue.WorkItemID] = receipt, after
		})
	}
	logs.Reset()
	for _, fixture := range []hubfixture.Native{f, other} {
		if _, err := coreimport.ImportCoreWorkItems(t.Context(), repository, coreimport.CoreWorkItemImportSource{BundlePath: path, OrganizationID: organization, ProjectID: fixture.Project.ID, Objects: f.Hub.Config.RunDetails, Logger: logger}); err != nil {
			t.Fatal(err)
		}
	}
	for _, fixture := range []hubfixture.Native{f, other} {
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			sequence, err := r.ProjectEventSequence(t.Context(), fixture.Project.ID)
			if err == nil && sequence != projectSequences[fixture.Project.ID] {
				return fmt.Errorf("replayed backfill changed project history")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, value := range cases {
		after := hubfixture.ReadWorkItem(t, value.fixture, value.issue.WorkItemID, "")
		if !reflect.DeepEqual(after, public[value.issue.WorkItemID]) {
			t.Fatal("replayed import changed native content or history")
		}
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			receipt, err := r.Receipt(t.Context(), "production-import", "core-work-item", strconv.FormatInt(value.numeric, 10))
			if err == nil && !reflect.DeepEqual(receipt, receipts[value.issue.WorkItemID]) {
				return fmt.Errorf("replayed import changed its durable receipt")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
