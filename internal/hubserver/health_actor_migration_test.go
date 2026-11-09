package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type healthActorImportExpectation struct {
	fixture  nativeFixture
	issue    tracker.NativeIssue
	comment  tracker.NativeComment
	history  []tracker.CollaborationEvent
	versions map[eventstore.CollaborationVersionKey]json.RawMessage
}

func TestHealthDetectorActorMigration(t *testing.T) {
	objects := &memoryRunDetails{}
	service := newPostgresNativeService(t, Config{RunDetails: objects})
	repository := service.database.postgres
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct{ name, kind, principal, state, want string }{
		{"active health finding", "system", "health_detector", "Todo", "integration"},
		{"terminal health finding", "system", "health_detector", "Done", "integration"},
		{"human unchanged", "human", "health_detector", "Todo", "human"},
		{"runner unchanged", "runner", "health_detector", "Todo", "runner"},
		{"integration unchanged", "integration", "health_detector", "Todo", "integration"},
		{"other principal unchanged", "system", "another-system", "Todo", "system"},
	}
	var expected []healthActorImportExpectation
	var sources []CoreWorkItemImportSource
	encode := func(value any) json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		fixture := newCoreNeutralFixture(t, org, "issues", "workflow_states", "repositories", "queue_entries", "native_comments", "collaboration_versions", "collaboration_events", "issue_dependencies", "change_content_objects", "attachments")
		fixture.add("repositories", map[string]any{"id": 1, "github_owner": "example-org", "github_name": "example-project"})
		var projects []tracker.ProjectID
		for projectIndex, name := range []string{"First actor history", "Second actor history"} {
			f := newNativeFixture(t, service, org, name)
			projects = append(projects, f.project.ID)
			for stateIndex, state := range []string{"Todo", "Done"} {
				fixture.add("workflow_states", map[string]any{"id": projectIndex*100 + stateIndex + 1, "project_id": f.project.ID, "detent_state": state})
			}
			for index, test := range cases {
				id := projectIndex*100 + index + 1
				item := tracker.NativeWorkItemID(fmt.Sprintf("wi_health_%d_%d", projectIndex, index))
				actor := tracker.Actor{Kind: test.kind, PrincipalID: test.principal}
				wantActor := actor
				wantActor.Kind = test.want
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: f.project.ID, WorkItemID: item, Number: index + 1, Revision: 1, Profile: "native"}, Title: test.name, Body: "Retained evidence" + issueContractTestSections, State: test.state, Actor: actor, CreatedAt: at, UpdatedAt: at, LastActivityAt: at}
				comment := tracker.NativeComment{ID: fmt.Sprintf("comment_health_%d_%d", projectIndex, index), OrganizationID: org, ProjectID: f.project.ID, WorkItemID: item, Revision: 1, Sequence: 1, Body: "Retained occurrence", Actor: actor, CreatedAt: at, UpdatedAt: at}
				edited := comment
				edited.Revision = 2
				edited.EditedBy = &actor
				wantIssue, wantComment, wantEdited := issue, comment, edited
				wantIssue.Actor, wantComment.Actor, wantEdited.Actor = wantActor, wantActor, wantActor
				wantEdited.EditedBy = &wantActor
				want := healthActorImportExpectation{fixture: f, issue: wantIssue, comment: wantEdited, versions: map[eventstore.CollaborationVersionKey]json.RawMessage{}}
				for _, version := range []struct {
					key              eventstore.CollaborationVersionKey
					source, expected any
				}{
					{eventstore.CollaborationVersionKey{RecordID: string(item), Revision: 1}, issue, wantIssue},
					{eventstore.CollaborationVersionKey{RecordID: comment.ID, Revision: 1}, comment, wantComment},
					{eventstore.CollaborationVersionKey{RecordID: comment.ID, Revision: 2}, edited, wantEdited},
				} {
					fixture.add("collaboration_versions", map[string]any{"project_id": f.project.ID, "work_item_id": item, "record_id": version.key.RecordID, "revision": version.key.Revision, "encoding": "snapshot", "record_json": string(encode(version.source))})
					want.versions[version.key] = encode(version.expected)
				}
				fixture.add("native_comments", map[string]any{"id": comment.ID, "project_id": f.project.ID, "work_item_id": item, "revision": 2, "sequence": 1, "body": comment.Body, "actor_json": string(encode(actor)), "edited_by_json": string(encode(actor)), "provenance_json": nil, "source_key": nil, "deleted": 0, "created_at": at.Format(time.RFC3339Nano), "updated_at": at.Format(time.RFC3339Nano)})
				stateID := projectIndex*100 + 1
				if test.state == "Done" {
					stateID++
				}
				fixture.add("issues", map[string]any{"id": id, "native_id": item, "project_id": f.project.ID, "number": index + 1, "revision": 1, "event_sequence": 3, "native_source_key": nil, "title": issue.Title, "body": issue.Body, "issue_contract_json": nil, "report_context_json": nil, "actor_json": string(encode(actor)), "provenance_json": nil, "archived": 0, "created_at": at.Format(time.RFC3339Nano), "updated_at": at.Format(time.RFC3339Nano), "native_created_at": at.Format(time.RFC3339Nano), "native_updated_at": at.Format(time.RFC3339Nano), "last_activity_at": at.Format(time.RFC3339Nano), "workflow_state_id": stateID, "labels_json": "[]", "assignees_json": "[]", "author_login": "example-author", "repository_id": 1, "dispatch_wait_since": nil})
				for sequence, kind := range []string{"issue.created", "comment.created", "comment.edited"} {
					data := tracker.CollaborationData{Revision: 1}
					if sequence > 0 {
						data.CommentID = comment.ID
					}
					if sequence == 2 {
						data.Revision = 2
					}
					event := tracker.CollaborationEvent{ID: fmt.Sprintf("event_health_%d_%d_%d", projectIndex, index, sequence), OrganizationID: org, ProjectID: f.project.ID, AggregateType: "work_item", AggregateID: item, AggregateSequence: int64(sequence + 1), Type: kind, SchemaVersion: 1, Actor: wantActor, RecordedAt: at, Data: data}
					fixture.add("collaboration_events", map[string]any{"id": event.ID, "project_id": f.project.ID, "work_item_id": item, "sequence": sequence + 1, "type": kind, "schema_version": 1, "actor_json": string(encode(actor)), "data_json": string(encode(data)), "event_revision": data.Revision, "recorded_at": at.Format(time.RFC3339Nano)})
					want.history = append(want.history, event)
				}
				expected = append(expected, want)
			}
		}
		path := fixture.write()
		for _, project := range projects {
			sources = append(sources, CoreWorkItemImportSource{BundlePath: path, OrganizationID: org, ProjectID: project, Objects: objects})
		}
	}
	baselineIssues := map[tracker.NativeWorkItemID]map[tracker.OrganizationID]tracker.NativeIssue{}
	for _, stage := range []string{"import", "replay", "reopen"} {
		if stage == "reopen" {
			cfg := service.config
			cfg.Hosted = &HostedConfig{OrganizationID: "org_first", WorkOSOrganizationID: "provider_org_first", PublicURL: "https://hub.example.test", Provider: newHostedSecurityProvider()}
			cfg.runMaintenance = waitForMaintenanceFixtureShutdown
			if err := service.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			service = reopened
			t.Cleanup(func() { _ = reopened.Close() })
			service.config.Hosted = nil
			service.echo = echo.New()
			service.echo.Use(service.measureRequests, service.policyCacheMiddleware, service.changeContentMiddleware, service.runDetailMiddleware)
			service.registerNativeRoutes(service.echo)
			service.registerRunnerRoutes(service.echo)
			service.registerOperatorTools(service.echo)
			repository = service.database.postgres
		}
		for _, source := range sources {
			count, err := ImportCoreWorkItems(t.Context(), repository, source)
			if err != nil || count != int64(len(cases)) {
				t.Fatalf("stage=%s org=%s count=%d error=%v", stage, source.OrganizationID, count, err)
			}
		}
		for _, want := range expected {
			f := want.fixture
			f.service = service
			got := readWorkItem(t, f, want.issue.WorkItemID, "")
			if got.Actor != want.issue.Actor || got.Title != want.issue.Title || got.Body != want.issue.Body || got.State != want.issue.State {
				t.Fatalf("stage=%s issue changed beyond actor normalization: %+v", stage, got)
			}
			if stage == "import" {
				if baselineIssues[got.WorkItemID] == nil {
					baselineIssues[got.WorkItemID] = map[tracker.OrganizationID]tracker.NativeIssue{}
				}
				baselineIssues[got.WorkItemID][got.OrganizationID] = got
			} else if !reflect.DeepEqual(baselineIssues[got.WorkItemID][got.OrganizationID], got) {
				t.Fatal("import replay or reopen changed the public issue")
			}
			var history tracker.Page[tracker.CollaborationEvent]
			decodeHubResponse(t, performHubAPIRequest(t, service, http.MethodGet, f.base+"/work-items/"+string(want.issue.WorkItemID)+"/history", f.token, nil), &history)
			if !reflect.DeepEqual(history.Items, want.history) {
				t.Fatalf("stage=%s history changed beyond actor normalization: got=%+v want=%+v", stage, history.Items, want.history)
			}
			var versions []eventstore.CollaborationVersion
			if err := repository.ReadTenant(t.Context(), want.issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				comment, err := r.Comment(t.Context(), want.issue.ProjectID, want.issue.WorkItemID, want.comment.ID)
				if err != nil {
					return err
				}
				if comment.Revision != int64(want.comment.Revision) || comment.ActorKind != want.comment.Actor.Kind || comment.ActorPrincipalID != want.comment.Actor.PrincipalID || comment.EditedByKind == nil || *comment.EditedByKind != want.comment.EditedBy.Kind || comment.EditedByPrincipalID == nil || *comment.EditedByPrincipalID != want.comment.EditedBy.PrincipalID {
					t.Fatal("current comment or editor changed beyond actor normalization")
				}
				for key := range want.versions {
					row, err := r.CollaborationVersion(t.Context(), key.RecordID, key.Revision)
					if err != nil {
						return err
					}
					versions = append(versions, row)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, row := range versions {
				raw, err := eventstore.NewContent(service.runDetailStore()).CollaborationVersion(t.Context(), row)
				if err != nil {
					t.Fatal(err)
				}
				key := eventstore.CollaborationVersionKey{RecordID: row.RecordID, Revision: row.Revision}
				expected := want.versions[key]
				var gotMap, wantMap map[string]any
				if err := json.Unmarshal(raw, &gotMap); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(expected, &wantMap); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(gotMap, wantMap) {
					t.Fatalf("stage=%s retained version %+v changed beyond actor normalization: got=%s want=%s", stage, key, raw, expected)
				}
			}
			if err := service.refreshBoardRecord(t.Context(), want.issue.OrganizationID, viewstore.BoardCandidate{ProjectID: want.issue.ProjectID, WorkItemID: want.issue.WorkItemID}, at); err != nil {
				t.Fatal(err)
			}
			var page tracker.NativeIssuePage
			decodeHubResponse(t, performHubAPIRequest(t, service, http.MethodGet, f.base+"/work-items?archived=all", f.token, nil), &page)
			if !slices.ContainsFunc(page.Items, func(item tracker.NativeIssue) bool {
				return item.WorkItemID == want.issue.WorkItemID && item.Actor == want.issue.Actor
			}) {
				t.Fatalf("stage=%s org=%s item=%s normalized actor missing from Work list: pending=%v items=%+v", stage, want.issue.OrganizationID, want.issue.WorkItemID, page.ProjectionPending, page.Items)
			}
		}
	}
	admin := healthFixtureDatabase(t, expected[0].fixture)
	for _, want := range expected {
		for _, table := range []string{"collaboration_events", "collaboration_versions"} {
			for _, statement := range []string{"UPDATE " + table + " SET work_item_id=work_item_id WHERE organization_id=$1 AND work_item_id=$2", "DELETE FROM " + table + " WHERE organization_id=$1 AND work_item_id=$2"} {
				if _, err := admin.ExecContext(t.Context(), statement, want.issue.OrganizationID, want.issue.WorkItemID); err == nil {
					t.Fatalf("history immutability lost for %s", table)
				}
			}
		}
	}
}
