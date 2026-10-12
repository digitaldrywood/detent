package runner

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
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

func TestValidatorReadsPublishedImages(t *testing.T) {
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
	}{
		{name: "published image read across pages"},
		{name: "digest mismatch", corrupt: true},
		{name: "later read denied", denied: true},
		{name: "backend cannot display images", unsupported: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			reader := &validatorAttachments{head: "review-head", tree: "review-tree", images: !test.unsupported}
			reader.handle = func(_ context.Context, call AgentToolCall) (AgentToolResult, error) {
				calls++
				input, err := operatortool.DecodeAttachmentOperation(call.Name, call.Arguments)
				if err != nil {
					t.Fatal(err)
				}
				if test.denied && calls > 1 {
					return AgentToolResult{}, errors.New("attachment access denied")
				}
				end := min(int(input.Offset)+input.Length, len(image))
				chunk := slices.Clone(image[input.Offset:end])
				if test.corrupt && calls > 1 {
					chunk[0] ^= 1
				}
				data, err := json.Marshal(operatortool.AttachmentContentResult{Metadata: metadata, ContentBase64: base64.StdEncoding.EncodeToString(chunk), Offset: input.Offset, ReturnedBytes: len(chunk), EOF: end == len(image), MaxContentBytes: operatortool.AttachmentContentBytes})
				if err != nil {
					t.Fatal(err)
				}
				return AgentToolResult{Success: true, Content: string(data)}, nil
			}
			args, err := json.Marshal(operatortool.AttachmentOperationArguments{ProjectID: metadata.ProjectID, AttachmentID: metadata.ID, Length: 16})
			if err != nil {
				t.Fatal(err)
			}
			result, err := reader.call(t.Context(), AgentToolCall{Name: operatortool.ReadAttachment, Arguments: args})
			if test.denied || test.corrupt {
				if err == nil || result.ImageURL != "" || len(reader.evidence()) != 0 {
					t.Fatalf("unreadable image supplied evidence: err=%v reads=%v", err, reader.evidence())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.unsupported {
				if calls != 1 || result.ImageURL != "" || len(reader.evidence()) != 0 {
					t.Fatal("text-only backend claimed visual evidence")
				}
				return
			}
			reads := reader.evidence()
			if calls != 2 || result.ImageURL != "data:image/png;base64,"+encoded || len(reads) != 1 || reads[0].SHA256 != metadata.SHA256 || reads[0].CommentID != metadata.CommentID || !strings.Contains(result.Content, reads[0].ReceiptID) {
				t.Fatalf("published image not supplied with source-bound read receipt: calls=%d reads=%v", calls, reads)
			}
			validation := gate.ValidatorResult{HeadSHA: "review-head", AttachmentReads: reads, CriteriaEvidence: []gate.CriterionEvidence{{Criterion: "Page is visible", Kind: gate.EvidenceAttachment, Reference: "page.png", ReceiptIDs: []string{reads[0].ReceiptID}, HeadSHA: "review-head", TreeSHA: "review-tree", Behavior: "The independently viewed capture shows the required page"}}}
			gate.NormalizeCriterionEvidence(&validation, []string{"Page is visible"}, "review-tree")
			if len(validation.NotVerified) != 0 {
				t.Fatalf("observed image rejected: %+v", validation)
			}
		})
	}
}

type attachmentValidatorBackend struct {
	fakeCodexClient
	t *testing.T
}

func (b *attachmentValidatorBackend) RunTurnWithTools(ctx context.Context, req AgentTurnRequest, tools []AgentTool, handle AgentToolHandler, update AgentUpdateHandler) (AgentTurnResult, error) {
	b.t.Helper()
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
	err = update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: `{"verdict":"rework","score":0.5,"summary":"Reviewed ordinary comment; image still required","criteria_evidence":[{"criterion":"Page is visible","kind":"not_verified","reference":"Image needed"}]}`})
	return AgentTurnResult{}, err
}

type attachmentValidatorWorkspace struct{ reviewMutationWorkspace }

func (*attachmentValidatorWorkspace) RunReviewCommand(_ context.Context, _ workspace.Info, issue workspace.Issue, command string) (gate.CommandResult, error) {
	return gate.CommandResult{Command: command, HeadSHA: issue.PullRequestHeadSHA, TreeSHA: "tree"}, nil
}

type attachmentValidatorExecution struct {
	testExecution
	result gate.ValidatorResult
}

func (*attachmentValidatorExecution) ValidatorVersion(context.Context) (NativeValidation, error) {
	return NativeValidation{
		Version: &tracker.ChangeVersion{ID: "version_example", ChangeVersionInput: tracker.ChangeVersionInput{Repository: "example-org/example-project", HeadSHA: "head", BaseSHA: "base", Validation: &gate.CommandResult{TreeSHA: "tree"}}, Policy: policy.Descriptor{Gates: policy.Gates{Validator: true}}},
		Diff:    &connector.ValidationDiff{Repository: "example-org/example-project", HeadSHA: "head", BaseSHA: "base", Digest: "digest", Patch: "diff --git a/page.go b/page.go\n+page\n"},
	}, nil
}

func (e *attachmentValidatorExecution) RecordValidator(_ context.Context, result gate.ValidatorResult) error {
	e.result = result
	return nil
}

func (*attachmentValidatorExecution) AgentTools() ([]AgentTool, AgentToolHandler) {
	return []AgentTool{{Name: operatortool.WorkComments, InputSchema: json.RawMessage(`{"type":"object"}`)}, {Name: "attach_evidence", InputSchema: json.RawMessage(`{"type":"object"}`)}}, func(_ context.Context, call AgentToolCall) (AgentToolResult, error) {
		if call.Name != operatortool.WorkComments {
			return AgentToolResult{}, errors.New("unexpected tool")
		}
		return AgentToolResult{Success: true, Content: "ordinary published evidence"}, nil
	}
}

func TestNativeValidatorReceivesScopedEvidenceReads(t *testing.T) {
	t.Parallel()
	backend := &attachmentValidatorBackend{t: t}
	runner, err := NewRunner(Dependencies{
		Workflow:  config.Workflow{Config: config.Config{Gate: gate.Config{Validator: gate.ValidatorConfig{Enabled: true}}, Agents: config.Agents{Backends: []config.AgentBackend{{ID: "codex", Kind: "codex", Protocol: "app-server", Command: "codex app-server"}}, Routes: []config.AgentRoute{{Name: "validator", Role: RoleValidator, Backend: "codex", Model: "example-model"}}}}, Prompt: "Review"},
		Workspace: &attachmentValidatorWorkspace{reviewMutationWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}}, heads: []string{"head"}}}, AgentBackend: backend,
	})
	if err != nil {
		t.Fatal(err)
	}
	execution := &attachmentValidatorExecution{}
	err = runner.validateNativeChange(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "wi_example", Identifier: "prj_example#1", Metadata: map[string]string{"hub_project_id": "prj_example"}, Description: "## Acceptance Criteria\n- Page is visible"}})
	if err != nil {
		t.Fatal(err)
	}
	if execution.result.Verdict != gate.ValidatorVerdictRework || len(execution.result.NotVerified) != 1 {
		t.Fatalf("unseen image incorrectly approved: %+v", execution.result)
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
