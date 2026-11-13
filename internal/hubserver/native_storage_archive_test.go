package hubserver

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type postgresStorageArchiveFixture struct {
	nativeFixture
	clock   *atomic.Int64
	scope   hubscope.NativeScope
	objects map[string][]byte
	mu      sync.Mutex
	bodies  *conversationContentFixture
	hook    func(entryauthority.ContentRequest) error
}

func newPostgresStorageArchiveFixture(t *testing.T) *postgresStorageArchiveFixture {
	t.Helper()
	f := &postgresStorageArchiveFixture{clock: &atomic.Int64{}, objects: map[string][]byte{}}
	f.clock.Store(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC).Add(-74 * time.Hour).UnixNano())
	content := func(ctx context.Context, r entryauthority.ContentRequest) ([]byte, error) {
		if f.service != nil {
			f.checkIO(t)
		}
		f.mu.Lock()
		if r.Write {
			f.objects[r.Reference.Key] = bytes.Clone(r.Data)
		}
		data, ok := f.objects[r.Reference.Key]
		f.mu.Unlock()
		if f.hook != nil {
			if err := f.hook(r); err != nil {
				return nil, err
			}
		}
		if !ok {
			return nil, errors.New("object missing")
		}
		return bytes.Clone(data), nil
	}
	service := newClockedPostgresNativeService(t, Config{now: func() time.Time { return time.Unix(0, f.clock.Load()).UTC() }, Content: content})
	f.nativeFixture = newNativeFixture(t, service, "org_first", "archive example")
	credential, status, err := service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil {
		t.Fatalf("archive credential status=%d error=%v", status, err)
	}
	f.scope = hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
	f.bodies = &conversationContentFixture{onIO: func() error { f.checkIO(t); return nil }}
	store := newConversationStore(nil)
	store.Contents = f.bodies
	service.conversations = &conversationService{Server: service.conversationServer(), Store: store, Broker: newNotificationBroker().Broker}
	t.Cleanup(func() { service.conversations = nil })
	service.tokenUseWork.Wait()
	return f
}

func (f *postgresStorageArchiveFixture) now() time.Time { return time.Unix(0, f.clock.Load()).UTC() }
func (f *postgresStorageArchiveFixture) checkIO(t *testing.T) {
	t.Helper()
	repository, ok := f.service.config.TenantRepository.(*postgres.Repository)
	if !ok {
		t.Fatal("archive fixture lacks PostgreSQL repository")
	}
	if n := repository.Stats().AcquiredConns(); n != 0 {
		t.Fatalf("archive I/O holds %d PostgreSQL connections", n)
	}
}
func (f *postgresStorageArchiveFixture) write(t *testing.T, fn func(tenantstore.Writer) error) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, fn); err != nil {
		t.Fatal(err)
	}
}
func (f *postgresStorageArchiveFixture) transition(t *testing.T, item tracker.NativeWorkItemID, state string) tracker.NativeIssue {
	t.Helper()
	issue := readWorkItem(t, f.nativeFixture, item, "")
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(item)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("transition")}, ExpectedRevision: issue.Revision, State: state, Reason: "user_requested"})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &issue)
	f.service.tokenUseWork.Wait()
	return issue
}
func (f *postgresStorageArchiveFixture) closed(t *testing.T) tracker.NativeIssue {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("create")}, Title: "Archive example", Body: strings.Repeat("Full issue content. ", 80) + issueContractTestSections, State: "Todo"})
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	issue = f.transition(t, issue.WorkItemID, "Done")
	f.clock.Add(int64(74 * time.Hour))
	return issue
}
func (f *postgresStorageArchiveFixture) messages(t *testing.T, item tracker.NativeWorkItemID, count int, visibility conversation.Visibility) conversationstore.Conversation {
	t.Helper()
	at := f.now()
	record := conversationstore.Conversation{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, ID: conversation.NewConversationID(), OwnerPrincipalID: f.scope.Credential.ID, Origin: "user", Title: "Archive conversation", Visibility: visibility, Status: conversation.StatusSettled, CreatedAt: at, UpdatedAt: at, SettledAt: new(at), Execution: conversation.Execution{Status: conversation.ExecutionCompleted, Settled: true, Owner: conversation.Owner{LeaseID: "lease_settled_archive"}}}
	if visibility == conversation.VisibilityShared {
		record.WorkItemID = string(item)
		record.LinkedAt = new(at)
	} else {
		record.SubjectWorkItemID = string(item)
	}
	f.write(t, func(w tenantstore.Writer) error {
		var err error
		record, err = w.CreateConversation(t.Context(), record)
		return err
	})
	admin := nativeAttemptTestDatabase(t, f.nativeFixture)
	_, err := admin.ExecContext(t.Context(), `INSERT INTO conversation_messages (organization_id,conversation_id,id,seq,role,kind,text,data_json,delivery,actor_json,actor_kind,actor_principal_id,created_at,updated_at,text_bytes,stored_text_bytes)
SELECT $1,$2,'msg_'||md5($1||':'||$2||':'||n::text),n,'assistant','text','retained message '||n::text,'{"opaque":{"kept":true}}','completed','{"kind":"runner","principal_id":"example-runner"}','runner','example-runner',$4,$4,length('retained message '||n::text),length('retained message '||n::text) FROM generate_series(1,$3::integer) n`, f.project.OrganizationID, record.ID, count, at)
	if err != nil {
		t.Fatal(err)
	}
	f.service.tokenUseWork.Wait()
	return record
}
func (f *postgresStorageArchiveFixture) inline(t *testing.T, id string) int {
	t.Helper()
	var n int
	if err := nativeAttemptTestDatabase(t, f.nativeFixture).QueryRowContext(t.Context(), `SELECT count(*) FROM conversation_messages WHERE organization_id=$1 AND conversation_id=$2 AND (text<>'' OR data_json<>'{}'::jsonb)`, f.project.OrganizationID, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func (f *postgresStorageArchiveFixture) stamp(t *testing.T) attachmentstore.StorageArchiveStamp {
	t.Helper()
	var page attachmentstore.StorageArchivePage
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		page, err = r.(attachmentstore.StorageArchiveReader).StorageArchiveCandidates(t.Context(), f.now().Add(-issueStorageArchiveAge), attachmentstore.StorageArchiveCursor{}, 4)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("archive candidates=%d", len(page.Items))
	}
	return page.Items[0].Stamp
}
func (f *postgresStorageArchiveFixture) archive(t *testing.T) {
	t.Helper()
	f.service.tokenUseWork.Wait()
	if err := f.service.archiveClosedNativeIssuesTenant(t.Context(), f.project.OrganizationID, f.now()); err != nil {
		t.Fatal(err)
	}
}
func (f *postgresStorageArchiveFixture) drain(t *testing.T, id string) {
	t.Helper()
	cursor := func() int64 {
		t.Helper()
		var value int64
		err := nativeAttemptTestDatabase(t, f.nativeFixture).QueryRowContext(t.Context(), `SELECT a.last_message_scanned FROM issue_storage_archives a JOIN conversations c ON c.organization_id=a.organization_id AND c.project_id=a.project_id AND c.work_item_id=a.work_item_id WHERE c.organization_id=$1 AND c.id=$2`, f.project.OrganizationID, id).Scan(&value)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		return value
	}
	for range 6 {
		before, position := f.inline(t, id), cursor()
		f.archive(t)
		after := f.inline(t, id)
		if after == 0 {
			return
		}
		if after >= before && position == cursor() {
			t.Fatalf("payload conversion and source cursor made no progress: %d -> %d", before, after)
		}
	}
	t.Fatal("bounded archive continuation left inline payloads")
}
func (f *postgresStorageArchiveFixture) readMessages(t *testing.T, id string) []conversationMessageRecord {
	t.Helper()
	var result []conversationMessageRecord
	before := int64(0)
	for {
		page, err := f.service.readPGConversationPage(t.Context(), f.scope, id, before, 100)
		if err != nil {
			t.Fatal(err)
		}
		result = append(page.Messages, result...)
		if len(page.Messages) < 100 {
			break
		}
		next := page.Messages[0].Seq
		if before != 0 && next >= before {
			t.Fatal("message cursor did not advance")
		}
		before = next
	}
	return result
}
func archiveTestBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return raw
}
func (f *postgresStorageArchiveFixture) archiveObjects(t *testing.T) map[string]attachmentstore.StorageArchiveDocument {
	t.Helper()
	result := map[string]attachmentstore.StorageArchiveDocument{}
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, data := range f.objects {
		raw := archiveTestBytes(t, data)
		var document attachmentstore.StorageArchiveDocument
		if json.Unmarshal(raw, &document) == nil && document.SchemaVersion == 2 && (document.Kind == "root" || document.Kind == "conversation_messages") {
			if len(raw) > attachment.MaxSectionBytes {
				t.Fatal("issue archive section exceeds its byte budget")
			}
			result[key] = document
		}
	}
	return result
}

func storageArchiveJSONEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var left, right bytes.Buffer
	if err := json.Compact(&left, a); err != nil {
		t.Fatal(err)
	}
	if err := json.Compact(&right, b); err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(left.Bytes(), right.Bytes())
}

func TestNativeStorageArchivePublication(t *testing.T) {
	for _, phase := range []string{"sections uploaded", "root uploaded", "pointer commit", "pointer committed", "chunk retired", "reopened at sections", "reopened at root", "reopened at retirement", "message children"} {
		t.Run(phase, func(t *testing.T) {
			f := newPostgresStorageArchiveFixture(t)
			issue := f.closed(t)
			record := f.messages(t, issue.WorkItemID, 600, conversation.VisibilityShared)
			before := f.readMessages(t, record.ID)
			if len(before) != 600 {
				t.Fatalf("initial messages=%d", len(before))
			}
			if phase == "message children" {
				raw := []byte("example attachment")
				ref, err := f.bodies.PublishContent(t.Context(), string(f.project.OrganizationID), raw)
				if err != nil {
					t.Fatal(err)
				}
				object := conversationstore.ContentObject{OrganizationID: f.project.OrganizationID, ID: contentArchiveID(string(f.project.OrganizationID), ref), Reference: ref, CreatedAt: f.now()}
				attachmentID := conversation.NewAttachmentID()
				f.write(t, func(w tenantstore.Writer) error {
					owner := w.(conversationstore.AttachmentWriter)
					attachment := conversationstore.Attachment{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, ConversationID: record.ID, ID: attachmentID, PrincipalID: f.scope.Credential.ID, Name: "example.txt", MIME: "text/plain", Size: int64(len(raw)), ArtifactRef: "example-attachment", ContentID: object.ID, CreatedAt: f.now(), ExpiresAt: new(f.now().Add(time.Hour))}
					if err := owner.CreateConversationAttachment(t.Context(), attachment, object); err != nil {
						return err
					}
					return owner.BindConversationAttachment(t.Context(), f.project.ID, record.ID, attachmentID, f.scope.Credential.ID, before[0].ID, f.now())
				})
				f.write(t, func(w tenantstore.Writer) error {
					if err := w.ReplaceMessageReferences(t.Context(), record.ID, before[0].ID, []conversationstore.MessageReference{{OrganizationID: f.project.OrganizationID, ConversationID: record.ID, MessageID: before[0].ID, Kind: conversation.ReferenceIssue, TargetID: string(issue.WorkItemID), Position: 0, Label: "Example", CreatedAt: f.now()}}, f.now()); err != nil {
						return err
					}
					return w.PutToolRevision(t.Context(), conversationstore.ToolRevision{OrganizationID: f.project.OrganizationID, ConversationID: record.ID, MessageID: before[1].ID, Revision: 1, Text: "original tool history", Tool: "example", ItemID: "item_example", Status: "completed"})
				})
			}
			injected := errors.New("injected archive failure")
			hookUsed := false
			if strings.Contains(phase, "uploaded") || strings.HasPrefix(phase, "reopened at") && phase != "reopened at retirement" {
				f.hook = func(request entryauthority.ContentRequest) error {
					if !request.Write || request.Reference.SchemaVersion != 2 || hookUsed {
						return nil
					}
					var document attachmentstore.StorageArchiveDocument
					if err := json.Unmarshal(archiveTestBytes(t, request.Data), &document); err != nil {
						return err
					}
					wantRoot := phase == "root uploaded" || phase == "reopened at root"
					if (document.Kind == "root") != wantRoot {
						return nil
					}
					hookUsed = true
					if strings.HasPrefix(phase, "reopened") {
						f.transition(t, issue.WorkItemID, "Todo")
						return nil
					}
					return injected
				}
			}
			admin := nativeAttemptTestDatabase(t, f.nativeFixture)
			if phase == "pointer commit" {
				if _, err := admin.ExecContext(t.Context(), `CREATE FUNCTION reject_archive_pointer() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected archive pointer failure' USING ERRCODE='23514'; END $$`); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `CREATE TRIGGER reject_archive_pointer BEFORE INSERT OR UPDATE ON issue_storage_archives FOR EACH ROW EXECUTE FUNCTION reject_archive_pointer()`); err != nil {
					t.Fatal(err)
				}
			}
			err := f.service.publishStorageArchive(t.Context(), f.project.OrganizationID, f.stamp(t), f.now())
			fails := phase == "sections uploaded" || phase == "root uploaded" || phase == "pointer commit" || phase == "reopened at sections" || phase == "reopened at root"
			if (err != nil) != fails {
				t.Fatalf("publication error=%v want failure=%v", err, fails)
			}
			if f.inline(t, record.ID) != 600 {
				t.Fatal("publication retired payloads before its durable receipt")
			}
			var pointers int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM issue_storage_archives WHERE organization_id=$1 AND work_item_id=$2`, f.project.OrganizationID, issue.WorkItemID).Scan(&pointers); err != nil {
				t.Fatal(err)
			}
			if (pointers == 0) != fails {
				t.Fatalf("pointer count=%d failed=%v", pointers, fails)
			}
			if phase == "pointer commit" {
				if _, err := admin.ExecContext(t.Context(), `DROP TRIGGER reject_archive_pointer ON issue_storage_archives`); err != nil {
					t.Fatal(err)
				}
			}
			f.hook = nil
			if fails {
				f.clock.Add(int64(time.Second))
			}
			if phase == "reopened at retirement" {
				f.transition(t, issue.WorkItemID, "Todo")
			}
			if phase == "chunk retired" || phase == "message children" || phase == "reopened at retirement" {
				if err := f.service.retireStorageArchiveSections(t.Context(), f.project.OrganizationID, f.now()); err != nil {
					t.Fatal(err)
				}
				want := 100
				if phase == "reopened at retirement" {
					want = 600
				}
				if got := f.inline(t, record.ID); got != want {
					t.Fatalf("inline payloads=%d want=%d", got, want)
				}
			}
			if strings.HasPrefix(phase, "reopened") {
				f.transition(t, issue.WorkItemID, "Done")
				f.clock.Add(int64(74 * time.Hour))
			}
			f.drain(t, record.ID)
			after := f.readMessages(t, record.ID)
			if len(after) != 600 {
				t.Fatalf("retained message metadata=%d", len(after))
			}
			seen := map[string]bool{}
			for i, message := range after {
				if seen[message.ID] || message.ID != before[i].ID || message.Seq != int64(i+1) || message.Revision != before[i].Revision || message.Text != before[i].Text || !storageArchiveJSONEqual(t, message.Data, before[i].Data) {
					t.Fatalf("message %d changed across archive", i)
				}
				seen[message.ID] = true
			}
			var canonical, logical int
			if err := admin.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM conversation_messages WHERE organization_id=$1 AND conversation_id=$2),message_count FROM conversations WHERE organization_id=$1 AND id=$2`, f.project.OrganizationID, record.ID).Scan(&canonical, &logical); err != nil {
				t.Fatal(err)
			}
			if canonical != 600 || logical != 600 {
				t.Fatalf("canonical=%d logical=%d", canonical, logical)
			}
			var pointer attachmentstore.StorageArchivePointer
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				pointer, err = r.(attachmentstore.StorageArchiveReader).StorageArchivePointer(t.Context(), f.project.ID, issue.WorkItemID)
				if err != nil {
					return err
				}
				current, err := r.WorkItem(t.Context(), f.project.ID, issue.WorkItemID)
				if err == nil && (pointer.Stamp.Revision != current.Revision || pointer.Stamp.EventSequence != current.EventSequence) {
					t.Fatal("archive pointer does not reflect the current committed issue")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var pending int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM issue_archive_sections WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND kind='conversation_messages' AND NOT retirement_complete`, f.project.OrganizationID, f.project.ID, issue.WorkItemID).Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if pending != 0 {
				t.Fatalf("pending payload sections=%d", pending)
			}
			if !strings.HasPrefix(phase, "reopened") && len(f.archiveObjects(t)) != 4 {
				t.Fatalf("interruption duplicated bounded archive pages: objects=%d want=4", len(f.archiveObjects(t)))
			}
			if phase == "message children" {
				var attachmentReference attachment.ArchiveReference
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					attachments, err := r.(conversationstore.AttachmentReader).MessageAttachments(t.Context(), f.project.ID, record.ID, []string{before[0].ID})
					if err != nil {
						return err
					}
					if len(attachments[before[0].ID]) != 1 || attachments[before[0].ID][0].Name != "example.txt" {
						t.Fatal("message attachment was lost")
					}
					content, err := r.ConversationContent(t.Context(), attachments[before[0].ID][0].ContentID)
					if err != nil {
						return err
					}
					attachmentReference = content.Reference
					refs, err := r.MessageReferences(t.Context(), record.ID, before[0].ID)
					if err != nil {
						return err
					}
					if len(refs) != 1 || refs[0].TargetID != string(issue.WorkItemID) {
						t.Fatal("message reference was lost")
					}
					revisions, err := r.ToolRevisions(t.Context(), record.ID, before[1].ID, 10)
					if err != nil {
						return err
					}
					if len(revisions) != 1 || revisions[0].Text != "original tool history" {
						t.Fatal("tool revision was lost")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				raw, err := f.bodies.ReadContent(t.Context(), string(f.project.OrganizationID), attachmentReference)
				if err != nil || string(raw) != "example attachment" {
					t.Fatalf("attachment content=%q error=%v", raw, err)
				}
			}
			objects := len(f.archiveObjects(t))
			f.archive(t)
			f.archive(t)
			if len(f.archiveObjects(t)) != objects {
				t.Fatal("idempotent continuation created new issue archive objects")
			}
		})
	}
}

func TestNativeStorageArchiveCommentRevisions(t *testing.T) {
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(at.Add(-73 * time.Hour).UnixNano())
	service := newClockedPostgresNativeService(t, Config{now: func() time.Time { return time.Unix(0, clock.Load()).UTC() }})
	f := newNativeFixture(t, service, "", "comment history")
	issue := f.create(t, "comment history")
	path := f.base + "/work-items/" + string(issue.WorkItemID) + "/comments"
	response := performHubAPIRequest(t, service, http.MethodPost, path, f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment-revision-1"}, Body: "comment revision 1"})
	requireNativeStatus(t, response, http.StatusOK)
	var comment tracker.NativeComment
	decodeHubResponse(t, response, &comment)
	if comment.Revision != 1 || comment.Body != "comment revision 1" {
		t.Fatalf("initial comment=%+v", comment)
	}
	for revision := 2; revision <= 40; revision++ {
		body := "comment revision " + strconv.Itoa(revision)
		response = performHubAPIRequest(t, service, http.MethodPatch, path+"/"+comment.ID, f.token, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment-revision-" + strconv.Itoa(revision)}, ExpectedRevision: comment.Revision, Body: body})
		requireNativeStatus(t, response, http.StatusOK)
		previousID := comment.ID
		decodeHubResponse(t, response, &comment)
		if comment.ID != previousID || comment.Revision != tracker.Revision(revision) || comment.Body != body {
			t.Fatalf("edited comment revision %d=%+v", revision, comment)
		}
	}
	issue = readWorkItem(t, f, issue.WorkItemID, "")
	response = performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "close-comment-history"}, ExpectedRevision: issue.Revision, State: "Done", Reason: "user_requested"})
	requireNativeStatus(t, response, http.StatusOK)
	clock.Store(at.UnixNano())
	admin := nativeAttemptTestDatabase(t, f)
	schemaSnapshot := func() []string {
		t.Helper()
		rows, err := admin.QueryContext(t.Context(), `
WITH n AS (SELECT oid,nspname FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname<>'information_schema'),
acl(kind,owner,item) AS (
 SELECT 'relation',c.oid,a::text FROM pg_class c, unnest(c.relacl) a
 UNION ALL SELECT 'routine',p.oid,a::text FROM pg_proc p, unnest(p.proacl) a
 UNION ALL SELECT 'schema',s.oid,a::text FROM pg_namespace s, unnest(s.nspacl) a
),
grants AS (SELECT kind,owner,string_agg(item,',' ORDER BY item) AS items FROM acl GROUP BY kind,owner)
SELECT entry FROM (
 SELECT format('schema %s owner=%s acl=%s',n.nspname,pg_get_userbyid(s.nspowner),g.items) AS entry
  FROM n JOIN pg_namespace s ON s.oid=n.oid LEFT JOIN grants g ON g.kind='schema' AND g.owner=s.oid
 UNION ALL SELECT format('relation %s.%s kind=%s owner=%s rls=%s force=%s persistence=%s options=%s acl=%s view=%s',n.nspname,c.relname,c.relkind,pg_get_userbyid(c.relowner),c.relrowsecurity,c.relforcerowsecurity,c.relpersistence,c.reloptions,g.items,CASE WHEN c.relkind IN ('v','m') THEN pg_get_viewdef(c.oid) END)
  FROM pg_class c JOIN n ON n.oid=c.relnamespace LEFT JOIN grants g ON g.kind='relation' AND g.owner=c.oid WHERE c.relkind IN ('r','p','v','m','S','f')
 UNION ALL SELECT format('column %s.%s.%s position=%s type=%s notnull=%s default=%s identity=%s generated=%s collation=%s acl=%s',n.nspname,c.relname,a.attname,
   row_number() OVER (PARTITION BY c.oid ORDER BY a.attnum),format_type(a.atttypid,a.atttypmod),a.attnotnull,pg_get_expr(d.adbin,d.adrelid),a.attidentity,a.attgenerated,
   (SELECT collname FROM pg_collation WHERE oid=a.attcollation),(SELECT string_agg(i::text,',' ORDER BY i::text) FROM unnest(a.attacl) i))
  FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN n ON n.oid=c.relnamespace LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
  WHERE a.attnum>0 AND NOT a.attisdropped AND c.relkind IN ('r','p','v','m','f')
 UNION ALL SELECT format('constraint %s.%s.%s %s validated=%s',n.nspname,c.relname,o.conname,pg_get_constraintdef(o.oid),o.convalidated)
  FROM pg_constraint o JOIN pg_class c ON c.oid=o.conrelid JOIN n ON n.oid=c.relnamespace
 UNION ALL SELECT format('index %s.%s %s valid=%s ready=%s',n.nspname,i.relname,pg_get_indexdef(x.indexrelid),x.indisvalid,x.indisready)
  FROM pg_index x JOIN pg_class i ON i.oid=x.indexrelid JOIN n ON n.oid=i.relnamespace
 UNION ALL SELECT format('trigger %s.%s.%s %s enabled=%s',n.nspname,c.relname,t.tgname,pg_get_triggerdef(t.oid),t.tgenabled)
  FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN n ON n.oid=c.relnamespace WHERE NOT t.tgisinternal
 UNION ALL SELECT format('policy %s.%s.%s %s %s roles=%s using=%s check=%s',p.schemaname,p.tablename,p.policyname,p.permissive,p.cmd,(SELECT string_agg(r::text,',' ORDER BY r::text) FROM unnest(p.roles) r),p.qual,p.with_check)
  FROM pg_policies p JOIN n ON n.nspname=p.schemaname
 UNION ALL SELECT format('routine %s.%s(%s) owner=%s acl=%s %s',n.nspname,p.proname,pg_get_function_identity_arguments(p.oid),pg_get_userbyid(p.proowner),g.items,pg_get_functiondef(p.oid))
  FROM pg_proc p JOIN n ON n.oid=p.pronamespace LEFT JOIN grants g ON g.kind='routine' AND g.owner=p.oid WHERE p.prokind IN ('f','p')
 UNION ALL SELECT format('type %s.%s kind=%s base=%s labels=%s',n.nspname,y.typname,y.typtype,CASE WHEN y.typtype='d' THEN format_type(y.typbasetype,y.typtypmod) END,(SELECT string_agg(e.enumlabel,',' ORDER BY e.enumsortorder) FROM pg_enum e WHERE e.enumtypid=y.oid))
  FROM pg_type y JOIN n ON n.oid=y.typnamespace WHERE y.typtype IN ('e','d','r','m')
 UNION ALL SELECT format('sequence %s.%s type=%s start=%s min=%s max=%s increment=%s cycle=%s',q.schemaname,q.sequencename,q.data_type,q.start_value,q.min_value,q.max_value,q.increment_by,q.cycle)
  FROM pg_sequences q JOIN n ON n.nspname=q.schemaname
 UNION ALL SELECT format('rule %s.%s.%s %s',r.schemaname,r.tablename,r.rulename,r.definition) FROM pg_rules r JOIN n ON n.nspname=r.schemaname
 UNION ALL SELECT format('extension %s version=%s',extname,extversion) FROM pg_extension
 UNION ALL SELECT format('event trigger %s event=%s function=%s enabled=%s tags=%s',evtname,evtevent,evtfoid::regproc,evtenabled,evttags) FROM pg_event_trigger
 UNION ALL SELECT format('default acl role=%s schema=%s object=%s acl=%s',pg_get_userbyid(defaclrole),defaclnamespace::regnamespace,defaclobjtype,(SELECT string_agg(i::text,',' ORDER BY i::text) FROM unnest(defaclacl) i)) FROM pg_default_acl
) catalog ORDER BY entry COLLATE "C"`)
		if err != nil {
			t.Fatal(err)
		}
		var entries []string
		for rows.Next() {
			var entry string
			if err := rows.Scan(&entry); err != nil {
				t.Fatal(err)
			}
			entries = append(entries, entry)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			t.Fatal("catalog snapshot is empty")
		}
		return entries
	}

	beforeSchema := schemaSnapshot()
	for range 8 {
		if err := service.maintainNativeRetention(t.Context(), at); err != nil {
			t.Fatal(err)
		}
	}
	if after := schemaSnapshot(); !slices.Equal(after, beforeSchema) {
		t.Fatal("history maintenance changed PostgreSQL schema")
	}
	func() {
		config := postgresStartupTemplate.NativeConfig(t, admin, service.config.Postgres.Connection.User, service.config.Postgres.Connection.Password)
		connection, err := pgx.ConnectConfig(t.Context(), config.ConnConfig)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.ReadTimeout)
			defer cancel()
			if err := connection.Close(cleanup); err != nil {
				t.Error(err)
			}
		}()
		tx, err := connection.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.ReadTimeout)
			defer cancel()
			if err := tx.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Error(err)
			}
		}()
		if _, err := tx.Exec(t.Context(), "SELECT set_config('detent.organization_id', $1, true)", string(f.project.OrganizationID)); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(t.Context(), "DELETE FROM collaboration_versions WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND record_id=$4", f.project.OrganizationID, f.project.ID, issue.WorkItemID, comment.ID)
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "23514" || pgError.Message != "history is immutable" {
			t.Fatalf("ordinary revision deletion error=%v, want immutable history rejection", err)
		}
	}()
	keys := make([]eventstore.CollaborationVersionKey, 40)
	for index := range keys {
		keys[index] = eventstore.CollaborationVersionKey{RecordID: comment.ID, Revision: int64(index + 1)}
	}
	var versions []eventstore.CollaborationVersion
	if err := service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		versions, err = reader.(eventstore.CollaborationVersionsReader).CollaborationVersions(t.Context(), keys)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 40 {
		t.Fatalf("immutable history retained %d snapshots, want 40", len(versions))
	}
	seen := map[int64]bool{}
	for _, version := range versions {
		if version.OrganizationID != f.project.OrganizationID || version.ProjectID != f.project.ID || version.WorkItemID != issue.WorkItemID || version.RecordID != comment.ID || version.Revision < 1 || version.Revision > 40 || seen[version.Revision] {
			t.Fatalf("wrong immutable version=%+v", version)
		}
		raw, err := eventstore.NewContent(service.runDetailStore()).CollaborationVersion(t.Context(), version)
		if err != nil {
			t.Fatal(err)
		}
		var got tracker.NativeComment
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got.ID != comment.ID || int64(got.Revision) != version.Revision || got.Body != "comment revision "+strconv.FormatInt(version.Revision, 10) {
			t.Fatalf("revision %d comment=%+v", version.Revision, got)
		}
		seen[version.Revision] = true
	}
	for _, revision := range []int64{1, 15, 31, 32, 40} {
		response := performHubAPIRequest(t, service, http.MethodGet, path+"/"+comment.ID+"/versions/"+strconv.FormatInt(revision, 10), f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var got tracker.NativeComment
		decodeHubResponse(t, response, &got)
		if int64(got.Revision) != revision || got.Body != "comment revision "+strconv.FormatInt(revision, 10) {
			t.Fatalf("revision %d body=%q", revision, got.Body)
		}
	}
	response = performHubAPIRequest(t, service, http.MethodPatch, path+"/"+comment.ID, f.token, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment-revision-41"}, ExpectedRevision: comment.Revision, Body: "late comment revision"})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &comment)
	if comment.Revision != 41 || comment.Body != "late comment revision" {
		t.Fatalf("late edit=%+v", comment)
	}
	response = performHubAPIRequest(t, service, http.MethodGet, path+"/"+comment.ID+"/versions/41", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var latest tracker.NativeComment
	decodeHubResponse(t, response, &latest)
	if latest.Revision != 41 || latest.Body != "late comment revision" {
		t.Fatal("late edit lost its live reconstruction basis")
	}
}

func TestNativeStorageArchivePrivacyAndOrganizations(t *testing.T) {
	f := newPostgresStorageArchiveFixture(t)
	issue := f.closed(t)
	shared := f.messages(t, issue.WorkItemID, 2, conversation.VisibilityShared)
	private := f.messages(t, issue.WorkItemID, 1, conversation.VisibilityPrivate)
	admin := nativeAttemptTestDatabase(t, f.nativeFixture)
	if _, err := admin.ExecContext(t.Context(), `UPDATE conversation_messages SET text='private conversation content' WHERE organization_id=$1 AND conversation_id=$2`, f.project.OrganizationID, private.ID); err != nil {
		t.Fatal(err)
	}
	hidden := newNativeFixture(t, f.service, f.project.OrganizationID, "private dependency project")
	secret := hidden.create(t, "private dependency title")
	f.write(t, func(w tenantstore.Writer) error {
		if err := w.LockRelationshipGraph(t.Context()); err != nil {
			return err
		}
		return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: issue.WorkItemID, RelatedProjectID: hidden.project.ID, RelatedWorkItemID: secret.WorkItemID, Type: "dependency"}, "add", f.now())
	})
	second := &postgresStorageArchiveFixture{nativeFixture: newNativeFixture(t, f.service, "org_first_second", "second archive example"), clock: f.clock, bodies: f.bodies}
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), second.project.OrganizationID, apikey.HashToken(second.token), "", "")
	if err != nil {
		t.Fatalf("second credential status=%d error=%v", status, err)
	}
	second.scope = hubscope.NativeScope{Organization: second.project.OrganizationID, Project: second.project.ID, Credential: credential}
	otherIssue := second.closed(t)
	otherConversation := second.messages(t, otherIssue.WorkItemID, 2, conversation.VisibilityShared)
	f.drain(t, shared.ID)
	second.drain(t, otherConversation.ID)
	if f.inline(t, private.ID) != 1 {
		t.Fatal("private payload was retired")
	}
	organizations := map[tracker.OrganizationID]bool{}
	for key, document := range f.archiveObjects(t) {
		organizations[document.OrganizationID] = true
		if !strings.HasPrefix(key, "orgs/"+string(document.OrganizationID)+"/") {
			t.Fatal("archive key crossed organization scope")
		}
		raw, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []string{string(secret.WorkItemID), secret.Title, string(hidden.project.ID), private.ID, "private conversation content"} {
			if bytes.Contains(raw, []byte(value)) {
				t.Fatalf("public archive leaked %q", value)
			}
		}
	}
	if len(organizations) != 2 {
		t.Fatalf("published organizations=%d", len(organizations))
	}
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.(attachmentstore.StorageArchiveReader).StorageArchivePointer(t.Context(), second.project.ID, otherIssue.WorkItemID)
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("cross-organization pointer error=%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var section attachmentstore.StorageArchiveSection
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		section, err = r.(attachmentstore.StorageArchiveReader).StorageArchiveSection(t.Context(), attachmentstore.StorageArchiveSectionQuery{ProjectID: f.project.ID, WorkItemID: issue.WorkItemID, Kind: "conversation_messages", Subject: shared.ID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.write(t, func(w tenantstore.Writer) error {
		record, err := w.Conversation(t.Context(), f.project.ID, shared.ID)
		if err != nil {
			return err
		}
		record.Visibility = conversation.VisibilityPrivate
		_, err = w.UpdateConversation(t.Context(), record, record.Revision)
		return err
	})
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.(attachmentstore.ArchiveReader).AttachmentArchive(t.Context(), attachmentstore.ArchiveQuery{ProjectID: f.project.ID, WorkItemID: issue.WorkItemID, Digest: section.Archive.Reference.SHA256, Section: section.ID, Revision: section.Stamp.Revision})
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("private section access error=%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeStorageArchiveRangesAndLateMessages(t *testing.T) {
	f := newPostgresStorageArchiveFixture(t)
	issue := f.closed(t)
	record := f.messages(t, issue.WorkItemID, 501, conversation.VisibilityShared)
	admin := nativeAttemptTestDatabase(t, f.nativeFixture)
	large := strings.Repeat("payload", 35000)
	if _, err := admin.ExecContext(t.Context(), `UPDATE conversation_messages SET text=$3,text_bytes=length($3),stored_text_bytes=length($3) WHERE organization_id=$1 AND conversation_id=$2 AND seq=1`, f.project.OrganizationID, record.ID, large); err != nil {
		t.Fatal(err)
	}
	f.archive(t)
	if got := f.inline(t, record.ID); got != 1 {
		t.Fatalf("bounded first conversion left=%d want=1", got)
	}
	var late conversationstore.Message
	f.write(t, func(w tenantstore.Writer) error {
		var err error
		late, err = w.CreateMessage(t.Context(), conversationstore.Message{OrganizationID: f.project.OrganizationID, ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: "late message", Data: json.RawMessage(`{"opaque":{"late":true}}`), Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: "example-runner"}, CommandKey: "late_archive_message", CreatedAt: f.now(), UpdatedAt: f.now()})
		return err
	})
	if late.Sequence != 502 {
		t.Fatalf("late sequence=%d want=502", late.Sequence)
	}
	if err := f.service.retireStorageArchiveSections(t.Context(), f.project.OrganizationID, f.now()); err != nil {
		t.Fatal(err)
	}
	var untouched string
	if err := admin.QueryRowContext(t.Context(), `SELECT text FROM conversation_messages WHERE organization_id=$1 AND conversation_id=$2 AND id=$3`, f.project.OrganizationID, record.ID, late.ID).Scan(&untouched); err != nil {
		t.Fatal(err)
	}
	if untouched != "late message" {
		t.Fatal("old section retired an unarchived late payload")
	}
	f.drain(t, record.ID)
	sections := 0
	for _, document := range f.archiveObjects(t) {
		if document.Kind == "conversation_messages" {
			sections++
		}
	}
	if sections < 2 {
		t.Fatalf("large history uses %d sections", sections)
	}
	messages := f.readMessages(t, record.ID)
	if len(messages) != 502 || messages[0].Text != large || messages[501].ID != late.ID || messages[501].Text != "late message" {
		t.Fatal("paged range traversal lost old or late content")
	}
	unique := map[string]bool{}
	for i, message := range messages {
		if message.Seq != int64(i+1) || unique[message.ID] {
			t.Fatalf("duplicated or missing ranged message at %d", i)
		}
		unique[message.ID] = true
	}
	f.write(t, func(w tenantstore.Writer) error {
		page, err := w.FilteredMessages(t.Context(), conversationstore.MessageFilter{ConversationID: record.ID, Role: conversation.RoleAssistant, Limit: 1})
		if err != nil {
			return err
		}
		if len(page.Items) != 1 || page.Items[0].ID != late.ID {
			t.Fatal("filtered archived metadata is unavailable")
		}
		message, found, err := w.MessageByCommandKey(t.Context(), record.ID, "late_archive_message")
		if err != nil {
			return err
		}
		if !found || message.ID != late.ID {
			t.Fatal("archived message idempotency identity was lost")
		}
		message.Text = "late edit"
		message.TextBytes, message.StoredTextBytes = int64(len(message.Text)), int64(len(message.Text))
		message.Data = json.RawMessage(`{"opaque":{"edited":true}}`)
		message.ContentReference = ""
		message, err = w.UpdateMessage(t.Context(), message, message.Revision)
		if err != nil {
			return err
		}
		if message.Revision != 2 {
			t.Fatalf("late edit revision=%d", message.Revision)
		}
		return nil
	})
	f.drain(t, record.ID)
	messages = f.readMessages(t, record.ID)
	if len(messages) != 502 || messages[501].Text != "late edit" || messages[501].Revision != 2 {
		t.Fatal("late mutation did not survive re-archival")
	}
	var logical, ranges int
	if err := admin.QueryRowContext(t.Context(), `SELECT message_count FROM conversations WHERE organization_id=$1 AND id=$2`, f.project.OrganizationID, record.ID).Scan(&logical); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM issue_archive_sections WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND kind='conversation_messages'`, f.project.OrganizationID, f.project.ID, issue.WorkItemID).Scan(&ranges); err != nil {
		t.Fatal(err)
	}
	if logical != 502 || ranges < 2 {
		t.Fatalf("logical messages=%d published ranges=%d", logical, ranges)
	}
}

func TestNativeStorageArchiveTerminalClock(t *testing.T) {
	f := newPostgresStorageArchiveFixture(t)
	at := f.now()
	issue := f.create(t, "Terminal age")
	f.transition(t, issue.WorkItemID, "Done")
	clock := func() *time.Time {
		t.Helper()
		var result *time.Time
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			item, err := r.WorkItem(t.Context(), f.project.ID, issue.WorkItemID)
			result = item.TerminalEnteredAt
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if got := clock(); got == nil || !got.Equal(at) {
		t.Fatalf("initial terminal clock=%v", got)
	}
	f.clock.Add(int64(time.Hour))
	f.transition(t, issue.WorkItemID, "Todo")
	if clock() != nil {
		t.Fatal("reopen retained terminal age")
	}
	f.transition(t, issue.WorkItemID, "Done")
	reclosed := f.now()
	record := f.messages(t, issue.WorkItemID, 1, conversation.VisibilityShared)
	f.archive(t)
	if f.inline(t, record.ID) != 1 {
		t.Fatal("fresh terminal streak archived too early")
	}
	f.clock.Add(int64(73 * time.Hour))
	f.archive(t)
	if f.inline(t, record.ID) != 0 {
		t.Fatal("aged terminal payload was not archived")
	}
	pointer := func() attachmentstore.StorageArchivePointer {
		t.Helper()
		var result attachmentstore.StorageArchivePointer
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			result, err = r.(attachmentstore.StorageArchiveReader).StorageArchivePointer(t.Context(), f.project.ID, issue.WorkItemID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return result
	}
	previous := pointer()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "late_archive_comment"}, Body: "late comment"})
	requireNativeStatus(t, response, http.StatusOK)
	if got := clock(); got == nil || !got.Equal(reclosed) {
		t.Fatalf("late comment reset terminal streak=%v", got)
	}
	f.archive(t)
	if pointer().Root.Reference == previous.Root.Reference {
		t.Fatal("late comment did not publish a new root")
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID)+"/comments", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var comments tracker.Page[tracker.NativeComment]
	decodeHubResponse(t, response, &comments)
	if len(comments.Items) != 1 || comments.Items[0].Body != "late comment" {
		t.Fatal("comments lost after payload archival")
	}
}

