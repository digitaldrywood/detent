package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	largeSource := "beginning\n" + strings.Repeat("source line\n", 9000) + "ending\n"
	attemptDiffFile(t, source, "large.txt", largeSource)
	unicodeSource := strings.Repeat("a", 16383) + "✓🙂end\n"
	attemptDiffFile(t, source, "unicode.txt", unicodeSource)
	var listing strings.Builder
	listing.WriteString("README.md\n")
	for i := range 600 {
		name := fmt.Sprintf("files/%04d-%s.txt", i, strings.Repeat("x", 120))
		attemptDiffFile(t, source, name, "fixture\n")
		fmt.Fprintln(&listing, name)
	}
	listing.WriteString("large.txt\nunicode.txt\n")
	runRunnerGit(t, source, "add", ".")
	runRunnerGit(t, source, "commit", "-m", "document source contract")
	head := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
	tree := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD^{tree}"))
	for _, test := range []struct {
		name                                                                string
		gateFails, checkFails, missingRead, missingEvidence, forgedEvidence bool
		checkRecovers                                                       bool
		firstRead, revision                                                 string
		paged                                                               bool
		want                                                                string
	}{
		{name: "large source and listing pages", paged: true, want: gate.ValidatorVerdictPass},
		{name: "missing source read recovered", want: gate.ValidatorVerdictPass},
		{name: "pinned base read recovered", revision: "base", want: gate.ValidatorVerdictPass},
		{name: "failed configured gate cannot be omitted", gateFails: true, want: gate.ValidatorVerdictWait},
		{name: "failed required check cannot be omitted", checkFails: true, want: gate.ValidatorVerdictWait},
		{name: "later pass of the same check on the same tree clears its failure", checkRecovers: true, want: gate.ValidatorVerdictPass},
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
			type expectedPage struct {
				offset  int
				content string
				total   int
			}
			var pages []expectedPage
			backend := &diagnosticCommandBackend{t: t, missingEvidence: test.missingEvidence, forgedEvidence: test.forgedEvidence}
			if test.checkFails {
				backend.calls = append(backend.calls, AgentToolCall{Name: "detent_run_validation", Arguments: json.RawMessage(`{"command":"git diff --exit-code HEAD~1 HEAD -- README.md"}`)})
			}
			if test.checkRecovers {
				marker := filepath.Join(t.TempDir(), "ran")
				raw, err := json.Marshal(map[string]string{"command": "test -f " + marker + " || { touch " + marker + "; exit 1; }"})
				if err != nil {
					t.Fatal(err)
				}
				call := AgentToolCall{Name: "detent_run_validation", Arguments: raw}
				backend.calls = append(backend.calls, call, call)
			}
			firstCall := read("read", first)
			if test.missingRead {
				firstCall.Arguments = json.RawMessage(`{"operation":"read","revision":"head","path":"missing.md","offset":65536,"length":1}`)
			}
			backend.calls = append(backend.calls, firstCall)
			if !test.missingRead {
				backend.calls = append(backend.calls, read("list", "."), read("read", "README.md"))
			}
			if test.paged {
				for _, fixture := range []struct{ operation, path, content string }{
					{"read", "large.txt", largeSource}, {"list", ".", listing.String()},
				} {
					for _, offset := range []int{0, len(fixture.content) / 2, len(fixture.content) - 200, int(^uint(0) >> 1)} {
						raw, err := json.Marshal(map[string]any{"operation": fixture.operation, "revision": "head", "path": fixture.path, "offset": offset, "length": 1024})
						if err != nil {
							t.Fatal(err)
						}
						backend.calls = append(backend.calls, AgentToolCall{Name: "detent_read_source", Arguments: raw})
						start := min(offset, len(fixture.content))
						end := start + min(1024, len(fixture.content)-start)
						pages = append(pages, expectedPage{start, fixture.content[start:end], len(fixture.content)})
					}
				}
				for _, page := range []struct {
					offset, length, start int
					content               string
				}{
					{0, 16384, 0, strings.Repeat("a", 16383)},
					{16383, 16384, 16383, "✓🙂end\n"},
					{16384, 1, 16383, "✓"},
				} {
					raw, err := json.Marshal(map[string]any{"operation": "read", "revision": "head", "path": "unicode.txt", "offset": page.offset, "length": page.length})
					if err != nil {
						t.Fatal(err)
					}
					backend.calls = append(backend.calls, AgentToolCall{Name: "detent_read_source", Arguments: raw})
					pages = append(pages, expectedPage{page.start, page.content, len(unicodeSource)})
				}
				backend.calls = append(backend.calls, read("read", "README.md"))
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
				t.Fatalf("host command history has %d receipts for %d calls", len(result.Commands), len(backend.calls))
			}
			failed := result.Commands[1]
			if test.checkFails {
				failed = result.Commands[2]
			}
			if test.checkRecovers {
				if result.Commands[1].ExitCode == 0 || result.Commands[2].ExitCode != 0 || result.Commands[1].TreeSHA != result.Commands[2].TreeSHA {
					t.Fatalf("flaky check receipts not retained: %+v %+v", result.Commands[1], result.Commands[2])
				}
				failed = result.Commands[3]
			}
			if failed.ExitCode != 128 || !strings.Contains(failed.Output, "fatal: path") || failed.ReceiptID == "" || failed.HeadSHA != head || failed.TreeSHA != tree {
				t.Fatalf("failed source read not retained: %+v", failed)
			}
			if !test.missingRead {
				last := result.Commands[len(result.Commands)-1]
				want := "The pinned source contract.\n"
				if revision == "base" {
					want = "source repo\n"
				}
				if last.ExitCode != 0 || !strings.HasSuffix(last.Output, want) || last.HeadSHA != head || last.TreeSHA != tree {
					t.Fatalf("successful pinned source read: %+v", last)
				}
			}
			if test.paged {
				receipt := backend.receipts[1]
				prefix := fmt.Sprintf("[source bytes 0:16384 of %d; next_offset=16384; eof=false]\n", listing.Len())
				if receipt.Output != prefix+listing.String()[:16384] || !receipt.OutputTruncated {
					t.Fatalf("default listing page not bounded: bytes=%d truncated=%t", len(receipt.Output), receipt.OutputTruncated)
				}
			}
			for i, page := range pages {
				receipt := backend.receipts[3+i]
				end := page.offset + len(page.content)
				prefix := fmt.Sprintf("[source bytes %d:%d of %d; next_offset=%d; eof=%t]\n", page.offset, end, page.total, end, end == page.total)
				if receipt.ExitCode != 0 || receipt.Output != prefix+page.content || receipt.HeadSHA != head || receipt.TreeSHA != tree || receipt.ReceiptID == "" || receipt.OutputTruncated != (page.offset > 0 || end < page.total) {
					t.Fatalf("page %d lost source content or identity: %+v", i, receipt)
				}
			}
			if test.missingRead && (len(result.NotVerified) != 1 || result.CriteriaEvidence[0].Kind != gate.EvidenceNotVerified) {
				t.Fatalf("missing source verified: %+v", result.CriteriaEvidence)
			}
			if _, err := os.Stat(filepath.Join(source, "injected")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("source path executed shell syntax: %v", err)
			}
		})
	}
}

func TestValidatorSourceReadRejectsUnboundedOperations(t *testing.T) {
	for _, raw := range []string{
		`{"operation":"read","revision":"head","path":"README.md","offset":-1}`,
		`{"operation":"read","revision":"head","path":"README.md","length":0}`,
		`{"operation":"read","revision":"head","path":"README.md","length":16385}`,
		`{"operation":"read","revision":"head","path":"README.md","offset":1.5}`,
		`{"operation":"read","revision":"head","path":"README.md","offset":9223372036854775808}`,
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
