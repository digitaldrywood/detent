package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type implementationReviewExecution struct {
	attachmentValidatorExecution
	events            *[]string
	validatorDisabled bool
	reviewResult      gate.ValidatorResult
	validatorResult   gate.ValidatorResult
	recordError       error
	commands          []gate.CommandResult
	validation        *gate.CommandResult
	discussion        NativeReviewDiscussion
}

func (e *implementationReviewExecution) ReviewVersion(context.Context) (NativeValidation, error) {
	*e.events = append(*e.events, "review version")
	input, _ := e.attachmentValidatorExecution.ValidatorVersion(context.Background())
	input.Version.Policy.Gates.AutomatedReview = gate.AutomatedReviewRequired
	input.Version.Policy.Gates.Validator = !e.validatorDisabled
	input.Commands = e.commands
	input.Discussion = e.discussion
	if e.validation != nil {
		input.Version.Validation = e.validation
	}
	return input, nil
}

func (e *implementationReviewExecution) RecordReview(_ context.Context, result gate.ValidatorResult) error {
	*e.events = append(*e.events, "record review")
	if result.VersionID != "version_example" {
		return fmt.Errorf("independent review did not identify the expected immutable version: %+v", result)
	}
	e.reviewResult = result
	return e.recordError
}

func (e *implementationReviewExecution) ValidatorVersion(ctx context.Context) (NativeValidation, error) {
	*e.events = append(*e.events, "validator version")
	input, err := e.attachmentValidatorExecution.ValidatorVersion(ctx)
	input.Commands = append(input.Commands, e.commands...)
	input.Commands = append(input.Commands, e.reviewResult.Commands...)
	if e.validation != nil {
		input.Version.Validation = e.validation
	}
	return input, err
}

func (e *implementationReviewExecution) RecordValidator(_ context.Context, result gate.ValidatorResult) error {
	*e.events = append(*e.events, "record validator")
	e.validatorResult = result
	return nil
}

func TestNativeImplementationReviewSSHCallbacksRemainSeparate(t *testing.T) {
	t.Parallel()
	var events []string
	execution := &implementationReviewExecution{events: &events, commands: []gate.CommandResult{{ReceiptID: "receipt-original", Command: "make check", HeadSHA: "head", TreeSHA: "tree", DurationNS: 123}}}
	human := tracker.Actor{Kind: "human", PrincipalID: "operator"}
	execution.discussion = NativeReviewDiscussion{Issue: []tracker.NativeComment{{ID: "issue_feedback", Body: "Human edited source feedback", Actor: tracker.Actor{Kind: "runner", PrincipalID: "worker"}, EditedBy: &human, CreatedAt: time.Unix(100, 0).UTC(), UpdatedAt: time.Unix(200, 0).UTC(), Provenance: &tracker.Provenance{Provider: "github", ExternalID: "feedback-original", AuthorID: "author"}}}, Change: []tracker.ChangeDiscussion{{ID: "change_feedback", VersionID: "version_example", Body: "Current source discussion", Actor: human, CreatedAt: time.Unix(200, 0).UTC()}}}
	left, right := net.Pipe()
	central := NewSSHPeer(t.Context(), left, left, NewSSHCallbackHandler(RunRequest{Execution: execution}, nil, nil, nil))
	remote := NewSSHPeer(t.Context(), right, right, nil)
	t.Cleanup(func() {
		central.Close()
		remote.Close()
		left.Close()
		right.Close()
	})
	transport := &sshNativeExecution{sshExecution: &sshExecution{peer: remote}}
	input, err := transport.ReviewVersion(t.Context())
	if err != nil || input.Version == nil || input.Version.ID != "version_example" {
		t.Fatalf("SSH lost immutable source-review version: %+v, %v", input, err)
	}
	if !reflect.DeepEqual(input.Commands, execution.commands) {
		t.Fatalf("SSH lost original command evidence: %+v", input.Commands)
	}
	if !reflect.DeepEqual(input.Discussion, execution.discussion) {
		t.Fatalf("SSH lost typed discussion actor/editor/version/provenance: %+v", input.Discussion)
	}
	review := gate.ValidatorResult{VersionID: input.Version.ID, HeadSHA: input.Version.HeadSHA, Verdict: gate.ValidatorVerdictWait, SessionID: 123, Submitted: true}
	if err := transport.RecordReview(t.Context(), review); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.ValidatorVersion(t.Context()); err != nil {
		t.Fatal(err)
	}
	validator := review
	validator.Verdict, validator.SessionID = gate.ValidatorVerdictPass, 124
	if err := transport.RecordValidator(t.Context(), validator); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(execution.reviewResult, review) || !reflect.DeepEqual(execution.validatorResult, validator) {
		t.Fatalf("SSH confused source and acceptance records: source=%+v acceptance=%+v", execution.reviewResult, execution.validatorResult)
	}
	if want := []string{"review version", "record review", "validator version", "record validator"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("SSH lost separate review callbacks: got %v, want %v", events, want)
	}
}

