package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type resolvingReworkAgent struct {
	fakeCodexClient
	unresolved bool
}

func (a *resolvingReworkAgent) RunTurn(ctx context.Context, req AgentTurnRequest, _ AgentUpdateHandler) (AgentTurnResult, error) {
	a.request = req
	if !a.unresolved {
		if err := os.WriteFile(filepath.Join(req.Workspace, "README.md"), []byte("resolved\n"), 0o600); err != nil {
			return AgentTurnResult{}, err
		}
		if err := runAgentGit(ctx, req.Workspace, "add", "README.md"); err != nil {
			return AgentTurnResult{}, err
		}
	}
	return AgentTurnResult{ThreadID: "native-rework", TurnID: "1"}, nil
}

type reworkArtifactsExecution struct {
	testExecution
	base       string
	head       string
	diffHead   string
	files      []tracker.AttemptDiffFile
	diffSource AttemptDiffSource
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
	t.Parallel()
	for _, unresolved := range []bool{false, true} {
		name := "resolved source"
		if unresolved {
			name = "unresolved source"
		}
		t.Run(name, func(t *testing.T) {
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
			if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runRunnerGit(t, source, "add", "README.md")
			runRunnerGit(t, source, "commit", "-m", "base")
			runRunnerGit(t, source, "push", "origin", "main")
			runRunnerGit(t, source, "config", "commit.gpgsign", "true")
			runRunnerGit(t, source, "config", "gpg.program", filepath.Join(t.TempDir(), "unavailable-personal-signer"))
			agent := &resolvingReworkAgent{unresolved: unresolved}
			execution := &reworkArtifactsExecution{base: base, testExecution: testExecution{recovery: tracker.NativeRecovery{Lease: tracker.NativeLease{PolicyID: "unchanged-policy"}}}}
			r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{Tracker: config.Tracker{Kind: config.TrackerHubNative}}, Prompt: "Resolve the source"}, Workspace: backend, AgentBackend: agent})
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.Run(t.Context(), RunRequest{Mode: RunModeImplement, Issue: issue, Execution: execution})
			if unresolved {
				if !errors.Is(err, workspace.ErrMergeResolutionInvalid) || errors.Is(err, ErrWorkspacePreparation) || execution.finish != "failed" || execution.head != "" {
					t.Fatalf("source conflict published evidence or became infrastructure: %v, %#v", err, execution)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			head := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD"))
			if head == original || execution.head != head || execution.diffHead != head || execution.checkpoint == nil || execution.checkpoint.HeadSHA != head || execution.finish != "succeeded" {
				t.Fatalf("immutable evidence missed final head %s: %#v", head, execution)
			}
			if len(execution.files) != 1 || execution.files[0].Path != "README.md" || !strings.Contains(execution.files[0].Patch, "+resolved") {
				t.Fatalf("final diff authority = %#v", execution.files)
			}
			if execution.recovery.Lease.PolicyID != "unchanged-policy" {
				t.Fatal("rework changed policy identity")
			}
			if !strings.Contains(agent.request.Prompt, "runner owns native rebase") {
				t.Fatal("worker was left owning Git transactions")
			}
		})
	}
}