func TestNativeStorageArchiveSafety(t *testing.T) {
	for _, test := range []string{"unknown terminal time", "active lease", "pending delivery", "pending question", "unknown execution", "unknown execution lease", "typed active execution"} {
		t.Run(test, func(t *testing.T) {
			f := newPostgresStorageArchiveFixture(t)
			issue := f.closed(t)
			record := f.messages(t, issue.WorkItemID, 1, conversation.VisibilityShared)
			admin := nativeAttemptTestDatabase(t, f.nativeFixture)
			var err error
			switch test {
			case "unknown terminal time":
				_, err = admin.ExecContext(t.Context(), `UPDATE issues SET terminal_entered_at=NULL WHERE organization_id=$1 AND native_id=$2`, f.project.OrganizationID, issue.WorkItemID)
			case "pending delivery":
				_, err = admin.ExecContext(t.Context(), `UPDATE conversation_messages SET delivery='queued' WHERE organization_id=$1 AND conversation_id=$2`, f.project.OrganizationID, record.ID)
			case "pending question":
				_, err = admin.ExecContext(t.Context(), `INSERT INTO conversation_questions (organization_id,conversation_id,id,status,created_at,updated_at) VALUES ($1,$2,'question_archive','pending',$3,$3)`, f.project.OrganizationID, record.ID, f.now())
			case "unknown execution":
				_, err = admin.ExecContext(t.Context(), `UPDATE conversations SET execution_status=NULL WHERE organization_id=$1 AND id=$2`, f.project.OrganizationID, record.ID)
			case "unknown execution lease":
				_, err = admin.ExecContext(t.Context(), `UPDATE conversations SET execution_lease_id=NULL WHERE organization_id=$1 AND id=$2`, f.project.OrganizationID, record.ID)
			case "typed active execution":
				_, err = admin.ExecContext(t.Context(), `UPDATE conversations SET execution_status='running' WHERE organization_id=$1 AND id=$2`, f.project.OrganizationID, record.ID)
			case "active lease":
				_, err = admin.ExecContext(t.Context(), `INSERT INTO machines (organization_id,id,hostname,display_name,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES ($1,'machine_archive','example','example',1,'test',$2,$2,$2)`, f.project.OrganizationID, f.now())
				if err == nil {
					_, err = admin.ExecContext(t.Context(), `INSERT INTO leases (organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at) SELECT organization_id,'lease_archive',id,'machine_archive','session_archive',revision,$3,$3,$4 FROM issues WHERE organization_id=$1 AND native_id=$2`, f.project.OrganizationID, issue.WorkItemID, f.now(), f.now().Add(time.Hour))
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			f.archive(t)
			if len(f.archiveObjects(t)) != 0 || f.inline(t, record.ID) != 1 {
				t.Fatal("unsafe work published or retired payloads")
			}
		})
	}
}

type storageArchiveDetailObserver struct {
	attachment.DetailStore
	reads atomic.Int64
	check func() error
}

func (s *storageArchiveDetailObserver) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	s.reads.Add(1)
	return s.DetailStore.ReadDetail(ctx, organization, ref)
}
func (s *storageArchiveDetailObserver) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	if err := s.check(); err != nil {
		return attachment.ArchiveReference{}, err
	}
	return s.DetailStore.StoreDetail(ctx, organization, raw)
}

