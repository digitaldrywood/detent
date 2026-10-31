package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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
	noChange          bool
	auditResult       tracker.SecurityAuditReview
	reviewResult      gate.ValidatorResult
	validatorResult   gate.ValidatorResult
	recordError       error
	commands          []gate.CommandResult
	validation        *gate.CommandResult
	discussion        NativeReviewDiscussion
	head              string
	securityAudit     *NativeSecurityAuditEvidence
	reviewSource      tracker.NativeChangeReference
}

func (e *implementationReviewExecution) RecoverChangeSource(ctx context.Context) (workspace.ChangeSource, error) {
	input, err := e.attachmentValidatorExecution.ValidatorVersion(ctx)
	if e.head != "" {
		input.Version.HeadSHA = e.head
	}
	return workspace.ChangeSource{Version: *input.Version}, err
}

func (e *implementationReviewExecution) PrepareReview(_ context.Context, source tracker.NativeChangeReference) error {
	*e.events = append(*e.events, "prepare review")
	e.reviewSource = source
	return e.recordError
}

func (e *implementationReviewExecution) ReviewVersion(context.Context) (NativeValidation, error) {
	*e.events = append(*e.events, "review version")
	input, _ := e.attachmentValidatorExecution.ValidatorVersion(context.Background())
	input.Version.Policy.Gates.AutomatedReview = gate.AutomatedReviewRequired
	input.Version.Policy.Gates.Validator = !e.validatorDisabled
	if e.head != "" {
		input.Version.HeadSHA = e.head
		input.Diff.HeadSHA = e.head
	}
	if e.noChange {
		input.Version.ID, input.Version.ChangeID = "attempt_example", ""
		input.Version.BaseSHA = input.Version.HeadSHA
		input.Diff.BaseSHA = input.Diff.HeadSHA
		input.Diff.Files, input.Diff.Patch = nil, ""
	}
	input.Commands = e.commands
	input.Discussion = e.discussion
	if e.validation != nil {
		input.Version.Validation = e.validation
	}
	return input, nil
}

func (e *implementationReviewExecution) SecurityAuditVersion(ctx context.Context) (NativeValidation, error) {
	*e.events = append(*e.events, "audit version")
	return e.attachmentValidatorExecution.ValidatorVersion(ctx)
}

func (e *implementationReviewExecution) RecordSecurityAudit(_ context.Context, result tracker.SecurityAuditReview) error {
	*e.events = append(*e.events, "record audit")
	e.auditResult = result
	return nil
}

func (e *implementationReviewExecution) RecordReview(_ context.Context, result gate.ValidatorResult) error {
	*e.events = append(*e.events, "record review")
	if result.VersionID != "version_example" && (!e.noChange || result.VersionID != "attempt_example") {
		return fmt.Errorf("independent review did not identify the expected immutable version: %+v", result)
	}
	e.reviewResult = result
	return e.recordError
}

func (e *implementationReviewExecution) ValidatorVersion(ctx context.Context) (NativeValidation, error) {
	*e.events = append(*e.events, "validator version")
	input, err := e.attachmentValidatorExecution.ValidatorVersion(ctx)
	if e.head != "" {
		input.Version.HeadSHA = e.head
		input.Diff.HeadSHA = e.head
	}
	if e.noChange {
		input.Version.ID, input.Version.ChangeID = "attempt_example", ""
		input.Version.BaseSHA = input.Version.HeadSHA
		input.Diff.BaseSHA = input.Diff.HeadSHA
		input.Diff.Files, input.Diff.Patch = nil, ""
	}
	input.Commands = append(input.Commands, e.commands...)
	input.Commands = append(input.Commands, e.reviewResult.Commands...)
	input.SecurityAudit = e.securityAudit
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
	source := tracker.NativeChangeReference{ChangeID: "change_example", VersionID: "version_example", HeadSHA: "head"}
	if err := transport.PrepareReview(t.Context(), source); err != nil || execution.reviewSource != source {
		t.Fatalf("SSH lost review continuation binding: %+v, %v", execution.reviewSource, err)
	}
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
	if _, err := transport.SecurityAuditVersion(t.Context()); err != nil {
		t.Fatal(err)
	}
	audit := tracker.SecurityAuditReview{SessionID: 125, Verdict: "pass"}
	if err := transport.RecordSecurityAudit(t.Context(), audit); err != nil {
		t.Fatal(err)
	}
	if execution.auditResult.SessionID != 125 {
		t.Fatal("SSH lost security audit result")
	}
	if !reflect.DeepEqual(execution.reviewResult, review) || !reflect.DeepEqual(execution.validatorResult, validator) {
		t.Fatalf("SSH confused source and acceptance records: source=%+v acceptance=%+v", execution.reviewResult, execution.validatorResult)
	}
	if want := []string{"prepare review", "review version", "record review", "validator version", "record validator", "audit version", "record audit"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("SSH lost separate review callbacks: got %v, want %v", events, want)
	}
	execution.recordError = ErrExecutionAuthorityUnavailable
	if err := transport.PrepareReview(t.Context(), source); !errors.Is(err, ErrExecutionAuthorityUnavailable) {
		t.Fatalf("SSH lost review continuation refusal: %v", err)
	}
}

