package workspace

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

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLocalGitNativeReworkOwnsPausedRebase(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                string
		inheritedPause      bool
		unresolved          bool
		alter               string
		infrastructure      bool
		clean               bool
		advanceTarget       bool
		authorityLost       bool
		unstaged            bool
		untracked           bool
		legacy              bool
		checkpoint          bool
		checkpointRefusal   string
		checkpointCommitted bool
		absorbedConflict    bool
	}{
		{name: "runner prepares unsigned conflict"},
		{name: "absorbed conflict retains an idempotent publication head", absorbedConflict: true},
		{name: "inherited absorbed conflict retains a publication head", absorbedConflict: true, inheritedPause: true},
		{name: "inherited signed pause", inheritedPause: true},
		{name: "legacy unpublished pause", legacy: true},
		{name: "resolved paused replay integrates selected target", advanceTarget: true},
		{name: "unresolved conflict", unresolved: true},
		{name: "authority lost before continuation", authorityLost: true},
		{name: "unstaged paused resolution retained", unstaged: true},
		{name: "untracked paused source retained", untracked: true},
		{name: "wrong branch", alter: "head-name"},
		{name: "branch advanced", alter: "branch"},
		{name: "additional refs", alter: "update-refs"},
		{name: "executable todo", alter: "git-rebase-todo"},
		{name: "assigned ref unavailable", infrastructure: true},
		{name: "clean packed branch", clean: true},
		{name: "restored dirty checkpoint rebases", clean: true, checkpoint: true},
		{name: "restored dirty checkpoint pauses on conflict", checkpoint: true},
		{name: "restored checkpoint retains work through an earlier conflict", checkpoint: true, checkpointCommitted: true},
		{name: "restored dirty checkpoint retains source on authority loss", clean: true, checkpoint: true, checkpointRefusal: "authority"},
		{name: "changed dirty checkpoint is not committed", clean: true, checkpoint: true, checkpointRefusal: "source"},
		{name: "distinct staged checkpoint work is preserved", clean: true, checkpoint: true, checkpointRefusal: "index"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			remote := initBareRemote(t)
			runGit(t, source, "remote", "add", "origin", remote)
			runGit(t, source, "push", "-u", "origin", "main")
			backend, err := NewBackend(KindLocalGit, LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			owner := backend.(*LocalGit)
			issue := Issue{Identifier: "native#141", NativeRework: true}
			var info Info
			if test.legacy {
				issue = Issue{ID: "wi_pause", ProjectID: "local-alias", Identifier: "prj_example#141", NativeRework: true}
				key := legacyIssueKey(issue)
				info = Info{Key: key, Path: filepath.Join(owner.root, key), Branch: autoBranchPrefix + strings.ToLower(key)}
				runGit(t, source, "worktree", "add", "-b", info.Branch, info.Path, "HEAD")
			} else {
				info, err = backend.Create(t.Context(), issue)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("feature\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, info.Path, "add", "README.md")
			if !test.checkpoint || test.checkpointCommitted {
				runGit(t, info.Path, "commit", "-m", "feature")
			}
			if test.checkpoint {
				writeCheckpointFile(t, info.Path, "retained.txt", "staged work\n")
				runGit(t, info.Path, "add", "retained.txt")
				writeCheckpointFile(t, info.Path, "retained.txt", "latest uncommitted work\n")
				checkpoint := publishLegacyCheckpointFixture(t, info, issue)
				runGit(t, source, "worktree", "remove", "--force", info.Path)
				checkpoint.HeadSHA = ""
				issue.Checkpoint = &checkpoint
				info, err = backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				if diff, err := owner.DiffStat(t.Context(), info, issue); err != nil || diff.IsEmpty() {
					t.Fatalf("restored checkpoint is not dirty: %+v, %v", diff, err)
				}
			}
			original := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			runGit(t, source, "branch", "unrelated", original)
			sibling, err := backend.Create(t.Context(), Issue{Identifier: "native#142"})
			if err != nil {
				t.Fatal(err)
			}
			siblingHead := strings.TrimSpace(runGit(t, sibling.Path, "rev-parse", "HEAD"))
			siblingFiles := readFile(t, filepath.Join(sibling.Path, "README.md"))
			baseFile := "README.md"
			if test.clean {
				baseFile = "base.txt"
			}
			if err := os.WriteFile(filepath.Join(source, baseFile), []byte("base\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, source, "add", baseFile)
			runGit(t, source, "commit", "-m", "base conflict")
			runGit(t, source, "push", "origin", "main")
			base := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			signer := filepath.Join(t.TempDir(), "signer")
			if err := os.WriteFile(signer, []byte("personal signer sentinel\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, source, "config", "commit.gpgsign", "true")
			runGit(t, source, "config", "gpg.program", signer)
			runGit(t, source, "config", "rebase.updateRefs", "true")
			configPath := filepath.Join(source, ".git", "config")
			configBefore := readFile(t, configPath)
			runGit(t, source, "pack-refs", "--all")
			if test.inheritedPause {
				runGit(t, info.Path, "fetch", "origin")
				if _, err := runGitAt(t.Context(), info.Path, "rebase", "--no-update-refs", "origin/main"); err == nil {
					t.Fatal("expected real source conflict")
				}
				if got := readFile(t, filepath.Join(linkedWorktreeGitDir(t, info.Path), "rebase-merge", "gpg_sign_opt")); !strings.Contains(got, "-S") {
					t.Fatalf("signer choice was not recorded: %q", got)
				}
			}
			if test.legacy {
				runGit(t, info.Path, "fetch", "origin")
				if _, err := runGitAt(t.Context(), info.Path, "rebase", "--no-update-refs", "origin/main"); err == nil {
					t.Fatal("expected legacy source conflict")
				}
				status := runGit(t, info.Path, "status", "--porcelain")
				index := runGit(t, info.Path, "ls-files", "--stage", "-z")
				contents := readFile(t, filepath.Join(info.Path, "README.md"))
				if _, err := owner.PrepareRework(t.Context(), info, issue, MergePrepareOptions{}, func(ctx context.Context) error { return ctx.Err() }); !errors.Is(err, ErrMergeResolutionInvalid) {
					t.Fatalf("unpublished legacy replay authorized: %v", err)
				}
				if _, err := owner.FinalizeNativeWork(t.Context(), info, issue, func(ctx context.Context) error { return ctx.Err() }); !errors.Is(err, ErrMergeResolutionInvalid) {
					t.Fatalf("unpublished legacy replay finalized: %v", err)
				}

				if _, err := owner.Existing(issue); !errors.Is(err, ErrMissingWorkspace) {
					t.Fatalf("unpublished legacy replay discovered: %v", err)
				}
				if got := runGit(t, info.Path, "status", "--porcelain"); got != status || runGit(t, info.Path, "ls-files", "--stage", "-z") != index || readFile(t, filepath.Join(info.Path, "README.md")) != contents {
					t.Fatal("refusal changed retained legacy conflict")
				}
				if got := strings.TrimSpace(runGit(t, source, "rev-parse", "refs/heads/"+info.Branch)); got != original {
					t.Fatal("refusal moved legacy branch")
				}
				return
			}
			if test.checkpointRefusal != "" {
				if test.checkpointRefusal == "source" {
					writeCheckpointFile(t, info.Path, "README.md", "unverified source\n")
				}
				if test.checkpointRefusal == "index" {
					writeCheckpointFile(t, info.Path, "retained.txt", "unpublished staged checkpoint content with a different size\n")
					runGit(t, info.Path, "add", "retained.txt")
					writeCheckpointFile(t, info.Path, "retained.txt", "latest uncommitted work\n")
				}
				before, err := owner.RecoveryState(t.Context(), info, issue)
				if err != nil {
					t.Fatal(err)
				}
				indexPath := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "--git-path", "index"))
				index := readFile(t, indexPath)
				authorityErr := errors.New("execution lease lost")
				checks := 0
				prepared, err := owner.PrepareRework(t.Context(), info, issue, MergePrepareOptions{}, func(ctx context.Context) error {
					checks++
					if test.checkpointRefusal == "authority" && checks > 1 {
						return authorityErr
					}
					return ctx.Err()
				})
				if test.checkpointRefusal == "authority" && !errors.Is(err, authorityErr) || (test.checkpointRefusal == "source" || test.checkpointRefusal == "index") && (err != nil || prepared.Status != MergePrepareStatusDirty) {
					t.Fatalf("unsafe checkpoint preparation: %+v, %v", prepared, err)
				}
				after, err := owner.RecoveryState(t.Context(), info, issue)
				if err != nil || before.HeadSHA != after.HeadSHA || before.WorkspaceFingerprint != after.WorkspaceFingerprint || index != readFile(t, indexPath) {
					t.Fatalf("refused checkpoint preparation changed source: before=%+v after=%+v error=%v", before, after, err)
				}
				return
			}
			prepared, err := owner.PrepareRework(t.Context(), info, issue, MergePrepareOptions{}, func(ctx context.Context) error { return ctx.Err() })
			wantStatus := MergePrepareStatusConflict
			if test.clean {
				wantStatus = MergePrepareStatusClean
			}
			if err != nil || prepared.BaseSHA != base || prepared.Status != wantStatus || !test.clean && (len(prepared.ConflictPaths) != 1 || prepared.ConflictPaths[0] != "README.md") {
				t.Fatalf("prepare = %#v, %v", prepared, err)
			}
			if test.checkpoint {
				issue.Checkpoint = nil
				if test.clean {
					runGit(t, info.Path, "merge-base", "--is-ancestor", base, "HEAD")
				} else if paused, err := rebaseInProgress(t.Context(), info.Path); err != nil || !paused {
					t.Fatalf("checkpoint conflict has no paused replay: paused=%v error=%v", paused, err)
				}
				got := runGit(t, info.Path, "show", info.Branch+":retained.txt")
				if !test.checkpointCommitted {
					got = readFile(t, filepath.Join(info.Path, "retained.txt"))
				}
				if got != "latest uncommitted work\n" {
					t.Fatalf("checkpoint work lost: %q", got)
				}
			}
			if !test.clean && !strings.Contains(readFile(t, filepath.Join(info.Path, "README.md")), "<<<<<<<") {
				t.Fatal("fixture has no source conflict")
			}
			resumed, err := backend.Create(t.Context(), issue)
			if err != nil || resumed.Created || resumed.Path != info.Path || resumed.Branch != info.Branch {
				t.Fatalf("native rework creation replaced paused transaction: %#v, %v", resumed, err)
			}
			roots, err := GitMetadataWritableRoots(t.Context(), info.Path)
			if err != nil {
				t.Fatal(err)
			}
			common := mustCanonicalExistingPath(t, filepath.Join(source, ".git"))
			for _, root := range roots {
				ownedRef := filepath.Join(common, "refs", "heads", filepath.FromSlash(info.Branch))
				ownedLog := filepath.Join(common, "logs", "refs", "heads", filepath.FromSlash(info.Branch))
				if root != linkedWorktreeGitDir(t, info.Path) && root != filepath.Join(common, "objects") && (!test.clean || root != ownedRef && root != ownedRef+".lock" && root != ownedLog && root != ownedLog+".lock") {
					t.Fatalf("paused worker has shared-ref authority: %s", root)
				}
			}
			release, err := owner.acquireSourceOperation(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			release()
			if !test.unresolved && !test.clean {
				sandboxed := false
				if runtime.GOOS == "darwin" {
					profile := fmt.Sprintf("(version 1)(allow default)(deny file-write*)(allow file-write* (literal \"/dev/null\") (subpath %q)", mustCanonicalExistingPath(t, info.Path))
					var allowedRoots strings.Builder
					for _, root := range roots {
						fmt.Fprintf(&allowedRoots, " (subpath %q)", root)
					}
					profile += allowedRoots.String()
					profile += ")"
					cmd := exec.CommandContext(t.Context(), "sandbox-exec", "-p", profile, "/bin/sh", "-c", `printf 'resolved\n' > "$1/README.md" && git -C "$1" add README.md`, "resolve", info.Path)
					output, err := cmd.CombinedOutput()
					if err != nil && strings.Contains(string(output), "sandbox_apply: Operation not permitted") {
						t.Log("nested macOS sandbox unavailable; verifying source/index and explicit root boundaries")
					} else if err != nil {
						t.Fatalf("sandboxed source resolution: %v: %s", err, output)
					} else {
						sandboxed = true
					}
				}
				if !sandboxed {
					if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("resolved\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					runGit(t, info.Path, "add", "README.md")
				}
			}
			if test.absorbedConflict {
				issue.Source = &ChangeSource{Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: strings.TrimSpace(runGit(t, info.Path, "rev-parse", original+"^")), HeadSHA: original}}}
				writeCheckpointFile(t, info.Path, "README.md", "base\n")
				runGit(t, info.Path, "add", "README.md")
			}
			if test.alter == "branch" {
				runGit(t, source, "update-ref", "refs/heads/"+info.Branch, base)
			} else if test.alter != "" {
				value := "refs/heads/unrelated\n"
				if test.alter == "git-rebase-todo" {
					value = "exec false\n"
				}
				if err := os.WriteFile(filepath.Join(linkedWorktreeGitDir(t, info.Path), "rebase-merge", test.alter), []byte(value), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.infrastructure {
				lock := filepath.Join(common, "refs", "heads", filepath.FromSlash(info.Branch)) + ".lock"
				if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(lock, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			selectedBase := base
			if test.advanceTarget {
				if err := os.WriteFile(filepath.Join(source, "UPSTREAM.md"), []byte("later target work\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, source, "add", "UPSTREAM.md")
				runGit(t, source, "-c", "commit.gpgsign=false", "commit", "-m", "later target work")
				runGit(t, source, "push", "origin", "main")
				runGit(t, source, "fetch", "origin")
				selectedBase = strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			}
			if test.unstaged {
				writeCheckpointFile(t, info.Path, "README.md", "unstaged resolution\n")
			}
			if test.untracked {
				writeCheckpointFile(t, info.Path, "untracked.txt", "retained source\n")
			}
			if test.alter != "" || test.authorityLost || test.unstaged || test.untracked {
				before, err := owner.RecoveryState(t.Context(), info, issue)
				if err != nil {
					t.Fatal(err)
				}
				indexPath := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "--git-path", "index"))
				index := readFile(t, indexPath)
				authorityErr := errors.New("execution lease lost")
				checks := 0
				prepared, err := owner.PrepareRework(t.Context(), info, issue, MergePrepareOptions{}, func(ctx context.Context) error {
					checks++
					if test.authorityLost && checks > 1 {
						return authorityErr
					}
					return ctx.Err()
				})
				if test.alter != "" && !errors.Is(err, ErrMergeResolutionInvalid) || test.authorityLost && !errors.Is(err, authorityErr) || (test.unstaged || test.untracked) && (err != nil || prepared.Status != MergePrepareStatusDirty) {
					t.Fatalf("unsafe preparation: %+v, %v", prepared, err)
				}
				after, err := owner.RecoveryState(t.Context(), info, issue)
				if err != nil || before.HeadSHA != after.HeadSHA || before.WorkspaceFingerprint != after.WorkspaceFingerprint || index != readFile(t, indexPath) {
					t.Fatalf("refused preparation changed source custody: before=%+v after=%+v error=%v", before, after, err)
				}
				if test.authorityLost || test.unstaged || test.untracked {
					return
				}
			}
			finalBase, err := owner.FinalizeNativeWork(t.Context(), info, issue, func(ctx context.Context) error { return ctx.Err() })
			invalid := test.unresolved || test.alter != ""
			if invalid {
				if !errors.Is(err, ErrMergeResolutionInvalid) {
					t.Fatalf("invalid resolution = %v", err)
				}
			} else if test.infrastructure {
				if err == nil || errors.Is(err, ErrMergeResolutionInvalid) {
					t.Fatalf("Git metadata failure became a source conflict: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if finalBase != selectedBase {
					t.Fatalf("final baseline = %s, want selected integration base %s", finalBase, selectedBase)
				}
				diff, err := GitFileDiffs(t.Context(), info.Path, finalBase, 1<<20)
				wantFiles := 1
				if test.checkpoint {
					wantFiles++
				}
				if test.absorbedConflict {
					wantFiles = 0
				}
				if err != nil || diff.BaseSHA != selectedBase || len(diff.Files) != wantFiles || wantFiles > 0 && diff.Files[0].Path != "README.md" {
					t.Fatalf("final baseline lost the owned resolution: %+v, %v", diff, err)
				}
				head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
				if head == original || head == base || head != strings.TrimSpace(runGit(t, source, "rev-parse", "refs/heads/"+info.Branch)) {
					t.Fatalf("final head is not a new assigned-branch commit: %s", head)
				}
				if got := strings.TrimSpace(runGit(t, info.Path, "symbolic-ref", "--short", "HEAD")); got != info.Branch {
					t.Fatalf("final branch = %s", got)
				}
				wantContent := "resolved\n"
				if test.clean {
					wantContent = "feature\n"
				}
				if test.absorbedConflict {
					wantContent = "base\n"
					if _, err := CaptureChangeSource(t.Context(), info.Path, finalBase, head); err != nil {
						t.Fatalf("empty resolution cannot be retained for publication: %v", err)
					}
					repeatedBase, err := owner.FinalizeNativeWork(t.Context(), info, issue, func(ctx context.Context) error { return ctx.Err() })
					if err != nil || repeatedBase != finalBase || strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")) != head {
						t.Fatalf("repeated finalization changed the resolved source: base=%s error=%v", repeatedBase, err)
					}
				}
				if got := readFile(t, filepath.Join(info.Path, "README.md")); got != wantContent {
					t.Fatalf("resolution = %q", got)
				}
				if test.checkpoint && readFile(t, filepath.Join(info.Path, "retained.txt")) != "latest uncommitted work\n" {
					t.Fatal("final rebase lost retained checkpoint work")
				}
				if got := runGit(t, info.Path, "cat-file", "-p", head); strings.Contains(got, "gpgsig") {
					t.Fatal("machine rebase invoked signing")
				}
				runGit(t, info.Path, "merge-base", "--is-ancestor", base, head)
				if paused, err := rebaseInProgress(t.Context(), info.Path); paused || err != nil {
					t.Fatalf("rebase remains paused: %v", err)
				}
			}
			if got := strings.TrimSpace(runGit(t, source, "rev-parse", "unrelated")); got != original {
				t.Fatal("unrelated ref changed")
			}
			if got := strings.TrimSpace(runGit(t, sibling.Path, "rev-parse", "HEAD")); got != siblingHead {
				t.Fatal("sibling head changed")
			}
			if readFile(t, filepath.Join(sibling.Path, "README.md")) != siblingFiles || readFile(t, configPath) != configBefore || readFile(t, signer) != "personal signer sentinel\n" {
				t.Fatal("sibling files or host signing material changed")
			}
		})
	}
}

func TestLocalGitReworkPreservesIncorporatedSource(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		incorporation string
		conflict      bool
		followup      bool
	}{
		{name: "merged source awaiting evidence", incorporation: "merge"},
		{name: "squashed source awaiting evidence", incorporation: "squash"},
		{name: "incorporated followup retains its candidate", incorporation: "squash", followup: true},
		{name: "undelivered source still rebases"},
		{name: "conflicting source still pauses", conflict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			remote := initBareRemote(t)
			runGit(t, source, "remote", "add", "origin", remote)
			runGit(t, source, "push", "-u", "origin", "main")
			base := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			backend, err := NewBackend(KindLocalGit, LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			owner := backend.(*LocalGit)
			issue := Issue{Identifier: "native#141", NativeRework: true, BaseRef: base}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			writeCheckpointFile(t, info.Path, "README.md", "retained source\n")
			runGit(t, info.Path, "add", "README.md")
			runGit(t, info.Path, "commit", "-m", "retain source")
			head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			issue.Source = &ChangeSource{Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: base, HeadSHA: head}}}
			if test.followup {
				writeCheckpointFile(t, info.Path, "README.md", "updated retained source\n")
				runGit(t, info.Path, "add", "README.md")
				runGit(t, info.Path, "commit", "-m", "revise source")
				head = strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			}
			switch test.incorporation {
			case "merge":
				runGit(t, source, "merge", "--ff-only", head)
			case "squash":
				runGit(t, source, "merge", "--squash", head)
				runGit(t, source, "commit", "-m", "deliver source")
			}
			writeCheckpointFile(t, source, "base.txt", "unrelated integration change\n")
			runGit(t, source, "add", "base.txt")
			if test.conflict {
				writeCheckpointFile(t, source, "README.md", "conflicting integration change\n")
				runGit(t, source, "add", "README.md")
			}
			runGit(t, source, "commit", "-m", "advance integration")
			runGit(t, source, "push", "origin", "main")
			target := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			validate := func(ctx context.Context) error { return ctx.Err() }
			prepared, err := owner.PrepareRework(t.Context(), info, issue, MergePrepareOptions{}, validate)
			if test.conflict {
				if err != nil || prepared.Status != MergePrepareStatusConflict || len(prepared.ConflictPaths) != 1 || prepared.ConflictPaths[0] != "README.md" {
					t.Fatalf("source conflict did not retain resolution custody: %+v, %v", prepared, err)
				}
				return
			}
			if err != nil || prepared.Status != MergePrepareStatusClean {
				t.Fatalf("prepare retained source: %+v, %v", prepared, err)
			}
			wantBase := target
			if test.incorporation != "" {
				wantBase = base
				if prepared.HeadSHA != head {
					t.Fatalf("incorporation erased source awaiting review: head=%s want=%s", prepared.HeadSHA, head)
				}
			} else {
				runGit(t, info.Path, "merge-base", "--is-ancestor", target, prepared.HeadSHA)
			}
			finalBase, err := owner.FinalizeNativeWork(t.Context(), info, issue, validate)
			if err != nil || prepared.BaseSHA != wantBase || finalBase != wantBase {
				t.Fatalf("finalized source base: prepared=%s final=%s want=%s error=%v", prepared.BaseSHA, finalBase, wantBase, err)
			}
			diff, err := GitFileDiffs(t.Context(), info.Path, finalBase, tracker.MaxDiffBytes)
			if err != nil || diff.HeadSHA != prepared.HeadSHA || len(diff.Files) != 1 || diff.Files[0].Path != "README.md" {
				t.Fatalf("final publication diff lost the owned delta: %+v, %v", diff, err)
			}
		})
	}
}

func TestLocalGitReworkPreparation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		cached       bool
		signed       bool
		indexLock    bool
		hookFails    bool
		dirty        string
		paused       string
		fetchFails   bool
		signingFails bool
	}{
		{name: "unstaged work", dirty: "unstaged"},
		{name: "staged work", dirty: "staged"},
		{name: "staged work canceled by unstaged edit", dirty: "canceled"},
		{name: "paused merge conflict", paused: "merge"},
		{name: "paused apply conflict", paused: "apply"},
		{name: "staged paused resolution continues", paused: "resolved"},
		{name: "paused rebase on another branch is preserved", paused: "foreign"},
		{name: "incomplete rebase metadata is preserved", paused: "incomplete"},
		{name: "fetch failure", fetchFails: true},
		{name: "signing failure on a later pick", signed: true, signingFails: true},
		{name: "unsigned conflict"},
		{name: "signed conflict", signed: true},
		{name: "cached resolution", cached: true},
		{name: "signed cached resolution", signed: true, cached: true},
		{name: "index failure", indexLock: true},
		{name: "rebase hook refusal without conflict", hookFails: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			remote := initBareRemote(t)
			runGit(t, source, "remote", "add", "origin", remote)
			runGit(t, source, "push", "-u", "origin", "main")
			root := filepath.Join(t.TempDir(), "workspaces")
			owner, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "example#1", NativeRework: true, ProgressBaseRef: "main"}
			info, err := owner.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if test.signingFails {
				writeCheckpointFile(t, info.Path, "first.txt", "first\n")
				runGit(t, info.Path, "add", "first.txt")
				runGit(t, info.Path, "commit", "-m", "first-pick")
				writeCheckpointFile(t, info.Path, "second.txt", "second\n")
				runGit(t, info.Path, "add", "second.txt")
				runGit(t, info.Path, "commit", "-m", "replay-signing-failure")
			}
			writeCheckpointFile(t, info.Path, "README.md", "feature\n")
			runGit(t, info.Path, "add", "README.md")
			runGit(t, info.Path, "commit", "-m", "feature")
			writeCheckpointFile(t, source, "README.md", "base\n")
			runGit(t, source, "add", "README.md")
			runGit(t, source, "commit", "-m", "base")
			runGit(t, source, "push", "origin", "main")
			base := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			if test.cached {
				runGit(t, source, "config", "rerere.enabled", "true")
				runGit(t, source, "config", "rerere.autoupdate", "true")
				if _, err := runGitAt(t.Context(), info.Path, "rebase", base); err == nil {
					t.Fatal("fixture rebase did not conflict")
				}
				writeCheckpointFile(t, info.Path, "README.md", "cached resolution\n")
				runGit(t, info.Path, "add", "README.md")
				runGit(t, info.Path, "rerere")
				runGit(t, info.Path, "rebase", "--abort")
			}
			if test.signed {
				key := filepath.Join(t.TempDir(), "signing-key")
				if output, err := exec.CommandContext(t.Context(), "ssh-keygen", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
					t.Fatalf("create signing fixture: %v: %s", err, output)
				}
				if test.signingFails {
					program, err := exec.LookPath("ssh-keygen")
					if err != nil {
						t.Fatal(err)
					}
					signer := filepath.Join(t.TempDir(), "signer")
					script := "#!/bin/sh\nfor arg do payload=$arg; done\nif test -f \"$payload\"; then\n content=$(cat \"$payload\")\n case \"$content\" in *replay-signing-failure*) exit 1;; esac\nfi\nexec " + shellQuote(program) + " \"$@\"\n"
					if err := os.WriteFile(signer, []byte(script), 0o700); err != nil {
						t.Fatal(err)
					}
					runGit(t, source, "config", "gpg.ssh.program", signer)
				}
				runGit(t, source, "config", "commit.gpgsign", "true")
				runGit(t, source, "config", "gpg.format", "ssh")
				runGit(t, source, "config", "user.signingkey", key)
				owner, err = NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				if owner.CommitReport().Signing != "ssh" {
					t.Fatal("fixture did not enable signing")
				}
			}
			if test.indexLock {
				lock := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "--git-path", "index.lock"))
				if err := os.WriteFile(lock, []byte("held\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.hookFails {
				hook := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "--git-path", "hooks/pre-rebase"))
				if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if test.paused != "" {
				runGit(t, info.Path, "fetch", "origin")
				mode := "--merge"
				if test.paused == "apply" {
					mode = "--apply"
				}
				if _, err := runGitAt(t.Context(), info.Path, "-c", "commit.gpgsign=false", "rebase", mode, "--no-update-refs", "--no-autostash", "--no-rebase-merges", base); err == nil {
					t.Fatal("fixture did not pause a real rebase")
				}
				if test.paused == "resolved" {
					writeCheckpointFile(t, info.Path, "README.md", "resolved retained work\n")
					runGit(t, info.Path, "add", "README.md")
				}
				if test.paused == "foreign" || test.paused == "incomplete" {
					name := "head-name"
					if test.paused == "incomplete" {
						name = "orig-head"
					}
					path := filepath.Join(linkedWorktreeGitDir(t, info.Path), "rebase-merge", name)
					if test.paused == "incomplete" {
						if err := os.Rename(path, path+".saved"); err != nil {
							t.Fatal(err)
						}
					} else if err := os.WriteFile(path, []byte("refs/heads/other\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				_, rawErr := runGitAt(t.Context(), info.Path, "-c", "rerere.enabled=false", "rebase", "--no-update-refs", "--no-autostash", "--no-rebase-merges", base)
				var commandErr *CommandError
				if !errors.As(rawErr, &commandErr) || commandErr.ExitCode == 0 {
					t.Fatalf("leftover rebase did not refuse another start: %v", rawErr)
				}
				var preparationErr *ReworkPreparationError
				if classified := reworkPreparationError(rawErr); !errors.As(classified, &preparationErr) || preparationErr.Reason != ReworkPreparationRebaseInProgress {
					t.Fatalf("leftover replay Git failure has no typed reason: %v, output=%s", classified, commandErr.Output)
				}
				before := runGit(t, info.Path, "diff", "--binary") + runGit(t, info.Path, "ls-files", "--stage")
				prepared, err := owner.PrepareRework(t.Context(), info, issue, MergePrepareOptions{TargetBranch: "main"}, func(ctx context.Context) error { return ctx.Err() })
				if test.paused == "foreign" || test.paused == "incomplete" {
					var preparationErr *ReworkPreparationError
					if !errors.As(err, &preparationErr) || preparationErr.Reason != ReworkPreparationRebaseInProgress {
						t.Fatalf("unverified replay has no typed refusal: %+v, %v", prepared, err)
					}
				} else if test.paused == "resolved" {
					if err != nil || prepared.Status != MergePrepareStatusClean || readFile(t, filepath.Join(info.Path, "README.md")) != "resolved retained work\n" {
						t.Fatalf("staged replay did not finish safely: %+v, %v", prepared, err)
					}
					return
				} else if err != nil || prepared.Status != MergePrepareStatusConflict || prepared.BaseSHA != base {
					t.Fatalf("retained conflict did not reach agent: %+v, %v", prepared, err)
				}
				if after := runGit(t, info.Path, "diff", "--binary") + runGit(t, info.Path, "ls-files", "--stage"); after != before {
					t.Fatal("preparation changed retained replay work")
				}
				return
			}
			if test.fetchFails {
				runGit(t, info.Path, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
			}
			if test.dirty != "" {
				writeCheckpointFile(t, info.Path, "README.md", "unpublished work\n")
				if test.dirty != "unstaged" {
					runGit(t, info.Path, "add", "README.md")
				}
				if test.dirty == "canceled" {
					writeCheckpointFile(t, info.Path, "README.md", "feature\n")
				}
				_, rawErr := owner.runCommit(t.Context(), info.Path, nil, "-c", "rerere.enabled=false", "rebase", "--no-update-refs", "--no-autostash", "--no-rebase-merges", base)
				var commandErr *CommandError
				if !errors.As(rawErr, &commandErr) || commandErr.ExitCode != 1 || !strings.Contains(commandErr.Output, "cannot rebase:") {
					t.Fatalf("dirty fixture did not reproduce Git refusal: %v", rawErr)
				}
				conflicts, conflictErr := reworkConflictPaths(t.Context(), info.Path)
				if conflictErr != nil || len(conflicts) != 0 {
					t.Fatalf("dirty refusal has unmerged entries: %v, %v", conflicts, conflictErr)
				}
				before := runGit(t, info.Path, "diff", "--binary") + runGit(t, info.Path, "diff", "--cached", "--binary")
				prepared, err := owner.PrepareRework(t.Context(), info, issue, MergePrepareOptions{TargetBranch: "main"}, func(ctx context.Context) error { return ctx.Err() })
				if err != nil || prepared.Status != MergePrepareStatusDirty {
					t.Fatalf("dirty work did not reach agent: %+v, %v", prepared, err)
				}
				if after := runGit(t, info.Path, "diff", "--binary") + runGit(t, info.Path, "diff", "--cached", "--binary"); after != before {
					t.Fatal("preparation changed unpublished index or worktree content")
				}
				return
			}
			prepared, err := owner.PrepareRework(t.Context(), info, issue, MergePrepareOptions{TargetBranch: "main"}, func(ctx context.Context) error { return ctx.Err() })
			if test.signingFails || test.fetchFails {
				want := ReworkPreparationSigningFailed
				if test.fetchFails {
					want = ReworkPreparationFetchFailed
				}
				var preparationErr *ReworkPreparationError
				var commandErr *CommandError
				hasCommand := errors.As(err, &commandErr)
				if !errors.As(err, &preparationErr) || preparationErr.Reason != want || !hasCommand || err.Error() != "rework preparation failed: "+string(want) {
					t.Fatalf("preparation failure lost bounded reason: %+v, %v", prepared, err)
				}
				conflicts, conflictErr := reworkConflictPaths(t.Context(), info.Path)
				if conflictErr != nil || len(conflicts) != 0 {
					t.Fatalf("failure unexpectedly has unmerged entries: %v, %v", conflicts, conflictErr)
				}
				if test.signingFails {
					if commandErr.ExitCode != 1 || strings.TrimSpace(runGit(t, info.Path, "log", "-1", "--format=%s")) != "first-pick" {
						t.Fatal("fixture did not fail signing after the first pick")
					}
					owner.commits.Report.Signing = "unsigned"
					before := strings.TrimSpace(runGit(t, info.Path, "write-tree"))
					retried, err := owner.PrepareRework(t.Context(), info, issue, MergePrepareOptions{TargetBranch: "main"}, func(ctx context.Context) error { return ctx.Err() })
					if err != nil || retried.Status != MergePrepareStatusConflict || retried.BaseSHA != base {
						t.Fatalf("signing failure's retained replay did not reach its next conflict: %+v, %v", retried, err)
					}
					if after := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD^{tree}")); after != before {
						t.Fatal("signing retry did not preserve and commit unpublished staged work")
					}
				}
				return
			}
			if test.indexLock || test.hookFails {
				var commandErr *CommandError
				if !errors.As(err, &commandErr) || prepared.Status == MergePrepareStatusConflict || commandErr.ExitCode <= 0 {
					t.Fatalf("Git failure became a source conflict: %+v, %v", prepared, err)
				}
				if test.hookFails && !strings.Contains(commandErr.Output, "pre-rebase hook refused") {
					t.Fatalf("fixture did not reproduce hook refusal: exit=%d, output=%s", commandErr.ExitCode, commandErr.Output)
				}
				return
			}
			if err != nil || prepared.Status != MergePrepareStatusConflict || prepared.BaseSHA != base || len(prepared.ConflictPaths) != 1 || prepared.ConflictPaths[0] != "README.md" {
				t.Fatalf("conflict did not reach resolution: %+v, %v", prepared, err)
			}
			if paused, err := owner.verifyReworkBranch(t.Context(), info, issue); err != nil || !paused {
				t.Fatalf("conflict lost assigned replay: paused=%v, %v", paused, err)
			}
			if contents := readFile(t, filepath.Join(info.Path, "README.md")); !strings.Contains(contents, "<<<<<<<") || strings.Contains(contents, "cached resolution") {
				t.Fatalf("agent did not receive conflict markers: %q", contents)
			}
		})
	}
}
