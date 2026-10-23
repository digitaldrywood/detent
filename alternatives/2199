package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type convertedConversationContent struct{ service *Service }

func (c convertedConversationContent) PublishContent(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	return prepareChangeContent(c.service.contentContext(ctx), organization, data)
}

func (c convertedConversationContent) ReadContent(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	return readChangeContent(c.service.contentContext(ctx), organization, ref)
}

func TestStorageConversionHistoricalConversations(t *testing.T) {
	f := newStorageArchiveFixture(t)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	for _, status := range []conversation.Status{conversation.StatusActive, conversation.StatusSettled} {
		f.store.contents = nil
		record := f.record(f.owner, "legacy conversation", f.now)
		record.Status = status
		record = f.create(t, record)
		tx := f.tx(t)
		old := strings.Repeat("historical message", 1024)
		latest := strings.Repeat("latest message", 1024)
		message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: latest, Data: json.RawMessage(`{"answer":"latest"}`), Delivery: conversation.DeliveryCompleted, CreatedAt: f.now, UpdatedAt: f.now, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: "example-runner"}}
		if err := f.store.appendMessage(t.Context(), tx, &message); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.appendEvent(t.Context(), tx, record.ID, conversation.EventMessageAccepted, map[string]any{"id": message.ID, "text": old, "data": map[string]string{"answer": "historical"}}, f.now); err != nil {
			t.Fatal(err)
		}
		f.commit(t, tx)
		f.store.contents = convertedConversationContent{service: f.service}
		before, err := f.store.listMessages(t.Context(), f.store.reader, record.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(before) != 1 {
			t.Fatalf("messages before conversion = %d, want 1", len(before))
		}
		events, err := f.store.listEvents(t.Context(), f.store.reader, record.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 {
			t.Fatalf("events before conversion = %d, want 1", len(events))
		}
		if err := f.service.convertLegacyStorage(t.Context(), f.now); err != nil {
			t.Fatal(err)
		}
		progress, err := readStorageConversionProgress(t.Context(), f.service.database.reader, scope)
		if err != nil {
			t.Fatal(err)
		}
		converted, found := false, false
		for _, p := range progress {
			if p.Family == "messages" && p.Aggregate == record.ID {
				found = true
				converted = p.Complete && p.Rows == 1
			}
		}
		if !found || converted != (status == conversation.StatusSettled) {
			t.Fatalf("status=%s converted=%v", status, converted)
		}
		after, err := f.store.listMessages(t.Context(), f.store.reader, record.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != 1 || after[0].Text != before[0].Text || !bytes.Equal(after[0].Data, before[0].Data) {
			t.Fatal("latest message changed")
		}
		afterEvents, err := f.store.listEvents(t.Context(), f.store.reader, record.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(afterEvents) != 1 || !bytes.Equal(events[0].Data, afterEvents[0].Data) {
			t.Fatal("historical message was replaced with latest content")
		}
	}
}

func TestStorageConversionResumesAcrossScopes(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
	tenant := newNativeFixture(t, f.service, "", "other-tenant")
	objects := &changeObjectFixture{}
	f.service.config.content = objects.call
	for _, fixture := range []nativeFixture{f, other, tenant} {
		fixture.service = f.service
		issue := fixture.create(t, "legacy history")
		scope := nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID}
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		for i := range 205 {
			data := tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: fmt.Sprintf("attempt_legacy_%d", i), Sequence: int64(i + 1), CompletionBody: strings.Repeat("historical output", 2048)}}
			if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", data, f.service.config.now()); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		path := fixture.base + "/work-items/" + string(issue.WorkItemID) + "/history?limit=200"
		before := performHubAPIRequest(t, f.service, http.MethodGet, path, fixture.token, nil)
		requireNativeStatus(t, before, http.StatusOK)
		for pass, want := range []int64{100, 200, 205} {
			if err := f.service.convertStorageFamily(f.service.contentContext(t.Context()), scope, storageConversionFamilies[0], f.service.config.now()); err != nil {
				t.Fatal(err)
			}
			progress, err := readStorageConversionProgress(t.Context(), f.service.database.reader, scope)
			if err != nil || len(progress) != 1 || progress[0].Rows != want || progress[0].Complete != (pass == 2) || progress[0].Bytes <= 0 {
				t.Fatalf("pass %d progress=%+v err=%v", pass, progress, err)
			}
			var maximum int
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT max(length(CAST(data_json AS BLOB))) FROM collaboration_events WHERE organization_id=? AND project_id=? AND json_type(data_json,'$._storage_archive')='object'`, scope.organization, scope.project).Scan(&maximum); err != nil || maximum > storageConversionBytes {
				t.Fatalf("reference write bytes=%d err=%v", maximum, err)
			}
			if pass == 0 {
				cfg := f.service.config
				if err := f.service.Close(); err != nil {
					t.Fatal(err)
				}
				f.service = openTestService(t, cfg)
			}
		}
		after := performHubAPIRequest(t, f.service, http.MethodGet, path, fixture.token, nil)
		requireNativeStatus(t, after, http.StatusOK)
		if !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
			t.Fatal("history changed after conversion")
		}
		if err := f.service.convertStorageFamily(f.service.contentContext(t.Context()), scope, storageConversionFamilies[0], f.service.config.now()); err != nil {
			t.Fatal(err)
		}
	}
	if len(objects.objects) != 615 {
		t.Fatalf("objects=%d want 615 immutable row snapshots", len(objects.objects))
	}
}

func TestStorageConversionPublicationFailures(t *testing.T) {
	for _, phase := range []string{"uploaded", "verification", "commit", "revision", "live lease"} {
		t.Run(phase, func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			issue := f.nativeFixture.create(t, "immutable snapshot")
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			tx := f.tx(t)
			if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", tracker.CollaborationData{Run: &tracker.NativeRunData{CompletionBody: strings.Repeat("original", 4096)}}, f.now); err != nil {
				t.Fatal(err)
			}
			f.commit(t, tx)
			var original string
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT data_json FROM collaboration_events WHERE work_item_id=? AND type='run.finished'", issue.WorkItemID).Scan(&original); err != nil {
				t.Fatal(err)
			}
			f.hook = func(r entryauthority.ContentRequest) error {
				if !r.Write && phase == "verification" {
					return errors.New("verification failed")
				}
				if !r.Write {
					return nil
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET revision=revision WHERE native_id=?", issue.WorkItemID); err != nil {
					return err
				}
				switch phase {
				case "uploaded":
					return errors.New("upload interrupted after durable write")
				case "revision":
					_, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET revision=revision+1 WHERE native_id=?", issue.WorkItemID)
					return err
				case "live lease":
					approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
					claimNativeAttempt(t, f.nativeFixture, f.worker(t, "worker"), "machine", "session", issue.WorkItemID)
				}
				return nil
			}
			if phase == "commit" {
				if _, err := f.service.database.db.ExecContext(t.Context(), `CREATE TRIGGER conversion_interrupt BEFORE UPDATE ON storage_conversion_progress WHEN NEW.converted_rows>OLD.converted_rows BEGIN SELECT RAISE(ABORT,'conversion interrupted'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err := f.service.convertLegacyStorage(t.Context(), f.now)
			if (err != nil) != (phase == "uploaded" || phase == "verification" || phase == "commit") {
				t.Fatalf("conversion err=%v", err)
			}
			var actual string
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT data_json FROM collaboration_events WHERE work_item_id=? AND type='run.finished'", issue.WorkItemID).Scan(&actual); err != nil {
				t.Fatal(err)
			}
			if actual != original {
				t.Fatal("source retired before verified reference commit")
			}
			if phase == "live lease" {
				return
			}
			f.hook = nil
			if phase == "commit" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER conversion_interrupt"); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.convertLegacyStorage(t.Context(), f.now); err != nil {
				t.Fatal(err)
			}
			if len(f.objects) != 1 {
				t.Fatalf("restart stored %d objects", len(f.objects))
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT data_json FROM collaboration_events WHERE work_item_id=? AND type='run.finished'", issue.WorkItemID).Scan(&actual); err != nil {
				t.Fatal(err)
			}
			resolved, err := resolveStorageJSON(f.service.contentContext(t.Context()), []byte(actual))
			if err != nil || !bytes.Equal(resolved, []byte(original)) {
				t.Fatalf("historical content changed: %v", err)
			}
		})
	}
}

