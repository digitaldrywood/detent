package runner

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestValidatorReadsPublishedAttachments(t *testing.T) {
	t.Parallel()
	const encoded = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+j4c8AAAAASUVORK5CYII="
	image, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(image)
	metadata := attachment.Metadata{ID: "att_0123456789abcdef0123456789abcdef", ProjectID: "prj_example", WorkItemID: "wi_example", CommentID: "cmt_example", Name: "page.png", ContentType: "image/png", Size: int64(len(image)), SHA256: hex.EncodeToString(digest[:]), Width: 1, Height: 1}
	for _, test := range []struct {
		name        string
		corrupt     bool
		denied      bool
		unsupported bool
		text        bool
		json        bool
		offset      int64
		invalidUTF8 bool
		oversized   bool
		wrongReview bool
		textBytes   int
		truncated   bool
		splitRune   bool
	}{
		{name: "published image read across pages"},
		{name: "digest mismatch", corrupt: true},
		{name: "later read denied", denied: true},
		{name: "backend cannot display images", unsupported: true},
		{name: "bootstrap installation partial recovery and full rerun text log", text: true, unsupported: true},
		{name: "small JSON needs supported publication", text: true, json: true},
		{name: "JSON chunk still needs supported publication", text: true, json: true, offset: 16},
		{name: "text digest mismatch", text: true, corrupt: true},
		{name: "partial text read denied", text: true, denied: true},
		{name: "unread text prefix", text: true, offset: 16},
		{name: "invalid text encoding", text: true, invalidUTF8: true},
		{name: "text read exceeds requested byte bound", text: true, oversized: true},
		{name: "text receipt from another review", text: true, wrongReview: true},
		{name: "text at display bound", text: true, textBytes: operatortool.AttachmentContentBytes},
		{name: "text over display bound", text: true, textBytes: operatortool.AttachmentContentBytes + 1, truncated: true},
		{name: "display bound does not split a UTF-8 rune", text: true, truncated: true, splitRune: true},
		{name: "multi-megabyte text is bounded without observation receipt", text: true, textBytes: 2 << 20, truncated: true},
		{name: "omitted text still participates in digest verification", text: true, textBytes: 2 << 20, truncated: true, corrupt: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			content := image
			metadata := metadata
			if test.text {
				content = []byte("bootstrap source_sha256=example-source-digest\npsql installed\npartial installation recovered\nfull bootstrap succeeded\nfull rerun succeeded\n")
				if test.textBytes > 0 {
					content = []byte(strings.Repeat("x", test.textBytes))
				}
				if test.splitRune {
					content = []byte(strings.Repeat("x", operatortool.AttachmentContentBytes-1) + "é")
				}
				if test.invalidUTF8 {
					content[0] = 0xff
				}
				if test.json {
					content = []byte(`{"source":"baseline-head","command":"go test -run TestExample","output":"failed"}`)
				}
				digest := sha256.Sum256(content)
				metadata.Name, metadata.ContentType = "bootstrap.log", "text/plain"
				if test.json {
					metadata.Name, metadata.ContentType = "diagnostic.json", "application/json"
				}
				metadata.Size, metadata.SHA256 = int64(len(content)), hex.EncodeToString(digest[:])
				metadata.Width, metadata.Height = 0, 0
			}
			calls := 0
			reader := &validatorAttachments{head: "review-head", tree: "review-tree", images: !test.unsupported}
			if test.wrongReview {
				reader.head = "other-review-head"
			}
			reader.handle = func(_ context.Context, call AgentToolCall) (AgentToolResult, error) {
				calls++
				input, err := operatortool.DecodeAttachmentOperation(call.Name, call.Arguments)
				if err != nil {
					t.Fatal(err)
				}
				if test.denied && calls > 1 {
					return AgentToolResult{}, errors.New("attachment access denied")
				}
				end := min(int(input.Offset)+input.Length, len(content))
				if test.oversized {
					end = min(end+1, len(content))
				}
				chunk := slices.Clone(content[input.Offset:end])
				if test.corrupt && calls > 1 && (!test.truncated || end == len(content)) {
					chunk[0] ^= 1
				}
				data, err := json.Marshal(operatortool.AttachmentContentResult{Metadata: metadata, ContentBase64: base64.StdEncoding.EncodeToString(chunk), Offset: input.Offset, ReturnedBytes: len(chunk), EOF: end == len(content), MaxContentBytes: operatortool.AttachmentContentBytes})
				if err != nil {
					t.Fatal(err)
				}
				return AgentToolResult{Success: true, Content: string(data)}, nil
			}
			args, err := json.Marshal(operatortool.AttachmentOperationArguments{ProjectID: metadata.ProjectID, AttachmentID: metadata.ID, Offset: test.offset, Length: 16})
			if err != nil {
				t.Fatal(err)
			}
			result, err := reader.call(t.Context(), AgentToolCall{Name: operatortool.ReadAttachment, Arguments: args})
			if test.denied || test.corrupt || test.invalidUTF8 || test.oversized {
				if err == nil || result.ImageURL != "" || len(reader.evidence()) != 0 {
					t.Fatalf("unreadable attachment supplied evidence: err=%v reads=%v", err, reader.evidence())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.offset != 0 && !test.json || test.unsupported && !test.text {
				if calls != 1 || result.ImageURL != "" || len(reader.evidence()) != 0 {
					t.Fatal("text-only backend claimed visual evidence")
				}
				return
			}
			reads := reader.evidence()
			if test.json {
				var observed struct {
					Metadata        attachment.Metadata  `json:"metadata"`
					InspectionError string               `json:"inspection_error"`
					ReadReceipt     *gate.AttachmentRead `json:"read_receipt"`
				}
				if err := json.Unmarshal([]byte(result.Content), &observed); err != nil {
					t.Fatal(err)
				}
				if !result.Success || calls != 1 || len(reads) != 0 || observed.ReadReceipt != nil || result.ImageURL != "" || !reflect.DeepEqual(observed.Metadata, metadata) || len(result.Content) > 4096 || strings.Contains(result.Content, "content_base64") {
					t.Fatalf("unsupported JSON supplied inspection evidence or lost metadata: %+v", result)
				}
				for _, required := range []string{"application/json", "text/plain", "32768", "ordered", "SHA-256", "no read_receipt"} {
					if !strings.Contains(observed.InspectionError, required) {
						t.Fatalf("unsupported JSON lacks actionable guidance %q: %s", required, observed.InspectionError)
					}
				}
				return
			}
			if test.truncated {
				var observed struct {
					Metadata        attachment.Metadata  `json:"metadata"`
					Content         string               `json:"content"`
					Truncated       bool                 `json:"truncated"`
					ReadReceipt     *gate.AttachmentRead `json:"read_receipt"`
					InspectionError string               `json:"inspection_error"`
				}
				if err := json.Unmarshal([]byte(result.Content), &observed); err != nil {
					t.Fatal(err)
				}
				wantBytes := operatortool.AttachmentContentBytes
				if test.splitRune {
					wantBytes--
				}
				if !observed.Truncated || len(observed.Content) != wantBytes || observed.Content != string(content[:wantBytes]) || len(result.Content) > operatortool.AttachmentContentBytes+4096 || observed.Metadata.SHA256 != metadata.SHA256 || observed.ReadReceipt != nil || len(reads) != 0 || result.ImageURL != "" {
					t.Fatalf("truncated text was unbounded or supplied evidence: bytes=%d truncated=%v reads=%v", len(result.Content), observed.Truncated, reads)
				}
				for _, required := range []string{"32768", "text/plain", "ordered", "byte ranges", "no read_receipt"} {
					if !strings.Contains(observed.InspectionError, required) {
						t.Fatalf("truncated text lacks actionable guidance %q: %s", required, observed.InspectionError)
					}
				}
				validation := gate.ValidatorResult{HeadSHA: "review-head", AttachmentReads: reads, CriteriaEvidence: []gate.CriterionEvidence{{Criterion: "Full log inspected", Kind: gate.EvidenceAttachment, Reference: metadata.Name, ReceiptIDs: []string{"attachment:" + metadata.ID + ":" + metadata.SHA256}, HeadSHA: "review-head", TreeSHA: "review-tree", Behavior: "Claimed complete log inspection"}}}
				gate.NormalizeCriterionEvidence(&validation, []string{"Full log inspected"}, "review-tree")
				if len(validation.NotVerified) != 1 {
					t.Fatal("omitted text incorrectly accepted as fully inspected")
				}
				return
			}
			if calls != 2 || len(reads) != 1 || reads[0].SHA256 != metadata.SHA256 || reads[0].CommentID != metadata.CommentID || !strings.Contains(result.Content, reads[0].ReceiptID) {
				t.Fatalf("published attachment not supplied with source-bound read receipt: calls=%d reads=%v", calls, reads)
			}
			if test.text {
				var observed struct {
					Content string `json:"content"`
				}
				if json.Unmarshal([]byte(result.Content), &observed) != nil || observed.Content != string(content) || result.ImageURL != "" {
					t.Fatal("text log was not displayed without an image")
				}
			} else if result.ImageURL != "data:image/png;base64,"+encoded {
				t.Fatal("image was not displayed")
			}
			if strings.Contains(result.Content, "content_base64") || strings.Contains(result.Content, encoded[:16]) {
				t.Fatal("image payload duplicated into text context")
			}
			criterion, behavior := "Page is visible", "The independently viewed capture shows the required page"
			if test.text {
				criterion = "Exercise bootstrap including reruns after partial installation"
				behavior = "The inspected log records installation, partial recovery and a full rerun. Its source digest matches the reviewed script; this is retained execution evidence, not independent execution."
			}
			validation := gate.ValidatorResult{Submitted: true, Verdict: gate.ValidatorVerdictPass, Score: 1, HeadSHA: "review-head", AttachmentReads: reads, CriteriaEvidence: []gate.CriterionEvidence{{Criterion: criterion, Kind: gate.EvidenceAttachment, Reference: metadata.Name + " sha256=" + metadata.SHA256, ReceiptIDs: []string{reads[0].ReceiptID}, HeadSHA: "review-head", TreeSHA: "review-tree", Behavior: behavior}}}
			gate.NormalizeCriterionEvidence(&validation, []string{criterion}, "review-tree")
			if (len(validation.NotVerified) != 0) != test.wrongReview {
				t.Fatalf("attachment source applicability not preserved: %+v", validation)
			}
			decision, applied := gate.EvaluateValidator(gate.ValidatorConfig{Enabled: true, UnverifiedCriteria: gate.UnverifiedCriteriaRework}, validation)
			if applied != test.wrongReview || (decision.Action == gate.ActionRework) != test.wrongReview {
				t.Fatalf("attachment gate decision: %+v, applied=%v", decision, applied)
			}
		})
	}
}

type attachmentValidatorBackend struct {
	fakeCodexClient
	t       *testing.T
	text    bool
	unread  bool
	corrupt bool
}

func (b *attachmentValidatorBackend) RunTurnWithTools(ctx context.Context, req AgentTurnRequest, tools []AgentTool, handle AgentToolHandler, update AgentUpdateHandler) (AgentTurnResult, error) {
	b.t.Helper()
	if !strings.Contains(req.Prompt, "att_current_attempt") || !strings.Contains(req.Prompt, "Uploaded before completion") {
		b.t.Fatal("current attempt evidence missing before its completion comment")
	}
	if !req.ReadOnly || !strings.Contains(req.Prompt, "project_id=prj_example reference=wi_example") || !strings.Contains(req.Prompt, "ordinary comments") {
		b.t.Fatalf("validator cannot discover native evidence: readonly=%v", req.ReadOnly)
	}
	if slices.ContainsFunc(tools, func(tool AgentTool) bool { return tool.Name == "attach_evidence" }) {
		b.t.Fatal("validator received publication permission")
	}
	result, err := handle(ctx, AgentToolCall{Name: operatortool.WorkComments, Arguments: json.RawMessage(`{"project_id":"prj_example","reference":"wi_example","limit":20}`)})
	if err != nil || !result.Success || result.Content != "ordinary published evidence" {
		b.t.Fatalf("validator evidence read failed: %v %+v", err, result)
	}
	if b.text {
		if !slices.ContainsFunc(tools, func(tool AgentTool) bool { return tool.Name == operatortool.ReadAttachment }) {
			b.t.Fatal("validator lost execution attachment tools across worker transport")
		}
		var read gate.AttachmentRead
		if !b.unread {
			result, err = handle(ctx, AgentToolCall{Name: operatortool.ReadAttachment, Arguments: json.RawMessage(`{"project_id":"prj_example","attachment_id":"att_0123456789abcdef0123456789abcdef","offset":0,"length":32768}`)})
			if b.corrupt {
				if err == nil || result.Success {
					b.t.Fatal("digest mismatch accepted through worker transport")
				}
			} else {
				var observed struct {
					ReadReceipt gate.AttachmentRead `json:"read_receipt"`
					Content     string              `json:"content"`
				}
				if err != nil || !result.Success || json.Unmarshal([]byte(result.Content), &observed) != nil || observed.Content != validatorHistoricalLog || result.ImageURL != "" {
					b.t.Fatalf("text log not displayed through worker transport: %v %+v", err, result)
				}
				read = observed.ReadReceipt
				if read.ReceiptID == "" || read.HeadSHA != "head" || read.TreeSHA != "tree" {
					b.t.Fatalf("text log missing review-bound receipt: %+v", read)
				}
			}
		}
		verdict := gate.ValidatorResult{Verdict: gate.ValidatorVerdictPass, Score: 1, Summary: "Inspected historical reproduction"}
		if read.ReceiptID == "" {
			read = gate.AttachmentRead{ReceiptID: "attachment:invented", HeadSHA: "head", TreeSHA: "tree"}
		}
		verdict.CriteriaEvidence = []gate.CriterionEvidence{{Criterion: "Historical failure is reproduced", Kind: gate.EvidenceAttachment, Reference: "baseline.log sha256=" + read.SHA256, ReceiptIDs: []string{read.ReceiptID}, HeadSHA: read.HeadSHA, TreeSHA: read.TreeSHA, Behavior: "The log records the baseline command failing with host_capacity at source baseline-head. The reviewed fix changes the implicated accounting; historical diagnosis remains applicable, without claiming execution at this review head."}}
		data, err := json.Marshal(verdict)
		if err != nil {
			return AgentTurnResult{}, err
		}
		return AgentTurnResult{}, update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: string(data)})
	}
	err = update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: `{"verdict":"rework","score":0.5,"summary":"Reviewed ordinary comment; image still required","criteria_evidence":[{"criterion":"Page is visible","kind":"not_verified","reference":"Image needed"}]}`})
	return AgentTurnResult{}, err
}

