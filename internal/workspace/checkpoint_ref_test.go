package workspace

import (
	"context"
	"crypto/rand"
	"encoding/json"
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
		name        string
		edit        func(t *testing.T, path string)
		validate    func(context.Context) error
		want        error
		excluded    string
		transaction bool
	}{
		{name: "runner killed mid-stage resumes elsewhere", validate: owned},
		{name: "unfinished two-commit rework restores exact transaction", validate: owned, transaction: true},
		{name: "excluded paths stay on the machine", validate: owned, excluded: ".env", edit: func(t *testing.T, path string) {
			writeCheckpointFile(t, path, ".env", "TOKEN=example")
			runGit(t, path, "add", "--force", "--", ".env")
		}},
		{name: "credential content is refused", validate: owned, want: ErrCheckpointUnsafe, edit: func(t *testing.T, path string) {
			writeCheckpointFile(t, path, "config.go", strings.Join([]string{"-----BEGIN", "PRIVATE KEY-----"}, " "))
		}},
		{name: "credential history is refused", validate: owned, want: ErrCheckpointUnsafe, edit: func(t *testing.T, path string) {
			writeCheckpointFile(t, path, "config.go", strings.Join([]string{"-----BEGIN", "PRIVATE KEY-----"}, " "))
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
			if test.transaction {
				verifyPortableReworkCheckpoint(t, source, info, issue, first, second, remote)
				return
			}
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

func verifyPortableReworkCheckpoint(t *testing.T, source *LocalGit, info Info, issue Issue, first, second, remote string) {
	t.Helper()
	issue.NativeRework, issue.CheckpointQuiescent = true, true
	for path, content := range map[string]string{"later.txt": "initial text\n", "binary.dat": "initial\x00binary", "name.txt": "rename contents\n"} {
		writeCheckpointFile(t, first, path, content)
	}
	runGit(t, first, "add", ".")
	runGit(t, first, "commit", "-m", "shared conflict fixture")
	runGit(t, first, "push", "origin", "main")
	runGit(t, info.Path, "merge", "--ff-only", "main")
	writeCheckpointFile(t, info.Path, "README.md", "first source\n")
	runGit(t, info.Path, "add", "README.md")
	runGit(t, info.Path, "commit", "-m", "first source")
	writeCheckpointFile(t, info.Path, "later.txt", "later source\n")
	writeCheckpointFile(t, info.Path, "binary.dat", "source\x00binary")
	runGit(t, info.Path, "mv", "name.txt", "source-name.txt")
	runGit(t, info.Path, "add", ".")
	runGit(t, info.Path, "commit", "-m", "remaining source")
	writeCheckpointFile(t, info.Path, "pending.txt", "unplayed source\n")
	runGit(t, info.Path, "add", "pending.txt")
	runGit(t, info.Path, "commit", "-m", "unplayed source")
	original := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
	writeCheckpointFile(t, first, "README.md", "first target\n")
	writeCheckpointFile(t, first, "later.txt", "later target\n")
	writeCheckpointFile(t, first, "binary.dat", "target\x00binary")
	runGit(t, first, "mv", "name.txt", "target-name.txt")
	runGit(t, first, "add", ".")
	runGit(t, first, "commit", "-m", "conflicting target")
	runGit(t, first, "push", "origin", "main")
	prepared, err := source.PrepareRework(t.Context(), info, issue, MergePrepareOptions{})
	if err != nil || prepared.Status != MergePrepareStatusConflict {
		t.Fatalf("prepare = %#v, %v", prepared, err)
	}
	initial, pushed, err := source.PushCheckpointRef(t.Context(), info, issue, CheckpointRef{}, func(ctx context.Context) error { return ctx.Err() })
	if err != nil || !pushed {
		t.Fatalf("initial conflict capture = %#v, %v, %v", initial, pushed, err)
	}
	writeCheckpointFile(t, info.Path, "README.md", "first resolution\n")
	runGit(t, info.Path, "add", "README.md")
	if _, err := source.FinalizeNativeWork(t.Context(), info, issue, func(ctx context.Context) error { return ctx.Err() }); err != nil {
		t.Fatalf("continue first resolution: %v", err)
	}
	writeCheckpointFile(t, info.Path, "later.txt", "partial later resolution\n")
	writeCheckpointFile(t, info.Path, "staged.txt", "staged alternative\n")
	runGit(t, info.Path, "add", "staged.txt")
	writeCheckpointFile(t, info.Path, "staged.txt", "unstaged alternative\n")
	index := runGit(t, info.Path, "ls-files", "--stage", "-z")
	status := runGit(t, info.Path, "status", "--porcelain")
	checkpoint, pushed, err := source.PushCheckpointRef(t.Context(), info, issue, initial, func(ctx context.Context) error { return ctx.Err() })
	if err != nil || !pushed {
		t.Fatalf("capture = %#v, %v, %v", checkpoint, pushed, err)
	}
	if tree := strings.TrimSpace(runGit(t, remote, "rev-parse", checkpoint.CommitSHA+"^{tree}")); tree == checkpoint.TreeSHA {
		t.Fatal("old reader could interpret a transaction as ordinary source")
	}
	if _, pushed, err := source.PushCheckpointRef(t.Context(), info, issue, checkpoint, func(ctx context.Context) error { return ctx.Err() }); err != nil || pushed {
		t.Fatalf("unchanged transaction republished: %v, %v", pushed, err)
	}

	for _, failure := range []string{"staged secret", "unresolved secret", "staged size", "executable replay", "capture failure", "push failure", "live capture"} {
		t.Run(failure, func(t *testing.T) {
			before := readFile(t, filepath.Join(info.Path, "staged.txt"))
			dir := linkedWorktreeGitDir(t, info.Path)
			todo := readFile(t, filepath.Join(dir, "rebase-merge", "git-rebase-todo"))
			stopped := readFile(t, filepath.Join(dir, "rebase-merge", "stopped-sha"))
			current := issue
			want := ErrCheckpointUnsafe
			switch failure {
			case "staged secret":
				writeCheckpointFile(t, info.Path, "staged.txt", strings.Join([]string{"-----BEGIN", "PRIVATE KEY-----"}, " "))
				runGit(t, info.Path, "add", "staged.txt")
				writeCheckpointFile(t, info.Path, "staged.txt", before)
			case "unresolved secret":
				file := filepath.Join(t.TempDir(), "content")
				if err := os.WriteFile(file, []byte(strings.Join([]string{"-----BEGIN", "PRIVATE KEY-----"}, " ")), 0o600); err != nil {
					t.Fatal(err)
				}
				blob := strings.TrimSpace(runGit(t, info.Path, "hash-object", "-w", file))
				if _, err := runGitAtWithInput(t.Context(), info.Path, nil, false, "100644 "+blob+" 3\tbinary.dat\x00", "update-index", "-z", "--index-info"); err != nil {
					t.Fatal(err)
				}
			case "staged size":
				data := make([]byte, MaxCheckpointRefBytes+1<<20)
				if _, err := rand.Read(data); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(info.Path, "staged.txt"), data, 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, info.Path, "add", "staged.txt")
				writeCheckpointFile(t, info.Path, "staged.txt", before)
			case "executable replay":
				if err := os.WriteFile(filepath.Join(dir, "rebase-merge", "git-rebase-todo"), []byte("exec false\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "capture failure":
				if err := os.Remove(filepath.Join(dir, "rebase-merge", "stopped-sha")); err != nil {
					t.Fatal(err)
				}
				want = os.ErrNotExist
			case "push failure":
				writeCheckpointFile(t, info.Path, "staged.txt", "publication candidate\n")
				runGit(t, info.Path, "remote", "set-url", "--push", "origin", filepath.Join(t.TempDir(), "missing"))
				want = nil
			case "live capture":
				current.CheckpointQuiescent = false
			}
			result, pushed, err := source.PushCheckpointRef(t.Context(), info, current, checkpoint, func(ctx context.Context) error { return ctx.Err() })
			if err == nil || want != nil && !errors.Is(err, want) || pushed || result.CommitSHA != checkpoint.CommitSHA {
				t.Fatalf("failed capture replaced custody: %#v, %v, %v", result, pushed, err)
			}
			if got := strings.TrimSpace(runGit(t, remote, "rev-parse", checkpoint.Ref)); got != checkpoint.CommitSHA {
				t.Fatalf("failed capture replaced remote: %s", got)
			}
			writeCheckpointFile(t, info.Path, "staged.txt", "staged alternative\n")
			runGit(t, info.Path, "add", "staged.txt")
			writeCheckpointFile(t, info.Path, "staged.txt", before)
			if _, err := runGitAtWithInput(t.Context(), info.Path, nil, false, "0 "+strings.Repeat("0", 40)+"\tbinary.dat\x00"+index, "update-index", "-z", "--index-info"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "rebase-merge", "git-rebase-todo"), []byte(todo), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "rebase-merge", "stopped-sha"), []byte(stopped), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, info.Path, "remote", "set-url", "--push", "origin", remote)
		})
	}
	writeCheckpointFile(t, info.Path, "staged.txt", "not acknowledged\n")
	next, pushed, err := source.PushCheckpointRef(t.Context(), info, issue, checkpoint, func(ctx context.Context) error { return ctx.Err() })
	if err != nil || !pushed || next.CommitSHA == checkpoint.CommitSHA {
		t.Fatalf("unacknowledged push = %#v, %v, %v", next, pushed, err)
	}
	runGit(t, remote, "merge-base", "--is-ancestor", checkpoint.CommitSHA, next.CommitSHA)
	runGit(t, remote, "gc", "--prune=now")
	transaction := *checkpoint.transaction
	transaction.Schema = 99
	data, err := json.Marshal(transaction)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	file := filepath.Join(scratch, "transaction.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimSpace(runGit(t, info.Path, "hash-object", "-w", file))
	corruptIndex := checkpointRefGit(info.Path, "GIT_INDEX_FILE="+filepath.Join(scratch, "index"))
	if _, err := corruptIndex(t.Context(), "read-tree", checkpoint.CommitSHA+"^{tree}"); err != nil {
		t.Fatal(err)
	}
	if _, err := corruptIndex(t.Context(), "update-index", "--add", "--cacheinfo", "100644,"+digest+",transaction.json"); err != nil {
		t.Fatal(err)
	}
	corruptTree, err := corruptIndex(t.Context(), "write-tree")
	if err != nil {
		t.Fatal(err)
	}
	corrupt, err := checkpointCommit(t.Context(), checkpointRefGit(info.Path), strings.TrimSpace(corruptTree), []string{checkpoint.HeadSHA, original})
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, info.Path, "push", "origin", corrupt+":"+checkpoint.Ref+"-incompatible")
	rejector, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "rejected"), SourceRoot: second, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	unsafe := issue
	unsafe.Checkpoint = &CheckpointRef{Ref: checkpoint.Ref + "-incompatible", CommitSHA: corrupt, TreeSHA: digest}
	if _, err := rejector.Create(t.Context(), unsafe); !errors.Is(err, ErrCheckpointUnsafe) {
		t.Fatalf("incompatible checkpoint accepted: %v", err)
	}
	writeCheckpointFile(t, first, "advance.txt", "advanced target\n")
	runGit(t, first, "add", "advance.txt")
	runGit(t, first, "commit", "-m", "advance target")
	runGit(t, first, "push", "origin", "main")
	if err := os.RemoveAll(info.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(first); err != nil {
		t.Fatal(err)
	}
	restorer, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "portable"), SourceRoot: second, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	issue.Checkpoint = &CheckpointRef{Ref: checkpoint.Ref, CommitSHA: checkpoint.CommitSHA, TreeSHA: checkpoint.TreeSHA}
	restored, err := restorer.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if runGit(t, restored.Path, "ls-files", "--stage", "-z") != index || runGit(t, restored.Path, "status", "--porcelain") != status {
		t.Fatal("restored index alternatives or staged/unstaged state differ")
	}
	for path, want := range map[string]string{"README.md": "first resolution\n", "later.txt": "partial later resolution\n", "staged.txt": "unstaged alternative\n"} {
		if got := readFile(t, filepath.Join(restored.Path, path)); got != want {
			t.Fatalf("%s = %q", path, got)
		}
	}
	if got := strings.TrimSpace(runGit(t, restored.Path, "rev-parse", "refs/heads/"+restored.Branch)); got != original {
		t.Fatalf("source head = %s", got)
	}
	observed, err := restorer.RecoveryState(t.Context(), restored, issue)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := restorer.VerifyReworkRecovery(t.Context(), restored, issue, checkpoint.HeadSHA, observed.WorkspaceFingerprint, observed)
	if err != nil || !verified {
		t.Fatalf("portable custody verification = %v, %v", verified, err)
	}
	continued, err := restorer.PrepareRework(t.Context(), restored, issue, MergePrepareOptions{})
	if err != nil || continued.BaseSHA != prepared.BaseSHA || len(continued.ConflictPaths) < 3 {
		t.Fatalf("restored conflict = %#v, %v", continued, err)
	}
	writeCheckpointFile(t, restored.Path, "later.txt", "finished later resolution\n")
	writeCheckpointFile(t, restored.Path, "binary.dat", "resolved\x00binary")
	runGit(t, restored.Path, "add", "-A")
	base, err := restorer.FinalizeNativeWork(t.Context(), restored, issue, func(ctx context.Context) error { return ctx.Err() })
	if err != nil || base != prepared.BaseSHA {
		t.Fatalf("finish = %s, %v", base, err)
	}
	if got := readFile(t, filepath.Join(restored.Path, "README.md")); got != "first resolution\n" {
		t.Fatalf("first resolution lost: %q", got)
	}
	if got := readFile(t, filepath.Join(restored.Path, "pending.txt")); got != "unplayed source\n" {
		t.Fatalf("unplayed source lost: %q", got)
	}
	if got := readFile(t, filepath.Join(restored.Path, "staged.txt")); got != "unstaged alternative\n" {
		t.Fatalf("remaining source lost: %q", got)
	}
}
