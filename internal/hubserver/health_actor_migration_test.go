package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthDetectorActorMigration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, kind, principal, state, wantKind, editorKind, editorPrincipal, wantEditorKind string
	}{
		{"active health finding", "system", "health_detector", "Todo", "integration", "system", "health_detector", "integration"},
		{"terminal health finding", "system", "health_detector", "Done", "integration", "system", "health_detector", "integration"},
		{"human unchanged", "human", "health_detector", "Todo", "human", "human", "health_detector", "human"},
		{"runner unchanged", "runner", "health_detector", "Todo", "runner", "runner", "health_detector", "runner"},
		{"integration unchanged", "integration", "health_detector", "Todo", "integration", "integration", "health_detector", "integration"},
		{"other principal unchanged", "system", "another-system", "Todo", "system", "system", "another-system", "system"},
		{"human editor unchanged", "system", "health_detector", "Todo", "integration", "human", "example-editor", "human"},
		{"health editor only", "human", "example-author", "Todo", "human", "system", "health_detector", "integration"},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := &memoryRunDetails{}
			first := newDefaultNativeFixture(t, Config{RunDetails: objects})
			service := first.service
			repository := service.database.postgres
			adminValue, found := projectRecordDatabases.Load(repository)
			if !found {
				t.Fatal("PostgreSQL fixture admin unavailable")
			}
			admin := adminValue.(*sql.DB)
			actor := tracker.Actor{Kind: test.kind, PrincipalID: test.principal, DisplayName: "Example actor", Agent: "example-agent", ViaKey: "example-source-key"}
			editor := tracker.Actor{Kind: test.editorKind, PrincipalID: test.editorPrincipal, DisplayName: "Example editor", Agent: "example-editor-agent", ViaKey: "example-editor-key"}
			wantActor, wantEditor := actor, editor
			wantActor.Kind, wantEditor.Kind = test.wantKind, test.wantEditorKind
			marshal := func(value any) []byte {
				t.Helper()
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			for _, organization := range []tracker.OrganizationID{first.project.OrganizationID, first.project.OrganizationID + "_second"} {
				f := first
				if organization != first.project.OrganizationID {
					f = newNativeFixture(t, service, organization, "Example second project")
				}
				fixture := coreWorkItemNeutralFixture(t, organization, f.project.ID, false)
				fixture.Records["native_comments"], fixture.Records["collaboration_versions"], fixture.Records["collaboration_events"] = nil, nil, nil
				fixture.Set("workflow_states", map[string]string{"id": "1"}, map[string]any{"detent_state": test.state})
				fixture.Set("queue_entries", nil, map[string]any{"state": test.state})
				expectedVersions := make(map[string]map[string]any)
				expectedHistory := make(map[tracker.NativeWorkItemID][]tracker.CollaborationEvent)
				expectedEventPayloads := make(map[string]map[string]any)
				for index, suffix := range []string{"", "_second"} {
					item := tracker.NativeWorkItemID("wi_core_import" + suffix)
					created, updated := parseTestTime(t, "2026-10-01T00:00:00Z"), parseTestTime(t, "2026-10-02T00:00:00Z")
					issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: f.project.ID, WorkItemID: item, Number: 1 + index*2, Revision: 1, Profile: "native"}, Title: "Retained evidence", Body: coreImportBody(), State: test.state, Labels: []string{"needs-review"}, Assignees: []string{"example-assignee"}, Actor: actor, CreatedAt: created, UpdatedAt: updated, LastActivityAt: updated}
					fixture.Set("issues", map[string]string{"native_id": string(item)}, map[string]any{"title": issue.Title, "revision": 1, "event_sequence": 3, "actor_json": string(marshal(actor))})
					comment := tracker.NativeComment{ID: "comment_core_import" + suffix, OrganizationID: organization, ProjectID: f.project.ID, WorkItemID: item, Revision: 1, Sequence: 1, Body: "Retained occurrence", Actor: actor, CreatedAt: created, UpdatedAt: created}
					edited := comment
					edited.Revision, edited.EditedBy, edited.UpdatedAt = 2, &editor, updated
					fixture.Add("native_comments", map[string]any{"id": edited.ID, "revision": 2, "sequence": 1, "body": edited.Body, "actor_json": string(marshal(actor)), "edited_by_json": string(marshal(editor)), "provenance_json": nil, "source_key": nil, "deleted": 0, "created_at": created.Format(time.RFC3339Nano), "updated_at": updated.Format(time.RFC3339Nano), "organization_id": organization, "project_id": f.project.ID, "work_item_id": item})
					var previous []byte
					for _, version := range []struct {
						id       string
						revision int
						value    any
					}{{string(item), 1, issue}, {comment.ID, 1, comment}, {comment.ID, 2, edited}} {
						var fields map[string]any
						if err := json.Unmarshal(marshal(version.value), &fields); err != nil {
							t.Fatal(err)
						}
						fields["retained_extension"] = map[string]any{"value": "source evidence", "actor": map[string]any{"kind": "system", "principal_id": "health_detector"}}
						raw := marshal(fields)
						encoded, encoding := raw, "snapshot"
						if version.revision == 2 {
							var err error
							encoded, encoding, err = neutralimport.EncodeVersion(previous, "snapshot", raw, 2)
							if err != nil {
								t.Fatal(err)
							}
						}
						fixture.Add("collaboration_versions", map[string]any{"organization_id": organization, "project_id": f.project.ID, "work_item_id": item, "record_id": version.id, "revision": version.revision, "encoding": encoding, "record_json": string(encoded)})
						if version.id == comment.ID {
							previous = raw
						}
						fields["actor"] = wantActor
						if version.revision == 2 {
							fields["edited_by"] = wantEditor
						}
						var expected map[string]any
						if err := json.Unmarshal(marshal(fields), &expected); err != nil {
							t.Fatal(err)
						}
						expectedVersions[fmt.Sprintf("%s/%d", version.id, version.revision)] = expected
					}
					for i, eventType := range []string{"issue.created", "comment.created", "comment.edited"} {
						revision, eventActor, wantEventActor := tracker.Revision(1), actor, wantActor
						data := tracker.CollaborationData{Revision: revision}
						if i > 0 {
							data.CommentID = comment.ID
						}
						if i == 2 {
							data.Revision, eventActor, wantEventActor = 2, editor, wantEditor
						}
						id := fmt.Sprintf("event_health_%d%s", i+1, suffix)
						at := created
						if i == 2 {
							at = updated
						}
						var sourceActor map[string]any
						if err := json.Unmarshal(marshal(eventActor), &sourceActor); err != nil {
							t.Fatal(err)
						}
						sourceActor["retained_event_extension"] = map[string]any{"value": "historical event evidence", "actor": map[string]any{"kind": "system", "principal_id": "health_detector"}}
						fixture.Add("collaboration_events", map[string]any{"id": id, "sequence": i + 1, "type": eventType, "schema_version": 1, "actor_json": string(marshal(sourceActor)), "data_json": string(marshal(data)), "event_revision": int(data.Revision), "recorded_at": at.Format(time.RFC3339Nano), "organization_id": organization, "project_id": f.project.ID, "work_item_id": item})
						expectedEvent := tracker.CollaborationEvent{ID: id, OrganizationID: organization, ProjectID: f.project.ID, AggregateType: "work_item", AggregateID: item, AggregateSequence: int64(i + 1), Type: eventType, SchemaVersion: 1, RecordedAt: at, Actor: wantEventActor, Data: data}
						expectedHistory[item] = append(expectedHistory[item], expectedEvent)
						var expectedPayload map[string]any
						if err := json.Unmarshal(marshal(expectedEvent), &expectedPayload); err != nil {
							t.Fatal(err)
						}
						var expectedActor map[string]any
						if err := json.Unmarshal(marshal(wantEventActor), &expectedActor); err != nil {
							t.Fatal(err)
						}
						expectedActor["retained_event_extension"] = map[string]any{"value": "historical event evidence", "actor": map[string]any{"kind": "system", "principal_id": "health_detector"}}
						expectedPayload["actor"] = expectedActor
						expectedEventPayloads[id] = expectedPayload
					}
				}
				source := coreimport.CoreWorkItemImportSource{BundlePath: fixture.Write(), OrganizationID: organization, ProjectID: f.project.ID, Objects: objects}
				sourceBytes := func() map[string][]byte {
					t.Helper()
					entries, err := os.ReadDir(source.BundlePath)
					if err != nil {
						t.Fatal(err)
					}
					values := make(map[string][]byte, len(entries))
					for _, entry := range entries {
						if entry.IsDir() {
							t.Fatal("unexpected source subdirectory")
						}
						raw, err := os.ReadFile(filepath.Join(source.BundlePath, entry.Name()))
						if err != nil {
							t.Fatal(err)
						}
						values[entry.Name()] = raw
					}
					return values
				}
				before := sourceBytes()
				parent := t
				for _, step := range []string{"repair", "restart"} {
					t.Run(string(organization)+"/"+step, func(t *testing.T) {
						if step == "restart" {
							previous := service
							if err := previous.Close(); err != nil {
								t.Fatal(err)
							}
							service = reopenNativeAttemptService(parent, previous)
							repository = service.database.postgres
							first.service, f.service = service, service
						}
						count, err := coreimport.ImportCoreWorkItems(t.Context(), repository, source)
						if err != nil || count != 2 {
							t.Fatalf("import count=%d error=%v", count, err)
						}
						if after := sourceBytes(); !reflect.DeepEqual(before, after) {
							t.Fatal("source bundle changed")
						}
						if err := repository.ReadRecords(t.Context(), organization, corestore.Serving, func(r eventstore.Reader) error {
							for _, suffix := range []string{"", "_second"} {
								item := tracker.NativeWorkItemID("wi_core_import" + suffix)
								events, err := r.Events(t.Context(), f.project.ID, item, 0, 10)
								if err != nil {
									return err
								}
								var history []tracker.CollaborationEvent
								for _, event := range events {
									value, err := eventstore.NewContent(objects).Event(t.Context(), event)
									if err != nil {
										return err
									}
									raw, err := objects.ReadDetail(t.Context(), string(organization), eventstore.Reference(event.PayloadObjectKey, event.PayloadSHA256, event.PayloadBytes, event.PayloadContentBytes))
									if err != nil {
										return err
									}
									var payload map[string]any
									if err := json.Unmarshal(raw, &payload); err != nil {
										return err
									}
									if want := expectedEventPayloads[event.ID]; !reflect.DeepEqual(payload, want) {
										t.Fatalf("event payload changed beyond top-level actor kind: got=%+v want=%+v", payload, want)
									}
									history = append(history, value)
								}
								if !reflect.DeepEqual(expectedHistory[item], history) {
									t.Fatalf("history changed beyond actor kind: got=%+v want=%+v", history, expectedHistory[item])
								}
								versions := []struct {
									record   string
									revision int64
								}{{string(item), 1}, {"comment_core_import" + suffix, 1}, {"comment_core_import" + suffix, 2}}
								for _, key := range versions {
									row, err := r.CollaborationVersion(t.Context(), key.record, key.revision)
									if err != nil {
										return err
									}
									raw, err := eventstore.NewContent(objects).CollaborationVersion(t.Context(), row)
									if err != nil {
										return err
									}
									var got map[string]any
									if err := json.Unmarshal(raw, &got); err != nil {
										return err
									}
									if want := expectedVersions[fmt.Sprintf("%s/%d", key.record, key.revision)]; !reflect.DeepEqual(got, want) {
										t.Fatalf("saved revision changed beyond actor kind: got=%+v want=%+v", got, want)
									}
								}

							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
						for _, suffix := range []string{"", "_second"} {
							item := tracker.NativeWorkItemID("wi_core_import" + suffix)
							var comments tracker.Page[tracker.NativeComment]
							response := performHubAPIRequest(t, service, http.MethodGet, f.base+"/work-items/"+string(item)+"/comments", f.token, nil)
							requireNativeStatus(t, response, http.StatusOK)
							decodeHubResponse(t, response, &comments)
							if len(comments.Items) != 1 || comments.Items[0].Actor != wantActor || comments.Items[0].EditedBy == nil || *comments.Items[0].EditedBy != wantEditor || comments.Items[0].Revision != 2 || comments.Items[0].Body != "Retained occurrence" {
								t.Fatalf("comment actor/editor intent lost: %+v", comments)
							}
						}
						var refreshed []tracker.NativeWorkItemID
						for cursor := tracker.NativeWorkItemID(""); ; {
							next, processed, err := service.maintainBoardRecord(t.Context(), organization, cursor, refreshed...)
							if err != nil {
								t.Fatal(err)
							}
							if !processed {
								break
							}
							cursor, refreshed = next, append(refreshed, next)
						}
						var page tracker.NativeIssuePage
						response := performHubAPIRequest(t, service, http.MethodGet, f.base+"/work-items?archived=all", f.token, nil)
						requireNativeStatus(t, response, http.StatusOK)
						decodeHubResponse(t, response, &page)
						for _, item := range []tracker.NativeWorkItemID{"wi_core_import", "wi_core_import_second"} {
							if !slices.ContainsFunc(page.Items, func(value tracker.NativeIssue) bool {
								return value.WorkItemID == item && value.Actor == wantActor && value.State == test.state
							}) {
								t.Fatal("repaired actor missing from Work list")
							}
						}
					})
				}
				config := postgresStartupTemplate.NativeConfig(t, admin, service.config.Postgres.Connection.User, service.config.Postgres.Connection.Password)
				connection, err := pgx.ConnectConfig(t.Context(), config.ConnConfig)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.DefaultCleanupBudget)
					defer cancel()
					if err := connection.Close(cleanup); err != nil {
						t.Error(err)
					}
				})
				for _, table := range []string{"collaboration_events", "collaboration_versions"} {
					for _, statement := range []string{"UPDATE " + table + " SET work_item_id=work_item_id WHERE organization_id=$1 AND project_id=$2", "DELETE FROM " + table + " WHERE organization_id=$1 AND project_id=$2"} {
						tx, err := connection.Begin(t.Context())
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.DefaultCleanupBudget)
							defer cancel()
							if err := tx.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
								t.Error(err)
							}
						})
						if _, err := tx.Exec(t.Context(), "SELECT set_config('detent.organization_id', $1, true)", string(organization)); err != nil {
							t.Fatal(err)
						}
						_, writeErr := tx.Exec(t.Context(), statement, organization, f.project.ID)
						var failure *pgconn.PgError
						if !errors.As(writeErr, &failure) || failure.Code != "23514" {
							t.Fatalf("history immutability lost: %s: %v", statement, writeErr)
						}
						cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.DefaultCleanupBudget)
						rollbackErr := tx.Rollback(cleanup)
						cancel()
						if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
							t.Fatal(rollbackErr)
						}
					}
				}
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.DefaultCleanupBudget)
				closeErr := connection.Close(cleanup)
				cancel()
				if closeErr != nil {
					t.Fatal(closeErr)
				}
			}
		})
	}
}
