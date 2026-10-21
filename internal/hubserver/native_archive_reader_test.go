package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func archiveTestTable(t *testing.T, s *Service, name, condition string, args ...any) attachment.ArchiveTable {
	t.Helper()
	rows, err := s.database.reader.QueryContext(t.Context(), "SELECT rowid AS archive_rowid,* FROM "+name+" WHERE "+condition+" ORDER BY rowid", args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	columns[0] = "rowid"
	table := attachment.ArchiveTable{Name: name, Columns: columns, Rows: [][]json.RawMessage{}}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		row := make([]json.RawMessage, len(columns))
		for i, value := range values {
			if raw, ok := value.([]byte); ok {
				value = string(raw)
			}
			row[i], err = json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
		}
		table.Rows = append(table.Rows, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return table
}

func publishTestSection(t *testing.T, f nativeFixture, item, kind, subject string, first, last int64, tables ...attachment.ArchiveTable) (attachment.ArchiveReference, []byte) {
	t.Helper()
	id := kind + "_" + subject + "_" + strconv.FormatInt(first, 10)
	section := attachment.ArchiveSection{SchemaVersion: 1, OrganizationID: string(f.project.OrganizationID), ProjectID: string(f.project.ID), WorkItemID: item, SectionID: id, Tables: tables}
	raw, err := json.Marshal(section)
	if err != nil {
		t.Fatal(err)
	}
	ref, data, err := attachment.PrepareArchive(string(f.project.OrganizationID), 1, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	attachmentID := "att_" + ref.SHA256[:32]
	credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []struct {
		statement string
		args      []any
	}{
		{`INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at,kind,retention_class,object_key,uncompressed_size,codec,schema_version) VALUES(?,?,?,?,'archive.gz','application/gzip',?,?,?,'archive','organization',?,?,?,?)`, []any{attachmentID, f.project.OrganizationID, f.project.ID, credential.ID, ref.CompressedSize, ref.SHA256, formatHubTime(f.service.config.now()), ref.Key, ref.UncompressedSize, ref.Codec, ref.SchemaVersion}},
		{`INSERT INTO attachment_archive_references(organization_id,project_id,work_item_id,attachment_id) VALUES(?,?,?,?)`, []any{f.project.OrganizationID, f.project.ID, item, attachmentID}},
		{`INSERT INTO native_archive_sections(organization_id,project_id,work_item_id,kind,subject_id,first_sequence,last_sequence,section_id,attachment_id) VALUES(?,?,?,?,?,?,?,?,?)`, []any{f.project.OrganizationID, f.project.ID, item, kind, subject, first, last, id, attachmentID}},
	} {
		if _, err := f.service.database.db.ExecContext(t.Context(), operation.statement, operation.args...); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "run" {
		for _, table := range tables {
			if table.Name != "native_attempts" {
				continue
			}
			index := slices.Index(table.Columns, "data_json")
			if index < 0 || len(table.Rows) == 0 {
				continue
			}
			var encoded string
			if err := json.Unmarshal(table.Rows[0][index], &encoded); err != nil {
				t.Fatal(err)
			}
			var run tracker.NativeRunData
			if err := json.Unmarshal([]byte(encoded), &run); err != nil {
				t.Fatal(err)
			}
			if run.Runtime != nil {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_archive_sections SET local_attempt_id=? WHERE organization_id=? AND project_id=? AND work_item_id=? AND section_id=?", run.Runtime.LocalAttemptID, f.project.OrganizationID, f.project.ID, item, id); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return ref, data
}

func archiveGolden(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var result map[string]json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	delete(result, "archive")
	delete(result, "generated_at")
	return result
}

func assertArchiveGolden(t *testing.T, hot, archived []byte) {
	t.Helper()
	left, err := json.Marshal(archiveGolden(t, hot))
	if err != nil {
		t.Fatal(err)
	}
	right, err := json.Marshal(archiveGolden(t, archived))
	if err != nil {
		t.Fatal(err)
	}
	var hotValue, archivedValue any
	if err := json.Unmarshal(left, &hotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(right, &archivedValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hotValue, archivedValue) {
		t.Fatalf("archived output differs\nhot: %s\narchive: %s", hot, archived)
	}
}

func TestNativeArchiveSectionFailures(t *testing.T) {
	for _, test := range []struct {
		name, availability string
		mutate             func(*testing.T, nativeFixture, string, *attachment.ArchiveReference, *[]byte)
		wantFetches        int
	}{
		{"valid", "available", nil, 1},
		{"signed read", "available", nil, 1},
		{"missing object", "unavailable", func(_ *testing.T, _ nativeFixture, _ string, _ *attachment.ArchiveReference, data *[]byte) {
			*data = nil
		}, 1},
		{"digest mismatch", "invalid", func(_ *testing.T, _ nativeFixture, _ string, _ *attachment.ArchiveReference, data *[]byte) {
			(*data)[0] ^= 1
		}, 1},
		{"crafted URL", "invalid", func(t *testing.T, f nativeFixture, item string, ref *attachment.ArchiveReference, _ *[]byte) {
			if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER immutable_attachment_archive"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE attachments SET object_key='http://169.254.169.254/latest/meta-data/' WHERE sha256=?", ref.SHA256); err != nil {
				t.Fatal(err)
			}
		}, 0},
		{"oversized section", "invalid", func(t *testing.T, f nativeFixture, _ string, ref *attachment.ArchiveReference, _ *[]byte) {
			if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER immutable_attachment_archive"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE attachments SET uncompressed_size=? WHERE sha256=?", attachment.MaxSectionBytes+1, ref.SHA256); err != nil {
				t.Fatal(err)
			}
		}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "archive-failures")
			issue := f.create(t, "Archived revision")
			item := string(issue.WorkItemID)
			table := archiveTestTable(t, f.service, "collaboration_versions", "record_id=?", item)
			ref, data := publishTestSection(t, f, item, "revision", item, 1, int64(issue.Revision), table)
			if test.mutate != nil {
				test.mutate(t, f, item, &ref, &data)
			}
			fetches := 0
			f.service.config.ArchiveReader = func(_ context.Context, got attachment.ArchiveReference, presign bool) (entryauthority.ArchiveResponse, error) {
				fetches++
				if len(data) == 0 {
					return entryauthority.ArchiveResponse{}, errors.New("not found")
				}
				if got.Key != ref.Key {
					t.Fatal("resolver changed reference")
				}
				response := entryauthority.ArchiveResponse{Data: data}
				if presign {
					response.Access = &attachment.ArchiveRead{ArchiveReference: ref, URL: "https://objects.example.test/section", ExpiresAt: f.service.config.now().Add(5 * time.Minute)}
				}
				return response, nil
			}
			token := f.token
			if test.name == "signed read" {
				token = testHubAdminToken
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+item+"/versions/1?archive_url=true", token, nil)
			status := http.StatusOK
			if test.availability != "available" {
				status = http.StatusServiceUnavailable
			}
			requireNativeStatus(t, response, status)
			var result struct {
				Archive tracker.ArchiveDescriptor `json:"archive"`
			}
			decodeHubResponse(t, response, &result)
			if result.Archive.Availability != test.availability || fetches != test.wantFetches {
				t.Fatalf("availability=%s fetches=%d body=%s", result.Archive.Availability, fetches, response.Body.String())
			}
			if (result.Archive.URL != "") != (test.name == "signed read") {
				t.Fatalf("unexpected signed URL descriptor: %+v", result.Archive)
			}
		})
	}
}

func TestArchiveSectionIsolation(t *testing.T) {
	f := newNativeFixture(t, nil, "", "archive-isolation")
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "archive-other")
	items := []tracker.NativeIssue{f.create(t, "First revision"), f.create(t, "Second revision")}
	hidden := other.create(t, "Hidden issue body")
	objects := map[string][]byte{}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER collaboration_versions_no_delete"); err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		id := string(item.WorkItemID)
		table := archiveTestTable(t, f.service, "collaboration_versions", "record_id=?", id)
		column := slices.Index(table.Columns, "record_json")
		var snapshot string
		if err := json.Unmarshal(table.Rows[0][column], &snapshot); err != nil {
			t.Fatal(err)
		}
		var historical tracker.NativeIssue
		if err := json.Unmarshal([]byte(snapshot), &historical); err != nil {
			t.Fatal(err)
		}
		historical.Dependencies = []tracker.NativeWorkItemID{hidden.WorkItemID}
		historical.Blockers = []tracker.NativeDependency{{ID: hidden.WorkItemID, ProjectID: other.project.ID, Title: hidden.Title}}
		encoded, err := json.Marshal(historical)
		if err != nil {
			t.Fatal(err)
		}
		table.Rows[0][column], err = json.Marshal(string(encoded))
		if err != nil {
			t.Fatal(err)
		}
		ref, data := publishTestSection(t, f, id, "revision", id, 1, int64(item.Revision), table)
		objects[ref.Key] = data
		if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM collaboration_versions WHERE record_id=?", id); err != nil {
			t.Fatal(err)
		}
	}
	requests := 0
	f.service.config.ArchiveReader = func(_ context.Context, ref attachment.ArchiveReference, presign bool) (entryauthority.ArchiveResponse, error) {
		requests++
		if presign {
			t.Fatal("limited principal received an unfiltered archive URL")
		}
		return entryauthority.ArchiveResponse{Data: objects[ref.Key]}, nil
	}
	for _, item := range items {
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(item.WorkItemID)+"/versions/1?archive_url=true", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		if !strings.Contains(response.Body.String(), item.Title) || strings.Contains(response.Body.String(), hidden.Title) {
			t.Fatal("archive returned the wrong issue")
		}
	}
	if requests != 2 {
		t.Fatalf("requests=%d", requests)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, other.base+"/work-items/"+string(hidden.WorkItemID)+"/versions/1", f.token, nil)
	requireNativeStatus(t, response, http.StatusNotFound)
	if requests != 2 {
		t.Fatal("unauthorized archive reached object storage")
	}
}

func TestArchiveQueryKeepsReusedRowIDs(t *testing.T) {
	f := newNativeFixture(t, nil, "", "archive-rowid")
	item := f.create(t, "Historical revision")
	id := string(item.WorkItemID)
	table := archiveTestTable(t, f.service, "collaboration_versions", "record_id=?", id)
	if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER collaboration_versions_no_delete"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM collaboration_versions WHERE record_id=?", id); err != nil {
		t.Fatal(err)
	}
	second := f.create(t, "New revision")
	section := &attachment.ArchiveSection{
		OrganizationID: string(f.project.OrganizationID), ProjectID: string(f.project.ID),
		WorkItemID: id, SectionID: "revision", Tables: []attachment.ArchiveTable{table},
	}
	query, err := newNativeArchiveQuery(t.Context(), f.service.database.reader, section, id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		subject string
		want    int
	}{{id, 1}, {string(second.WorkItemID), 0}} {
		var count int
		if err := query.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_versions WHERE record_id=?", test.subject).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != test.want {
			t.Fatalf("revision %s count after rowid reuse: %d, want %d", test.subject, count, test.want)
		}
	}
}