type attachmentValidatorWorkspace struct{ reviewMutationWorkspace }

func (*attachmentValidatorWorkspace) RunReviewCommand(_ context.Context, _ workspace.Info, issue workspace.Issue, command string) (gate.CommandResult, error) {
	return gate.CommandResult{Command: command, HeadSHA: issue.PullRequestHeadSHA, TreeSHA: "tree"}, nil
}

const validatorHistoricalLog = "source=baseline-head\ngo test -run TestBatchClaim\nphysical host slots=1\nclaim failed: 409 host_capacity\n"

type attachmentValidatorExecution struct {
	corrupt bool
	testExecution
	result gate.ValidatorResult
}

func (*attachmentValidatorExecution) ValidatorVersion(context.Context) (NativeValidation, error) {
	return NativeValidation{
		Evidence: []tracker.NativeEvidence{{AttachmentID: "att_current_attempt", Caption: "Uploaded before completion"}},
		Version:  &tracker.ChangeVersion{ID: "version_example", ChangeVersionInput: tracker.ChangeVersionInput{Repository: "example-org/example-project", HeadSHA: "head", BaseSHA: "base", Validation: &gate.CommandResult{TreeSHA: "tree"}}, Policy: policy.Descriptor{Gates: policy.Gates{Validator: true}}},
		Diff:     &connector.ValidationDiff{Repository: "example-org/example-project", HeadSHA: "head", BaseSHA: "base", Digest: "digest", Patch: "diff --git a/page.go b/page.go\n+page\n"},
	}, nil
}

