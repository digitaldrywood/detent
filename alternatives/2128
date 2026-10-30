package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
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
		if err := f.insertLegacyMessage(t, t.Context(), tx, &message); err != nil {
			t.Fatal(err)
		}
		if _, err := f.insertLegacyEvent(t, t.Context(), tx, record.ID, conversation.EventMessageAccepted, map[string]any{"id": message.ID, "text": old, "data": map[string]string{"answer": "historical"}}, f.now); err != nil {
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
		if converted && (!after[0].StoredTextBytes.Valid || after[0].StoredTextBytes.Int64 != int64(len(latest))) {
			t.Fatalf("converted message stored text length=%+v, want %d", after[0].StoredTextBytes, len(latest))
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
	f.service.config.Content = objects.call
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
			if err := appendLegacyNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", data, f.service.config.now()); err != nil {
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
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT coalesce(max(length(CAST(data_json AS BLOB))),0) FROM collaboration_events WHERE organization_id=? AND project_id=? AND payload_version=1`, scope.organization, scope.project).Scan(&maximum); err != nil || maximum > storageConversionBytes {
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
	if count := len(storageTestArchiveObjects(t, f.service, objects.objects)); count != 615 {
		t.Fatalf("objects=%d want 615 immutable row snapshots", count)
	}
}

func TestStorageConversionPublicationFailures(t *testing.T) {
	for _, phase := range []string{"uploaded", "verification", "commit", "revision", "live lease"} {
		t.Run(phase, func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			issue := f.nativeFixture.create(t, "immutable snapshot")
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			tx := f.tx(t)
			if err := appendLegacyNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", tracker.CollaborationData{Run: &tracker.NativeRunData{CompletionBody: strings.Repeat("original", 4096)}}, f.now); err != nil {
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
					f.hook = nil
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
			if (phase == "uploaded" || phase == "verification" || phase == "commit") && actual != original {
				t.Fatal("source retired before verified reference commit")
			}
			objectsBeforeRestart := len(f.objects)
			f.hook = nil
			if phase == "commit" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER conversion_interrupt"); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.convertLegacyStorage(t.Context(), f.now); err != nil {
				t.Fatal(err)
			}
			if len(f.objects) != objectsBeforeRestart {
				t.Fatalf("restart stored %d objects, want %d", len(f.objects), objectsBeforeRestart)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT "+collaborationPayloadColumn("")+" FROM collaboration_events WHERE work_item_id=? AND type='run.finished'", issue.WorkItemID).Scan(&actual); err != nil {
				t.Fatal(err)
			}
			resolved, err := resolveCollaborationPayload(f.service.contentContext(t.Context()), f.service.database.reader, actual)
			if err != nil || !bytes.Equal([]byte(resolved), []byte(original)) {
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
	converted, _, err := f.service.prepareStorageConversion(ctx, scope, storageConversionFamilies[1], 1, raw)
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
	for _, test := range []struct {
		name              string
		detailBytes, rows int
	}{
		{"batch budget", 180 << 10, 11},
		{"oversized row advances", 2 << 20, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			issue := f.nativeFixture.create(t, "bounded legacy rows")
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			tx := f.tx(t)
			for range test.rows {
				if err := appendLegacyNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", tracker.CollaborationData{ReasonDetail: strings.Repeat("x", test.detailBytes), Run: &tracker.NativeRunData{CompletionBody: strings.Repeat("old output", 4096)}}, f.now); err != nil {
					t.Fatal(err)
				}
			}
			f.commit(t, tx)
			var through int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT max(rowid) FROM collaboration_events WHERE work_item_id=?", issue.WorkItemID).Scan(&through); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,cursor_rowid,complete,updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET format_version=0,cursor_rowid=excluded.cursor_rowid,complete=1`, scope.organization, scope.project, "runs", issue.WorkItemID, 0, through, through, 1, formatHubTime(f.now)); err != nil {
				t.Fatal(err)
			}
			converted := int64(0)
			previousRows, previousBytes, previousOversized := 0, 0, 0
			for tick := 0; tick < 12 && converted < int64(test.rows); tick++ {
				var sourceBefore, maximumSource int64
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT sum(length(CAST(data_json AS BLOB))),max(length(CAST(data_json AS BLOB))) FROM collaboration_events WHERE work_item_id=? AND type='run.finished'`, issue.WorkItemID).Scan(&sourceBefore, &maximumSource); err != nil {
					t.Fatal(err)
				}
				if err := f.service.convertLegacyStorage(t.Context(), f.now); err != nil {
					t.Fatal(err)
				}
				var sourceAfter int64
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT sum(length(CAST(data_json AS BLOB))) FROM collaboration_events WHERE work_item_id=? AND type='run.finished'`, issue.WorkItemID).Scan(&sourceAfter); err != nil {
					t.Fatal(err)
				}
				if removed := sourceBefore - sourceAfter; removed > max(1<<20, maximumSource) {
					t.Fatalf("tick removed %d source bytes, limit %d", removed, max(1<<20, maximumSource))
				}
				var rows, bytes, oversized int
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT coalesce(sum(examined_rows),0),coalesce(sum(examined_bytes),0),coalesce(sum(oversized_rows),0) FROM storage_conversion_cursors`).Scan(&rows, &bytes, &oversized); err != nil {
					t.Fatal(err)
				}
				rowDelta, byteDelta, oversizedDelta := rows-previousRows, bytes-previousBytes, oversized-previousOversized
				if rowDelta > 100 || (byteDelta > 1<<20 && (oversizedDelta != 1 || rowDelta != 1)) {
					t.Fatalf("tick=%d rows=%d bytes=%d oversized=%d", tick, rowDelta, byteDelta, oversizedDelta)
				}
				previousRows, previousBytes, previousOversized = rows, bytes, oversized
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT converted_rows FROM storage_conversion_progress WHERE organization_id=? AND project_id=? AND family='runs' AND aggregate_id=?`, scope.organization, scope.project, issue.WorkItemID).Scan(&converted); err != nil {
					t.Fatal(err)
				}
			}
			if converted != int64(test.rows) {
				t.Fatalf("conversion stalled: rows=%d want=%d", converted, test.rows)
			}
			progress, err := readStorageConversionProgress(t.Context(), f.service.database.reader, scope)
			if err != nil || len(progress) != 1 || progress[0].Version != storageConversionVersion || !progress[0].Complete {
				t.Fatalf("progress=%+v err=%v", progress, err)
			}

		})
	}
}

func storageSchemaVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var version int
	if err := db.QueryRowContext(t.Context(), "PRAGMA schema_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func TestStorageConversionImmutability(t *testing.T) {
	f := newStorageArchiveFixture(t)
	db := f.service.database
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	ctx := f.service.contentContext(t.Context())
	bundle := []byte("# v2 git bundle\n-legacy-base prerequisite\nlegacy-head HEAD\n\n")
	bundle = append(bundle, bytes.Repeat([]byte("x"), 8192-len(bundle))...)
	source, err := json.Marshal(tracker.ChangeSource{Format: "git-bundle", BaseSHA: "legacy-base", HeadSHA: "legacy-head", BundleSHA256: tracker.ChangeSourceDigest(bundle), DiffSHA256: strings.Repeat("a", 64), Bytes: int64(len(bundle))})
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"policy_id":"legacy-policy","base_sha":"legacy-base","head_sha":"legacy-head","policy":{"gates":{"github_pull_request":false}},"actor":{"principal_id":"reviewer"},"decision":"approved","source":` + string(source) + `,"output":"` + strings.Repeat("x", 8192) + `"}`
	for i := range 2 {
		issue := f.nativeFixture.create(t, fmt.Sprintf("immutable history %d", i))
		change, version := fmt.Sprintf("conversion-change-%d", i), fmt.Sprintf("conversion-version-%d", i)
		tx := f.tx(t)
		for _, insert := range []struct {
			query string
			args  []any
		}{
			{`INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json) VALUES(?,?,?,?, '{}')`, []any{change, scope.organization, scope.project, issue.WorkItemID}},
			{`INSERT INTO change_versions(id,change_id,number,record_json) VALUES(?,?,1,?)`, []any{version, change, raw}},
			{`INSERT INTO change_evidence(change_id,version_id,kind,source_key,record_json) VALUES(?,?,'review',NULL,?)`, []any{change, version, raw}},
			{`INSERT INTO change_evidence(change_id,kind,source_key,record_json) VALUES(?,'discussion','source','{}')`, []any{change}},
			{`INSERT INTO change_sources(version_id,bundle) VALUES(?,?)`, []any{version, bundle}},
		} {
			if _, err := tx.ExecContext(t.Context(), insert.query, insert.args...); err != nil {
				t.Fatal(err)
			}
		}
		if err := appendLegacyNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", tracker.CollaborationData{Run: &tracker.NativeRunData{CompletionBody: strings.Repeat("output", 2048)}}, f.now); err != nil {
			t.Fatal(err)
		}
		f.commit(t, tx)
	}
	substitution, err := encodeChangeSource(bytes.Repeat([]byte("b"), 8192))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeChangeSource(substitution, 8192)
	if err != nil || !bytes.Equal(decoded, bytes.Repeat([]byte("b"), 8192)) || len(substitution) >= 8192 || len(substitution) < 6 || substitution[5] != 1 {
		t.Fatalf("invalid substitution fixture: %v", err)
	}
	if _, err := db.db.ExecContext(t.Context(), "UPDATE change_sources SET bundle=? WHERE version_id='conversion-version-0'", substitution); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("unauthorized compressed substitution: %v", err)
	}
	assertImmutable := func() {
		t.Helper()
		for _, table := range []struct {
			name    string
			allowed map[string]bool
		}{
			{"change_versions", map[string]bool{
				"claim_head_sha": true, "gate_github_pull_request": true, "external_url": true, "activity_external_url": true,
				"created_at": true, "attempt_id": true, "head_sha": true, "repository": true, "external_provider": true, "external_id": true,
				"implementing_session_id": true, "implementing_machine_id": true, "session_history_sequence": true,
				"session_history_candidate": true, "session_history_state": true, "session_history_machine_id": true,
			}},
			{"change_evidence", map[string]bool{
				"claim_review_kind": true, "claim_review_actor": true, "claim_review_decision": true, "claim_review_current": true,
				"has_implementation_review": true, "has_validator": true, "has_security_audit": true, "security_audit_landing": true,
				"check_run_id": true, "created_at": true, "received_at": true, "escape_occurrence_id": true, "escape_observed_at": true,
			}},
			{"change_sources", nil},
			{"collaboration_events", map[string]bool{
				"scheduler_outcome": true, "scheduler_reason": true, "scheduler_source": true, "scheduler_runner_id": true,
				"health_projected": true, "scheduler_present": true, "scheduler_at": true, "scheduler_revision": true,
				"transition_from": true, "transition_to": true,
				"transition_reason": true, "transition_detail": true, "transition_revision": true,
				"event_landed": true, "event_landing_at": true,
				"analytics_recorded_ns": true, "analytics_from_state": true, "analytics_to_state": true,
				"analytics_attempt_id": true, "analytics_skip_key": true, "analytics_projection_version": true,
				"query_metadata_version": true, "run_attempt_id": true, "activity_spans_present": true,
				"claim_attempt_id": true, "run_id": true, "run_lease_id": true, "run_fencing_token": true,
				"run_sequence": true, "run_policy_id": true, "push_change_id": true, "push_version_id": true,
				"push_head_sha": true, "push_external_effect": true, "push_effect_state": true,
				"archive_operation": true, "archive_from_state": true, "archive_to_state": true,
				"archive_attempt_id": true, "archive_outcome": true, "archive_landed_commit": true,
				"page_projection_ready": true, "actor_kind": true, "actor_principal_id": true, "event_revision": true,
				"from_state": true, "to_state": true, "reason": true, "run_present": true,
				"edited_title": true, "edited_body": true,
				"scheduler_change_id": true, "scheduler_version_id": true, "scheduler_head_sha": true, "scheduler_change_present": true,
				"run_outcome": true, "run_machine_id": true, "run_session_id": true, "run_runner_id": true, "push_effect_id": true, "push_checkpoint_head_sha": true,
			}},
		} {
			rows, err := db.reader.QueryContext(t.Context(), "PRAGMA table_info("+table.name+")")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			columns := []string{"rowid"}
			for rows.Next() {
				var cid, notnull, pk int
				var name, kind string
				var def sql.NullString
				if err := rows.Scan(&cid, &name, &kind, &notnull, &def, &pk); err != nil {
					t.Fatal(err)
				}
				if name != "conversion_state" && !table.allowed[name] {
					columns = append(columns, name)
				}
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				t.Fatal(err)
			}
			for _, column := range columns {
				value := "'tampered'"
				if column == "rowid" || column == "sequence" || column == "number" || column == "schema_version" || column == "object_bytes" || column == "content_bytes" {
					value = column + "+100000"
				}
				if column == "bundle" {
					value = "x'01'"
				}
				if column == "record_json" || column == "data_json" || column == "actor_json" {
					value = `'{"tampered":true}'`
				}
				_, err := db.db.ExecContext(t.Context(), "UPDATE "+table.name+" SET "+column+"="+value)
				if err == nil || !strings.Contains(err.Error(), "immutable") {
					t.Fatalf("%s.%s update: %v", table.name, column, err)
				}
			}
			if _, err := db.db.ExecContext(t.Context(), "DELETE FROM "+table.name); err == nil || !strings.Contains(err.Error(), "immutable") {
				t.Fatalf("%s delete: %v", table.name, err)
			}
		}
		for _, assignment := range []string{"version_id=NULL", "source_key=NULL", `record_json='{"tampered":true}',claim_review_current=0`} {
			if _, err := db.db.ExecContext(t.Context(), "UPDATE change_evidence SET "+assignment+" WHERE kind='review' AND source_key IS NULL OR kind='discussion'"); err == nil || !strings.Contains(err.Error(), "immutable") {
				t.Fatalf("evidence %s: %v", assignment, err)
			}
		}
	}
	for _, mismatch := range []bool{false, true} {
		var row int64
		var aggregate string
		if err := db.reader.QueryRowContext(t.Context(), `SELECT v.rowid,c.work_item_id FROM change_versions v JOIN change_requests c ON c.id=v.change_id WHERE v.id='conversion-version-0'`).Scan(&row, &aggregate); err != nil {
			t.Fatal(err)
		}
		family := storageConversionFamilies[5]
		converted, ref, err := f.service.prepareStorageConversion(ctx, scope, family, row, []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		tx := f.tx(t)
		prior := raw
		if mismatch {
			prior = "wrong prior content"
		}
		if err := authorizeStorageConversion(t.Context(), tx, scope, family, aggregate, row, []byte(prior), converted, ref); err != nil && !mismatch {
			t.Fatal(err)
		}
		for _, update := range []struct {
			query string
			value string
		}{
			{`UPDATE change_versions SET record_json=? WHERE id='conversion-version-0'`, `{"tampered":true}`},
			{`UPDATE change_versions SET record_json=? WHERE id='conversion-version-1'`, string(converted)},
			{`UPDATE change_versions SET record_json=?,policy_revision_id='tampered' WHERE id='conversion-version-0'`, string(converted)},
		} {
			if _, err := tx.ExecContext(t.Context(), update.query, update.value); err == nil || !strings.Contains(err.Error(), "immutable") {
				t.Fatalf("conversion exception allowed a different mutation: %v", err)
			}
		}
		_, err = tx.ExecContext(t.Context(), `UPDATE change_versions SET record_json=? WHERE id='conversion-version-0'`, string(converted))
		if (err != nil) != mismatch {
			t.Fatalf("prior content mismatch=%v: %v", mismatch, err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	assertImmutable()
	before := storageSchemaVersion(t, db.reader)
	if err := f.service.convertObjectPayloadFamily(ctx, storageConversionFamilies[5], f.now); err != nil {
		t.Fatal(err)
	}
	if err := f.service.populateChangeColumns(ctx); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		var policy, head, stored string
		if err := db.reader.QueryRowContext(t.Context(), `SELECT policy_revision_id,head_sha,record_json FROM change_versions WHERE id=?`, fmt.Sprintf("conversion-version-%d", i)).Scan(&policy, &head, &stored); err != nil {
			t.Fatal(err)
		}
		if policy != "legacy-policy" || head != "legacy-head" || stored != "{}" {
			t.Fatalf("version %d backfill changed content or missed typed columns: policy=%q head=%q", i, policy, head)
		}
	}
	assertImmutable()
	if err := db.populateClaimFacts(ctx); err != nil {
		t.Fatal(err)
	}
	for _, family := range storageConversionFamilies {
		if err := f.service.convertStorageFamily(ctx, scope, family, f.now); err != nil {
			t.Fatalf("%s: %v", family.name, err)
		}
	}
	if after := storageSchemaVersion(t, db.reader); after != before {
		t.Fatalf("maintenance changed schema: %d -> %d", before, after)
	}
	assertImmutable()
	for _, table := range []string{"change_versions", "change_evidence"} {
		filter := ""
		if table == "change_evidence" {
			filter = " AND kind='review'"
		}
		rows, err := db.reader.QueryContext(t.Context(), "SELECT payload_object_key FROM "+table+" WHERE payload_object_key<>''"+filter)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var stored string
			if err := rows.Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(stored, "orgs/") {
				t.Fatalf("%s was not converted", table)
			}
			resolved, err := resolveStorageJSON(ctx, []byte(stored))
			if err != nil || string(resolved) != raw {
				t.Fatalf("%s content changed: %v", table, err)
			}
			count++
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		if count != 2 {
			t.Fatalf("%s converted rows=%d", table, count)
		}
	}
	rows, err := db.reader.QueryContext(t.Context(), `SELECT length(bundle),object_key,object_sha256,object_bytes,content_bytes FROM change_sources`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	sources := 0
	for rows.Next() {
		var inline int
		ref := contentReference()
		if err := rows.Scan(&inline, &ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize); err != nil {
			t.Fatal(err)
		}
		if inline != 0 || ref.Key == "" {
			t.Fatal("source was not converted")
		}
		content, err := readChangeContent(ctx, string(scope.organization), ref)
		if err != nil || !bytes.Equal(content, bundle) {
			t.Fatalf("converted source changed: %v", err)
		}
		sources++
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	if sources != 2 {
		t.Fatalf("converted sources=%d", sources)
	}
	var authorized int
	if err := db.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM storage_conversion_progress WHERE conversion_rowid<>0 OR conversion_before_sha256<>'' OR conversion_after_sha256<>''").Scan(&authorized); err != nil || authorized != 0 {
		t.Fatalf("conversion authority leaked: %d %v", authorized, err)
	}
}

func TestStorageConversionDiscoveryBounded(t *testing.T) {
	f := newStorageArchiveFixture(t)
	db := f.service.database
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	for i := range 2 {
		issue := f.nativeFixture.create(t, fmt.Sprintf("converted history %d", i))
		if _, err := db.db.ExecContext(t.Context(), `WITH RECURSIVE history(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM history WHERE n<6000)
INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at,payload_object_key,payload_version)
SELECT ?||n,?,?,?,n+100,'run.finished',1,'{}','{}',?,'example-object-'||n,1 FROM history`, fmt.Sprintf("converted-%d-", i), scope.organization, scope.project, issue.WorkItemID, formatHubTime(f.now)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.ExecContext(t.Context(), `UPDATE storage_conversion_progress SET cursor_rowid=through_rowid,complete=1 WHERE family='runs'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(t.Context(), `WITH RECURSIVE history(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM history WHERE n<12000)
INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,cursor_rowid,complete,updated_at)
SELECT ?,?,'runs','completed-'||n,1,n,n,1,'' FROM history`, scope.organization, scope.project); err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		issue := f.nativeFixture.create(t, fmt.Sprintf("pending history %d", i))
		tx := f.tx(t)
		if err := appendLegacyNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", tracker.CollaborationData{Run: &tracker.NativeRunData{CompletionBody: strings.Repeat("pending", 1024)}}, f.now); err != nil {
			t.Fatal(err)
		}
		f.commit(t, tx)
	}
	for _, test := range []struct {
		query, index string
		args         []any
	}{
		{storageConversionDiscovery, "storage_conversion_pending", []any{scope.organization, scope.project, "runs"}},
		{storageConversionSchedule, "storage_conversion_schedule", nil},
	} {
		rows, err := db.reader.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+test.query, test.args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			plan.WriteString(detail)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan.String(), "USING INDEX "+test.index) || strings.Contains(plan.String(), "TEMP B-TREE") {
			t.Fatalf("unbounded discovery plan: %s", plan.String())
		}
	}
	for pass, want := range []int{0, 0} {
		if err := f.service.convertLegacyStorage(t.Context(), f.now.Add(time.Duration(pass)*time.Second)); err != nil {
			t.Fatal(err)
		}
		var pending int
		if err := db.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM storage_conversion_progress WHERE family='runs' AND complete=0`).Scan(&pending); err != nil || pending != want {
			t.Fatalf("pending=%d want=%d: %v", pending, want, err)
		}
	}
}

func TestStorageConversionDiscoveryInitialization(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
	db := f.service.database
	ctx := t.Context()
	migration, err := migrationFiles.ReadFile("migrations/20261026041522_maintenance_without_ddl.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(migration), "-- +goose Down")
	if !ok {
		t.Fatal("missing migration rollback")
	}
	payloadMigration, err := migrationFiles.ReadFile("migrations/20261028195844_conversation_payload_references.sql")
	if err != nil {
		t.Fatal(err)
	}
	payloadUp, payloadDown, ok := strings.Cut(string(payloadMigration), "-- +goose Down")
	if !ok {
		t.Fatal("missing payload migration rollback")
	}
	storedTextMigration, err := migrationFiles.ReadFile("migrations/20261029061743_conversation_stored_text_bytes.sql")
	if err != nil {
		t.Fatal(err)
	}
	storedTextUp, storedTextDown, ok := strings.Cut(string(storedTextMigration), "-- +goose Down")
	if !ok {
		t.Fatal("missing stored text migration rollback")
	}
	if _, err := db.db.ExecContext(ctx, storedTextDown); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, payloadDown); err != nil {
		t.Fatal(err)
	}
	down = strings.ReplaceAll(down, "DROP TRIGGER storage_conversion_enqueue_analytics;", "")
	if _, err := db.db.ExecContext(ctx, down); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []nativeFixture{f, other} {
		if _, err := db.db.ExecContext(ctx, `WITH RECURSIVE history(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM history WHERE n<205)
INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json)
SELECT ?,?,n,'attempt','legacy-'||n,1,'{"obsolete":"payload"}' FROM history`, fixture.project.OrganizationID, fixture.project.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.ExecContext(ctx, up); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, payloadUp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, storedTextUp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, "DROP TRIGGER storage_conversion_enqueue_analytics"); err != nil {
		t.Fatal(err)
	}
	var progress int
	if err := db.reader.QueryRowContext(ctx, "SELECT count(*) FROM storage_conversion_progress WHERE family='analytics'").Scan(&progress); err != nil || progress != 0 {
		t.Fatalf("startup seeded progress=%d: %v", progress, err)
	}
	family := storageConversionFamilies[2]
	if _, err := db.db.ExecContext(ctx, `CREATE TRIGGER interrupt_discovery BEFORE UPDATE OF conversion_state ON analytics_hourly_rollups WHEN NEW.conversion_state=2 BEGIN SELECT RAISE(ABORT,'interrupted discovery'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.service.convertObjectPayloadFamily(f.service.runDetailContext(ctx), family, time.Now()); err == nil {
		t.Fatal("discovery did not propagate interruption")
	}
	if err := db.reader.QueryRowContext(ctx, "SELECT count(*) FROM storage_conversion_progress WHERE family='analytics'").Scan(&progress); err != nil || progress != 0 {
		t.Fatalf("interruption committed progress=%d: %v", progress, err)
	}
	if _, err := db.db.ExecContext(ctx, "DROP TRIGGER interrupt_discovery"); err != nil {
		t.Fatal(err)
	}
	beforeSchema := storageSchemaVersion(t, db.reader)
	for pass, want := range []int{310, 210, 110, 10, 0} {
		if err := f.service.convertObjectPayloadFamily(f.service.runDetailContext(ctx), family, time.Now()); err != nil {
			t.Fatal(err)
		}
		var remaining int
		if err := db.reader.QueryRowContext(ctx, "SELECT count(*) FROM analytics_hourly_rollups WHERE conversion_state=0").Scan(&remaining); err != nil || remaining != want {
			t.Fatalf("pass=%d undiscovered=%d want=%d: %v", pass, remaining, want, err)
		}
		if want > 0 {
			var completed int
			if err := db.reader.QueryRowContext(ctx, "SELECT count(*) FROM storage_conversion_progress p WHERE family='analytics' AND complete=1 AND EXISTS(SELECT 1 FROM analytics_hourly_rollups r WHERE r.organization_id=p.organization_id AND r.project_id=p.project_id AND r.metric='attempt' AND r.payload_object_key='' AND r.data_json<>'{}')").Scan(&completed); err != nil || completed != 0 {
				t.Fatalf("discovery completed prematurely=%d: %v", completed, err)
			}
		}
		if pass == 0 {
			config := f.service.config
			if err := f.service.Close(); err != nil {
				t.Fatal(err)
			}
			f.service = openTestService(t, config)
			db = f.service.database
		}
	}
	for _, fixture := range []nativeFixture{f, other} {
		scope := nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID}
		progress, err := readStorageConversionProgress(ctx, db.reader, scope)
		if err != nil || len(progress) != 1 || !progress[0].Complete || progress[0].Rows != 205 {
			t.Fatalf("initialized scope progress=%+v: %v", progress, err)
		}
	}
	if after := storageSchemaVersion(t, db.reader); after != beforeSchema {
		t.Fatalf("background discovery changed schema: %d -> %d", beforeSchema, after)
	}
}

