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
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
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
		authorityLost  bool
		unstaged       bool
		untracked      bool
		legacy         bool
	}{
		{name: "runner prepares unsigned conflict"},
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
				if _, _, err := owner.PushCheckpointRef(t.Context(), info, issue, CheckpointRef{}, func(ctx context.Context) error { return ctx.Err() }); !errors.Is(err, ErrCheckpointUnsafe) {
					t.Fatalf("unpublished legacy replay published without source authority: %v", err)
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
			prepared, err := owner.PrepareRework(t.Context(), info, issue, MergePrepareOptions{}, func(ctx context.Context) error { return ctx.Err() })
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
				if err != nil || diff.BaseSHA != selectedBase || len(diff.Files) != 1 || diff.Files[0].Path != "README.md" {
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

	for _, test := range []struct {
		name          string
		signed        bool
		rework        bool
		noIdentity    bool
		gpg           string
		signerLost    bool
		lossAt        string
		requireSigned bool
		indexLocked   bool
		gpgsign       string
		sshDefaultKey bool
		paused        bool
		landingMethod string
	}{
		{name: "unsigned Code"},
		{name: "signed Code", signed: true},
		{name: "signed Rework", signed: true, rework: true},
		{name: "signed SSH default key command", signed: true, sshDefaultKey: true},
		{name: "no identity or GPG", noIdentity: true},
		{name: "working OpenPGP", signed: true, gpg: "working"},
		{name: "OpenPGP unavailable after startup finalization", signed: true, gpg: "warm", signerLost: true},
		{name: "OpenPGP unavailable after startup checkpoint", signed: true, gpg: "warm", signerLost: true, lossAt: "checkpoint"},
		{name: "OpenPGP unavailable after startup paused rework", signed: true, rework: true, paused: true, gpg: "warm", signerLost: true},
		{name: "required signing refuses unsigned landing", signed: true, gpg: "warm", signerLost: true, requireSigned: true},
		{name: "index storage failure retains signed policy", signed: true, gpg: "working", indexLocked: true},
		{name: "working OpenPGP uppercase boolean", signed: true, gpg: "working", gpgsign: "TRUE"},
		{name: "working OpenPGP mixed-case boolean", signed: true, gpg: "working", gpgsign: "Yes"},
		{name: "unusable OpenPGP key", gpg: "missing"},
		{name: "locked OpenPGP key never prompts", gpg: "locked"},
		{name: "unresponsive signer is bounded", gpg: "stalled"},
		{name: "signed paused Rework", signed: true, rework: true, gpg: "working", paused: true},
		{name: "unusable key paused Rework", rework: true, gpg: "missing", paused: true},
		{name: "no identity paused Rework", rework: true, noIdentity: true, paused: true},
		{name: "signed merge landing", signed: true, gpg: "working", landingMethod: "merge"},
		{name: "signed replay landing", signed: true, gpg: "working", landingMethod: "rebase"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", home)
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			t.Setenv("GNUPGHOME", filepath.Join(home, "gnupg"))
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
			if test.paused {
				writeCheckpointFile(t, info.Path, "README.md", "feature conflict\n")
				runGit(t, info.Path, "add", "README.md")
			}
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
			if test.signed && test.gpg == "" {
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
				if test.sshDefaultKey {
					runGit(t, source, "config", "--unset", "user.signingkey")
					keyCommand := filepath.Join(filepath.Dir(signer), "default-key")
					if err := os.WriteFile(keyCommand, []byte("#!/bin/sh\nprintf 'key::'\ncat "+shellQuote(signer+".pub")+"\n"), 0o700); err != nil {
						t.Fatal(err)
					}
					runGit(t, source, "config", "gpg.ssh.defaultKeyCommand", shellQuote(keyCommand))
					program := filepath.Join(filepath.Dir(signer), "agent-signer")
					script := "#!/bin/sh\ncd " + shellQuote(filepath.Dir(signer)) + "\nexec ssh-agent -a \"signing-agent-$$\" sh -c 'ssh-add \"$1\" && shift && exec \"$@\"' sh " + shellQuote(signer) + " ssh-keygen \"$@\"\n"
					if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
						t.Fatal(err)
					}
					runGit(t, source, "config", "gpg.ssh.program", program)
					head := strings.TrimSpace(runGit(t, source, "commit-tree", "-S", "HEAD^{tree}", "-m", "test: verify SSH default key signing"))
					runGit(t, source, "verify-commit", head)
				}
			}
			if test.noIdentity {
				runGit(t, source, "config", "--unset", "user.name")
				runGit(t, source, "config", "--unset", "user.email")
				runGit(t, source, "config", "gpg.program", filepath.Join(home, "unavailable-gpg"))
			}
			pinentry := filepath.Join(home, "pinentry-called")
			if test.gpg != "" {
				configureGPGCommitFixture(t, source, test.gpg, pinentry)
			}
			if test.gpgsign != "" {
				runGit(t, source, "config", "commit.gpgsign", test.gpgsign)
			}
			probeStarted := time.Now()
			backend, err = NewBackend(KindLocalGit, LocalGitOptions{Root: filepath.Dir(info.Path), SourceRoot: source, AutoBranch: true, CommitIdentity: CommitIdentity{Name: "Enrolled runner", Email: "machine_fixture@runners.detent.invalid"}})
			if err != nil {
				t.Fatal(err)
			}
			if time.Since(probeStarted) > signingProbeTimeout+5*time.Second {
				t.Fatal("signing probe exceeded its bounded startup deadline")
			}
			owner := backend.(*LocalGit)
			report := owner.CommitReport()
			if (report.Signing != "unsigned") != test.signed {
				t.Fatalf("signing report = %+v", report)
			}
			if got := owner.Problems(); (len(got) > 0) == test.signed {
				t.Fatalf("signing problems = %+v", got)
			}
			loseSigner := func() {
				proof := strings.TrimSpace(runGit(t, info.Path, "commit-tree", "-S", "HEAD^{tree}", "-p", "HEAD", "-m", "test: startup signing"))
				assertRunnerCommitPolicy(t, info.Path, proof, "Native Author <native@example.com>", true)
				cmd := exec.CommandContext(t.Context(), "gpgconf", "--kill", "gpg-agent")
				cmd.Dir = os.Getenv("GNUPGHOME")
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("make isolated signer unavailable: %v: %s", err, output)
				}
				test.signed = false
			}
			if test.signerLost && test.lossAt == "" {
				loseSigner()
			}
			configPath := filepath.Join(source, ".git", "config")
			configBefore := readFile(t, configPath)
			if test.paused {
				writeCheckpointFile(t, source, "README.md", "upstream conflict\n")
				runGit(t, source, "add", "README.md")
				runGit(t, source, "-c", "user.name=Upstream", "-c", "user.email=upstream@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "advance upstream")
				runGit(t, source, "push", "origin", "main")
				prepared, err := owner.PrepareRework(t.Context(), info, issue, MergePrepareOptions{TargetBranch: "main"}, func(ctx context.Context) error { return ctx.Err() })
				if err != nil || prepared.Status != MergePrepareStatusConflict {
					t.Fatalf("prepare paused rework: %+v, %v", prepared, err)
				}
				checkpoint, changed, err := owner.PushCheckpointRef(t.Context(), info, issue, CheckpointRef{}, func(ctx context.Context) error { return ctx.Err() })
				if err != nil || !changed {
					t.Fatalf("paused rework checkpoint: changed=%v error=%v", changed, err)
				}
				identity := "Native Author <native@example.com>"
				if test.noIdentity {
					identity = "Enrolled runner <machine_fixture@runners.detent.invalid>"
				}
				assertRunnerCommitPolicy(t, info.Path, checkpoint.CommitSHA, identity, test.signed)
				writeCheckpointFile(t, info.Path, "README.md", "resolved conflict\n")
				runGit(t, info.Path, "add", "README.md")
			}
			before := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			if err := os.WriteFile(filepath.Join(info.Path, "repair.md"), []byte("repair\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, info.Path, "add", "repair.md")
			intendedTree := strings.TrimSpace(runGit(t, info.Path, "write-tree"))
			indexPath := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "--git-path", "index"))
			indexBefore := readFile(t, indexPath)
			if test.indexLocked {
				lock := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "--git-path", "index.lock"))
				if err := os.WriteFile(lock, []byte("retained lock\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := owner.FinalizeNativeWork(t.Context(), info, issue, func(ctx context.Context) error { return ctx.Err() }); err != nil {
				if !test.indexLocked {
					t.Fatalf("finalize native work: %v: %s", err, commandErrorOutput(err))
				}
				if strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")) != before || readFile(t, indexPath) != indexBefore || owner.CommitReport().Signing != "openpgp" || len(owner.Problems()) != 0 {
					t.Fatal("storage failure changed source or downgraded usable signing")
				}
				return
			} else if test.indexLocked {
				t.Fatal("finalization succeeded despite the index lock")
			}
			if !test.paused {
				for _, hook := range hooks {
					if _, err := os.Stat(filepath.Join(markers, hook)); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("host finalization executed %s: %v", hook, err)
					}
				}
			}
			head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			if head == before || strings.TrimSpace(runGit(t, info.Path, "status", "--porcelain")) != "" {
				t.Fatal("finalization did not commit the staged repair")
			}
			if test.signed {
				runGit(t, info.Path, "verify-commit", head)
			}
			expected := "Native Author <native@example.com>"
			if test.noIdentity {
				expected = "Enrolled runner <machine_fixture@runners.detent.invalid>"
			}
			author := expected
			if test.paused {
				author = "Test User <test@example.com>"
			}
			if got := strings.TrimSpace(runGit(t, info.Path, "show", "-s", "--format=%an <%ae>", head)); got != author {
				t.Fatalf("finalization changed author: %s", got)
			}
			assertRunnerCommitPolicy(t, info.Path, head, expected, test.signed)
			if test.signerLost && !test.paused && (strings.TrimSpace(runGit(t, info.Path, "rev-parse", head+"^{tree}")) != intendedTree || strings.TrimSpace(runGit(t, info.Path, "show", "-s", "--format=%P", head)) != before) {
				t.Fatal("signer loss changed finalization tree or parent")
			}
			if test.signerLost && test.lossAt == "checkpoint" {
				loseSigner()
			}
			if test.paused {
				if readFile(t, configPath) != configBefore {
					t.Fatal("paused rework changed host Git configuration")
				}
				if _, err := os.Stat(pinentry); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("paused rework invoked pinentry: %v", err)
				}
				return
			}
			owned := func(ctx context.Context) error { return ctx.Err() }
			writeCheckpointFile(t, info.Path, "checkpoint.md", "checkpoint work\n")
			if test.signerLost {
				runGit(t, info.Path, "add", "checkpoint.md")
				intendedTree = strings.TrimSpace(runGit(t, info.Path, "write-tree"))
			}
			ref, changed, err := owner.PushCheckpointRef(t.Context(), info, issue, CheckpointRef{}, owned)
			if err != nil || !changed {
				t.Fatalf("checkpoint ref: changed=%v error=%v", changed, err)
			}
			assertRunnerCommitPolicy(t, info.Path, ref.CommitSHA, expected, test.signed)
			if test.signerLost && (ref.TreeSHA != intendedTree || ref.HeadSHA != head || strings.TrimSpace(runGit(t, info.Path, "show", "-s", "--format=%P", ref.CommitSHA)) != head || owner.CommitReport().Signing != "unsigned" || len(owner.Problems()) == 0) {
				t.Fatal("signer loss changed checkpoint identity or omitted signing problem")
			}
			plan, err := owner.PrepareCheckpoint(t.Context(), info, issue)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err := owner.Checkpoint(t.Context(), plan, CheckpointSelection{HeadSHA: head, Paths: []string{"checkpoint.md", "repair.md", "README.md", ".githooks/pre-commit", ".githooks/prepare-commit-msg", ".githooks/commit-msg", ".githooks/post-commit"}, Reviewed: true}, owned, procgroup.Environment{Variables: map[string]string{"GIT_COMMITTER_NAME": "inherited worker", "GIT_COMMITTER_EMAIL": "inherited@example.com", "GNUPGHOME": filepath.Join(home, "unavailable-worker-keys"), "HOME": filepath.Join(home, "worker-home")}})
			if err != nil {
				t.Fatal(err)
			}
			assertRunnerCommitPolicy(t, info.Path, checkpoint, expected, test.signed)
			if test.signerLost && (strings.TrimSpace(runGit(t, info.Path, "rev-parse", checkpoint+"^{tree}")) != intendedTree || strings.TrimSpace(runGit(t, info.Path, "show", "-s", "--format=%P", checkpoint)) != head) {
				t.Fatal("signer loss changed checkpoint tree or parent")
			}
			if test.requireSigned {
				hook := "#!/bin/sh\nwhile read old new ref; do\n git verify-commit \"$new\" || exit 1\ndone\n"
				if err := os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte(hook), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			method := test.landingMethod
			if method == "" {
				method = "squash"
			}
			result, err := owner.LandChange(t.Context(), info, issue, LandOptions{HeadSHA: checkpoint, Method: method, Message: "Land fixture", TargetBranch: "main", Authorize: owned})
			if test.requireSigned {
				var refusal *LandRefusal
				if !errors.As(err, &refusal) || strings.TrimSpace(runGit(t, remote, "rev-parse", "main")) != strings.TrimSpace(runGit(t, source, "rev-parse", "main")) || strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")) != checkpoint {
					t.Fatalf("required signing did not refuse landing with retained source: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertRunnerCommitPolicy(t, source, result.MergeSHA, expected, test.signed)
			if _, err := os.Stat(pinentry); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("signing invoked pinentry: %v", err)
			}
			if test.signerLost && test.lossAt == "checkpoint" {
				warmGPGCommitFixture(t, os.Getenv("GNUPGHOME"))
				writeCheckpointFile(t, info.Path, "restored.md", "restored signing\n")
				runGit(t, info.Path, "add", "restored.md")
				if _, err := owner.FinalizeNativeWork(t.Context(), info, issue, owned); err != nil {
					t.Fatal(err)
				}
				restored := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
				assertRunnerCommitPolicy(t, info.Path, restored, expected, true)
				if owner.CommitReport().Signing != "openpgp" || len(owner.Problems()) != 0 {
					t.Fatal("restored usable signer remained downgraded")
				}
			}
			if got := strings.TrimSpace(runGit(t, source, "config", "core.hooksPath")); got != ".githooks" || readFile(t, configPath) != configBefore {
				t.Fatal("finalization changed repository hooks or signing configuration")
			}
		})
	}
}

