package hubserver

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type archiveRevisionFixture struct {
	nativeFixture
	scope hubscope.NativeScope
	issue tracker.NativeIssue
	table attachment.ArchiveTable
}

func newArchiveRevisionFixture(t *testing.T, f nativeFixture, title string, count int64) archiveRevisionFixture {
	t.Helper()
	scope := analyticsTestScope(t, f)
	var number int64
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		number, err = r.NextWorkItemNumber(t.Context(), scope.Project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	now := f.service.config.Clock().UTC().Truncate(time.Microsecond)
	issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: tracker.NativeWorkItemID(hubapi.NewID("wi")), Number: int(number), Revision: tracker.Revision(count + 1), Profile: "native"}, Title: title, State: "Todo", Actor: scope.Actor(), CreatedAt: now, UpdatedAt: now, LastActivityAt: now}
	content := eventstore.NewContent(f.service.runDetailStore())
	version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: count + 1}, historyTestJSON(t, issue))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: scope.Organization, ProjectID: scope.Project, NativeID: issue.WorkItemID, Number: number, Revision: count + 1, Title: title, WorkflowStateName: &issue.State, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, CreatedAt: now, UpdatedAt: now, NativeCreatedAt: now, NativeUpdatedAt: now, LastActivityAt: now}); err != nil {
			return err
		}
		_, err := w.CreateCollaborationVersion(t.Context(), version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	table := attachment.ArchiveTable{Name: "collaboration_versions", Columns: []string{"rowid", "organization_id", "project_id", "work_item_id", "record_id", "revision", "record_json", "encoding"}}
	for revision := int64(1); revision <= count; revision++ {
		snapshot := issue
		snapshot.Revision = tracker.Revision(revision)
		table.Rows = append(table.Rows, archiveRevisionCells(t, revision, scope.Organization, scope.Project, issue.WorkItemID, string(issue.WorkItemID), revision, string(historyTestJSON(t, snapshot)), "snapshot"))
	}
	return archiveRevisionFixture{nativeFixture: f, scope: scope, issue: issue, table: table}
}

func archiveRevisionCells(t *testing.T, values ...any) []json.RawMessage {
	t.Helper()
	row := make([]json.RawMessage, len(values))
	for index, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		row[index] = raw
	}
	return row
}

