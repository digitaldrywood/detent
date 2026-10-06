package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type resolvingReworkAgent struct {
	fakeCodexClient
	unresolved  bool
	staged      bool
	signer      string
	beforeHead  string
	beforeStage func()
	afterStage  func()
	t           *testing.T
}

func (a *resolvingReworkAgent) RunTurn(ctx context.Context, req AgentTurnRequest, _ AgentUpdateHandler) (AgentTurnResult, error) {
	a.request = req
	a.calls++
	if a.beforeStage != nil {
		a.beforeStage()
	}
	head, err := exec.CommandContext(ctx, "git", "-C", req.Workspace, "rev-parse", "HEAD").Output()
	if err != nil {
		return AgentTurnResult{}, err
	}
	a.beforeHead = strings.TrimSpace(string(head))
	if !a.unresolved {
		path := "README.md"
		if a.staged {
			path = "repair.md"
		}
		if err := os.WriteFile(filepath.Join(req.Workspace, path), []byte("resolved\n"), 0o600); err != nil {
			return AgentTurnResult{}, err
		}
		if runtime.GOOS == "linux" && os.Getenv("DETENT_TEST_CODEX_SANDBOX") == "1" {
			roots := append([]string{req.Workspace, req.TempDir}, req.ExtraWritableRoots...)
			entries := make([]string, 0, len(roots))
			seen := map[string]bool{}
			for _, root := range roots {
				if !seen[root] {
					entries = append(entries, fmt.Sprintf("%q=true", root))
					seen[root] = true
				}
			}
			profile := `permissions.native-stage-probe={filesystem={"/"="read",":workspace_roots"="write"},workspace_roots={` + strings.Join(entries, ",") + `},network={enabled=false}}`
			probeCtx := isolation.WithPolicy(ctx, isolation.Policy{Tier: isolation.Sandbox, WritableRoots: roots})
			cmd := exec.CommandContext(probeCtx, "codex", "-c", profile, "sandbox", "-P", "native-stage-probe", "-C", req.Workspace, "/bin/sh", "-c", `
target=$(git -C "$1" rev-parse --verify 'HEAD^{commit}^') || exit 4
git -C "$1" diff --cached --quiet -- "$2" || exit 5
git -C "$1" add "$2" || exit 1
[ "$(git -C "$1" diff --cached --name-only -- "$2")" = "$2" ] || exit 6
if git -C "$1" update-ref refs/heads/unrelated "$target" 2>/dev/null; then exit 2; fi
if git -C "$1" update-ref "$(git -C "$1" symbolic-ref HEAD)" "$target" 2>/dev/null; then exit 3; fi
`, "worker", req.Workspace, path)
			procgroup.Configure(probeCtx, cmd)
			procgroup.SetTempDir(cmd, req.TempDir)
			if output, err := cmd.CombinedOutput(); err != nil {
				return AgentTurnResult{}, fmt.Errorf("native Codex staging sandbox: %w: %s", err, output)
			}
		} else if err := runAgentGit(ctx, req.Workspace, "add", path); err != nil {
			return AgentTurnResult{}, err
		}
		if a.signer != "" && runtime.GOOS == "darwin" {
			roots, err := workspace.GitMetadataWritableRoots(ctx, req.Workspace)
			if err != nil {
				return AgentTurnResult{}, err
			}
			// Seatbelt matches resolved paths; macOS temporary directories can use
			// aliases such as /var for /private/var. Deny the actual key target.
			signerPath, err := filepath.EvalSymlinks(a.signer)
			if err != nil {
				return AgentTurnResult{}, err
			}
			profile := fmt.Sprintf("(version 1)(allow default)(deny file-write*)(deny file-read* (literal %q))(allow file-write* (literal %q) (subpath %q) (subpath %q)", signerPath, os.DevNull, req.Workspace, req.TempDir)
			var allowedRoots strings.Builder
			for _, root := range roots {
				allowedRoots.WriteString(fmt.Sprintf(" (subpath %q)", root))
			}
			profile += allowedRoots.String()
			profile += ")"
			cmd := exec.CommandContext(ctx, "sandbox-exec", "-p", profile, "/bin/sh", "-c", `
git -C "$1" add repair.md || exit 1
if cat "$2" >/dev/null 2>&1; then exit 2; fi
if git -C "$1" update-ref refs/heads/unrelated HEAD 2>/dev/null; then exit 3; fi
if git -C "$1" commit -m worker-signing-attempt; then exit 4; fi
`, "worker", req.Workspace, a.signer)
			output, err := cmd.CombinedOutput()
			if err != nil && strings.Contains(string(output), "sandbox_apply: Operation not permitted") {
				a.t.Log("nested sandbox denied; source/index and host signing still exercised, OS denial acceptance pending")
			} else if err != nil {
				return AgentTurnResult{}, fmt.Errorf("sandbox worker: %w: %s", err, output)
			}
		}
	}
	if a.afterStage != nil {
		a.afterStage()
	}
	return AgentTurnResult{ThreadID: "native-rework", TurnID: "1"}, nil
}

