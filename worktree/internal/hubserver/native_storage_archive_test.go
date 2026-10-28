package hubserver

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type storageArchiveFixture struct {
	conversationFixture
	objects map[string][]byte
	now     time.Time
	hook    func(entryauthority.ContentRequest) error
}

func newStorageArchiveFixture(t *testing.T) *storageArchiveFixture {
	t.Helper()
	f := &storageArchiveFixture{conversationFixture: newConversationFixture(t), objects: map[string][]byte{}, now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	f.service.conversations = &conversationService{server: f.service, store: f.store, broker: newNotificationBroker()}
	t.Cleanup(func() { f.service.conversations = nil })
	f.service.config.now = func() time.Time { return f.now }
	f.service.database.now = f.service.config.now
	f.service.config.content = func(_ context.Context, r entryauthority.ContentRequest) ([]byte, error) {
		if r.Write {
			f.objects[r.Reference.Key] = slices.Clone(r.Data)
		}
		if f.hook != nil {
			if err := f.hook(r); err != nil {
				return nil, err
			}
		}
		data, ok := f.objects[r.Reference.Key]
		if !ok {
			return nil, errors.New("object missing")
		}
		return slices.Clone(data), nil
	}
	f.service.config.ArchiveReader = func(_ context.Context, ref attachment.ArchiveReference, _ bool) (entryauthority.ArchiveResponse, error) {
		data, ok := f.objects[ref.Key]
		if !ok {
			return entryauthority.ArchiveResponse{}, errors.New("object missing")
		}
		return entryauthority.ArchiveResponse{Data: slices.Clone(data)}, nil
	}
	return f
}

func (f *storageArchiveFixture) transition(t *testing.T, item, state string, at time.Time) tracker.NativeIssue {
	t.Helper()
	tx := f.tx(t)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	issue, _, err := readNativeIssue(t.Context(), tx, scope, item)
	if err != nil {
		t.Fatal(err)
	}
	from := issue.State
	issue.State = state
	issue, err = persistNativeIssue(t.Context(), tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: from, ToState: state}, at)
	if err != nil {
		t.Fatal(err)
	}
	f.commit(t, tx)
	return issue
}

func (f *storageArchiveFixture) messages(t *testing.T, item string, count int, visibility conversation.Visibility) conversationRecord {
	t.Helper()
	record := f.record(f.owner, "archive conversation", f.now.Add(-73*time.Hour))
	record.Visibility = visibility
	if visibility == conversation.VisibilityShared {
		record.WorkItemID = item
		record.LinkedAt = &record.CreatedAt
	} else {
		record.SubjectWorkItemID = item
	}
	record = f.create(t, record)
	tx := f.tx(t)
	for index := range count {
		message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: "retained message " + strings.Repeat("x", index%7), Delivery: conversation.DeliveryCompleted, CreatedAt: f.now, UpdatedAt: f.now, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: "example-runner"}}
		if err := f.store.appendMessage(t.Context(), tx, &message); err != nil {
			t.Fatal(err)
		}
	}
	f.commit(t, tx)
	return record
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