func (e *attachmentValidatorExecution) RecordValidator(_ context.Context, result gate.ValidatorResult) error {
	e.result = result
	return nil
}

func (e *attachmentValidatorExecution) AgentTools() ([]AgentTool, AgentToolHandler) {
	return []AgentTool{{Name: operatortool.ReadAttachment, InputSchema: json.RawMessage(`{"type":"object"}`)}, {Name: operatortool.WorkComments, InputSchema: json.RawMessage(`{"type":"object"}`)}, {Name: "attach_evidence", InputSchema: json.RawMessage(`{"type":"object"}`)}}, func(_ context.Context, call AgentToolCall) (AgentToolResult, error) {
		if call.Name == operatortool.ReadAttachment {
			content := []byte(validatorHistoricalLog)
			digest := sha256.Sum256(content)
			metadata := attachment.Metadata{ID: "att_0123456789abcdef0123456789abcdef", ProjectID: "prj_example", WorkItemID: "wi_example", CommentID: "cmt_example", Name: "baseline.log", ContentType: "text/plain", Size: int64(len(content)), SHA256: hex.EncodeToString(digest[:])}
			if e.corrupt {
				content[0] = 'x'
			}
			data, err := json.Marshal(operatortool.AttachmentContentResult{Metadata: metadata, ContentBase64: base64.StdEncoding.EncodeToString(content), ReturnedBytes: len(content), EOF: true, MaxContentBytes: operatortool.AttachmentContentBytes})
			return AgentToolResult{Success: err == nil, Content: string(data)}, err
		}
		if call.Name != operatortool.WorkComments {
			return AgentToolResult{}, errors.New("unexpected tool")
		}
		return AgentToolResult{Success: true, Content: "ordinary published evidence"}, nil
	}
}

