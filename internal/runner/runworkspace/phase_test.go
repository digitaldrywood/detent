package runworkspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

type phaseExecution struct {
	branch, head, tree string
	receipts           int
	receiptErr         error
}

func (*phaseExecution) PhaseReceipts(context.Context, string) (*tracker.NativePhaseContinuation, error) {
	return nil, nil
}

func (*phaseExecution) VerifyPhaseAuthority(context.Context) error { return nil }

func (e *phaseExecution) RecordPhaseReceipt(_ context.Context, _, branch, head, tree, _ string) error {
	if e.receiptErr != nil {
		return e.receiptErr
	}
	e.branch, e.head, e.tree = branch, head, tree
	e.receipts++
	return nil
}

func TestPublishPhaseUsesConfiguredRepository(t *testing.T) {
	for _, name := range []string{"normal", "isolated configuration", "lease lost after push", "missing project", "pushurl", "insteadOf", "pushInsteadOf", "multiple push urls"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source")
			origin := filepath.Join(dir, "origin.git")
			other := filepath.Join(dir, "other.git")
			root := filepath.Join(dir, "workspaces")
			checkout := filepath.Join(root, "item")
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, output)
				}
				return strings.TrimSpace(string(output))
			}
			git("init", "--bare", "--template=", origin)
			git("init", "--bare", "--template=", other)
			git("init", "--template=", source)
			git("-C", source, "remote", "add", "origin", origin)
			git("-C", source, "commit", "--allow-empty", "-m", "Phase source")
			backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			const branch = "detent/item"
			const ref = "refs/heads/" + branch
			git("-C", source, "worktree", "add", "-b", branch, checkout)
			switch name {
			case "isolated configuration":
				git("-C", checkout, "config", "push.pushOption", "fixture-local")
				global := filepath.Join(dir, "global.gitconfig")
				git("config", "--file", global, "push.pushOption", "fixture-global")
				t.Setenv("GIT_CONFIG_GLOBAL", global)
				t.Setenv("GIT_CONFIG_COUNT", "1")
				t.Setenv("GIT_CONFIG_KEY_0", "push.pushOption")
				t.Setenv("GIT_CONFIG_VALUE_0", "fixture-environment")
			case "pushurl":
				git("-C", checkout, "remote", "set-url", "--push", "origin", other)
			case "insteadOf", "pushInsteadOf":
				git("-C", checkout, "config", "url."+other+"."+name, origin)
			case "multiple push urls":
				git("-C", checkout, "remote", "set-url", "--push", "origin", origin)
				git("-C", checkout, "remote", "set-url", "--add", "--push", "origin", other)
			}

			info := workspace.Info{Path: checkout, Key: "item", Branch: branch, PublicationBranch: branch}
			execution := &phaseExecution{}
			issue := workspace.Issue{Identifier: "ITEM", PublicationRepository: origin}
			if name == "lease lost after push" {
				git("-C", source, "push", "origin", "HEAD:refs/heads/main")
				if err := os.WriteFile(filepath.Join(checkout, "orphaned.txt"), []byte("unfinished phase\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				git("-C", checkout, "add", "orphaned.txt")
				git("-C", checkout, "commit", "-m", "Publish before recording receipt")
				execution.receiptErr = errors.New("lease lost recording receipt")
			}
			if name == "missing project" {
				issue.PublicationRepository = ""
			}
			_, err = PublishPhase(t.Context(), execution, backend, info, issue, nil, "implement", "")
			remoteHead := git("-C", origin, "for-each-ref", "--format=%(objectname)", ref)
			otherHead := git("-C", other, "for-each-ref", "--format=%(objectname)", ref)
			if name == "lease lost after push" {
				orphan := git("-C", checkout, "rev-parse", "HEAD")
				if !errors.Is(err, execution.receiptErr) || execution.receipts != 0 || remoteHead != orphan {
					t.Fatalf("interrupted publication: head=%s receipts=%d error=%v", remoteHead, execution.receipts, err)
				}
				replacementSource := filepath.Join(dir, "replacement")
				git("clone", "--branch", "main", origin, replacementSource)
				replacement, err := workspace.NewLocalGit(workspace.LocalGitOptions{
					Root: filepath.Join(dir, "replacement-workspaces"), SourceRoot: replacementSource, AutoBranch: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				issue.BaseRef, issue.BranchName, issue.VerifyPhaseBranch = git("-C", source, "rev-parse", "HEAD"), branch, true
				restarted, err := replacement.Create(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				if head := git("-C", restarted.Path, "rev-parse", "HEAD"); head != orphan {
					t.Fatalf("replacement starts at %s, want %s", head, orphan)
				}
				git("-C", restarted.Path, "commit", "--allow-empty", "-m", "Complete the restarted phase")
				replacementExecution := &phaseExecution{}
				receipt, err := PublishPhase(t.Context(), replacementExecution, replacement, restarted, issue, nil, "implement", "")
				got := git("-C", origin, "rev-parse", ref)
				if err != nil || replacementExecution.receipts != 1 || receipt.HeadSHA != got || got == orphan {
					t.Fatalf("restarted publication: receipt=%+v remote=%s error=%v", receipt, got, err)
				}
				git("-C", origin, "merge-base", "--is-ancestor", orphan, got)
				return
			}
			if name != "normal" && name != "isolated configuration" {
				var attributed interface {
					FailureAttribution() tracker.FailureAttribution
				}
				if !errors.As(err, &attributed) || attributed.FailureAttribution().Outcome != "instance" {
					t.Errorf("destination mismatch has no instance attribution: %v", err)
				}
				if err == nil || execution.receipts != 0 || remoteHead != "" || otherHead != "" {
					t.Fatalf("publication mismatch: error=%v receipts=%d configured ref=%q other ref=%q", err, execution.receipts, remoteHead, otherHead)
				}
				return
			}
			head := git("-C", checkout, "rev-parse", "HEAD")
			tree := git("-C", checkout, "rev-parse", "HEAD^{tree}")
			if err != nil || execution.receipts != 1 || execution.branch != branch || execution.head != head || execution.tree != tree || remoteHead != head || otherHead != "" {
				t.Fatalf("configured publication: error=%v receipt=%+v configured ref=%q other ref=%q", err, execution, remoteHead, otherHead)
			}
		})
	}
}