func TestStorageConversionReusedRowIDs(t *testing.T) {
	for _, explicitLower := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit-lower=%v", explicitLower), func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
			db := f.service.database
			ctx := f.service.runDetailContext(t.Context())
			family := storageConversionFamilies[2]
			for _, fixture := range []nativeFixture{f, other} {
				scope := nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID}
				var prior int64
				for pass := range 2 {
					query := `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json) VALUES(?,?,1,'attempt','legacy',1,'{"obsolete":"payload"}') RETURNING rowid`
					if explicitLower {
						row := int64(100)
						if pass == 1 {
							row = 1
						}
						query = strings.Replace(query, "(organization_id", "(rowid,organization_id", 1)
						query = strings.Replace(query, "VALUES(", fmt.Sprintf("VALUES(%d,", row), 1)
					}
					var row int64
					if err := db.db.QueryRowContext(ctx, query, scope.organization, scope.project).Scan(&row); err != nil {
						t.Fatal(err)
					}
					if pass == 1 && (row > prior || explicitLower && row == prior) {
						t.Fatalf("fixture did not reuse a lower identity: %d -> %d", prior, row)
					}
					if err := f.service.convertStorageFamily(ctx, scope, family, time.Now()); err != nil {
						t.Fatal(err)
					}
					var raw string
					var state int
					if err := db.reader.QueryRowContext(ctx, "SELECT data_json,conversion_state FROM analytics_hourly_rollups WHERE rowid=?", row).Scan(&raw, &state); err != nil || strings.Contains(raw, "obsolete") || state != 2 {
						t.Fatalf("pass=%d unconverted reused row: state=%d data=%s: %v", pass, state, raw, err)
					}
					progress, err := readStorageConversionProgress(ctx, db.reader, scope)
					if err != nil || len(progress) != 1 || !progress[0].Complete || progress[0].Rows != int64(pass+1) {
						t.Fatalf("pass=%d progress=%+v: %v", pass, progress, err)
					}
					if _, err := db.db.ExecContext(ctx, "DELETE FROM analytics_hourly_rollups WHERE rowid=?", row); err != nil {
						t.Fatal(err)
					}
					prior = row
				}
			}
		})
	}
}

