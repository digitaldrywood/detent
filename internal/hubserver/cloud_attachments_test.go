package hubserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCloudAttachmentSavedReferences(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"issue", "comment", "issue-path", "comment-path"} {
		t.Run(source, func(t *testing.T) {
			f := newHostedSharedFixture(t)
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			csrf := cloudassert.CSRFToken("shared-"+owner.identity.Subject, "org_security")
			send := func(method, suffix string, input any, want int) []byte {
				t.Helper()
				body, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				response := f.serve(t, hostedSharedRequest{user: &owner, method: method, target: f.base + suffix, body: string(body), csrf: csrf})
				if response.Code != want {
					t.Fatalf("%s %s: %d %s", method, suffix, response.Code, response.Body.String())
				}
				return response.Body.Bytes()
			}
			record := attachment.Metadata{ID: conversation.NewAttachmentID(), Name: "image.png", ContentType: "image/png", Size: 100, Width: 1, Height: 1, SHA256: artifact.Digest([]byte("image"))}
			raw := send(http.MethodPost, "/attachment-metadata", record, http.StatusCreated)
			if err := json.Unmarshal(raw, &record); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.Exec(`INSERT INTO projects(id,organization_id,name,profile,states_json,created_at,github_repository_enabled) SELECT 'prj_foreign',organization_id,'foreign',profile,states_json,created_at,0 FROM projects WHERE id=?`, f.project); err != nil {
				t.Fatal(err)
			}
			foreign := conversation.NewAttachmentID()
			if _, err := f.service.database.db.Exec(`INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at) VALUES(?,'org_security','prj_foreign',?,'file.txt','text/plain',10,?,?)`, foreign, record.Uploader, record.SHA256, formatHubTime(time.Now())); err != nil {
				t.Fatal(err)
			}
			body := "![image](attachment:" + record.ID + ")\n[file](attachment:" + record.ID + ")\n![foreign](attachment:" + foreign + ")"
			copyBody := body
			if strings.HasSuffix(source, "-path") {
				body = record.Markdown("org_security")
			}
			issue := tracker.NativeIssue{}
			comment := tracker.NativeComment{}
			if strings.HasPrefix(source, "issue") {
				raw = send(http.MethodPost, "/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "save-image"}, Title: "Image issue", Body: body, State: "Todo"}, http.StatusOK)
				if err := json.Unmarshal(raw, &issue); err != nil {
					t.Fatal(err)
				}
			} else {
				issue.WorkItemID = f.seedIssue(t, 1)
				raw = send(http.MethodPost, "/work-items/"+string(issue.WorkItemID)+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "save-image"}, Body: body}, http.StatusOK)
				if err := json.Unmarshal(raw, &comment); err != nil {
					t.Fatal(err)
				}
			}
			assertReference := func(id, item, comment string) {
				t.Helper()
				var gotItem, gotComment string
				if err := f.service.database.db.QueryRow("SELECT coalesce(work_item_id,''),coalesce(comment_id,'') FROM attachments WHERE id=?", id).Scan(&gotItem, &gotComment); err != nil {
					t.Fatal(err)
				}
				if gotItem != item || gotComment != comment {
					t.Fatalf("reference=%s/%s want %s/%s", gotItem, gotComment, item, comment)
				}
			}
			assertReference(record.ID, string(issue.WorkItemID), comment.ID)
			assertReference(foreign, "", "")
			var count int
			if err := f.service.database.db.QueryRow("SELECT count(*) FROM attachment_references WHERE attachment_id=?", record.ID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("references=%d err=%v", count, err)
			}
			if _, err := f.service.database.db.Exec("UPDATE attachments SET created_at=? WHERE id=?", formatHubTime(time.Now().Add(-attachment.OrphanTTL-time.Hour)), record.ID); err != nil {
				t.Fatal(err)
			}
			sweep := f.serve(t, hostedSharedRequest{kind: cloudassert.KindService, method: http.MethodPost, target: "/internal/v1/attachments/expired", body: "{}"})
			if sweep.Code != http.StatusOK || strings.Contains(sweep.Body.String(), record.ID) {
				t.Fatalf("referenced image swept: %d %s", sweep.Code, sweep.Body.String())
			}
			var other tracker.NativeComment
			raw = send(http.MethodPost, "/work-items/"+string(issue.WorkItemID)+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "copy-image"}, Body: copyBody}, http.StatusOK)
			if err := json.Unmarshal(raw, &other); err != nil {
				t.Fatal(err)
			}
			var metadata attachment.Metadata
			response := f.serve(t, hostedSharedRequest{user: &owner, target: f.base + "/attachment-metadata/" + record.ID})
			if response.Code != http.StatusOK {
				t.Fatalf("metadata=%d", response.Code)
			}
			if err := json.Unmarshal(response.Body.Bytes(), &metadata); err != nil {
				t.Fatal(err)
			}
			if len(metadata.ReferencedBy) != 2 {
				t.Fatalf("referenced_by=%v", metadata.ReferencedBy)
			}
			if strings.HasPrefix(source, "issue") {
				removed := "No image"
				send(http.MethodPatch, "/work-items/"+string(issue.WorkItemID), tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "remove-image"}, ExpectedRevision: issue.Revision, Body: &removed}, http.StatusOK)
			} else {
				send(http.MethodPatch, "/work-items/"+string(issue.WorkItemID)+"/comments/"+comment.ID, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "remove-image"}, ExpectedRevision: comment.Revision, Body: "No image"}, http.StatusOK)
			}
			assertReference(record.ID, string(issue.WorkItemID), other.ID)
			var last tracker.NativeComment
			raw = send(http.MethodPost, "/work-items/"+string(issue.WorkItemID)+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "retain-copy"}, Body: copyBody}, http.StatusOK)
			if err := json.Unmarshal(raw, &last); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.Exec("DELETE FROM native_comments WHERE id=?", other.ID); err != nil {
				t.Fatal(err)
			}
			response = f.serve(t, hostedSharedRequest{user: &owner, target: f.base + "/attachment-metadata/" + record.ID})
			if response.Code != http.StatusOK {
				t.Fatalf("retained copy read=%d %s", response.Code, response.Body.String())
			}
			assertReference(record.ID, string(issue.WorkItemID), last.ID)
			send(http.MethodPatch, "/work-items/"+string(issue.WorkItemID)+"/comments/"+last.ID, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "orphan-copy"}, ExpectedRevision: last.Revision, Body: "No attachment"}, http.StatusOK)
			assertReference(record.ID, "", "")
			raw = send(http.MethodPost, "/work-items/"+string(issue.WorkItemID)+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "last-copy"}, Body: body}, http.StatusOK)
			if err := json.Unmarshal(raw, &last); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.Exec("DELETE FROM native_comments WHERE id=?", last.ID); err != nil {
				t.Fatal(err)
			}
			response = f.serve(t, hostedSharedRequest{user: &owner, target: f.base + "/attachment-metadata/" + record.ID})
			if response.Code != http.StatusNotFound {
				t.Fatalf("last source deletion read=%d", response.Code)
			}
		})
	}
}