func TestNativeStorageArchivePublication(t *testing.T) {
	for _, phase := range []string{"sections uploaded", "root uploaded", "pointer commit", "pointer committed", "chunk retired", "reopened at sections", "reopened at root", "reopened at retirement", "message children"} {
		t.Run(phase, func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			issue := f.nativeFixture.create(t, "closed history")
			record := f.messages(t, string(issue.WorkItemID), 600, conversation.VisibilityShared)

			var childMessages []string
			if phase == "message children" {
				rows, err := f.service.database.reader.QueryContext(t.Context(), "SELECT id FROM conversation_messages WHERE conversation_id=? ORDER BY seq LIMIT 2", record.ID)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						t.Fatal(err)
					}
					childMessages = append(childMessages, id)
				}
				if err := errors.Join(rows.Err(), rows.Close()); err != nil {
					t.Fatal(err)
				}
				if len(childMessages) != 2 {
					t.Fatal("missing child messages")
				}
				tx := f.tx(t)
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO message_references(message_id,conversation_id,target_kind,target_id,label,created_at) VALUES(?,?,'issue',?,'retained reference',?)`, childMessages[0], record.ID, issue.WorkItemID, conversationTime(f.now)); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO conversation_attachments(id,conversation_id,message_id,principal_id,name,mime,size,artifact_ref,created_at) VALUES(?,?,?,?, 'history.txt','text/plain',7,?,?)`, "attachment-"+record.ID, record.ID, childMessages[1], f.owner, "blob-"+record.ID, conversationTime(f.now)); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO conversation_attachment_blobs(artifact_ref,content) VALUES(?,?)`, "blob-"+record.ID, []byte("history")); err != nil {
					t.Fatal(err)
				}
				f.commit(t, tx)
			}
			issue = f.transition(t, string(issue.WorkItemID), "Done", f.now.Add(-73*time.Hour))
			stopped := false
			f.hook = func(r entryauthority.ContentRequest) error {
				root := strings.HasSuffix(r.Reference.Key, "/issue.json.gz")
				match := r.Write && !root && (phase == "sections uploaded" || phase == "reopened at sections") || r.Write && root && (phase == "root uploaded" || phase == "reopened at root") || !r.Write && (phase == "pointer committed" || phase == "reopened at retirement")
				if stopped || !match {
					return nil
				}
				stopped = true
				if strings.HasPrefix(phase, "reopened") {
					f.transition(t, string(issue.WorkItemID), "Todo", f.now)
					return nil
				}
				return errors.New("injected interruption")
			}
			if phase == "pointer commit" {
				if _, err := f.service.database.db.ExecContext(t.Context(), `CREATE TRIGGER archive_test_interrupt BEFORE INSERT ON issue_storage_archives BEGIN SELECT RAISE(ABORT,'injected interruption'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err := f.service.archiveClosedNativeIssues(t.Context(), f.now)
			wantError := phase == "sections uploaded" || phase == "root uploaded" || phase == "pointer commit" || phase == "pointer committed"
			if (err != nil) != wantError {
				t.Fatalf("archive error=%v want error=%v", err, wantError)
			}
			var remaining, pointers int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_messages WHERE conversation_id=?", record.ID).Scan(&remaining); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issue_storage_archives WHERE work_item_id=?", string(issue.WorkItemID)).Scan(&pointers); err != nil {
				t.Fatal(err)
			}
			wantRemaining := 600
			if phase == "chunk retired" || phase == "message children" {
				wantRemaining = 100 + len(childMessages)
			}
			if remaining != wantRemaining {
				t.Fatalf("remaining=%d want=%d", remaining, wantRemaining)
			}
			wantPointer := 0
			if phase == "message children" || phase == "chunk retired" || phase == "pointer committed" || phase == "reopened at retirement" {
				wantPointer = 1
			}
			if pointers != wantPointer {
				t.Fatalf("pointers=%d want=%d", pointers, wantPointer)
			}
			if pointers == 1 {
				var revision int64
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT issue_revision FROM issue_storage_archives WHERE work_item_id=?", issue.WorkItemID).Scan(&revision); err != nil {
					t.Fatal(err)
				}
				if revision != int64(issue.Revision) {
					t.Fatal("pointer does not reflect the last committed revision")
				}
			}
			if strings.HasPrefix(phase, "reopened") {
				var entered *string
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT terminal_entered_at FROM issues WHERE native_id=?", string(issue.WorkItemID)).Scan(&entered); err != nil {
					t.Fatal(err)
				}
				if entered != nil {
					t.Fatal("reopen retained terminal time")
				}
				if err := f.service.archiveClosedNativeIssues(t.Context(), f.now); err != nil {
					t.Fatal(err)
				}
				return
			}
			f.hook = nil
			if phase == "pointer commit" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER archive_test_interrupt"); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := f.service.archiveClosedNativeIssues(t.Context(), f.now); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_messages WHERE conversation_id=?", record.ID).Scan(&remaining); err != nil {
				t.Fatal(err)
			}
			if remaining != len(childMessages) {
				t.Fatalf("resume left %d messages, want %d", remaining, len(childMessages))
			}
			if len(childMessages) > 0 {
				messages := []*conversationMessageRecord{{ID: childMessages[0]}, {ID: childMessages[1]}}
				if err := resolveMessageReferences(t.Context(), f.service.database.reader, messages); err != nil {
					t.Fatal(err)
				}
				if err := resolveMessageAttachments(t.Context(), f.service.database.reader, messages); err != nil {
					t.Fatal(err)
				}
				if len(messages[0].References) != 1 || messages[0].References[0].Label != "retained reference" || len(messages[1].Attachments) != 1 {
					t.Fatalf("children not readable: %+v %+v", messages[0], messages[1])
				}
				content, err := f.store.readAttachmentContent(t.Context(), f.service.database.reader, "blob-"+record.ID)
				if err != nil || string(content) != "history" {
					t.Fatalf("attachment content=%q error=%v", content, err)
				}
				var pending int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_archive_sections WHERE work_item_id=? AND retirement_complete=0", issue.WorkItemID).Scan(&pending); err != nil {
					t.Fatal(err)
				}
				if pending != 0 {
					t.Fatalf("pending sections=%d", pending)
				}
			}
			if len(f.objects) != 2 {
				t.Fatalf("interruption duplicated archive objects: %d", len(f.objects))
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			ctx := f.service.archiveContext(t.Context(), scope, false)
			query, err := archiveQuery(ctx, f.service.database.reader, string(issue.WorkItemID), "conversation", record.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			var total, unique int
			if err := query.QueryRowContext(ctx, "SELECT count(*),count(DISTINCT id) FROM conversation_messages WHERE conversation_id=?", record.ID).Scan(&total, &unique); err != nil {
				t.Fatal(err)
			}
			if total != 600 || unique != 600 {
				t.Fatalf("history total=%d unique=%d", total, unique)
			}
			var revision int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT issue_revision FROM issue_storage_archives WHERE work_item_id=?", string(issue.WorkItemID)).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			if revision != int64(issue.Revision) {
				t.Fatalf("archive revision=%d want=%d", revision, issue.Revision)
			}
		})
	}
}

