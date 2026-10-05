//go:build !windows

package cloudentry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeAgentEvidence(t *testing.T) {
	for _, completion := range []string{"Verified the page in a browser.", ""} {
		t.Run("completion="+completion, func(t *testing.T) {
			f := newEntryFixture(t)
			store := newSpacesFixture(t, false)
			storage, err := attachment.NewStorage(t.Context(), store.config(), store.transport)
			if err != nil {
				t.Fatal(err)
			}
			f.service.attachments = storage
			owner := newBrowser(t, f.service.Handler())
			owner.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
			_, page := owner.get("/organizations/org_alpha/organization")
			o := pilotOrganization{id: "org_alpha", project: attachmentProject(t, owner, "org_alpha"), ownerCSRF: csrfFrom(t, page)}
			request := func(method, path string, body io.Reader, headers map[string]string) pilotResponse {
				r := attachmentRequest(t, owner, method, path, body, headers)
				return pilotResponse{status: r.Code, body: r.Body.String(), header: r.Header()}
			}
			ownerJSON := func(method, path, csrf string, input any) pilotResponse {
				return request(method, path, jsonBody(t, input), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": csrf})
			}
			machine := func(method, path, token string, input any) pilotResponse {
				return request(method, path, jsonBody(t, input), map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + token})
			}
			binding := runnerauth.NewBinding()
			enrollment := ownerJSON(http.MethodPost, o.api()+"/runner-enrollments", o.ownerCSRF, runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{tracker.ProjectID(o.project)}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 900})
			pilotStatus(t, "enrollment", enrollment, http.StatusCreated)
			var issued runnerauth.Enrollment
			pilotDecode(t, enrollment, &issued)
			credential, err := apikey.GenerateToken()
			if err != nil {
				t.Fatal(err)
			}
			redemption := runnerauth.Redemption{BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: binding, Credential: credential, Hostname: "evidence-runner", DisplayName: "evidence-runner", Capacity: 1, Version: "test", OS: "linux", Architecture: "amd64"}
			pilotStatus(t, "redemption", machine(http.MethodPost, o.api()+"/runner-enrollments/redeem", issued.Token, redemption), http.StatusCreated)
			descriptor := pilotPolicy()
			pilotStatus(t, "policy", ownerJSON(http.MethodPut, o.projectAPI()+"/onboarding/policy", o.ownerCSRF, policy.Change{Policy: descriptor}), http.StatusOK)
			var issue tracker.NativeIssue
			response := ownerJSON(http.MethodPost, o.projectAPI()+"/work-items", o.ownerCSRF, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "evidence-work"}, Title: "Screenshot the page", State: "Todo"})
			pilotStatus(t, "issue", response, http.StatusOK)
			pilotDecode(t, response, &issue)
			client, err := hubclient.New(hubclient.Config{URL: testPublicURL + "/organizations/" + o.id, TokenSource: func() string { return credential }, HTTPClient: &http.Client{Transport: handlerTransport{f.service.Handler()}}})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := hubclient.NewScheduler(client, hubclient.SchedulerConfig{OrganizationID: tracker.OrganizationID(o.id), NativeProjects: map[string]tracker.ProjectID{"local": tracker.ProjectID(o.project)}, Machine: hubclient.Machine{ID: binding.MachineID, Hostname: "evidence-runner", Capacity: 1, Version: "test", BackendIsolation: redemption.BackendIsolation}, LeaseTTL: 90 * time.Second, HeartbeatInterval: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			items, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: descriptor, WorkflowStates: []string{"Todo"}})
			if err != nil || len(items) != 1 || items[0].ID != string(issue.WorkItemID) {
				t.Fatalf("claim: %+v, %v", items, err)
			}
			execution := scheduler.RunExecution(items[0].ID)
			if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "merge", Backend: "git", Model: "none"}); err != nil {
				t.Fatal(err)
			}
			var pixels bytes.Buffer
			if err := png.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
				t.Fatal(err)
			}
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "page.png"), pixels.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			execution.(runner.EvidenceSourceExecution).SetEvidenceSource(func(_ context.Context, path string) (runner.ValidationEvidence, error) {
				content, err := os.ReadFile(filepath.Join(workspace, path))
				return runner.ValidationEvidence{Name: "page.png", ContentType: "image/png", Content: content}, err
			})
			_, handler := execution.(runner.ToolExecution).AgentTools()
			call := runner.AgentToolCall{Name: "attach_evidence", Arguments: json.RawMessage(`{"path":"page.png","caption":"Page rendered correctly"}`)}
			result, err := handler(t.Context(), call)
			var uploaded attachment.Metadata
			if err != nil || !result.Success || json.Unmarshal([]byte(result.Content), &uploaded) != nil || uploaded.Reference != uploaded.Markdown(o.id) {
				t.Fatalf("tool: %+v, %v", result, err)
			}
			replay, err := handler(t.Context(), call)
			var repeated attachment.Metadata
			if err != nil || json.Unmarshal([]byte(replay.Content), &repeated) != nil || repeated.ID != uploaded.ID || repeated.Reference != uploaded.Reference {
				t.Fatalf("upload replay: %+v, %v", replay, err)
			}
			if err := execution.Checkpoint(t.Context(), tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "dirty", ExternalEffect: "none", EffectState: "none"}); err != nil {
				t.Fatal(err)
			}
			if completion != "" {
				if err := execution.(runner.CompletionExecution).PrepareFinish(t.Context(), "failed", completion); err != nil {
					t.Fatal(err)
				}
			}
			if err := execution.Finish(t.Context(), "failed"); err != nil {
				t.Fatal(err)
			}
			if err := execution.Finish(t.Context(), "failed"); err != nil {
				t.Fatal(err)
			}
			native, ok := scheduler.NativeClient("local")
			if !ok {
				t.Fatal("native client unavailable")
			}
			comments, err := native.Comments(t.Context(), issue.WorkItemID, "")
			if err != nil || len(comments.Items) != 1 || !strings.Contains(comments.Items[0].Body, completion) || !strings.Contains(comments.Items[0].Body, "Page rendered correctly") || !strings.Contains(comments.Items[0].Body, uploaded.Reference) {
				t.Fatalf("completion evidence: %+v, %v", comments, err)
			}
			attempts, err := native.Attempts(t.Context(), issue.WorkItemID, "")
			if err != nil || len(attempts.Items) != 1 || len(attempts.Items[0].Evidence) != 1 || attempts.Items[0].Evidence[0].AttachmentID != uploaded.ID {
				t.Fatalf("attempt evidence: %+v, %v", attempts, err)
			}
			read := request(http.MethodGet, strings.TrimSuffix(uploaded.Reference, ")")[strings.Index(uploaded.Reference, "(")+1:], nil, nil)
			if read.status != http.StatusOK {
				t.Fatalf("operator attachment read: %+v", read)
			}
			lease := execution.Recovery().Lease
			lease.FencingToken++
			authority, err := json.Marshal(attachment.EvidenceRequest{Mutation: tracker.Mutation{IdempotencyKey: "stale-evidence", LeaseID: lease.ID, FencingToken: lease.FencingToken}, AttemptID: attempts.Items[0].AttemptID, WorkItemID: issue.WorkItemID, Caption: "Stale screenshot"})
			if err != nil {
				t.Fatal(err)
			}
			stale := request(http.MethodPost, "/organizations/"+o.id+"/api/v2/projects/"+o.project+"/attempts/"+attempts.Items[0].AttemptID+"/evidence", bytes.NewReader(pixels.Bytes()), map[string]string{"Authorization": "Bearer " + credential, "Content-Type": "image/png", "X-Attachment-Name": "page.png", "Idempotency-Key": "stale-evidence", "X-Detent-Evidence": base64.RawURLEncoding.EncodeToString(authority)})
			if stale.status != http.StatusConflict || !strings.Contains(stale.body, "stale_execution") {
				t.Fatalf("stale upload: %+v", stale)
			}
			store.mu.Lock()
			objects := len(store.objects)
			store.mu.Unlock()
			if objects != 1 {
				t.Fatalf("replay or stale upload created extra storage objects: %d", objects)
			}
		})
	}
}