func TestCloudAttachmentQuota(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		allowance int64
		status    int
	}{{"allowed", 10000, http.StatusCreated}, {"refused", 1, http.StatusTooManyRequests}} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostedSharedFixture(t)
			hostedTestPlans(t, f.service, map[string]int64{"collaboration_bytes": test.allowance})
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			record := attachment.Metadata{ID: conversation.NewAttachmentID(), Name: "file.txt", ContentType: "text/plain", Size: 100, SHA256: artifact.Digest([]byte("hello"))}
			body, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			csrf := cloudassert.CSRFToken("shared-"+owner.identity.Subject, "org_security")
			response := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: f.base + "/attachment-metadata", body: string(body), csrf: csrf})
			if response.Code != test.status {
				t.Fatalf("upload=%d %s", response.Code, response.Body.String())
			}
			var count int
			if err := f.service.database.db.QueryRow("SELECT count(*) FROM attachments WHERE id=?", record.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if test.status == http.StatusTooManyRequests {
				if count != 0 || !strings.Contains(response.Body.String(), "allowance_exhausted") {
					t.Fatal("quota refusal retained metadata or changed allowance error")
				}
				return
			}
			usage, err := f.service.database.hostedConsumption(t.Context(), f.service.database.db, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if usage["collaboration_bytes"] != 100 {
				t.Fatalf("attachment bytes=%d", usage["collaboration_bytes"])
			}
		})
	}
}