func TestNativeValidatorReceivesScopedEvidenceReads(t *testing.T) {
	t.Parallel()
	for _, worker := range []bool{false, true} {
		for _, test := range []struct {
			name                  string
			text, unread, corrupt bool
		}{
			{name: "unseen image"},
			{name: "complete historical log", text: true},
			{name: "unread log", text: true, unread: true},
			{name: "mismatched digest", text: true, corrupt: true},
		} {
			t.Run(test.name+"/worker="+strconv.FormatBool(worker), func(t *testing.T) {
				t.Parallel()
				backend := &attachmentValidatorBackend{t: t, text: test.text, unread: test.unread, corrupt: test.corrupt}
				runner, err := NewRunner(Dependencies{
					Workflow:  config.Workflow{Config: config.Config{Gate: gate.Config{Validator: gate.ValidatorConfig{Enabled: true}}, Agents: config.Agents{Backends: []config.AgentBackend{{ID: "codex", Kind: "codex", Protocol: "app-server", Command: "codex app-server"}}, Routes: []config.AgentRoute{{Name: "validator", Role: RoleValidator, Backend: "codex", Model: "example-model"}}}}, Prompt: "Review"},
					Workspace: &attachmentValidatorWorkspace{reviewMutationWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}}, heads: []string{"head"}}}, AgentBackend: backend,
				})
				if err != nil {
					t.Fatal(err)
				}
				execution := &attachmentValidatorExecution{corrupt: test.corrupt}
				var reviewExecution Execution = execution
				if worker {
					left, right := net.Pipe()
					t.Cleanup(func() { left.Close(); right.Close() })
					central := NewSSHPeer(t.Context(), left, left, NewSSHCallbackHandler(RunRequest{Execution: execution}, nil, nil, nil))
					remote := NewSSHPeer(t.Context(), right, right, nil)
					t.Cleanup(central.Close)
					t.Cleanup(remote.Close)
					wire := NewSSHRunRequest(RunRequest{Execution: execution})
					wire.Native = true
					data, err := json.Marshal(wire)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(data, &wire); err != nil {
						t.Fatal(err)
					}
					reviewExecution = wire.Bind(t.Context(), remote).Execution
				}
				criterion := "Page is visible"
				if test.text {
					criterion = "Historical failure is reproduced"
				}
				err = runner.validateNativeChange(t.Context(), RunRequest{Execution: reviewExecution, Issue: connector.Issue{ID: "wi_example", Identifier: "prj_example#1", Metadata: map[string]string{"hub_project_id": "prj_example"}, Description: "## Acceptance Criteria\n- " + criterion}})
				if err != nil {
					t.Fatal(err)
				}
				want := gate.ValidatorVerdictRework
				if test.text && !test.unread && !test.corrupt {
					want = gate.ValidatorVerdictPass
				}
				if execution.result.Verdict != want || (len(execution.result.NotVerified) == 0) != (want == gate.ValidatorVerdictPass) {
					t.Fatalf("attachment acceptance=%+v, want %s", execution.result, want)
				}
				if (len(execution.result.AttachmentReads) == 1) != (want == gate.ValidatorVerdictPass) {
					t.Fatalf("host observation receipts=%+v, want %s", execution.result.AttachmentReads, want)
				}
			})
		}
	}
}

func TestValidatorEvidenceToolsExcludeWrites(t *testing.T) {
	t.Parallel()
	tools := validatorEvidenceTools([]AgentTool{{Name: operatortool.WorkItem}, {Name: operatortool.WorkComments}, {Name: operatortool.ReadAttachment}, {Name: operatortool.ReadAttachmentMetadata}, {Name: "attach_evidence"}, {Name: "move_item"}, {Name: operatortool.RunnerProjectDiagnostics}})
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	if !slices.Equal(names, []string{operatortool.WorkItem, operatortool.WorkComments, operatortool.ReadAttachment, operatortool.ReadAttachmentMetadata}) {
		t.Fatalf("validator tool access=%v", names)
	}
}