type implementationReviewBackend struct {
	fakeCodexClient
	t          *testing.T
	events     *[]string
	response   string
	discussion *NativeReviewDiscussion
}

type implementationReviewWorkspace struct {
	attachmentValidatorWorkspace
	commands []string
}

func (*implementationReviewWorkspace) ReviewCommandEnvironment(_ context.Context, _ workspace.Info, _ workspace.Issue, command string) gate.CheckEnvironment {
	if command != "go test ./..." {
		return gate.CheckEnvironment{}
	}
	return gate.CheckEnvironment{OS: "linux", Architecture: "amd64", GoVersion: "go1.26.9"}
}

func (w *implementationReviewWorkspace) RunReviewCommand(ctx context.Context, info workspace.Info, issue workspace.Issue, command string) (gate.CommandResult, error) {
	w.commands = append(w.commands, command)
	return gate.CommandResult{ReceiptID: fmt.Sprintf("receipt-new-%d", len(w.commands)), Command: command, HeadSHA: issue.PullRequestHeadSHA, TreeSHA: "tree", DurationNS: int64(time.Second), StartedAt: time.Now(), Evidence: &gate.CommandEvidence{Environment: w.ReviewCommandEnvironment(ctx, info, issue, "go test ./...")}}, nil
}

func (b *implementationReviewBackend) RunTurn(_ context.Context, request AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	if !request.ReadOnly {
		b.t.Fatal("native review received a writable turn")
	}
	*b.events = append(*b.events, request.Model)
	if request.Model == "review-model" && b.discussion != nil {
		raw, err := json.Marshal(*b.discussion)
		if err != nil || !strings.Contains(request.Prompt, string(raw)) || !strings.Contains(request.Prompt, "untrusted") {
			b.t.Fatal("independent source review did not consume typed untrusted discussion")
		}
	}
	response := b.response
	if request.Model == "validator-model" || response == "" {
		response = `{"verdict":"pass","score":1,"summary":"Current immutable source has no defects","criteria_evidence":[]}`
	}
	return AgentTurnResult{}, update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: response})
}

