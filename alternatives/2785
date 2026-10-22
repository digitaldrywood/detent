package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type resolvingReworkAgent struct {
	fakeCodexClient
	preserved     bool
	unresolved    bool
	staged        bool
	signer        string
	beforeHead    string
	reviewCalls   int
	reviewVerdict string
	beforeStage   func()
	afterStage    func()
	t             *testing.T
}

func (a *resolvingReworkAgent) RunTurn(ctx context.Context, req AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	a.request = req
	if req.ReadOnly {
		a.reviewCalls++
		return AgentTurnResult{}, update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: fmt.Sprintf(`{"verdict":%q,"score":1,"summary":"Current source reviewed","criteria_evidence":[]}`, a.reviewVerdict)})
	}
	a.calls++
	if a.beforeStage != nil {
		a.beforeStage()
	}
	head, err := exec.CommandContext(ctx, "git", "-C", req.Workspace, "rev-parse", "HEAD").Output()
	if err != nil {
		return AgentTurnResult{}, err
	}
	a.beforeHead = strings.TrimSpace(string(head))
	if a.preserved {
		conflicts, err := exec.CommandContext(ctx, "git", "-C", req.Workspace, "diff", "--name-only", "--diff-filter=U").Output()
		if err != nil {
			return AgentTurnResult{}, err
		}
		_, targetErr := os.Stat(filepath.Join(req.Workspace, "target-fix.md"))
		if len(conflicts) != 0 || targetErr != nil {
			return AgentTurnResult{}, update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: "\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: stale_base\nblockers: []\nhuman_action: null\n```"})
		}
		return AgentTurnResult{ThreadID: "native-rework", TurnID: "1"}, nil
	}
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
				fmt.Fprintf(&allowedRoots, " (subpath %q)", root)
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
	validation      *gate.CommandResult
	base            string
	head            string
	diffHead        string
	files           []tracker.AttemptDiffFile
	diffSource      AttemptDiffSource
	denyAuthority   bool
	authorityChecks int
}

type reworkValidationExecution struct {
	*reworkArtifactsExecution
}

func (e *reworkValidationExecution) RecordSourceValidation(ctx context.Context, receipt gate.CommandResult) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	e.validation = &receipt
	return nil
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

type reworkReviewExecution struct {
	reworkArtifactsExecution
	source          workspace.ChangeSource
	body            string
	reviews         []gate.ValidatorResult
	acceptance      []gate.ValidatorResult
	recordError     error
	recoverError    error
	activitySession int64
	repairTree      string
}

