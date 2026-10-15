package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type diagnosticCommandBackend struct {
	fakeCodexClient
	t               *testing.T
	calls           []AgentToolCall
	receipts        []gate.CommandResult
	missingEvidence bool
	forgedEvidence  bool
}

func (b *diagnosticCommandBackend) RunTurnWithTools(ctx context.Context, _ AgentTurnRequest, _ []AgentTool, handle AgentToolHandler, update AgentUpdateHandler) (AgentTurnResult, error) {
	for _, call := range b.calls {
		response, err := handle(ctx, call)
		if err != nil {
			return AgentTurnResult{}, err
		}
		var receipt gate.CommandResult
		if json.Unmarshal([]byte(response.Content), &receipt) == nil && receipt.Command != "" {
			b.receipts = append(b.receipts, receipt)
		}
	}
	result := gate.ValidatorResult{Verdict: gate.ValidatorVerdictPass, Score: 1, Summary: "Independently inspected the pinned documentation and required check results."}
	if !b.missingEvidence {
		id := b.receipts[len(b.receipts)-1].ReceiptID
		if b.forgedEvidence {
			id = "unobserved-receipt"
		}
		result.CriteriaEvidence = []gate.CriterionEvidence{{Criterion: "Document the source contract.", Kind: gate.EvidenceReceipt, Reference: "README.md", ReceiptIDs: []string{id}, Behavior: "The pinned file documents the source contract."}}
	}
	output, err := json.Marshal(result)
	if err != nil {
		b.t.Fatal(err)
	}
	return AgentTurnResult{}, update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: string(output)})
}

