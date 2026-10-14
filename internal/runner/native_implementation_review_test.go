package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
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
	reviewResult      gate.ValidatorResult
	validatorResult   gate.ValidatorResult
	recordError       error
	commands          []gate.CommandResult
	validation        *gate.CommandResult
	discussion        NativeReviewDiscussion
	head              string
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
	if e.head != "" {
		input.Version.HeadSHA = e.head
		input.Diff.HeadSHA = e.head
	}
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
	return AgentTurnResult{}, update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: response})
}

func TestNativeImplementationReviewPrecedesAcceptanceValidator(t *testing.T) {
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
		wrapped      bool
		changedInput string
		unsupported  bool
		shadow       bool
		gateFailure  bool
	}{
		{name: "separate source and acceptance passes reuse source receipt", validator: true, verdict: "pass"},
		{name: "linked app source declines reuse", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "app-link", wantRuns: 2},
		{name: "external npm script shell declines reuse", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "npm-shell", wantRuns: 2},
		{name: "external npm configuration declines reuse", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "npm-config", wantRuns: 2},
		{name: "effective Go execution hook declines reuse", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "toolexec", wantRuns: 2},
		{name: "external CGO source declines reuse", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "cgo-source", wantRuns: 2},
		{name: "external Python source declines reuse", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "python-source", wantRuns: 2},
		{name: "asset file framing changes require command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "asset-framing", wantRuns: 1},
		{name: "npm implementation replacement requires command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "npm-implementation", wantRuns: 1},
		{name: "scope selector Git replacement requires command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "scope-git", wantRuns: 1},
		{name: "selected SDK vet replacement requires command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "sdk-vet", wantRuns: 1},
		{name: "SDK source replacement requires command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "sdk-source", wantRuns: 1},
		{name: "external Node preload declines reuse", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "node-preload", wantRuns: 2},
		{name: "local npm source declines reuse", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "npm-local", wantRuns: 2},
		{name: "persistent external Go flags decline reuse", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "persistent-flags", wantRuns: 2},
		{name: "direct Go launcher replacement requires command", validator: true, verdict: "pass", command: "go build ./...", wrapped: true, changedInput: "relative-go", wantRuns: 1},
		{name: "relative PATH Go replacement requires command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "relative-go", wantRuns: 1},
		{name: "configured shell replacement requires command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "shell", wantRuns: 1},
		{name: "selected compiler replacement requires command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "compiler", wantRuns: 1},
		{name: "local module replacement declines reuse", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "local-replace", wantRuns: 2},
		{name: "external asset source declines reuse", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "external-app", wantRuns: 2},
		{name: "Tailwind dependency replacement requires command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "tailwind", wantRuns: 1},
		{name: "linter changes during validation decline reuse", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "during-lint", wantRuns: 2},
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
		{name: "matching wrapped finalization receipt is reused", validator: true, verdict: "pass", command: "make check-landing", wrapped: true},
		{name: "wrapped receipt survives transport", ssh: true, validator: true, verdict: "pass", command: "make check-landing", wrapped: true},
		{name: "changed effective wrapper toolchain requires command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "toolchain", wantRuns: 1},
		{name: "changed Go flags require command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "flags", wantRuns: 1},
		{name: "changed compiler flags require command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "gcc-flags", wantRuns: 1},
		{name: "changed CGO flags require command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "cgo-flags", wantRuns: 1},
		{name: "changed compiler prefix map target requires command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "prefix-target", wantRuns: 1},
		{name: "replaced tool requires command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "tool", wantRuns: 1},
		{name: "replaced landing linter requires command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "linter", wantRuns: 1},
		{name: "missing landing linter declines reuse", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "missing-linter", gateFailure: true, wantRuns: 1},
		{name: "unexecutable landing linter declines reuse", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "unexecutable-linter", gateFailure: true, wantRuns: 1},
		{name: "moved landing base requires command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, changedInput: "base", wantRuns: 1},
		{name: "changed wrapped tree requires command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, editReceipt: func(r *gate.CommandResult) { r.TreeSHA = strings.Repeat("f", 40) }, wantRuns: 1},
		{name: "failed wrapped receipt requires command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, editReceipt: func(r *gate.CommandResult) { r.ExitCode = 7 }, wantRuns: 1},
		{name: "legacy wrapped environment requires command", validator: true, verdict: "pass", command: "make check-landing", wrapped: true, editReceipt: func(r *gate.CommandResult) { r.Evidence.InputsSHA = "" }, wantRuns: 1},
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
				if test.changedInput == "local-replace" {
					module += "\nreplace example.test/dependency => ../dependency\n"
				}
				if err := os.WriteFile(filepath.Join(source, "go.mod"), []byte(module), 0o600); err != nil {
					t.Fatal(err)
				}
				for name, content := range map[string]string{
					".gitignore":        "tmp/\nnode_modules/\n",
					".golangci-version": "v2.14.0\n",
					"main.go":           "package main\nfunc main() {}\n",
				} {
					if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if test.changedInput == "tailwind" || test.changedInput == "asset-framing" {
					if err := os.MkdirAll(filepath.Join(source, "static", "css"), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(source, "static", "css", "input.css"), []byte("body {}\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(filepath.Join(source, "node_modules", ".bin"), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(source, "node_modules", ".bin", "tailwindcss"), []byte("#!/bin/sh\ncat node_modules/css-runtime >> .git/asset-invocations\n"), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(source, "node_modules", "css-runtime"), []byte("first\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					if test.changedInput == "asset-framing" {
						if err := os.WriteFile(filepath.Join(source, "node_modules", "z-next"), []byte("second\n"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				if test.changedInput == "npm-local" {
					if err := os.MkdirAll(filepath.Join(source, "web", "conversation"), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(source, "web", "conversation", "package.json"), []byte(`{"dependencies":{"external":"file:../../../dependency"}}`), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if test.changedInput == "app-link" {
					app := t.TempDir()
					if err := os.WriteFile(filepath.Join(app, "package.json"), []byte(`{}`), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(filepath.Join(source, "web"), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(app, filepath.Join(source, "web", "conversation")); err != nil {
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
				if test.changedInput == "during-lint" {
					t.Setenv("FIXTURE_SWAP_LINTER", linter)
				}
				if err := os.WriteFile(linter, []byte(linterTool), 0o700); err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				goTool := `#!/bin/sh
version=${FIXTURE_GO_VERSION:-${GOTOOLCHAIN:-go1.27.1}}
if [ "$1" = env ]; then
    if [ "$2" = -json ]; then
        count=0
        if [ -f .git/env-probes ]; then count=$(cat .git/env-probes); fi
        count=$((count + 1))
        echo "$count" > .git/env-probes
        effective_flags=""
        if [ -f .git/effective-go-flags ]; then effective_flags=$(cat .git/effective-go-flags); fi
        flags=-fPIC
        cgo_flags='-O2 -g'
        target=/tmp/go-build
        if [ -f .git/compiler-flags ]; then flags=$(cat .git/compiler-flags); fi
        if [ -f .git/cgo-flags ]; then cgo_flags=$(cat .git/cgo-flags); fi
        if [ -f .git/prefix-target ]; then target=$(cat .git/prefix-target); fi
		printf '{"GOHOSTOS":"linux","GOHOSTARCH":"amd64","GOVERSION":"%s","GOFLAGS":"%s","GOROOT":"%s","GOTOOLDIR":"%s/pkg/tool/linux_amd64","CGO_CFLAGS":"%s","GOGCCFLAGS":"%s -ffile-prefix-map=/scratch/probe-%s/go-build%s=%s -gno-record-gcc-switches"}\n' "$version" "$effective_flags" "$FIXTURE_GOROOT" "$FIXTURE_GOROOT" "$cgo_flags" "$flags" "$count" "$count" "$target"
    else
        case "$2" in GOHOSTOS) echo linux;; GOHOSTARCH) echo amd64;; GOVERSION) echo "$version";; esac
    fi
elif [ "$1" = build ]; then
    echo build >> .git/invocations
    if [ -n "$FIXTURE_SWAP_LINTER" ]; then
        printf '#!/bin/sh\necho replacement >> .git/linter-invocations\n' > "$FIXTURE_SWAP_LINTER"
    fi
elif [ "$1" = list ]; then
    printf '{"Dir":"%s","ImportPath":"example.test/project"}\n' "$PWD"
fi
`
				if err := os.WriteFile(filepath.Join(bin, "go"), []byte(goTool), 0o700); err != nil {
					t.Fatal(err)
				}
				npmRoot := filepath.Join(bin, "npm-package")
				for name, content := range map[string]string{"package.json": `{"name":"npm"}`, "bin/npm-cli.js": "#!/bin/sh\nif [ \"$1\" = config ]; then\n    case \"$3\" in\n        userconfig) printf '%s/.git/npm-userconfig\\n' \"$PWD\";;\n        globalconfig) printf '%s/.git/npm-globalconfig\\n' \"$PWD\";;\n    esac\nfi\n", "lib/cli.js": "first\n"} {
					path := filepath.Join(npmRoot, name)
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(filepath.Join(npmRoot, "bin", "npm-cli.js"), filepath.Join(bin, "npm")); err != nil {
					t.Fatal(err)
				}
				gitTool := ""
				if test.changedInput == "scope-git" {
					realGit, err := exec.LookPath("git")
					if err != nil {
						t.Fatal(err)
					}
					gitTool = "#!/bin/sh\nexec '" + strings.ReplaceAll(realGit, "'", "'\\''") + "' \"$@\"\n"
					if err := os.WriteFile(filepath.Join(bin, "git"), []byte(gitTool), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				toolRoot := filepath.Join(bin, "sdk")
				for _, name := range []string{"bin/go", "pkg/tool/linux_amd64/compile", "pkg/tool/linux_amd64/asm", "pkg/tool/linux_amd64/link", "pkg/tool/linux_amd64/cgo", "pkg/tool/linux_amd64/vet", "src/runtime/fixture.go"} {
					path := filepath.Join(toolRoot, filepath.FromSlash(name))
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("fixture executable\n"), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				t.Setenv("FIXTURE_GOROOT", toolRoot)
				searchPath := bin + string(os.PathListSeparator) + os.Getenv("PATH")
				if test.changedInput == "relative-go" {
					if err := os.MkdirAll(filepath.Join(source, "tmp", "bin"), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(source, "tmp", "bin", "go"), []byte(goTool), 0o700); err != nil {
						t.Fatal(err)
					}
					searchPath = "tmp/bin" + string(os.PathListSeparator) + searchPath
				}
				t.Setenv("PATH", searchPath)
				t.Setenv("GOTOOLCHAIN", "go1.27.1")
				if test.changedInput == "npm-shell" {
					t.Setenv("npm_config_script_shell", filepath.Join(bin, "external-shell"))
				}
				if test.changedInput == "python-source" {
					t.Setenv("PYTHONPATH", filepath.Join(bin, "python-source"))
				}
				if strings.HasPrefix(command, "go ") {
					t.Setenv("GOTOOLCHAIN", "go1.26.9")
				}
				if test.changedInput == "external-app" {
					t.Setenv("APP_DIR", "../app")
				}
				if test.changedInput == "node-preload" {
					t.Setenv("NODE_OPTIONS", "--require=../hook.js")
				}
				options := workspace.LocalGitOptions{Root: filepath.Dir(source), SourceRoot: source}
				if test.changedInput == "shell" {
					options.Hooks.Shell = filepath.Join(bin, "fixture-shell")
					if err := os.WriteFile(options.Hooks.Shell, []byte("#!/bin/sh\nexec /bin/sh \"$@\"\n"), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				local, err := workspace.NewLocalGit(options)
				if err != nil {
					t.Fatal(err)
				}
				worktree.local = local
				worktree.info.Path = source
				head := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
				worktree.heads = []string{head}
				if test.changedInput == "persistent-flags" || test.changedInput == "toolexec" {
					flags := "-overlay=../overlay.json"
					if test.changedInput == "toolexec" {
						flags = "-toolexec=../external-wrapper"
					}
					if err := os.WriteFile(filepath.Join(source, ".git", "effective-go-flags"), []byte(flags), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if test.changedInput == "npm-config" || test.changedInput == "cgo-source" {
					name, content := "npm-userconfig", "script-shell=../external-shell"
					if test.changedInput == "cgo-source" {
						name, content = "cgo-flags", "-include ../external-header.h"
					}
					if err := os.WriteFile(filepath.Join(source, ".git", name), []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				receipt, err = local.RunReviewCommand(t.Context(), worktree.info, workspace.Issue{ProjectID: defaultProjectID, ID: "wi_example", Identifier: "prj_example#1", PullRequestHeadSHA: head}, command)
				if err != nil || receipt.ExitCode != 0 {
					t.Fatalf("host finalization command: %+v %v", receipt, err)
				}
				receipt.TimingStage = "finalization"
				receipt = tracker.PublicGateResult(receipt)
				switch test.changedInput {
				case "asset-framing":
					if err := os.WriteFile(filepath.Join(source, "node_modules", "css-runtime"), []byte("first\n/z-next\n-rw-------\nsecond\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(filepath.Join(source, "node_modules", "z-next")); err != nil {
						t.Fatal(err)
					}
				case "npm-implementation":
					if err := os.WriteFile(filepath.Join(npmRoot, "lib", "cli.js"), []byte("replacement\n"), 0o700); err != nil {
						t.Fatal(err)
					}
				case "scope-git":
					if err := os.WriteFile(filepath.Join(bin, "git"), []byte(gitTool+"\n:\n"), 0o700); err != nil {
						t.Fatal(err)
					}
				case "sdk-vet", "sdk-source":
					path := filepath.Join(toolRoot, "pkg", "tool", "linux_amd64", "vet")
					if test.changedInput == "sdk-source" {
						path = filepath.Join(toolRoot, "src", "runtime", "fixture.go")
					}
					if err := os.WriteFile(path, []byte("replacement\n"), 0o700); err != nil {
						t.Fatal(err)
					}
				case "during-lint":
					if err := os.WriteFile(linter, []byte(linterTool), 0o700); err != nil {
						t.Fatal(err)
					}
				case "relative-go":
					if err := os.WriteFile(filepath.Join(source, "tmp", "bin", "go"), []byte(goTool+"\n:\n"), 0o700); err != nil {
						t.Fatal(err)
					}
				case "shell":
					if err := os.WriteFile(options.Hooks.Shell, []byte("#!/bin/sh\n:\nexec /bin/sh \"$@\"\n"), 0o700); err != nil {
						t.Fatal(err)
					}
				case "compiler":
					if err := os.WriteFile(filepath.Join(toolRoot, "pkg", "tool", "linux_amd64", "compile"), []byte("replacement executable\n"), 0o700); err != nil {
						t.Fatal(err)
					}
				case "tailwind":
					if err := os.WriteFile(filepath.Join(source, "node_modules", "css-runtime"), []byte("replacement\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "toolchain":
					t.Setenv("FIXTURE_GO_VERSION", "go1.26.10")
				case "flags":
					t.Setenv("GOFLAGS", "-tags=changed")
				case "gcc-flags", "cgo-flags", "prefix-target":
					name, value := "compiler-flags", "-fPIC -fno-common"
					switch test.changedInput {
					case "cgo-flags":
						name, value = "cgo-flags", "-O3 -g"
					case "prefix-target":
						name, value = "prefix-target", "/stable/output"
					}
					if err := os.WriteFile(filepath.Join(source, ".git", name), []byte(value), 0o600); err != nil {
						t.Fatal(err)
					}
				case "tool":
					if err := os.WriteFile(filepath.Join(bin, "go"), []byte(goTool+"\n:\n"), 0o700); err != nil {
						t.Fatal(err)
					}
				case "linter":
					if err := os.WriteFile(linter, []byte("#!/bin/sh\necho replacement >> .git/linter-invocations\n"), 0o700); err != nil {
						t.Fatal(err)
					}
				case "missing-linter":
					if err := os.Remove(linter); err != nil {
						t.Fatal(err)
					}
				case "unexecutable-linter":
					if err := os.Chmod(linter, 0o600); err != nil {
						t.Fatal(err)
					}
				case "base":
					runRunnerGit(t, source, "update-ref", "refs/remotes/origin/develop", "HEAD")
				}
			}
			candidateTree := receipt.TreeSHA
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
			err = r.validateNativeChange(t.Context(), RunRequest{Execution: reviewExecution, Issue: connector.Issue{ID: "wi_example", Identifier: "prj_example#1", Description: "## Acceptance criteria\n- Preserve existing behavior"}})
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
				if test.changedInput == "linter" || test.wantRuns == 0 {
					invocations, err := os.ReadFile(filepath.Join(worktree.info.Path, ".git", "linter-invocations"))
					want := "original\n"
					if test.changedInput == "linter" {
						want += "replacement\n"
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
