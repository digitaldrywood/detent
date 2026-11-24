package workspace

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace/checkpoint"
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
		name             string
		incorporation    string
		conflict         bool
		followup         bool
		remoteOnly       bool
		dirtySubmodule   bool
		noAdvanceTarget  bool
	}{
		{name: "merged source awaiting evidence", incorporation: "merge"},
		{name: "merged source with dirty submodule under ignoreSubmodules=all", incorporation: "merge", dirtySubmodule: true},
		{name: "merged source awaiting evidence on remote-only target", incorporation: "merge", remoteOnly: true},
		{name: "squashed source awaiting evidence", incorporation: "squash"},
		{name: "incorporated followup retains its candidate", incorporation: "squash", followup: true},
		{name: "fast-forwarded followup retains its candidate across repeated prepare", incorporation: "merge", followup: true},
		{name: "fast-forwarded followup at exact target head retains base", incorporation: "merge", followup: true, noAdvanceTarget: true},
		{name: "undelivered source still rebases"},
		{name: "undelivered source without tracking refs still counts unpushed", remoteOnly: true},
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
			if test.dirtySubmodule {
				subRepo := initSourceRepo(t)
				runGit(t, info.Path, "-c", "protocol.file.allow=always", "submodule", "add", subRepo, "submod")
			}
			runGit(t, info.Path, "commit", "-m", "retain source")
			head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			issue.Source = &ChangeSource{Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: base, HeadSHA: head}}}
			if test.dirtySubmodule {
				runGit(t, info.Path, "config", "diff.ignoreSubmodules", "all")
				writeCheckpointFile(t, filepath.Join(info.Path, "submod"), "README.md", "dirty submodule edit\n")
			}
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
			if !test.noAdvanceTarget {
				writeCheckpointFile(t, source, "base.txt", "unrelated integration change\n")
				runGit(t, source, "add", "base.txt")
				if test.conflict {
					writeCheckpointFile(t, source, "README.md", "conflicting integration change\n")
					runGit(t, source, "add", "README.md")
				}
				runGit(t, source, "commit", "-m", "advance integration")
			}
			target := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			prepareOpts := MergePrepareOptions{}
			if test.remoteOnly {
				runGit(t, source, "push", remote, target+":refs/heads/remote-only")
				if test.incorporation == "" {
					for ref := range strings.SplitSeq(strings.TrimSpace(runGit(t, source, "for-each-ref", "--format=%(refname)", "refs/remotes")), "\n") {
						if ref != "" {
							runGit(t, source, "update-ref", "-d", ref)
						}
					}
				}
				issue.ProgressBaseRef = "remote-only"
				prepareOpts.TargetBranch = "remote-only"
			} else {
				runGit(t, source, "push", "origin", "main")
			}
			validate := func(ctx context.Context) error { return ctx.Err() }
			if test.noAdvanceTarget {
				repoURL := "https://github.com/example-org/example-project"
				runGit(t, source, "remote", "set-url", "origin", repoURL+".git")
				issue.ProgressBaseRef = "main"
				issue.Source.Version.Repository = repoURL
				reopened, reopenErr := backend.Create(t.Context(), issue)
				runGit(t, source, "remote", "set-url", "origin", remote)
				if reopenErr != nil {
					t.Fatalf("reopen Create() error = %v", reopenErr)
				}
				if got := strings.TrimSpace(runGit(t, reopened.Path, "rev-parse", "HEAD")); got != head {
					t.Fatalf("reopen Create() head = %s, want follow-up %s", got, head)
				}
				alternatesPath := filepath.Join(source, ".git", "objects", "info", "alternates")
				if err := os.MkdirAll(filepath.Dir(alternatesPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(alternatesPath, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, source, "remote", "set-url", "origin", repoURL+".git")
				_, altErr := backend.Create(t.Context(), issue)
				runGit(t, source, "remote", "set-url", "origin", remote)
				if err := os.Remove(alternatesPath); err != nil {
					t.Fatal(err)
				}
				if altErr == nil {
					t.Fatal("reopen Create() with alternates file succeeded, want error")
				}
				if got := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got != head {
					t.Fatalf("reopen Create() after alternates error head = %s, want follow-up %s", got, head)
				}
				shallowPath := filepath.Join(source, ".git", "shallow")
				if err := os.WriteFile(shallowPath, []byte(head+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, source, "remote", "set-url", "origin", repoURL+".git")
				_, shallowErr := backend.Create(t.Context(), issue)
				runGit(t, source, "remote", "set-url", "origin", remote)
				if err := os.Remove(shallowPath); err != nil {
					t.Fatal(err)
				}
				if shallowErr == nil {
					t.Fatal("reopen Create() with shallow boundary succeeded, want error")
				}
				if got := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got != head {
					t.Fatalf("reopen Create() after shallow error head = %s, want follow-up %s", got, head)
				}
			}
			prepared, err := owner.PrepareRework(t.Context(), info, issue, prepareOpts, validate)
			if test.dirtySubmodule {
				if err != nil || prepared.Status != MergePrepareStatusDirty {
					t.Fatalf("dirty submodule under ignoreSubmodules=all: %+v, %v, want dirty", prepared, err)
				}
				return
			}
			if test.conflict {
				if err != nil || prepared.Status != MergePrepareStatusConflict || len(prepared.ConflictPaths) != 1 || prepared.ConflictPaths[0] != "README.md" {
					t.Fatalf("source conflict did not retain resolution custody: %+v, %v", prepared, err)
				}
				return
			}
			if err != nil || prepared.Status != MergePrepareStatusClean {
				t.Fatalf("prepare retained source: %+v, %v", prepared, err)
			}
			recoveryState, err := owner.RecoveryState(t.Context(), info, issue)
			if err != nil {
				t.Fatalf("RecoveryState after clean preparation: %v", err)
			}
			if test.remoteOnly && test.incorporation == "" && recoveryState.UnpushedCommits != 1 {
				t.Fatalf("RecoveryState without tracking refs UnpushedCommits = %d, want 1", recoveryState.UnpushedCommits)
			}
			if test.remoteOnly && test.incorporation == "" {
				runGit(t, source, "branch", "-f", "remote-only", target)
				recoveryWithLocalBranch, err := owner.RecoveryState(t.Context(), info, issue)
				if err != nil {
					t.Fatalf("RecoveryState with local target branch and no tracking refs: %v", err)
				}
				if recoveryWithLocalBranch.UnpushedCommits != 1 {
					t.Fatalf("RecoveryState with local target branch and no tracking refs UnpushedCommits = %d, want 1", recoveryWithLocalBranch.UnpushedCommits)
				}
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

func TestFinalizeNativeWorkContinuesPausedRebaseForInProgressCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                      string
		resolved                  bool
		unrelatedTarget           bool
		spoofGraft                bool
		stalePausedHead           bool
		descendantPausedHead      bool
		tamperedRewrittenList     bool
		unrelatedRewrittenContent bool
		unrelatedPendingPick      bool
		droppedPendingPick        bool
		duplicatePendingPick      bool
		ancestorPendingPick       bool
		applySecondCommit         bool
		applyKeepUpstream         bool
		redundantFirstPick        bool
		tamperedApplyPatch        bool
		attachedPausedHead        bool
		mismatchedOrigin          bool
		redirectFetchConfig       bool
		redirectDefaultBranch     bool
		unresolvablePubRepository bool
	}{
		{name: "resolved_paused_rebase", resolved: true},
		{name: "resolved_apply_second_commit_rebase", resolved: true, applySecondCommit: true},
		{name: "resolved_apply_keep_upstream_custom_sig", resolved: true, applySecondCommit: true, applyKeepUpstream: true},
		{name: "resolved_redundant_pick_rebase", resolved: true, redundantFirstPick: true},
		{name: "tampered_apply_patch_rejected", resolved: true, applySecondCommit: true, tamperedApplyPatch: true},
		{name: "unresolved_paused_rebase", resolved: false},
		{name: "unrelated_target_resolved", resolved: true, unrelatedTarget: true},
		{name: "unrelated_target_unresolved", resolved: false, unrelatedTarget: true},
		{name: "unrelated_target_grafted_rejected", resolved: true, unrelatedTarget: true, spoofGraft: true},
		{name: "stale_paused_head_rejected", resolved: true, stalePausedHead: true},
		{name: "descendant_paused_head_rejected", resolved: true, descendantPausedHead: true},
		{name: "tampered_rewritten_list_rejected", resolved: true, tamperedRewrittenList: true},
		{name: "unrelated_rewritten_content_rejected", resolved: true, unrelatedRewrittenContent: true},
		{name: "unrelated_pending_pick_rejected", resolved: true, unrelatedPendingPick: true},
		{name: "dropped_pending_pick_rejected", resolved: true, droppedPendingPick: true},
		{name: "duplicate_pending_pick_rejected", resolved: true, duplicatePendingPick: true},
		{name: "ancestor_pending_pick_rejected", resolved: true, ancestorPendingPick: true},
		{name: "attached_paused_head_rejected", resolved: true, attachedPausedHead: true},
		{name: "mismatched_origin_rejected", resolved: true, mismatchedOrigin: true},
		{name: "redirected_fetch_config_rejected", resolved: true, mismatchedOrigin: true, redirectFetchConfig: true},
		{name: "redirected_default_branch_rejected", resolved: true, redirectDefaultBranch: true},
		{name: "unresolvable_publication_repo_rejected", resolved: true, mismatchedOrigin: true, unresolvablePubRepository: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandingFixture(t)
			if tc.applySecondCommit {
				writeFileDiffFile(t, f.info.Path, "first-pick.txt", "line1\nline2\nline3\n- \nline5\nline6\n")
				runGit(t, f.info.Path, "add", "first-pick.txt")
				runGit(t, f.info.Path, "commit", "-m", "add hyphen space line")
				writeFileDiffFile(t, f.info.Path, "first-pick.txt", "line1\nline2\nline3\nitem\nline5\nline6\n")
				runGit(t, f.info.Path, "add", "first-pick.txt")
				runGit(t, f.info.Path, "commit", "-m", "document diff --git output\n\n---\ndiff --git a/example b/example\n+example")
			}
			if tc.redundantFirstPick {
				writeFileDiffFile(t, f.info.Path, "absorbed.txt", "already upstream\n")
				runGit(t, f.info.Path, "add", "absorbed.txt")
				runGit(t, f.info.Path, "commit", "-m", "already absorbed upstream")
			}
			if tc.droppedPendingPick {
				writeFileDiffFile(t, f.info.Path, "conflict.txt", "branch\n")
				runGit(t, f.info.Path, "add", "conflict.txt")
				runGit(t, f.info.Path, "commit", "-m", "branch conflict")
				writeFileDiffFile(t, f.info.Path, "after-conflict.txt", "after conflict\n")
				runGit(t, f.info.Path, "add", "after-conflict.txt")
				runGit(t, f.info.Path, "commit", "-m", "branch after conflict")
			} else {
				writeFileDiffFile(t, f.info.Path, "conflict.txt", "branch\n")
				runGit(t, f.info.Path, "add", "conflict.txt")
				runGit(t, f.info.Path, "commit", "-m", "branch conflict")
			}

			rebaseTarget := "origin/main"
			var upstream string
			var helperSentinel string
			if tc.redirectDefaultBranch {
				fork := initBareRemote(t)
				runGit(t, f.source, "remote", "add", "fork", fork)
				runGit(t, f.source, "push", "fork", "main")
				runGit(t, f.source, "checkout", "-b", "alt")
				writeFileDiffFile(t, f.source, "conflict.txt", "alt-branch\n")
				runGit(t, f.source, "add", "conflict.txt")
				runGit(t, f.source, "commit", "-m", "shared alt branch")
				runGit(t, f.source, "push", "origin", "alt")
				runGit(t, f.source, "push", "fork", "alt")
				runGit(t, fork, "symbolic-ref", "HEAD", "refs/heads/alt")
				runGit(t, f.source, "checkout", "main")
				runGit(t, f.info.Path, "config", "remote.origin.uploadpack", "sh -c 'exec git-upload-pack \"$1\"' _ "+fork)
				rebaseTarget = "origin/alt"
			} else if tc.mismatchedOrigin {
				fork := initBareRemote(t)
				runGit(t, f.source, "remote", "add", "fork", fork)
				runGit(t, f.source, "push", "fork", "main")
				writeFileDiffFile(t, f.source, "conflict.txt", "fork-main\n")
				writeFileDiffFile(t, f.source, "fork-only.txt", "fork commit\n")
				runGit(t, f.source, "add", "conflict.txt", "fork-only.txt")
				runGit(t, f.source, "commit", "-m", "fork divergent main")
				runGit(t, f.source, "push", "fork", "main")
				if tc.redirectFetchConfig {
					runGit(t, f.info.Path, "config", "remote.origin.uploadpack", "sh -c 'exec git-upload-pack \"$1\"' _ "+fork)
				} else {
					runGit(t, f.info.Path, "remote", "set-url", "origin", fork)
				}
			} else if tc.unrelatedTarget {
				runGit(t, f.source, "checkout", "-b", "topic-unrelated")
				writeFileDiffFile(t, f.source, "conflict.txt", "topic\n")
				writeFileDiffFile(t, f.source, "topic-only.txt", "unrelated commit\n")
				runGit(t, f.source, "add", "conflict.txt", "topic-only.txt")
				runGit(t, f.source, "commit", "-m", "unrelated topic conflict")
				runGit(t, f.source, "push", "origin", "topic-unrelated")
				runGit(t, f.source, "checkout", "main")
				rebaseTarget = "origin/topic-unrelated"
			} else {
				if tc.redundantFirstPick {
					writeFileDiffFile(t, f.source, "absorbed.txt", "already upstream\n")
					runGit(t, f.source, "add", "absorbed.txt")
				}
				writeFileDiffFile(t, f.source, "conflict.txt", "upstream\n")
				runGit(t, f.source, "add", "conflict.txt")
				runGit(t, f.source, "commit", "-m", "upstream conflict")
				runGit(t, f.source, "push", "origin", "main")
				upstream = strings.TrimSpace(runGit(t, f.source, "rev-parse", "HEAD"))
			}

			runGit(t, f.info.Path, "fetch", "origin")
			rebaseArgs := []string{"rebase", "--no-update-refs", rebaseTarget}
			if tc.applySecondCommit {
				if tc.applyKeepUpstream {
					runGit(t, f.info.Path, "config", "format.signature", "custom-sig-test")
					runGit(t, f.info.Path, "config", "diff.context", "0")
				} else {
					runGit(t, f.info.Path, "config", "format.signature", "")
				}
				rebaseArgs = []string{"rebase", "--apply", rebaseTarget}
			}
			if _, err := runGitAt(t.Context(), f.info.Path, rebaseArgs...); err == nil {
				t.Fatal("expected rebase conflict")
			}
			if tc.resolved {
				if tc.applyKeepUpstream {
					writeFileDiffFile(t, f.info.Path, "conflict.txt", "upstream\n")
				} else {
					writeFileDiffFile(t, f.info.Path, "conflict.txt", "resolved\n")
				}
				runGit(t, f.info.Path, "add", "conflict.txt")
			}
			if tc.stalePausedHead {
				runGit(t, f.source, "checkout", "-b", "stale-unrelated", "HEAD~1")
				writeFileDiffFile(t, f.source, "stale.txt", "stale unrelated\n")
				runGit(t, f.source, "add", "stale.txt")
				runGit(t, f.source, "commit", "-m", "stale unrelated head")
				staleHead := strings.TrimSpace(runGit(t, f.source, "rev-parse", "HEAD"))
				runGit(t, f.source, "push", "origin", "stale-unrelated")
				runGit(t, f.source, "checkout", "main")
				runGit(t, f.info.Path, "fetch", "origin", "stale-unrelated")
				runGit(t, f.info.Path, "update-ref", "--no-deref", "HEAD", staleHead)
			}
			if tc.droppedPendingPick || tc.duplicatePendingPick || tc.ancestorPendingPick {
				todoPath, pErr := gitPathFor(t.Context(), f.info.Path, "rebase-merge/git-rebase-todo")
				if pErr != nil {
					t.Fatalf("gitPathFor(git-rebase-todo) error = %v", pErr)
				}
				var todoBody string
				if tc.duplicatePendingPick {
					stoppedPath, sErr := gitPathFor(t.Context(), f.info.Path, "rebase-merge/stopped-sha")
					if sErr != nil {
						t.Fatalf("gitPathFor(stopped-sha) error = %v", sErr)
					}
					stopped, rErr := os.ReadFile(stoppedPath)
					if rErr != nil {
						t.Fatalf("read stopped-sha: %v", rErr)
					}
					todoBody = "pick " + strings.TrimSpace(string(stopped)) + " duplicate\n"
				} else if tc.ancestorPendingPick {
					ancestor := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "origin/main~1"))
					todoBody = "pick " + ancestor + " ancestor\n"
				}
				if wErr := os.WriteFile(todoPath, []byte(todoBody), 0o600); wErr != nil {
					t.Fatalf("write git-rebase-todo: %v", wErr)
				}
			}
			if tc.tamperedApplyPatch {
				lastPath, pErr := gitPathFor(t.Context(), f.info.Path, "rebase-apply/last")
				if pErr != nil {
					t.Fatalf("gitPathFor(rebase-apply/last) error = %v", pErr)
				}
				rawLast, rErr := os.ReadFile(lastPath)
				if rErr != nil {
					t.Fatalf("read rebase-apply/last: %v", rErr)
				}
				var lastNum int
				if _, sErr := fmt.Sscanf(strings.TrimSpace(string(rawLast)), "%d", &lastNum); sErr != nil {
					t.Fatalf("parse rebase-apply/last: %v", sErr)
				}
				patchPath, pErr := gitPathFor(t.Context(), f.info.Path, fmt.Sprintf("rebase-apply/%04d", lastNum))
				if pErr != nil {
					t.Fatalf("gitPathFor(last patch) error = %v", pErr)
				}
				rawPatch, rErr := os.ReadFile(patchPath)
				if rErr != nil {
					t.Fatalf("read last patch: %v", rErr)
				}
				altered := strings.Replace(string(rawPatch), "+branch", "+  branch", 1)
				if altered == string(rawPatch) {
					t.Fatalf("expected last apply patch to contain +branch, got:\n%s", string(rawPatch))
				}
				if wErr := os.WriteFile(patchPath, []byte(altered), 0o600); wErr != nil {
					t.Fatalf("write tampered last patch: %v", wErr)
				}
			}
			if tc.descendantPausedHead || tc.tamperedRewrittenList || tc.unrelatedRewrittenContent || tc.unrelatedPendingPick {
				runGit(t, f.source, "checkout", "-b", "descendant-unrelated", "main")
				writeFileDiffFile(t, f.source, "unrelated-descendant.txt", "unrelated descendant\n")
				runGit(t, f.source, "add", "unrelated-descendant.txt")
				runGit(t, f.source, "commit", "-m", "unrelated descendant head")
				descendantHead := strings.TrimSpace(runGit(t, f.source, "rev-parse", "HEAD"))
				runGit(t, f.source, "push", "origin", "descendant-unrelated")
				runGit(t, f.source, "checkout", "main")
				runGit(t, f.info.Path, "fetch", "origin", "descendant-unrelated")
				if tc.unrelatedPendingPick {
					todoPath, pErr := gitPathFor(t.Context(), f.info.Path, "rebase-merge/git-rebase-todo")
					if pErr != nil {
						t.Fatalf("gitPathFor(git-rebase-todo) error = %v", pErr)
					}
					if wErr := os.WriteFile(todoPath, []byte("pick "+descendantHead+" unrelated\n"), 0o600); wErr != nil {
						t.Fatalf("write unrelated git-rebase-todo: %v", wErr)
					}
				} else {
					runGit(t, f.info.Path, "update-ref", "--no-deref", "HEAD", descendantHead)
					if tc.tamperedRewrittenList || tc.unrelatedRewrittenContent {
						rewrittenPath, pErr := gitPathFor(t.Context(), f.info.Path, "rebase-merge/rewritten-list")
						if pErr != nil {
							t.Fatalf("gitPathFor(rewritten-list) error = %v", pErr)
						}
						srcCommit := descendantHead
						if tc.unrelatedRewrittenContent {
							srcCommit = strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "refs/heads/"+f.info.Branch))
						}
						if wErr := os.WriteFile(rewrittenPath, []byte(srcCommit+" "+descendantHead+"\n"), 0o600); wErr != nil {
							t.Fatalf("write rewritten-list: %v", wErr)
						}
					}
				}
			}
			if tc.attachedPausedHead {
				currentHead := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD"))
				runGit(t, f.info.Path, "update-ref", "refs/heads/other-branch", currentHead)
				runGit(t, f.info.Path, "symbolic-ref", "HEAD", "refs/heads/other-branch")
			}
			if tc.spoofGraft {
				mainHead := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "origin/main"))
				topicHead := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "origin/topic-unrelated"))
				runGit(t, f.info.Path, "replace", "--graft", mainHead, topicHead)
				if _, err := runGitAt(t.Context(), f.info.Path, "merge-base", "--is-ancestor", topicHead, mainHead); err != nil {
					t.Fatalf("expected local replace graft to spoof plain merge-base: %v", err)
				}
			}

			issue := f.issue
			issue.NativeRework = false
			issue.NoChangeNeeded = false
			issue.ProgressBaseRef = "main"
			if tc.redirectDefaultBranch {
				issue.ProgressBaseRef = ""
			}
			issue.PublicationRepository = f.remote
			if tc.unresolvablePubRepository {
				issue.PublicationRepository = filepath.Join(t.TempDir(), "missing-repo.git")
			}
			issue.VerifyPhaseBranch = true
			issue.RetainPhaseCheckout = true

			headBefore := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD"))
			indexBefore := runGit(t, f.info.Path, "ls-files", "--stage", "-z")
			remoteMainBefore := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "refs/remotes/origin/main"))
			if tc.mismatchedOrigin {
				writeFileDiffFile(t, f.source, "fork-extra.txt", "unfetched fork tip\n")
				runGit(t, f.source, "add", "fork-extra.txt")
				runGit(t, f.source, "commit", "-m", "advance fork main")
				runGit(t, f.source, "push", "fork", "main")
			}
			if tc.redirectFetchConfig || tc.redirectDefaultBranch {
				helperSentinel = filepath.Join(t.TempDir(), "uploadpack-invoked")
				runGit(t, f.info.Path, "config", "remote.origin.uploadpack", "sh -c 'touch \"$1\"; exit 1' _ "+helperSentinel)
			}

			retained, err := f.backend.Create(t.Context(), issue)
			if tc.attachedPausedHead {
				if !errors.Is(err, ErrMergeResolutionInvalid) {
					t.Fatalf("attached paused HEAD Create() err = %v, want ErrMergeResolutionInvalid", err)
				}
				if err := f.backend.VerifyItemPhaseCheckout(t.Context(), f.info, issue); !errors.Is(err, ErrMergeResolutionInvalid) {
					t.Fatalf("attached paused HEAD VerifyItemPhaseCheckout() err = %v, want ErrMergeResolutionInvalid", err)
				}
				if _, err := f.backend.FinalizeNativeWork(t.Context(), f.info, issue, func(ctx context.Context) error { return ctx.Err() }); !errors.Is(err, ErrMergeResolutionInvalid) {
					t.Fatalf("attached paused HEAD FinalizeNativeWork() err = %v, want ErrMergeResolutionInvalid", err)
				}
				if ref := strings.TrimSpace(runGit(t, f.info.Path, "symbolic-ref", "HEAD")); ref != "refs/heads/other-branch" {
					t.Fatalf("attached HEAD changed to %q", ref)
				}
				return
			}
			if tc.redirectFetchConfig {
				var incomplete *SourceFinalizationIncomplete
				if !errors.Is(err, ErrMergeResolutionInvalid) || errors.As(err, &incomplete) {
					t.Fatalf("redirected fetch Create() err = %v, want non-repairable ErrMergeResolutionInvalid", err)
				}
				if err := f.backend.VerifyItemPhaseCheckout(t.Context(), f.info, issue); !errors.Is(err, ErrMergeResolutionInvalid) || errors.As(err, &incomplete) {
					t.Fatalf("redirected fetch VerifyItemPhaseCheckout() err = %v, want non-repairable ErrMergeResolutionInvalid", err)
				}
				if _, err := f.backend.FinalizeNativeWork(t.Context(), f.info, issue, func(ctx context.Context) error { return ctx.Err() }); !errors.Is(err, ErrMergeResolutionInvalid) || errors.As(err, &incomplete) {
					t.Fatalf("redirected fetch FinalizeNativeWork() err = %v, want non-repairable ErrMergeResolutionInvalid", err)
				}
				if _, statErr := os.Stat(helperSentinel); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("checkout-configured uploadpack helper was invoked: statErr=%v", statErr)
				}
				if got := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "refs/remotes/origin/main")); got != remoteMainBefore {
					t.Fatalf("redirected fetch tracking ref = %q, want %q", got, remoteMainBefore)
				}
				if got := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD")); got != headBefore {
					t.Fatalf("redirected fetch changed HEAD from %q to %q", headBefore, got)
				}
				if got := runGit(t, f.info.Path, "ls-files", "--stage", "-z"); got != indexBefore {
					t.Fatal("redirected fetch changed index")
				}
				if paused, pErr := f.backend.verifyReworkBranch(t.Context(), f.info, issue); pErr != nil || !paused {
					t.Fatalf("redirected fetch altered paused rebase: paused=%v err=%v", paused, pErr)
				}
				return
			}
			if tc.mismatchedOrigin {
				var refusal *LandRefusal
				if !errors.As(err, &refusal) || refusal.Kind != LandRefusalProtected {
					t.Fatalf("mismatched origin Create() err = %v, want LandRefusalProtected", err)
				}
				if err := f.backend.VerifyItemPhaseCheckout(t.Context(), f.info, issue); !errors.As(err, &refusal) || refusal.Kind != LandRefusalProtected {
					t.Fatalf("mismatched origin VerifyItemPhaseCheckout() err = %v, want LandRefusalProtected", err)
				}
				if _, err := f.backend.FinalizeNativeWork(t.Context(), f.info, issue, func(ctx context.Context) error { return ctx.Err() }); !errors.As(err, &refusal) || refusal.Kind != LandRefusalProtected {
					t.Fatalf("mismatched origin FinalizeNativeWork() err = %v, want LandRefusalProtected", err)
				}
				if got := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "refs/remotes/origin/main")); got != remoteMainBefore {
					t.Fatalf("mismatched origin updated fetch ref from %q to %q", remoteMainBefore, got)
				}
				if got := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD")); got != headBefore {
					t.Fatalf("mismatched origin changed HEAD from %q to %q", headBefore, got)
				}
				if got := runGit(t, f.info.Path, "ls-files", "--stage", "-z"); got != indexBefore {
					t.Fatal("mismatched origin changed index")
				}
				if paused, pErr := f.backend.verifyReworkBranch(t.Context(), f.info, issue); pErr != nil || !paused {
					t.Fatalf("mismatched origin altered paused rebase: paused=%v err=%v", paused, pErr)
				}
				return
			}
			if tc.unrelatedTarget || tc.stalePausedHead || tc.descendantPausedHead || tc.tamperedRewrittenList || tc.unrelatedRewrittenContent || tc.unrelatedPendingPick || tc.droppedPendingPick || tc.duplicatePendingPick || tc.ancestorPendingPick || tc.tamperedApplyPatch || tc.redirectDefaultBranch {
				var incomplete *SourceFinalizationIncomplete
				if !errors.Is(err, ErrMergeResolutionInvalid) || errors.As(err, &incomplete) {
					t.Fatalf("invalid paused rebase Create() err = %v, want non-repairable ErrMergeResolutionInvalid", err)
				}
				if err := f.backend.VerifyItemPhaseCheckout(t.Context(), f.info, issue); !errors.Is(err, ErrMergeResolutionInvalid) || errors.As(err, &incomplete) {
					t.Fatalf("invalid paused rebase VerifyItemPhaseCheckout() err = %v, want non-repairable ErrMergeResolutionInvalid", err)
				}
				if _, err := f.backend.FinalizeNativeWork(t.Context(), f.info, issue, func(ctx context.Context) error { return ctx.Err() }); !errors.Is(err, ErrMergeResolutionInvalid) || errors.As(err, &incomplete) {
					t.Fatalf("invalid paused rebase FinalizeNativeWork() err = %v, want non-repairable ErrMergeResolutionInvalid", err)
				}
				if got := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD")); got != headBefore {
					t.Fatalf("invalid paused rebase changed HEAD from %q to %q", headBefore, got)
				}
				if tc.redirectDefaultBranch {
					if _, statErr := os.Stat(helperSentinel); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("checkout-configured uploadpack helper was invoked: statErr=%v", statErr)
					}
				}
				if tc.unrelatedPendingPick {
					if active, aErr := rebaseInProgress(t.Context(), f.info.Path); aErr != nil || !active {
						t.Fatalf("invalid pending pick rebase should remain untouched: active=%v err=%v", active, aErr)
					}
				} else if paused, pErr := f.backend.verifyReworkBranch(t.Context(), f.info, issue); pErr != nil || !paused {
					t.Fatalf("invalid paused rebase should remain untouched: paused=%v err=%v", paused, pErr)
				}
				return
			}
			if err != nil || retained.Path != f.info.Path || retained.Created {
				t.Fatalf("same-lease retained Create() = %+v, err = %v", retained, err)
			}
			if err := f.backend.VerifyItemPhaseCheckout(t.Context(), retained, issue); err != nil {
				t.Fatalf("VerifyItemPhaseCheckout() error = %v", err)
			}

			base, err := f.backend.FinalizeNativeWork(t.Context(), retained, issue, func(ctx context.Context) error { return ctx.Err() })
			if !tc.resolved {
				var incomplete *SourceFinalizationIncomplete
				if !errors.As(err, &incomplete) || incomplete.Status != MergePrepareStatusConflict {
					t.Fatalf("unresolved paused rebase err = %v, want SourceFinalizationIncomplete(conflict)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("FinalizeNativeWork() error = %v", err)
			}
			if base != upstream {
				t.Fatalf("FinalizeNativeWork() base = %q, want %q", base, upstream)
			}
			if paused, err := f.backend.verifyReworkBranch(t.Context(), retained, issue); err != nil || paused {
				t.Fatalf("rebase still paused after finalization: paused=%v err=%v", paused, err)
			}
			wantConflict := "resolved\n"
			if tc.applyKeepUpstream {
				wantConflict = "upstream\n"
			}
			if got := readFile(t, filepath.Join(retained.Path, "conflict.txt")); got != wantConflict {
				t.Fatalf("conflict.txt = %q, want %q", got, wantConflict)
			}
			if tc.applyKeepUpstream {
				return
			}

			otherClone := filepath.Join(t.TempDir(), "other-clone")
			runGit(t, filepath.Dir(otherClone), "clone", f.remote, otherClone)
			for _, cfg := range [][]string{
				{"config", "user.name", "Other"},
				{"config", "user.email", "other@example.com"},
				{"config", "commit.gpgsign", "false"},
			} {
				runGit(t, otherClone, cfg...)
			}
			writeCheckpointFile(t, otherClone, "upstream-extra.txt", "extra\n")
			runGit(t, otherClone, "add", "upstream-extra.txt")
			runGit(t, otherClone, "commit", "-m", "upstream extra")
			extraUpstream := strings.TrimSpace(runGit(t, otherClone, "rev-parse", "HEAD"))
			runGit(t, otherClone, "push", "origin", "main")
			originMainBefore := strings.TrimSpace(runGit(t, retained.Path, "rev-parse", "refs/remotes/origin/main"))
			progressBeforeAdvance, err := f.backend.LocalProgress(t.Context(), retained, issue)
			if err != nil {
				t.Fatalf("LocalProgress() before upstream advance error = %v", err)
			}

			preparedAfterAdvance, err := f.backend.PrepareRework(t.Context(), retained, issue, MergePrepareOptions{}, func(ctx context.Context) error { return ctx.Err() })
			if err != nil {
				t.Fatalf("PrepareRework() after upstream advance error = %v", err)
			}
			if preparedAfterAdvance.BaseSHA != extraUpstream {
				t.Fatalf("PrepareRework() base after upstream advance = %q, want %q", preparedAfterAdvance.BaseSHA, extraUpstream)
			}
			if originMainAfter := strings.TrimSpace(runGit(t, retained.Path, "rev-parse", "refs/remotes/origin/main")); originMainAfter != originMainBefore {
				t.Fatalf("PrepareRework() mutated origin/main from %q to %q", originMainBefore, originMainAfter)
			}
			progressAfterAdvance, err := f.backend.LocalProgress(t.Context(), retained, issue)
			if err != nil {
				t.Fatalf("LocalProgress() after upstream advance error = %v", err)
			}
			if progressAfterAdvance.CommitFingerprint != progressBeforeAdvance.CommitFingerprint {
				t.Fatalf("LocalProgress().CommitFingerprint after unrelated upstream rebase = %q, want %q", progressAfterAdvance.CommitFingerprint, progressBeforeAdvance.CommitFingerprint)
			}
			recovery, err := f.backend.RecoveryState(t.Context(), retained, issue)
			if err != nil {
				t.Fatalf("RecoveryState() after upstream advance error = %v", err)
			}
			wantUnpushed := strings.TrimSpace(runGit(t, retained.Path, "rev-list", "--count", extraUpstream+"..HEAD"))
			if fmt.Sprintf("%d", recovery.UnpushedCommits) != wantUnpushed {
				t.Fatalf("RecoveryState().UnpushedCommits = %d, want %s (published upstream commit %s must not be counted as unpushed)", recovery.UnpushedCommits, wantUnpushed, extraUpstream)
			}

			runGit(t, otherClone, "checkout", "-b", "remote-only")
			writeCheckpointFile(t, otherClone, "remote-only.txt", "remote-only\n")
			runGit(t, otherClone, "add", "remote-only.txt")
			runGit(t, otherClone, "commit", "-m", "remote-only base")
			remoteOnlyBase := strings.TrimSpace(runGit(t, otherClone, "rev-parse", "HEAD"))
			runGit(t, otherClone, "push", "origin", "remote-only")

			remoteOnlyIssue := issue
			remoteOnlyIssue.ProgressBaseRef = "remote-only"
			preparedRemoteOnly, err := f.backend.PrepareRework(t.Context(), retained, remoteOnlyIssue, MergePrepareOptions{TargetBranch: "remote-only"}, func(ctx context.Context) error { return ctx.Err() })
			if err != nil {
				t.Fatalf("PrepareRework(remote-only) error = %v", err)
			}
			if preparedRemoteOnly.BaseSHA != remoteOnlyBase {
				t.Fatalf("PrepareRework(remote-only) base = %q, want %q", preparedRemoteOnly.BaseSHA, remoteOnlyBase)
			}
			remoteOnlyRecovery, err := f.backend.RecoveryState(t.Context(), retained, remoteOnlyIssue)
			if err != nil {
				t.Fatalf("RecoveryState(remote-only) error = %v", err)
			}
			wantRemoteOnlyUnpushed := strings.TrimSpace(runGit(t, retained.Path, "rev-list", "--count", remoteOnlyBase+"..HEAD"))
			if fmt.Sprintf("%d", remoteOnlyRecovery.UnpushedCommits) != wantRemoteOnlyUnpushed {
				t.Fatalf("RecoveryState(remote-only).UnpushedCommits = %d, want %s", remoteOnlyRecovery.UnpushedCommits, wantRemoteOnlyUnpushed)
			}
			if _, err := f.backend.LocalProgress(t.Context(), retained, remoteOnlyIssue); err != nil {
				t.Fatalf("LocalProgress(remote-only) error = %v", err)
			}
			staleDefaultTip := strings.TrimSpace(runGit(t, retained.Path, "rev-parse", "HEAD"))
			runGit(t, retained.Path, "update-ref", "refs/remotes/origin/main", staleDefaultTip)
			remoteOnlyStaleDefaultRecovery, err := f.backend.RecoveryState(t.Context(), retained, remoteOnlyIssue)
			if err != nil {
				t.Fatalf("RecoveryState(remote-only with stale origin/main) error = %v", err)
			}
			if fmt.Sprintf("%d", remoteOnlyStaleDefaultRecovery.UnpushedCommits) != wantRemoteOnlyUnpushed {
				t.Fatalf("RecoveryState(remote-only with stale origin/main).UnpushedCommits = %d, want %s", remoteOnlyStaleDefaultRecovery.UnpushedCommits, wantRemoteOnlyUnpushed)
			}
			runGit(t, retained.Path, "update-ref", "refs/remotes/origin/main", originMainBefore)
			runGit(t, retained.Path, "update-ref", "--no-deref", "refs/remotes/origin/HEAD", staleDefaultTip)
			staleDirectHeadRecovery, err := f.backend.RecoveryState(t.Context(), retained, remoteOnlyIssue)
			if err != nil {
				t.Fatalf("RecoveryState(remote-only with stale direct origin/HEAD) error = %v", err)
			}
			if fmt.Sprintf("%d", staleDirectHeadRecovery.UnpushedCommits) != wantRemoteOnlyUnpushed {
				t.Fatalf("RecoveryState(remote-only with stale direct origin/HEAD).UnpushedCommits = %d, want %s", staleDirectHeadRecovery.UnpushedCommits, wantRemoteOnlyUnpushed)
			}
			runGit(t, retained.Path, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

			runGit(t, retained.Path, "reset", "--soft", remoteOnlyBase)
			runGit(t, retained.Path, "commit", "-m", "single branch commit")
			runGit(t, otherClone, "checkout", "-b", "remote-only-conflict")
			writeCheckpointFile(t, otherClone, "conflict.txt", "remote conflict\n")
			runGit(t, otherClone, "add", "conflict.txt")
			runGit(t, otherClone, "commit", "-m", "remote-only conflict base")
			remoteOnlyConflictBase := strings.TrimSpace(runGit(t, otherClone, "rev-parse", "HEAD"))
			runGit(t, otherClone, "push", "origin", "remote-only-conflict")

			conflictIssue := issue
			conflictIssue.Checkpoint = nil
			conflictIssue.ProgressBaseRef = "remote-only-conflict"
			preConflictHead := strings.TrimSpace(runGit(t, retained.Path, "rev-parse", "refs/heads/"+retained.Branch))
			preConflictDigest := workspaceRecoveryFingerprint(preConflictHead, "")
			preparedConflict, err := f.backend.PrepareRework(t.Context(), retained, conflictIssue, MergePrepareOptions{TargetBranch: "remote-only-conflict"}, func(ctx context.Context) error { return ctx.Err() })
			if err != nil {
				t.Fatalf("PrepareRework(remote-only-conflict) error = %v", err)
			}
			if preparedConflict.Status != MergePrepareStatusConflict || preparedConflict.BaseSHA != remoteOnlyConflictBase {
				t.Fatalf("PrepareRework(remote-only-conflict) = %+v, want conflict on %q", preparedConflict, remoteOnlyConflictBase)
			}
			observedConflict, err := f.backend.RecoveryState(t.Context(), retained, conflictIssue)
			if err != nil {
				t.Fatalf("RecoveryState(remote-only-conflict paused) error = %v", err)
			}
			verifiedBeforeRewind, err := f.backend.VerifyReworkRecovery(t.Context(), retained, conflictIssue, preConflictHead, preConflictDigest, observedConflict)
			if err != nil {
				t.Fatalf("VerifyReworkRecovery() before target rewind error = %v", err)
			}
			if !verifiedBeforeRewind {
				t.Fatal("VerifyReworkRecovery() rejected valid paused recovery before target rewind")
			}

			tx, err := checkpoint.CaptureTransaction(t.Context(), retained.Path, retained.Branch, t.TempDir())
			if err != nil {
				t.Fatalf("CaptureTransaction() error = %v", err)
			}
			txTree := strings.TrimSpace(runGit(t, retained.Path, "rev-parse", tx.Head+"^{tree}"))
			txConflictIssue := conflictIssue
			txConflictIssue.NativeRework = true
			txConflictIssue.Checkpoint = &CheckpointRef{
				Ref:         CheckpointRefName(txConflictIssue),
				CommitSHA:   tx.Head,
				TreeSHA:     txTree,
				Transaction: tx,
			}
			verifiedTxBeforeRewind, err := f.backend.VerifyReworkRecovery(t.Context(), retained, txConflictIssue, "", "", observedConflict)
			if err != nil {
				t.Fatalf("VerifyReworkRecovery(transaction) before target rewind error = %v", err)
			}
			if !verifiedTxBeforeRewind {
				t.Fatal("VerifyReworkRecovery(transaction) rejected valid paused recovery before target rewind")
			}

			runGit(t, otherClone, "push", "--force", "origin", remoteOnlyBase+":refs/heads/remote-only-conflict")
			verifiedAfterRewind, err := f.backend.VerifyReworkRecovery(t.Context(), retained, conflictIssue, preConflictHead, preConflictDigest, observedConflict)
			if err != nil {
				t.Fatalf("VerifyReworkRecovery() after target rewind error = %v", err)
			}
			if verifiedAfterRewind {
				t.Fatal("VerifyReworkRecovery() verified paused recovery after remote target rewound past onto")
			}
			verifiedTxAfterRewind, err := f.backend.VerifyReworkRecovery(t.Context(), retained, txConflictIssue, "", "", observedConflict)
			if err != nil {
				t.Fatalf("VerifyReworkRecovery(transaction) after target rewind error = %v", err)
			}
			if verifiedTxAfterRewind {
				t.Fatal("VerifyReworkRecovery(transaction) verified paused recovery after remote target rewound past onto")
			}

			runGit(t, retained.Path, "rebase", "--abort")
			runGit(t, otherClone, "checkout", "-b", "remote-only-worker-paused", remoteOnlyBase)
			writeCheckpointFile(t, otherClone, "conflict.txt", "worker paused remote conflict\n")
			runGit(t, otherClone, "add", "conflict.txt")
			runGit(t, otherClone, "commit", "-m", "remote-only worker paused base")
			workerPausedBase := strings.TrimSpace(runGit(t, otherClone, "rev-parse", "HEAD"))
			runGit(t, otherClone, "push", "origin", "remote-only-worker-paused")

			runGit(t, retained.Path, "fetch", "--no-tags", "--refmap=", "origin", "remote-only-worker-paused")
			if out, err := runGitAt(t.Context(), retained.Path, "rebase", "FETCH_HEAD"); err == nil {
				t.Fatalf("expected worker-initiated rebase conflict on FETCH_HEAD, got output=%q", out)
			}
			workerPausedIssue := issue
			workerPausedIssue.ProgressBaseRef = "remote-only-worker-paused"
			preparedWorkerPaused, err := f.backend.PrepareRework(t.Context(), retained, workerPausedIssue, MergePrepareOptions{TargetBranch: "remote-only-worker-paused"}, func(ctx context.Context) error { return ctx.Err() })
			if err != nil {
				t.Fatalf("PrepareRework(remote-only-worker-paused) error = %v", err)
			}
			if preparedWorkerPaused.Status != MergePrepareStatusConflict || preparedWorkerPaused.BaseSHA != workerPausedBase {
				t.Fatalf("PrepareRework(remote-only-worker-paused) = %+v, want conflict on %q", preparedWorkerPaused, workerPausedBase)
			}
			if _, err := f.backend.RecoveryState(t.Context(), retained, workerPausedIssue); err != nil {
				t.Fatalf("RecoveryState(remote-only-worker-paused) error = %v", err)
			}

			runGit(t, retained.Path, "rebase", "--abort")
			runGit(t, retained.Path, "fetch", "origin", "+refs/heads/main:refs/remotes/origin/main")
			mainBeforeRewind := strings.TrimSpace(runGit(t, retained.Path, "rev-parse", "refs/remotes/origin/main"))
			mainParent := strings.TrimSpace(runGit(t, otherClone, "rev-parse", mainBeforeRewind+"^1"))
			runGit(t, retained.Path, "reset", "--hard", mainBeforeRewind)
			writeCheckpointFile(t, retained.Path, "after-rewind-1.txt", "1\n")
			runGit(t, retained.Path, "add", "after-rewind-1.txt")
			runGit(t, retained.Path, "commit", "-m", "after rewind 1")
			writeCheckpointFile(t, retained.Path, "after-rewind-2.txt", "2\n")
			runGit(t, retained.Path, "add", "after-rewind-2.txt")
			runGit(t, retained.Path, "commit", "-m", "after rewind 2")
			if _, err := f.backend.PrepareRework(t.Context(), retained, issue, MergePrepareOptions{}, func(ctx context.Context) error { return ctx.Err() }); err != nil {
				t.Fatalf("PrepareRework() before rewind error = %v", err)
			}

			runGit(t, otherClone, "push", "--force", "origin", mainParent+":refs/heads/main")
			if _, err := f.backend.PrepareRework(t.Context(), retained, issue, MergePrepareOptions{}, func(ctx context.Context) error { return ctx.Err() }); err != nil {
				t.Fatalf("PrepareRework() after remote rewind error = %v", err)
			}
			rewindRecovery, err := f.backend.RecoveryState(t.Context(), retained, issue)
			if err != nil {
				t.Fatalf("RecoveryState() with stale origin/main after rewind error = %v", err)
			}
			wantRewindUnpushed := strings.TrimSpace(runGit(t, retained.Path, "rev-list", "--count", mainParent+"..HEAD"))
			if fmt.Sprintf("%d", rewindRecovery.UnpushedCommits) != wantRewindUnpushed {
				t.Fatalf("RecoveryState().UnpushedCommits with stale origin/main = %d, want %s", rewindRecovery.UnpushedCommits, wantRewindUnpushed)
			}
			shaBaseIssue := issue
			shaBaseIssue.ProgressBaseRef = ""
			shaBaseIssue.BaseRef = mainParent
			shaBaseRecovery, err := f.backend.RecoveryState(t.Context(), retained, shaBaseIssue)
			if err != nil {
				t.Fatalf("RecoveryState(SHA BaseRef) with stale origin/main error = %v", err)
			}
			if fmt.Sprintf("%d", shaBaseRecovery.UnpushedCommits) != wantRewindUnpushed {
				t.Fatalf("RecoveryState(SHA BaseRef).UnpushedCommits with stale origin/main = %d, want %s", shaBaseRecovery.UnpushedCommits, wantRewindUnpushed)
			}

			runGit(t, retained.Path, "config", "--add", "remote.origin.fetch", "+refs/heads/main:refs/remotes/origin/integration")
			runGit(t, retained.Path, "update-ref", "refs/remotes/origin/integration", mainBeforeRewind)
			multiMapRecovery, err := f.backend.RecoveryState(t.Context(), retained, issue)
			if err != nil {
				t.Fatalf("RecoveryState(second fetch mapping) error = %v", err)
			}
			if fmt.Sprintf("%d", multiMapRecovery.UnpushedCommits) != wantRewindUnpushed {
				t.Fatalf("RecoveryState(second fetch mapping).UnpushedCommits = %d, want %s", multiMapRecovery.UnpushedCommits, wantRewindUnpushed)
			}
			runGit(t, retained.Path, "update-ref", "-d", "refs/remotes/origin/integration")
			runGit(t, retained.Path, "config", "--unset", "remote.origin.fetch", `refs/remotes/origin/integration`)

			runGit(t, retained.Path, "config", "--add", "remote.origin.fetch", "+HEAD:refs/remotes/origin/current")
			runGit(t, retained.Path, "update-ref", "refs/remotes/origin/current", mainBeforeRewind)
			headMapRecovery, err := f.backend.RecoveryState(t.Context(), retained, issue)
			if err != nil {
				t.Fatalf("RecoveryState(HEAD fetch mapping) error = %v", err)
			}
			if fmt.Sprintf("%d", headMapRecovery.UnpushedCommits) != wantRewindUnpushed {
				t.Fatalf("RecoveryState(HEAD fetch mapping).UnpushedCommits = %d, want %s", headMapRecovery.UnpushedCommits, wantRewindUnpushed)
			}
			runGit(t, f.remote, "symbolic-ref", "HEAD", "refs/heads/missing")
			unbornHeadMapRecovery, err := f.backend.RecoveryState(t.Context(), retained, issue)
			if err != nil {
				t.Fatalf("RecoveryState(unborn remote HEAD fetch mapping) error = %v", err)
			}
			if fmt.Sprintf("%d", unbornHeadMapRecovery.UnpushedCommits) != wantRewindUnpushed {
				t.Fatalf("RecoveryState(unborn remote HEAD fetch mapping).UnpushedCommits = %d, want %s", unbornHeadMapRecovery.UnpushedCommits, wantRewindUnpushed)
			}
			runGit(t, otherClone, "push", "origin", mainParent+":refs/heads/other-default")
			runGit(t, f.remote, "symbolic-ref", "HEAD", "refs/heads/other-default")
			switchedHeadMapRecovery, err := f.backend.RecoveryState(t.Context(), retained, issue)
			if err != nil {
				t.Fatalf("RecoveryState(switched remote HEAD fetch mapping) error = %v", err)
			}
			if fmt.Sprintf("%d", switchedHeadMapRecovery.UnpushedCommits) != wantRewindUnpushed {
				t.Fatalf("RecoveryState(switched remote HEAD fetch mapping).UnpushedCommits = %d, want %s", switchedHeadMapRecovery.UnpushedCommits, wantRewindUnpushed)
			}
			firstBranchCommit := strings.TrimSpace(runGit(t, retained.Path, "rev-parse", "HEAD~1"))
			runGit(t, retained.Path, "push", f.remote, firstBranchCommit+":refs/heads/switched-default")
			runGit(t, f.remote, "symbolic-ref", "HEAD", "refs/heads/switched-default")
			advancedDefaultRecovery, err := f.backend.RecoveryState(t.Context(), retained, issue)
			if err != nil {
				t.Fatalf("RecoveryState(advanced switched remote HEAD) error = %v", err)
			}
			if advancedDefaultRecovery.UnpushedCommits != 1 {
				t.Fatalf("RecoveryState(advanced switched remote HEAD).UnpushedCommits = %d, want 1", advancedDefaultRecovery.UnpushedCommits)
			}
			runGit(t, retained.Path, "update-ref", "-d", "refs/remotes/origin/current")
			absentAliasRecovery, err := f.backend.RecoveryState(t.Context(), retained, issue)
			if err != nil {
				t.Fatalf("RecoveryState(absent HEAD alias ref) error = %v", err)
			}
			if absentAliasRecovery.UnpushedCommits != 1 {
				t.Fatalf("RecoveryState(absent HEAD alias ref).UnpushedCommits = %d, want 1", absentAliasRecovery.UnpushedCommits)
			}
			runGit(t, retained.Path, "config", "--unset", "remote.origin.fetch", `refs/remotes/origin/current`)
			runGit(t, f.remote, "update-ref", "--no-deref", "HEAD", firstBranchCommit)
			detachedHeadRecovery, err := f.backend.RecoveryState(t.Context(), retained, issue)
			if err != nil {
				t.Fatalf("RecoveryState(detached remote HEAD) error = %v", err)
			}
			if detachedHeadRecovery.UnpushedCommits != 1 {
				t.Fatalf("RecoveryState(detached remote HEAD).UnpushedCommits = %d, want 1", detachedHeadRecovery.UnpushedCommits)
			}
			runGit(t, otherClone, "fetch", "origin", firstBranchCommit)
			runGit(t, otherClone, "checkout", "--detach", firstBranchCommit)
			writeCheckpointFile(t, otherClone, "detached-advance.txt", "advanced detached HEAD\n")
			runGit(t, otherClone, "add", "detached-advance.txt")
			runGit(t, otherClone, "commit", "-m", "advance detached HEAD")
			advancedDetachedHead := strings.TrimSpace(runGit(t, otherClone, "rev-parse", "HEAD"))
			runGit(t, otherClone, "push", "origin", advancedDetachedHead+":refs/heads/detached-holder")
			runGit(t, f.remote, "update-ref", "--no-deref", "HEAD", advancedDetachedHead)
			runGit(t, f.remote, "update-ref", "-d", "refs/heads/detached-holder")
			advancedDetachedRecovery, err := f.backend.RecoveryState(t.Context(), retained, issue)
			if err != nil {
				t.Fatalf("RecoveryState(advanced detached remote HEAD) error = %v", err)
			}
			if advancedDetachedRecovery.UnpushedCommits != 1 {
				t.Fatalf("RecoveryState(advanced detached remote HEAD).UnpushedCommits = %d, want 1", advancedDetachedRecovery.UnpushedCommits)
			}
			writeCheckpointFile(t, otherClone, "detached-corrupt.txt", "corrupt detached HEAD\n")
			runGit(t, otherClone, "add", "detached-corrupt.txt")
			runGit(t, otherClone, "commit", "-m", "corrupt detached HEAD")
			corruptDetachedHead := strings.TrimSpace(runGit(t, otherClone, "rev-parse", "HEAD"))
			corruptTree := strings.TrimSpace(runGit(t, otherClone, "rev-parse", corruptDetachedHead+"^{tree}"))
			runGit(t, otherClone, "push", "origin", corruptDetachedHead+":refs/heads/detached-corrupt")
			runGit(t, f.remote, "update-ref", "--no-deref", "HEAD", corruptDetachedHead)
			runGit(t, f.remote, "update-ref", "-d", "refs/heads/detached-corrupt")
			corruptTreePath := filepath.Join(f.remote, "objects", corruptTree[:2], corruptTree[2:])
			if err := os.Chmod(corruptTreePath, 0o600); err != nil {
				t.Fatalf("chmod corrupt tree object: %v", err)
			}
			if err := os.Remove(corruptTreePath); err != nil {
				t.Fatalf("remove corrupt tree object: %v", err)
			}
			if _, err := f.backend.RecoveryState(t.Context(), retained, issue); err == nil {
				t.Fatal("RecoveryState(failed detached remote HEAD fetch) succeeded, want error")
			}
			runGit(t, f.remote, "symbolic-ref", "HEAD", "refs/heads/main")

			runGit(t, otherClone, "push", "--force", "origin", mainBeforeRewind+":refs/heads/main")
			if _, err := f.backend.PrepareRework(t.Context(), retained, issue, MergePrepareOptions{}, func(ctx context.Context) error { return ctx.Err() }); err != nil {
				t.Fatalf("PrepareRework() recording B error = %v", err)
			}
			runGit(t, otherClone, "push", "--force", "origin", mainParent+":refs/heads/main")
			runGit(t, retained.Path, "fetch", "origin", "+refs/heads/main:refs/remotes/origin/main")
			gotBaseAfterRefresh, err := f.backend.localProgressBase(t.Context(), retained.Path, issue)
			if err != nil {
				t.Fatalf("localProgressBase() after origin/main refresh error = %v", err)
			}
			if gotBaseAfterRefresh != mainParent {
				t.Fatalf("localProgressBase() after origin/main refresh = %q, want refreshed %q (not stale %q)", gotBaseAfterRefresh, mainParent, mainBeforeRewind)
			}
			if f.backend.headIntegrated(t.Context(), retained.Path, issue, mainBeforeRewind) {
				t.Fatalf("headIntegrated(%q) = true after origin/main rewound to %q", mainBeforeRewind, mainParent)
			}

			runGit(t, otherClone, "push", "--force", "origin", mainBeforeRewind+":refs/heads/main")
			runGit(t, retained.Path, "fetch", "origin", "+refs/heads/main:refs/remotes/origin/main")
			runGit(t, otherClone, "push", "--force", "origin", mainParent+":refs/heads/main")
			if _, err := f.backend.PrepareRework(t.Context(), retained, issue, MergePrepareOptions{}, func(ctx context.Context) error { return ctx.Err() }); err != nil {
				t.Fatalf("PrepareRework() recording rewound A error = %v", err)
			}
			runGit(t, retained.Path, "fetch", "origin", "+refs/heads/main:refs/remotes/origin/main")
			runGit(t, otherClone, "push", "--force", "origin", mainBeforeRewind+":refs/heads/main")
			runGit(t, retained.Path, "fetch", "origin", "+refs/heads/main:refs/remotes/origin/main")
			gotBaseAfterRestore, err := f.backend.localProgressBase(t.Context(), retained.Path, issue)
			if err != nil {
				t.Fatalf("localProgressBase() after rewind-and-restore error = %v", err)
			}
			if gotBaseAfterRestore != mainBeforeRewind {
				t.Fatalf("localProgressBase() after rewind-and-restore = %q, want restored %q (not stale %q)", gotBaseAfterRestore, mainBeforeRewind, mainParent)
			}
			if !f.backend.headIntegrated(t.Context(), retained.Path, issue, mainBeforeRewind) {
				t.Fatalf("headIntegrated(%q) = false after origin/main restored to %q", mainBeforeRewind, mainBeforeRewind)
			}

			defaultBranchIssue := issue
			defaultBranchIssue.ProgressBaseRef = ""
			defaultBranchIssue.BaseRef = ""
			runGit(t, retained.Path, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
			runGit(t, otherClone, "push", "--force", "origin", mainParent+":refs/heads/main")
			if _, err := f.backend.PrepareRework(t.Context(), retained, defaultBranchIssue, MergePrepareOptions{}, func(ctx context.Context) error { return ctx.Err() }); err != nil {
				t.Fatalf("PrepareRework(default branch) recording rewound A error = %v", err)
			}
			runGit(t, retained.Path, "fetch", "origin", "+refs/heads/main:refs/remotes/origin/main")
			runGit(t, otherClone, "push", "--force", "origin", mainBeforeRewind+":refs/heads/main")
			runGit(t, retained.Path, "fetch", "origin", "+refs/heads/main:refs/remotes/origin/main")
			gotDefaultAfterRestore, err := f.backend.localProgressBase(t.Context(), retained.Path, defaultBranchIssue)
			if err != nil {
				t.Fatalf("localProgressBase(default branch) after rewind-and-restore error = %v", err)
			}
			if gotDefaultAfterRestore != mainBeforeRewind {
				t.Fatalf("localProgressBase(default branch) after rewind-and-restore = %q, want restored %q (not stale %q)", gotDefaultAfterRestore, mainBeforeRewind, mainParent)
			}
			if !f.backend.headIntegrated(t.Context(), retained.Path, defaultBranchIssue, mainBeforeRewind) {
				t.Fatalf("headIntegrated(default branch, %q) = false after origin/main restored", mainBeforeRewind)
			}

			runGit(t, retained.Path, "config", "core.logAllRefUpdates", "false")
			runGit(t, retained.Path, "reflog", "expire", "--expire=now", "--all")
			runGit(t, otherClone, "push", "--force", "origin", mainParent+":refs/heads/main")
			if _, err := f.backend.PrepareRework(t.Context(), retained, defaultBranchIssue, MergePrepareOptions{}, func(ctx context.Context) error { return ctx.Err() }); err != nil {
				t.Fatalf("PrepareRework(no reflogs) recording rewound A error = %v", err)
			}
			runGit(t, retained.Path, "fetch", "origin", "+refs/heads/main:refs/remotes/origin/main")
			runGit(t, otherClone, "push", "--force", "origin", mainBeforeRewind+":refs/heads/main")
			runGit(t, retained.Path, "fetch", "origin", "+refs/heads/main:refs/remotes/origin/main")
			gotNoReflogAfterRestore, err := f.backend.localProgressBase(t.Context(), retained.Path, defaultBranchIssue)
			if err != nil {
				t.Fatalf("localProgressBase(no reflogs) after rewind-and-restore error = %v", err)
			}
			if gotNoReflogAfterRestore != mainBeforeRewind {
				t.Fatalf("localProgressBase(no reflogs) after rewind-and-restore = %q, want restored %q (not stale %q)", gotNoReflogAfterRestore, mainBeforeRewind, mainParent)
			}
			if !f.backend.headIntegrated(t.Context(), retained.Path, defaultBranchIssue, mainBeforeRewind) {
				t.Fatalf("headIntegrated(no reflogs, %q) = false after origin/main restored", mainBeforeRewind)
			}

			runGit(t, otherClone, "push", "--force", "origin", mainParent+":refs/heads/main")
			if _, err := f.backend.PrepareRework(t.Context(), retained, defaultBranchIssue, MergePrepareOptions{}, func(ctx context.Context) error { return ctx.Err() }); err != nil {
				t.Fatalf("PrepareRework(no intermediate fetch) recording rewound A error = %v", err)
			}
			unreachableIssue := defaultBranchIssue
			unreachableIssue.PublicationRepository = filepath.Join(t.TempDir(), "missing-remote.git")
			if _, err := f.backend.localProgressBase(t.Context(), retained.Path, unreachableIssue); err == nil {
				t.Fatal("localProgressBase() succeeded when live remote verification failed with diverged recorded base")
			}
			if f.backend.headIntegrated(t.Context(), retained.Path, unreachableIssue, mainBeforeRewind) {
				t.Fatalf("headIntegrated(%q) = true when live remote verification failed", mainBeforeRewind)
			}
			runGit(t, otherClone, "push", "--force", "origin", mainBeforeRewind+":refs/heads/main")
			runGit(t, retained.Path, "fetch", "origin", "+refs/heads/main:refs/remotes/origin/main")
			gotNoIntermFetch, err := f.backend.localProgressBase(t.Context(), retained.Path, defaultBranchIssue)
			if err != nil {
				t.Fatalf("localProgressBase(no intermediate fetch) error = %v", err)
			}
			if gotNoIntermFetch != mainBeforeRewind {
				t.Fatalf("localProgressBase(no intermediate fetch) = %q, want restored %q (not stale %q)", gotNoIntermFetch, mainBeforeRewind, mainParent)
			}
			if !f.backend.headIntegrated(t.Context(), retained.Path, defaultBranchIssue, mainBeforeRewind) {
				t.Fatalf("headIntegrated(no intermediate fetch, %q) = false after restore", mainBeforeRewind)
			}

			runGit(t, otherClone, "push", "--force", "origin", mainParent+":refs/heads/main")
			if _, err := f.backend.PrepareRework(t.Context(), retained, defaultBranchIssue, MergePrepareOptions{}, func(ctx context.Context) error { return ctx.Err() }); err != nil {
				t.Fatalf("PrepareRework(sibling setup) error = %v", err)
			}
			runGit(t, otherClone, "checkout", "-B", "sibling-main", mainParent)
			writeCheckpointFile(t, otherClone, "sibling-tip.txt", "sibling\n")
			runGit(t, otherClone, "add", "sibling-tip.txt")
			runGit(t, otherClone, "commit", "-m", "sibling tip C")
			siblingTip := strings.TrimSpace(runGit(t, otherClone, "rev-parse", "HEAD"))
			runGit(t, otherClone, "push", "--force", "origin", siblingTip+":refs/heads/main")

			gotSiblingBase, err := f.backend.localProgressBase(t.Context(), retained.Path, defaultBranchIssue)
			if err != nil {
				t.Fatalf("localProgressBase(sibling live tip) error = %v", err)
			}
			if gotSiblingBase != siblingTip {
				t.Fatalf("localProgressBase(sibling live tip) = %q, want live %q (not stale %q)", gotSiblingBase, siblingTip, mainBeforeRewind)
			}
			if f.backend.headIntegrated(t.Context(), retained.Path, defaultBranchIssue, mainBeforeRewind) {
				t.Fatalf("headIntegrated(%q) = true when live tip is sibling %q", mainBeforeRewind, siblingTip)
			}
			siblingRecovery, err := f.backend.RecoveryState(t.Context(), retained, defaultBranchIssue)
			if err != nil {
				t.Fatalf("RecoveryState(sibling live tip) error = %v", err)
			}
			wantSiblingUnpushed := strings.TrimSpace(runGit(t, retained.Path, "rev-list", "--count", siblingTip+"..HEAD"))
			if fmt.Sprintf("%d", siblingRecovery.UnpushedCommits) != wantSiblingUnpushed {
				t.Fatalf("RecoveryState(sibling live tip).UnpushedCommits = %d, want %s", siblingRecovery.UnpushedCommits, wantSiblingUnpushed)
			}

			runGit(t, otherClone, "push", "--force", "origin", mainBeforeRewind+":refs/heads/main")
			if _, err := f.backend.PrepareRework(t.Context(), retained, defaultBranchIssue, MergePrepareOptions{}, func(ctx context.Context) error { return ctx.Err() }); err != nil {
				t.Fatalf("PrepareRework(equal-cache setup) error = %v", err)
			}
			runGit(t, otherClone, "push", "--force", "origin", siblingTip+":refs/heads/main")
			gotEqualCacheBase, err := f.backend.localProgressBase(t.Context(), retained.Path, defaultBranchIssue)
			if err != nil {
				t.Fatalf("localProgressBase(equal-cache sibling live tip) error = %v", err)
			}
			if gotEqualCacheBase != siblingTip {
				t.Fatalf("localProgressBase(equal-cache sibling live tip) = %q, want live %q (not stale %q)", gotEqualCacheBase, siblingTip, mainBeforeRewind)
			}
			if f.backend.headIntegrated(t.Context(), retained.Path, defaultBranchIssue, mainBeforeRewind) {
				t.Fatalf("headIntegrated(equal-cache, %q) = true when live tip is sibling %q", mainBeforeRewind, siblingTip)
			}
		})
	}
}

