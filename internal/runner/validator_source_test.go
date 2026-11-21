package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runner/runworkspace"
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

type continuedReviewExecution struct {
	deadlineRoundExecution
	phase tracker.NativePhaseReceipt
}

func (*continuedReviewExecution) PrepareReview(context.Context, tracker.NativeChangeReference) error {
	return nil
}

func (e *continuedReviewExecution) PhaseReceipts(context.Context, string) (*tracker.NativePhaseContinuation, error) {
	return &tracker.NativePhaseContinuation{LastPushed: &e.phase}, nil
}

type retainedLocalReviewWorkspace struct {
	*workspace.LocalGit
	uses int
}

func (w *retainedLocalReviewWorkspace) Use(ctx context.Context, issue workspace.Issue) (func(), error) {
	w.uses++
	if w.uses > 1 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		cancel()
	}
	return w.LocalGit.Use(ctx, issue)
}

func TestResumeReviewWorkspaceRetainsExactSource(t *testing.T) {
	isolateAttemptDiffGitConfig(t)
	source := initRunnerSourceRepo(t)
	hookLog := filepath.Join(t.TempDir(), "hooks")
	local, err := workspace.NewLocalGit(workspace.LocalGitOptions{
		ProcessSource: emptyProcessSource, Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true,
		Hooks: workspace.Hooks{AfterCreate: "printf 'created\\n' >> " + strconv.Quote(hookLog)},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{projectID: "prj_example", store: &scratchReapSessionStore{fakeSessionStore: &fakeSessionStore{sessionID: 1}}}
	req := RunRequest{
		Issue:            connector.Issue{ID: "wi_example", Identifier: "prj_example#1"},
		AttemptWorkspace: workspace.AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_11111111111111111111111111111111"},
	}
	target, _ := req.workspaceTarget(t.Context(), r.projectID)
	resume := NativeReviewContinuation{Source: tracker.NativeChangeReference{HeadSHA: strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))}}
	if _, _, err := r.resumeReviewWorkspace(t.Context(), req, local, resume); !errors.Is(err, workspace.ErrMissingWorkspace) {
		t.Fatalf("review created or accepted a missing workspace: %v", err)
	}
	ctx, stop := runworkspace.WithHooks(r.WorkerCommandContext(t.Context(), req))
	info, err := local.Create(ctx, target)
	stop(&err)
	if err != nil {
		t.Fatal(err)
	}
	retained, release, err := r.resumeReviewWorkspace(t.Context(), req, local, resume)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if retained.info.Path != info.Path {
		t.Fatalf("review workspace = %q, want %q", retained.info.Path, info.Path)
	}
	t.Run("continuation with a matching pushed phase", func(t *testing.T) {
		head, tree, err := local.ItemPhaseHead(t.Context(), info, target)
		if err != nil {
			t.Fatal(err)
		}
		lease := tracker.NativeLease{ID: "lease_example", PolicyID: "policy_example", FencingToken: 1}
		execution := &continuedReviewExecution{phase: tracker.NativePhaseReceipt{
			Mutation:  tracker.Mutation{LeaseID: lease.ID, FencingToken: lease.FencingToken},
			AttemptID: req.AttemptWorkspace.Attempt, PolicyID: lease.PolicyID, Phase: "project_gate",
			Branch: info.PublishBranch(), HeadSHA: head, TreeSHA: tree,
		}}
		execution.recovery.Lease = lease
		execution.checkpoint = &tracker.NativeCheckpoint{HeadSHA: head}
		execution.finalized = true
		source := &retainedLocalReviewWorkspace{LocalGit: local}
		agent := &deadlineRoundBackend{run: func(_ context.Context, _ AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
			return AgentTurnResult{}, update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: `{"action":"continue","next_phase":"implement","reason":"continue the existing work"}`})
		}}
		runner, err := NewRunner(Dependencies{ProjectID: r.projectID, ProcessSource: emptyProcessSource, Workspace: source, AgentBackend: agent, Store: r.store})
		if err != nil {
			t.Fatal(err)
		}
		runner.sleepInhibitor = nil
		request := req
		request.Execution = execution
		request.Mode = RunModeImplement
		request.RetainItemWorkspace = true
		request.DeferExecutionFinish = true
		continuation := resume
		continuation.Source.VersionID = "version_example"
		request.NativeReviewContinuation = &continuation
		result, err := runner.Run(t.Context(), request)
		if err != nil || result.WorkspaceBranch != info.Branch || source.uses != 1 {
			t.Fatalf("review did not reuse the exact held workspace: branch=%q acquisitions=%d error=%v", result.WorkspaceBranch, source.uses, err)
		}
	})

	other := resume
	other.Source.HeadSHA = strings.Repeat("a", 40)
	if _, _, err := r.resumeReviewWorkspace(t.Context(), req, local, other); !errors.Is(err, ErrNativeRecoveryRequired) {
		t.Fatalf("review accepted a different head: %v", err)
	}
	attemptDiffFile(t, info.Path, "README.md", "unpublished change\n")
	if _, _, err := r.resumeReviewWorkspace(t.Context(), req, local, resume); !errors.Is(err, ErrNativeRecoveryRequired) {
		t.Fatalf("review accepted a dirty workspace: %v", err)
	}
	if hooks, err := os.ReadFile(hookLog); err != nil || string(hooks) != "created\n" {
		t.Fatalf("workspace creation hooks = %q, %v", hooks, err)
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
			worktree := &attachmentValidatorWorkspace{reviewMutationWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: source}}, heads: []string{observed}}}
			runner, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Store: &fakeSessionStore{sessionID: 73},
				Workflow: config.Workflow{Config: config.Config{
					Tracker: config.Tracker{Kind: config.TrackerHubNative},
					Gate:    gate.Config{Validator: gate.ValidatorConfig{Enabled: true, MaxInlineDiffBytes: &limit}},
					Agents:  config.Agents{Backends: []config.AgentBackend{{ID: "codex", Kind: "codex", Protocol: "app-server", Command: "codex app-server"}}, Routes: []config.AgentRoute{{Name: "validator", Role: RoleValidator, Backend: "codex", Model: "example-model"}}},
				}, Prompt: "Review"},
				Workspace:    worktree,
				AgentBackend: backend,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := runner.Validate(t.Context(), ValidatorRequest{Issue: connector.Issue{ID: "wi_example", Identifier: "prj_example#1"}, NativeVersion: input.Version, Diff: input.Diff, retainedReview: &retainedReviewWorkspace{backend: worktree, info: worktree.info}})
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