func TestNativeStorageArchivePrivacyAndOrganizations(t *testing.T) {
	f := newStorageArchiveFixture(t)
	other := newNativeFixture(t, f.service, "", "other-organization")
	hidden := newNativeFixture(t, f.service, f.project.OrganizationID, "hidden-project")
	secret := hidden.create(t, "hidden dependency content")
	issue := f.nativeFixture.create(t, "shared history")
	request := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "hidden-dependency"}, ExpectedRevision: issue.Revision, RelatedWorkItemID: secret.WorkItemID, Operation: "add"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/dependencies", testHubAdminToken, request), http.StatusOK)
	private := f.messages(t, string(issue.WorkItemID), 1, conversation.VisibilityPrivate)
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversation_messages SET text='private conversation content' WHERE conversation_id=?", private.ID); err != nil {
		t.Fatal(err)
	}
	f.transition(t, string(issue.WorkItemID), "Done", f.now.Add(-73*time.Hour))
	second := other.create(t, "second organization history")
	tx := f.tx(t)
	scope := nativeScope{organization: other.project.OrganizationID, project: other.project.ID}
	second.State = "Done"
	if _, err := persistNativeIssue(t.Context(), tx, scope, second, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "Done"}, f.now.Add(-73*time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.commit(t, tx)
	if err := f.service.archiveClosedNativeIssues(t.Context(), f.now); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issue_storage_archives").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("archives=%d want two organizations", count)
	}
	for key, data := range f.objects {
		raw := archiveTestBytes(t, data)
		for _, secret := range []string{string(secret.WorkItemID), secret.Title, private.ID, "private conversation content"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatalf("shared archive %s exposes %q", key, secret)
			}
		}
		var root storageArchiveRoot
		if err := json.Unmarshal(raw, &root); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(key, "orgs/"+root.Organization+"/") {
			t.Fatalf("cross-organization root %s", key)
		}
	}
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_messages WHERE conversation_id=?", private.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("private conversation retired")
	}
}