func TestResolveReworkTargetBasePreservesSSHTransport(t *testing.T) {
	for _, tc := range []struct {
		name               string
		originURL          string
		globalConfig       string
		emptyPubRepository bool
		wantProtectedErr   bool
		wantSSHSubstr      string
		forbidSSHSubstr    string
	}{
		{
			name:          "scp_ssh",
			originURL:     "git@github.com:example-org/example-repo.git",
			wantSSHSubstr: "github.com",
		},
		{
			name:          "git_plus_ssh",
			originURL:     "git+ssh://git@github.com/example-org/example-repo.git",
			wantSSHSubstr: "github.com",
		},
		{
			name:          "ssh_plus_git",
			originURL:     "ssh+git://git@github.com/example-org/example-repo.git",
			wantSSHSubstr: "github.com",
		},
		{
			name:            "global_insteadOf_rewrite",
			originURL:       "git@alias.example:wrong-org/wrong-repo.git",
			globalConfig:    "[url \"git@github.com:example-org/example-repo.git\"]\n\tinsteadOf = git@alias.example:wrong-org/wrong-repo.git\n",
			wantSSHSubstr:   "github.com",
			forbidSSHSubstr: "alias.example",
		},
		{
			name:             "overlapping_insteadOf_redirect_rejected",
			originURL:        "git@alias.example:wrong-org/wrong-repo.git",
			globalConfig:     "[url \"git@github.com:example-org/example-repo.git\"]\n\tinsteadOf = git@alias.example:wrong-org/wrong-repo.git\n[url \"git@mirror.invalid:other-org/other-repo.git\"]\n\tinsteadOf = git@github.com:example-org/example-repo.git\n",
			wantProtectedErr: true,
		},
		{
			name:               "empty_publication_repo_global_insteadOf",
			originURL:          "git@alias.example:wrong-org/wrong-repo.git",
			globalConfig:       "[url \"git@github.com:example-org/example-repo.git\"]\n\tinsteadOf = git@alias.example:wrong-org/wrong-repo.git\n",
			emptyPubRepository: true,
			wantSSHSubstr:      "github.com",
			forbidSSHSubstr:    "alias.example",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLandingFixture(t)
			upstream := strings.TrimSpace(runGit(t, f.remote, "rev-parse", "refs/heads/main"))

			fakeBin := t.TempDir()
			sshLog := filepath.Join(fakeBin, "ssh-args.log")
			sshScript := filepath.Join(fakeBin, "ssh")
			script := "#!/bin/sh\n" +
				"printf '%s\\n' \"$*\" >> " + sshLog + "\n" +
				"exec git-upload-pack " + f.remote + "\n"
			if err := os.WriteFile(sshScript, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if tc.globalConfig != "" {
				globalCfg := filepath.Join(t.TempDir(), "gitconfig")
				if err := os.WriteFile(globalCfg, []byte(tc.globalConfig), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
			}

			runGit(t, f.info.Path, "remote", "set-url", "origin", tc.originURL)
			issue := f.issue
			if tc.emptyPubRepository {
				issue.PublicationRepository = ""
			} else {
				issue.PublicationRepository = "https://github.com/example-org/example-repo.git"
			}

			got, _, err := f.backend.resolveReworkTargetBase(t.Context(), f.info.Path, issue, MergePrepareOptions{})
			if tc.wantProtectedErr {
				var refusal *LandRefusal
				if !errors.As(err, &refusal) || refusal.Kind != LandRefusalProtected {
					t.Fatalf("overlapping insteadOf err = %v, want LandRefusalProtected", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveReworkTargetBase() error = %v", err)
			}
			if got != upstream {
				t.Fatalf("resolveReworkTargetBase() = %q, want %q", got, upstream)
			}
			logged := readFile(t, sshLog)
			if !strings.Contains(logged, tc.wantSSHSubstr) {
				t.Fatalf("SSH args %q missing %q", logged, tc.wantSSHSubstr)
			}
			if tc.forbidSSHSubstr != "" && strings.Contains(logged, tc.forbidSSHSubstr) {
				t.Fatalf("SSH args %q leaked unrewritten remote %q", logged, tc.forbidSSHSubstr)
			}
		})
	}
}

func TestResolveReworkTargetBaseAuthenticatedHTTPSWithDotGit(t *testing.T) {
	f := newLandingFixture(t)
	upstream := strings.TrimSpace(runGit(t, f.remote, "rev-parse", "refs/heads/main"))

	const wantUser = "rework-user"
	const wantPass = "rework-secret"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != wantUser || pass != wantPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/org/repo.git/info/refs" && r.URL.Query().Get("service") == "git-upload-pack" {
			cmd := exec.CommandContext(r.Context(), "git", "upload-pack", "--stateless-rpc", "--advertise-refs", f.remote)
			out, err := cmd.Output()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			header := "# service=git-upload-pack\n"
			_, _ = fmt.Fprintf(w, "%04x%s0000", len(header)+4, header)
			_, _ = w.Write(out)
			return
		}
		if r.URL.Path == "/org/repo.git/git-upload-pack" {
			cmd := exec.CommandContext(r.Context(), "git", "upload-pack", "--stateless-rpc", f.remote)
			cmd.Stdin = r.Body
			out, err := cmd.Output()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			_, _ = w.Write(out)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfgDir := t.TempDir()
	caPath := filepath.Join(cfgDir, "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	helperPath := filepath.Join(cfgDir, "cred-helper.sh")
	helperScript := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"get\" ]; then\n  printf 'username=%s\\npassword=%s\\n'\nfi\n", wantUser, wantPass)
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o700); err != nil {
		t.Fatal(err)
	}
	globalCfg := filepath.Join(cfgDir, "gitconfig")
	cfgContent := fmt.Sprintf("[http]\n\tsslCAInfo = %s\n[credential]\n\thelper = %s\n", caPath, helperPath)
	if err := os.WriteFile(globalCfg, []byte(cfgContent), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)

	httpsRepo := srv.URL + "/org/repo.git"
	runGit(t, f.info.Path, "remote", "set-url", "origin", httpsRepo)
	issue := f.issue
	issue.PublicationRepository = httpsRepo

	got, _, err := f.backend.resolveReworkTargetBase(t.Context(), f.info.Path, issue, MergePrepareOptions{})
	if err != nil {
		t.Fatalf("resolveReworkTargetBase() with authenticated HTTPS .git URL error = %v", err)
	}
	if got != upstream {
		t.Fatalf("resolveReworkTargetBase() = %q, want %q", got, upstream)
	}
}

func TestResolveReworkTargetBaseConfiguredRelativeRemote(t *testing.T) {
	t.Parallel()
	f := newLandingFixture(t)
	upstream := strings.TrimSpace(runGit(t, f.remote, "rev-parse", "refs/heads/main"))

	relBase, err := filepath.Rel(f.info.Path, f.remote)
	if err != nil {
		t.Fatal(err)
	}
	unsuffixed := strings.TrimSuffix(relBase, ".git")
	if unsuffixed == relBase {
		bareDir := filepath.Join(filepath.Dir(f.remote), "upstream.git")
		runGit(t, filepath.Dir(f.remote), "clone", "--bare", f.remote, bareDir)
		f.remote = bareDir
		relBase, err = filepath.Rel(f.info.Path, bareDir)
		if err != nil {
			t.Fatal(err)
		}
		unsuffixed = strings.TrimSuffix(relBase, ".git")
	}
	runGit(t, f.info.Path, "remote", "set-url", "origin", unsuffixed)

	issue := f.issue
	issue.PublicationRepository = f.remote
	got, _, err := f.backend.resolveReworkTargetBase(t.Context(), f.info.Path, issue, MergePrepareOptions{})
	if err != nil {
		t.Fatalf("resolveReworkTargetBase() with configured relative remote %q error = %v", unsuffixed, err)
	}
	if got != upstream {
		t.Fatalf("resolveReworkTargetBase() = %q, want %q", got, upstream)
	}

	plainDir := filepath.Join(filepath.Dir(f.remote), strings.TrimSuffix(filepath.Base(f.remote), ".git"))
	if err := os.MkdirAll(filepath.Join(plainDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	gotWithPlainDir, _, err := f.backend.resolveReworkTargetBase(t.Context(), f.info.Path, issue, MergePrepareOptions{})
	if err != nil {
		t.Fatalf("resolveReworkTargetBase() with plain directory beside %q error = %v", f.remote, err)
	}
	if gotWithPlainDir != upstream {
		t.Fatalf("resolveReworkTargetBase() with plain directory = %q, want %q", gotWithPlainDir, upstream)
	}

	if err := os.RemoveAll(plainDir); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.source, "bundle", "create", plainDir, "HEAD")
	_, _, bundleErr := f.backend.resolveReworkTargetBase(t.Context(), f.info.Path, issue, MergePrepareOptions{})
	var refusal *LandRefusal
	if !errors.As(bundleErr, &refusal) || refusal.Kind != LandRefusalProtected {
		t.Fatalf("resolveReworkTargetBase() with bundle beside .git repo err = %v, want LandRefusalProtected", bundleErr)
	}
}

func TestPrepareReworkAllowsPausedMergeRebaseWithOmittedEmptyCommit(t *testing.T) {
	t.Parallel()
	f := newLandingFixture(t)
	runGit(t, f.info.Path, "commit", "--allow-empty", "-m", "intentionally empty")
	if err := os.WriteFile(filepath.Join(f.info.Path, "shared.txt"), []byte("branch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.info.Path, "add", "shared.txt")
	runGit(t, f.info.Path, "commit", "-m", "branch change")

	upstream := t.TempDir()
	runGit(t, upstream, "clone", f.remote, ".")
	runGit(t, upstream, "config", "user.name", "Test User")
	runGit(t, upstream, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(upstream, "shared.txt"), []byte("upstream\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, upstream, "add", "shared.txt")
	runGit(t, upstream, "commit", "-m", "upstream change")
	runGit(t, upstream, "push", "origin", "main")

	runGit(t, f.info.Path, "fetch", "origin", "main")
	if _, err := runGitAt(t.Context(), f.info.Path, "rebase", "--no-keep-empty", "origin/main"); err == nil {
		t.Fatal("expected rebase conflict")
	}

	if err := os.WriteFile(filepath.Join(f.info.Path, "shared.txt"), []byte("upstream\nbranch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.info.Path, "add", "shared.txt")

	prepared, err := f.backend.PrepareRework(t.Context(), f.info, f.issue, MergePrepareOptions{TargetBranch: "main"}, func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("PrepareRework() with omitted empty commit error = %v", err)
	}
	if prepared.Status != MergePrepareStatusClean {
		t.Fatalf("PrepareRework() status = %q, want clean", prepared.Status)
	}
}

func TestPrepareReworkRejectsConcurrentRebaseChangeBeforeSkip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(t *testing.T, worktree string)
		verify func(t *testing.T, worktree string)
	}{
		{
			name: "staged edit during skip callback",
			mutate: func(t *testing.T, worktree string) {
				if err := os.WriteFile(filepath.Join(worktree, "extra.txt"), []byte("concurrent\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, worktree, "add", "extra.txt")
			},
			verify: func(t *testing.T, worktree string) {
				staged := strings.TrimSpace(runGit(t, worktree, "diff", "--cached", "--name-only"))
				if staged != "extra.txt" {
					t.Fatalf("staged files = %q, want extra.txt", staged)
				}
			},
		},
		{
			name: "unstaged tracked edit during skip callback",
			mutate: func(t *testing.T, worktree string) {
				if err := os.WriteFile(filepath.Join(worktree, "shared.txt"), []byte("concurrent-unstaged\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			verify: func(t *testing.T, worktree string) {
				got, err := os.ReadFile(filepath.Join(worktree, "shared.txt"))
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != "concurrent-unstaged\n" {
					t.Fatalf("shared.txt = %q, want %q", string(got), "concurrent-unstaged\n")
				}
			},
		},
		{
			name: "staged submodule edit with diff.ignoreSubmodules=all",
			mutate: func(t *testing.T, worktree string) {
				runGit(t, worktree, "config", "diff.ignoreSubmodules", "all")
				head := strings.TrimSpace(runGit(t, worktree, "rev-parse", "HEAD"))
				runGit(t, worktree, "update-index", "--add", "--cacheinfo", "160000,"+head+",submod")
			},
			verify: func(t *testing.T, worktree string) {
				staged := strings.TrimSpace(runGit(t, worktree, "diff", "--cached", "--ignore-submodules=none", "--name-only"))
				if staged != "submod" {
					t.Fatalf("staged files = %q, want submod", staged)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandingFixture(t)
			if err := os.WriteFile(filepath.Join(f.info.Path, "shared.txt"), []byte("branch\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, f.info.Path, "add", "shared.txt")
			runGit(t, f.info.Path, "commit", "-m", "first conflict")
			if err := os.WriteFile(filepath.Join(f.info.Path, "second.txt"), []byte("second\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, f.info.Path, "add", "second.txt")
			runGit(t, f.info.Path, "commit", "-m", "second commit")

			upstream := t.TempDir()
			runGit(t, upstream, "clone", f.remote, ".")
			runGit(t, upstream, "config", "user.name", "Test User")
			runGit(t, upstream, "config", "user.email", "test@example.com")
			if err := os.WriteFile(filepath.Join(upstream, "shared.txt"), []byte("upstream\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, upstream, "add", "shared.txt")
			runGit(t, upstream, "commit", "-m", "upstream change")
			runGit(t, upstream, "push", "origin", "main")

			runGit(t, f.info.Path, "fetch", "origin", "main")
			if _, err := runGitAt(t.Context(), f.info.Path, "rebase", "--apply", "origin/main"); err == nil {
				t.Fatal("expected apply rebase conflict")
			}
			if err := os.WriteFile(filepath.Join(f.info.Path, "shared.txt"), []byte("upstream\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, f.info.Path, "add", "shared.txt")

			calls := 0
			_, err := f.backend.PrepareRework(t.Context(), f.info, f.issue, MergePrepareOptions{TargetBranch: "main"}, func(context.Context) error {
				calls++
				if calls == 3 {
					tc.mutate(t, f.info.Path)
				}
				return nil
			})
			if !errors.Is(err, ErrMergeResolutionInvalid) {
				t.Fatalf("PrepareRework() err = %v, want ErrMergeResolutionInvalid (calls=%d)", err, calls)
			}
			if calls != 3 {
				t.Fatalf("validate calls = %d, want 3 (skip callback)", calls)
			}
			active, activeErr := rebaseInProgress(t.Context(), f.info.Path)
			if activeErr != nil || !active {
				t.Fatalf("rebaseInProgress() = (%v, %v), want (true, nil)", active, activeErr)
			}
			tc.verify(t, f.info.Path)
		})
	}
}

func TestPrepareReworkReportsDirtyWhenTargetAdvancesSubmodule(t *testing.T) {
	t.Parallel()
	source := initSourceRepo(t)
	remote := initBareRemote(t)
	subRepo := initSourceRepo(t)
	runGit(t, source, "remote", "add", "origin", remote)
	runGit(t, source, "-c", "protocol.file.allow=always", "submodule", "add", subRepo, "submod")
	runGit(t, source, "commit", "-m", "add submodule")
	runGit(t, source, "push", "-u", "origin", "main")
	base := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	owner := backend.(*LocalGit)
	issue := Issue{Identifier: "native#142", NativeRework: true, BaseRef: base}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, info.Path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")
	writeCheckpointFile(t, info.Path, "feature.txt", "branch change\n")
	runGit(t, info.Path, "add", "feature.txt")
	runGit(t, info.Path, "commit", "-m", "branch change")

	writeCheckpointFile(t, subRepo, "sub.txt", "advanced submodule\n")
	runGit(t, subRepo, "add", "sub.txt")
	runGit(t, subRepo, "commit", "-m", "advance submodule")
	runGit(t, filepath.Join(source, "submod"), "pull", "origin", "main")
	runGit(t, source, "add", "submod")
	runGit(t, source, "commit", "-m", "bump submodule")
	runGit(t, source, "push", "origin", "main")

	prepared, err := owner.PrepareRework(t.Context(), info, issue, MergePrepareOptions{TargetBranch: "main"}, func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("PrepareRework() error = %v", err)
	}
	if prepared.Status != MergePrepareStatusDirty {
		t.Fatalf("PrepareRework() status = %q, want dirty", prepared.Status)
	}
}

func TestPrepareReworkRejectsCheckoutChangeDuringFinalValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		mutateCall        int
		conflict          bool
		prePaused         bool
		prePausedResolved bool
		prePausedDirty    bool
		abortOnMutat      bool
		moveBranchTip     bool
		createUntracked   bool
		expectStillPaused bool
	}{
		{name: "initial validation callback", mutateCall: 1},
		{name: "post-target validation callback", mutateCall: 2},
		{name: "pre-rebase validation callback", mutateCall: 3},
		{name: "post-rebase validation callback", mutateCall: 4},
		{name: "rebase aborted during conflict validation callback", mutateCall: 4, conflict: true, abortOnMutat: true},
		{name: "already-paused conflict aborted during validation callback", mutateCall: 2, conflict: true, prePaused: true, abortOnMutat: true},
		{name: "already-paused dirty aborted during validation callback", mutateCall: 2, conflict: true, prePaused: true, prePausedDirty: true, abortOnMutat: true},
		{name: "already-paused conflict branch tip moved during validation callback", mutateCall: 2, conflict: true, prePaused: true, moveBranchTip: true},
		{name: "already-paused dirty branch tip moved during validation callback", mutateCall: 2, conflict: true, prePaused: true, prePausedDirty: true, moveBranchTip: true},
		{name: "already-paused continuation untracked file created during validation callback", mutateCall: 2, conflict: true, prePaused: true, prePausedResolved: true, createUntracked: true, expectStillPaused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandingFixture(t)
			writeCheckpointFile(t, f.info.Path, "feature.txt", "branch change\n")
			runGit(t, f.info.Path, "add", "feature.txt")
			runGit(t, f.info.Path, "commit", "-m", "branch change")

			upstream := t.TempDir()
			runGit(t, upstream, "clone", f.remote, ".")
			runGit(t, upstream, "config", "user.name", "Test User")
			runGit(t, upstream, "config", "user.email", "test@example.com")
			if tc.conflict {
				writeCheckpointFile(t, upstream, "feature.txt", "upstream conflict\n")
				runGit(t, upstream, "add", "feature.txt")
			} else {
				writeCheckpointFile(t, upstream, "upstream.txt", "upstream\n")
				runGit(t, upstream, "add", "upstream.txt")
			}
			runGit(t, upstream, "commit", "-m", "upstream change")
			runGit(t, upstream, "push", "origin", "main")

			if tc.prePaused {
				runGit(t, f.info.Path, "fetch", "origin", "main")
				if _, err := runGitAt(t.Context(), f.info.Path, "rebase", "origin/main"); err == nil {
					t.Fatal("expected pre-paused rebase conflict")
				}
				if tc.prePausedResolved {
					writeCheckpointFile(t, f.info.Path, "feature.txt", "resolved\n")
					runGit(t, f.info.Path, "add", "feature.txt")
				}
				if tc.prePausedDirty {
					writeCheckpointFile(t, f.info.Path, "feature.txt", "resolved\n")
					runGit(t, f.info.Path, "add", "feature.txt")
					writeCheckpointFile(t, f.info.Path, "feature.txt", "resolved-unstaged\n")
				}
			}

			calls := 0
			_, err := f.backend.PrepareRework(t.Context(), f.info, f.issue, MergePrepareOptions{TargetBranch: "main"}, func(context.Context) error {
				calls++
				if calls == tc.mutateCall {
					if tc.abortOnMutat {
						runGit(t, f.info.Path, "rebase", "--abort")
						return nil
					}
					if tc.moveBranchTip {
						originMain := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "origin/main"))
						runGit(t, f.info.Path, "update-ref", "refs/heads/"+f.info.Branch, originMain)
						return nil
					}
					if tc.createUntracked {
						writeCheckpointFile(t, f.info.Path, "untracked.txt", "created during validation\n")
						return nil
					}
					writeCheckpointFile(t, f.info.Path, "concurrent.txt", "concurrent commit\n")
					runGit(t, f.info.Path, "add", "concurrent.txt")
					runGit(t, f.info.Path, "commit", "-m", "concurrent commit")
				}
				return nil
			})
			if !errors.Is(err, ErrMergeResolutionInvalid) {
				t.Fatalf("PrepareRework() err = %v, want ErrMergeResolutionInvalid (calls=%d)", err, calls)
			}
			if calls < tc.mutateCall {
				t.Fatalf("validate calls = %d, want >= %d", calls, tc.mutateCall)
			}
			if tc.expectStillPaused {
				active, activeErr := rebaseInProgress(t.Context(), f.info.Path)
				if activeErr != nil || !active {
					t.Fatalf("rebaseInProgress() = (%v, %v), want (true, nil)", active, activeErr)
				}
			}
		})
	}
}

func TestFinalizeNativeWorkRejectsUnstagedEditDuringValidationWithoutCommitting(t *testing.T) {
	t.Parallel()
	f := newLandingFixture(t)
	headBefore := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD"))
	writeCheckpointFile(t, f.info.Path, "feature.txt", "staged version\n")
	runGit(t, f.info.Path, "add", "feature.txt")

	calls := 0
	_, err := f.backend.FinalizeNativeWork(t.Context(), f.info, f.issue, func(context.Context) error {
		calls++
		if calls == 2 {
			writeCheckpointFile(t, f.info.Path, "feature.txt", "unstaged edit during validation\n")
		}
		return nil
	})
	var incomplete *SourceFinalizationIncomplete
	if !errors.As(err, &incomplete) || incomplete.Status != MergePrepareStatusDirty {
		t.Fatalf("FinalizeNativeWork() err = %v, want dirty SourceFinalizationIncomplete (calls=%d)", err, calls)
	}
	if calls != 2 {
		t.Fatalf("validate calls = %d, want 2", calls)
	}
	headAfter := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD"))
	if headAfter != headBefore {
		t.Fatalf("HEAD advanced from %s to %s after unstaged edit during validation", headBefore, headAfter)
	}
}

func TestFinalizeNativeWorkRejectsDirtyWorktreeBeforeReconciliationCommit(t *testing.T) {
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
	issue := Issue{Identifier: "native#143", NativeRework: true, BaseRef: base, ProgressBaseRef: "main"}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	writeCheckpointFile(t, info.Path, "shared.txt", "branch change\n")
	runGit(t, info.Path, "add", "shared.txt")
	runGit(t, info.Path, "commit", "-m", "branch change")
	sourceHead := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))

	writeCheckpointFile(t, source, "shared.txt", "upstream wins\n")
	runGit(t, source, "add", "shared.txt")
	runGit(t, source, "commit", "-m", "upstream change")
	runGit(t, source, "push", "origin", "main")
	targetHead := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))

	runGit(t, info.Path, "fetch", "origin", "main")
	if _, err := runGitAt(t.Context(), info.Path, "rebase", "origin/main"); err == nil {
		t.Fatal("expected rebase conflict")
	}
	writeCheckpointFile(t, info.Path, "shared.txt", "upstream wins\n")
	runGit(t, info.Path, "add", "shared.txt")

	issue.Source = &ChangeSource{Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: base, HeadSHA: sourceHead}}}

	var createdDirty bool
	calls := 0
	_, err = owner.FinalizeNativeWork(t.Context(), info, issue, func(context.Context) error {
		calls++
		if calls == 8 {
			createdDirty = true
			writeCheckpointFile(t, info.Path, "untracked.txt", "dirty before reconcile\n")
		}
		return nil
	})
	var incomplete *SourceFinalizationIncomplete
	if !errors.As(err, &incomplete) || incomplete.Status != MergePrepareStatusDirty {
		t.Fatalf("FinalizeNativeWork() err = %v, want dirty SourceFinalizationIncomplete (calls=%d)", err, calls)
	}
	if !createdDirty {
		t.Fatalf("did not reach reconciliation validation callback (calls=%d)", calls)
	}
	headAfter := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
	if headAfter != targetHead {
		t.Fatalf("HEAD advanced from %s to %s on reconciliation commit despite dirty worktree", targetHead, headAfter)
	}
}

func TestPrepareChangeSourceWorktreeRejectsUntrackedFileCreatedDuringLockWait(t *testing.T) {
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
	issue := Issue{Identifier: "native#144", NativeRework: true, BaseRef: base}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	writeCheckpointFile(t, info.Path, "v1.txt", "version 1\n")
	runGit(t, info.Path, "add", "v1.txt")
	runGit(t, info.Path, "commit", "-m", "version 1")
	runGit(t, info.Path, "checkout", "-b", "other-base", base)
	writeCheckpointFile(t, info.Path, "v2.txt", "version 2\n")
	runGit(t, info.Path, "add", "v2.txt")
	runGit(t, info.Path, "commit", "-m", "version 2")
	v2Head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
	runGit(t, info.Path, "checkout", info.Branch)
	runGit(t, info.Path, "reset", "--hard", base)
	runGit(t, info.Path, "branch", "-D", "other-base")

	issue.Source = &ChangeSource{
		Version: tracker.ChangeVersion{
			ID: "ver-2",
			ChangeVersionInput: tracker.ChangeVersionInput{
				BaseSHA: base,
				HeadSHA: v2Head,
			},
		},
	}

	release, err := acquireWorktreeOperation(t.Context(), info.Path)
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		_, prepErr := owner.prepareChangeSourceWorktree(t.Context(), info, issue)
		errCh <- prepErr
	}()
	for {
		worktreeOperationLocks.Lock()
		entry := worktreeOperationLocks.byPath[worktreeOperationKey(info.Path)]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		worktreeOperationLocks.Unlock()
		if refs >= 2 {
			break
		}
		select {
		case prepErr := <-errCh:
			release()
			t.Fatalf("prepareChangeSourceWorktree returned before lock wait: %v", prepErr)
		case <-t.Context().Done():
			release()
			t.Fatal("timed out waiting for prepareChangeSourceWorktree to queue on worktree lock")
		default:
		}
	}
	pendingPath := filepath.Join(info.Path, "pending.txt")
	writeCheckpointFile(t, info.Path, "pending.txt", "untracked during wait\n")
	release()

	prepErr := <-errCh
	var refusal *LandRefusal
	if !errors.As(prepErr, &refusal) || refusal.Kind != LandRefusalHeadMoved {
		t.Fatalf("prepareChangeSourceWorktree() err = %v, want LandRefusalHeadMoved", prepErr)
	}
	got, readErr := os.ReadFile(pendingPath)
	if readErr != nil || string(got) != "untracked during wait\n" {
		t.Fatalf("pending.txt after refusal = (%q, %v), want preserved", string(got), readErr)
	}
}

func TestFinalizeNativeWorkSerializesAgainstConcurrentFreshCheckout(t *testing.T) {
	t.Parallel()
	f := newLandingFixture(t)
	baseHead := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD"))
	writeCheckpointFile(t, f.info.Path, "staged.txt", "staged work\n")
	runGit(t, f.info.Path, "add", "staged.txt")

	freshStarted := make(chan struct{})
	freshDone := make(chan error, 1)
	freshIssue := f.issue
	freshIssue.FreshCheckout = true

	calls := 0
	_, err := f.backend.FinalizeNativeWork(t.Context(), f.info, f.issue, func(context.Context) error {
		calls++
		if calls == 1 {
			go func() {
				close(freshStarted)
				_, createErr := f.backend.Create(t.Context(), freshIssue)
				freshDone <- createErr
			}()
			<-freshStarted
			for {
				worktreeOperationLocks.Lock()
				entry := worktreeOperationLocks.byPath[worktreeOperationKey(f.info.Path)]
				refs := 0
				if entry != nil {
					refs = entry.refs
				}
				worktreeOperationLocks.Unlock()
				if refs >= 2 {
					break
				}
				select {
				case createErr := <-freshDone:
					return fmt.Errorf("fresh checkout bypassed worktree lock: %w", createErr)
				case <-t.Context().Done():
					return t.Context().Err()
				default:
				}
			}
		} else {
			select {
			case createErr := <-freshDone:
				return fmt.Errorf("fresh checkout finished while FinalizeNativeWork held lock: %w", createErr)
			default:
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("FinalizeNativeWork() error = %v", err)
	}
	if createErr := <-freshDone; createErr != nil {
		t.Fatalf("concurrent FreshCheckout error = %v", createErr)
	}
	reflog := runGit(t, f.source, "reflog", "show", "--format=%H %gs", "refs/heads/"+f.info.Branch)
	var foundCommit string
	for _, line := range strings.Split(strings.TrimSpace(reflog), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] != baseHead {
			if strings.Contains(runGit(t, f.source, "show", "--stat", "--format=", fields[0]), "staged.txt") {
				foundCommit = fields[0]
				break
			}
		}
	}
	if foundCommit == "" {
		t.Fatalf("FinalizeNativeWork did not commit staged.txt before FreshCheckout ran; reflog:\n%s", reflog)
	}
}

func TestFinalizeNativeWorkProtectsBranchHolderFromConcurrentFreshCheckout(t *testing.T) {
	t.Parallel()
	f := newLandingFixture(t)
	writeCheckpointFile(t, f.info.Path, "staged.txt", "staged work\n")
	runGit(t, f.info.Path, "add", "staged.txt")

	otherBackend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       filepath.Join(t.TempDir(), "other-workspaces"),
		SourceRoot: f.source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherIssue := f.issue
	otherIssue.FreshCheckout = true

	var otherErr error
	calls := 0
	_, err = f.backend.FinalizeNativeWork(t.Context(), f.info, f.issue, func(context.Context) error {
		calls++
		if calls == 1 {
			_, otherErr = otherBackend.Create(t.Context(), otherIssue)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("FinalizeNativeWork() error = %v", err)
	}
	if !errors.Is(otherErr, ErrMergeResolutionInvalid) {
		t.Fatalf("concurrent FreshCheckout at other path err = %v, want ErrMergeResolutionInvalid", otherErr)
	}
	branchNow := strings.TrimSpace(runGit(t, f.info.Path, "symbolic-ref", "--short", "HEAD"))
	if branchNow != f.info.Branch {
		t.Fatalf("branch after FinalizeNativeWork = %q, want %q", branchNow, f.info.Branch)
	}
}

func TestCreateHoldsWorktreeLockAcrossCheckpointMatchAndReuse(t *testing.T) {
	t.Parallel()
	f := newLandingFixture(t)
	baseHead := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD"))
	writeCheckpointFile(t, f.info.Path, "staged.txt", "staged work\n")
	runGit(t, f.info.Path, "add", "staged.txt")

	treeSHA := strings.TrimSpace(runGit(t, f.info.Path, "write-tree"))
	commitSHA := strings.TrimSpace(runGit(t, f.info.Path, "commit-tree", treeSHA, "-p", baseHead, "-m", "checkpoint"))
	cpRef := "refs/detent/checkpoints/" + f.info.Key + "/attempt-1"
	runGit(t, f.source, "update-ref", cpRef, commitSHA)

	cpIssue := f.issue
	cpIssue.Checkpoint = &CheckpointRef{
		Ref:       cpRef,
		CommitSHA: commitSHA,
		HeadSHA:   baseHead,
		TreeSHA:   treeSHA,
	}

	createStarted := make(chan struct{})
	createDone := make(chan error, 1)
	calls := 0
	_, err := f.backend.FinalizeNativeWork(t.Context(), f.info, f.issue, func(context.Context) error {
		calls++
		if calls == 1 {
			go func() {
				close(createStarted)
				_, createErr := f.backend.Create(t.Context(), cpIssue)
				createDone <- createErr
			}()
			<-createStarted
			for {
				worktreeOperationLocks.Lock()
				entry := worktreeOperationLocks.byPath[worktreeOperationKey(f.info.Path)]
				refs := 0
				if entry != nil {
					refs = entry.refs
				}
				worktreeOperationLocks.Unlock()
				if refs >= 2 {
					break
				}
				select {
				case createErr := <-createDone:
					return fmt.Errorf("Create bypassed worktree lock during checkpoint check: %w", createErr)
				case <-t.Context().Done():
					return t.Context().Err()
				default:
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("FinalizeNativeWork() error = %v", err)
	}
	if createErr := <-createDone; createErr != nil {
		t.Fatalf("Create() with checkpoint after FinalizeNativeWork error = %v", createErr)
	}
	headAfter := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD"))
	if headAfter != baseHead {
		t.Fatalf("HEAD after checkpoint Create = %s, want checkpoint HeadSHA %s", headAfter, baseHead)
	}
}

func TestCreateRetainedPhaseSerializesAgainstFinalizeNativeWork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		call func(ctx context.Context, f landingFixture, issue Issue) error
	}{
		{
			name: "Create",
			call: func(ctx context.Context, f landingFixture, issue Issue) error {
				_, err := f.backend.Create(ctx, issue)
				return err
			},
		},
		{
			name: "VerifyItemPhaseCheckout",
			call: func(ctx context.Context, f landingFixture, issue Issue) error {
				info := f.info
				info.Created = false
				return f.backend.VerifyItemPhaseCheckout(ctx, info, issue)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandingFixture(t)
			writeCheckpointFile(t, f.info.Path, "staged.txt", "staged work\n")
			runGit(t, f.info.Path, "add", "staged.txt")

			retainedIssue := f.issue
			retainedIssue.VerifyPhaseBranch = true
			retainedIssue.RetainPhaseCheckout = true

			started := make(chan struct{})
			done := make(chan error, 1)
			calls := 0
			_, err := f.backend.FinalizeNativeWork(t.Context(), f.info, f.issue, func(context.Context) error {
				calls++
				if calls == 1 {
					go func() {
						close(started)
						done <- tc.call(t.Context(), f, retainedIssue)
					}()
					<-started
					for {
						worktreeOperationLocks.Lock()
						entry := worktreeOperationLocks.byPath[worktreeOperationKey(f.info.Path)]
						refs := 0
						if entry != nil {
							refs = entry.refs
						}
						worktreeOperationLocks.Unlock()
						if refs >= 2 {
							break
						}
						select {
						case callErr := <-done:
							return fmt.Errorf("%s bypassed worktree lock: %w", tc.name, callErr)
						case <-t.Context().Done():
							return t.Context().Err()
						default:
						}
					}
				} else {
					select {
					case callErr := <-done:
						return fmt.Errorf("%s returned while FinalizeNativeWork held lock: %w", tc.name, callErr)
					default:
					}
				}
				return nil
			})
			if err != nil {
				t.Fatalf("FinalizeNativeWork() error = %v", err)
			}
			if callErr := <-done; callErr != nil {
				t.Fatalf("%s() after FinalizeNativeWork error = %v", tc.name, callErr)
			}
		})
	}
}

func TestPushItemPhaseSerializesAgainstFinalizeNativeWorkReconciliationCommit(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T, id string) (*LocalGit, Info, Issue, string, string) {
		t.Helper()
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
		issue := Issue{Identifier: id, NativeRework: true, BaseRef: base, ProgressBaseRef: "main", PublicationRepository: remote}
		info, err := backend.Create(t.Context(), issue)
		if err != nil {
			t.Fatal(err)
		}
		writeCheckpointFile(t, info.Path, "shared.txt", "branch change\n")
		runGit(t, info.Path, "add", "shared.txt")
		runGit(t, info.Path, "commit", "-m", "branch change")
		sourceHead := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))

		writeCheckpointFile(t, source, "shared.txt", "upstream wins\n")
		runGit(t, source, "add", "shared.txt")
		runGit(t, source, "commit", "-m", "upstream change")
		runGit(t, source, "push", "origin", "main")
		targetHead := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))

		runGit(t, info.Path, "fetch", "origin", "main")
		if _, err := runGitAt(t.Context(), info.Path, "rebase", "origin/main"); err == nil {
			t.Fatal("expected rebase conflict")
		}
		writeCheckpointFile(t, info.Path, "shared.txt", "upstream wins\n")
		runGit(t, info.Path, "add", "shared.txt")

		issue.Source = &ChangeSource{Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: base, HeadSHA: sourceHead}}}
		return owner, info, issue, remote, targetHead
	}

	t.Run("authority-only callback waits for reconciliation commit", func(t *testing.T) {
		t.Parallel()
		owner, info, issue, _, targetHead := setup(t, "native#145")
		type pushResult struct {
			head string
			err  error
		}
		pushStarted := make(chan struct{})
		pushDone := make(chan pushResult, 1)
		calls := 0
		_, err := owner.FinalizeNativeWork(t.Context(), info, issue, func(context.Context) error {
			calls++
			if calls == 8 {
				go func() {
					close(pushStarted)
					_, pushedHead, _, pushErr := owner.PushItemPhase(t.Context(), info, issue, func(context.Context) error {
						return nil
					})
					pushDone <- pushResult{head: pushedHead, err: pushErr}
				}()
				<-pushStarted
				for {
					worktreeOperationLocks.Lock()
					entry := worktreeOperationLocks.byPath[worktreeOperationKey(info.Path)]
					refs := 0
					if entry != nil {
						refs = entry.refs
					}
					worktreeOperationLocks.Unlock()
					if refs >= 2 {
						break
					}
					select {
					case res := <-pushDone:
						return fmt.Errorf("PushItemPhase bypassed worktree lock: head=%s err=%v", res.head, res.err)
					case <-t.Context().Done():
						return t.Context().Err()
					default:
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("FinalizeNativeWork() error = %v", err)
		}
		reconcileHead := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
		if reconcileHead == targetHead {
			t.Fatalf("expected reconciliation commit beyond target %s", targetHead)
		}
		res := <-pushDone
		if res.err != nil {
			t.Fatalf("PushItemPhase() error = %v", res.err)
		}
		if res.head != reconcileHead {
			t.Fatalf("PushItemPhase() published %s, want reconciliation commit %s (target=%s)", res.head, reconcileHead, targetHead)
		}
	})

	for _, tc := range []struct {
		name     string
		id       string
		denyCall int
	}{
		{name: "paused rebase denial at callback 1 blocks queued PushItemPhase", id: "native#146a", denyCall: 1},
		{name: "failed reconciliation commit at callback 8 blocks queued PushItemPhase", id: "native#146b", denyCall: 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			owner, info, issue, remote, _ := setup(t, tc.id)
			reconcileBlocked := errors.New("reconciliation authority denied")
			pushStarted := make(chan struct{})
			pushDone := make(chan error, 1)
			calls := 0
			_, err := owner.FinalizeNativeWork(t.Context(), info, issue, func(context.Context) error {
				calls++
				if calls == tc.denyCall {
					go func() {
						close(pushStarted)
						_, _, _, pushErr := owner.PushItemPhase(t.Context(), info, issue, func(context.Context) error {
							return nil
						})
						pushDone <- pushErr
					}()
					<-pushStarted
					for {
						worktreeOperationLocks.Lock()
						entry := worktreeOperationLocks.byPath[worktreeOperationKey(info.Path)]
						refs := 0
						if entry != nil {
							refs = entry.refs
						}
						worktreeOperationLocks.Unlock()
						if refs >= 2 {
							break
						}
						select {
						case pushErr := <-pushDone:
							return fmt.Errorf("PushItemPhase bypassed worktree lock: %w", pushErr)
						case <-t.Context().Done():
							return t.Context().Err()
						default:
						}
					}
					return reconcileBlocked
				}
				return nil
			})
			if !errors.Is(err, reconcileBlocked) {
				t.Fatalf("FinalizeNativeWork() err = %v, want %v", err, reconcileBlocked)
			}
			pushErr := <-pushDone
			var refusal *LandRefusal
			if !errors.As(pushErr, &refusal) || refusal.Kind != LandRefusalHeadMoved {
				t.Fatalf("PushItemPhase() err = %v, want LandRefusalHeadMoved", pushErr)
			}
			if remoteRef := strings.TrimSpace(runGit(t, remote, "for-each-ref", "refs/heads/"+info.PublishBranch())); remoteRef != "" {
				t.Fatalf("remote branch was published despite incomplete finalization: %s", remoteRef)
			}
		})
	}

	t.Run("authorize callback can call ItemPhaseHead without deadlocking", func(t *testing.T) {
		t.Parallel()
		f := newLandingFixture(t)
		issue := f.issue
		issue.PublicationRepository = f.remote
		var callbackHead string
		_, pushedHead, _, err := f.backend.PushItemPhase(t.Context(), f.info, issue, func(ctx context.Context) error {
			head, _, headErr := f.backend.ItemPhaseHead(ctx, f.info, issue)
			callbackHead = head
			return headErr
		})
		if err != nil {
			t.Fatalf("PushItemPhase() with nested ItemPhaseHead error = %v", err)
		}
		if pushedHead == "" || pushedHead != callbackHead {
			t.Fatalf("pushedHead=%q callbackHead=%q, want matching non-empty SHA", pushedHead, callbackHead)
		}
	})

	t.Run("fresh rebase absorbed by upstream blocks queued PushItemPhase on post-rebase denial", func(t *testing.T) {
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
		issue := Issue{Identifier: "native#147", NativeRework: true, BaseRef: base, ProgressBaseRef: "main", PublicationRepository: remote}
		info, err := backend.Create(t.Context(), issue)
		if err != nil {
			t.Fatal(err)
		}
		writeCheckpointFile(t, info.Path, "shared.txt", "branch change\n")
		runGit(t, info.Path, "add", "shared.txt")
		runGit(t, info.Path, "commit", "-m", "branch change")
		sourceHead := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))

		runGit(t, source, "merge", "--ff-only", sourceHead)
		writeCheckpointFile(t, source, "shared.txt", "upstream followup\n")
		runGit(t, source, "add", "shared.txt")
		runGit(t, source, "commit", "-m", "upstream followup")
		runGit(t, source, "push", "origin", "main")
		targetHead := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))

		issue.Source = &ChangeSource{Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: base, HeadSHA: sourceHead}}}

		denied := errors.New("post-rebase validation denied")
		pushStarted := make(chan struct{})
		pushDone := make(chan error, 1)
		calls := 0
		_, err = owner.FinalizeNativeWork(t.Context(), info, issue, func(context.Context) error {
			calls++
			if strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")) == targetHead {
				go func() {
					close(pushStarted)
					_, _, _, pushErr := owner.PushItemPhase(t.Context(), info, issue, func(context.Context) error {
						return nil
					})
					pushDone <- pushErr
				}()
				<-pushStarted
				for {
					worktreeOperationLocks.Lock()
					entry := worktreeOperationLocks.byPath[worktreeOperationKey(info.Path)]
					refs := 0
					if entry != nil {
						refs = entry.refs
					}
					worktreeOperationLocks.Unlock()
					if refs >= 2 {
						break
					}
					select {
					case pushErr := <-pushDone:
						return fmt.Errorf("PushItemPhase bypassed worktree lock: %w", pushErr)
					case <-t.Context().Done():
						return t.Context().Err()
					default:
					}
				}
				return denied
			}
			return nil
		})
		if !errors.Is(err, denied) {
			t.Fatalf("FinalizeNativeWork() err = %v, want %v (calls=%d)", err, denied, calls)
		}
		pushErr := <-pushDone
		var refusal *LandRefusal
		if !errors.As(pushErr, &refusal) || refusal.Kind != LandRefusalHeadMoved {
			t.Fatalf("PushItemPhase() err = %v, want LandRefusalHeadMoved", pushErr)
		}
		if remoteRef := strings.TrimSpace(runGit(t, remote, "for-each-ref", "refs/heads/"+info.PublishBranch())); remoteRef != "" {
			t.Fatalf("remote branch was published despite post-rebase denial: %s", remoteRef)
		}

		writeCheckpointFile(t, source, "unrelated.txt", "target advanced before retry\n")
		runGit(t, source, "add", "unrelated.txt")
		runGit(t, source, "commit", "-m", "advance target before retry")
		runGit(t, source, "push", "origin", "main")
		advancedTarget := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))

		lockPath := filepath.Join(linkedWorktreeGitDir(t, info.Path), "index.lock")
		if err := os.WriteFile(lockPath, []byte("locked\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, lockErr := owner.FinalizeNativeWork(t.Context(), info, issue, func(context.Context) error { return nil }); lockErr == nil {
			t.Fatal("expected FinalizeNativeWork to fail when index.lock is held")
		}
		if err := os.Remove(lockPath); err != nil {
			t.Fatal(err)
		}
		_, _, _, blockedPushErr := owner.PushItemPhase(t.Context(), info, issue, func(context.Context) error { return nil })
		if !errors.As(blockedPushErr, &refusal) || refusal.Kind != LandRefusalHeadMoved {
			t.Fatalf("PushItemPhase() after failed retry err = %v, want LandRefusalHeadMoved", blockedPushErr)
		}
		if remoteRef := strings.TrimSpace(runGit(t, remote, "for-each-ref", "refs/heads/"+info.PublishBranch())); remoteRef != "" {
			t.Fatalf("remote branch was published after failed retry: %s", remoteRef)
		}

		if _, err := owner.FinalizeNativeWork(t.Context(), info, issue, func(context.Context) error { return nil }); err != nil {
			t.Fatalf("retry FinalizeNativeWork() error = %v", err)
		}
		reconcileHead := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
		if reconcileHead == advancedTarget || reconcileHead == targetHead {
			t.Fatalf("retry did not create reconciliation commit: head=%s advancedTarget=%s targetHead=%s", reconcileHead, advancedTarget, targetHead)
		}
		_, pushedHead, _, pushErr := owner.PushItemPhase(t.Context(), info, issue, func(context.Context) error { return nil })
		if pushErr != nil {
			t.Fatalf("retry PushItemPhase() error = %v", pushErr)
		}
		if pushedHead != reconcileHead {
			t.Fatalf("retry PushItemPhase() published %s, want reconciliation commit %s", pushedHead, reconcileHead)
		}
	})
}

func TestPausedRebaseSnapshotRejectsFIFO(t *testing.T) {
	if wtPath := os.Getenv("DETENT_TEST_SNAPSHOT_FIFO_PATH"); wtPath != "" {
		if _, err := pausedRebaseSnapshot(t.Context(), wtPath); !errors.Is(err, ErrMergeResolutionInvalid) {
			t.Fatalf("pausedRebaseSnapshot(fifo) err = %v, want ErrMergeResolutionInvalid", err)
		}
		return
	}
	t.Parallel()
	mkfifo, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("mkfifo unavailable")
	}
	f := newLandingFixture(t)
	gitDir := linkedWorktreeGitDir(t, f.info.Path)
	rebaseDir := filepath.Join(gitDir, "rebase-apply")
	if err := os.MkdirAll(rebaseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rebaseDir, "last"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fifoPath := filepath.Join(rebaseDir, "0001")
	if out, err := exec.CommandContext(t.Context(), mkfifo, fifoPath).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo failed: %v (%s)", err, out)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestPausedRebaseSnapshotRejectsFIFO$")
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(),
		"DETENT_TEST_SNAPSHOT_FIFO_PATH="+f.info.Path,
		"GOCOVERDIR="+t.TempDir(),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pausedRebaseSnapshot(fifo) subprocess failed: %v\n%s", err, out)
	}
}