func TestStorageConversionHistoricalJSONScope(t *testing.T) {
	f := newStorageArchiveFixture(t)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	raw := []byte(`{"output":"` + strings.Repeat("old snapshot", 2048) + `","runtime":{"landing":{"waiting":true}},"usage":{"output":42}}`)
	ctx := f.service.contentContext(t.Context())
	converted, _, err := f.service.prepareStorageConversion(ctx, scope, storageConversionFamilies[0], 1, raw)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(converted, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope == nil {
		t.Fatal("conversion returned a null envelope")
	}
	envelope["runtime"] = json.RawMessage(`{"landing":{"waiting":false,"landed":true}}`)
	updated, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveStorageJSON(ctx, updated)
	if err != nil {
		t.Fatal(err)
	}
	var run struct {
		Output  string `json:"output"`
		Runtime struct {
			Landing struct {
				Waiting bool `json:"waiting"`
				Landed  bool `json:"landed"`
			} `json:"landing"`
		} `json:"runtime"`
		Usage struct {
			Output int64 `json:"output"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(resolved, &run); err != nil {
		t.Fatal(err)
	}
	if run.Output != strings.Repeat("old snapshot", 2048) || run.Runtime.Landing.Waiting || !run.Runtime.Landing.Landed || run.Usage.Output != 42 {
		t.Fatal("mutable landing metadata or immutable historical content changed")
	}
	var ref storageJSONReference
	if err := json.Unmarshal(envelope["_storage_archive"], &ref); err != nil {
		t.Fatal(err)
	}
	ref.Project = "other-project"
	envelope["_storage_archive"], err = json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	tampered, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveStorageJSON(ctx, tampered); err == nil {
		t.Fatal("accepted content from another project")
	}
}

func TestStorageConversionByteBudgetAndVersion(t *testing.T) {
	f := newStorageArchiveFixture(t)
	issue := f.nativeFixture.create(t, "bounded legacy rows")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	tx := f.tx(t)
	for range 11 {
		if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", tracker.CollaborationData{ReasonDetail: strings.Repeat("x", 180<<10), Run: &tracker.NativeRunData{CompletionBody: strings.Repeat("old output", 4096)}}, f.now); err != nil {
			t.Fatal(err)
		}
	}
	f.commit(t, tx)
	var through int64
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT max(rowid) FROM collaboration_events WHERE work_item_id=?", issue.WorkItemID).Scan(&through); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,cursor_rowid,complete,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, scope.organization, scope.project, "runs", issue.WorkItemID, 0, through, through, 1, formatHubTime(f.now)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int64{5, 10, 11} {
		var before int64
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT coalesce(sum(length(CAST(data_json AS BLOB))),0) FROM collaboration_events WHERE work_item_id=? AND json_type(data_json,'$._storage_archive')='object'", issue.WorkItemID).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if err := f.service.convertLegacyStorage(t.Context(), f.now); err != nil {
			t.Fatal(err)
		}
		var after int64
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT coalesce(sum(length(CAST(data_json AS BLOB))),0) FROM collaboration_events WHERE work_item_id=? AND json_type(data_json,'$._storage_archive')='object'", issue.WorkItemID).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after-before > storageConversionBytes {
			t.Fatalf("chunk wrote %d payload bytes", after-before)
		}
		progress, err := readStorageConversionProgress(t.Context(), f.service.database.reader, scope)
		if err != nil || len(progress) != 1 || progress[0].Version != storageConversionVersion || progress[0].Rows != want || progress[0].Complete != (want == 11) {
			t.Fatalf("progress=%+v err=%v", progress, err)
		}
	}
}
