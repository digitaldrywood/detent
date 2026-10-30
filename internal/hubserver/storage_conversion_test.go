package hubserver

import (
	"bytes"
	"context"
	"database/sql"
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
	for _, test := range []struct {
		name              string
		detailBytes, rows int
		progress          []int64
	}{
		{"batch budget", 180 << 10, 11, []int64{5, 10, 11}},
		{"oversized row advances", 2 << 20, 2, []int64{1, 2}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			issue := f.nativeFixture.create(t, "bounded legacy rows")
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			tx := f.tx(t)
			for range test.rows {
				if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", tracker.CollaborationData{ReasonDetail: strings.Repeat("x", test.detailBytes), Run: &tracker.NativeRunData{CompletionBody: strings.Repeat("old output", 4096)}}, f.now); err != nil {
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
			for _, want := range test.progress {
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
				if after-before > int64(max(storageConversionBytes, test.detailBytes+4096)) {
					t.Fatalf("chunk wrote %d payload bytes", after-before)
				}
				progress, err := readStorageConversionProgress(t.Context(), f.service.database.reader, scope)
				if err != nil || len(progress) != 1 || progress[0].Version != storageConversionVersion || progress[0].Rows != want || progress[0].Complete != (want == int64(test.rows)) {
					t.Fatalf("progress=%+v err=%v", progress, err)
				}
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
	raw := `{"policy_id":"legacy-policy","head_sha":"legacy-head","policy":{"gates":{"github_pull_request":false}},"actor":{"principal_id":"reviewer"},"decision":"approved","source":{"bytes":8192},"output":"` + strings.Repeat("x", 8192) + `"}`
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
			{`INSERT INTO change_sources(version_id,bundle) VALUES(?,?)`, []any{version, bytes.Repeat([]byte("x"), 8192)}},
		} {
			if _, err := tx.ExecContext(t.Context(), insert.query, insert.args...); err != nil {
				t.Fatal(err)
			}
		}
		if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", tracker.CollaborationData{Run: &tracker.NativeRunData{CompletionBody: strings.Repeat("output", 2048)}}, f.now); err != nil {
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
		for _, assignment := range []string{"version_id=NULL", "source_key=NULL", "record_json='{}',claim_review_current=0"} {
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
		if err := authorizeStorageConversion(t.Context(), tx, scope, family, aggregate, row, []byte(prior), converted, ref); err != nil {
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
	if err := f.service.populateChangeColumns(ctx); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		var policy, head, stored string
		if err := db.reader.QueryRowContext(t.Context(), `SELECT policy_revision_id,head_sha,record_json FROM change_versions WHERE id=?`, fmt.Sprintf("conversion-version-%d", i)).Scan(&policy, &head, &stored); err != nil {
			t.Fatal(err)
		}
		if policy != "legacy-policy" || head != "legacy-head" || stored != raw {
			t.Fatalf("version %d backfill changed content or missed typed columns: policy=%q head=%q", i, policy, head)
		}
	}
	assertImmutable()
	if err := db.populateClaimFacts(t.Context()); err != nil {
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
		rows, err := db.reader.QueryContext(t.Context(), "SELECT record_json FROM "+table+" WHERE record_json<>'{}'")
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
			if !strings.Contains(stored, "_storage_archive") {
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
		if err != nil || !bytes.Equal(content, bytes.Repeat([]byte("x"), 8192)) {
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
INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at)
SELECT ?||n,?,?,?,n+100,'run.finished',1,'{}','{}',? FROM history`, fmt.Sprintf("converted-%d-", i), scope.organization, scope.project, issue.WorkItemID, formatHubTime(f.now)); err != nil {
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
		if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", tracker.CollaborationData{Run: &tracker.NativeRunData{CompletionBody: strings.Repeat("pending", 1024)}}, f.now); err != nil {
			t.Fatal(err)
		}
		f.commit(t, tx)
	}
	rows, err := db.reader.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+storageConversionDiscovery, scope.organization, scope.project, "runs")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "USING INDEX storage_conversion_pending") || strings.Contains(plan.String(), "SCAN") || strings.Contains(plan.String(), "TEMP B-TREE") {
		t.Fatalf("unbounded discovery plan: %s", plan.String())
	}
	for pass, want := range []int{2, 0} {
		if err := f.service.convertStorageFamily(f.service.contentContext(t.Context()), scope, storageConversionFamilies[0], f.now.Add(time.Duration(pass)*time.Second)); err != nil {
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
	var progress int
	if err := db.reader.QueryRowContext(ctx, "SELECT count(*) FROM storage_conversion_progress WHERE family='analytics'").Scan(&progress); err != nil || progress != 0 {
		t.Fatalf("startup seeded progress=%d: %v", progress, err)
	}
	family := storageConversionFamilies[2]
	if _, err := db.db.ExecContext(ctx, `CREATE TRIGGER interrupt_discovery BEFORE UPDATE OF conversion_state ON analytics_hourly_rollups WHEN NEW.conversion_state=1 BEGIN SELECT RAISE(ABORT,'interrupted discovery'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.service.discoverStorageFamily(ctx, family, time.Now()); err == nil {
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
		if err := f.service.discoverStorageFamily(ctx, family, time.Now()); err != nil {
			t.Fatal(err)
		}
		var remaining int
		if err := db.reader.QueryRowContext(ctx, "SELECT count(*) FROM analytics_hourly_rollups WHERE conversion_state=0").Scan(&remaining); err != nil || remaining != want {
			t.Fatalf("pass=%d undiscovered=%d want=%d: %v", pass, remaining, want, err)
		}
		for _, fixture := range []nativeFixture{f, other} {
			scope := nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID}
			if err := f.service.convertStorageFamily(ctx, scope, family, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		if want > 0 {
			var completed int
			if err := db.reader.QueryRowContext(ctx, "SELECT count(*) FROM storage_conversion_progress WHERE family='analytics' AND complete=1").Scan(&completed); err != nil || completed != 0 {
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
			ctx := t.Context()
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

func TestStorageConversionSkipsFailedRowsAndRetries(t *testing.T) {
	f := newStorageArchiveFixture(t)
	issue := f.nativeFixture.create(t, "Failed storage rows")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	tx := f.tx(t)
	for range storageConversionRows + 3 {
		if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", tracker.CollaborationData{Run: &tracker.NativeRunData{CompletionBody: strings.Repeat("example", 4096)}}, f.now); err != nil {
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
		err := f.service.convertStorageAggregate(f.service.contentContext(t.Context()), scope, family, string(issue.WorkItemID), f.now)
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
	if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", tracker.CollaborationData{Run: &tracker.NativeRunData{CompletionBody: strings.Repeat("example", 4096)}}, f.now); err != nil {
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
	for tick, want := range []int{storageConversionRows, storageConversionRows + 3, storageConversionRows + 3} {
		if err := f.service.convertLegacyStorage(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		var cancelled int
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempts WHERE status='cancelled'").Scan(&cancelled); err != nil {
			t.Fatal(err)
		}
		if cancelled != want {
			t.Fatalf("tick %d cancelled %d attempts, want %d", tick, cancelled, want)
		}
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
}
