package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspace/checkpoint"
)

func publishLegacyCheckpointFixture(t *testing.T, info Info, issue Issue) CheckpointRef {
	t.Helper()
	head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
	runGit(t, info.Path, "add", "-A")
	tree := strings.TrimSpace(runGit(t, info.Path, "write-tree"))
	runGit(t, info.Path, "reset", "-q", "HEAD")
	commit := strings.TrimSpace(runGit(t, info.Path, "commit-tree", tree, "-p", head, "-m", "legacy checkpoint"))
	ref := CheckpointRefName(issue)
	runGit(t, info.Path, "push", "origin", commit+":"+ref)
	return CheckpointRef{Ref: ref, CommitSHA: commit, HeadSHA: head, TreeSHA: tree}
}

func TestLegacyPausedRebaseCheckpointRestoresOnReplacement(t *testing.T) {
	remote, source := initBareRemote(t), initSourceRepo(t)
	runGit(t, source, "remote", "add", "origin", remote)
	runGit(t, source, "push", "-u", "origin", "main")
	newBackend := func(source string) *LocalGit {
		t.Helper()
		backend, err := NewLocalGit(LocalGitOptions{Root: t.TempDir(), SourceRoot: source, AutoBranch: true})
		if err != nil {
			t.Fatal(err)
		}
		return backend
	}
	ctx := t.Context()
	authorize := func(ctx context.Context) error { return ctx.Err() }
	backend := newBackend(source)
	issue := Issue{ID: "wi_example", Identifier: "prj_example#1", NativeRework: true}
	old, err := backend.Create(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	writeCheckpointFile(t, old.Path, "README.md", "feature\n")
	runGit(t, old.Path, "add", "README.md")
	runGit(t, old.Path, "commit", "-m", "feature")
	runGit(t, old.Path, "push", "origin", "HEAD:refs/heads/"+old.PublishBranch())
	writeCheckpointFile(t, source, "README.md", "updated base\n")
	runGit(t, source, "add", "README.md")
	runGit(t, source, "commit", "-m", "advance base")
	runGit(t, source, "push", "origin", "main")
	prepared, err := backend.PrepareRework(ctx, old, issue, MergePrepareOptions{TargetBranch: "main"}, authorize)
	if err != nil || prepared.Status != MergePrepareStatusConflict {
		t.Fatalf("pause rebase: %+v, %v", prepared, err)
	}
	writeCheckpointFile(t, old.Path, "README.md", "staged resolution\n")
	runGit(t, old.Path, "add", "README.md")
	writeCheckpointFile(t, old.Path, "README.md", "unstaged follow-up\n")

	tx, err := checkpoint.CaptureTransaction(ctx, old.Path, old.Branch, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(tx)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	manifest := filepath.Join(scratch, "transaction.json")
	if err := os.WriteFile(manifest, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimSpace(runGit(t, old.Path, "hash-object", "-w", manifest))
	indexEnv := "GIT_INDEX_FILE=" + filepath.Join(scratch, "index")
	git := checkpoint.RefGit(old.Path, indexEnv)
	if _, err := git(ctx, "read-tree", "--empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, "update-index", "--add", "--cacheinfo", "100644,"+digest+",transaction.json"); err != nil {
		t.Fatal(err)
	}
	var alternatives strings.Builder
	for i, entry := range tx.Index {
		fmt.Fprintf(&alternatives, "100644 %s\talternatives/%d\x00", entry.Blob, i)
	}
	if _, err := runGitAtWithInput(ctx, old.Path, []string{indexEnv}, false, alternatives.String(), "update-index", "-z", "--index-info"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, "read-tree", "--prefix=worktree/", tx.Worktree); err != nil {
		t.Fatal(err)
	}
	wrapper, err := git(ctx, "write-tree")
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.TrimSpace(runGit(t, old.Path, "commit-tree", strings.TrimSpace(wrapper), "-p", tx.Head, "-p", tx.Original, "-m", "legacy paused rebase"))
	ref := CheckpointRefName(issue)
	runGit(t, old.Path, "push", "origin", commit+":"+ref)

	replacementSource := initSourceRepo(t)
	runGit(t, replacementSource, "remote", "add", "origin", remote)
	replacement := newBackend(replacementSource)
	issue.Checkpoint = &CheckpointRef{Ref: ref, CommitSHA: commit, TreeSHA: digest}
	info, err := replacement.Create(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	if paused, err := replacement.verifyReworkBranch(ctx, info, issue); err != nil || !paused {
		t.Fatalf("restored rebase paused=%t, error=%v", paused, err)
	}
	if got := strings.TrimSpace(runGit(t, info.Path, "show", ":README.md")); got != "staged resolution" {
		t.Fatalf("restored index = %q", got)
	}
	if got := readFile(t, filepath.Join(info.Path, "README.md")); got != "unstaged follow-up\n" {
		t.Fatalf("restored worktree = %q", got)
	}
	runGit(t, info.Path, "add", "README.md")
	if _, err := replacement.FinalizeNativeWork(ctx, info, issue, authorize); err != nil {
		t.Fatalf("continue restored rebase: %v", err)
	}
	if paused, err := rebaseInProgress(ctx, info.Path); err != nil || paused {
		t.Fatalf("continued rebase paused=%t, error=%v", paused, err)
	}
}

func TestTerminalCleanupDeletesCheckpointRef(t *testing.T) {
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
	checkpoint := publishLegacyCheckpointFixture(t, info, issue)
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

func TestAttemptPrivateCheckoutRecoversLegacyCheckpoint(t *testing.T) {
	t.Parallel()
	source := initSourceRepo(t)
	remote := initBareRemote(t)
	runGit(t, source, "remote", "add", "origin", remote)
	runGit(t, source, "push", "-u", "origin", "main")
	root := filepath.Join(t.TempDir(), "workspaces")
	backend, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	issue := Issue{ID: "wi_example", Identifier: "prj_example#1"}
	legacy, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	writeCheckpointFile(t, legacy.Path, "recovered.txt", "legacy checkpoint content\n")
	checkpoint := publishLegacyCheckpointFixture(t, legacy, issue)
	issue.AttemptWorkspace = AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_" + strings.Repeat("1", 32)}
	existing, err := backend.Existing(issue)
	if !errors.Is(err, ErrMissingWorkspace) || existing.Path != "" {
		t.Fatalf("unverified lookup selected a legacy checkout: %+v %v", existing, err)
	}
	issue.Checkpoint = &CheckpointRef{Ref: checkpoint.Ref, CommitSHA: checkpoint.CommitSHA, TreeSHA: checkpoint.TreeSHA}
	first, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path == legacy.Path || first.Branch == legacy.Branch || filepath.Dir(first.Path) != bestEffortCanonicalPath(root) {
		t.Fatalf("new worktree reused the old layout: legacy=%+v new=%+v", legacy, first)
	}
	for _, checkout := range []Info{legacy, first} {
		content, err := os.ReadFile(filepath.Join(checkout.Path, "recovered.txt"))
		if err != nil || string(content) != "legacy checkpoint content\n" {
			t.Fatalf("checkpoint content in %s is %q, error %v", checkout.Path, content, err)
		}
	}
	reused, err := backend.Existing(issue)
	if err != nil || reused.Path != first.Path {
		t.Fatalf("existing checkout did not prefer the canonical path: %+v %v", reused, err)
	}
	issue.AttemptWorkspace.Attempt = "attempt_" + strings.Repeat("2", 32)
	second, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}

	if second.Path == first.Path || second.Branch == first.Branch || second.Path == legacy.Path {
		t.Fatalf("two attempts share a checkout: legacy=%+v first=%+v second=%+v", legacy, first, second)
	}
	for _, identity := range []AttemptWorkspace{
		{Organization: "org_example", Project: "prj_example", Attempt: "attempt_" + strings.Repeat("1", 32)},
		{Organization: "org_example", Project: "prj_example", Attempt: "attempt_" + strings.Repeat("2", 32)},
	} {
		scratch, err := PrepareWorkerScratch(WithAttemptWorkspace(t.Context(), identity), legacy.Path)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(scratch) != filepath.Join(WorkerScratchRoot(legacy.Path), identity.Key()) {
			t.Fatalf("scratch %s lacks private attempt parent", scratch)
		}
		if err := CleanupWorkerScratch(legacy.Path, scratch); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if err := RemoveWorkerScratchRoot(legacy.Path); err != nil {
			t.Error(err)
		}
	})
}

func TestReview1973PrivateCheckoutKeepsPublicationBranch(t *testing.T) {
	for _, restore := range []bool{false, true} {
		name := "independent checkout"
		if restore {
			name = "checkpoint restore"
		}
		t.Run(name, func(t *testing.T) {
			source := initSourceRepo(t)
			publishCleanupSource(t, source)
			backend, err := NewLocalGit(LocalGitOptions{Root: t.TempDir(), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{ID: "wi_example", Identifier: "prj_example#1", BranchName: "example/publish"}
			legacy, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			runGit(t, legacy.Path, "commit", "--allow-empty", "-m", "published checkpoint")
			published := strings.TrimSpace(runGit(t, legacy.Path, "rev-parse", "HEAD"))
			runGit(t, legacy.Path, "push", "origin", "HEAD:refs/heads/"+issue.BranchName)
			if restore {
				writeCheckpointFile(t, legacy.Path, "progress.txt", "preserved progress")
				checkpoint := publishLegacyCheckpointFixture(t, legacy, issue)
				issue.Checkpoint = &checkpoint
			}
			issue.AttemptWorkspace = AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_11111111111111111111111111111111"}
			private, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if private.Path == legacy.Path || private.Branch == legacy.Branch || private.PublishBranch() != issue.BranchName ||
				strings.TrimSpace(runGit(t, legacy.Path, "branch", "--show-current")) != legacy.Branch {
				t.Fatalf("legacy=%+v private=%+v", legacy, private)
			}
			delivery, err := backend.DeliverableState(t.Context(), private, issue)
			if err != nil || !delivery.RemoteBranchExists || strings.TrimSpace(runGit(t, private.Path, "rev-parse", "HEAD")) != published {
				t.Fatalf("private checkout lost its publication ref: %+v %v", delivery, err)
			}
			if restore {
				for _, checkout := range []Info{legacy, private} {
					if got, err := os.ReadFile(filepath.Join(checkout.Path, "progress.txt")); err != nil || string(got) != "preserved progress" {
						t.Fatalf("checkpoint content in %s = %q, error %v", checkout.Branch, got, err)
					}
				}
			}
		})
	}
}

func TestIndependentDefaultPublicationRef(t *testing.T) {
	for _, restore := range []bool{false, true} {
		name := "independent checkout"
		if restore {
			name = "checkpoint restore"
		}
		t.Run(name, func(t *testing.T) {
			source := initSourceRepo(t)
			publishCleanupSource(t, source)
			backend, err := NewLocalGit(LocalGitOptions{Root: t.TempDir(), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{ID: "wi_example", Identifier: "prj_example#1"}
			legacy, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			runGit(t, legacy.Path, "commit", "--allow-empty", "-m", "published checkpoint")
			published := strings.TrimSpace(runGit(t, legacy.Path, "rev-parse", "HEAD"))
			runGit(t, legacy.Path, "push", "origin", "HEAD:refs/heads/"+legacy.Branch)
			if restore {
				writeCheckpointFile(t, legacy.Path, "progress.txt", "preserved progress")
				checkpoint := publishLegacyCheckpointFixture(t, legacy, issue)
				issue.Checkpoint = &checkpoint
			}
			issue.AttemptWorkspace = AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_11111111111111111111111111111111"}
			private, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if private.Path == legacy.Path || private.Branch == legacy.Branch || private.PublishBranch() != legacy.Branch ||
				strings.TrimSpace(runGit(t, legacy.Path, "branch", "--show-current")) != legacy.Branch {
				t.Errorf("publication ref was %s, now %s", legacy.Branch, private.PublishBranch())
			}
			delivery, err := backend.DeliverableState(t.Context(), private, issue)
			if err != nil || !delivery.RemoteBranchExists || strings.TrimSpace(runGit(t, private.Path, "rev-parse", "HEAD")) != published {
				t.Fatalf("private checkout lost its publication ref: %+v %v", delivery, err)
			}
			if restore {
				for _, checkout := range []Info{legacy, private} {
					if got, err := os.ReadFile(filepath.Join(checkout.Path, "progress.txt")); err != nil || string(got) != "preserved progress" {
						t.Fatalf("checkpoint content in %s = %q, error %v", checkout.Branch, got, err)
					}
				}
			}
		})
	}
}
