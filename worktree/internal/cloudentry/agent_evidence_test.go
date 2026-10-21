//go:build !windows

package cloudentry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/claudecode"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeAgentEvidence(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

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
	var previousIssue tracker.NativeIssue
	var previousAttempt string
	var previousUploads []attachment.Metadata
	for index, test := range []struct {
		completion, tier string
	}{{strings.Repeat("x", 64<<10), isolation.NativeTrusted}, {"", isolation.Sandbox}} {
		t.Run(test.tier, func(t *testing.T) {
			completion := test.completion
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
			redemption := runnerauth.Redemption{BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}, "claude": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: binding, Credential: credential, Hostname: "evidence-runner", DisplayName: "evidence-runner", Capacity: 1, Version: "test", OS: "linux", Architecture: "amd64"}
			pilotStatus(t, "redemption", machine(http.MethodPost, o.api()+"/runner-enrollments/redeem", issued.Token, redemption), http.StatusCreated)
			descriptor := pilotPolicy()
			pilotStatus(t, "policy", ownerJSON(http.MethodPut, o.projectAPI()+"/onboarding/policy", o.ownerCSRF, policy.Change{Policy: descriptor}), http.StatusOK)
			var issue tracker.NativeIssue
			draft := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("evidence-work-%d", index)}, Title: fmt.Sprintf("Capture screen %d", index), Body: issueContractTestSections, State: "Todo"}
			response := ownerJSON(http.MethodPost, o.projectAPI()+"/work-items", o.ownerCSRF, draft)
			if response.status == http.StatusConflict {
				var challenge nativeDuplicateFixture
				pilotDecode(t, response, &challenge)
				draft.ChallengeToken = challenge.ChallengeToken
				for _, match := range challenge.PossibleDuplicates {
					draft.NotDuplicateOf = append(draft.NotDuplicateOf, match.Number)
				}
				response = ownerJSON(http.MethodPost, o.projectAPI()+"/work-items", o.ownerCSRF, draft)
			}
			pilotStatus(t, "issue", response, http.StatusOK)
			pilotDecode(t, response, &issue)
			inputContent := strings.Repeat("approved portable input\n", 1600)
			inputPath := o.projectAPI() + "/attachments"
			inputUpload := request(http.MethodPost, inputPath, strings.NewReader(inputContent), map[string]string{"Content-Type": "text/plain", "X-Attachment-Name": "approved-input.txt", "X-CSRF-Token": o.ownerCSRF})
			pilotStatus(t, "human input upload", inputUpload, http.StatusCreated)
			var inputAttachment attachment.Metadata
			pilotDecode(t, inputUpload, &inputAttachment)
			inputComment := ownerJSON(http.MethodPost, o.projectAPI()+"/work-items/"+string(issue.WorkItemID)+"/comments", o.ownerCSRF, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "input-comment"}, Body: inputAttachment.Reference})
			pilotStatus(t, "human input comment", inputComment, http.StatusOK)
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
			if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "design", Backend: "claude", Model: "fable"}); err != nil {
				t.Fatal(err)
			}
			var pixels bytes.Buffer
			canvas := image.NewRGBA(image.Rect(0, 0, 128, 128))
			noise := rand.New(rand.NewPCG(1, 2))
			for i := range canvas.Pix {
				canvas.Pix[i] = byte(noise.Uint32())
			}
			if err := png.Encode(&pixels, canvas); err != nil {
				t.Fatal(err)
			}
			if pixels.Len() <= operatortool.MaxArgumentBytes {
				t.Fatal("PNG does not exercise publication beyond the MCP argument limit")
			}
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "page.png"), pixels.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			indexContent := "Screen: example page\nState: default\nArtifact: page.png\nDecision: awaiting human design approval\n"
			if err := os.WriteFile(filepath.Join(workspace, "index.txt"), []byte(indexContent), 0o600); err != nil {
				t.Fatal(err)
			}
			execution.(runner.EvidenceSourceExecution).SetEvidenceSource(func(_ context.Context, path string) (runner.ValidationEvidence, error) {
				if path == "large.log" || path == "oversized.log" {
					size := attachment.MaxBytes
					if path == "oversized.log" {
						size++
					}
					return runner.ValidationEvidence{Name: "large.log", ContentType: "text/plain", Content: bytes.Repeat([]byte("x"), size)}, nil
				}
				content, err := os.ReadFile(filepath.Join(workspace, path))
				media := "image/png"
				if path == "index.txt" {
					media = "text/plain"
				}
				return runner.ValidationEvidence{Name: path, ContentType: media, Content: content}, err
			})
			tools, handler := execution.(runner.ToolExecution).AgentTools()
			backend, err := claudecode.NewAgentBackend(claudecode.Options{IsolationPolicy: func() (isolation.Policy, error) {
				return isolation.Policy{Tier: test.tier}, nil
			}, CommandFactoryWithArgs: func(ctx context.Context, args []string) *exec.Cmd {
				cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestClaudeEvidenceWorkerProcess$", "--"}, args...)...)
				cmd.Env = append(os.Environ(), "DETENT_CLAUDE_EVIDENCE_HELPER=1")
				return cmd
			}})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := any(backend).(runner.AgentToolBackend); !ok {
				t.Fatal("official Claude backend drops the native worker evidence catalog")
			}
			for _, tool := range tools {
				if operatortool.IsAttachmentTool(tool.Name) && tool.Name != operatortool.ReadAttachment && tool.Name != operatortool.ReadAttachmentMetadata {
					t.Fatalf("worker gained attachment mutation %s", tool.Name)
				}
			}
			for _, part := range []struct {
				name string
				args map[string]any
				want string
			}{
				{operatortool.WorkComments, map[string]any{"reference": issue.WorkItemID}, inputAttachment.ID},
				{operatortool.ReadAttachmentMetadata, map[string]any{"attachment_id": inputAttachment.ID}, inputAttachment.SHA256},
				{operatortool.ReadAttachment, map[string]any{"attachment_id": inputAttachment.ID}, inputContent[:operatortool.AttachmentContentBytes]},
				{operatortool.ReadAttachment, map[string]any{"attachment_id": inputAttachment.ID, "offset": operatortool.AttachmentContentBytes}, inputContent[operatortool.AttachmentContentBytes:]},
				{operatortool.ReadAttachment, map[string]any{"attachment_id": inputAttachment.ID, "length": 1}, inputContent[:1]},
				{operatortool.ReadAttachment, map[string]any{"attachment_id": inputAttachment.ID, "offset": len(inputContent)}, ""},
			} {
				part.args["project_id"] = o.project
				arguments, err := json.Marshal(part.args)
				if err != nil {
					t.Fatal(err)
				}
				result, err := handler(t.Context(), runner.AgentToolCall{Name: part.name, Arguments: arguments})
				if err != nil || !result.Success {
					t.Fatalf("worker input %s: %+v, %v", part.name, result, err)
				}
				if strings.Contains(result.Content, credential) || strings.Contains(result.Content, "authorized_principal") || strings.Contains(result.Content, "spaces.test") {
					t.Fatal("worker attachment read exposed internal authority or storage")
				}
				if part.name == operatortool.ReadAttachment {
					var read operatortool.AttachmentContentResult
					if json.Unmarshal([]byte(result.Content), &read) != nil {
						t.Fatal("worker input was not a content result")
					}
					content, err := base64.StdEncoding.DecodeString(read.ContentBase64)
					if err != nil || string(content) != part.want || read.ReturnedBytes != len(content) || read.Metadata.ID != inputAttachment.ID || len(read.Metadata.ReferencedBy) != 1 || read.Metadata.ReferencedBy[0].WorkItemID != string(issue.WorkItemID) || read.EOF != (read.Offset+int64(len(content)) == inputAttachment.Size) {
						t.Fatalf("worker input content=%+v, %v", read, err)
					}
				} else if !strings.Contains(result.Content, part.want) {
					t.Fatalf("worker input %s omitted %s", part.name, part.want)
				}
			}
			for _, refusal := range []struct {
				name string
				args map[string]any
				want error
			}{
				{operatortool.ReadAttachment, map[string]any{"length": 32769}, operatortool.ErrInvalidArguments},
				{operatortool.ReadAttachment, map[string]any{"length": 0}, operatortool.ErrInvalidArguments},
				{operatortool.ReadAttachment, map[string]any{"offset": -1}, operatortool.ErrInvalidArguments},
				{operatortool.ReadAttachment, map[string]any{"offset": inputAttachment.Size + 1}, operatortool.ErrInvalidArguments},
				{operatortool.ReadAttachment, map[string]any{"project_id": "prj_foreign"}, operatortool.ErrAccessDenied},
				{operatortool.ReadAttachmentMetadata, map[string]any{"project_id": "prj_foreign"}, operatortool.ErrAccessDenied},
				{operatortool.ReadAttachment, map[string]any{"attachment_id": "att_missing"}, operatortool.ErrInvalidArguments},
				{operatortool.ReadAttachment, map[string]any{"attachment_id": "att_" + strings.Repeat("0", 32)}, operatortool.ErrAccessDenied},
				{operatortool.ReadAttachmentMetadata, map[string]any{"attachment_id": "att_" + strings.Repeat("0", 32)}, operatortool.ErrAccessDenied},
				{operatortool.DeleteAttachment, map[string]any{}, operatortool.ErrUnknownTool},
				{operatortool.ReferenceAttachment, map[string]any{}, operatortool.ErrUnknownTool},
			} {
				arguments := map[string]any{"project_id": o.project, "attachment_id": inputAttachment.ID}
				for field, value := range refusal.args {
					arguments[field] = value
				}
				raw, err := json.Marshal(arguments)
				if err != nil {
					t.Fatal(err)
				}
				before := len(store.requests())
				result, err := handler(t.Context(), runner.AgentToolCall{Name: refusal.name, Arguments: raw})
				if !errors.Is(err, refusal.want) || result.Success || len(store.requests()) != before {
					t.Fatalf("worker input refusal %s %s: %+v, %v, want %v", refusal.name, raw, result, err, refusal.want)
				}
			}
			canonical := request(http.MethodGet, "/api/v2/organizations/"+o.id+"/projects/"+o.project+"/attachments/"+inputAttachment.ID+"?length=1", nil, map[string]string{"Authorization": "Bearer " + credential})
			var canonicalContent operatortool.AttachmentContentResult
			if canonical.status != http.StatusOK || json.Unmarshal([]byte(canonical.body), &canonicalContent) != nil || canonicalContent.ContentBase64 != base64.StdEncoding.EncodeToString([]byte(inputContent[:1])) {
				t.Fatalf("canonical worker input: %+v", canonical)
			}
			for _, bounds := range []struct {
				query string
				want  int
			}{{"?offset=invalid", http.StatusBadRequest}, {"?offset=-1", http.StatusBadRequest}, {"?length=1.5", http.StatusBadRequest}, {"?length=32769", http.StatusBadRequest}, {"?length=1&length=2", http.StatusNotFound}} {
				before := len(store.requests())
				read := request(http.MethodGet, inputPath+"/"+inputAttachment.ID+bounds.query, nil, map[string]string{"Authorization": "Bearer " + credential})
				if read.status != bounds.want || len(store.requests()) != before {
					t.Fatalf("invalid worker read bounds %s: %+v", bounds.query, read)
				}
			}
			inputKey, err := attachment.Key(o.id, inputAttachment.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, unavailable := range []struct {
				missing bool
				want    error
			}{{want: operatortool.ErrReadUnavailable}, {missing: true, want: operatortool.ErrAccessDenied}} {
				store.mu.Lock()
				object := store.objects[inputKey]
				if unavailable.missing {
					delete(store.objects, inputKey)
				} else {
					store.objects[inputKey] = spacesObject{content: []byte("short"), created: object.created}
				}
				store.mu.Unlock()
				raw, err := json.Marshal(map[string]string{"project_id": o.project, "attachment_id": inputAttachment.ID})
				if err != nil {
					t.Fatal(err)
				}
				result, err := handler(t.Context(), runner.AgentToolCall{Name: operatortool.ReadAttachment, Arguments: raw})
				store.mu.Lock()
				store.objects[inputKey] = object
				store.mu.Unlock()
				if !errors.Is(err, unavailable.want) || result.Success {
					t.Fatalf("unavailable worker input: %+v, %v", result, err)
				}
			}
			pilotStatus(t, "delete human input", request(http.MethodDelete, inputPath+"/"+inputAttachment.ID, nil, map[string]string{"X-CSRF-Token": o.ownerCSRF}), http.StatusNoContent)
			for _, name := range []string{operatortool.ReadAttachment, operatortool.ReadAttachmentMetadata} {
				raw, err := json.Marshal(map[string]string{"project_id": o.project, "attachment_id": inputAttachment.ID})
				if err != nil {
					t.Fatal(err)
				}
				result, err := handler(t.Context(), runner.AgentToolCall{Name: name, Arguments: raw})
				if !errors.Is(err, operatortool.ErrAccessDenied) || result.Success {
					t.Fatalf("deleted worker input: %+v, %v", result, err)
				}
			}
			var uploads []attachment.Metadata
			for i := range 12 {
				path := "page.png"
				if index == 1 && i < 3 {
					path = "large.log"
				}
				args, err := json.Marshal(map[string]string{"path": path, "caption": fmt.Sprintf("Page state %d rendered correctly", i)})
				if err != nil {
					t.Fatal(err)
				}
				call := runner.AgentToolCall{Name: "attach_evidence", Arguments: args}
				result, err := handler(t.Context(), call)
				var uploaded attachment.Metadata
				if err != nil || !result.Success || json.Unmarshal([]byte(result.Content), &uploaded) != nil || uploaded.Reference != uploaded.Markdown(o.id) {
					t.Fatalf("upload %d: %+v, %v", i, result, err)
				}
				uploads = append(uploads, uploaded)
				replay, err := handler(t.Context(), call)
				var repeated attachment.Metadata
				if err != nil || json.Unmarshal([]byte(replay.Content), &repeated) != nil || repeated.ID != uploaded.ID || repeated.Reference != uploaded.Reference {
					t.Fatalf("upload replay: %+v, %v", replay, err)
				}
			}
			call := runner.AgentToolCall{Name: "attach_evidence", Arguments: json.RawMessage(`{"path":"page.png","caption":"Page rendered correctly"}`)}
			var result runner.AgentToolResult
			var indexUpload attachment.Metadata
			_, err = backend.RunTurnWithTools(t.Context(), runner.AgentTurnRequest{Workspace: workspace, TempDir: t.TempDir(), Prompt: "Publish the page for human design review."}, tools, func(ctx context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
				response, callErr := handler(ctx, call)
				if response.Success && strings.Contains(string(call.Arguments), "index.txt") {
					if decodeErr := json.Unmarshal([]byte(response.Content), &indexUpload); decodeErr != nil {
						return runner.AgentToolResult{}, decodeErr
					}
				} else {
					result = response
				}
				return response, callErr
			}, nil)
			var uploaded attachment.Metadata
			if err != nil || !result.Success || json.Unmarshal([]byte(result.Content), &uploaded) != nil || uploaded.Reference != uploaded.Markdown(o.id) {
				t.Fatalf("tool: %+v, %v", result, err)
			}
			for _, refusal := range []struct {
				path, message string
			}{
				{"oversized.log", "20 MiB"},
				{"large.log", "collaboration_bytes"},
			} {
				if refusal.path == "large.log" && index == 0 {
					continue
				}
				args, err := json.Marshal(map[string]string{"path": refusal.path, "caption": "Must refuse this upload"})
				if err != nil {
					t.Fatal(err)
				}
				result, err := handler(t.Context(), runner.AgentToolCall{Name: "attach_evidence", Arguments: args})
				if err == nil || result.Success || !strings.Contains(result.Content, refusal.message) {
					t.Fatalf("%s refusal: %+v, %v", refusal.path, result, err)
				}
			}
			published := append(slices.Clone(uploads), uploaded, indexUpload)
			native, ok := scheduler.NativeClient("local")
			if !ok {
				t.Fatal("native client unavailable")
			}
			pending, err := native.RuntimeEvidence(t.Context(), issue.WorkItemID, "")
			if err != nil || pending.Attempt == nil || len(pending.Attempt.Evidence) != len(published) || pending.Attempt.Evidence[0].AttachmentID != uploads[0].ID {
				t.Fatalf("authenticated upload unavailable before finish: %+v, %v", pending.Attempt, err)
			}
			allComments := func() []tracker.NativeComment {
				t.Helper()
				var comments []tracker.NativeComment
				cursor := ""
				for {
					page, err := native.Comments(t.Context(), issue.WorkItemID, cursor)
					if err != nil {
						t.Fatal(err)
					}
					comments = append(comments, page.Items...)
					if page.NextCursor == "" {
						return comments
					}
					cursor = page.NextCursor
				}
			}
			before := allComments()
			if len(before) != len(published)+1 {
				t.Fatalf("published upload comments: got %d", len(before))
			}
			for _, design := range []struct {
				file    attachment.Metadata
				caption string
			}{{uploaded, "Page rendered correctly"}, {indexUpload, "Screen and state index; awaiting human approval"}} {
				if design.file.Reference == "" || !slices.ContainsFunc(before, func(comment tracker.NativeComment) bool {
					return strings.Contains(comment.Body, design.file.Reference) && strings.Contains(comment.Body, design.caption)
				}) {
					t.Fatalf("design evidence was not published immediately: %+v", design)
				}
			}
			for _, file := range previousUploads {
				for _, comment := range before {
					if strings.Contains(comment.Body, file.ID) {
						t.Fatal("another item's evidence leaked into comments")
					}
				}
				for _, fileHere := range pending.Attempt.Evidence {
					if fileHere.AttachmentID == file.ID {
						t.Fatal("another attempt's evidence leaked into runtime")
					}
				}
			}
			lease := execution.Recovery().Lease
			for _, refusal := range []struct {
				name, attempt string
				item          tracker.NativeWorkItemID
				fence         tracker.FencingToken
				want          int
			}{
				{"wrong fence", pending.Attempt.AttemptID, issue.WorkItemID, lease.FencingToken + 1, http.StatusConflict},
				{"wrong attempt", "attempt_missing", issue.WorkItemID, lease.FencingToken, http.StatusConflict},
				{"cross item", pending.Attempt.AttemptID, previousIssue.WorkItemID, lease.FencingToken, http.StatusNotFound},
				{"cross attempt", previousAttempt, issue.WorkItemID, lease.FencingToken, http.StatusConflict},
			} {
				if refusal.item == "" || refusal.attempt == "" {
					continue
				}
				authority, err := json.Marshal(attachment.EvidenceRequest{Mutation: tracker.Mutation{IdempotencyKey: refusal.name, LeaseID: lease.ID, FencingToken: refusal.fence}, AttemptID: refusal.attempt, WorkItemID: refusal.item, Caption: "Rejected evidence"})
				if err != nil {
					t.Fatal(err)
				}
				response := request(http.MethodPost, o.projectAPI()+"/attempts/"+refusal.attempt+"/evidence", bytes.NewReader(pixels.Bytes()), map[string]string{"Authorization": "Bearer " + credential, "Content-Type": "image/png", "X-Attachment-Name": "page.png", "Idempotency-Key": refusal.name, "X-Detent-Evidence": base64.RawURLEncoding.EncodeToString(authority)})
				if response.status != refusal.want {
					t.Fatalf("%s: %+v", refusal.name, response)
				}
			}
			_, organizationPage := owner.get("/organizations/" + o.id + "/organization")
			other := owner.do(http.MethodPost, "/organizations/"+o.id+"/projects", url.Values{"name": {fmt.Sprintf("Other evidence project %d", index)}, "grant_access": {"true"}, "csrf": {csrfFrom(t, organizationPage)}}, nil)
			if other.StatusCode != http.StatusSeeOther {
				t.Fatalf("other project: %d", other.StatusCode)
			}
			foreignProject := strings.TrimPrefix(other.Header.Get("Location"), "/organizations/"+o.id+"/projects/")
			for _, test := range []struct {
				name, project, token string
				fence                tracker.FencingToken
				status               int
			}{
				{"cross-project", foreignProject, credential, lease.FencingToken, http.StatusNotFound},
				{"unauthorized", o.project, "invalid-worker-token", lease.FencingToken, http.StatusUnauthorized},
			} {
				authority := attachment.EvidenceRequest{Mutation: tracker.Mutation{IdempotencyKey: test.name, LeaseID: lease.ID, FencingToken: test.fence}, AttemptID: pending.Attempt.AttemptID, WorkItemID: issue.WorkItemID, Caption: "Refused evidence"}
				raw, err := json.Marshal(authority)
				if err != nil {
					t.Fatal(err)
				}
				refusal := request(http.MethodPost, "/organizations/"+o.id+"/api/v2/projects/"+test.project+"/attempts/"+authority.AttemptID+"/evidence", bytes.NewReader(pixels.Bytes()), map[string]string{"Authorization": "Bearer " + test.token, "Content-Type": "image/png", "X-Attachment-Name": "page.png", "Idempotency-Key": test.name, "X-Detent-Evidence": base64.RawURLEncoding.EncodeToString(raw)})
				if refusal.status != test.status {
					t.Fatalf("%s publication refusal: %+v", test.name, refusal)
				}
			}
			if err := execution.Checkpoint(t.Context(), tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "dirty", ExternalEffect: "none", EffectState: "none"}); err != nil {
				t.Fatal(err)
			}
			if completion != "" {
				if err := execution.(runner.CompletionExecution).PrepareFinish(t.Context(), "failed", completion, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := execution.Finish(t.Context(), "failed"); err != nil {
				t.Fatal(err)
			}
			refused, refusalErr := handler(t.Context(), call)
			if refusalErr == nil || refused.Success || strings.Contains(refused.Content, uploaded.Reference) {
				t.Fatalf("finished worker retained publication authority: %+v, %v", refused, refusalErr)
			}
			if err := execution.Finish(t.Context(), "failed"); err != nil {
				t.Fatal(err)
			}
			comments := allComments()
			wantComments := len(published) + 1
			if completion != "" {
				wantComments++
			}
			if len(comments) != wantComments {
				t.Fatalf("completion comments: got %d, want %d", len(comments), wantComments)
			}
			if completion != "" && comments[len(comments)-1].Body != completion {
				t.Fatalf("completion response was changed: length=%d, want %d", len(comments[len(comments)-1].Body), len(completion))
			}
			for i, file := range uploads {
				found := 0
				for _, comment := range comments {
					if strings.Contains(comment.Body, file.Reference) && strings.Contains(comment.Body, fmt.Sprintf("Page state %d rendered correctly", i)) {
						found++
					}
				}
				if found != 1 {
					t.Fatalf("evidence %d has %d published references", i, found)
				}
			}
			attempts, err := native.Attempts(t.Context(), issue.WorkItemID, "")
			if err != nil || len(attempts.Items) != 1 || len(attempts.Items[0].Evidence) != len(published) || attempts.Items[0].Evidence[0].AttachmentID != uploads[0].ID {
				t.Fatalf("attempt evidence: %+v, %v", attempts, err)
			}
			for _, file := range published {
				found := false
				for _, evidence := range attempts.Items[0].Evidence {
					if evidence.AttachmentID == file.ID && evidence.Reference == file.Reference {
						found = true
					}
				}
				if !found {
					t.Fatalf("attempt lost attachment %s", file.ID)
				}
			}
			read := request(http.MethodGet, strings.TrimSuffix(uploaded.Reference, ")")[strings.Index(uploaded.Reference, "(")+1:], nil, nil)
			if read.status != http.StatusOK || !bytes.Equal([]byte(read.body), pixels.Bytes()) {
				t.Fatalf("operator attachment read: %+v", read)
			}
			readIndex := request(http.MethodGet, strings.TrimSuffix(indexUpload.Reference, ")")[strings.Index(indexUpload.Reference, "(")+1:], nil, nil)
			if readIndex.status != http.StatusOK || readIndex.body != indexContent {
				t.Fatal("screen/state index was not retrievable")
			}
			current, err := native.Issue(t.Context(), issue.WorkItemID)
			if err != nil || current.State != issue.State {
				t.Fatalf("publication changed the workflow state: %+v, %v", current, err)
			}
			lease = execution.Recovery().Lease
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
			if objects != (index+1)*14 {
				t.Fatalf("replay or stale upload created extra storage objects: %d", objects)
			}
			previousIssue, previousAttempt, previousUploads = issue, pending.Attempt.AttemptID, published
		})
	}
}