func TestCloudAttachmentRetention(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t)
	owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
	var principal string
	if err := f.service.database.db.QueryRow("SELECT principal_id FROM hosted_members WHERE user_id=?", owner.identity.Subject).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	item := f.seedIssue(t, 1)
	now := time.Now().UTC()
	for _, test := range []struct {
		name                         string
		age                          time.Duration
		referenced, deleted, expired bool
	}{
		{name: "old orphan", age: attachment.OrphanTTL + time.Minute, expired: true},
		{name: "new orphan", age: time.Hour},
		{name: "archived reference", age: attachment.OrphanTTL + time.Hour, referenced: true},
		{name: "deleted reference", age: time.Hour, referenced: true, deleted: true, expired: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := conversation.NewAttachmentID()
			var reference, deleted any
			if test.referenced {
				reference = string(item)
			}
			if test.deleted {
				deleted = formatHubTime(now)
			}
			_, err := f.service.database.db.Exec(`INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at,work_item_id,deleted_at) VALUES(?,'org_security',?,?,'file.txt','text/plain',10,?,?,?,?)`, id, f.project, principal, artifact.Digest([]byte("hello")), formatHubTime(now.Add(-test.age)), reference, deleted)
			if err != nil {
				t.Fatal(err)
			}
			if test.referenced && !test.deleted {
				if _, err := f.service.database.db.Exec("UPDATE issues SET archived=1 WHERE native_id=?", item); err != nil {
					t.Fatal(err)
				}
			}
			response := f.serve(t, hostedSharedRequest{kind: cloudassert.KindService, method: http.MethodPost, target: "/internal/v1/attachments/expired", body: "{}"})
			if response.Code != http.StatusOK {
				t.Fatalf("sweep=%d %s", response.Code, response.Body.String())
			}
			var expired []attachment.Metadata
			if err := json.Unmarshal(response.Body.Bytes(), &expired); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, record := range expired {
				if record.ID == id {
					found = true
				}
			}
			if found != test.expired {
				t.Fatalf("expired=%v want %v", found, test.expired)
			}
			if test.expired {
				response := f.serve(t, hostedSharedRequest{kind: cloudassert.KindService, method: http.MethodPost, target: "/internal/v1/attachments/deleted", body: `{"id":"` + id + `"}`})
				if response.Code != 204 {
					t.Fatalf("deleted=%d", response.Code)
				}
			}
		})
	}
}

func TestCloudAttachmentReferenceDeletion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		comment bool
	}{{name: "issue"}, {name: "comment", comment: true}} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostedSharedFixture(t)
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			item := f.seedIssue(t, 1)
			var principal string
			if err := f.service.database.db.QueryRow("SELECT principal_id FROM hosted_members WHERE user_id=?", owner.identity.Subject).Scan(&principal); err != nil {
				t.Fatal(err)
			}
			id := conversation.NewAttachmentID()
			_, err := f.service.database.db.Exec(`INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at) VALUES(?,'org_security',?,?,'file.txt','text/plain',10,?,?)`, id, f.project, principal, artifact.Digest([]byte("hello")), formatHubTime(time.Now()))
			if err != nil {
				t.Fatal(err)
			}
			var comment string
			if test.comment {
				comment = "comment_test"
				_, err := f.service.database.db.Exec(`INSERT INTO native_comments(id,organization_id,project_id,work_item_id,revision,sequence,body,actor_json,created_at,updated_at) VALUES(?,'org_security',?,?,1,1,'body','{}',?,?)`, comment, f.project, item, formatHubTime(time.Now()), formatHubTime(time.Now()))
				if err != nil {
					t.Fatal(err)
				}
			}
			payload, err := json.Marshal(map[string]string{"work_item_id": string(item), "comment_id": comment})
			if err != nil {
				t.Fatal(err)
			}
			response := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: f.base + "/attachment-metadata/" + id + "/reference", body: string(payload), csrf: cloudassert.CSRFToken("shared-"+owner.identity.Subject, "org_security")})
			if response.Code != 204 {
				t.Fatalf("reference=%d %s", response.Code, response.Body.String())
			}
			statement, args := "DELETE FROM issues WHERE native_id=?", []any{item}
			if test.comment {
				statement, args = "DELETE FROM native_comments WHERE id=?", []any{comment}
			}
			if _, err := f.service.database.db.Exec(statement, args...); err != nil {
				t.Fatal(err)
			}
			response = f.serve(t, hostedSharedRequest{user: &owner, target: f.base + "/attachment-metadata/" + id})
			if response.Code != 404 {
				t.Fatalf("deleted reference read=%d", response.Code)
			}
		})
	}
}