func TestConversationConversionClearsOneLegacyPayloadPerTransaction(t *testing.T) {
	for _, size := range []int{32, 64 << 10} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			records := make([]conversationRecord, 0, 2)
			for range 2 {
				record := f.record(f.owner, "legacy payload", f.now)
				record.Status = conversation.StatusSettled
				records = append(records, f.create(t, record))
			}
			for _, record := range records {
				tx := f.tx(t)
				for range 2 {
					message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryCompleted, Text: strings.Repeat("x", size), Data: json.RawMessage(`{"value":"` + strings.Repeat("y", size) + `"}`), Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: f.owner}, CreatedAt: f.now, UpdatedAt: f.now}
					if err := f.insertLegacyMessage(t, t.Context(), tx, &message); err != nil {
						t.Fatal(err)
					}
					if _, err := f.insertLegacyEvent(t, t.Context(), tx, record.ID, conversation.EventHeartbeat, map[string]string{"text": strings.Repeat("z", size)}, f.now); err != nil {
						t.Fatal(err)
					}
				}
				f.commit(t, tx)
			}
			if _, err := f.store.db.ExecContext(t.Context(), "UPDATE conversation_events SET conversion_state=2"); err != nil {
				t.Fatal(err)
			}
			f.store.contents = convertedConversationContent{service: f.service}
			f.hook = func(r entryauthority.ContentRequest) error {
				if r.Write {
					if _, err := f.store.db.ExecContext(t.Context(), "UPDATE conversations SET title=title WHERE id=?", records[0].ID); err != nil {
						return fmt.Errorf("writer held during object publication: %w", err)
					}
				}
				return nil
			}
			for _, family := range storageConversionFamilies[3:5] {
				for _, record := range records {
					beforeMessages, err := f.store.listMessages(t.Context(), f.store.reader, record.ID, 0, 10)
					if err != nil {
						t.Fatal(err)
					}
					beforeEvents, err := f.store.listEvents(t.Context(), f.store.reader, record.ID, 0, 10)
					if err != nil {
						t.Fatal(err)
					}
					for remaining := 1; remaining >= 0; remaining-- {
						if err := f.service.discoverStorageFamily(t.Context(), family, f.now); err != nil {
							t.Fatal(err)
						}
						if err := f.service.convertConversationPayload(t.Context(), scope, family, record.ID, f.now); err != nil {
							t.Fatal(err)
						}
						var pending int
						if err := f.store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM "+family.table()+" WHERE conversation_id=? AND "+family.pending(), record.ID).Scan(&pending); err != nil || pending != remaining {
							t.Fatalf("%s pending=%d want=%d err=%v", family.name, pending, remaining, err)
						}
					}
					afterMessages, err := f.store.listMessages(t.Context(), f.store.reader, record.ID, 0, 10)
					if err != nil {
						t.Fatal(err)
					}
					for i, message := range beforeMessages {
						if message.Text != afterMessages[i].Text || !bytes.Equal(message.Data, afterMessages[i].Data) || message.Actor != afterMessages[i].Actor {
							t.Fatal("legacy message read changed")
						}
					}
					afterEvents, err := f.store.listEvents(t.Context(), f.store.reader, record.ID, 0, 10)
					if err != nil || !reflect.DeepEqual(beforeEvents, afterEvents) {
						t.Fatalf("legacy event read changed: %v", err)
					}
					var inline int
					column := "length(CAST(body_json AS BLOB))"
					want := 2
					if family.name == "messages" {
						column = "length(CAST(text AS BLOB))+length(CAST(data_json AS BLOB))+length(CAST(actor_json AS BLOB))"
						want = 4
					}
					if err := f.store.reader.QueryRowContext(t.Context(), "SELECT max("+column+") FROM "+family.table()+" WHERE conversation_id=?", record.ID).Scan(&inline); err != nil || inline != want {
						t.Fatalf("%s inline=%d want=%d err=%v", family.name, inline, want, err)
					}
				}
			}
		})
	}
}

func TestConversationToolRevisionConversionPreservesReplay(t *testing.T) {
	f := newStorageArchiveFixture(t)
	record := f.record(f.owner, "legacy tools", f.now)
	record.Status = conversation.StatusSettled
	record = f.create(t, record)
	message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleSystem, Kind: conversation.MessageTool, Delivery: conversation.DeliveryCompleted, Text: "Running tool " + strings.Repeat("x", 5000), Data: json.RawMessage(`{"tool":"example","item_id":"item","status":"running"}`), Actor: conversation.Actor{Kind: conversation.ActorCoordinator}, CreatedAt: f.now, UpdatedAt: f.now}
	tx := f.tx(t)
	if err := f.insertLegacyMessage(t, t.Context(), tx, &message); err != nil {
		t.Fatal(err)
	}
	if _, err := f.insertLegacyEvent(t, t.Context(), tx, record.ID, conversation.EventMessageAccepted, projectMessage(message), f.now); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"completed", "saved"} {
		message.Text = status + strings.Repeat("y", 7000)
		message.Data = json.RawMessage(`{"tool":"example","item_id":"item","status":"` + status + `"}`)
		if err := f.updateLegacyToolMessage(t, t.Context(), tx, message, f.now); err != nil {
			t.Fatal(err)
		}
	}
	f.commit(t, tx)
	before, err := f.store.listEvents(t.Context(), f.store.reader, record.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	f.store.contents = convertedConversationContent{service: f.service}
	family := storageConversionFamilies[4]
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	if err := f.service.discoverStorageFamily(t.Context(), family, f.now); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := f.service.convertConversationPayload(t.Context(), scope, family, record.ID, f.now); err != nil {
			t.Fatal(err)
		}
	}
	after, err := f.store.listEvents(t.Context(), f.store.reader, record.ID, 0, 10)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("tool replay changed: err=%v", err)
	}
	var inline int
	if err := f.store.reader.QueryRowContext(t.Context(), `SELECT coalesce(sum(length(CAST(text AS BLOB))),0) FROM conversation_tool_revisions WHERE conversation_id=?`, record.ID).Scan(&inline); err != nil || inline != 0 {
		t.Fatalf("tool revision inline bytes=%d err=%v", inline, err)
	}
}

func TestConversationConversionPublicationFailureKeepsInlineContent(t *testing.T) {
	for _, family := range storageConversionFamilies[3:5] {
		for _, failure := range []string{"publish", "read"} {
			t.Run(family.name+"/"+failure, func(t *testing.T) {
				f := newStorageArchiveFixture(t)
				record := f.record(f.owner, "legacy conversion", f.now)
				record.Status = conversation.StatusSettled
				record = f.create(t, record)
				tx := f.tx(t)
				message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryCompleted, Text: "original text", Data: json.RawMessage(`{}`), Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: f.owner}, CreatedAt: f.now, UpdatedAt: f.now}
				if err := f.insertLegacyMessage(t, t.Context(), tx, &message); err != nil {
					t.Fatal(err)
				}
				if _, err := f.insertLegacyEvent(t, t.Context(), tx, record.ID, conversation.EventHeartbeat, map[string]string{"text": "original event"}, f.now); err != nil {
					t.Fatal(err)
				}
				f.commit(t, tx)
				f.store.contents = convertedConversationContent{service: f.service}
				scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
				if err := f.service.discoverStorageFamily(t.Context(), family, f.now); err != nil {
					t.Fatal(err)
				}
				column := family.column
				if family.name == "messages" {
					column = "text"
				}
				var original string
				query := "SELECT " + column + " FROM " + family.table() + " WHERE conversation_id=?"
				if err := f.store.reader.QueryRowContext(t.Context(), query, record.ID).Scan(&original); err != nil {
					t.Fatal(err)
				}
				f.hook = func(r entryauthority.ContentRequest) error {
					if r.Write == (failure == "publish") {
						return errors.New("object storage unavailable")
					}
					return nil
				}
				if err := f.service.convertConversationPayload(t.Context(), scope, family, record.ID, f.now); err == nil {
					t.Fatal("conversion accepted unavailable storage")
				}
				var after string
				if err := f.store.reader.QueryRowContext(t.Context(), query, record.ID).Scan(&after); err != nil || after != original {
					t.Fatalf("failed conversion changed source: err=%v", err)
				}
				f.hook = nil
				if err := f.service.convertConversationPayload(t.Context(), scope, family, record.ID, f.now); err != nil {
					t.Fatal(err)
				}
				var pending int
				if err := f.store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM "+family.table()+" WHERE conversation_id=? AND "+family.pending(), record.ID).Scan(&pending); err != nil || pending != 0 {
					t.Fatalf("conversion did not resume: pending=%d err=%v", pending, err)
				}
			})
		}
	}
}