func TestNativeStorageArchiveRangesAndLateMessages(t *testing.T) {
	f := newStorageArchiveFixture(t)
	issue := f.nativeFixture.create(t, "ranged history")
	record := f.messages(t, string(issue.WorkItemID), 30, conversation.VisibilityShared)
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversation_messages SET text=? WHERE conversation_id=?", strings.Repeat("large shared message ", 12000), record.ID); err != nil {
		t.Fatal(err)
	}
	f.transition(t, string(issue.WorkItemID), "Done", f.now.Add(-73*time.Hour))
	if err := f.service.archiveClosedNativeIssues(t.Context(), f.now); err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	var before int64
	seen := map[int64]bool{}
	for {
		ctx := f.service.archiveContext(t.Context(), scope, false)
		messages, err := f.store.listMessages(ctx, f.service.database.reader, record.ID, before, 500)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) == 0 {
			break
		}
		for _, m := range messages {
			if seen[m.Seq] {
				t.Fatalf("duplicated message %d", m.Seq)
			}
			seen[m.Seq] = true
		}
		before = messages[0].Seq
	}
	recordAfter, err := f.store.readConversation(t.Context(), f.service.database.reader, f.project.OrganizationID, f.project.ID, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recordAfter.MessageCount != 30 {
		t.Fatalf("archival changed message count to %d", recordAfter.MessageCount)
	}
	if len(seen) != 30 {
		t.Fatalf("ranged history has %d messages", len(seen))
	}
	var sections int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_archive_sections WHERE subject_id=?", record.ID).Scan(&sections); err != nil {
		t.Fatal(err)
	}
	if sections < 2 {
		t.Fatalf("large history uses %d sections", sections)
	}
	for _, data := range f.objects {
		if len(archiveTestBytes(t, data)) > attachment.MaxSectionBytes {
			t.Fatal("oversized section")
		}
	}
	tx := f.tx(t)
	message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "late message", Delivery: conversation.DeliveryDelivered, CreatedAt: f.now, UpdatedAt: f.now, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.owner}}
	if err := f.store.appendMessage(t.Context(), tx, &message); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "UPDATE conversations SET event_seq=event_seq+1 WHERE id=?", record.ID); err != nil {
		t.Fatal(err)
	}
	f.commit(t, tx)
	if message.Seq != 31 {
		t.Fatalf("late message reused sequence %d", message.Seq)
	}
	if err := f.service.archiveClosedNativeIssues(t.Context(), f.now); err != nil {
		t.Fatal(err)
	}
	ctx := f.service.archiveContext(t.Context(), scope, false)
	latest, err := f.store.listMessages(ctx, f.service.database.reader, record.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 1 || latest[0].Seq != 31 || latest[0].Text != "late message" {
		t.Fatalf("late message not retrievable: %+v", latest)
	}
}

func TestNativeStorageArchiveTerminalClock(t *testing.T) {
	f := newStorageArchiveFixture(t)
	issue := f.nativeFixture.create(t, "terminal clock")
	tx := f.tx(t)
	states := append(append([]tracker.NativeState{}, f.project.States...), tracker.NativeState{Name: "Cancelled", Terminal: true})
	if err := updateNativeProjectStates(t.Context(), tx, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, states, f.now); err != nil {
		t.Fatal(err)
	}
	f.commit(t, tx)
	for _, step := range []struct {
		state   string
		at      time.Time
		entered *time.Time
	}{
		{"Done", f.now.Add(-74 * time.Hour), new(f.now.Add(-74 * time.Hour))},
		{"Cancelled", f.now.Add(-73 * time.Hour), new(f.now.Add(-74 * time.Hour))},
		{"Todo", f.now, nil},
		{"Done", f.now, new(f.now)},
	} {
		issue = f.transition(t, string(issue.WorkItemID), step.state, step.at)
		var entered *string
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT terminal_entered_at FROM issues WHERE native_id=?", issue.WorkItemID).Scan(&entered); err != nil {
			t.Fatal(err)
		}
		if step.entered == nil {
			if entered != nil {
				t.Fatal("reopen did not clear clock")
			}
		} else if entered == nil || *entered != formatHubTime(*step.entered) {
			t.Fatalf("state %s entered=%v", step.state, entered)
		}
	}
	f.now = f.now.Add(72 * time.Hour)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "late-comment"}, Body: "late comment remains searchable"}), http.StatusOK)
	if err := f.service.archiveClosedNativeIssues(t.Context(), f.now); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "later-comment"}, Body: "later comment remains live"}), http.StatusOK)
	var previous string
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT attachment_id FROM issue_storage_archives WHERE work_item_id=?", issue.WorkItemID).Scan(&previous); err != nil {
		t.Fatal(err)
	}
	if err := f.service.archiveClosedNativeIssues(t.Context(), f.now); err != nil {
		t.Fatal(err)
	}
	var next string
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT attachment_id FROM issue_storage_archives WHERE work_item_id=?", issue.WorkItemID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if previous == next {
		t.Fatal("late comment did not publish a new immutable root")
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID)+"/comments", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	if !strings.Contains(response.Body.String(), "late comment remains searchable") || !strings.Contains(response.Body.String(), "later comment remains live") {
		t.Fatal("comments lost after archival")
	}
}

