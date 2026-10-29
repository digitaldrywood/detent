package runner

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type retainedReviewExecution struct {
	testExecution
	source  workspace.ChangeSource
	input   NativeValidation
	reviews []gate.ValidatorResult
}

func (e *retainedReviewExecution) ValidatorVersion(context.Context) (NativeValidation, error) {
	return e.input, nil
}

func (e *retainedReviewExecution) ReviewVersion(context.Context) (NativeValidation, error) {
	return e.input, nil
}

func (e *retainedReviewExecution) RecordValidator(ctx context.Context, result gate.ValidatorResult) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	e.reviews = append(e.reviews, result)
	return nil
}

func (e *retainedReviewExecution) RecordReview(ctx context.Context, result gate.ValidatorResult) error {
	return e.RecordValidator(ctx, result)
}

func (*retainedReviewExecution) SetChangeSource(func(context.Context, string, string) (tracker.ChangeSourceCapture, error)) {
}

func (e *retainedReviewExecution) RecoverChangeSource(ctx context.Context) (workspace.ChangeSource, error) {
	return e.source, e.Validate(ctx)
}

func TestNativeReviewPreservesImplementingSource(t *testing.T) {
	isolateAttemptDiffGitConfig(t)
	for _, test := range []struct {
		name        string
		dirty       bool
		transferred bool
		ssh         bool
		refusal     string
		retry       bool
	}{
		{name: "clean"},
		{name: "dirty", dirty: true},
		{name: "transferred bundle", dirty: true, transferred: true},
		{name: "SSH retained bundle", dirty: true, transferred: true, ssh: true},
		{name: "missing bundle", dirty: true, refusal: "bundle"},
		{name: "changed version", dirty: true, refusal: "version"},
		{name: "changed head", dirty: true, refusal: "head"},
		{name: "stale authority", dirty: true, refusal: "authority"},
		{name: "retry retained version", dirty: true, retry: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := initRunnerSourceRepo(t)
			base := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
			repository := "https://github.com/example-org/example-project"
			runRunnerGit(t, source, "remote", "add", "origin", repository)
			runRunnerGit(t, source, "config", "url."+source+".insteadOf", repository)
			local, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := connector.Issue{ID: "wi_example", Identifier: "prj_example#1", BranchName: "detent/implementing"}
			implementing, err := local.Create(t.Context(), workspaceIssue(defaultProjectID, issue))
			if err != nil {
				t.Fatal(err)
			}
			published := source
			if test.transferred {
				published = filepath.Join(t.TempDir(), "published")
				runRunnerGit(t, source, "clone", "--no-local", source, published)
				runRunnerGit(t, published, "config", "user.name", "Test User")
				runRunnerGit(t, published, "config", "user.email", "test@example.com")
			}
			attemptDiffFile(t, published, "README.md", "published immutable source\n")
			runRunnerGit(t, published, "add", "README.md")
			runRunnerGit(t, published, "commit", "-m", "published source")
			head := strings.TrimSpace(runRunnerGit(t, published, "rev-parse", "HEAD"))
			capture, err := workspace.CaptureChangeSource(t.Context(), published, base, head)
			if err != nil {
				t.Fatal(err)
			}
			version := tracker.ChangeVersion{ID: "version_example", ChangeID: "change_example", ChangeVersionInput: tracker.ChangeVersionInput{Repository: repository, BaseSHA: base, HeadSHA: head, Source: &capture.Source}}
			version.Policy.Gates.Validator = true
			version.Policy.Gates.AutomatedReview = gate.AutomatedReviewRequired
			if test.dirty {
				attemptDiffFile(t, implementing.Path, "README.md", "staged source\n")
				runRunnerGit(t, implementing.Path, "add", "README.md")
				attemptDiffFile(t, implementing.Path, "README.md", "unstaged source\n")
				attemptDiffFile(t, implementing.Path, "untracked.txt", "untracked source\n")
			}
			before := []string{runRunnerGit(t, implementing.Path, "status", "--porcelain"), runRunnerGit(t, implementing.Path, "diff", "--binary"), runRunnerGit(t, implementing.Path, "diff", "--cached", "--binary")}
			backend := &fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateMessageDelta, Delta: `{"verdict":"pass","score":1,"summary":"Reviewed immutable source","criteria_evidence":[]}`}}}
			r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{Tracker: config.Tracker{Kind: config.TrackerHubNative}, Gate: gate.Config{Run: "git diff --exit-code", SecurityAudit: gate.SecurityAuditConfig{Enabled: false}, AutomatedReview: gate.AutomatedReviewRequired, Validator: gate.ValidatorConfig{Enabled: true}}, Agents: config.Agents{Backends: []config.AgentBackend{{ID: "codex", Kind: "codex", Protocol: "app-server", Command: "codex app-server"}}, Routes: []config.AgentRoute{{Name: "review", Role: RoleCode, Backend: "codex", Model: "example-model"}, {Name: "validator", Role: RoleValidator, Backend: "codex", Model: "example-model"}}}}, Prompt: "Review"}, Workspace: local, AgentBackend: backend})
			if err != nil {
				t.Fatal(err)
			}
			execution := &retainedReviewExecution{source: workspace.ChangeSource{Version: version, Bundle: capture.Bundle, Transferred: test.transferred}, input: NativeValidation{Version: &version, Diff: &connector.ValidationDiff{Repository: repository, BaseSHA: base, HeadSHA: head, Digest: capture.Source.DiffSHA256, Patch: capture.Patch}}}
			var reviewExecution Execution = execution
			if test.ssh {
				left, right := net.Pipe()
				central := NewSSHPeer(t.Context(), left, left, NewSSHCallbackHandler(RunRequest{Execution: execution}, nil, nil, nil))
				remote := NewSSHPeer(t.Context(), right, right, nil)
				t.Cleanup(func() { central.Close(); remote.Close(); left.Close(); right.Close() })
				reviewExecution = &sshNativeExecution{sshExecution: &sshExecution{peer: remote}}
			}
			request := RunRequest{Issue: issue, Execution: reviewExecution, workspacePrepared: true}
			if test.retry {
				execution.validateErr = ErrExecutionAuthorityUnavailable
				if err := r.validateNativeChange(t.Context(), request); !errors.Is(err, ErrExecutionAuthorityUnavailable) || backend.calls != 0 || len(execution.reviews) != 0 {
					t.Fatalf("unavailable source authority reached review: err=%v calls=%d reviews=%+v", err, backend.calls, execution.reviews)
				}
				execution.validateErr = nil
			}
			switch test.refusal {
			case "bundle":
				execution.source.Bundle = nil
			case "version":
				execution.source.Version.ID = "version_other"
			case "head":
				execution.source.Version.HeadSHA = base
			case "authority":
				execution.validateErr = ErrExecutionAuthorityUnavailable
			}
			err = r.validateNativeChange(t.Context(), request)
			if test.refusal != "" {
				if err == nil || backend.calls != 0 || len(execution.reviews) != 0 {
					t.Fatalf("unverified source reached review: err=%v calls=%d reviews=%+v", err, backend.calls, execution.reviews)
				}
			} else {
				if err != nil {
					t.Fatalf("native review of B=%s with implementing A=%s: %v", head, base, err)
				}
				if backend.calls != 2 || len(execution.reviews) != 2 || backend.request.Workspace == implementing.Path {
					t.Fatalf("native reviews did not use a separate immutable workspace: calls=%d reviews=%+v path=%s", backend.calls, execution.reviews, backend.request.Workspace)
				}
				for _, review := range execution.reviews {
					if review.VersionID != version.ID || review.HeadSHA != head || review.BaseSHA != base || review.Repository != repository {
						t.Fatalf("review lost immutable identity: %+v", review)
					}
				}
				if got := strings.TrimSpace(runRunnerGit(t, backend.request.Workspace, "rev-parse", "HEAD")); got != head {
					t.Fatalf("review HEAD=%s, want B=%s", got, head)
				}
				if got := runRunnerGit(t, backend.request.Workspace, "status", "--porcelain"); got != "" {
					t.Fatalf("review workspace is dirty: %s", got)
				}
				t.Logf("implementing A=%s preserved; reviewed immutable B=%s tree=%s", base, head, strings.TrimSpace(runRunnerGit(t, backend.request.Workspace, "rev-parse", "HEAD^{tree}")))
			}
			if got := strings.TrimSpace(runRunnerGit(t, implementing.Path, "rev-parse", "HEAD")); got != base {
				t.Fatalf("implementing HEAD=%s, want A=%s", got, base)
			}
			after := []string{runRunnerGit(t, implementing.Path, "status", "--porcelain"), runRunnerGit(t, implementing.Path, "diff", "--binary"), runRunnerGit(t, implementing.Path, "diff", "--cached", "--binary")}
			for i := range before {
				if before[i] != after[i] {
					t.Fatalf("implementing source changed: before=%q after=%q", before[i], after[i])
				}
			}
			if test.dirty {
				data, err := os.ReadFile(filepath.Join(implementing.Path, "untracked.txt"))
				if err != nil || string(data) != "untracked source\n" {
					t.Fatalf("untracked source changed: %q %v", data, err)
				}
			}
		})
	}
}

