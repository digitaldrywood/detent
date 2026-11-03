package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeVersionPayloadWrites(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	issue := f.create(t, "object history")
	body := strings.Repeat("unchanged content ", 512) + issueContractTestSections
	title := "edited title"
	for i, update := range []tracker.UpdateIssue{{Body: &body}, {Title: &title}} {
		update.Mutation = tracker.Mutation{IdempotencyKey: fmt.Sprintf("edit-%d", i)}
		update.ExpectedRevision = issue.Revision
		response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token, update)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &issue)
	}
	rows, err := f.service.database.reader.QueryContext(t.Context(), "SELECT revision,encoding,base_revision,record_json,payload_object_key FROM collaboration_versions WHERE organization_id=? AND record_id=? ORDER BY revision", f.project.OrganizationID, issue.WorkItemID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var patch bool
	for rows.Next() {
		var revision, base int64
		var encoding, raw, key string
		if err := rows.Scan(&revision, &encoding, &base, &raw, &key); err != nil {
			t.Fatal(err)
		}
		if raw != "{}" || key == "" || base != nativeVersionBase(encoding, revision) {
			t.Fatalf("revision %d retained inline payload or lost typed metadata: encoding=%s base=%d inline=%d key=%q", revision, encoding, base, len(raw), key)
		}
		patch = patch || encoding == "patch"
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	if !patch {
		t.Fatal("edits stopped producing patches")
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	raw, err := f.service.readVersion(f.service.contentContext(t.Context()), scope, string(issue.WorkItemID), "", int64(issue.Revision))
	if err != nil || !bytes.Contains(raw, []byte("edited")) {
		t.Fatalf("edited history missing: %v", err)
	}
}

func TestCollaborationVersionConversionHistory(t *testing.T) {
	objects := &changeObjectFixture{}
	interrupted := false
	f := newDefaultNativeFixture(t, Config{Content: func(ctx context.Context, r entryauthority.ContentRequest) ([]byte, error) {
		if r.Write && interrupted {
			return nil, errors.New("interrupted upload")
		}
		return objects.call(ctx, r)
	}})
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO organizations(id,name,created_at) VALUES('org_history_second','Second example',?)", formatHubTime(f.service.config.now())); err != nil {
		t.Fatal(err)
	}
	second := newNativeFixture(t, f.service, "org_history_second", "history-second")
	type history struct {
		scope        nativeScope
		item, record string
		snapshots    [][]byte
	}
	var histories []history
	const count = 70
	for _, fixture := range []nativeFixture{f, second} {
		issue := fixture.create(t, "history conversion")
		h := history{scope: nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID}, item: string(issue.WorkItemID), record: "comment-history"}
		var previous []byte
		var previousEncoding string
		for revision := int64(1); revision <= count; revision++ {
			raw := []byte(fmt.Sprintf(`{"body":%q,"revision":%d,"tenant":%q}`, strings.Repeat("retained content 雪 ", 256), revision, fixture.project.OrganizationID))
			encoded, encoding, err := encodeNativeVersion(previous, previousEncoding, raw, revision)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO collaboration_versions(organization_id,project_id,work_item_id,record_id,revision,record_json,encoding) VALUES(?,?,?,?,?,?,?)", h.scope.organization, h.scope.project, h.item, h.record, revision, string(encoded), encoding); err != nil {
				t.Fatal(err)
			}
			previous, previousEncoding = encoded, encoding
			h.snapshots = append(h.snapshots, raw)
		}
		histories = append(histories, h)
	}
	verify := func(t *testing.T) {
		t.Helper()
		for _, h := range histories {
			ctx := f.service.contentContext(t.Context())
			for i, want := range h.snapshots {
				got, err := reconstructNativeVersion(ctx, f.service.database.reader, string(h.scope.organization), string(h.scope.project), h.item, h.record, int64(i+1))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("organization %s revision %d history changed: %v", h.scope.organization, i+1, err)
				}
			}
		}
	}
	t.Run("legacy", verify)
	family := storageConversionFamily{name: "collaboration_versions", column: "record_json", from: "collaboration_versions r", where: "r.payload_object_key=''", aggregate: "r.work_item_id"}
	h := histories[0]
	var row int64
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT rowid FROM collaboration_versions WHERE organization_id=? AND record_id=? AND revision=1", h.scope.organization, h.record).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if err := f.service.convertObjectPayloadRow(f.service.contentContext(t.Context()), family, objectPayloadConversionPosition{row: row, scope: h.scope, aggregate: h.item}, f.service.config.now()); err != nil {
		t.Fatal(err)
	}
	interrupted = true
	if err := f.service.convertObjectPayloadFamily(f.service.contentContext(t.Context()), family, f.service.config.now()); err == nil {
		t.Fatal("interrupted patch upload succeeded")
	}
	interrupted = false
	var converted int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_versions WHERE record_id=? AND payload_object_key<>''", h.record).Scan(&converted); err != nil || converted != 1 {
		t.Fatalf("interruption advanced chain: %d %v", converted, err)
	}
	t.Run("mixed", verify)
	if err := f.service.convertStorageBatch(t.Context(), f.service.config.now()); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_versions WHERE record_id=? AND payload_object_key<>''", h.record).Scan(&converted); err != nil || converted > 1+storageConversionRows || converted <= 1 {
		t.Fatalf("batch bound or progress: %d %v", converted, err)
	}
	t.Run("resumed", verify)
	if err := f.service.convertStorageBatch(t.Context(), f.service.config.now()); err != nil {
		t.Fatal(err)
	}
	t.Run("converted", verify)
	var inline, remaining int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT sum(length(CAST(record_json AS BLOB))),sum(payload_object_key='') FROM collaboration_versions WHERE record_id=?", h.record).Scan(&inline, &remaining); err != nil || inline != 2*2*count || remaining != 0 {
		t.Fatalf("payloads remain inline: bytes=%d rows=%d %v", inline, remaining, err)
	}
	for _, h := range histories {
		var complete bool
		var converted int
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT complete,converted_rows FROM storage_conversion_progress WHERE organization_id=? AND project_id=? AND family='collaboration_versions' AND aggregate_id=?", h.scope.organization, h.scope.project, h.item).Scan(&complete, &converted); err != nil || !complete || converted != count {
			t.Fatalf("conversion progress incomplete: %t %d %v", complete, converted, err)
		}
	}
	t.Run("immutable", func(t *testing.T) {
		for _, set := range []string{"record_json='{}'", "encoding='patch'", "revision=1000", "base_revision=999", "payload_object_key='other'", "record_id='other'", "organization_id='other'"} {
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE collaboration_versions SET "+set+" WHERE record_id=? AND revision=1", h.record); err == nil {
				t.Fatalf("immutable history allowed %s", set)
			}
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM collaboration_versions WHERE record_id=?", h.record); err == nil {
			t.Fatal("history deletion allowed")
		}
	})
	t.Run("missing object", func(t *testing.T) {
		var key string
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT payload_object_key FROM collaboration_versions WHERE organization_id=? AND record_id=? AND revision=2", h.scope.organization, h.record).Scan(&key); err != nil {
			t.Fatal(err)
		}
		objects.mu.Lock()
		delete(objects.objects, key)
		objects.mu.Unlock()
		got, err := reconstructNativeVersion(f.service.contentContext(t.Context()), f.service.database.reader, string(h.scope.organization), string(h.scope.project), h.item, h.record, 3)
		if err == nil || errors.Is(err, sql.ErrNoRows) || len(got) != 0 {
			t.Fatalf("missing patch silently returned history: bytes=%d err=%v", len(got), err)
		}
		if !errors.Is(err, artifact.ErrStorage) {
			t.Fatalf("missing object lost storage error: %v", err)
		}
	})
}