func (f archiveRevisionFixture) publish(t *testing.T) (attachment.ArchiveReference, []byte, string) {
	t.Helper()
	item := string(f.issue.WorkItemID)
	sectionID := "historical_" + item
	section := attachment.ArchiveSection{SchemaVersion: 1, OrganizationID: string(f.scope.Organization), ProjectID: string(f.scope.Project), WorkItemID: item, SectionID: sectionID, Tables: []attachment.ArchiveTable{f.table}}
	raw, err := json.Marshal(section)
	if err != nil {
		t.Fatal(err)
	}
	ref, data, err := attachment.PrepareArchive(string(f.scope.Organization), 1, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	attachmentID := "att_" + ref.SHA256[:32]
	now := f.service.config.Clock()
	record := attachmentstore.ArchiveRecord{OrganizationID: f.scope.Organization, ProjectID: f.scope.Project, ID: attachmentID, Uploader: f.scope.Actor().PrincipalID, Reference: ref, CreatedAt: now}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.PrepareAttachmentArchive(t.Context(), record); err != nil {
			return err
		}
		if err := w.PublishAttachmentArchive(t.Context(), record, f.issue.WorkItemID); err != nil {
			return err
		}
		object, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: f.scope.Organization, ID: attachmentID, Reference: ref, CreatedAt: now})
		if err != nil {
			return err
		}
		return w.AddArchiveOwner(t.Context(), artifactstore.ArchiveOwner{OrganizationID: f.scope.Organization, ArchiveID: object.ID, Kind: "issue-storage-archive", ID: attachmentID, CreatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	admin := hostedPostgresAdmin(t, f.service)
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO issue_archive_sections (organization_id,project_id,work_item_id,archive_revision,section_id,attachment_id,kind,subject_id,first_sequence,last_sequence,retirement_complete) VALUES ($1,$2,$3,$4,$5,$6,'revision',$3,1,$7,true)`, f.scope.Organization, f.scope.Project, f.issue.WorkItemID, int64(f.issue.Revision), sectionID, attachmentID, int64(f.issue.Revision)-1); err != nil {
		t.Fatal(err)
	}
	return ref, data, sectionID
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
	var hotValue, archivedValue map[string]any
	if err := json.Unmarshal(left, &hotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(right, &archivedValue); err != nil {
		t.Fatal(err)
	}
	normalizeTestDashboardWatermark(t, hotValue, archivedValue)
	if !reflect.DeepEqual(hotValue, archivedValue) {
		t.Fatalf("archived output differs\nhot: %s\narchive: %s", hot, archived)
	}
}

func TestRuntimeEvidenceEnvelope(t *testing.T) {
	t.Parallel()
	result := tracker.NativeRuntimeEvidence{Attempt: &tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{
		CompletionBody: strings.Repeat("a", (1<<20)-(12<<10)),
		Evidence:       []tracker.NativeEvidence{{AttachmentID: "att_" + strings.Repeat("a", 32), Caption: strings.Repeat("<", 3072)}},
	}}}
	legacy := httptest.NewRecorder()
	if err := nativeArchiveJSON(echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), legacy), http.StatusOK, result); err != nil {
		t.Fatal(err)
	}
	var refusal hubapi.APIErrorResponse
	if legacy.Code != http.StatusUnprocessableEntity || json.Unmarshal(legacy.Body.Bytes(), &refusal) != nil || refusal.Code != "invalid_request" || !strings.Contains(refusal.Message, "Complete runtime input exceeds") {
		t.Fatalf("legacy runner received no explicit response size error: status %d", legacy.Code)
	}
	recorder := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/?evidence_after=", nil), recorder)
	if err := nativeArchiveJSON(c, http.StatusOK, result); err != nil {
		t.Fatal(err)
	}
	var got tracker.NativeRuntimeEvidence
	if recorder.Body.Len() > 1<<20 || json.Unmarshal(recorder.Body.Bytes(), &got) != nil || got.Attempt == nil || got.Attempt.NextEvidenceAfter != "start" || !slices.Contains(got.Attempt.Unavailable, "evidence") || got.Attempt.CompletionBody != result.Attempt.CompletionBody {
		t.Fatalf("runtime envelope did not preserve a readable receipt and evidence continuation: %d bytes", recorder.Body.Len())
	}
}

func TestNativeArchiveSectionFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, availability string
		wantFetches        int
	}{
		{"valid", "available", 1}, {"signed read", "available", 1}, {"browser fallback", "available", 1}, {"missing object", "unavailable", 1}, {"digest mismatch", "invalid", 1}, {"crafted URL", "invalid", 0}, {"oversized section", "invalid", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newArchiveRevisionFixture(t, newNativeFixture(t, nil, "", "archive-failures"), "Archived revision", 1)
			ref, data, sectionID := f.publish(t)
			switch test.name {
			case "missing object":
				data = nil
			case "digest mismatch":
				data[0] ^= 1
			case "crafted URL":
				ref.Key = "http://169.254.169.254/latest/meta-data/"
			case "oversized section":
				ref.UncompressedSize = attachment.MaxSectionBytes + 1
			}
			fetches := 0
			f.service.config.ArchiveReader = func(_ context.Context, got attachment.ArchiveReference, presign bool) (entryauthority.ArchiveResponse, error) {
				fetches++
				if len(data) == 0 {
					return entryauthority.ArchiveResponse{}, errors.New("not found")
				}
				if got != ref {
					t.Fatal("resolver changed reference")
				}
				response := entryauthority.ArchiveResponse{Data: data}
				if presign {
					response.Access = &attachment.ArchiveRead{ArchiveReference: ref, URL: "https://objects.example.test/section", ExpiresAt: f.service.config.Clock().Add(5 * time.Minute)}
				}
				return response, nil
			}
			token := f.token
			if test.name == "signed read" {
				token = testHubAdminToken
			}
			target := f.base + "/work-items/" + string(f.issue.WorkItemID) + "/versions/1?archive_url=true"
			response := httptest.NewRecorder()
			if test.name == "crafted URL" || test.name == "oversized section" {
				state := &nativeArchiveRead{service: f.service, scope: f.scope, presign: true}
				_, _, err := state.load(t.Context(), string(f.issue.WorkItemID), "revision", string(f.issue.WorkItemID), 1, sectionID, 1, 1, ref)
				if err == nil {
					t.Fatal("invalid historical reference accepted")
				}
				request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
				c := echo.New().NewContext(request, response)
				c.Set("native_scope", f.scope)
				if err := f.service.nativeAPIError(c, err); err != nil {
					t.Fatal(err)
				}
			} else {
				request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
				request.Header.Set("Authorization", "Bearer "+token)
				if test.name == "browser fallback" {
					request.Header.Set("X-Detent-Archive-Read", "1")
				}
				f.service.Handler().ServeHTTP(response, request)
			}
			status := http.StatusOK
			if test.availability != "available" {
				status = http.StatusServiceUnavailable
			}
			requireNativeStatus(t, response, status)
			var result struct {
				Archive tracker.ArchiveDescriptor `json:"archive"`
			}
			decodeHubResponse(t, response, &result)
			expected := test.availability
			if test.name == "browser fallback" {
				expected = ""
				if !strings.Contains(response.Body.String(), `"title":"Archived revision"`) {
					t.Fatal("browser fallback lost resolved revision")
				}
			}
			if result.Archive.Availability != expected || fetches != test.wantFetches {
				t.Fatalf("availability=%s fetches=%d body=%s", result.Archive.Availability, fetches, response.Body.String())
			}
			if (result.Archive.URL != "") != (test.name == "signed read") {
				t.Fatalf("unexpected signed URL descriptor: %+v", result.Archive)
			}
		})
	}
}

func TestArchiveSectionIsolation(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "archive-isolation")
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "archive-other")
	hidden := other.create(t, "Hidden issue body")
	items := []archiveRevisionFixture{newArchiveRevisionFixture(t, f, "First revision", 1), newArchiveRevisionFixture(t, f, "Second revision", 1)}
	objects := map[string][]byte{}
	for _, item := range items {
		historical := item.issue
		historical.Revision = 1
		historical.Dependencies = []tracker.NativeWorkItemID{hidden.WorkItemID}
		historical.Blockers = []tracker.NativeDependency{{ID: hidden.WorkItemID, ProjectID: other.project.ID, Title: hidden.Title}}
		item.table.Rows[0][slices.Index(item.table.Columns, "record_json")] = archiveRevisionCells(t, string(historyTestJSON(t, historical)))[0]
		ref, data, _ := item.publish(t)
		objects[ref.Key] = data
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
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(item.issue.WorkItemID)+"/versions/1?archive_url=true", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		if !strings.Contains(response.Body.String(), item.issue.Title) || strings.Contains(response.Body.String(), hidden.Title) {
			t.Fatal("archive returned the wrong issue")
		}
		var result tracker.NativeIssue
		decodeHubResponse(t, response, &result)
		if len(result.Blockers) != 0 || len(result.Dependencies) != 0 {
			t.Fatal("archive retained a hidden related issue identity")
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
	t.Parallel()
	f := newNativeFixture(t, nil, "", "archive-rowid")
	repository, ok := f.service.config.TenantRepository.(*postgres.Repository)
	if !ok {
		t.Fatal("archive fixture lacks a PostgreSQL repository")
	}
	historical := newArchiveRevisionFixture(t, f, "Historical revision", 1)
	current := f.create(t, "New revision")
	var live corestore.WorkItem
	if err := repository.ReadTenant(t.Context(), historical.scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		live, err = r.WorkItem(t.Context(), historical.scope.Project, current.WorkItemID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	historical.table.Rows[0][0] = archiveRevisionCells(t, live.ID)[0]
	ref, data, _ := historical.publish(t)
	f.service.config.ArchiveReader = func(_ context.Context, got attachment.ArchiveReference, _ bool) (entryauthority.ArchiveResponse, error) {
		if connections := repository.Stats().AcquiredConns(); connections != 0 {
			t.Fatalf("archive read holds %d PostgreSQL connections", connections)
		}
		if got != ref {
			t.Fatal("wrong archive")
		}
		return entryauthority.ArchiveResponse{Data: data}, nil
	}
	section := attachment.ArchiveSection{Tables: []attachment.ArchiveTable{historical.table}}
	if _, err := archivedVersionPayload(section, string(current.WorkItemID), 1); !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("reused numeric identity matched another record: %v", err)
	}
	requireRetiredRevision := func() {
		t.Helper()
		err := repository.ReadTenant(t.Context(), historical.scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.CollaborationVersion(t.Context(), string(historical.issue.WorkItemID), 1)
			return err
		})
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("retired revision should be absent from live versions: %v", err)
		}
	}
	requireVersion := func(id tracker.NativeWorkItemID, title string) {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(id)+"/versions/1", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		if issue.WorkItemID != id || issue.Title != title {
			t.Fatalf("revision after rowid reuse: %+v", issue)
		}
	}
	f.service.tokenUseWork.Wait()
	requireRetiredRevision()
	requireVersion(historical.issue.WorkItemID, historical.issue.Title)
	requireRetiredRevision()
	requireVersion(current.WorkItemID, current.Title)
}

func TestArchiveQueryScopesLiveFallback(t *testing.T) {
	t.Parallel()
	f := newPostgresStorageArchiveFixture(t)
	installReportsBudgetTrace(t, f.service)
	admin := hostedPostgresAdmin(t, f.service)
	items := []tracker.NativeIssue{f.closed(t), f.closed(t)}
	type retainedRead struct {
		item             tracker.NativeWorkItemID
		conversation     string
		version, history []byte
		messages         []conversationMessageRecord
	}
	reads := make([]retainedRead, 0, 2)
	for _, item := range items {
		record := f.messages(t, item.WorkItemID, 2, conversation.VisibilityShared)
		version, err := f.service.readVersion(t.Context(), f.scope, string(item.WorkItemID), "", 1)
		if err != nil {
			t.Fatal(err)
		}
		history := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(item.WorkItemID)+"/history?limit=100", f.token, nil)
		requireNativeStatus(t, history, http.StatusOK)
		var page tracker.Page[tracker.CollaborationEvent]
		decodeHubResponse(t, history, &page)
		if len(page.Items) < 2 {
			t.Fatalf("history fixture lost creation/transition: %+v", page.Items)
		}
		found := false
		for _, event := range page.Items {
			if event.AggregateSequence == 1 {
				found = true
				if event.Type != "issue.created" || event.AggregateID != item.WorkItemID || event.Data.Revision != 1 {
					t.Fatalf("historical fields changed: %+v", event)
				}
			}
		}
		if !found {
			t.Fatal("creation history absent")
		}
		reads = append(reads, retainedRead{item.WorkItemID, record.ID, bytes.Clone(version), bytes.Clone(history.Body.Bytes()), f.readMessages(t, record.ID)})
	}
	for _, read := range reads {
		f.drain(t, read.conversation)
	}
	for _, read := range reads {
		var mu sync.Mutex
		var statements []tracedStatement
		timing := &requestTiming{Trace: func(query string, args []driver.NamedValue) {
			values := make([]any, len(args))
			for i, arg := range args {
				values[i] = arg.Value
			}
			mu.Lock()
			statements = append(statements, tracedStatement{query, values})
			mu.Unlock()
		}}
		ctx := withRequestTiming(t.Context(), timing)
		version, err := f.service.readVersion(ctx, f.scope, string(read.item), "", 1)
		if err != nil || !bytes.Equal(version, read.version) {
			t.Fatalf("immutable historical version changed: error=%v", err)
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(read.item)+"/history?limit=100", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		assertArchiveGolden(t, read.history, response.Body.Bytes())
		page, err := f.service.readPGConversationPage(ctx, f.scope, read.conversation, 0, 100)
		if err != nil || len(page.Messages) != len(read.messages) {
			t.Fatalf("unrelated live records entered archive read: messages=%+v error=%v", page.Messages, err)
		}
		for i, message := range page.Messages {
			before := read.messages[i]
			if message.ID != before.ID || message.Seq != before.Seq || message.Revision != before.Revision || message.Text != before.Text || !storageArchiveJSONEqual(t, message.Data, before.Data) || message.Role != before.Role || !reflect.DeepEqual(message.Actor, before.Actor) || !message.CreatedAt.Equal(before.CreatedAt) || !message.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("historical message fields changed across archive: %+v", message)
			}
		}
		if len(page.Messages) != 2 || page.Messages[0].ConversationID != read.conversation || page.Messages[1].ConversationID != read.conversation {
			t.Fatalf("archive read crossed conversation scope: %+v", page.Messages)
		}
		if err := f.service.config.TenantRepository.ReadTenant(ctx, f.scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
			for _, lookup := range []struct {
				name string
				read func() error
			}{
				{"run", func() error { _, err := reader.Attempt(ctx, "absent-subject"); return err }},
				{"change", func() error { _, err := reader.Version(ctx, "absent-change", "absent-subject"); return err }},
				{"revision", func() error { _, err := reader.CollaborationVersion(ctx, "absent-subject", 1); return err }},
				{"conversation", func() error { _, err := reader.Conversation(ctx, f.scope.Project, "absent-subject"); return err }},
				{"history", func() error { _, err := reader.RunEvent(ctx, f.scope.Project, read.item, "absent-subject"); return err }},
			} {
				if err := lookup.read(); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("unrelated live records entered %s lookup: %v", lookup.name, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		captured := append([]tracedStatement(nil), statements...)
		mu.Unlock()
		if len(captured) == 0 {
			t.Fatal("no archive owner metadata statements traced")
		}
		for _, statement := range captured {
			if !reportsBudgetReadStatement(statement.query) {
				continue
			}
			plan, raw := reportsExplainPostgresPlan(t, admin, f.scope.Organization, statement)
			var inspect func(reportsBudgetPlan)
			inspect = func(node reportsBudgetPlan) {
				keys := map[string]string{"native_attempts": "id =", "change_versions": "id =", "collaboration_versions": "record_id =", "conversations": "id =", "conversation_messages": "conversation_id =", "collaboration_events": "work_item_id ="}
				if key, ok := keys[node.Relation]; ok && node.Loops > 0 {
					conditions, indexed := reportsBudgetConditions(node, node.Relation)
					if node.Relation == "collaboration_events" && strings.Contains(statement.query, "-- name: RecordGetEvent :one") {
						key = "id ="
					}
					if !indexed || !strings.Contains(conditions, "organization_id =") || !strings.Contains(conditions, key) {
						t.Errorf("archive lookup scans unrelated live/history rows: %s key=%s conditions=%s plan=%s", node.Relation, key, conditions, raw)
					}
					if visits := (node.Rows + node.Removed + node.IndexRemoved) * node.Loops; visits > 201 {
						t.Errorf("archive metadata visits=%.0f above the existing201-row source page: plan=%s", visits, raw)
					}
				}
				for _, child := range node.Plans {
					inspect(child)
				}
			}
			inspect(plan)
		}
	}
}

func TestArchiveSQLScalar(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		raw     string
		want    any
		invalid bool
	}{
		{raw: `"quote' and question? --"`, want: "quote' and question? --"}, {raw: `"nul\u0000byte"`, want: "nul\x00byte"}, {raw: `9223372036854775806`, want: json.Number("9223372036854775806")}, {raw: `1.25`, want: json.Number("1.25")}, {raw: `true`, want: true}, {raw: `null`, want: nil}, {raw: `{}`, want: map[string]any{}}, {raw: `1; SELECT 2`, invalid: true},
	} {
		t.Run(test.raw, func(t *testing.T) {
			payload := `{"legacy_value":` + test.raw + `}`
			section := attachment.ArchiveSection{Tables: []attachment.ArchiveTable{{Name: "collaboration_versions", Columns: []string{"rowid", "record_id", "revision", "record_json", "encoding"}, Rows: [][]json.RawMessage{archiveRevisionCells(t, 1, "record", 1, payload, "snapshot")}}}}
			got, err := archivedVersionPayload(section, "record", 1)
			if test.invalid {
				if err == nil {
					t.Fatal("invalid historical snapshot accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != payload {
				t.Fatalf("historical snapshot bytes changed: %s", got)
			}
			decoder := json.NewDecoder(bytes.NewReader(got))
			decoder.UseNumber()
			var value map[string]any
			if err := decoder.Decode(&value); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(value["legacy_value"], test.want) {
				t.Fatalf("archive value=%#v want=%#v", value["legacy_value"], test.want)
			}
		})
	}
}

func TestArchiveQueryExceedsSQLiteParameterCount(t *testing.T) {
	t.Parallel()
	const count = 30000/8 + 1
	f := newArchiveRevisionFixture(t, newNativeFixture(t, nil, "", "archive-large"), "archive with many scalar cells", count)
	if len(f.table.Rows) != count || len(f.table.Rows)*len(f.table.Columns) <= 30000 {
		t.Fatal("fixture lost historical scalar workload")
	}
	ref, data, _ := f.publish(t)
	if ref.UncompressedSize > attachment.MaxSectionBytes {
		t.Fatal("fixture exceeds archive section limit")
	}
	fetches := 0
	f.service.config.ArchiveReader = func(_ context.Context, got attachment.ArchiveReference, _ bool) (entryauthority.ArchiveResponse, error) {
		fetches++
		if got != ref {
			t.Fatal("wrong large section")
		}
		return entryauthority.ArchiveResponse{Data: data}, nil
	}
	for _, revision := range []int64{1, count / 2, count} {
		before := fetches
		ctx := f.service.archiveContext(t.Context(), f.scope, false)
		got, err := f.service.readVersion(ctx, f.scope, string(f.issue.WorkItemID), "", revision)
		if err != nil {
			t.Fatal(err)
		}
		var issue tracker.NativeIssue
		if err := json.Unmarshal(got, &issue); err != nil {
			t.Fatal(err)
		}
		if issue.WorkItemID != f.issue.WorkItemID || int64(issue.Revision) != revision || issue.Title != f.issue.Title || fetches != before+1 {
			t.Fatalf("large section revision=%+v fetches=%d", issue, fetches-before)
		}
	}
}

func TestArchiveMigrationCompatibility(t *testing.T) {
	t.Parallel()
	for _, layout := range []string{"pre-reference", "scoped", "additional metadata"} {
		t.Run(layout, func(t *testing.T) {
			f := newArchiveRevisionFixture(t, newNativeFixture(t, nil, "", "archive-schema"), "Historical schema", 1)
			switch layout {
			case "pre-reference":
				f.table.Columns = []string{"rowid", "record_id", "revision", "record_json", "encoding"}
				original := f.table.Rows[0]
				f.table.Rows[0] = []json.RawMessage{original[0], original[4], original[5], original[6], original[7]}
			case "additional metadata":
				f.table.Columns = append(f.table.Columns, "legacy_nullable", "legacy_integer", "legacy_text")
				f.table.Rows[0] = append(f.table.Rows[0], archiveRevisionCells(t, nil, int64(9223372036854775806), "quote' ? --")...)
			}
			ref, data, _ := f.publish(t)
			fetches := 0
			f.service.config.ArchiveReader = func(_ context.Context, got attachment.ArchiveReference, _ bool) (entryauthority.ArchiveResponse, error) {
				fetches++
				if got != ref {
					t.Fatal("wrong historical schema section")
				}
				return entryauthority.ArchiveResponse{Data: data}, nil
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/versions/1", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			if issue.WorkItemID != f.issue.WorkItemID || issue.Revision != 1 || issue.Title != "Historical schema" || fetches != 1 {
				t.Fatalf("historical schema lost snapshot: %+v fetches=%d", issue, fetches)
			}
		})
	}
}

func TestOlderArchiveViews(t *testing.T) {
	t.Parallel()
	t.Run("change recovery", func(t *testing.T) {
		f := newChangeFixture(t, nil)
		version := f.publish(t, "legacy-version", "")
		review := tracker.ChangeReview{ID: "legacy-review", VersionID: version.ID, Decision: "commented", Body: "Historical review", CreatedAt: f.service.config.Clock()}
		record, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: f.project.OrganizationID, ChangeID: f.change.ID, Sequence: 1, VersionID: &version.ID, Kind: "review", SourceKey: &review.ID}, historyTestJSON(t, review))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.(*postgres.Repository).WriteRecords(t.Context(), f.project.OrganizationID, corestore.Background, func(w eventstore.Writer) error {
			_, err := w.AppendEvidence(t.Context(), record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID)+"?view=recovery&detail=full", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var recovery tracker.NativeRecovery
		decodeHubResponse(t, response, &recovery)
		if recovery.ChangeDetail == nil || len(recovery.ChangeDetail.Reviews) != 1 || recovery.ChangeDetail.Reviews[0].Body != review.Body {
			t.Fatalf("lost historical evidence: %s", response.Body.String())
		}
	})
	t.Run("attempt and compacted history", func(t *testing.T) {
		a := newTerminalAttemptFixture(t)
		if err := a.service.stopGitHubWebhookMaintenance(); err != nil {
			t.Fatal(err)
		}
		if a.service.outbox != nil {
			a.service.outbox.stop()
		}
		a.finish(t)
		issue := readWorkItem(t, a.nativeFixture, a.issue.WorkItemID, "")
		response := performHubAPIRequest(t, a.service, http.MethodPost, a.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", a.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "close_older_archive"}, ExpectedRevision: issue.Revision, State: "Done", Reason: "user_requested"})
		requireNativeStatus(t, response, http.StatusOK)
		a.advance(74 * time.Hour)
		var mu sync.Mutex
		objects := map[string][]byte{}
		previous := a.service.config.Content
		a.service.config.Content = func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
			mu.Lock()
			if request.Write {
				objects[request.Reference.Key] = bytes.Clone(request.Data)
			}
			data, ok := objects[request.Reference.Key]
			mu.Unlock()
			if ok {
				return bytes.Clone(data), nil
			}
			if previous == nil {
				return nil, errors.New("object missing")
			}
			return previous(ctx, request)
		}
		item, attempt := string(a.issue.WorkItemID), a.start.Data.AttemptID
		suffixes := []string{"/attempts/" + attempt, "/attempts?view=recovery", "/history?limit=200"}
		before := map[string][]byte{}
		for _, suffix := range suffixes {
			response := performHubAPIRequest(t, a.service, http.MethodGet, a.base+"/work-items/"+item+suffix, a.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			before[suffix] = bytes.Clone(response.Body.Bytes())
		}
		a.service.tokenUseWork.Wait()
		if err := a.service.maintainNativeRetention(t.Context(), a.service.config.Clock()); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range suffixes {
			response := performHubAPIRequest(t, a.service, http.MethodGet, a.base+"/work-items/"+item+suffix, a.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			assertArchiveGolden(t, before[suffix], response.Body.Bytes())
			if suffix == "/history?limit=200" && !strings.Contains(response.Body.String(), "run.finished") {
				t.Fatalf("lost compacted history: %s", response.Body.String())
			}
		}
		mu.Lock()
		objectCount := len(objects)
		mu.Unlock()
		if objectCount == 0 {
			t.Fatal("retention did not publish archive objects")
		}
	})
}