func TestGitIndexChecksumDetectsChangeUnderSkipHash(t *testing.T) {
	t.Parallel()
	f := newLandingFixture(t)
	runGit(t, f.info.Path, "config", "index.skipHash", "true")
	indexPath := filepath.Join(linkedWorktreeGitDir(t, f.info.Path), "index")
	fixedTime := time.Unix(1700000000, 0)

	writeCheckpointFile(t, f.info.Path, "file.txt", "aaaa\n")
	runGit(t, f.info.Path, "add", "file.txt")
	if err := os.Chtimes(indexPath, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}
	sumA, err := gitIndexChecksum(t.Context(), f.info.Path)
	if err != nil {
		t.Fatal(err)
	}

	writeCheckpointFile(t, f.info.Path, "file.txt", "bbbb\n")
	runGit(t, f.info.Path, "add", "file.txt")
	if err := os.Chtimes(indexPath, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}
	sumB, err := gitIndexChecksum(t.Context(), f.info.Path)
	if err != nil {
		t.Fatal(err)
	}
	if sumA == sumB {
		t.Fatalf("gitIndexChecksum() identical (%q) across different staged contents with index.skipHash=true", sumA)
	}
}

func TestFinalizeNativeWorkPausedRebaseMergeDoneAccounting(t *testing.T) {
	t.Parallel()
	t.Run("fast-forwarded commit in done without rewritten-list succeeds", func(t *testing.T) {
		t.Parallel()
		f := newLandingFixture(t)
		f.issue.NativeRework = true
		f.issue.ProgressBaseRef = "main"

		baseMain := strings.TrimSpace(runGit(t, f.source, "rev-parse", "main"))
		runGit(t, f.info.Path, "reset", "--hard", baseMain)
		writeCheckpointFile(t, f.info.Path, "ff.txt", "fast-forwarded commit\n")
		runGit(t, f.info.Path, "add", "ff.txt")
		runGit(t, f.info.Path, "commit", "-m", "fast-forwarded commit")
		ffCommit := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD"))

		writeCheckpointFile(t, f.info.Path, "conflict.txt", "branch change\n")
		runGit(t, f.info.Path, "add", "conflict.txt")
		runGit(t, f.info.Path, "commit", "-m", "conflicting commit")
		conflictCommit := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD"))

		runGit(t, f.info.Path, "checkout", "--detach", ffCommit)
		gitDir := linkedWorktreeGitDir(t, f.info.Path)
		rebaseDir := filepath.Join(gitDir, "rebase-merge")
		if err := os.MkdirAll(rebaseDir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, content := range map[string]string{
			"onto":            baseMain + "\n",
			"orig-head":       conflictCommit + "\n",
			"head-name":       "refs/heads/" + f.info.Branch + "\n",
			"stopped-sha":     conflictCommit + "\n",
			"done":            "pick " + ffCommit + " fast-forwarded commit\npick " + conflictCommit + " conflicting commit\n",
			"git-rebase-todo": "",
			"message":         "conflicting commit\n",
			"author-script":   "GIT_AUTHOR_NAME='Test'\nGIT_AUTHOR_EMAIL='test@example.com'\nGIT_AUTHOR_DATE='@1700000000 +0000'\n",
		} {
			if err := os.WriteFile(filepath.Join(rebaseDir, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		writeCheckpointFile(t, f.info.Path, "conflict.txt", "branch change\n")
		runGit(t, f.info.Path, "add", "conflict.txt")

		if _, err := f.backend.FinalizeNativeWork(t.Context(), f.info, f.issue, func(context.Context) error { return nil }); err != nil {
			t.Fatalf("FinalizeNativeWork() with fast-forwarded done commit error = %v", err)
		}
		gotFF, err := os.ReadFile(filepath.Join(f.info.Path, "ff.txt"))
		if err != nil || string(gotFF) != "fast-forwarded commit\n" {
			t.Fatalf("ff.txt = (%q, %v), want preserved", string(gotFF), err)
		}
	})

	t.Run("non-redundant commit dropped from done without rewritten-list is rejected", func(t *testing.T) {
		t.Parallel()
		f := newLandingFixture(t)
		f.issue.NativeRework = true
		f.issue.ProgressBaseRef = "main"

		baseMain := strings.TrimSpace(runGit(t, f.source, "rev-parse", "main"))
		writeCheckpointFile(t, f.info.Path, "dropped.txt", "non-redundant work\n")
		runGit(t, f.info.Path, "add", "dropped.txt")
		runGit(t, f.info.Path, "commit", "-m", "non-redundant commit")
		droppedCommit := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD"))

		writeCheckpointFile(t, f.info.Path, "second.txt", "second work\n")
		runGit(t, f.info.Path, "add", "second.txt")
		runGit(t, f.info.Path, "commit", "-m", "second commit")
		secondCommit := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD"))

		runGit(t, f.info.Path, "checkout", "--detach", baseMain)
		gitDir := linkedWorktreeGitDir(t, f.info.Path)
		rebaseDir := filepath.Join(gitDir, "rebase-merge")
		if err := os.MkdirAll(rebaseDir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, content := range map[string]string{
			"onto":            baseMain + "\n",
			"orig-head":       secondCommit + "\n",
			"head-name":       "refs/heads/" + f.info.Branch + "\n",
			"stopped-sha":     secondCommit + "\n",
			"done":            "pick " + droppedCommit + " non-redundant commit\npick " + secondCommit + " second commit\n",
			"git-rebase-todo": "",
			"message":         "second commit\n",
			"author-script":   "GIT_AUTHOR_NAME='Test'\nGIT_AUTHOR_EMAIL='test@example.com'\nGIT_AUTHOR_DATE='@1700000000 +0000'\n",
		} {
			if err := os.WriteFile(filepath.Join(rebaseDir, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		writeCheckpointFile(t, f.info.Path, "second.txt", "second work\n")
		runGit(t, f.info.Path, "add", "second.txt")

		_, err := f.backend.FinalizeNativeWork(t.Context(), f.info, f.issue, func(context.Context) error { return nil })
		if !errors.Is(err, ErrMergeResolutionInvalid) {
			t.Fatalf("FinalizeNativeWork() with dropped non-redundant commit err = %v, want ErrMergeResolutionInvalid", err)
		}
	})

	t.Run("rescheduled signing failure recovers through FinalizeNativeWork", func(t *testing.T) {
		t.Parallel()
		sshKeygen, err := exec.LookPath("ssh-keygen")
		if err != nil {
			t.Skip("ssh-keygen unavailable")
		}
		source := initSourceRepo(t)
		remote := initBareRemote(t)
		runGit(t, source, "remote", "add", "origin", remote)
		runGit(t, source, "push", "-u", "origin", "main")
		root := filepath.Join(t.TempDir(), "workspaces")
		owner, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
		if err != nil {
			t.Fatal(err)
		}
		issue := Issue{Identifier: "example#rescheduled", NativeRework: true, ProgressBaseRef: "main"}
		info, err := owner.Create(t.Context(), issue)
		if err != nil {
			t.Fatal(err)
		}
		writeCheckpointFile(t, info.Path, "topic.txt", "signed work\n")
		runGit(t, info.Path, "add", "topic.txt")
		runGit(t, info.Path, "commit", "-m", "topic commit")

		writeCheckpointFile(t, source, "upstream.txt", "upstream change\n")
		runGit(t, source, "add", "upstream.txt")
		runGit(t, source, "commit", "-m", "upstream commit")
		runGit(t, source, "push", "origin", "main")
		targetHead := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))

		key := filepath.Join(t.TempDir(), "signing-key")
		if out, err := exec.CommandContext(t.Context(), sshKeygen, "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen failed: %v: %s", err, out)
		}
		failingSigner := filepath.Join(t.TempDir(), "failing-signer")
		script := "#!/bin/sh\nfor arg do payload=$arg; done\nif test -f \"$payload\"; then\n content=$(cat \"$payload\")\n case \"$content\" in *\"topic commit\"*) exit 1;; esac\nfi\nexec " + shellQuote(sshKeygen) + " \"$@\"\n"
		if err := os.WriteFile(failingSigner, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		runGit(t, source, "config", "commit.gpgsign", "true")
		runGit(t, source, "config", "gpg.format", "ssh")
		runGit(t, source, "config", "user.signingkey", key)
		runGit(t, source, "config", "gpg.ssh.program", failingSigner)

		if _, rebaseErr := runGitAt(t.Context(), info.Path, "rebase", "--no-update-refs", "--no-autostash", "--no-rebase-merges", targetHead); rebaseErr == nil {
			t.Fatal("expected rebase with failing signer to pause")
		}
		gitDir := linkedWorktreeGitDir(t, info.Path)
		if _, statErr := os.Stat(filepath.Join(gitDir, "rebase-merge", "stopped-sha")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("expected rescheduled signing failure without stopped-sha, stat err = %v", statErr)
		}

		failingOwner, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := failingOwner.FinalizeNativeWork(t.Context(), info, issue, func(context.Context) error { return nil }); err == nil {
			t.Fatal("expected second FinalizeNativeWork with failing signer to fail")
		}

		runGit(t, source, "config", "gpg.ssh.program", sshKeygen)
		recoveredOwner, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
		if err != nil {
			t.Fatal(err)
		}
		writeCheckpointFile(t, info.Path, "extra.txt", "extra staged work\n")
		runGit(t, info.Path, "add", "extra.txt")
		if _, err := recoveredOwner.FinalizeNativeWork(t.Context(), info, issue, func(context.Context) error { return nil }); !errors.Is(err, ErrMergeResolutionInvalid) {
			t.Fatalf("FinalizeNativeWork() with extra staged file on rescheduled pick err = %v, want ErrMergeResolutionInvalid", err)
		}
		if extra, err := os.ReadFile(filepath.Join(info.Path, "extra.txt")); err != nil || string(extra) != "extra staged work\n" {
			t.Fatalf("extra.txt after rejection = (%q, %v), want preserved", string(extra), err)
		}
		runGit(t, info.Path, "rm", "-f", "extra.txt")

		if _, err := recoveredOwner.FinalizeNativeWork(t.Context(), info, issue, func(context.Context) error { return nil }); err != nil {
			t.Fatalf("FinalizeNativeWork() after two consecutive rescheduled signing failures error = %v", err)
		}
		got, err := os.ReadFile(filepath.Join(info.Path, "topic.txt"))
		if err != nil || string(got) != "signed work\n" {
			t.Fatalf("topic.txt = (%q, %v), want preserved", string(got), err)
		}
	})

	t.Run("rescheduled signing failure recovers when pick replaces tracked directory with tracked symlink", func(t *testing.T) {
		t.Parallel()
		sshKeygen, err := exec.LookPath("ssh-keygen")
		if err != nil {
			t.Skip("ssh-keygen unavailable")
		}
		source := initSourceRepo(t)
		if err := os.MkdirAll(filepath.Join(source, "config"), 0o700); err != nil {
			t.Fatal(err)
		}
		writeCheckpointFile(t, source, "config/app.env", "tracked-env\n")
		runGit(t, source, "add", "config/app.env")
		runGit(t, source, "commit", "-m", "track config dir")
		remote := initBareRemote(t)
		runGit(t, source, "remote", "add", "origin", remote)
		runGit(t, source, "push", "-u", "origin", "main")
		root := filepath.Join(t.TempDir(), "workspaces")
		owner, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
		if err != nil {
			t.Fatal(err)
		}
		issue := Issue{Identifier: "example#rescheduled-tracked-symlink", NativeRework: true, ProgressBaseRef: "main"}
		info, err := owner.Create(t.Context(), issue)
		if err != nil {
			t.Fatal(err)
		}
		runGit(t, info.Path, "rm", "config/app.env")
		if err := os.Symlink("alt-config", filepath.Join(info.Path, "config")); err != nil {
			t.Fatal(err)
		}
		runGit(t, info.Path, "add", "config")
		runGit(t, info.Path, "commit", "-m", "replace config dir with symlink")

		writeCheckpointFile(t, source, "upstream.txt", "upstream change\n")
		runGit(t, source, "add", "upstream.txt")
		runGit(t, source, "commit", "-m", "upstream commit")
		runGit(t, source, "push", "origin", "main")
		targetHead := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))

		key := filepath.Join(t.TempDir(), "signing-key")
		if out, err := exec.CommandContext(t.Context(), sshKeygen, "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen failed: %v: %s", err, out)
		}
		failingSigner := filepath.Join(t.TempDir(), "failing-signer")
		script := "#!/bin/sh\nfor arg do payload=$arg; done\nif test -f \"$payload\"; then\n content=$(cat \"$payload\")\n case \"$content\" in *\"replace config dir with symlink\"*) exit 1;; esac\nfi\nexec " + shellQuote(sshKeygen) + " \"$@\"\n"
		if err := os.WriteFile(failingSigner, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		runGit(t, source, "config", "commit.gpgsign", "true")
		runGit(t, source, "config", "gpg.format", "ssh")
		runGit(t, source, "config", "user.signingkey", key)
		runGit(t, source, "config", "gpg.ssh.program", failingSigner)

		if _, rebaseErr := runGitAt(t.Context(), info.Path, "rebase", "--no-update-refs", "--no-autostash", "--no-rebase-merges", targetHead); rebaseErr == nil {
			t.Fatal("expected rebase with failing signer to pause")
		}
		runGit(t, source, "config", "gpg.ssh.program", sshKeygen)
		recoveredOwner, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := recoveredOwner.FinalizeNativeWork(t.Context(), info, issue, func(context.Context) error { return nil }); err != nil {
			t.Fatalf("FinalizeNativeWork() with tracked symlink replacement error = %v", err)
		}
		gotTarget, err := os.Readlink(filepath.Join(info.Path, "config"))
		if err != nil || gotTarget != "alt-config" {
			t.Fatalf("config symlink after recovery = (%q, %v), want alt-config", gotTarget, err)
		}
	})

	t.Run("rescheduled signing failure recovers when pick replaces tracked file with tracked directory", func(t *testing.T) {
		t.Parallel()
		sshKeygen, err := exec.LookPath("ssh-keygen")
		if err != nil {
			t.Skip("ssh-keygen unavailable")
		}
		for _, tc := range []struct {
			name       string
			cleanIndex bool
		}{
			{name: "staged index"},
			{name: "clean index", cleanIndex: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				source := initSourceRepo(t)
				writeCheckpointFile(t, source, "config", "tracked-file\n")
				runGit(t, source, "add", "config")
				runGit(t, source, "commit", "-m", "track config file")
				remote := initBareRemote(t)
				runGit(t, source, "remote", "add", "origin", remote)
				runGit(t, source, "push", "-u", "origin", "main")
				root := filepath.Join(t.TempDir(), "workspaces")
				owner, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				issue := Issue{Identifier: "example#rescheduled-tracked-dir", NativeRework: true, ProgressBaseRef: "main"}
				info, err := owner.Create(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				runGit(t, info.Path, "rm", "config")
				if err := os.MkdirAll(filepath.Join(info.Path, "config"), 0o700); err != nil {
					t.Fatal(err)
				}
				writeCheckpointFile(t, info.Path, "config/app.env", "dir-env\n")
				runGit(t, info.Path, "add", "config/app.env")
				runGit(t, info.Path, "commit", "-m", "replace config file with dir")

				writeCheckpointFile(t, source, "upstream.txt", "upstream change\n")
				runGit(t, source, "add", "upstream.txt")
				runGit(t, source, "commit", "-m", "upstream commit")
				runGit(t, source, "push", "origin", "main")
				targetHead := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))

				key := filepath.Join(t.TempDir(), "signing-key")
				if out, err := exec.CommandContext(t.Context(), sshKeygen, "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
					t.Fatalf("ssh-keygen failed: %v: %s", err, out)
				}
				failingSigner := filepath.Join(t.TempDir(), "failing-signer")
				script := "#!/bin/sh\nfor arg do payload=$arg; done\nif test -f \"$payload\"; then\n content=$(cat \"$payload\")\n case \"$content\" in *\"replace config file with dir\"*) exit 1;; esac\nfi\nexec " + shellQuote(sshKeygen) + " \"$@\"\n"
				if err := os.WriteFile(failingSigner, []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				runGit(t, source, "config", "commit.gpgsign", "true")
				runGit(t, source, "config", "gpg.format", "ssh")
				runGit(t, source, "config", "user.signingkey", key)
				runGit(t, source, "config", "gpg.ssh.program", failingSigner)

				if _, rebaseErr := runGitAt(t.Context(), info.Path, "rebase", "--no-update-refs", "--no-autostash", "--no-rebase-merges", targetHead); rebaseErr == nil {
					t.Fatal("expected rebase with failing signer to pause")
				}
				if tc.cleanIndex {
					runGit(t, info.Path, "reset", "--hard", "HEAD")
				}
				runGit(t, source, "config", "gpg.ssh.program", sshKeygen)
				recoveredOwner, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := recoveredOwner.FinalizeNativeWork(t.Context(), info, issue, func(context.Context) error { return nil }); err != nil {
					t.Fatalf("FinalizeNativeWork() with tracked dir replacement error = %v", err)
				}
				got, err := os.ReadFile(filepath.Join(info.Path, "config", "app.env"))
				if err != nil || string(got) != "dir-env\n" {
					t.Fatalf("config/app.env after recovery = (%q, %v), want dir-env", string(got), err)
				}
			})
		}
	})

	t.Run("rescheduled signing failure with clean index refuses overwriting ignored destination file", func(t *testing.T) {
		t.Parallel()
		sshKeygen, err := exec.LookPath("ssh-keygen")
		if err != nil {
			t.Skip("ssh-keygen unavailable")
		}
		for _, tc := range []struct {
			name            string
			secondPick      bool
			modifyDelete    bool
			dirRename       bool
			disableDirRenam bool
			unionConflict   bool
		}{
			{name: "active pick adds ignored file"},
			{name: "later pick adds ignored file", secondPick: true},
			{name: "later pick modifies upstream-deleted file", secondPick: true, modifyDelete: true},
			{name: "later pick adds file into upstream-renamed directory", secondPick: true, dirRename: true},
			{name: "later pick with merge.directoryRenames=false protects old path", secondPick: true, dirRename: true, disableDirRenam: true},
			{name: "later pick after merge=union conflict protects ignored file", secondPick: true, unionConflict: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				source := initSourceRepo(t)
				ignorePattern := "report\n"
				if tc.dirRename {
					ignorePattern = "new/report\n"
					if tc.disableDirRenam {
						ignorePattern = "old/report\n"
					}
					if err := os.MkdirAll(filepath.Join(source, "old"), 0o700); err != nil {
						t.Fatal(err)
					}
					writeCheckpointFile(t, source, "old/a.txt", "a\n")
					writeCheckpointFile(t, source, "old/b.txt", "b\n")
					runGit(t, source, "add", "old")
				}
				if tc.unionConflict {
					writeCheckpointFile(t, source, "shared.txt", "base\n")
					runGit(t, source, "add", "shared.txt")
				}
				writeCheckpointFile(t, source, ".gitignore", ignorePattern)
				runGit(t, source, "add", ".gitignore")
				if tc.modifyDelete {
					writeCheckpointFile(t, source, "report", "base-report\n")
					runGit(t, source, "add", "-f", "report")
				}
				runGit(t, source, "commit", "-m", "ignore report")
				remote := initBareRemote(t)
				runGit(t, source, "remote", "add", "origin", remote)
				runGit(t, source, "push", "-u", "origin", "main")
				root := filepath.Join(t.TempDir(), "workspaces")
				owner, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				issue := Issue{Identifier: "example#rescheduled-clean-index", NativeRework: true, ProgressBaseRef: "main"}
				info, err := owner.Create(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				failSubject := "add report"
				if tc.secondPick {
					writeCheckpointFile(t, info.Path, "first.txt", "first-body\n")
					runGit(t, info.Path, "add", "first.txt")
					runGit(t, info.Path, "commit", "-m", "first pick")
					failSubject = "first pick"
				}
				if tc.unionConflict {
					writeCheckpointFile(t, info.Path, "shared.txt", "branch-line\n")
					runGit(t, info.Path, "add", "shared.txt")
					runGit(t, info.Path, "commit", "-m", "union conflict pick")
				}
				commitReportPath := "report"
				localReportPath := "report"
				if tc.dirRename {
					commitReportPath = "old/report"
					localReportPath = "new/report"
					if tc.disableDirRenam {
						localReportPath = "old/report"
					}
				}
				writeCheckpointFile(t, info.Path, commitReportPath, "report-body\n")
				runGit(t, info.Path, "add", "-f", commitReportPath)
				runGit(t, info.Path, "commit", "-m", "add report")

				if tc.modifyDelete {
					runGit(t, source, "rm", "report")
				}
				if tc.dirRename {
					runGit(t, source, "mv", "old", "new")
				}
				if tc.unionConflict {
					writeCheckpointFile(t, source, "shared.txt", "upstream-line\n")
					runGit(t, source, "add", "shared.txt")
				}
				if tc.disableDirRenam {
					runGit(t, source, "config", "merge.directoryRenames", "false")
				}
				writeCheckpointFile(t, source, "upstream.txt", "upstream change\n")
				runGit(t, source, "add", "upstream.txt")
				runGit(t, source, "commit", "-m", "upstream commit")
				runGit(t, source, "push", "origin", "main")
				targetHead := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
				if tc.unionConflict {
					if err := os.WriteFile(filepath.Join(source, ".git", "info", "attributes"), []byte("shared.txt merge=union\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}

				key := filepath.Join(t.TempDir(), "signing-key")
				if out, err := exec.CommandContext(t.Context(), sshKeygen, "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
					t.Fatalf("ssh-keygen failed: %v: %s", err, out)
				}
				failingSigner := filepath.Join(t.TempDir(), "failing-signer")
				script := "#!/bin/sh\nfor arg do payload=$arg; done\nif test -f \"$payload\"; then\n content=$(cat \"$payload\")\n case \"$content\" in *\"" + failSubject + "\"*) exit 1;; esac\nfi\nexec " + shellQuote(sshKeygen) + " \"$@\"\n"
				if err := os.WriteFile(failingSigner, []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				runGit(t, source, "config", "commit.gpgsign", "true")
				runGit(t, source, "config", "gpg.format", "ssh")
				runGit(t, source, "config", "user.signingkey", key)
				runGit(t, source, "config", "gpg.ssh.program", failingSigner)

				if _, rebaseErr := runGitAt(t.Context(), info.Path, "rebase", "--no-update-refs", "--no-autostash", "--no-rebase-merges", targetHead); rebaseErr == nil {
					t.Fatal("expected rebase with failing signer to pause")
				}
				runGit(t, info.Path, "reset", "--hard", "HEAD")
				if tc.disableDirRenam {
					if err := os.MkdirAll(filepath.Join(info.Path, "old"), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				writeCheckpointFile(t, info.Path, localReportPath, "private local edit\n")
				runGit(t, source, "config", "gpg.ssh.program", sshKeygen)
				recoveredOwner, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := recoveredOwner.FinalizeNativeWork(t.Context(), info, issue, func(context.Context) error { return nil }); !errors.Is(err, ErrMergeResolutionInvalid) {
					t.Fatalf("FinalizeNativeWork() with clean index and ignored %s err = %v, want ErrMergeResolutionInvalid", localReportPath, err)
				}
				got, err := os.ReadFile(filepath.Join(info.Path, localReportPath))
				if err != nil || string(got) != "private local edit\n" {
					t.Fatalf("%s after refusal = (%q, %v), want private local edit preserved", localReportPath, string(got), err)
				}
			})
		}
	})

	t.Run("rescheduled signing failure refuses reset that would overwrite ignored untracked file", func(t *testing.T) {
		t.Parallel()
		sshKeygen, err := exec.LookPath("ssh-keygen")
		if err != nil {
			t.Skip("ssh-keygen unavailable")
		}
		for _, tc := range []struct {
			name        string
			rename      bool
			symlink     bool
			literal     bool
			caseVariant bool
			hardLink    bool
		}{
			{name: "deleted tracked file"},
			{name: "renamed tracked file", rename: true},
			{name: "deleted file behind ignored symlink directory", symlink: true},
			{name: "deleted file with pathspec magic prefix", literal: true},
			{name: "deleted file recreated with different case", caseVariant: true},
			{name: "deleted file recreated as hard link to added file", hardLink: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				source := initSourceRepo(t)
				trackedPath := ".env"
				recreatedPath := ".env"
				ignoreEntry := ".env\n"
				if tc.symlink {
					trackedPath = "config/app.env"
					ignoreEntry = "config\n"
					if err := os.MkdirAll(filepath.Join(source, "config"), 0o700); err != nil {
						t.Fatal(err)
					}
				} else if tc.literal {
					trackedPath = ":notes"
					recreatedPath = ":notes"
					ignoreEntry = ":notes\n"
				} else if tc.caseVariant {
					probeDir := t.TempDir()
					if err := os.WriteFile(filepath.Join(probeDir, "caseprobe"), []byte("x"), 0o600); err != nil {
						t.Fatal(err)
					}
					if _, err := os.Lstat(filepath.Join(probeDir, "CASEPROBE")); err != nil {
						t.Skip("filesystem is case-sensitive")
					}
					trackedPath = "notes"
					recreatedPath = "NOTES"
					ignoreEntry = "NOTES\n"
				} else if tc.hardLink {
					trackedPath = "notes"
					recreatedPath = "notes"
					ignoreEntry = "notes\n"
				}
				writeCheckpointFile(t, source, trackedPath, "tracked-env\n")
				runGit(t, source, "--literal-pathspecs", "add", "--", trackedPath)
				runGit(t, source, "commit", "-m", "track env")
				remote := initBareRemote(t)
				runGit(t, source, "remote", "add", "origin", remote)
				runGit(t, source, "push", "-u", "origin", "main")
				root := filepath.Join(t.TempDir(), "workspaces")
				owner, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				issue := Issue{Identifier: "example#rescheduled-env", NativeRework: true, ProgressBaseRef: "main"}
				info, err := owner.Create(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				if tc.rename {
					runGit(t, info.Path, "mv", ".env", "example.env")
				} else {
					runGit(t, info.Path, "--literal-pathspecs", "rm", "--", trackedPath)
				}
				if tc.hardLink {
					writeCheckpointFile(t, info.Path, "report", "report-body\n")
					runGit(t, info.Path, "add", "report")
				}
				writeCheckpointFile(t, info.Path, ".gitignore", ignoreEntry)
				runGit(t, info.Path, "add", ".gitignore")
				runGit(t, info.Path, "commit", "-m", "untrack env")

				writeCheckpointFile(t, source, "upstream.txt", "upstream change\n")
				runGit(t, source, "add", "upstream.txt")
				runGit(t, source, "commit", "-m", "upstream commit")
				runGit(t, source, "push", "origin", "main")
				targetHead := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))

				key := filepath.Join(t.TempDir(), "signing-key")
				if out, err := exec.CommandContext(t.Context(), sshKeygen, "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
					t.Fatalf("ssh-keygen failed: %v: %s", err, out)
				}
				failingSigner := filepath.Join(t.TempDir(), "failing-signer")
				script := "#!/bin/sh\nfor arg do payload=$arg; done\nif test -f \"$payload\"; then\n content=$(cat \"$payload\")\n case \"$content\" in *\"untrack env\"*) exit 1;; esac\nfi\nexec " + shellQuote(sshKeygen) + " \"$@\"\n"
				if err := os.WriteFile(failingSigner, []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				runGit(t, source, "config", "commit.gpgsign", "true")
				runGit(t, source, "config", "gpg.format", "ssh")
				runGit(t, source, "config", "user.signingkey", key)
				runGit(t, source, "config", "gpg.ssh.program", failingSigner)

				if _, rebaseErr := runGitAt(t.Context(), info.Path, "rebase", "--no-update-refs", "--no-autostash", "--no-rebase-merges", targetHead); rebaseErr == nil {
					t.Fatal("expected rebase with failing signer to pause")
				}
				var symlinkTarget string
				wantBody := "local-secret\n"
				if tc.symlink {
					symlinkTarget = t.TempDir()
					if err := os.Symlink(symlinkTarget, filepath.Join(info.Path, "config")); err != nil {
						t.Fatal(err)
					}
				} else if tc.hardLink {
					if err := os.Link(filepath.Join(info.Path, "report"), filepath.Join(info.Path, recreatedPath)); err != nil {
						t.Fatal(err)
					}
					wantBody = "report-body\n"
				} else {
					writeCheckpointFile(t, info.Path, recreatedPath, wantBody)
				}
				runGit(t, source, "config", "gpg.ssh.program", sshKeygen)
				recoveredOwner, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := recoveredOwner.FinalizeNativeWork(t.Context(), info, issue, func(context.Context) error { return nil }); !errors.Is(err, ErrMergeResolutionInvalid) {
					t.Fatalf("FinalizeNativeWork() with obstructed reset err = %v, want ErrMergeResolutionInvalid", err)
				}
				if tc.symlink {
					gotTarget, err := os.Readlink(filepath.Join(info.Path, "config"))
					if err != nil || gotTarget != symlinkTarget {
						t.Fatalf("config symlink after refusal = (%q, %v), want %q preserved", gotTarget, err, symlinkTarget)
					}
				} else {
					gotEnv, err := os.ReadFile(filepath.Join(info.Path, recreatedPath))
					if err != nil || string(gotEnv) != wantBody {
						t.Fatalf("%s after refusal = (%q, %v), want %q preserved", recreatedPath, string(gotEnv), err, wantBody)
					}
				}
			})
		}
	})
}