func (*reworkReviewExecution) SetChangeSource(func(context.Context, string, string) (tracker.ChangeSourceCapture, error)) {
}
func (e *reworkReviewExecution) RecoverChangeSource(context.Context) (workspace.ChangeSource, error) {
	return e.source, e.recoverError
}
func (e *reworkReviewExecution) NativeChange() *NativeChange {
	change := &NativeChange{Changed: true, ChangeID: e.source.Version.ChangeID, VersionID: e.source.Version.ID, HeadSHA: e.source.Version.HeadSHA}
	if len(e.reviews) > 0 {
		result := e.reviews[len(e.reviews)-1]
		change.ImplementationReview = &result
	}
	return change
}
func (e *reworkReviewExecution) FinalizeArtifacts(ctx context.Context, path string) error {
	if err := e.reworkArtifactsExecution.FinalizeArtifacts(ctx, path); err != nil {
		return err
	}
	if e.repairTree != "" {
		diff, ok := e.diffSource(ctx)
		if !ok {
			return errors.New("repair diff unavailable")
		}
		e.source.Version.ID = "version_repaired"
		e.source.Version.HeadSHA, e.source.Version.BaseSHA = diff.HeadSHA, diff.BaseSHA
		e.source.Version.Validation = e.validation
	}
	return nil
}
func (e *reworkReviewExecution) ObserveRuntime(ctx context.Context, observation tracker.NativeRuntimeObservation) error {
	if observation.Activity != nil {
		raw, err := json.Marshal(observation)
		if err != nil {
			return err
		}
		var submitted tracker.NativeRuntimeObservation
		if err := json.Unmarshal(raw, &submitted); err != nil {
			return err
		}
		if submitted.Activity.AttemptID != submitted.LocalAttemptID || submitted.Activity.Generation != submitted.Generation {
			return fmt.Errorf("serialized activity rejected: attempt=%d/%d generation=%d/%d", submitted.Activity.AttemptID, submitted.LocalAttemptID, submitted.Activity.Generation, submitted.Generation)
		}
		e.activitySession = submitted.Activity.SessionID
	}
	return e.testExecution.ObserveRuntime(ctx, observation)
}
func (e *reworkReviewExecution) RecordSourceValidation(_ context.Context, receipt gate.CommandResult) error {
	if e.repairTree != "" {
		if receipt.HeadSHA == "" || receipt.HeadSHA == e.source.Version.HeadSHA || receipt.TreeSHA != e.repairTree {
			return errors.New("source validation lost the staged repair")
		}
	} else if receipt.HeadSHA != e.source.Version.HeadSHA || receipt.TreeSHA != e.source.Version.Validation.TreeSHA {
		return errors.New("review source validation changed immutable source")
	}
	e.validation = &receipt
	return nil
}
func (e *reworkReviewExecution) PrepareFinish(_ context.Context, _ string, body string, _ *tracker.NativeTerminalFailure) error {
	e.body = body
	return nil
}
func (e *reworkReviewExecution) ReviewVersion(ctx context.Context) (NativeValidation, error) {
	diff, ok := e.diffSource(ctx)
	if !ok {
		return NativeValidation{}, errors.New("source diff unavailable")
	}
	return NativeValidation{Version: &e.source.Version, Diff: &connector.ValidationDiff{Repository: e.source.Version.Repository, BaseSHA: diff.BaseSHA, HeadSHA: diff.HeadSHA, Digest: "fixture-diff"}}, nil
}
func (e *reworkReviewExecution) RecordReview(_ context.Context, result gate.ValidatorResult) error {
	if e.recordError != nil {
		return e.recordError
	}
	e.reviews = append(e.reviews, result)
	return nil
}
func (e *reworkReviewExecution) ValidatorVersion(ctx context.Context) (NativeValidation, error) {
	return e.ReviewVersion(ctx)
}
func (e *reworkReviewExecution) RecordValidator(_ context.Context, result gate.ValidatorResult) error {
	e.acceptance = append(e.acceptance, result)
	return nil
}