func TestStorageConversionSkipsFailedRowsAndRetries(t *testing.T) {
	f := newStorageArchiveFixture(t)
	issue := f.nativeFixture.create(t, "Failed storage rows")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	tx := f.tx(t)
	for range storageConversionRows + 3 {
		if err := appendLegacyNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", tracker.CollaborationData{Run: &tracker.NativeRunData{CompletionBody: strings.Repeat("example", 4096)}}, f.now); err != nil {
			t.Fatal(err)
		}
	}
	f.commit(t, tx)
	family := storageConversionFamilies[0]
	for range 2 {
		if err := f.service.discoverStorageFamily(t.Context(), family, f.now); err != nil {
			t.Fatal(err)
		}
	}
	failed := 0
	f.hook = func(r entryauthority.ContentRequest) error {
		if r.Write && failed < storageConversionRows {
			failed++
			return errors.New("example publication failure")
		}
		return nil
	}
	for tick, want := range []int{0, 3, storageConversionRows + 3} {
		err := f.service.convertStorageAggregate(f.service.runDetailContext(t.Context()), scope, family, string(issue.WorkItemID), f.now)
		if (err != nil) != (tick == 0) {
			t.Fatalf("tick=%d error=%v", tick, err)
		}
		var converted int
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id=? AND type='run.finished' AND conversion_state=2", issue.WorkItemID).Scan(&converted); err != nil || converted != want {
			t.Fatalf("failed storage rows block progress or retry: tick=%d converted=%d want=%d error=%v", tick, converted, want, err)
		}
	}
}

func TestStorageConversionContinuesAfterProjectionFailure(t *testing.T) {
	f := newStorageArchiveFixture(t)
	issue := f.nativeFixture.create(t, "Projection failure before storage")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	tx := f.tx(t)
	if err := appendLegacyNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", tracker.CollaborationData{Run: &tracker.NativeRunData{CompletionBody: strings.Repeat("example", 4096)}}, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "UPDATE issues SET actor_principal_id=NULL,actor_json='[]' WHERE native_id=?", issue.WorkItemID); err != nil {
		t.Fatal(err)
	}
	f.commit(t, tx)
	if err := f.service.convertLegacyStorage(t.Context(), f.now); err == nil {
		t.Fatal("projection failure was not reported")
	}
	var converted int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id=? AND type='run.finished' AND conversion_state=2", issue.WorkItemID).Scan(&converted); err != nil || converted != 1 {
		t.Fatalf("projection failure stopped storage conversion: converted=%d error=%v", converted, err)
	}
}

func TestStorageConversionAttemptLeaseBudget(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	items := []tracker.NativeIssue{f.create(t, "First expired execution"), f.create(t, "Second expired execution")}
	worker := f.worker(t, "repair worker")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": "repair-machine", "hostname": "repair-machine", "version": "test", "capacity": 2}), http.StatusOK)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range storageConversionRows + 3 {
		data := tracker.NativeRunData{PolicyID: "example-policy", Sequence: 1}
		id := seedHealthRateAttempt(t, tx, scope, items[i%len(items)], "repair-machine", "running", data, now.Add(-time.Hour), -1)
		data.AttemptID = id
		if err := tx.QueryRowContext(t.Context(), "SELECT lease_id,fencing_token,run_id FROM native_attempts WHERE id=?", id).Scan(&data.LeaseID, &data.FencingToken, &data.RunID); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=? WHERE id=?", string(raw), id); err != nil {
			t.Fatal(err)
		}
	}
	for i, item := range items {
		id := seedHealthRateAttempt(t, tx, scope, item, "repair-machine", "running", tracker.NativeRunData{PolicyID: "example-policy", Sequence: 1}, now, -1)
		if _, err := tx.ExecContext(t.Context(), "UPDATE leases SET released_at=NULL WHERE lease_id=(SELECT lease_id FROM native_attempts WHERE id=?)", id); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET id=? WHERE id=?", fmt.Sprintf("zz-live-%d", i), id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	previous := 0
	for tick := range 30 {
		if err := f.service.convertLegacyStorage(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		var cancelled int
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempts WHERE status='cancelled'").Scan(&cancelled); err != nil {
			t.Fatal(err)
		}
		if cancelled < previous || cancelled-previous > 100 {
			t.Fatalf("unbounded lease repair: tick=%d before=%d after=%d", tick, previous, cancelled)
		}
		previous = cancelled
		for i := range items {
			var status string
			var sequence int64
			var released sql.NullString
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT a.status,a.sequence,l.released_at FROM native_attempts a JOIN leases l ON l.lease_id=a.lease_id WHERE a.id=?", fmt.Sprintf("zz-live-%d", i)).Scan(&status, &sequence, &released); err != nil {
				t.Fatal(err)
			}
			if status != "running" || sequence != 1 || released.Valid {
				t.Fatalf("tick %d changed live attempt %d: status=%s sequence=%d released=%v", tick, i, status, sequence, released)
			}
		}
	}
	if previous != storageConversionRows+3 {
		t.Fatalf("lease repair stalled at %d", previous)
	}
	var before, after int
	query := `SELECT examined_rows FROM storage_conversion_cursors WHERE family='projection-leases'`
	if err := f.service.database.reader.QueryRowContext(t.Context(), query).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := f.service.convertLegacyStorage(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.reader.QueryRowContext(t.Context(), query).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after-before > len(items) {
		t.Fatalf("same live leases repeatedly examined in one tick: %d", after-before)
	}

}

