package workspace

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckpointRefRestoresOnAnotherRunner(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	t.Parallel()
	owned := func(ctx context.Context) error { return ctx.Err() }
	for _, test := range []struct {
		name     string
		edit     func(t *testing.T, path string)
		validate func(context.Context) error
		want     error
		excluded string
	}{
		{name: "runner killed mid-stage resumes elsewhere", validate: owned},
		{name: "excluded paths stay on the machine", validate: owned, excluded: ".env", edit: func(t *testing.T, path string) {
			writeCheckpointFile(t, path, ".env", "TOKEN=example")
			runGit(t, path, "add", "--force", "--", ".env")
		}},
		{name: "credential content is refused", validate: owned, want: ErrCheckpointUnsafe, edit: func(t *testing.T, path string) {
			writeCheckpointFile(t, path, "config.go", "-----BEGIN PRIVATE KEY-----")
		}},
		{name: "credential history is refused", validate: owned, want: ErrCheckpointUnsafe, edit: func(t *testing.T, path string) {
			writeCheckpointFile(t, path, "config.go", "-----BEGIN PRIVATE KEY-----")
			runGit(t, path, "add", "config.go")
			runGit(t, path, "commit", "-m", "example unfinished work")
		}},
		{name: "oversized checkpoint is refused", validate: owned, want: ErrCheckpointUnsafe, edit: func(t *testing.T, path string) {
			data := make([]byte, MaxCheckpointRefBytes+1<<20)
			if _, err := rand.Read(data); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "large.bin"), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "paused rebase or detached head is refused", validate: owned, want: ErrCheckpointUnsafe, edit: func(t *testing.T, path string) {
			runGit(t, path, "checkout", "-q", "--detach")
		}},
		{name: "lost ownership never pushes", validate: func(context.Context) error { return context.Canceled }, want: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			remote := initBareRemote(t)
			first, second := initSourceRepo(t), initSourceRepo(t)
			runGit(t, first, "remote", "add", "origin", remote)
			runGit(t, first, "push", "-u", "origin", "main")
			runGit(t, second, "remote", "add", "origin", remote)
			runGit(t, second, "fetch", "origin")
			issue := Issue{ID: "wi_checkpoint", Identifier: "prj#1"}
			source, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "a"), SourceRoot: first, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			info, err := source.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			runGit(t, info.Path, "config", "user.name", "Test")
			runGit(t, info.Path, "config", "user.email", "test@example.com")
			writeCheckpointFile(t, info.Path, "committed.go", "package committed\n")
			runGit(t, info.Path, "add", "committed.go")
			runGit(t, info.Path, "commit", "-m", "committed work")
			head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			writeCheckpointFile(t, info.Path, "staged.go", "package staged\n")
			runGit(t, info.Path, "add", "staged.go")
			writeCheckpointFile(t, info.Path, "committed.go", "package committed // edited\n")
			writeCheckpointFile(t, info.Path, "untracked.go", "package untracked\n")
			runGit(t, info.Path, "rm", "-q", "README.md")
			if test.edit != nil {
				test.edit(t, info.Path)
			}
			status := runGit(t, info.Path, "status", "--porcelain")

			checkpoint, pushed, err := source.PushCheckpointRef(t.Context(), info, issue, CheckpointRef{}, test.validate)
			if test.want != nil {
				if !errors.Is(err, test.want) || pushed {
					t.Fatalf("push = %v pushed=%v, want %v", err, pushed, test.want)
				}
				if refs := runGit(t, remote, "for-each-ref", "--format=%(refname)", CheckpointRefPrefix); strings.TrimSpace(refs) != "" {
					t.Fatalf("refused checkpoint was published: %s", refs)
				}
				return
			}
			if err != nil || !pushed {
				t.Fatalf("push = %v pushed=%v", err, pushed)
			}
			if current := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); current != head || runGit(t, info.Path, "status", "--porcelain") != status {
				t.Fatal("checkpoint rewrote the item branch or its worktree")
			}
			if branches := runGit(t, remote, "for-each-ref", "--format=%(refname)", "refs/heads/"); strings.TrimSpace(branches) != "refs/heads/main" {
				t.Fatalf("checkpoint created visible branches: %s", branches)
			}
			if published := strings.TrimSpace(runGit(t, remote, "rev-parse", checkpoint.Ref)); published != checkpoint.CommitSHA || checkpoint.Ref != CheckpointRefPrefix+"wi_checkpoint" || checkpoint.HeadSHA != head {
				t.Fatalf("checkpoint = %#v, remote %s", checkpoint, published)
			}
			if _, again, err := source.PushCheckpointRef(t.Context(), info, issue, checkpoint, test.validate); err != nil || again {
				t.Fatalf("unchanged tree pushed again: %v %v", again, err)
			}

			again, err := source.Create(t.Context(), Issue{ID: issue.ID, Identifier: issue.Identifier, Checkpoint: &CheckpointRef{Ref: checkpoint.Ref, CommitSHA: checkpoint.CommitSHA, TreeSHA: checkpoint.TreeSHA}})
			if err != nil || again.Path != info.Path || runGit(t, info.Path, "status", "--porcelain") != status {
				t.Fatalf("same runner did not reuse its matching worktree: %v", err)
			}
			if quarantined := runGit(t, first, "worktree", "list", "--porcelain"); strings.Count(quarantined, "worktree ") != 2 {
				t.Fatalf("matching worktree was quarantined:\n%s", quarantined)
			}

			restorer, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "b"), SourceRoot: second, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			restoreIssue := issue
			restoreIssue.Checkpoint = &CheckpointRef{Ref: checkpoint.Ref, CommitSHA: checkpoint.CommitSHA, TreeSHA: checkpoint.TreeSHA}
			restored, err := restorer.Create(t.Context(), restoreIssue)
			if err != nil {
				t.Fatal(err)
			}
			if current := strings.TrimSpace(runGit(t, restored.Path, "rev-parse", "HEAD")); current != head {
				t.Fatalf("restored head = %s, want %s", current, head)
			}
			for path, want := range map[string]string{"committed.go": "package committed // edited\n", "staged.go": "package staged\n", "untracked.go": "package untracked\n"} {
				if got, err := os.ReadFile(filepath.Join(restored.Path, path)); err != nil || string(got) != want {
					t.Fatalf("restored %s = %q, %v", path, got, err)
				}
			}
			if _, err := os.Stat(filepath.Join(restored.Path, "README.md")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("restored deletion lost: %v", err)
			}
			if test.excluded != "" {
				if _, err := os.Stat(filepath.Join(restored.Path, test.excluded)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("excluded %s left the machine: %v", test.excluded, err)
				}
			}
			if staged := runGit(t, restored.Path, "diff", "--cached", "--name-only"); staged != "" {
				t.Fatalf("restored changes are staged: %s", staged)
			}
		})
	}
}

func TestTerminalCleanupDeletesCheckpointRef(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	t.Parallel()
	remote, source := initBareRemote(t), initSourceRepo(t)
	runGit(t, source, "remote", "add", "origin", remote)
	runGit(t, source, "push", "-u", "origin", "main")
	backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	issue := Issue{ID: "wi_terminal", Identifier: "prj#2"}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	writeCheckpointFile(t, info.Path, "scratch.go", "package scratch\n")
	checkpoint, pushed, err := backend.PushCheckpointRef(t.Context(), info, issue, CheckpointRef{}, func(ctx context.Context) error { return ctx.Err() })
	if err != nil || !pushed {
		t.Fatalf("push = %v pushed=%v", err, pushed)
	}
	if err := os.Remove(filepath.Join(info.Path, "scratch.go")); err != nil {
		t.Fatal(err)
	}
	issue.Terminal = true
	if _, err := backend.CleanupIssue(t.Context(), issue); err != nil {
		t.Fatal(err)
	}
	if refs := runGit(t, remote, "for-each-ref", "--format=%(refname)", checkpoint.Ref); strings.TrimSpace(refs) != "" {
		t.Fatalf("terminal cleanup kept %s", refs)
	}
}

func writeCheckpointFile(t *testing.T, dir, path, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
