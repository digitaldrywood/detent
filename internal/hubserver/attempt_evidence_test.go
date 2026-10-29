package hubserver

import (
	"encoding/json"
	"net/http"
	"reflect"
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
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET dispatch_generation=7 WHERE id=?", start.Data.AttemptID); err != nil {
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
	var summaries tracker.Page[tracker.NativeAttempt]
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?view=blockers", worker, nil), &summaries)
	if len(summaries.Items) != 1 || len(summaries.Items[0].Evidence) != 2 {
		t.Fatalf("blocker summaries lost uploaded evidence: %+v", summaries)
	}
	for range 80 {
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at,work_item_id,evidence_attempt_id,evidence_caption) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, conversation.NewAttachmentID(), scope.organization, scope.project, credential.ID, "screenshot.png", "image/png", 5, artifact.Digest([]byte("hello")), formatHubTime(now), issue.WorkItemID, start.Data.AttemptID, strings.Repeat("c", 1024)); err != nil {
			t.Fatal(err)
		}
	}
	var blockers tracker.Page[tracker.NativeAttempt]
	response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?view=recorded_blockers", worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &blockers)
	if len(blockers.Items) != 1 || len(blockers.Items[0].Evidence) != 1 || blockers.Items[0].Evidence[0].AttachmentID != files[0].ID {
		t.Fatalf("blocker projection carried uploaded evidence: %+v", blockers)
	}
	for _, released := range []bool{false, true} {
		if released {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts/"+start.Data.AttemptID, worker, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var full tracker.NativeAttempt
		decodeHubResponse(t, response, &full)
		if len(full.Evidence) != 82 || full.CompletionBody != "Verification response" {
			t.Fatal("full receipt lost retained completion or evidence")
		}
		response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?view=blockers", worker, nil)
		requireNativeStatus(t, response, http.StatusOK)
		summaries = tracker.Page[tracker.NativeAttempt]{}
		decodeHubResponse(t, response, &summaries)
		if len(summaries.Items) != 1 {
			t.Fatal("oversized evidence dropped the attempt")
		}
		got := summaries.Items[0]
		if got.Current == released || (got.ClaimReleasedAt != nil) != released || got.LeaseID != lease.ID || got.FencingToken != lease.FencingToken || got.DispatchGeneration != 7 {
			t.Fatalf("projection lost lease authority: current=%t released=%v lease=%s fence=%d", got.Current, got.ClaimReleasedAt, got.LeaseID, got.FencingToken)
		}
		full.CompletionBody, full.Evidence = "", nil
		full.Runtime, full.RuntimeFreshness = nil, "unavailable"
		full.Unavailable = []string{"completion_body", "evidence"}
		if !reflect.DeepEqual(got, full) {
			t.Fatal("evidence projection changed ownership or dispatch fields")
		}
	}
}
