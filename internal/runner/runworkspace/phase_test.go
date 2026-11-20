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
}

func (*phaseExecution) PhaseReceipts(context.Context, string) (*tracker.NativePhaseContinuation, error) {
	return nil, nil
}

func (*phaseExecution) VerifyPhaseAuthority(context.Context) error { return nil }

func (e *phaseExecution) RecordPhaseReceipt(_ context.Context, _, branch, head, tree, _ string) error {
	e.branch, e.head, e.tree = branch, head, tree
	e.receipts++
	return nil
}

func TestPublishPhaseUsesConfiguredRepository(t *testing.T) {
	for _, name := range []string{"normal", "isolated configuration", "missing project", "pushurl", "insteadOf", "pushInsteadOf", "multiple push urls"} {
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
			if name == "missing project" {
				issue.PublicationRepository = ""
			}
			err = PublishPhase(t.Context(), execution, backend, info, issue, nil, "implement", "")
			remoteHead := git("-C", origin, "for-each-ref", "--format=%(objectname)", ref)
			otherHead := git("-C", other, "for-each-ref", "--format=%(objectname)", ref)
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
