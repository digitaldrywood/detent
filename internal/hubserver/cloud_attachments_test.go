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
)

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