type reworkArtifactsExecution struct {
	testExecution
	base            string
	head            string
	diffHead        string
	files           []tracker.AttemptDiffFile
	diffSource      AttemptDiffSource
	denyAuthority   bool
	authorityChecks int
}

func (e *reworkArtifactsExecution) Validate(ctx context.Context) error {
	if e.denyAuthority {
		e.authorityChecks++
		if e.authorityChecks > 1 {
			return ErrExecutionAuthorityUnavailable
		}
	}
	return e.testExecution.Validate(ctx)
}

func (e *reworkArtifactsExecution) SetDiffSource(source AttemptDiffSource)       { e.diffSource = source }
func (*reworkArtifactsExecution) PrepareArtifacts(context.Context, string) error { return nil }
func (*reworkArtifactsExecution) ArtifactLog(context.Context, string) error      { return nil }
func (e *reworkArtifactsExecution) FinalizeArtifacts(ctx context.Context, path string) error {
	diff, err := workspace.GitFileDiffs(ctx, path, e.base, tracker.MaxDiffBytes)
	if err != nil {
		return err
	}
	e.head = diff.HeadSHA
	if source, ok := e.diffSource(ctx); ok {
		e.diffHead, e.files = source.HeadSHA, source.Files
	}
	return nil
}

func TestNativeReworkFinalizesBeforeImmutableEvidence(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name             string
		unresolved       bool
		staged           bool
		normal           bool
		signed           bool
		signingFail      bool
		authorityLost    bool
		lateConflict     bool
		followOnConflict bool
		refusal          string
		baseRefresh      bool
		historical       bool
		laterRefusal     string
		resume           bool
	}{
		{name: "resolved source"},
		{name: "stale base refusal in code lane refreshes owned source", normal: true, staged: true, refusal: workspace.LandRefusalBaseMoved, baseRefresh: true, resume: true},
		{name: "conflict refusal in code lane", normal: true, refusal: workspace.LandRefusalConflict},
		{name: "conflict refusal clean replay resumes", normal: true, staged: true, refusal: workspace.LandRefusalConflict, resume: true},
		{name: "conflict refusal conflicting replay restarts", normal: true, refusal: workspace.LandRefusalConflict, resume: true},
		{name: "non-conflict refusal preserves code session", normal: true, staged: true, refusal: workspace.LandRefusalHeadMoved, resume: true},
		{name: "historical conflict preserves code session", normal: true, staged: true, refusal: workspace.LandRefusalConflict, historical: true, resume: true},
		{name: "latest landing supersedes conflict", normal: true, staged: true, refusal: workspace.LandRefusalConflict, laterRefusal: workspace.LandRefusalBaseMoved, resume: true},
		{name: "unresolved source", unresolved: true},
		{name: "unpaused staged repair", staged: true},
		{name: "preserved staged work with late host conflict", staged: true, lateConflict: true},
		{name: "resolved pause with next host replay conflict", staged: true, lateConflict: true, followOnConflict: true},
		{name: "host signs staged native code", staged: true, normal: true, signed: true},
		{name: "host signs staged Rework", staged: true, signed: true},
		{name: "host signing unavailable", staged: true, signingFail: true},
		{name: "authority lost after staging", staged: true, authorityLost: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := initRunnerSourceRepo(t)
			remote := filepath.Join(t.TempDir(), "origin.git")
			runRunnerGit(t, source, "init", "--bare", "-b", "main", remote)
			runRunnerGit(t, source, "remote", "add", "origin", remote)
			runRunnerGit(t, source, "push", "-u", "origin", "main")
			backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := connector.Issue{ID: "native", Identifier: "native#141", State: "Rework"}
			if test.normal {
				issue.State = "In Progress"
			}
			info, err := backend.Create(t.Context(), workspaceIssue("default", issue))
			if err != nil {
				t.Fatal(err)
			}
			base := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
			if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("feature\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runRunnerGit(t, info.Path, "add", "README.md")
			runRunnerGit(t, info.Path, "commit", "-m", "feature")
			original := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD"))
			runRunnerGit(t, source, "branch", "unrelated", original)
			basePath := "README.md"
			if test.staged && !test.lateConflict {
				basePath = "base.md"
			}
			if err := os.WriteFile(filepath.Join(source, basePath), []byte("base\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runRunnerGit(t, source, "add", basePath)
			if test.followOnConflict {
				if err := os.WriteFile(filepath.Join(source, "repair.md"), []byte("parallel repair\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runRunnerGit(t, source, "add", "repair.md")
			}
			runRunnerGit(t, source, "commit", "-m", "base")
			runRunnerGit(t, source, "push", "origin", "main")
			target := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
			if !test.staged || test.signingFail {
				runRunnerGit(t, source, "config", "commit.gpgsign", "true")
				runRunnerGit(t, source, "config", "gpg.program", filepath.Join(t.TempDir(), "unavailable-personal-signer"))
			}
			signer := ""
			var allowed string
			var signerBefore []byte
			if test.signed {
				signer = filepath.Join(t.TempDir(), "signing-key")
				if output, err := exec.CommandContext(t.Context(), "ssh-keygen", "-t", "ed25519", "-N", "", "-f", signer).CombinedOutput(); err != nil {
					t.Fatalf("create signing fixture: %v: %s", err, output)
				}
				signerBefore, err = os.ReadFile(signer)
				if err != nil {
					t.Fatal(err)
				}
				public, err := os.ReadFile(signer + ".pub")
				if err != nil {
					t.Fatal(err)
				}
				allowed = filepath.Join(t.TempDir(), "allowed-signers")
				if err := os.WriteFile(allowed, append([]byte("test@example.com "), public...), 0o600); err != nil {
					t.Fatal(err)
				}
				runRunnerGit(t, source, "config", "commit.gpgsign", "true")
				runRunnerGit(t, source, "config", "gpg.format", "ssh")
				runRunnerGit(t, source, "config", "user.signingkey", signer)
				runRunnerGit(t, source, "config", "gpg.ssh.allowedSignersFile", allowed)
			}
			configBefore, err := os.ReadFile(filepath.Join(source, ".git", "config"))
			if err != nil {
				t.Fatal(err)
			}
			if test.lateConflict {
				if err := os.WriteFile(filepath.Join(info.Path, "repair.md"), []byte("preserved staged progress\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runRunnerGit(t, info.Path, "add", "repair.md")
			}
			agent := &resolvingReworkAgent{unresolved: test.unresolved, staged: test.staged, signer: signer, t: t}
			execution := &reworkArtifactsExecution{base: base, testExecution: testExecution{recovery: tracker.NativeRecovery{Lease: tracker.NativeLease{PolicyID: "unchanged-policy"}}}}
			request := RunRequest{Mode: RunModeImplement, Issue: issue, Execution: execution}
			prepareConflict := (test.refusal == workspace.LandRefusalConflict || test.baseRefresh) && !test.historical && test.laterRefusal == ""
			if test.refusal != "" {
				local, err := backend.(workspace.RecoveryStateProvider).RecoveryState(t.Context(), info, workspaceIssue("default", issue))
				if err != nil {
					t.Fatal(err)
				}
				checkpoint := executionCheckpoint(&local)
				change := &tracker.NativeChangeReference{ChangeID: "change_1", VersionID: "version_1", HeadSHA: original}
				execution.recovery.Change = change
				execution.recovery.Lease.MachineID = "host"
				landing := &tracker.NativeLandingReceipt{ChangeID: change.ChangeID, VersionID: change.VersionID, HeadSHA: original, RefusalKind: test.refusal}
				if test.baseRefresh {
					landing.BaseSHA = target
				}
				if test.historical {
					landing.VersionID = "previous_version"
				}
				execution.recovery.Attempts = []tracker.NativeAttempt{{Status: "succeeded", NativeRunData: tracker.NativeRunData{MachineID: "host", PolicyID: "unchanged-policy", Runtime: &tracker.NativeRuntimeObservation{Landing: landing}}, Checkpoint: &checkpoint}}
				if test.laterRefusal != "" {
					later := *landing
					later.RefusalKind = test.laterRefusal
					execution.recovery.Attempts = append(execution.recovery.Attempts, tracker.NativeAttempt{Status: "succeeded", NativeRunData: tracker.NativeRunData{MachineID: "host", PolicyID: "unchanged-policy", Runtime: &tracker.NativeRuntimeObservation{Landing: &later}}, Checkpoint: &checkpoint})
				}
				if test.resume {
					checkpoint.Resume = "resume_session"
					execution.recovery.Attempts = append(execution.recovery.Attempts, tracker.NativeAttempt{Status: "succeeded", NativeRunData: tracker.NativeRunData{MachineID: "host", PolicyID: "unchanged-policy", Identity: &tracker.NativeExecutionIdentity{Role: RoleCode, Backend: "codex", Model: "provider_default"}}, Checkpoint: &checkpoint})
					request.RetryMode = RetryModeResume
					request.ResumeState = store.AgentResumeState{ProviderThreadID: "source-thread", ProviderSessionID: "source-session"}
				}
				agent.beforeStage = func() {
					wantBase := base
					if prepareConflict {
						wantBase = target
					}
					if got := strings.TrimSpace(runRunnerGit(t, info.Path, "merge-base", "HEAD", target)); got != wantBase {
						t.Fatalf("agent starts on wrong base: merge-base=%s want=%s", got, wantBase)
					}
					wantResume := test.resume && (!prepareConflict || test.staged)
					if resumed := !agentResumeEmpty(agent.request.Resume); resumed != wantResume {
						t.Fatalf("provider resumed=%v, want %v", resumed, wantResume)
					}
					wantCheckpoint := "fresh_checkout"
					if wantResume {
						wantCheckpoint = "resume_session"
					}
					if execution.checkpoint == nil || execution.checkpoint.Resume != wantCheckpoint {
						t.Fatalf("prepared checkpoint=%+v, want %s", execution.checkpoint, wantCheckpoint)
					}
				}
			}
			var priorCheckpoint *tracker.NativeCheckpoint
			if test.authorityLost {
				agent.afterStage = func() {
					priorCheckpoint = execution.checkpoint
					execution.denyAuthority = true
				}
			}
			r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{Tracker: config.Tracker{Kind: config.TrackerHubNative}}, Prompt: "Resolve the source"}, Workspace: backend, AgentBackend: agent})
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.Run(t.Context(), request)
			if test.signingFail || test.authorityLost {
				want := ErrWorkspacePreparation
				if test.authorityLost {
					want = ErrExecutionAuthorityUnavailable
				}
				if !errors.Is(err, want) || errors.Is(err, workspace.ErrMergeResolutionInvalid) || execution.finish != "failed" || execution.head != "" {
					t.Fatalf("failed finalization lost identity or captured work: %v, %#v", err, execution)
				}
				if head := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD")); head != agent.beforeHead {
					t.Fatal("failed finalization advanced HEAD")
				}
				if staged := strings.TrimSpace(runRunnerGit(t, info.Path, "diff", "--cached", "--name-only")); staged != "repair.md" {
					t.Fatalf("failed finalization lost staged repair: %s", staged)
				}
				if test.signingFail && (execution.checkpoint == nil || execution.checkpoint.WorktreeState != "dirty") {
					t.Fatal("signing failure omitted dirty checkpoint")
				}
				if test.authorityLost && (execution.checkpoint != priorCheckpoint || execution.authorityChecks < 2) {
					t.Fatal("lost authority wrote checkpoint")
				}
				return
			}
			if test.unresolved {
				if !errors.Is(err, workspace.ErrMergeResolutionInvalid) || errors.Is(err, ErrWorkspacePreparation) || execution.finish != "failed" || execution.head != "" {
					t.Fatalf("source conflict published evidence or became infrastructure: %v, %#v", err, execution)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.lateConflict {
				if execution.finish != "succeeded" || execution.head != "" || execution.diffHead != "" || execution.checkpoint == nil || execution.checkpoint.WorktreeState != "dirty" {
					t.Fatalf("late host conflict failed or captured unfinished work: %#v", execution)
				}
				if paths := strings.TrimSpace(runRunnerGit(t, info.Path, "diff", "--name-only", "--diff-filter=U")); paths != "README.md" {
					t.Fatalf("late conflict not preserved: %s", paths)
				}
				if diff, ok := execution.diffSource(t.Context()); ok {
					t.Fatalf("paused index was captured: %#v", diff)
				}
				branchHead := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "refs/heads/"+info.Branch))
				if branchHead == original || !strings.Contains(runRunnerGit(t, info.Path, "show", branchHead+":repair.md"), "resolved") {
					t.Fatal("host did not preserve the successful staged repair")
				}
				agent.staged = false
				result, err := r.Run(t.Context(), RunRequest{Mode: RunModeImplement, Issue: issue, Execution: execution})
				if err != nil {
					t.Fatal(err)
				}
				if test.followOnConflict {
					if result.FinalState != FinalStateCompleted || result.DiffStats.UnpushedCommits != 1 || execution.finish != "succeeded" || execution.head != "" || execution.checkpoint.WorktreeState != "dirty" {
						t.Fatalf("next host replay conflict erased resolved progress: result=%+v execution=%+v", result, execution)
					}
					if paths := strings.TrimSpace(runRunnerGit(t, info.Path, "diff", "--name-only", "--diff-filter=U")); paths != "repair.md" {
						t.Fatalf("next host replay conflict not preserved: %s", paths)
					}
					agent.staged = true
					if _, err := r.Run(t.Context(), RunRequest{Mode: RunModeImplement, Issue: issue, Execution: execution}); err != nil {
						t.Fatal(err)
					}
				}
			}
			head := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD"))
			if prepareConflict {
				if got := strings.TrimSpace(runRunnerGit(t, info.Path, "merge-base", info.Branch, target)); got != target {
					t.Fatalf("finalized branch retains stale ancestry: merge-base=%s target=%s", got, target)
				}
			}
			if head == original || execution.head != head || execution.diffHead != head || execution.checkpoint == nil || execution.checkpoint.HeadSHA != head || execution.finish != "succeeded" {
				t.Fatalf("immutable evidence missed final head %s: %#v", head, execution)
			}
			path := "README.md"
			count := 1
			if test.staged {
				path, count = "repair.md", 2
			}
			found := false
			for _, file := range execution.files {
				found = found || file.Path == path && strings.Contains(file.Patch, "+resolved")
			}
			if len(execution.files) != count || !found {
				t.Fatalf("final diff authority = %#v", execution.files)
			}
			if execution.recovery.Lease.PolicyID != "unchanged-policy" {
				t.Fatal("rework changed policy identity")
			}
			if test.signed {
				runRunnerGit(t, info.Path, "verify-commit", head)
				signerAfter, err := os.ReadFile(signer)
				if err != nil || string(signerBefore) != string(signerAfter) {
					t.Fatal("host finalization changed signer material")
				}
			}
			if got := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "unrelated")); got != original {
				t.Fatal("finalization moved unrelated ref")
			}
			configAfter, err := os.ReadFile(filepath.Join(source, ".git", "config"))
			if err != nil || string(configBefore) != string(configAfter) {
				t.Fatal("finalization changed host signing configuration")
			}
			if !test.normal && !strings.Contains(agent.request.Prompt, "runner owns native rebase") {
				t.Fatal("worker was left owning Git transactions")
			}
		})
	}
}