func TestNativeStorageArchiveSafety(t *testing.T) {
	for _, test := range []string{"unknown terminal time", "active lease", "pending delivery", "pending question", "unknown execution", "unknown execution lease", "typed active execution"} {
		t.Run(test, func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			issue := f.nativeFixture.create(t, "protected history")
			record := f.messages(t, string(issue.WorkItemID), 1, conversation.VisibilityShared)
			if test == "active lease" {
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
				claimNativeAttempt(t, f.nativeFixture, f.worker(t, "worker"), "machine", "session", issue.WorkItemID)
			}
			if test == "unknown terminal time" {
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=issues.project_id AND detent_state='Done') WHERE native_id=?`, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			} else {
				f.transition(t, string(issue.WorkItemID), "Done", f.now.Add(-73*time.Hour))
			}
			switch test {
			case "unknown execution":
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversations SET execution_status=NULL WHERE id=?", record.ID); err != nil {
					t.Fatal(err)
				}
			case "unknown execution lease":
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversations SET execution_lease_id=NULL WHERE id=?", record.ID); err != nil {
					t.Fatal(err)
				}
			case "typed active execution":
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversations SET execution_status='running' WHERE id=?", record.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test == "pending delivery" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversation_messages SET delivery='queued' WHERE conversation_id=?", record.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test == "pending question" {
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO conversation_questions(id,conversation_id,status,created_at,updated_at) VALUES(?,?,'pending',?,?)`, "question_"+record.ID, record.ID, formatHubTime(f.now), formatHubTime(f.now)); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.archiveClosedNativeIssues(t.Context(), f.now); err != nil {
				t.Fatal(err)
			}
			if len(f.objects) != 0 {
				t.Fatal("protected work published an archive")
			}
		})
	}
}

func TestNativeStorageArchiveCommentRevisions(t *testing.T) {
	f := newStorageArchiveFixture(t)
	issue := f.nativeFixture.create(t, "comment history")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	tx := f.tx(t)
	comment, err := insertNativeComment(t.Context(), tx, scope, issue, "comment revision 1", nil, f.now)
	if err != nil {
		t.Fatal(err)
	}
	for revision := 2; revision <= 40; revision++ {
		comment.Revision = tracker.Revision(revision)
		comment.Body = "comment revision " + strconv.Itoa(revision)
		if err := recordNativeChange(t.Context(), tx, scope, comment, string(issue.WorkItemID), comment.Revision, "comment.edited", tracker.CollaborationData{CommentID: comment.ID, Revision: comment.Revision}, f.now); err != nil {
			t.Fatal(err)
		}
	}
	f.commit(t, tx)
	f.transition(t, string(issue.WorkItemID), "Done", f.now.Add(-73*time.Hour))
	beforeSchema := storageSchemaVersion(t, f.service.database.reader)
	for range 8 {
		if err := f.service.archiveClosedNativeIssues(t.Context(), f.now); err != nil {
			t.Fatal(err)
		}
	}
	if after := storageSchemaVersion(t, f.service.database.reader); after != beforeSchema {
		t.Fatalf("archive retirement changed schema: %d -> %d", beforeSchema, after)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM collaboration_versions WHERE record_id=?", comment.ID); err == nil {
		t.Fatal("ordinary revision deletion allowed after retirement")
	}
	var live int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_versions WHERE record_id=?", comment.ID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live <= 0 || live >= 40 {
		t.Fatalf("old revisions were not retired while preserving the live chain: %d", live)
	}
	for _, revision := range []int64{1, 15, 31, 32, 40} {
		ctx := f.service.archiveContext(t.Context(), scope, false)
		raw, err := f.service.readVersion(ctx, scope, string(issue.WorkItemID), comment.ID, revision)
		if err != nil {
			t.Fatal(err)
		}
		var got tracker.NativeComment
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got.Body != "comment revision "+strconv.FormatInt(revision, 10) {
			t.Fatalf("revision %d body=%q", revision, got.Body)
		}
	}
	tx = f.tx(t)
	comment.Revision = 41
	comment.Body = "late comment revision"
	if err := recordNativeChange(t.Context(), tx, scope, comment, string(issue.WorkItemID), comment.Revision, "comment.edited", tracker.CollaborationData{CommentID: comment.ID, Revision: comment.Revision}, f.now); err != nil {
		t.Fatal(err)
	}
	f.commit(t, tx)
	raw, err := f.service.readVersion(f.service.archiveContext(t.Context(), scope, false), scope, string(issue.WorkItemID), comment.ID, 41)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("late comment revision")) {
		t.Fatal("late edit lost its live reconstruction basis")
	}
}