func TestClaudeEvidenceWorkerProcess(t *testing.T) {
	if os.Getenv("DETENT_CLAUDE_EVIDENCE_HELPER") != "1" {
		return
	}
	var settings struct {
		Sandbox struct {
			Enabled bool `json:"enabled"`
			Network struct {
				AllowedDomains    []string `json:"allowedDomains"`
				AllowLocalBinding bool     `json:"allowLocalBinding"`
				StrictAllowlist   bool     `json:"strictAllowlist"`
			} `json:"network"`
		} `json:"sandbox"`
	}
	var effective json.RawMessage
	var config struct {
		Servers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	for i, arg := range os.Args {
		if arg == "--mcp-config" && i+1 < len(os.Args) {
			raw, err := os.ReadFile(os.Args[i+1])
			if err != nil || json.Unmarshal(raw, &config) != nil {
				t.Fatalf("MCP configuration: %v", err)
			}
		}
		if arg == "--settings" && i+1 < len(os.Args) {
			effective = json.RawMessage(os.Args[i+1])
			if err := json.Unmarshal(effective, &settings); err != nil {
				t.Fatal(err)
			}
		}
	}
	server, ok := config.Servers["detent"]
	if !ok || len(config.Servers) != 1 || server.Type != "http" {
		t.Fatal("Claude did not receive the scoped HTTP MCP server")
	}
	if settings.Sandbox.Enabled {
		address, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		network := settings.Sandbox.Network
		if !network.StrictAllowlist || network.AllowLocalBinding || !slices.Contains(network.AllowedDomains, address.Host) {
			t.Fatal("sandbox did not authorize the worker MCP endpoint")
		}
		if len(network.AllowedDomains) != len(isolation.Domains())+1 {
			t.Fatal("sandbox gained network access beyond the worker MCP endpoint")
		}
		decoder := json.NewDecoder(os.Stdin)
		for _, subtype := range []string{"initialize", "get_settings"} {
			var control struct {
				Type      string `json:"type"`
				RequestID string `json:"request_id"`
				Request   struct {
					Subtype string `json:"subtype"`
				} `json:"request"`
			}
			if err := decoder.Decode(&control); err != nil || control.Type != "control_request" || control.Request.Subtype != subtype {
				t.Fatalf("sandbox handshake %s: %+v, %v", subtype, control, err)
			}
			response := map[string]any{}
			if subtype == "get_settings" {
				response["effective"] = effective
			}
			if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": control.RequestID, "response": response}}); err != nil {
				t.Fatal(err)
			}
		}
		var prompt struct {
			Type string `json:"type"`
		}
		if err := decoder.Decode(&prompt); err != nil || prompt.Type != "user" {
			t.Fatalf("sandbox prompt: %+v, %v", prompt, err)
		}
	} else if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	session := ""
	request := func(body string, authorized bool) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if authorized {
			for name, value := range server.Headers {
				req.Header.Set(name, value)
			}
		}
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(response.Body)
		if closeErr := response.Body.Close(); err != nil || closeErr != nil {
			t.Fatal(errors.Join(err, closeErr))
		}
		if id := response.Header.Get("Mcp-Session-Id"); id != "" {
			session = id
		}
		return response.StatusCode, content
	}
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"claude-fixture","version":"test"}}}`
	if status, _ := request(initialize, false); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated connection: %d", status)
	}
	if status, body := request(initialize, true); status != http.StatusOK || session == "" {
		t.Fatalf("initialize: %d %s", status, body)
	}
	if status, body := request(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, true); status != http.StatusAccepted {
		t.Fatalf("initialized: %d %s", status, body)
	}
	if status, body := request(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, true); status != http.StatusOK || !bytes.Contains(body, []byte(`"name":"attach_evidence"`)) || bytes.Contains(body, []byte(`"name":"delete_attachment"`)) {
		t.Fatalf("worker catalog: %d %s", status, body)
	}
	for _, test := range []struct {
		name, arguments string
		refused         bool
	}{
		{"delete_attachment", `{}`, true},
		{"attach_evidence", `{"path":"page.png","caption":"Page rendered correctly","project_id":"prj_foreign"}`, true},
		{"attach_evidence", `{"path":"page.png","caption":"Page rendered correctly"}`, false},
		{"attach_evidence", `{"path":"index.txt","caption":"Screen and state index; awaiting human approval"}`, false},
	} {
		raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": test.name, "arguments": json.RawMessage(test.arguments)}})
		if err != nil {
			t.Fatal(err)
		}
		status, body := request(string(raw), true)
		var response struct {
			Error  json.RawMessage `json:"error"`
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if status != http.StatusOK || json.Unmarshal(body, &response) != nil || (len(response.Error) > 0 || response.Result.IsError) != test.refused {
			t.Fatalf("worker call %s: %d %s", test.name, status, body)
		}
		if !test.refused && (len(response.Result.Content) != 1 || !strings.Contains(response.Result.Content[0].Text, "/attachments/att_")) {
			t.Fatalf("worker did not receive durable attachment reference: %s", body)
		}
	}
	fmt.Fprintln(os.Stdout, `{"type":"system","subtype":"init","session_id":"claude-evidence","model":"fable"}`)
	fmt.Fprintln(os.Stdout, `{"type":"result","subtype":"success","session_id":"claude-evidence"}`)
	os.Exit(0)
}