func TestStorageConversionOwnerFairnessAndRestart(t *testing.T) {
	for _, history := range []int{20, 2000} {
		t.Run(strconv.Itoa(history), func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			f.service.outbox.stop()
			other := newNativeFixture(t, f.service, "", "other-tenant")
			sibling := newNativeFixture(t, f.service, f.project.OrganizationID, "sibling-project")
			fixtures := []nativeFixture{f.nativeFixture, other, sibling}
			convertedByProject := make(map[string]int)
			tx := f.tx(t)
			for i, fixture := range fixtures {
				limit := 2
				if i == 0 {
					limit = history
				}
				for j := range limit {
					raw := `{"work_item_id":"example-item","status":"failed","completion_body":"` + strings.Repeat("x", 4096) + `"}`
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json) VALUES(?,?,0,'attempt',?,1,?)`, fixture.project.OrganizationID, fixture.project.ID, fmt.Sprintf("row-%06d", j), raw); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json) VALUES(?,?,0,'attempt','000-malformed',1,'[]')`, f.project.OrganizationID, f.project.ID); err != nil {
				t.Fatal(err)
			}
			f.commit(t, tx)
			for _, fixture := range fixtures {
				for range 2 {
					issue := fixture.create(t, "Fair storage progress")
					tx = f.tx(t)
					scope := nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID}
					if err := appendLegacyNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", tracker.CollaborationData{Run: &tracker.NativeRunData{CompletionBody: strings.Repeat("output", 1024)}}, f.now); err != nil {
						t.Fatal(err)
					}
					f.commit(t, tx)
				}
			}
			calls := 0
			f.hook = func(entryauthority.ContentRequest) error {
				calls++
				if got := f.service.database.reader.Stats().InUse; got != 0 {
					t.Errorf("network call held %d readers", got)
				}
				if got := f.service.database.db.Stats().InUse; got != 0 {
					t.Errorf("network call held %d writer connections", got)
				}
				return nil
			}
			var firstStatements int
			ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(string, []driver.NamedValue) { firstStatements++ }})
			if err := f.service.convertLegacyStorage(ctx, f.now); err != nil && !strings.Contains(err.Error(), "cannot unmarshal array") && !strings.Contains(err.Error(), "invalid JSON object") {
				t.Fatal(err)
			}
			for _, fixture := range fixtures {
				var converted int
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events WHERE organization_id=? AND project_id=? AND type='run.finished' AND payload_version=1`, fixture.project.OrganizationID, fixture.project.ID).Scan(&converted); err != nil {
					t.Fatal(err)
				}
				if converted != 2 {
					t.Fatalf("analytics backlog or malformed row starved runs: converted=%d want=2", converted)
				}
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND payload_object_key<>''`, fixture.project.OrganizationID, fixture.project.ID).Scan(&converted); err != nil {
					t.Fatal(err)
				}
				if converted == 0 {
					t.Fatal("earlier scope backlog starved analytics in a later scope")
				}
				convertedByProject[string(fixture.project.ID)] = converted
			}
			var rows, bytes, oversized int
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT sum(examined_rows),sum(examined_bytes),sum(oversized_rows) FROM storage_conversion_cursors`).Scan(&rows, &bytes, &oversized); err != nil {
				t.Fatal(err)
			}
			if rows > 100 || bytes > 1<<20 || oversized != 0 {
				t.Fatalf("unbounded tick: rows=%d bytes=%d oversized=%d", rows, bytes, oversized)
			}
			if firstStatements > 2300 {
				t.Fatalf("tick executed %d statements", firstStatements)
			}
			var convertedRows, reclaimed int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT coalesce(sum(converted_rows),0),coalesce(sum(reclaimed_bytes),0) FROM storage_conversion_progress`).Scan(&convertedRows, &reclaimed); err != nil {
				t.Fatal(err)
			}
			t.Logf("history=%d tick rows=%d input_bytes=%d statements=%d network_calls=%d converted_rows=%d reclaimed_bytes=%d", history, rows, bytes, firstStatements, calls, convertedRows, reclaimed)
			var before int
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM analytics_hourly_rollups WHERE payload_object_key<>''`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if before == 0 || calls == 0 {
				t.Fatal("malformed first row prevented later conversion")
			}
			cfg := f.service.config
			f.service.conversations = nil
			if err := f.service.Close(); err != nil {
				t.Fatal(err)
			}
			f.service = openTestService(t, cfg)
			f.service.outbox.stop()
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT 1").Scan(new(int)); err != nil {
				t.Fatal(err)
			}
			for _, fixture := range fixtures {
				for index := range 2 {
					if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json) VALUES(?,?,0,'attempt',?,1,'{"status":"failed"}')`, fixture.project.OrganizationID, fixture.project.ID, fmt.Sprintf("000-added-%d", index)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json) VALUES(?,?,0,'attempt','gap-before-vacuum',1,'{}')`, f.project.OrganizationID, f.project.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `DELETE FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND dimension='row-000000'`, f.project.OrganizationID, f.project.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "VACUUM"); err != nil {
				t.Fatal(err)
			}
			for range 8 {
				if err := f.service.convertLegacyStorage(t.Context(), f.now); err != nil && !strings.Contains(err.Error(), "cannot unmarshal array") && !strings.Contains(err.Error(), "invalid JSON object") {
					t.Fatal(err)
				}
			}
			for _, fixture := range fixtures {
				var converted int
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events WHERE organization_id=? AND project_id=? AND type='run.finished' AND payload_version=1`, fixture.project.OrganizationID, fixture.project.ID).Scan(&converted); err != nil {
					t.Fatal(err)
				}
				if converted != 2 {
					t.Fatalf("large analytics family starved runs in tenant: converted=%d", converted)
				}
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND payload_object_key<>''`, fixture.project.OrganizationID, fixture.project.ID).Scan(&converted); err != nil || converted <= convertedByProject[string(fixture.project.ID)] {
					t.Fatalf("scope did not resume analytics after reopen and VACUUM: converted=%d err=%v", converted, err)
				}
			}
			var after int
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM analytics_hourly_rollups WHERE payload_object_key<>''`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after <= before {
				t.Fatalf("restart did not advance: before=%d after=%d", before, after)
			}
			var retained string
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT data_json FROM analytics_hourly_rollups WHERE dimension='000-malformed'`).Scan(&retained); err != nil || retained != "[]" {
				t.Fatalf("malformed source lost: %q %v", retained, err)
			}
		})
	}
}

func TestStorageConversionKeysetsUsePendingIndexes(t *testing.T) {
	f := newStorageArchiveFixture(t)
	for _, family := range storageConversionFamilies {
		t.Run(family.name, func(t *testing.T) {
			keys, numeric := family.keyset()
			args := make([]any, len(keys))
			for index := range args {
				args[index] = "example-key"
				if numeric && index == len(keys)-1 {
					args[index] = int64(1)
				}
			}
			query := "SELECT r.rowid FROM " + family.keysetFrom() + " WHERE " + family.payloadPredicate() + " AND (" + strings.Join(keys, ",") + ")>(" + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + ") ORDER BY " + strings.Join(keys, ",") + " LIMIT 1"
			plan := strings.Join(explainQueryPlan(t, f.service.database.reader, query, args), "; ")
			if strings.Contains(plan, "TEMP B-TREE") || !strings.Contains(plan, "SEARCH r USING") {
				t.Fatalf("keyset scans or sorts history: %s", plan)
			}
		})
	}
}

func TestStorageConversionIdleTickDoesNotWrite(t *testing.T) {
	f := newStorageArchiveFixture(t)
	if err := f.service.convertLegacyStorage(t.Context(), f.now); err != nil {
		t.Fatal(err)
	}
	var before, after int64
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := f.service.convertLegacyStorage(t.Context(), f.now); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("idle conversion tick changed %d rows", after-before)
	}
}

func TestStorageConversionProjectionKeysetsAvoidHistorySorts(t *testing.T) {
	f := newStorageArchiveFixture(t)
	var selections []tracedStatement
	ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, args []driver.NamedValue) {
		if !strings.HasPrefix(query, "SELECT ") || !strings.Contains(query, " ORDER BY ") || !strings.HasSuffix(query, " LIMIT ?") {
			return
		}
		statement := tracedStatement{query: query}
		for _, arg := range args {
			statement.args = append(statement.args, arg.Value)
		}
		selections = append(selections, statement)
	}})
	if err := f.service.convertLegacyStorage(ctx, f.now); err != nil {
		t.Fatal(err)
	}
	if len(selections) < 10 {
		t.Fatalf("only %d projection families checked", len(selections))
	}
	for _, statement := range selections {
		plan := strings.Join(explainQueryPlan(t, f.service.database.reader, statement.query, statement.args), "; ")
		if strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("projection sorts history: %s: %s", statement.query, plan)
		}
	}
}

func TestStorageConversionDeferredProjectionResumes(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(fmt.Sprintf("archived=%t", archived), func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			issue := f.nativeFixture.create(t, "deferred hot projection")
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			raw := `{"completion_body":"` + strings.Repeat("x", 2<<20) + `"}`
			var payloadKeys [2]string
			if archived {
				ctx := f.service.freshRunDetailContext(t.Context())
				for i, payload := range []string{"[]", raw} {
					ref, err := prepareChangeContent(ctx, string(scope.organization), []byte(payload))
					if err != nil {
						t.Fatal(err)
					}
					payloadKeys[i] = ref.Key
				}
			} else {
				f.service.config.Content = nil
			}

			tx := f.tx(t)
			at := formatHubTime(f.now)
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO machines(id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES('example-machine','example',1,'test',?,?,?)`, at, at, at); err != nil {
				t.Fatal(err)
			}
			for i, payload := range []string{`[]`, raw} {
				id := seedHealthRateAttempt(t, tx, scope, issue, "example-machine", "failed", tracker.NativeRunData{}, f.now, -1)
				if _, err := tx.ExecContext(t.Context(), `UPDATE native_attempts SET id=?,data_json=?,payload_object_key=?,activity_projection_version=1,report_projection_version=1,health_disposition_reason='',worker_cleanup_required=0 WHERE id=?`, []string{"A", "B"}[i], payload, payloadKeys[i], id); err != nil {
					t.Fatal(err)
				}
			}
			f.commit(t, tx)
			if err := f.service.convertStorageBatch(t.Context(), f.now); err == nil {
				t.Fatal("malformed A was not attempted")
			}
			var projected int
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT hot_projection_version FROM native_attempts WHERE id='B'`).Scan(&projected); err != nil || projected != 0 {
				t.Fatalf("B admitted alongside A: projected=%d err=%v", projected, err)
			}
			cfg := f.service.config
			f.service.conversations = nil
			if err := f.service.Close(); err != nil {
				t.Fatal(err)
			}
			f.service = openTestService(t, cfg)
			if !archived {
				f.service.config.Content = nil
			}
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			for range 6 {
				if err := f.service.convertStorageBatch(t.Context(), f.now); err != nil && !strings.Contains(err.Error(), "cannot unmarshal array") {
					t.Fatal(err)
				}
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT hot_projection_version FROM native_attempts WHERE id='B'`).Scan(&projected); err != nil || projected != 1 {
				t.Fatalf("deferred B starved after reopen: projected=%d err=%v", projected, err)
			}
			var retained string
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT data_json FROM native_attempts WHERE id='A'`).Scan(&retained); err != nil || retained != "[]" {
				t.Fatalf("malformed A lost: %q %v", retained, err)
			}
		})
	}
}

func TestStorageConversionChargesPublishedLegacyRuns(t *testing.T) {
	f := newStorageArchiveFixture(t)
	issue := f.nativeFixture.create(t, "expanded legacy runs")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	ctx := f.service.freshRunDetailContext(t.Context())
	var row int64
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT coalesce(max(rowid),0) FROM collaboration_events").Scan(&row); err != nil {
		t.Fatal(err)
	}
	row++
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO collaboration_events(rowid,id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at,health_projected,page_projection_ready,query_metadata_version,analytics_projection_version,activity_spans_present) VALUES(?,'000-malformed',?,?,?,99,'run.finished',1,'{}','[]',?,1,1,1,1,0)`, row, scope.organization, scope.project, issue.WorkItemID, formatHubTime(f.now)); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		row++
		raw := []byte(`{"run":{"completion_body":"` + strings.Repeat("x", 2<<20) + `"},"reason_detail":"` + strconv.Itoa(i) + `"}`)
		encoded, err := json.Marshal(storageJSONPayload{Organization: string(scope.organization), Project: string(scope.project), Family: "runs", Row: row, Data: raw})
		if err != nil {
			t.Fatal(err)
		}
		ref, err := prepareChangeContent(ctx, string(scope.organization), encoded)
		if err != nil {
			t.Fatal(err)
		}
		compact, err := compactStorageJSON(raw, storageJSONReference{Organization: string(scope.organization), Project: string(scope.project), Family: "runs", Row: row, Reference: ref})
		if err != nil {
			t.Fatal(err)
		}
		if len(compact) > 4096 {
			t.Fatalf("reference is not compact: %d", len(compact))
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO collaboration_events(rowid,id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at,health_projected,page_projection_ready,query_metadata_version,analytics_projection_version,activity_spans_present) VALUES(?,?,?,?,?,?,'run.finished',1,'{}',?,?,1,1,1,1,0)`, row, fmt.Sprintf("legacy-run-%d", i), scope.organization, scope.project, issue.WorkItemID, 100+i, string(compact), formatHubTime(f.now)); err != nil {
			t.Fatal(err)
		}
	}
	publishedBytes, publishedRows := 0, 0
	f.hook = func(r entryauthority.ContentRequest) error {
		if r.Write {
			publishedBytes += len(archiveTestBytes(t, r.Data))
			publishedRows++
		}
		return nil
	}
	for tick := range 8 {
		publishedBytes, publishedRows = 0, 0
		if err := f.service.convertStorageBatch(t.Context(), f.now); err != nil && !strings.Contains(err.Error(), "cannot unmarshal array") {
			t.Fatal(err)
		}
		if tick == 0 {
			cfg := f.service.config
			f.service.conversations = nil
			if err := f.service.Close(); err != nil {
				t.Fatal(err)
			}
			f.service = openTestService(t, cfg)
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
		}
		if publishedRows > 1 || (publishedBytes > storageConversionBytes && publishedRows != 1) {
			t.Fatalf("tick %d published %d uncompressed bytes across %d rows", tick, publishedBytes, publishedRows)
		}
	}
	var converted int
	if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events WHERE type='run.finished' AND payload_version=1 AND work_item_id=?`, issue.WorkItemID).Scan(&converted); err != nil || converted != 3 {
		t.Fatalf("deferred runs stalled: converted=%d err=%v", converted, err)
	}
}