type implementationReviewBackend struct {
	fakeCodexClient
	t                 *testing.T
	events            *[]string
	response          string
	discussion        *NativeReviewDiscussion
	securityAudit     *NativeSecurityAuditEvidence
	validatorResponse string
}

type implementationReviewWorkspace struct {
	attachmentValidatorWorkspace
	commands []string
	local    *workspace.LocalGit
}

func (w *implementationReviewWorkspace) ReviewCommandEnvironment(ctx context.Context, info workspace.Info, issue workspace.Issue, command string) gate.CheckEnvironment {
	if w.local != nil {
		return w.local.ReviewCommandEnvironment(ctx, info, issue, command)
	}
	if command != "go test ./..." {
		return gate.CheckEnvironment{}
	}
	return gate.CheckEnvironment{OS: "linux", Architecture: "amd64", GoVersion: "go1.26.9"}
}

func (w *implementationReviewWorkspace) ReviewCommandEvidence(ctx context.Context, info workspace.Info, issue workspace.Issue, command string) *gate.CommandEvidence {
	if w.local != nil {
		return w.local.ReviewCommandEvidence(ctx, info, issue, command)
	}
	return &gate.CommandEvidence{Environment: w.ReviewCommandEnvironment(ctx, info, issue, command), InputsSHA: strings.Repeat("a", 64)}
}

func (w *implementationReviewWorkspace) RunReviewCommand(ctx context.Context, info workspace.Info, issue workspace.Issue, command string) (gate.CommandResult, error) {
	w.commands = append(w.commands, command)
	if w.local != nil {
		return w.local.RunReviewCommand(ctx, info, issue, command)
	}
	return gate.CommandResult{ReceiptID: fmt.Sprintf("receipt-new-%d", len(w.commands)), Command: command, HeadSHA: issue.PullRequestHeadSHA, TreeSHA: "tree", DurationNS: int64(time.Second), StartedAt: time.Now(), Evidence: w.ReviewCommandEvidence(ctx, info, issue, "go test ./...")}, nil
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
	if request.Model == "validator-model" && b.securityAudit != nil {
		for _, identity := range []string{b.securityAudit.ReviewID, b.securityAudit.VersionID, b.securityAudit.DiffDigest, b.securityAudit.ReviewerDigest, "does not approve this validator's decision"} {
			if !strings.Contains(request.Prompt, identity) {
				b.t.Fatalf("validator lost accepted security audit context %q", identity)
			}
		}
	}
	if request.Model == "validator-model" && b.validatorResponse != "" {
		response = b.validatorResponse
	}
	return AgentTurnResult{}, update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: response})
}