func assertRunnerCommitPolicy(t *testing.T, path, head, identity string, signed bool) {
	t.Helper()
	if got := strings.TrimSpace(runGit(t, path, "show", "-s", "--format=%cn <%ce>", head)); got != identity {
		t.Fatalf("committer = %q, want %q", got, identity)
	}
	commit := runGit(t, path, "cat-file", "commit", head)
	if strings.Contains(commit, "\ngpgsig ") != signed {
		t.Fatalf("commit signature present = %v, want %v", strings.Contains(commit, "\ngpgsig "), signed)
	}
	if signed {
		runGit(t, path, "verify-commit", head)
	}
}

func configureGPGCommitFixture(t *testing.T, source, mode, pinentry string) {
	t.Helper()
	program, err := exec.LookPath("gpg")
	if err != nil {
		t.Fatal("GPG is required for the commit signing regression")
	}
	home := os.Getenv("GNUPGHOME")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"S.gpg-agent", "S.gpg-agent.extra", "S.gpg-agent.browser", "S.gpg-agent.ssh"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte("%Assuan%\nsocket="+name+".socket\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prompt := filepath.Join(home, "pinentry")
	if err := os.WriteFile(prompt, []byte("#!/bin/sh\nprintf called > "+shellQuote(pinentry)+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "gpg-agent.conf"), []byte("pinentry-program "+prompt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	signer := filepath.Join(home, "signer")
	script := "#!/bin/sh\ncd " + shellQuote(home) + "\nexec " + shellQuote(program) + " \"$@\"\n"
	if err := os.WriteFile(signer, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "gpgconf", "--kill", "gpg-agent")
		cmd.Dir = home
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("stop isolated signing agent: %v: %s", err, output)
		}
	}
	if mode == "stalled" {
		if err := os.WriteFile(signer, []byte("#!/bin/sh\nexec sleep 60\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "working" || mode == "locked" || mode == "warm" {
		passphrase := ""
		if mode == "locked" || mode == "warm" {
			passphrase = "fixture-passphrase"
		}
		cmd := exec.CommandContext(t.Context(), program, "--batch", "--pinentry-mode", "loopback", "--passphrase", passphrase, "--quick-generate-key", "Native Author <native@example.com>", "ed25519", "sign", "0")
		cmd.Dir = home
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generate isolated signing key: %v: %s", err, output)
		}
		stop()
	}
	if mode == "warm" {
		warmGPGCommitFixture(t, home)
	}
	t.Cleanup(stop)
	runGit(t, source, "config", "commit.gpgsign", "true")
	runGit(t, source, "config", "gpg.format", "openpgp")
	runGit(t, source, "config", "gpg.program", signer)
	key := "native@example.com"
	if mode == "missing" {
		key = "unavailable@example.com"
	}
	runGit(t, source, "config", "user.signingkey", key)
}

func warmGPGCommitFixture(t *testing.T, home string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), filepath.Join(home, "signer"), "--batch", "--yes", "--pinentry-mode", "loopback", "--passphrase", "fixture-passphrase", "--detach-sign", "--output", filepath.Join(home, "warm-signature"))
	cmd.Dir = home
	cmd.Stdin = strings.NewReader("warm isolated signer\n")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("warm isolated signer: %v: %s", err, output)
	}
}