func TestNativeImplementationReviewPrecedesAcceptanceValidator(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		ssh          bool
		validator    bool
		verdict      string
		recordError  error
		editReceipt  func(*gate.CommandResult)
		prior        bool
		latestFailed bool
		latestPassed bool
		replayedPass bool
		command      string
		missingTree  bool
		wantRuns     int
	}{
		{name: "separate source and acceptance passes reuse source receipt", validator: true, verdict: "pass"},
		{name: "source review without acceptance validator", verdict: "pass"},
		{name: "source rework without acceptance validator", verdict: "rework"},
		{name: "source rework stops acceptance validator", validator: true, verdict: "rework"},
		{name: "source wait stops acceptance validator", validator: true, verdict: "wait"},
		{name: "SSH source feedback reaches independent review before acceptance", ssh: true, validator: true, verdict: "pass"},
		{name: "SSH source feedback rework stops acceptance", ssh: true, validator: true, verdict: "rework"},
		{name: "record failure stops acceptance validator", validator: true, verdict: "pass", recordError: errors.New("review recording unavailable")},
		{name: "prior exact-source review receipt reused", validator: true, verdict: "pass", prior: true},
		{name: "latest failure prevents older passing receipt reuse", validator: true, verdict: "pass", latestFailed: true, wantRuns: 1},
		{name: "clock-behind failure with replayed pass requires command", validator: true, verdict: "pass", latestFailed: true, replayedPass: true, wantRuns: 1},
		{name: "clock-behind newer pass supersedes failure", validator: true, verdict: "pass", latestPassed: true},
		{name: "opaque make toolchain requires commands", validator: true, verdict: "pass", command: "make check-landing", wantRuns: 2},
		{name: "pnpm without Node toolchain evidence requires commands", validator: true, verdict: "pass", command: "pnpm test", wantRuns: 2},
		{name: "missing candidate tree requires both commands", validator: true, verdict: "pass", missingTree: true, wantRuns: 2},
		{name: "changed head requires command", validator: true, verdict: "pass", editReceipt: func(r *gate.CommandResult) { r.HeadSHA = "old-head" }, wantRuns: 1},
		{name: "changed tree requires command", validator: true, verdict: "pass", editReceipt: func(r *gate.CommandResult) { r.TreeSHA = "old-tree" }, wantRuns: 1},
		{name: "changed command requires command", validator: true, verdict: "pass", editReceipt: func(r *gate.CommandResult) { r.Command = "make other-check" }, wantRuns: 1},
		{name: "failed receipt requires command", validator: true, verdict: "pass", editReceipt: func(r *gate.CommandResult) { r.ExitCode = 1 }, wantRuns: 1},
		{name: "missing execution requires command", validator: true, verdict: "pass", editReceipt: func(r *gate.CommandResult) { r.DurationNS = 0 }, wantRuns: 1},
		{name: "unknown environment requires command", validator: true, verdict: "pass", editReceipt: func(r *gate.CommandResult) { r.Evidence = nil }, wantRuns: 1},
		{name: "legacy receipt without identity requires command", validator: true, verdict: "pass", editReceipt: func(r *gate.CommandResult) { r.ReceiptID = "" }, wantRuns: 1},
		{name: "changed environment requires command", validator: true, verdict: "pass", editReceipt: func(r *gate.CommandResult) { r.Evidence.Environment.OS = "darwin" }, wantRuns: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := test.command
			if command == "" {
				command = "go test ./..."
			}
			var events []string
			backend := &implementationReviewBackend{t: t, events: &events, response: fmt.Sprintf(`{"verdict":%q,"score":1,"summary":"Independent source result","criteria_evidence":[]}`, test.verdict)}
			worktree := &implementationReviewWorkspace{attachmentValidatorWorkspace: attachmentValidatorWorkspace{reviewMutationWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}}, heads: []string{"head"}}}}
			receipt := gate.CommandResult{ReceiptID: "receipt-original", Command: command, HeadSHA: "head", TreeSHA: "tree", DurationNS: int64(time.Second), StartedAt: time.Now().Add(time.Hour), Evidence: &gate.CommandEvidence{Environment: worktree.ReviewCommandEnvironment(t.Context(), workspace.Info{}, workspace.Issue{}, "go test ./...")}}
			if test.ssh {
				receipt.StartedAt = receipt.StartedAt.UTC()
			}
			if test.editReceipt != nil {
				test.editReceipt(&receipt)
			}
			r, err := NewRunner(Dependencies{
				Workflow: config.Workflow{Config: config.Config{
					Gate: gate.Config{Run: command, AutomatedReview: gate.AutomatedReviewRequired, Validator: gate.ValidatorConfig{Enabled: test.validator}},
					Agents: config.Agents{Backends: []config.AgentBackend{{ID: "codex", Kind: "codex", Protocol: "app-server", Command: "codex app-server"}}, Routes: []config.AgentRoute{
						{Name: "implementation", Role: RoleCode, Backend: "codex", Model: "review-model"},
						{Name: "acceptance", Role: RoleValidator, Backend: "codex", Model: "validator-model"},
					}},
				}, Prompt: "Review"},
				Workspace:    worktree,
				AgentBackend: backend,
			})
			if err != nil {
				t.Fatal(err)
			}
			execution := &implementationReviewExecution{events: &events, validatorDisabled: !test.validator, recordError: test.recordError, validation: &receipt}
			var reviewExecution Execution = execution
			if test.ssh {
				human := tracker.Actor{Kind: "human", PrincipalID: "operator"}
				execution.discussion = NativeReviewDiscussion{Issue: []tracker.NativeComment{{ID: "issue_feedback", Body: "Updated issue feedback", Actor: tracker.Actor{Kind: "runner", PrincipalID: "worker"}, EditedBy: &human, CreatedAt: time.Unix(100, 0), UpdatedAt: time.Unix(200, 0)}}, Change: []tracker.ChangeDiscussion{{ID: "change_feedback", VersionID: "version_example", Body: "Current version feedback", Actor: human, CreatedAt: time.Unix(200, 0), Provenance: &tracker.Provenance{Provider: "github", ExternalID: "feedback-original", AuthorID: "author"}}}}
				backend.discussion = &execution.discussion
				left, right := net.Pipe()
				central := NewSSHPeer(t.Context(), left, left, NewSSHCallbackHandler(RunRequest{Execution: execution}, nil, nil, nil))
				remote := NewSSHPeer(t.Context(), right, right, nil)
				t.Cleanup(func() { central.Close(); remote.Close(); left.Close(); right.Close() })
				reviewExecution = &sshNativeExecution{sshExecution: &sshExecution{peer: remote}}
			}
			if test.prior || test.editReceipt != nil {
				execution.validation = &gate.CommandResult{TreeSHA: "tree"}
				execution.commands = []gate.CommandResult{receipt}
			}
			if test.latestFailed {
				failed := receipt
				failed.ReceiptID = "receipt-failed"
				failed.ExitCode, failed.StartedAt = 7, time.Now().Add(-time.Hour)
				execution.commands = []gate.CommandResult{failed}
				if test.replayedPass {
					execution.commands = append(execution.commands, receipt)
				}
			}
			expectedReceipt := receipt
			if test.latestPassed {
				passed := receipt
				passed.ReceiptID, passed.StartedAt = "receipt-passed", time.Now().Add(-time.Hour)
				expectedReceipt = passed
				execution.validation.ExitCode = 7
				execution.commands = []gate.CommandResult{passed}
			}
			if test.missingTree {
				execution.validation = &gate.CommandResult{}
			}
			err = r.validateNativeChange(t.Context(), RunRequest{Execution: reviewExecution, Issue: connector.Issue{ID: "wi_example", Identifier: "prj_example#1", Description: "## Acceptance criteria\n- Preserve existing behavior"}})
			if (err != nil) != (test.recordError != nil) {
				t.Fatalf("source verdict became a run failure or recording failure was ignored: %v", err)
			}
			want := []string{"review version", "review-model", "record review"}
			if test.verdict == "pass" && test.validator && test.recordError == nil {
				want = append(want, "validator version", "validator-model", "record validator")
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("native completion omitted or reordered independent review: got %v, want %v", events, want)
			}
			if execution.reviewResult.Verdict != test.verdict {
				t.Fatalf("source review verdict was rewritten: got %q, want %q", execution.reviewResult.Verdict, test.verdict)
			}
			if len(worktree.commands) != test.wantRuns {
				t.Fatalf("applicable evidence was rerun or invalid evidence skipped execution: commands=%v want executions=%d", worktree.commands, test.wantRuns)
			}
			if test.wantRuns == 0 && !reflect.DeepEqual(execution.reviewResult.Commands, []gate.CommandResult{expectedReceipt}) {
				t.Fatalf("reuse rewrote the original receipt: %+v", execution.reviewResult.Commands)
			}
			if execution.validatorResult.Submitted && !test.missingTree && test.command == "" && !reflect.DeepEqual(execution.validatorResult.Commands[len(execution.validatorResult.Commands)-1], execution.reviewResult.Commands[len(execution.reviewResult.Commands)-1]) {
				t.Fatalf("fresh acceptance review lost applicable source-review evidence: %+v", execution.validatorResult.Commands)
			}
		})
	}
}
