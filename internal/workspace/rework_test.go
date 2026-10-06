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
)

func TestLocalGitNativeReworkOwnsPausedRebase(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name           string
		inheritedPause bool
		unresolved     bool
		alter          string
		infrastructure bool
		clean          bool
		advanceTarget  bool
	}{
		{name: "runner prepares unsigned conflict"},
		{name: "inherited signed pause", inheritedPause: true},
		{name: "paused rebase retains integration base after target advances", advanceTarget: true},
		{name: "unresolved conflict", unresolved: true},
		{name: "wrong branch", alter: "head-name"},
		{name: "branch advanced", alter: "branch"},
		{name: "additional refs", alter: "update-refs"},
		{name: "executable todo", alter: "git-rebase-todo"},
		{name: "assigned ref unavailable", infrastructure: true},
		{name: "clean packed branch", clean: true},
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
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("feature\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, info.Path, "add", "README.md")
			runGit(t, info.Path, "commit", "-m", "feature")
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
			prepared, err := owner.PrepareRework(t.Context(), info, issue, MergePrepareOptions{})
			wantStatus := MergePrepareStatusConflict
			if test.clean {
				wantStatus = MergePrepareStatusClean
			}
			if err != nil || prepared.BaseSHA != base || prepared.Status != wantStatus || !test.clean && (len(prepared.ConflictPaths) != 1 || prepared.ConflictPaths[0] != "README.md") {
				t.Fatalf("prepare = %#v, %v", prepared, err)
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
			if test.advanceTarget {
				if err := os.WriteFile(filepath.Join(source, "UPSTREAM.md"), []byte("later target work\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, source, "add", "UPSTREAM.md")
				runGit(t, source, "-c", "commit.gpgsign=false", "commit", "-m", "later target work")
				runGit(t, source, "push", "origin", "main")
				runGit(t, source, "fetch", "origin")
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
				if finalBase != base {
					t.Fatalf("final baseline = %s, want prepared integration base %s", finalBase, base)
				}
				diff, err := GitFileDiffs(t.Context(), info.Path, finalBase, 1<<20)
				if err != nil || diff.BaseSHA != base || len(diff.Files) != 1 || diff.Files[0].Path != "README.md" {
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
				if got := readFile(t, filepath.Join(info.Path, "README.md")); got != wantContent {
					t.Fatalf("resolution = %q", got)
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

func TestLocalGitNativeWorkDisablesTrackedHooks(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name   string
		signed bool
		rework bool
	}{
		{name: "unsigned Code"},
		{name: "signed Code", signed: true},
		{name: "signed Rework", signed: true, rework: true},
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
			issue := Issue{Identifier: "native#156", NativeRework: test.rework, ProgressBaseRef: "main"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			hooks := []string{"pre-commit", "prepare-commit-msg", "commit-msg", "post-commit"}
			markers := t.TempDir()
			if err := os.Mkdir(filepath.Join(info.Path, ".githooks"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, hook := range hooks {
				script := fmt.Sprintf("#!/bin/sh\nprintf 'executed\\n' >> %q\n", filepath.Join(markers, hook))
				if err := os.WriteFile(filepath.Join(info.Path, ".githooks", hook), []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			runGit(t, source, "config", "core.hooksPath", ".githooks")
			runGit(t, info.Path, "add", ".githooks")
			runGit(t, info.Path, "commit", "-m", "test: install tracked hooks")
			for _, hook := range hooks {
				if readFile(t, filepath.Join(markers, hook)) != "executed\n" {
					t.Fatalf("hook fixture did not execute %s", hook)
				}
				if err := os.Remove(filepath.Join(markers, hook)); err != nil {
					t.Fatal(err)
				}
			}
			runGit(t, source, "config", "user.name", "Native Author")
			runGit(t, source, "config", "user.email", "native@example.com")
			if test.signed {
				signer := filepath.Join(t.TempDir(), "signing-key")
				if output, err := exec.CommandContext(t.Context(), "ssh-keygen", "-t", "ed25519", "-N", "", "-f", signer).CombinedOutput(); err != nil {
					t.Fatalf("create signing fixture: %v: %s", err, output)
				}
				public, err := os.ReadFile(signer + ".pub")
				if err != nil {
					t.Fatal(err)
				}
				allowed := filepath.Join(t.TempDir(), "allowed-signers")
				if err := os.WriteFile(allowed, append([]byte("native@example.com "), public...), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, source, "config", "commit.gpgsign", "true")
				runGit(t, source, "config", "gpg.format", "ssh")
				runGit(t, source, "config", "user.signingkey", signer)
				runGit(t, source, "config", "gpg.ssh.allowedSignersFile", allowed)
			}
			configPath := filepath.Join(source, ".git", "config")
			configBefore := readFile(t, configPath)
			before := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			if err := os.WriteFile(filepath.Join(info.Path, "repair.md"), []byte("repair\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, info.Path, "add", "repair.md")
			if _, err := backend.(*LocalGit).FinalizeNativeWork(t.Context(), info, issue, func(ctx context.Context) error { return ctx.Err() }); err != nil {
				t.Fatal(err)
			}
			for _, hook := range hooks {
				if _, err := os.Stat(filepath.Join(markers, hook)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("host finalization executed %s: %v", hook, err)
				}
			}
			head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			if head == before || strings.TrimSpace(runGit(t, info.Path, "status", "--porcelain")) != "" {
				t.Fatal("finalization did not commit the staged repair")
			}
			if test.signed {
				runGit(t, info.Path, "verify-commit", head)
			}
			if got := strings.TrimSpace(runGit(t, info.Path, "show", "-s", "--format=%an <%ae>", head)); got != "Native Author <native@example.com>" {
				t.Fatalf("finalization changed author: %s", got)
			}
			if got := strings.TrimSpace(runGit(t, source, "config", "core.hooksPath")); got != ".githooks" || readFile(t, configPath) != configBefore {
				t.Fatal("finalization changed repository hooks or signing configuration")
			}
		})
	}
}