func TestNativeStorageArchiveRunTimeline(t *testing.T) {
	a := newTerminalAttemptFixture(t)
	a.snapshots(t, 2)
	a.finish(t)
	a.advance(74 * time.Hour)
	objects := map[string][]byte{}
	a.service.config.content = func(_ context.Context, r entryauthority.ContentRequest) ([]byte, error) {
		if r.Write {
			objects[r.Reference.Key] = slices.Clone(r.Data)
		}
		data, ok := objects[r.Reference.Key]
		if !ok {
			return nil, errors.New("object missing")
		}
		return data, nil
	}
	a.service.config.ArchiveReader = func(_ context.Context, ref attachment.ArchiveReference, _ bool) (entryauthority.ArchiveResponse, error) {
		return entryauthority.ArchiveResponse{Data: objects[ref.Key]}, nil
	}
	tx, err := a.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	issue, _, err := readNativeIssue(t.Context(), tx, a.scope, string(a.issue.WorkItemID))
	if err != nil {
		t.Fatal(err)
	}
	from := issue.State
	issue.State = "Done"
	if _, err := persistNativeIssue(t.Context(), tx, a.scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: from, ToState: "Done"}, a.service.config.now().Add(-73*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	paths := []string{a.base + "/work-items/" + string(a.issue.WorkItemID) + "/runtime?native_attempt_id=" + a.start.Data.AttemptID, a.base + "/work-items/" + string(a.issue.WorkItemID) + "/history?limit=200"}
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
				return slices.Clone(response.Body.Bytes())
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
	for index, path := range paths {
		before[index] = read(index, path)
	}
	if err := a.service.convertLegacyStorage(t.Context(), a.service.config.now()); err != nil {
		t.Fatal(err)
	}
	if err := a.service.maintainNativeRetention(t.Context(), a.service.config.now()); err != nil {
		t.Fatal(err)
	}
	for index, path := range paths {
		assertArchiveGolden(t, before[index], read(index, path))
	}
	var checkpoints int
	if err := a.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempt_events WHERE attempt_id=?", a.start.Data.AttemptID).Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if checkpoints == 0 {
		t.Fatal("replay identities retired")
	}
	var roots int
	for key, data := range objects {
		if !strings.HasSuffix(key, "/issue.json.gz") {
			continue
		}
		roots++
		raw := archiveTestBytes(t, data)
		for _, fact := range []string{"run.started", "run.finished", "succeeded", a.start.Data.AttemptID, "actor"} {
			if !bytes.Contains(raw, []byte(fact)) {
				t.Fatalf("root timeline lost %s", fact)
			}
		}
	}
	if roots != 1 {
		t.Fatalf("run history published %d roots", roots)
	}
	a.service.config.ArchiveReader = func(context.Context, attachment.ArchiveReference, bool) (entryauthority.ArchiveResponse, error) {
		return entryauthority.ArchiveResponse{}, errors.New("archive temporarily unavailable")
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, paths[1], nil)
	request.Header.Set("Authorization", "Bearer "+a.token)
	request.Header.Set("X-Detent-Archive-Read", "1")
	response := httptest.NewRecorder()
	a.service.Handler().ServeHTTP(response, request)
	requireNativeStatus(t, response, http.StatusOK)
	if !strings.Contains(response.Body.String(), `"type":"run.finished"`) || !strings.Contains(response.Body.String(), `"outcome":"succeeded"`) {
		t.Fatal("SQLite timeline lost run facts")
	}
}