func TestNativeValidatorReadsPinnedSourceWithoutRemote(t *testing.T) {
	isolateAttemptDiffGitConfig(t)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	source := initRunnerSourceRepo(t)
	base := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
	attemptDiffFile(t, source, "source.txt", strings.Repeat("pinned source\n", 100))
	runRunnerGit(t, source, "add", "source.txt")
	runRunnerGit(t, source, "commit", "-m", "reviewed source")
	head := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
	attemptDiffFile(t, source, "unreviewed.txt", "unreviewed source\n")
	runRunnerGit(t, source, "add", "unreviewed.txt")
	runRunnerGit(t, source, "commit", "-m", "later branch tip")
	other := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
	runRunnerGit(t, source, "checkout", "--detach", head)
	patch := runRunnerGit(t, source, "diff", "--no-ext-diff", "--no-textconv", base, head, "--")
	limit := 64
	if len(patch) <= limit || strings.TrimSpace(runRunnerGit(t, source, "remote")) != "" {
		t.Fatal("fixture must exceed the inline budget and have no remote")
	}
	for _, test := range []struct {
		name        string
		omitted     bool
		wrongHead   bool
		wrongDiff   bool
		wrongBase   bool
		missingDiff bool
		missingID   bool
	}{
		{name: "patch exceeds inline budget"},
		{name: "source owner omitted patch", omitted: true},
		{name: "wrong workspace head", wrongHead: true},
		{name: "wrong diff head", wrongDiff: true},
		{name: "wrong diff base", wrongBase: true},
		{name: "source evidence unavailable", missingDiff: true},
		{name: "version identity unavailable", missingID: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input, err := (&attachmentValidatorExecution{}).ValidatorVersion(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			input.Version.BaseSHA, input.Version.HeadSHA = base, head
			input.Diff.BaseSHA, input.Diff.HeadSHA, input.Diff.Patch = base, head, patch
			input.Diff.Files = []string{"source.txt"}
			observed := head
			if test.omitted {
				input.Diff.Patch = ""
			}
			if test.wrongHead {
				observed = other
			}
			if test.wrongDiff {
				input.Diff.HeadSHA = other
			}
			if test.wrongBase {
				input.Diff.BaseSHA = other
			}
			if test.missingDiff {
				input.Diff = nil
			}
			if test.missingID {
				input.Version.ID = ""
			}
			backend := &fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateMessageDelta, Delta: `{"verdict":"pass","score":1,"summary":"Reviewed pinned source"}`}}}
			runner, err := NewRunner(Dependencies{
				Workflow: config.Workflow{Config: config.Config{
					Tracker: config.Tracker{Kind: config.TrackerHubNative},
					Gate:    gate.Config{Validator: gate.ValidatorConfig{Enabled: true, MaxInlineDiffBytes: &limit}},
					Agents:  config.Agents{Backends: []config.AgentBackend{{ID: "codex", Kind: "codex", Protocol: "app-server", Command: "codex app-server"}}, Routes: []config.AgentRoute{{Name: "validator", Role: RoleValidator, Backend: "codex", Model: "example-model"}}},
				}, Prompt: "Review"},
				Workspace:    &attachmentValidatorWorkspace{reviewMutationWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: source}}, heads: []string{observed}}},
				AgentBackend: backend,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := runner.Validate(t.Context(), ValidatorRequest{Issue: connector.Issue{ID: "wi_example", Identifier: "prj_example#1"}, NativeVersion: input.Version, Diff: input.Diff})
			if test.wrongHead || test.wrongDiff || test.wrongBase || test.missingDiff || test.missingID {
				if !errors.Is(err, ErrValidatorInfrastructure) || result.Submitted || backend.calls != 0 {
					t.Fatalf("unbound source reached review: result=%+v err=%v calls=%d", result, err, backend.calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.VersionID != input.Version.ID || result.BaseSHA != base || result.HeadSHA != head || result.DiffDigest != input.Diff.Digest {
				t.Fatalf("review lost immutable evidence identity: %+v", result)
			}
			prompt := backend.request.Prompt
			if !backend.request.ReadOnly || strings.Contains(prompt, "+pinned source") || strings.Contains(prompt, "use the named GitHub PR diff") || !strings.Contains(prompt, "Full diff omitted") {
				t.Fatal("native omitted diff did not receive a read-only source fallback")
			}
			commands := regexp.MustCompile("(?m)^`(git [^`]+)`$").FindAllStringSubmatch(prompt, -1)
			if len(commands) != 3 {
				t.Fatalf("native source fallback supplied %d commands, want two object checks and a comparison", len(commands))
			}
			for i, command := range commands {
				args := strings.Fields(command[1])
				output := runRunnerGit(t, backend.request.Workspace, args[1:]...)
				switch i {
				case 0:
					if strings.TrimSpace(output) != base {
						t.Fatal("source fallback did not resolve the pinned base")
					}
				case 1:
					if strings.TrimSpace(output) != head {
						t.Fatal("source fallback did not resolve the pinned head")
					}
				case 2:
					if !strings.Contains(output, "+pinned source") || strings.Contains(output, "unreviewed source") {
						t.Fatal("source fallback did not compare only the intended immutable change")
					}
				}
			}
		})
	}
}