func TestNativeImplementationReviewPrecedesAcceptanceValidator(t *testing.T) {
	isolateAttemptDiffGitConfig(t)
	for _, test := range []struct {
		name             string
		noChange         bool
		ssh              bool
		validator        bool
		verdict          string
		recordError      error
		editReceipt      func(*gate.CommandResult)
		prior            bool
		latestFailed     bool
		latestPassed     bool
		replayedPass     bool
		command          string
		missingTree      bool
		wantRuns         int
		wrapped          bool
		linterState      string
		unsupported      bool
		shadow           bool
		gateFailure      bool
		securityAudit    bool
		acceptanceRework bool
		unsatisfiable    bool
	}{
		{name: "separate source and acceptance passes reuse source receipt", validator: true, verdict: "pass"},
		{name: "no-change evidence runs independent producers", noChange: true, validator: true, verdict: "pass"},
		{name: "no-change review rejection stops acceptance", noChange: true, validator: true, verdict: "rework"},
		{name: "no-change evidence producers cross SSH", noChange: true, ssh: true, validator: true, verdict: "pass"},

		{name: "1245 production criterion publishes wait", validator: true, verdict: "pass", unsatisfiable: true},
		{name: "1245 production criterion publishes wait over SSH", validator: true, verdict: "pass", unsatisfiable: true, ssh: true},
		{name: "accepted audit reaches validator", validator: true, verdict: "pass", securityAudit: true},
		{name: "accepted audit crosses SSH", validator: true, verdict: "pass", securityAudit: true, ssh: true},
		{name: "accepted audit does not approve acceptance", validator: true, verdict: "pass", securityAudit: true, acceptanceRework: true},
		{name: "direct Go launcher requires independent commands", validator: true, verdict: "pass", command: "go build ./...", wrapped: true, wantRuns: 2},
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
		{name: "matching landing receipt has unbound runtime inputs", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, wantRuns: 2},
		{name: "unbound landing receipt reruns after transport", ssh: true, validator: true, verdict: "pass", command: "make check-landing", wrapped: true, wantRuns: 2},
		{name: "replaced landing linter requires command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, linterState: "replaced", wantRuns: 2},
		{name: "missing landing linter declines reuse", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, linterState: "missing", gateFailure: true, wantRuns: 1},
		{name: "unexecutable landing linter declines reuse", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, linterState: "unexecutable", gateFailure: true, wantRuns: 1},
		{name: "unsupported wrapper is executed independently", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, unsupported: true, wantRuns: 2},
		{name: "shadow Makefile is executed independently", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, unsupported: true, shadow: true, wantRuns: 2},
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
			receipt := gate.CommandResult{ReceiptID: "receipt-original", Command: command, HeadSHA: "head", TreeSHA: "tree", DurationNS: int64(time.Second), StartedAt: time.Now().Add(time.Hour), Evidence: worktree.ReviewCommandEvidence(t.Context(), workspace.Info{}, workspace.Issue{}, "go test ./...")}
			if test.wrapped {
				if runtime.GOOS == "windows" {
					t.Skip("POSIX landing wrapper")
				}
				t.Setenv("CHECK_LAND_BASE", "origin/develop")
				t.Setenv("MAKEFLAGS", "")
				t.Setenv("MFLAGS", "")
				source := initRunnerSourceRepo(t)
				for _, name := range []string{"Makefile", "scripts/check-landing.sh", "scripts/check-evidence.sh"} {
					content, err := os.ReadFile(filepath.Join("..", "..", name))
					if err != nil {
						t.Fatal(err)
					}
					if test.unsupported && !test.shadow && name == "scripts/check-landing.sh" {
						content = append(content, []byte("\nprintf 'custom wrapper\\n'\n")...)
					}
					if err := os.MkdirAll(filepath.Dir(filepath.Join(source, name)), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(source, name), content, 0o600); err != nil {
						t.Fatal(err)
					}
					if test.shadow && name == "Makefile" {
						if err := os.WriteFile(filepath.Join(source, "GNUmakefile"), content, 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				module := "module example.test/project\n\ngo 1.26.0\ntoolchain go1.26.9\n"
				if err := os.WriteFile(filepath.Join(source, "go.mod"), []byte(module), 0o600); err != nil {
					t.Fatal(err)
				}
				for name, content := range map[string]string{
					".gitignore":        "tmp/\nnode_modules/\n/static/app/conversation/\n/docs/config.md/\n",
					".golangci-version": "v2.14.0\n",
					"main.go":           "package main\nfunc main() {}\n",
				} {
					if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				runRunnerGit(t, source, "add", ".")
				runRunnerGit(t, source, "commit", "-m", "landing fixture")
				runRunnerGit(t, source, "update-ref", "refs/remotes/origin/develop", "HEAD~1")
				linter := filepath.Join(source, "tmp", "tools", "golangci-lint", "v2.14.0", "go1.26.9", "golangci-lint")
				if err := os.MkdirAll(filepath.Dir(linter), 0o700); err != nil {
					t.Fatal(err)
				}
				linterTool := "#!/bin/sh\necho original >> .git/linter-invocations\n"
				if err := os.WriteFile(linter, []byte(linterTool), 0o700); err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				goTool := `#!/bin/sh
version=${GOTOOLCHAIN:-go1.27.1}
if [ "$1" = env ]; then
    printf '{"GOHOSTOS":"linux","GOHOSTARCH":"amd64","GOVERSION":"%s"}\n' "$version"
elif [ "$1" = build ]; then
    echo build >> .git/invocations
elif [ "$1" = list ]; then
    printf '{"Dir":"%s","ImportPath":"example.test/project"}\n' "$PWD"
fi
`
				if err := os.WriteFile(filepath.Join(bin, "go"), []byte(goTool), 0o700); err != nil {
					t.Fatal(err)
				}
				searchPath := bin + string(os.PathListSeparator) + os.Getenv("PATH")
				t.Setenv("PATH", searchPath)
				t.Setenv("GOTOOLCHAIN", "go1.27.1")
				if strings.HasPrefix(command, "go ") {
					t.Setenv("GOTOOLCHAIN", "go1.26.9")
				}
				options := workspace.LocalGitOptions{ProcessSource: emptyProcessSource, Root: filepath.Dir(source), SourceRoot: source}
				local, err := workspace.NewLocalGit(options)
				if err != nil {
					t.Fatal(err)
				}
				worktree.local = local
				worktree.info.Path = source
				head := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
				worktree.heads = []string{head}
				receipt, err = local.RunReviewCommand(t.Context(), worktree.info, workspace.Issue{ProjectID: defaultProjectID, ID: "wi_example", Identifier: "prj_example#1", PullRequestHeadSHA: head}, command)
				if err != nil || receipt.ExitCode != 0 {
					t.Fatalf("host finalization command: %+v %v", receipt, err)
				}
				receipt.TimingStage = "finalization"
				receipt = tracker.PublicGateResult(receipt)
				switch test.linterState {
				case "replaced":
					if err := os.WriteFile(linter, []byte("#!/bin/sh\necho replacement >> .git/linter-invocations\n"), 0o700); err != nil {
						t.Fatal(err)
					}
				case "missing":
					if err := os.Remove(linter); err != nil {
						t.Fatal(err)
					}
				case "unexecutable":
					if err := os.Chmod(linter, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			candidateTree := receipt.TreeSHA
			if test.ssh {
				receipt.StartedAt = receipt.StartedAt.UTC()
			}
			if test.editReceipt != nil {
				test.editReceipt(&receipt)
			}
			r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource,
				Workflow: config.Workflow{Config: config.Config{
					Gate: gate.Config{Run: command, SecurityAudit: gate.SecurityAuditConfig{Enabled: false}, AutomatedReview: gate.AutomatedReviewRequired, Validator: gate.ValidatorConfig{Enabled: test.validator}},
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
			execution := &implementationReviewExecution{events: &events, validatorDisabled: !test.validator, noChange: test.noChange, recordError: test.recordError, validation: &receipt}
			if test.securityAudit {
				execution.securityAudit = &NativeSecurityAuditEvidence{ReviewID: "review_security_accepted", VersionID: "version_example", Repository: "example-org/example-project", BaseSHA: "base", HeadSHA: "head", DiffDigest: "digest", Verdict: "pass", SessionID: 123, ReviewerVersion: "2", ReviewerDigest: "security-reviewer-digest"}
				backend.securityAudit = execution.securityAudit
				if test.acceptanceRework {
					backend.validatorResponse = `{"verdict":"rework","score":0,"summary":"Behavior remains unverified despite a passing security audit","criteria_evidence":[{"criterion":"Preserve existing behavior","kind":"not_verified","reference":"Missing behavioral evidence"}]}`
				}
			}
			if test.wrapped {
				execution.head = receipt.HeadSHA
			}
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
				execution.validation = &gate.CommandResult{TreeSHA: candidateTree}
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
			criterion := "Preserve existing behavior"
			if test.unsatisfiable {
				criterion = "a real ChatGPT custom connector signs in against production"
				backend.validatorResponse = fmt.Sprintf(`{"verdict":"pass","score":1,"summary":"Requires production","criteria_evidence":[{"criterion":%q,"kind":"not_verified","unverified_class":"unsatisfiable","reference":"Requires a real third-party client against production"}]}`, criterion)
			}
			err = r.validateNativeChange(t.Context(), RunRequest{Execution: reviewExecution, Issue: connector.Issue{ID: "wi_example", Identifier: "prj_example#1", Description: "## Acceptance criteria\n- " + criterion}})
			if (err != nil) != (test.recordError != nil) {
				t.Fatalf("source verdict became a run failure or recording failure was ignored: %v", err)
			}
			want := []string{"review version", "review-model", "record review"}
			if test.verdict == "pass" && test.validator && test.recordError == nil && !test.gateFailure {
				want = append(want, "validator version", "validator-model", "record validator")
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("native completion omitted or reordered independent review: got %v, want %v", events, want)
			}
			if test.unsatisfiable && (execution.validatorResult.Verdict != gate.ValidatorVerdictWait || !reflect.DeepEqual(execution.validatorResult.UnsatisfiableCriteria(), []string{criterion})) {
				t.Fatalf("unsatisfiable acceptance did not publish wait with the exact criterion: %+v", execution.validatorResult)
			}
			if test.acceptanceRework && execution.validatorResult.Verdict != gate.ValidatorVerdictRework {
				t.Fatalf("accepted security audit overrode independent acceptance: %+v", execution.validatorResult)
			}
			wantVerdict := test.verdict
			if test.gateFailure {
				wantVerdict = gate.ValidatorVerdictWait
				if len(execution.reviewResult.Commands) == 0 || execution.reviewResult.Commands[len(execution.reviewResult.Commands)-1].ExitCode == 0 {
					t.Fatal("failed linter validation did not retain its failed receipt")
				}
			}
			if execution.reviewResult.Verdict != wantVerdict {
				t.Fatalf("source review verdict = %q, want %q", execution.reviewResult.Verdict, wantVerdict)
			}
			if len(worktree.commands) != test.wantRuns {
				t.Fatalf("applicable evidence was rerun or invalid evidence skipped execution: commands=%v want executions=%d", worktree.commands, test.wantRuns)
			}
			if test.wrapped {
				invocations, err := os.ReadFile(filepath.Join(worktree.info.Path, ".git", "invocations"))
				if err != nil || string(invocations) != strings.Repeat("build\n", 1+test.wantRuns) {
					t.Fatalf("landing validation repeated: %q %v", invocations, err)
				}
				if !test.unsupported && receipt.Evidence.Environment.GoVersion != "go1.26.9" {
					t.Fatalf("receipt captured ambient toolchain: %+v", receipt.Evidence)
				}
				if receipt.Evidence.InputsSHA != "" {
					t.Fatal("observed local command received a reusable identity")
				}
				if test.linterState == "replaced" || test.wantRuns == 0 {
					invocations, err := os.ReadFile(filepath.Join(worktree.info.Path, ".git", "linter-invocations"))
					want := "original\n"
					if test.linterState == "replaced" {
						want += strings.Repeat("replacement\n", test.wantRuns)
					}
					if err != nil || string(invocations) != want {
						t.Fatalf("linter validation invocations = %q, want %q: %v", invocations, want, err)
					}
				}
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