func TestValidatorRecoveredSourceSearchDoesNotFailRequiredValidation(t *testing.T) {
	isolateAttemptDiffGitConfig(t)
	source := initRunnerSourceRepo(t)
	base := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
	attemptDiffFile(t, source, "README.md", "The pinned source contract.\n")
	runRunnerGit(t, source, "add", "README.md")
	runRunnerGit(t, source, "commit", "-m", "document source contract")
	head := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
	tree := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD^{tree}"))
	for _, test := range []struct {
		name                                                                string
		gateFails, checkFails, missingRead, missingEvidence, forgedEvidence bool
		firstRead, revision                                                 string
		want                                                                string
	}{
		{name: "missing source read recovered", want: gate.ValidatorVerdictPass},
		{name: "pinned base read recovered", revision: "base", want: gate.ValidatorVerdictPass},
		{name: "failed configured gate cannot be omitted", gateFails: true, want: gate.ValidatorVerdictWait},
		{name: "failed required check cannot be omitted", checkFails: true, want: gate.ValidatorVerdictWait},
		{name: "failed required source receipt", missingRead: true, want: gate.ValidatorVerdictRework},
		{name: "missing required source evidence", missingEvidence: true, want: gate.ValidatorVerdictRework},
		{name: "unobserved receipt cannot substitute", forgedEvidence: true, want: gate.ValidatorVerdictRework},
		{name: "shell syntax is a literal path", firstRead: "README.md'; touch injected; #", want: gate.ValidatorVerdictPass},
	} {
		t.Run(test.name, func(t *testing.T) {
			local, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: filepath.Dir(source), SourceRoot: source})
			if err != nil {
				t.Fatal(err)
			}
			worktree := &implementationReviewWorkspace{attachmentValidatorWorkspace: attachmentValidatorWorkspace{reviewMutationWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: source}}, heads: []string{head}}}, local: local}
			input, err := (&attachmentValidatorExecution{}).ValidatorVersion(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			input.Version.HeadSHA, input.Version.BaseSHA = head, base
			input.Version.Validation.TreeSHA = tree
			input.Diff.HeadSHA, input.Diff.BaseSHA = head, base
			first := test.firstRead
			if first == "" {
				first = "missing.md"
			}
			revision := test.revision
			if revision == "" {
				revision = "head"
			}
			read := func(operation, path string) AgentToolCall {
				raw, err := json.Marshal(map[string]string{"operation": operation, "revision": revision, "path": path})
				if err != nil {
					t.Fatal(err)
				}
				return AgentToolCall{Name: "detent_read_source", Arguments: raw}
			}
			backend := &diagnosticCommandBackend{t: t, missingEvidence: test.missingEvidence, forgedEvidence: test.forgedEvidence}
			if test.checkFails {
				backend.calls = append(backend.calls, AgentToolCall{Name: "detent_run_validation", Arguments: json.RawMessage(`{"command":"git diff --exit-code HEAD~1 HEAD -- README.md"}`)})
			}
			backend.calls = append(backend.calls, read("read", first))
			if !test.missingRead {
				backend.calls = append(backend.calls, read("list", "."), read("read", "README.md"))
			}
			command := "git diff --exit-code HEAD -- README.md"
			if test.gateFails {
				command = "git diff --exit-code HEAD~1 HEAD -- README.md"
			}
			runner, err := NewRunner(Dependencies{
				Workflow: config.Workflow{Config: config.Config{
					Gate:   gate.Config{Run: command, Validator: gate.ValidatorConfig{Enabled: true}},
					Agents: config.Agents{Backends: []config.AgentBackend{{ID: "codex", Kind: "codex", Protocol: "app-server", Command: "codex app-server"}}, Routes: []config.AgentRoute{{Name: "validator", Role: RoleValidator, Backend: "codex", Model: "example-model"}}},
				}, Prompt: "Review"}, Workspace: worktree, AgentBackend: backend,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := runner.Validate(t.Context(), ValidatorRequest{Issue: connector.Issue{ID: "wi_example", Identifier: "prj_example#1", Description: "## Acceptance criteria\n- Document the source contract.\n"}, NativeVersion: input.Version, Diff: input.Diff})
			if err != nil {
				t.Fatal(err)
			}
			if result.Verdict != test.want {
				t.Fatalf("verdict=%s want=%s summary=%s", result.Verdict, test.want, result.Summary)
			}
			if len(result.Commands) != len(backend.calls)+1 {
				t.Fatalf("host command history was lost: %+v", result.Commands)
			}
			failed := result.Commands[1]
			if test.checkFails {
				failed = result.Commands[2]
			}
			if failed.ExitCode == 0 || failed.ReceiptID == "" || failed.HeadSHA != head || failed.TreeSHA != tree {
				t.Fatalf("failed source read not retained: %+v", failed)
			}
			if !test.missingRead {
				last := result.Commands[len(result.Commands)-1]
				want := "The pinned source contract.\n"
				if revision == "base" {
					want = "source repo\n"
				}
				if last.ExitCode != 0 || last.Output != want || last.HeadSHA != head || last.TreeSHA != tree {
					t.Fatalf("successful pinned source read: %+v", last)
				}
			}
			if _, err := os.Stat(filepath.Join(source, "injected")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("source path executed shell syntax: %v", err)
			}
		})
	}
}

func TestValidatorSourceReadRejectsUnboundedOperations(t *testing.T) {
	for _, raw := range []string{
		`{"operation":"read","revision":"head","path":"../outside"}`,
		`{"operation":"read","revision":"head","path":"/etc/passwd"}`,
		`{"operation":"read","revision":"HEAD","path":"README.md"}`,
		`{"operation":"execute","revision":"head","path":"README.md"}`,
		`{"operation":"read","revision":"head","path":"README.md","command":"touch injected"}`,
		`{"operation":"read","revision":"head","path":"docs\\outside"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			worktree := &implementationReviewWorkspace{}
			commands := &validatorCommands{run: worktree, issue: workspace.Issue{PullRequestHeadSHA: strings.Repeat("a", 40)}}
			_, handle := commands.tools()
			result, err := handle(t.Context(), AgentToolCall{Name: "detent_read_source", Arguments: json.RawMessage(raw)})
			if err != nil || result.Success || len(worktree.commands) != 0 {
				t.Fatalf("invalid source operation executed: result=%+v err=%v commands=%v", result, err, worktree.commands)
			}
		})
	}
}