func TestArchiveQueryScopesLiveFallback(t *testing.T) {
	f := newNativeFixture(t, nil, "", "archive-query-scope")
	item := f.create(t, "Selected issue")
	f.create(t, "Unrelated issue")
	for _, kind := range []string{"run", "change", "revision", "conversation", "history"} {
		for _, name := range nativeArchiveTables(kind) {
			t.Run(name, func(t *testing.T) {
				table := archiveTestTable(t, f.service, name, "0")
				section := &attachment.ArchiveSection{
					OrganizationID: string(f.project.OrganizationID), ProjectID: string(f.project.ID),
					WorkItemID: string(item.WorkItemID), SectionID: kind, Tables: []attachment.ArchiveTable{table},
				}
				query, err := newNativeArchiveQuery(t.Context(), f.service.database.reader, section, "absent-subject", 1, 1)
				if err != nil {
					t.Fatal(err)
				}
				statement := "SELECT count(*) FROM " + name
				var count int
				if err := query.QueryRowContext(t.Context(), statement).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Errorf("unrelated live records entered archive overlay: %d", count)
				}
				sql, args := query.statement(statement, nil)
				rows, err := f.service.database.reader.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+sql, args...)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				for rows.Next() {
					var id, parent, unused int
					var detail string
					if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
						t.Fatal(err)
					}
					if strings.HasPrefix(detail, "SCAN main.") || strings.HasPrefix(detail, "SCAN live") {
						t.Errorf("archive read scans an unbounded live table: %s", detail)
					}
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
