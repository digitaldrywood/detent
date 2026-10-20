package hubserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptEvidenceAcrossStorageUpgrade(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "evidence-upgrade")
	worker := f.worker(t, "worker")
	lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID)
	start := nativeStartedEvent(lease)
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, start), http.StatusOK)
	credential, _, err := f.service.authenticateAPIToken(t.Context(), worker, "", "")
	if err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
	now := time.Now().UTC()
	var files [2]attachment.Metadata
	for i := range files {
		record := attachment.Metadata{ID: conversation.NewAttachmentID(), ProjectID: string(f.project.ID), Name: "receipt.txt", ContentType: "text/plain", Size: 5, SHA256: artifact.Digest([]byte("hello"))}
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, record.ID, scope.organization, scope.project, credential.ID, record.Name, record.ContentType, record.Size, record.SHA256, formatHubTime(now)); err != nil {
			t.Fatal(err)
		}
		files[i] = record
	}
	stored := start.Data
	stored.Evidence = []tracker.NativeEvidence{{AttachmentID: files[0].ID, Caption: "Legacy receipt", Reference: files[0].Markdown(string(scope.organization))}}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=? WHERE id=?", string(encoded), start.Data.AttemptID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	input := attachment.EvidenceRequest{Mutation: tracker.Mutation{LeaseID: lease.ID, FencingToken: lease.FencingToken}, WorkItemID: issue.WorkItemID, AttemptID: start.Data.AttemptID, Caption: "Current [receipt]"}
	if err := recordAttachmentEvidence(t.Context(), tx, scope, input, files[1], now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var legacyCount int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT json_array_length(data_json,'$.evidence') FROM native_attempts WHERE id=?", start.Data.AttemptID).Scan(&legacyCount); err != nil || legacyCount != 1 {
		t.Fatalf("upload grew legacy attempt JSON: count=%d error=%v", legacyCount, err)
	}
	finish := start
	finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome, finish.Data.CompletionBody = "run.finished", "finish", 2, "failed", "Verification response"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, finish), http.StatusOK)
	var comments tracker.Page[tracker.NativeComment]
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/comments", worker, nil), &comments)
	if len(comments.Items) != 2 || !strings.Contains(comments.Items[0].Body, "Current \\[receipt\\]") || !strings.Contains(comments.Items[0].Body, files[1].Markdown(string(scope.organization))) || !strings.Contains(comments.Items[1].Body, "Verification response") || !strings.Contains(comments.Items[1].Body, files[0].Markdown(string(scope.organization))) {
		t.Fatalf("upgrade lost evidence: %+v", comments)
	}
	var attempts tracker.Page[tracker.NativeAttempt]
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts", worker, nil), &attempts)
	if len(attempts.Items) != 1 || len(attempts.Items[0].Evidence) != 2 || attempts.Items[0].Evidence[0].AttachmentID != files[0].ID || attempts.Items[0].Evidence[1].AttachmentID != files[1].ID {
		t.Fatalf("upgrade lost attempt references: %+v", attempts)
	}
	for range 80 {
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at,work_item_id,evidence_attempt_id,evidence_caption) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, conversation.NewAttachmentID(), scope.organization, scope.project, credential.ID, "screenshot.png", "image/png", 5, artifact.Digest([]byte("hello")), formatHubTime(now), issue.WorkItemID, start.Data.AttemptID, strings.Repeat("c", 1024)); err != nil {
			t.Fatal(err)
		}
	}
	var blockers tracker.Page[tracker.NativeAttempt]
	response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?view=blockers", worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &blockers)
	if len(blockers.Items) != 1 || len(blockers.Items[0].Evidence) != 1 || blockers.Items[0].Evidence[0].AttachmentID != files[0].ID {
		t.Fatalf("blocker projection carried uploaded evidence: %+v", blockers)
	}
}
