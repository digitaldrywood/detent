//go:build !windows

package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/procstart"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestNativeImplementationReviewPrecedesAcceptanceValidator(t *testing.T) {
	isolateAttemptDiffGitConfig(t)
	for _, test := range []struct {
		name             string
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

				t.Setenv("CHECK_LAND_BASE", "origin/develop")
				t.Setenv("MAKEFLAGS", "")
				t.Setenv("MFLAGS", "")
				source := initRunnerSourceRepo(t)
				writeRunnerFiles(t, source, map[string]string{
					"internal/invariants/sqlite_policy.json":  "{\"files\":{},\"modules\":{}}\n",
					"internal/invariants/tenancy_policy.json": "{\"test_support\":[],\"exceptions\":{},\"debt\":{}}\n",
					"internal/invariants/tenancy_test.go":     "package invariants\n",
				})
				runRunnerGit(t, source, "add", "internal/invariants")
				runRunnerGit(t, source, "commit", "-m", "initialize storage policy")
				for _, name := range []string{"Makefile", "scripts/check-landing.sh", "scripts/check-evidence.sh", "scripts/check-sqlite-policy.py", "scripts/check-tenancy-policy.py", "scripts/check-landing.py"} {
					content, err := os.ReadFile(filepath.Join("..", "..", name))
					if err != nil {
						t.Fatal(err)
					}
					if test.unsupported && !test.shadow && name == "scripts/check-landing.sh" {
						content = append(content, []byte("\nprintf 'custom wrapper\\n'\n")...)
					}
					writeRunnerFiles(t, source, map[string]string{name: string(content)})
					if test.shadow && name == "Makefile" {
						writeRunnerFiles(t, source, map[string]string{"GNUmakefile": string(content)})
					}
				}
				writeRunnerFiles(t, source, map[string]string{
					"go.mod":            "module example.test/project\n\ngo 1.26.0\ntoolchain go1.26.9\n",
					".gitignore":        "tmp/\nnode_modules/\n/static/app/conversation/\n/docs/config.md/\n",
					".golangci-version": "v2.14.0\n",
					"main.go":           "package main\nfunc main() {}\n",
				})
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
elif [ "$1" = test ]; then
    python3 - <<'PY'
import json
import runpy

for package, tests in runpy.run_path("scripts/check-landing.py")["REQUIRED_POSTGRES"].items():
    for test in tests:
        for action in ("run", "pass"):
            print(json.dumps({"Action": action, "Package": package, "Test": test}))
    print(json.dumps({"Action": "pass", "Package": package}))
PY
fi
`
				if err := os.WriteFile(filepath.Join(bin, "go"), []byte(goTool), 0o700); err != nil {
					t.Fatal(err)
				}
				for _, tool := range []string{"initdb", "postgres"} {
					if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
						t.Fatal(err)
					}
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
			r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Store: &fakeSessionStore{sessionID: 73},
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
			execution := &implementationReviewExecution{events: &events, validatorDisabled: !test.validator, recordError: test.recordError, validation: &receipt}
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

func writeRunnerFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReview1973ProviderGroupRequiresSessionAcknowledgement(t *testing.T) {
	ctx := workspace.WithAttemptWorkspace(t.Context(), workspace.AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_11111111111111111111111111111111"})
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "read -r release")
	procgroup.Configure(ctx, cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := procgroup.Start(cmd); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = procgroup.Wait(cmd)
	}()
	identity, err := procgroup.Inspect(cmd)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"validator", "audit"} {
		t.Run(role, func(t *testing.T) {
			writeErr := errors.New("worker registration unavailable")
			owner := &review1973ProviderProcessStore{writeErr: writeErr}
			runner := &Runner{store: owner}
			worker := &sessionWorkerRegistration{sessionID: 1, cleanupRoot: "checkout"}
			start := runner.acknowledgeProviderProcess(ctx, worker, "")
			if err := start(ctx, identity, "scratch"); !errors.Is(err, writeErr) {
				t.Errorf("unacknowledged process registration returned %v", err)
			}
			if matches, err := procstart.Matches(identity.PID, owner.registered.StartKey); err != nil || !matches {
				t.Errorf("worker birth key does not match the blocked group: %v %v", matches, err)
			}
			owner.writeErr = nil
			if err := start(ctx, identity, "scratch"); err != nil || owner.writes != 2 {
				t.Fatalf("registration retry: writes=%d error=%v", owner.writes, err)
			}
			if err := runner.persistSessionWorkerProcess(ctx, worker, identity, "scratch"); err != nil || owner.writes != 2 {
				t.Fatalf("launch telemetry repeated an acknowledged registration: writes=%d error=%v", owner.writes, err)
			}
		})
	}
}

type review1973ProviderProcessStore struct {
	SessionStore
	writeErr   error
	registered store.WorkerProcessRegistration
	writes     int
}

func (s *review1973ProviderProcessStore) UpdateSessionWorkerProcess(_ context.Context, _ int64, registration store.WorkerProcessRegistration) error {
	s.registered = registration
	s.writes++
	return s.writeErr
}