func TestNativeReworkFinalizesBeforeImmutableEvidence(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name                     string
		reviewResume             string
		producerFailure          bool
		resumeTwice              bool
		writableResume           bool
		sourcePartial            bool
		laterPartial             bool
		sourceRework             bool
		sourceMismatch           bool
		reviewEvidence           string
		feedback                 string
		humanChanges             bool
		optionalAcceptance       bool
		summaryFailure           bool
		laterFinalizationSuccess bool
		landingRepair            *tracker.NativeLandingReceipt
		historyRefusal           bool
		unresolved               bool
		staged                   bool
		normal                   bool
		signed                   bool
		signingFail              bool
		authorityLost            bool
		resolvedPause            bool
		pausedLaterConflict      bool
		failedFinalGate          bool
		lateConflict             bool
		followOnConflict         bool
		refusal                  string
		baseRefresh              bool
		historical               bool
		laterRefusal             string
		resume                   bool
	}{
		{name: "resolved source"},
		{name: "resolved pause integrates target before validation with two remaining picks", resolvedPause: true},
		{name: "resolved pause preserves later replay conflict", resolvedPause: true, pausedLaterConflict: true},
		{name: "resolved pause retains genuine final gate failure", resolvedPause: true, failedFinalGate: true},
		{name: "complete evidence wait resumes review only", reviewResume: "wait", staged: true},
		{name: "complete operator resume in code lane only reviews", reviewResume: "pass", normal: true, staged: true},
		{name: "complete source review and acceptance resume without coding", reviewResume: "pass", staged: true},
		{name: "complete source producer failure preserves zero-turn version", reviewResume: "pass", staged: true, producerFailure: true},
		{name: "complete source repeated explicit evidence wait never codes", reviewResume: "wait", staged: true, resumeTwice: true},
		{name: "human requested current changes retain writable Rework", reviewResume: "pass", staged: true, reviewEvidence: "complete_pair", humanChanges: true, writableResume: true},
		{name: "human requested current changes retain writable code lane", reviewResume: "pass", staged: true, normal: true, reviewEvidence: "complete_pair", humanChanges: true, writableResume: true},
		{name: "completed current reviews retain comment driven Rework", reviewResume: "pass", staged: true, reviewEvidence: "complete_pair", writableResume: true},
		{name: "completed current reviews retain comment driven code lane", reviewResume: "pass", staged: true, normal: true, reviewEvidence: "complete_pair", writableResume: true},
		{name: "completed current reviews refresh newer technical change feedback without coding", reviewResume: "pass", staged: true, reviewEvidence: "complete_pair", feedback: "change"},
		{name: "completed current reviews independently reject newer issue feedback without coding", reviewResume: "rework", staged: true, reviewEvidence: "complete_pair", feedback: "issue"},
		{name: "optional completed source refreshes newer technical feedback without coding", reviewResume: "pass", staged: true, reviewEvidence: "source_pass", optionalAcceptance: true, feedback: "change"},
		{name: "completed current review pair ignores older failed finalization", reviewResume: "pass", staged: true, reviewEvidence: "complete_pair", summaryFailure: true, laterFinalizationSuccess: true, writableResume: true},
		{name: "optional acceptance completed source review retains writable Rework", reviewResume: "pass", staged: true, reviewEvidence: "source_pass", optionalAcceptance: true, writableResume: true},
		{name: "optional acceptance summary refresh failure only resumes evidence", reviewResume: "pass", staged: true, reviewEvidence: "source_pass", optionalAcceptance: true, summaryFailure: true},
		{name: "optional acceptance latest success supersedes old summary failure", reviewResume: "pass", staged: true, reviewEvidence: "source_pass", optionalAcceptance: true, summaryFailure: true, laterFinalizationSuccess: true, writableResume: true},
		{name: "required acceptance missing after current source pass only resumes evidence", reviewResume: "pass", staged: true, reviewEvidence: "source_pass"},
		{name: "historical completed reviews cannot displace current evidence wait", reviewResume: "wait", staged: true, reviewEvidence: "older_pair"},
		{name: "acceptance before latest source review remains missing evidence", reviewResume: "pass", staged: true, reviewEvidence: "pre_source_pass"},
		{name: "latest source wait supersedes old passing review pair", reviewResume: "wait", staged: true, reviewEvidence: "source_wait_after_pair"},
		{name: "rewritten unsatisfiable acceptance resumes retained source without coding", reviewResume: "pass", staged: true, reviewEvidence: "unsatisfiable_acceptance"},
		{name: "latest acceptance wait supersedes old passing review pair", reviewResume: "pass", staged: true, reviewEvidence: "validator_wait_after_pair"},
		{name: "latest acceptance findings supersede old passing review pair", reviewResume: "pass", staged: true, reviewEvidence: "validator_rework_after_pair", writableResume: true},
		{name: "current acceptance findings before source pass still code", reviewResume: "pass", staged: true, reviewEvidence: "validator_rework_before_source", writableResume: true},
		{name: "partial preserved source still codes", reviewResume: "pass", staged: true, sourcePartial: true, writableResume: true},
		{name: "later partial attempt cannot inherit complete source review resume", reviewResume: "pass", staged: true, laterPartial: true, writableResume: true},
		{name: "genuine source review findings still code", reviewResume: "pass", staged: true, sourceRework: true, writableResume: true},
		{name: "failed landing CI after passing reviews still codes", reviewResume: "pass", staged: true, writableResume: true, landingRepair: &tracker.NativeLandingReceipt{CI: &tracker.NativeLandingCIReceipt{State: "failure"}}},
		{name: "failed landing gate after passing reviews still codes", reviewResume: "pass", staged: true, writableResume: true, landingRepair: &tracker.NativeLandingReceipt{GateFailed: true}},
		{name: "protected landing review conversations after passing reviews still code", reviewResume: "pass", staged: true, writableResume: true, landingRepair: &tracker.NativeLandingReceipt{RefusalKind: workspace.LandRefusalProtected, Refusal: workspace.ReviewConversationsRefusalText}},
		{name: "protected landing review conversations retained in history still code", reviewResume: "pass", staged: true, writableResume: true, historyRefusal: true, landingRepair: &tracker.NativeLandingReceipt{RefusalKind: workspace.LandRefusalProtected}},
		{name: "source version race refuses before coding", reviewResume: "pass", staged: true, sourceMismatch: true},
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
			if test.resolvedPause {
				for _, path := range []string{"later-one.md", "later-two.md"} {
					if err := os.WriteFile(filepath.Join(info.Path, path), []byte(path+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					runRunnerGit(t, info.Path, "add", path)
					runRunnerGit(t, info.Path, "commit", "-m", path)
				}
			}
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
			if test.pausedLaterConflict {
				if err := os.WriteFile(filepath.Join(source, "later-one.md"), []byte("parallel later change\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runRunnerGit(t, source, "add", "later-one.md")
			}
			if test.followOnConflict {
				if err := os.WriteFile(filepath.Join(source, "repair.md"), []byte("parallel repair\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runRunnerGit(t, source, "add", "repair.md")
			}
			runRunnerGit(t, source, "commit", "-m", "base")
			runRunnerGit(t, source, "push", "origin", "main")
			if test.resolvedPause {
				ownedIssue := workspaceIssue("default", issue)
				ownedIssue.NativeRework = true
				prepared, err := backend.(workspace.ReworkPreparer).PrepareRework(t.Context(), info, ownedIssue, workspace.MergePrepareOptions{TargetBranch: "main"}, func(ctx context.Context) error { return ctx.Err() })
				if err != nil || prepared.Status != workspace.MergePrepareStatusConflict {
					t.Fatalf("pause original replay: %+v, %v", prepared, err)
				}
				if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("resolved\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runRunnerGit(t, info.Path, "add", "README.md")
				if err := os.WriteFile(filepath.Join(source, "target-fix.md"), []byte("required target fix\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runRunnerGit(t, source, "add", "target-fix.md")
				runRunnerGit(t, source, "commit", "-m", "target validation fix")
				runRunnerGit(t, source, "push", "origin", "main")
				oldGate := exec.CommandContext(t.Context(), "sh", "-c", "test -f target-fix.md && test -f later-one.md && test -f later-two.md")
				oldGate.Dir = info.Path
				if err := oldGate.Run(); err == nil {
					t.Fatal("obsolete paused source unexpectedly passes the final gate")
				}
			}
			target := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
			if !test.staged {
				runRunnerGit(t, source, "config", "commit.gpgsign", "true")
				runRunnerGit(t, source, "config", "gpg.program", filepath.Join(t.TempDir(), "unavailable-personal-signer"))
			}
			signer := ""
			signingUnavailable := ""
			var allowed string
			var signerBefore []byte
			if test.signed || test.signingFail {
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
				if test.signingFail {
					signingUnavailable = filepath.Join(filepath.Dir(signer), "signing-unavailable")
					program := filepath.Join(filepath.Dir(signer), "signer")
					script := fmt.Sprintf("#!/bin/sh\nif [ -f %q ]; then\n  rm -- %q || exit 2\n  exit 1\nfi\nexec ssh-keygen \"$@\"\n", signingUnavailable, signingUnavailable)
					if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
						t.Fatal(err)
					}
					runRunnerGit(t, source, "config", "gpg.ssh.program", program)
				}
				backend, err = workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{Root: filepath.Dir(info.Path), SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				if report := backend.(*workspace.LocalGit).CommitReport(); report.Signing != "ssh" {
					t.Fatalf("signing fixture was not captured at startup: %+v", report)
				}
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
			agent := &resolvingReworkAgent{preserved: test.resolvedPause, unresolved: test.unresolved, staged: test.staged, signer: signer, t: t}
			if test.signingFail {
				agent.signer = ""
				agent.afterStage = func() {
					if err := os.WriteFile(signingUnavailable, nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			execution := &reworkArtifactsExecution{base: base, testExecution: testExecution{recovery: tracker.NativeRecovery{Lease: tracker.NativeLease{PolicyID: "unchanged-policy"}}}}
			request := RunRequest{Mode: RunModeImplement, Issue: issue, Execution: execution}
			if test.resolvedPause {
				request.Execution = &reworkValidationExecution{execution}
			}
			if test.reviewResume != "" {
				request.WorkAttemptID, request.Generation = 41, 7
				runRunnerGit(t, source, "remote", "set-url", "origin", "https://github.com/example-org/example-project")
				runRunnerGit(t, source, "config", "url."+remote+".insteadOf", "https://github.com/example-org/example-project")
				capture, err := workspace.CaptureChangeSource(t.Context(), info.Path, base, original)
				if err != nil {
					t.Fatal(err)
				}
				descriptor := policy.Descriptor{Gates: policy.Gates{AutomatedReview: gate.AutomatedReviewRequired, Validator: !test.optionalAcceptance}}.WithID()
				version := tracker.ChangeVersion{ID: "version_current", ChangeID: "change_current", Policy: descriptor, ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: base, HeadSHA: original, Repository: workspace.RepositoryURL(t.Context(), source), PolicyID: descriptor.ID, RunID: "run_source", AttemptID: "attempt_source", Source: &capture.Source, Validation: &gate.CommandResult{Command: "true", HeadSHA: original, TreeSHA: strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD^{tree}"))}}}
				checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Availability: "available", Storage: "local_only", HeadSHA: original, WorktreeState: "clean", ExternalEffect: "none", EffectState: "none"}
				disposition := &tracker.NativeDisposition{Status: "complete", FinalSummary: "Source complete.\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"}
				execution.recovery = tracker.NativeRecovery{Lease: tracker.NativeLease{PolicyID: descriptor.ID}, Change: &tracker.NativeChangeReference{ChangeID: version.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}, ChangeDetail: &tracker.ChangeDetail{Change: tracker.ChangeRequest{ID: version.ChangeID, CurrentVersion: version.ID}, Versions: []tracker.ChangeVersion{version}, Reviews: []tracker.ChangeReview{{VersionID: version.ID, ImplementationReview: &gate.ValidatorResult{Verdict: gate.ValidatorVerdictWait}}}}, Attempts: []tracker.NativeAttempt{{Status: "succeeded", NativeRunData: tracker.NativeRunData{PolicyID: descriptor.ID, RunID: version.RunID, AttemptID: version.AttemptID, Disposition: disposition, CompletionBody: disposition.FinalSummary}, Checkpoint: &checkpoint}}}
				owner := &reworkReviewExecution{reworkArtifactsExecution: *execution, source: workspace.ChangeSource{Version: version, Bundle: capture.Bundle}}
				review := func(source bool, verdict string, versionID string) tracker.ChangeReview {
					result := &gate.ValidatorResult{VersionID: versionID, HeadSHA: original, Verdict: verdict}
					record := tracker.ChangeReview{VersionID: versionID, CreatedAt: time.Unix(200, 0)}
					if source {
						record.ImplementationReview = result
					} else {
						record.Validator = result
					}
					return record
				}
				sourcePass := review(true, gate.ValidatorVerdictPass, version.ID)
				acceptancePass := review(false, gate.ValidatorVerdictPass, version.ID)
				switch test.reviewEvidence {
				case "source_pass":
					owner.recovery.ChangeDetail.Reviews = []tracker.ChangeReview{sourcePass}
				case "complete_pair":
					owner.recovery.ChangeDetail.Reviews = []tracker.ChangeReview{sourcePass, acceptancePass}
				case "older_pair":
					owner.recovery.ChangeDetail.Reviews = []tracker.ChangeReview{review(true, gate.ValidatorVerdictPass, "version_old"), review(false, gate.ValidatorVerdictPass, "version_old"), review(true, gate.ValidatorVerdictWait, version.ID)}
				case "pre_source_pass":
					owner.recovery.ChangeDetail.Reviews = []tracker.ChangeReview{acceptancePass, sourcePass}
				case "source_wait_after_pair":
					owner.recovery.ChangeDetail.Reviews = []tracker.ChangeReview{sourcePass, acceptancePass, review(true, gate.ValidatorVerdictWait, version.ID)}
				case "unsatisfiable_acceptance":
					acceptance := review(false, gate.ValidatorVerdictWait, version.ID)
					acceptance.Validator.NotVerified = []string{"A real third-party client signs in against production"}
					acceptance.Validator.CriteriaEvidence = []gate.CriterionEvidence{{Criterion: acceptance.Validator.NotVerified[0], Kind: gate.EvidenceNotVerified, UnverifiedClass: gate.CriterionUnsatisfiable}}
					owner.recovery.ChangeDetail.Reviews = []tracker.ChangeReview{sourcePass, acceptance}
				case "validator_wait_after_pair":
					owner.recovery.ChangeDetail.Reviews = []tracker.ChangeReview{sourcePass, acceptancePass, review(false, gate.ValidatorVerdictWait, version.ID)}
				case "validator_rework_after_pair":
					owner.recovery.ChangeDetail.Reviews = []tracker.ChangeReview{sourcePass, acceptancePass, review(false, gate.ValidatorVerdictRework, version.ID)}
				case "validator_rework_before_source":
					owner.recovery.ChangeDetail.Reviews = []tracker.ChangeReview{review(false, gate.ValidatorVerdictRework, version.ID), sourcePass}
				}
				if test.humanChanges {
					owner.recovery.ChangeDetail.Summary.NativeReview = "changes_requested"
					owner.recovery.ChangeDetail.Reviews = append(owner.recovery.ChangeDetail.Reviews, tracker.ChangeReview{VersionID: version.ID, Decision: "changes_requested", Body: "Revise the current source to address the requested behavior.", Actor: tracker.Actor{Kind: "human", PrincipalID: "person_example"}})
				}
				if test.writableResume && !test.humanChanges && (test.reviewEvidence == "complete_pair" || test.optionalAcceptance) {
					owner.recovery.Discussion = []tracker.NativeComment{{WorkItemID: tracker.NativeWorkItemID(issue.ID), Body: "The current source has completed its reviews; implement the requested follow-up change.", Actor: tracker.Actor{Kind: "human", PrincipalID: "person_example"}, CreatedAt: time.Unix(100, 0)}}
				}
				if test.summaryFailure {
					attempt := owner.recovery.Attempts[0]
					attempt.AttemptID, attempt.RunID, attempt.Status = "attempt_review", "run_review", "interrupted"
					attempt.Finalization = &tracker.NativeFinalization{Changed: true, ChangeID: version.ChangeID, VersionID: version.ID, HeadSHA: original}
					owner.recovery.Attempts = append(owner.recovery.Attempts, attempt)
					if test.laterFinalizationSuccess {
						attempt.AttemptID, attempt.RunID, attempt.Status = "attempt_success", "run_success", "succeeded"
						finalization := *attempt.Finalization
						finalization.Reviewed = true
						attempt.Finalization = &finalization
						owner.recovery.Attempts = append(owner.recovery.Attempts, attempt)
					}
				}
				if test.producerFailure {
					owner.recordError = errors.New("required review recording unavailable")
				}
				if test.sourcePartial {
					owner.recovery.Attempts[0].Disposition.Status = "in_progress"
					owner.recovery.Attempts[0].CompletionBody = strings.ReplaceAll(disposition.FinalSummary, "status: complete", "status: in_progress")
				}
				if test.sourceRework {
					owner.recovery.ChangeDetail.Reviews[0].ImplementationReview.Verdict = gate.ValidatorVerdictRework
				}
				if test.landingRepair != nil {
					owner.recovery.ChangeDetail.Reviews = []tracker.ChangeReview{{VersionID: version.ID, ImplementationReview: &gate.ValidatorResult{VersionID: version.ID, HeadSHA: original, Verdict: gate.ValidatorVerdictPass}}, {VersionID: version.ID, Validator: &gate.ValidatorResult{VersionID: version.ID, HeadSHA: original, Verdict: gate.ValidatorVerdictPass}}}
					owner.recovery.ChangeDetail.Summary.Status = "reviewed"
					landing := *test.landingRepair
					landing.ChangeID, landing.VersionID, landing.HeadSHA = version.ChangeID, version.ID, original
					if landing.CI != nil {
						ci := *landing.CI
						ci.HeadSHA = original
						landing.CI = &ci
					}
					owner.recovery.Attempts = append(owner.recovery.Attempts, tracker.NativeAttempt{Status: "succeeded", NativeRunData: tracker.NativeRunData{AttemptID: "attempt_landing", RunID: "run_landing", PolicyID: descriptor.ID, Runtime: &tracker.NativeRuntimeObservation{Landing: &landing}}, Checkpoint: &checkpoint})
					if test.historyRefusal {
						owner.recovery.Discussion = []tracker.NativeComment{{Body: "Fix the unresolved review finding before landing again.", Actor: tracker.Actor{Kind: "human", PrincipalID: "operator"}, CreatedAt: time.Unix(300, 0)}}
						owner.recovery.History = []tracker.CollaborationEvent{{Type: "workflow.transitioned", Data: tracker.CollaborationData{FromState: "Merging", ToState: "Rework", ReasonDetail: workspace.ReviewConversationsRefusalText}}}
					}
				}
				if test.laterPartial {
					later := owner.recovery.Attempts[0]
					later.AttemptID = "attempt_partial"
					later.RunID = "run_partial"
					later.Disposition = &tracker.NativeDisposition{Status: "in_progress"}
					later.CompletionBody = "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```"
					owner.recovery.Attempts = append(owner.recovery.Attempts, later)
				}
				if test.sourceMismatch {
					owner.source.Version.ID = "version_newer"
				}

				freshSource := filepath.Join(t.TempDir(), "source")
				runRunnerGit(t, source, "clone", remote, freshSource)
				runRunnerGit(t, freshSource, "remote", "set-url", "origin", "https://github.com/example-org/example-project")
				runRunnerGit(t, freshSource, "config", "url."+remote+".insteadOf", "https://github.com/example-org/example-project")
				runRunnerGit(t, freshSource, "config", "user.name", "Test User")
				runRunnerGit(t, freshSource, "config", "user.email", "test@example.com")
				runRunnerGit(t, freshSource, "config", "commit.gpgsign", "false")
				backend, err = workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "fresh-workspaces"), SourceRoot: freshSource, AutoBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				switch test.feedback {
				case "change":
					owner.recovery.ChangeDetail.Discussion = []tracker.ChangeDiscussion{{ID: "feedback_current", VersionID: version.ID, Body: "The host evidence is available; independently review it without changing source.", Actor: tracker.Actor{Kind: "human", PrincipalID: "operator"}, CreatedAt: time.Unix(300, 0)}}
				case "issue":
					owner.recovery.Discussion = []tracker.NativeComment{{ID: "feedback_issue", Body: "The source has an actual defect; inspect it before acceptance.", Actor: tracker.Actor{Kind: "human", PrincipalID: "operator"}, CreatedAt: time.Unix(300, 0)}}
				}
				agent.reviewVerdict = test.reviewResume
				db, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "sessions.db")})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Close() })
				cfg := config.Config{Tracker: config.Tracker{Kind: config.TrackerHubNative}, Gate: gate.Config{Run: "true", AutomatedReview: gate.AutomatedReviewRequired, Validator: gate.ValidatorConfig{Enabled: true}}, Agents: config.Agents{Backends: []config.AgentBackend{{ID: "codex", Kind: "codex", Protocol: "app-server", Command: "codex app-server"}}, Routes: []config.AgentRoute{{Name: "implementation", Role: RoleCode, Backend: "codex", Model: "review-model"}, {Name: "rework", Role: RoleRework, Backend: "codex", Model: "review-model"}, {Name: "acceptance", Role: RoleValidator, Backend: "codex", Model: "validator-model"}}}}
				cfg.Gate.Validator.Enabled = !test.optionalAcceptance
				r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: cfg, Prompt: "Complete the source"}, Workspace: backend, AgentBackend: agent, Store: db})
				if err != nil {
					t.Fatal(err)
				}
				request.Execution = owner
				request.Issue.Description = "## Acceptance criteria\n- Preserve current source"
				if test.landingRepair != nil {
					agent.afterStage = func() {
						owner.repairTree = strings.TrimSpace(runRunnerGit(t, agent.request.Workspace, "write-tree"))
					}
				}
				result, err := r.Run(t.Context(), request)
				if test.writableResume {
					if agent.calls == 0 {
						t.Fatalf("partial or genuine rework skipped coding: %v", err)
					}
					if test.landingRepair != nil {
						if err != nil || result.NativeChange == nil || result.NativeChange.ReusedDispositionAttemptID != "" || owner.head == "" || owner.head == original || owner.validation == nil || owner.validation.HeadSHA != owner.head || owner.diffHead != owner.head {
							t.Fatalf("landing repair lost finalized source: head=%s original=%s validation=%+v result=%+v error=%v", owner.head, original, owner.validation, result, err)
						}
						repaired := false
						for _, file := range owner.files {
							repaired = repaired || file.Path == "repair.md"
						}
						if !repaired {
							t.Fatalf("finalized diff omitted repair: %+v", owner.files)
						}
					}
					return
				}
				if test.sourceMismatch {
					if agent.calls != 0 || !errors.Is(err, ErrNativeRecoveryRequired) || len(owner.reviews) != 0 {
						t.Fatalf("version race did not refuse without coding: writable=%d error=%v", agent.calls, err)
					}
					return
				}
				if test.producerFailure {
					if agent.calls != 0 || err == nil || result.NativeChange == nil || result.NativeChange.VersionID != version.ID || result.NativeChange.ImplementationReviewError == "" || owner.finish != "interrupted" || len(owner.reviews) != 0 || len(owner.acceptance) != 0 {
						t.Fatalf("review producer failure lost source or started coding: writable=%d error=%v change=%+v finish=%s", agent.calls, err, result.NativeChange, owner.finish)
					}
					return
				}
				if agent.calls != 0 || result.TurnStarted {
					t.Fatalf("complete retained source invoked coding: writable=%d review=%d error=%v", agent.calls, agent.reviewCalls, err)
				}
				if err != nil {
					t.Fatal(err)
				}
				if agent.reviewCalls == 0 {
					t.Fatal("source review never ran")
				}
				if owner.head != original || owner.body != disposition.FinalSummary || result.NativeChange == nil || result.NativeChange.ReusedDispositionAttemptID != "attempt_source" || len(owner.reviews) != 1 || owner.reviews[0].Verdict != test.reviewResume {
					t.Fatalf("review recovery altered immutable source or provenance: head=%s body=%q reviews=%+v", owner.head, owner.body, owner.reviews)
				}
				if test.reviewResume != "pass" && len(owner.acceptance) != 0 {
					t.Fatal("nonpassing source review ran acceptance")
				}
				if owner.activitySession <= 0 || owner.reviews[0].SessionID == owner.activitySession {
					t.Fatalf("execution and source review did not use distinct recorded sessions: activity=%d review=%d", owner.activitySession, owner.reviews[0].SessionID)
				}
				if test.resumeTwice {
					firstSession := owner.activitySession
					result, err := r.Run(t.Context(), request)
					if err != nil || agent.calls != 0 || result.TurnStarted || owner.activitySession == firstSession || owner.head != original || len(owner.reviews) != 2 || len(owner.acceptance) != 0 {
						t.Fatalf("second explicit wait invoked coding or reused session: %+v, %v", result, err)
					}
				}
				if test.optionalAcceptance && len(owner.acceptance) != 0 {
					t.Fatal("optional acceptance unexpectedly ran a validator")
				}
				if test.reviewResume == "pass" && !test.optionalAcceptance && (len(owner.acceptance) != 1 || owner.reviews[0].SessionID == owner.acceptance[0].SessionID || owner.acceptance[0].SessionID == owner.activitySession) {
					t.Fatal("source and acceptance did not use separate sessions")
				}
				return
			}

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
			cfg := config.Config{Tracker: config.Tracker{Kind: config.TrackerHubNative}}
			if test.resolvedPause {
				cfg.Gate.Run = "test -f target-fix.md && test -f later-one.md && test -f later-two.md"
				if test.failedFinalGate {
					cfg.Gate.Run += " && echo actual-final-gate-failure && exit 1"
				}
			}
			r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: cfg, Prompt: "Resolve the source"}, Workspace: backend, AgentBackend: agent})
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.Run(t.Context(), request)
			if test.resolvedPause {
				if test.pausedLaterConflict {
					if err != nil || execution.head != "" || execution.validation != nil || execution.checkpoint == nil || execution.checkpoint.WorktreeState != "dirty" {
						t.Fatalf("later conflict published unfinished source: %v, %+v", err, execution)
					}
					if got := strings.TrimSpace(runRunnerGit(t, info.Path, "diff", "--name-only", "--diff-filter=U")); got != "later-one.md" {
						t.Fatalf("replay did not advance to the later conflict: %q", got)
					}
					todo := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "--git-path", "rebase-merge/git-rebase-todo"))
					remaining, readErr := os.ReadFile(todo)
					if readErr != nil || !strings.Contains(string(remaining), "later-two.md") {
						t.Fatalf("last replay pick lost: %q, %v", remaining, readErr)
					}
					if got := runRunnerGit(t, info.Path, "show", "HEAD:README.md"); got != "resolved\n" {
						t.Fatalf("earlier resolution lost: %q", got)
					}
					return
				}
				if execution.validation == nil {
					t.Fatalf("resolved paused replay never reached the final gate: %v, checkpoint=%+v", err, execution.checkpoint)
				}
				head := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD"))
				tree := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD^{tree}"))
				if execution.validation.HeadSHA != head || execution.validation.TreeSHA != tree {
					t.Fatalf("gate did not validate final source: %+v", execution.validation)
				}
				runRunnerGit(t, info.Path, "merge-base", "--is-ancestor", target, head)
				for _, path := range []string{"later-one.md", "later-two.md"} {
					if got := runRunnerGit(t, info.Path, "show", "HEAD:"+path); got != path+"\n" {
						t.Fatalf("remaining pick lost: %s = %q", path, got)
					}
				}
				if test.failedFinalGate {
					var failure *workspace.ValidationError
					if !errors.As(err, &failure) || failure.Stage != workspace.ValidationStageSourceFinalization || !strings.Contains(failure.Output, "actual-final-gate-failure") || execution.head != "" || execution.finish != "failed" || execution.validation.ExitCode != 1 {
						t.Fatalf("genuine gate failure was suppressed: %v, %+v", err, execution)
					}
					return
				}
				if err != nil || execution.validation.ExitCode != 0 || execution.head != head || execution.diffHead != head || execution.finish != "succeeded" {
					t.Fatalf("finalized source publication failed: %v, %+v", err, execution)
				}
				return
			}
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
					t.Fatalf("signing failure omitted dirty checkpoint: %+v", execution.checkpoint)
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