func TestNativeStorageArchiveRunTimeline(t *testing.T) {
	a := newTerminalAttemptFixture(t)
	if err := a.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if a.service.outbox != nil {
		a.service.outbox.stop()
	}
	a.snapshots(t, 2)
	a.finish(t)
	issue := readWorkItem(t, a.nativeFixture, a.issue.WorkItemID, "")
	response := performHubAPIRequest(t, a.service, http.MethodPost, a.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", a.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "close_archived_run"}, ExpectedRevision: issue.Revision, State: "Done", Reason: "user_requested"})
	requireNativeStatus(t, response, http.StatusOK)
	a.advance(74 * time.Hour)
	a.service.tokenUseWork.Wait()
	var checking atomic.Bool
	check := func() error {
		if checking.Load() {
			if n := a.service.config.TenantRepository.(*postgres.Repository).Stats().AcquiredConns(); n != 0 {
				return fmt.Errorf("archive I/O holds %d PostgreSQL connections", n)
			}
		}
		return nil
	}
	observer := &storageArchiveDetailObserver{DetailStore: a.service.runDetailStore(), check: check}
	a.service.config.RunDetails = observer
	var mu sync.Mutex
	objects := map[string][]byte{}
	previousContent := a.service.config.Content
	a.service.config.Content = func(ctx context.Context, r entryauthority.ContentRequest) ([]byte, error) {
		if err := check(); err != nil {
			return nil, err
		}
		mu.Lock()
		if r.Write {
			objects[r.Reference.Key] = bytes.Clone(r.Data)
		}
		data, ok := objects[r.Reference.Key]
		mu.Unlock()
		if ok {
			return bytes.Clone(data), nil
		}
		if previousContent == nil {
			return nil, errors.New("object missing")
		}
		return previousContent(ctx, r)
	}
	paths := []string{a.base + "/work-items/" + string(issue.WorkItemID) + "/runtime?native_attempt_id=" + a.start.Data.AttemptID, a.base + "/work-items/" + string(issue.WorkItemID) + "/history?limit=200"}
	read := func(index int, path string) []byte {
		t.Helper()
		var items []tracker.CollaborationEvent
		cursor := ""
		for {
			pagePath := path
			if cursor != "" {
				pagePath += "&cursor=" + url.QueryEscape(cursor)
			}
			response := performHubAPIRequest(t, a.service, http.MethodGet, pagePath, a.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			if index == 0 {
				return bytes.Clone(response.Body.Bytes())
			}
			var page tracker.Page[tracker.CollaborationEvent]
			decodeHubResponse(t, response, &page)
			items = append(items, page.Items...)
			if page.NextCursor == "" {
				raw, err := json.Marshal(tracker.Page[tracker.CollaborationEvent]{Items: items})
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			if page.NextCursor == cursor {
				t.Fatal("history cursor did not advance")
			}
			cursor = page.NextCursor
		}
	}
	before := make([][]byte, len(paths))
	for i, path := range paths {
		before[i] = read(i, path)
	}
	a.service.tokenUseWork.Wait()
	checking.Store(true)
	if err := a.service.maintainNativeRetention(t.Context(), a.service.config.now()); err != nil {
		t.Fatal(err)
	}
	observer.reads.Store(0)
	for i, path := range paths {
		assertArchiveGolden(t, before[i], read(i, path))
	}
	if observer.reads.Load() == 0 {
		t.Fatal("runtime and history did not hydrate canonical archived payloads")
	}
	admin := nativeAttemptTestDatabase(t, a.nativeFixture)
	var checkpoints int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_attempt_events WHERE organization_id=$1 AND attempt_id=$2`, a.project.OrganizationID, a.start.Data.AttemptID).Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if checkpoints == 0 {
		t.Fatal("replay identities were retired")
	}
	roots := 0
	mu.Lock()
	for _, data := range objects {
		var root attachmentstore.StorageArchiveDocument
		if json.Unmarshal(archiveTestBytes(t, data), &root) != nil || root.SchemaVersion != 2 || root.Kind != "root" {
			continue
		}
		roots++
		raw, err := json.Marshal(root.Timeline)
		if err != nil {
			t.Fatal(err)
		}
		for _, fact := range []string{"run.started", "run.finished", "succeeded", a.start.Data.AttemptID, "actor"} {
			if !bytes.Contains(raw, []byte(fact)) {
				t.Fatalf("root factual timeline lost %s", fact)
			}
		}
	}
	mu.Unlock()
	if roots != 1 {
		t.Fatalf("published run roots=%d", roots)
	}
	a.service.config.ArchiveReader = func(context.Context, attachment.ArchiveReference, bool) (entryauthority.ArchiveResponse, error) {
		return entryauthority.ArchiveResponse{}, errors.New("optional issue archive unavailable")
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, paths[1], nil)
	request.Header.Set("Authorization", "Bearer "+a.token)
	request.Header.Set("X-Detent-Archive-Read", "1")
	result := httptest.NewRecorder()
	a.service.Handler().ServeHTTP(result, request)
	requireNativeStatus(t, result, http.StatusOK)
	if !strings.Contains(result.Body.String(), `"type":"run.finished"`) || !strings.Contains(result.Body.String(), `"outcome":"succeeded"`) {
		t.Fatal("canonical history lost factual run events")
	}
	checking.Store(false)
}

func TestNativeStorageArchiveScanResumesAcrossHubInstances(t *testing.T) {
	for _, canceled := range []bool{true, false} {
		t.Run(fmt.Sprintf("canceled=%v", canceled), func(t *testing.T) {
			expectedError := errors.New("archive publication rejected")
			if canceled {
				expectedError = context.Canceled
			}
			f := newPostgresStorageArchiveFixture(t)
			items := make([]tracker.NativeWorkItemID, 0, 6)
			for range 6 {
				issue := f.closed(t)
				f.messages(t, issue.WorkItemID, 1, conversation.VisibilityShared)
				items = append(items, issue.WorkItemID)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			uploaded := 0
			f.hook = func(request entryauthority.ContentRequest) error {
				if !request.Write || request.Reference.SchemaVersion != 2 {
					return nil
				}
				var document attachmentstore.StorageArchiveDocument
				if err := json.Unmarshal(archiveTestBytes(t, request.Data), &document); err != nil {
					return err
				}
				if document.Kind == "root" {
					uploaded++
					if uploaded == 2 {
						if canceled {
							cancel()
						} else {
							return expectedError
						}
					}
				}
				return nil
			}
			if err := f.service.archiveClosedNativeIssuesTenant(ctx, f.project.OrganizationID, f.now()); !errors.Is(err, expectedError) {
				t.Fatalf("interrupted archive error=%v", err)
			}
			admin := nativeAttemptTestDatabase(t, f.nativeFixture)
			var committed int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM issue_storage_archives WHERE organization_id=$1 AND project_id=$2`, f.project.OrganizationID, f.project.ID).Scan(&committed); err != nil {
				t.Fatal(err)
			}
			wantCommitted := 1
			if !canceled {
				wantCommitted = 3
			}
			if committed != wantCommitted {
				t.Fatalf("committed before restart=%d", committed)
			}
			var firstResumed tracker.NativeWorkItemID
			f.hook = func(request entryauthority.ContentRequest) error {
				if !request.Write || request.Reference.SchemaVersion != 2 {
					return nil
				}
				var document attachmentstore.StorageArchiveDocument
				if err := json.Unmarshal(archiveTestBytes(t, request.Data), &document); err != nil {
					return err
				}
				if document.Kind == "root" && document.WorkItemID == items[0] {
					t.Fatal("repeated committed unit")
				}
				if document.Kind == "root" && firstResumed == "" {
					firstResumed = document.WorkItemID
				}
				return nil
			}
			restarted := &Service{config: f.service.config, database: f.service.database, conversations: f.service.conversations}
			if err := restarted.archiveClosedNativeIssuesTenant(t.Context(), f.project.OrganizationID, f.now()); err != nil {
				t.Fatal(err)
			}
			wantResumed := items[1]
			if !canceled {
				wantResumed = items[4]
			}
			if firstResumed != wantResumed {
				t.Fatalf("restart resumed %s want %s", firstResumed, wantResumed)
			}
			for range 3 {
				if err := restarted.archiveClosedNativeIssuesTenant(t.Context(), f.project.OrganizationID, f.now()); err != nil {
					t.Fatal(err)
				}
			}
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM issue_storage_archives WHERE organization_id=$1 AND project_id=$2`, f.project.OrganizationID, f.project.ID).Scan(&committed); err != nil {
				t.Fatal(err)
			}
			if committed != 6 {
				t.Fatalf("restart left committed=%d want=6", committed)
			}
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_first_second", corestore.Background, func(r tenantstore.Reader) error {
				cursor, err := r.(attachmentstore.StorageArchiveReader).StorageArchiveScanCursor(t.Context())
				if err == nil && cursor.ID != 0 {
					t.Fatal("scan position crossed tenant scope")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}

}
