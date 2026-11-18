//go:build !windows

package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func makeFIFO(t *testing.T, path string) {
	t.Helper()
	if err := unix.Mkfifo(path, 0o666); err != nil {
		t.Fatal(err)
	}
}

func TestLocalGitCreateCreatesWorktreeBranchAndRunsAfterCreateHook(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	tracePath := filepath.Join(t.TempDir(), "after-create.trace")

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Hooks: Hooks{
			AfterCreate: "printf '%s|%s|%s|%s|%s|%s|%s|%s\n' \"$PWD\" \"$(git branch --show-current)\" \"$ISSUE_IDENTIFIER\" \"$WORKSPACE_KEY\" \"$BRANCH\" \"$DETENT_ISSUE_IDENTIFIER\" \"$DETENT_WORKSPACE_KEY\" \"$DETENT_BRANCH\" >> " + shellQuote(tracePath),
			Timeout:     hookCompletionTimeout(),
		},
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	info, err := backend.Create(context.Background(), Issue{ID: "issue-node", Identifier: "DD/19"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if !info.Created {
		t.Fatal("Create() Created = false, want true")
	}
	if info.Key != "DD_19" {
		t.Fatalf("Create() Key = %q, want DD_19", info.Key)
	}
	if filepath.Base(info.Path) != "DD_19" {
		t.Fatalf("Create() Path = %q, want basename DD_19", info.Path)
	}
	if info.Branch != "detent/dd_19" {
		t.Fatalf("Create() Branch = %q, want detent/dd_19", info.Branch)
	}
	if got := strings.TrimSpace(runGit(t, info.Path, "branch", "--show-current")); got != "detent/dd_19" {
		t.Fatalf("worktree branch = %q, want detent/dd_19", got)
	}
	if got := readFile(t, filepath.Join(info.Path, "README.md")); got != "source repo\n" {
		t.Fatalf("README.md = %q, want source repo", got)
	}

	trace := strings.TrimSpace(readFile(t, tracePath))
	fields := strings.Split(trace, "|")
	if len(fields) != 8 {
		t.Fatalf("after_create trace = %q, want eight fields", trace)
	}
	if fields[0] != info.Path {
		t.Fatalf("after_create cwd = %q, want %q", fields[0], info.Path)
	}
	if fields[1] != "detent/dd_19" {
		t.Fatalf("after_create branch = %q, want detent/dd_19", fields[1])
	}
	if fields[2] != "DD/19" {
		t.Fatalf("ISSUE_IDENTIFIER = %q, want DD/19", fields[2])
	}
	if fields[3] != "DD_19" {
		t.Fatalf("WORKSPACE_KEY = %q, want DD_19", fields[3])
	}
	if fields[4] != "detent/dd_19" {
		t.Fatalf("BRANCH = %q, want detent/dd_19", fields[4])
	}
	if fields[5] != "DD/19" {
		t.Fatalf("DETENT_ISSUE_IDENTIFIER = %q, want DD/19", fields[5])
	}
	if fields[6] != "DD_19" {
		t.Fatalf("DETENT_WORKSPACE_KEY = %q, want DD_19", fields[6])
	}
	if fields[7] != "detent/dd_19" {
		t.Fatalf("DETENT_BRANCH = %q, want detent/dd_19", fields[7])
	}
}

func TestLocalGitCreateSerializesAfterCreateHooksForSharedSource(t *testing.T) {
	t.Parallel()
	source := initSourceRepo(t)
	alternateSource := filepath.Join(t.TempDir(), "source-worktree")
	runGit(t, source, "worktree", "add", "--detach", alternateSource, "HEAD")
	hookState := t.TempDir()
	lockPath := filepath.Join(hookState, "hook.lock")
	startedPath := filepath.Join(hookState, "first.started")
	releasePath := filepath.Join(hookState, "first.release")
	hookCommand := "set -eu\n" +
		"mkdir " + shellQuote(lockPath) + "\n" +
		"if [ \"$ISSUE_IDENTIFIER\" = \"DD-FIRST\" ]; then\n" +
		"  : > " + shellQuote(startedPath) + "\n" +
		"  while [ ! -f " + shellQuote(releasePath) + " ]; do sleep 0.01; done\n" +
		"fi\n" +
		"rmdir " + shellQuote(lockPath)

	newBackend := func(sourceRoot string) *LocalGit {
		backend, err := NewLocalGit(LocalGitOptions{
			Root:       filepath.Join(t.TempDir(), "workspaces"),
			SourceRoot: sourceRoot,
			AutoBranch: true,
			Hooks: Hooks{
				AfterCreate: hookCommand,
				Timeout:     5 * time.Second,
			},
		})
		if err != nil {
			t.Fatalf("NewLocalGit() error = %v", err)
		}
		return backend
	}
	firstBackend := newBackend(source)
	secondBackend := newBackend(alternateSource)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	firstDone := make(chan error, 1)
	go func() {
		_, err := firstBackend.Create(ctx, Issue{Identifier: "DD-FIRST"})
		firstDone <- err
	}()
	waitForFile(t, startedPath, 5*time.Second)

	secondDone := make(chan error, 1)
	go func() {
		_, err := secondBackend.Create(ctx, Issue{Identifier: "DD-SECOND"})
		secondDone <- err
	}()

	premature := false
	var prematureErr error
	select {
	case prematureErr = <-secondDone:
		premature = true
	case <-time.After(500 * time.Millisecond):
	}
	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatalf("release first hook: %v", err)
	}
	if err := waitForError(t, firstDone, 5*time.Second); err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
	if premature {
		t.Fatalf("second Create() completed while first hook held the source lock: %v", prematureErr)
	}
	if err := waitForError(t, secondDone, 5*time.Second); err != nil {
		t.Fatalf("second Create() error = %v", err)
	}
}

func TestLocalGitCreateRunsAfterCreateHooksConcurrentlyForDifferentSources(t *testing.T) {
	t.Parallel()
	hookState := t.TempDir()
	startedPath := filepath.Join(hookState, "first.started")
	releasePath := filepath.Join(hookState, "first.release")
	hookCommand := "set -eu\n" +
		"if [ \"$ISSUE_IDENTIFIER\" = \"DD-FIRST\" ]; then\n" +
		"  : > " + shellQuote(startedPath) + "\n" +
		"  while [ ! -f " + shellQuote(releasePath) + " ]; do sleep 0.01; done\n" +
		"fi"
	newBackend := func(source string) *LocalGit {
		backend, err := NewLocalGit(LocalGitOptions{
			Root:       filepath.Join(t.TempDir(), "workspaces"),
			SourceRoot: source,
			AutoBranch: true,
			Hooks: Hooks{
				AfterCreate: hookCommand,
				Timeout:     5 * time.Second,
			},
		})
		if err != nil {
			t.Fatalf("NewLocalGit() error = %v", err)
		}
		return backend
	}
	firstBackend := newBackend(initSourceRepo(t))
	secondBackend := newBackend(initSourceRepo(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	firstDone := make(chan error, 1)
	go func() {
		_, err := firstBackend.Create(ctx, Issue{Identifier: "DD-FIRST"})
		firstDone <- err
	}()
	waitForFile(t, startedPath, 5*time.Second)

	secondDone := make(chan error, 1)
	go func() {
		_, err := secondBackend.Create(ctx, Issue{Identifier: "DD-SECOND"})
		secondDone <- err
	}()
	if err := waitForError(t, secondDone, 5*time.Second); err != nil {
		t.Fatalf("second Create() error = %v", err)
	}
	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatalf("release first hook: %v", err)
	}
	if err := waitForError(t, firstDone, 5*time.Second); err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
}

func TestLocalGitCreateBasesNewBranchOnFetchedRemoteDefault(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"new branch", "empty stale branch", "unique source", "active holder", "bound checkpoint", "Change version"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			issue := Issue{ID: "wi_remote_default", Identifier: "DD-REMOTE-DEFAULT"}
			branch := "detent/dd-remote-default"
			versionBase := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			if name == "Change version" {
				writeCheckpointFile(t, source, "version.txt", "selected version\n")
				runGit(t, source, "add", "version.txt")
				runGit(t, source, "commit", "-m", "selected version")
			}
			oldHead := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			var holder, retainedHead string
			if name != "new branch" {
				runGit(t, source, "branch", branch)
			}
			if name == "unique source" || name == "active holder" {
				holder = filepath.Join(t.TempDir(), "holder")
				runGit(t, source, "worktree", "add", holder, branch)
				writeCheckpointFile(t, holder, "retained.txt", "keep\n")
				if name == "unique source" {
					runGit(t, holder, "add", "retained.txt")
					runGit(t, holder, "commit", "-m", "retained source")
					retainedHead = strings.TrimSpace(runGit(t, holder, "rev-parse", "HEAD"))
					runGit(t, source, "worktree", "remove", holder)
				}
			}
			remote := initBareRemote(t)
			runGit(t, source, "remote", "add", "origin", remote)
			runGit(t, source, "push", "-u", "origin", "main")
			runGit(t, source, "switch", "-c", "dev")
			if err := os.WriteFile(filepath.Join(source, "dev.txt"), []byte("dev\n"), 0o600); err != nil {
				t.Fatalf("write dev file: %v", err)
			}
			runGit(t, source, "add", "dev.txt")
			runGit(t, source, "commit", "-m", "add dev branch")
			runGit(t, source, "push", "-u", "origin", "dev")
			runGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/dev")

			runGit(t, source, "switch", "-c", "local-feature", "main")
			if err := os.WriteFile(filepath.Join(source, "local-only.txt"), []byte("local only\n"), 0o600); err != nil {
				t.Fatalf("write local-only file: %v", err)
			}
			runGit(t, source, "add", "local-only.txt")
			runGit(t, source, "commit", "-m", "add local-only change")

			publisher := filepath.Join(t.TempDir(), "publisher")
			runGit(t, t.TempDir(), "clone", remote, publisher)
			runGit(t, publisher, "config", "user.name", "Test Publisher")
			runGit(t, publisher, "config", "user.email", "publisher@example.com")
			if err := os.WriteFile(filepath.Join(publisher, "remote-latest.txt"), []byte("remote latest\n"), 0o600); err != nil {
				t.Fatalf("write remote-latest file: %v", err)
			}
			writeCheckpointFile(t, publisher, "prepare-workspace.sh", "printf 'prepared\\n' > prepared.txt\n")
			runGit(t, publisher, "add", "remote-latest.txt", "prepare-workspace.sh")
			runGit(t, publisher, "commit", "-m", "advance remote dev")
			runGit(t, publisher, "push", "origin", "dev")
			wantHead := strings.TrimSpace(runGit(t, publisher, "rev-parse", "HEAD"))

			backend, err := NewLocalGit(LocalGitOptions{
				Root:       filepath.Join(t.TempDir(), "workspaces"),
				SourceRoot: source,
				AutoBranch: true,
				Hooks:      Hooks{AfterCreate: "sh ./prepare-workspace.sh", Timeout: hookCompletionTimeout()},
			})
			if err != nil {
				t.Fatalf("NewLocalGit() error = %v", err)
			}
			if name == "bound checkpoint" {
				holder = filepath.Join(t.TempDir(), "checkpoint")
				runGit(t, source, "worktree", "add", holder, branch)
				writeCheckpointFile(t, holder, "retained.txt", "keep\n")
				checkpoint, pushed, err := backend.PushCheckpointRef(t.Context(), Info{Path: holder, Branch: branch}, issue, CheckpointRef{}, func(ctx context.Context) error { return ctx.Err() })
				if err != nil || !pushed {
					t.Fatalf("PushCheckpointRef() = %v, pushed=%v", err, pushed)
				}
				issue.Checkpoint = &checkpoint
				runGit(t, source, "worktree", "remove", "--force", holder)
			}
			if name == "Change version" {
				repository := "https://example.com/example-org/example-project"
				runGit(t, source, "config", "remote.origin.url", repository)
				runGit(t, source, "config", "url."+remote+".insteadOf", repository)
				issue.FreshCheckout, issue.BaseRef = true, versionBase
				issue.Source = &ChangeSource{Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{
					BaseSHA: versionBase, HeadSHA: oldHead, Repository: repository,
				}}}
			}
			info, err := backend.Create(t.Context(), issue)
			if name == "bound checkpoint" || name == "Change version" {
				if !errors.Is(err, ErrHookFailed) {
					t.Fatalf("Create() = %v, want missing hook script failure", err)
				}
				if got := strings.TrimSpace(runGit(t, source, "rev-parse", branch)); got != oldHead {
					t.Fatalf("authoritative branch = %s, want %s", got, oldHead)
				}
				quarantines, err := filepath.Glob(filepath.Join(backend.root, ".detent", "quarantine", "*"))
				if err != nil || len(quarantines) != 1 {
					t.Fatalf("quarantines = %v, %v", quarantines, err)
				}
				if got := strings.TrimSpace(runGit(t, quarantines[0], "rev-parse", "HEAD")); got != oldHead {
					t.Fatalf("quarantined head = %s, want %s", got, oldHead)
				}
				if name == "Change version" {
					if got := readFile(t, filepath.Join(quarantines[0], "version.txt")); got != "selected version\n" {
						t.Fatalf("version source = %q", got)
					}
				}
				if name == "bound checkpoint" {
					if got := readFile(t, filepath.Join(quarantines[0], "retained.txt")); got != "keep\n" {
						t.Fatalf("checkpoint source = %q", got)
					}
					if got := runGit(t, quarantines[0], "status", "--porcelain"); got != "?? retained.txt\n" {
						t.Fatalf("checkpoint status = %q", got)
					}
				}
				return
			}
			if name == "active holder" {
				if err == nil {
					t.Fatal("Create() succeeded with an attached branch holder")
				}
				if got := strings.TrimSpace(runGit(t, source, "rev-parse", branch)); got != oldHead {
					t.Fatalf("held branch = %s, want %s", got, oldHead)
				}
				if got := strings.TrimSpace(runGit(t, holder, "branch", "--show-current")); got != branch {
					t.Fatalf("holder branch = %q, want %q", got, branch)
				}
				if got := readFile(t, filepath.Join(holder, "retained.txt")); got != "keep\n" {
					t.Fatalf("holder source = %q", got)
				}
				return
			}
			if name == "unique source" {
				if !errors.Is(err, ErrHookFailed) {
					t.Fatalf("Create() = %v, want missing hook script failure", err)
				}
				if got := strings.TrimSpace(runGit(t, source, "rev-parse", branch)); got != retainedHead {
					t.Fatalf("retained branch = %s, want %s", got, retainedHead)
				}
				if got := runGit(t, source, "show", branch+":retained.txt"); got != "keep\n" {
					t.Fatalf("retained source = %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}

			if got := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got != wantHead {
				t.Fatalf("worktree HEAD = %q, want remote default HEAD %q", got, wantHead)
			}
			if got := readFile(t, filepath.Join(info.Path, "prepared.txt")); got != "prepared\n" {
				t.Fatalf("hook output = %q, want prepared", got)
			}
			if got := readFile(t, filepath.Join(info.Path, "remote-latest.txt")); got != "remote latest\n" {
				t.Fatalf("remote-latest.txt = %q, want remote latest content", got)
			}
			if _, err := os.Stat(filepath.Join(info.Path, "local-only.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("local-only.txt stat error = %v, want file absent", err)
			}
		})
	}
}

func TestLocalGitCreateSerializesRemoteOperations(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	remote := initBareRemote(t)
	runGit(t, source, "remote", "add", "origin", remote)
	runGit(t, source, "push", "-u", "origin", "main")

	operationDir := t.TempDir()
	lockDir := filepath.Join(operationDir, "active")
	overlapPath := filepath.Join(operationDir, "overlap")
	uploadPackPath := filepath.Join(operationDir, "upload-pack")
	uploadPack := "#!/bin/sh\n" +
		"lock_dir=" + shellQuote(lockDir) + "\n" +
		"overlap_path=" + shellQuote(overlapPath) + "\n" +
		"owns_lock=\n" +
		"if mkdir \"$lock_dir\" 2>/dev/null; then\n" +
		"  owns_lock=1\n" +
		"else\n" +
		"  : > \"$overlap_path\"\n" +
		"fi\n" +
		"sleep 0.1\n" +
		"if [ -n \"$owns_lock\" ]; then\n" +
		"  rmdir \"$lock_dir\"\n" +
		"fi\n" +
		"exec git-upload-pack \"$@\"\n"
	if err := os.WriteFile(uploadPackPath, []byte(uploadPack), 0o700); err != nil {
		t.Fatalf("write upload-pack wrapper: %v", err)
	}
	runGit(t, source, "config", "remote.origin.uploadpack", uploadPackPath)

	backend, err := NewLocalGit(LocalGitOptions{
		Root:       filepath.Join(t.TempDir(), "workspaces"),
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewLocalGit() error = %v", err)
	}

	other, err := NewLocalGit(LocalGitOptions{
		Root:       filepath.Join(t.TempDir(), "workspaces"),
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewLocalGit() error = %v", err)
	}
	const creates = 6
	for i := 0; i < creates; i += 2 {
		runGit(t, source, "branch", fmt.Sprintf("detent/dd-concurrent-%d", i))
	}
	start := make(chan struct{})
	results := make(chan error, creates)
	for i := range creates {
		go func() {
			<-start
			creator := backend
			if i%2 == 0 {
				creator = other
			}
			_, err := creator.Create(context.Background(), Issue{Identifier: fmt.Sprintf("DD-CONCURRENT-%d", i)})
			results <- err
		}()
	}
	close(start)
	for range creates {
		if err := <-results; err != nil {
			t.Errorf("Create() error = %v", err)
		}
	}
	if _, err := os.Stat(overlapPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote operations overlapped, marker stat error = %v", err)
	}
}

func TestGitMetadataWritableRootsForLinkedWorktree(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	info, err := backend.Create(context.Background(), Issue{Identifier: "DD-GIT-ROOTS"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	roots, err := GitMetadataWritableRoots(context.Background(), info.Path)
	if err != nil {
		t.Fatalf("GitMetadataWritableRoots() error = %v", err)
	}

	commonDir := mustCanonicalExistingPath(t, filepath.Join(source, ".git"))
	wantRoots := []string{
		linkedWorktreeGitDir(t, info.Path),
		mustCanonicalExistingPath(t, filepath.Join(commonDir, "objects")),
		filepath.Join(commonDir, "refs", "heads", filepath.FromSlash(info.Branch)),
		filepath.Join(commonDir, "refs", "heads", filepath.FromSlash(info.Branch)) + ".lock",
		filepath.Join(commonDir, "logs", "refs", "heads", filepath.FromSlash(info.Branch)),
		filepath.Join(commonDir, "logs", "refs", "heads", filepath.FromSlash(info.Branch)) + ".lock",
	}
	for _, want := range wantRoots {
		if !containsString(roots, want) {
			t.Fatalf("GitMetadataWritableRoots() = %#v, missing %q", roots, want)
		}
	}
	if containsString(roots, commonDir) {
		t.Fatalf("GitMetadataWritableRoots() = %#v, should not allow entire common git dir %q", roots, commonDir)
	}
	for _, parent := range []string{filepath.Join(commonDir, "refs", "heads", "detent"), filepath.Join(commonDir, "logs", "refs", "heads", "detent")} {
		if containsString(roots, parent) {
			t.Fatalf("GitMetadataWritableRoots() grants sibling branch writes through %s", parent)
		}
	}

	if err := os.WriteFile(filepath.Join(info.Path, "agent.txt"), []byte("agent edit\n"), 0o600); err != nil {
		t.Fatalf("write agent edit: %v", err)
	}
	runGit(t, info.Path, "add", "agent.txt")
	runGit(t, info.Path, "commit", "-m", "agent commit")
	if got := strings.TrimSpace(runGit(t, info.Path, "log", "-1", "--pretty=%s")); got != "agent commit" {
		t.Fatalf("latest commit subject = %q, want agent commit", got)
	}
	runGit(t, source, "pack-refs", "--all")
	roots, err = GitMetadataWritableRoots(t.Context(), info.Path)
	if err != nil || len(roots) != len(wantRoots) {
		t.Fatalf("packed branch roots = %#v, %v", roots, err)
	}
	for _, root := range roots {
		if !containsString(wantRoots, root) {
			t.Fatalf("packed branch widened metadata authority: %s", root)
		}
	}
}

func TestGitMetadataWritableRootsRejectsEnclosingRepository(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	workspacePath := filepath.Join(source, "nested")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("mkdir nested workspace: %v", err)
	}

	if _, err := GitMetadataWritableRoots(context.Background(), workspacePath); err == nil {
		t.Fatal("GitMetadataWritableRoots() error = nil, want nested workspace rejection")
	}
}

func TestLocalGitHooksUseNonLoginShell(t *testing.T) {

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	tracePath := filepath.Join(t.TempDir(), "after-create.trace")
	argsPath := filepath.Join(t.TempDir(), "shell-args.trace")
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	shellPath := filepath.Join(binDir, "sh")
	shellScript := "#!/bin/sh\nprintf '%s\\n' \"$1\" > " + shellQuote(argsPath) + "\nexec /bin/sh \"$@\"\n"
	if err := os.WriteFile(shellPath, []byte(shellScript), 0o700); err != nil {
		t.Fatalf("write shell wrapper: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Hooks: Hooks{
			AfterCreate: "printf 'ok\n' > " + shellQuote(tracePath),
			Timeout:     time.Minute,
		},
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	if _, err := backend.Create(context.Background(), Issue{Identifier: "DD-SHELL"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if got := readFile(t, argsPath); got != "-c\n" {
		t.Fatalf("hook shell first arg = %q, want -c", got)
	}
	if got := readFile(t, tracePath); got != "ok\n" {
		t.Fatalf("hook trace = %q, want ok", got)
	}
}

func TestLocalGitHooksUseConfiguredShell(t *testing.T) {
	t.Parallel()
	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	tracePath := filepath.Join(t.TempDir(), "after-create.trace")
	argsPath := filepath.Join(t.TempDir(), "shell-args.trace")
	controlPath := filepath.Join(t.TempDir(), "shell.control")
	makeFIFO(t, controlPath)
	release, err := os.OpenFile(controlPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release.Close()
	shellPath := filepath.Join(t.TempDir(), "custom-sh")
	shellScript := "#!/bin/sh\nprintf '%s\\n' \"$0|$1|$2\" > " + shellQuote(argsPath) + "\nread -r release < " + shellQuote(controlPath) + "\nexec /bin/sh \"$@\"\n"
	if err := os.WriteFile(shellPath, []byte(shellScript), 0o700); err != nil {
		t.Fatalf("write shell wrapper: %v", err)
	}

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Hooks: Hooks{
			Shell:       shellPath,
			AfterCreate: "printf 'ok\n' > " + shellQuote(tracePath),
			Timeout:     testenv.SubprocessWaitTimeout,
		},
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	done := make(chan error, 1)
	joined := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		_ = release.Close()
		select {
		case <-joined:
		case <-time.After(testenv.SubprocessWaitTimeout):
			t.Error("configured shell fixture did not join during cleanup")
		}
	})
	go func() {
		defer close(joined)
		_, err := backend.Create(ctx, Issue{Identifier: "DD-SHELL-CFG"})
		done <- err
	}()
	waitForFile(t, argsPath, testenv.SubprocessWaitTimeout)
	if _, err := os.Stat(tracePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hook completed before configured shell release, stat error = %v", err)
	}
	if _, err := release.WriteString("release\n"); err != nil {
		t.Fatal(err)
	}
	if err := waitForError(t, done, testenv.SubprocessWaitTimeout); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	gotArgs := strings.TrimSpace(readFile(t, argsPath))
	wantArgs := shellPath + "|-c|" + "printf 'ok\n' > " + shellQuote(tracePath)
	if gotArgs != wantArgs {
		t.Fatalf("hook shell args = %q, want %q", gotArgs, wantArgs)
	}
	if got := readFile(t, tracePath); got != "ok\n" {
		t.Fatalf("hook trace = %q, want ok", got)
	}
}

func TestHookCancellationReapsDescendants(t *testing.T) {

	for _, kind := range []string{KindLocalGit, KindFilesystem} {
		for _, timeout := range []bool{false, true} {
			for _, resist := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/timeout=%t/resist=%t", kind, timeout, resist), func(t *testing.T) {
					root := t.TempDir()
					workspacePath := filepath.Join(root, "workspace")
					if err := os.Mkdir(workspacePath, 0o700); err != nil {
						t.Fatal(err)
					}
					neighbor := exec.CommandContext(t.Context(), "sleep", "30")
					neighbor.Dir = workspacePath
					procgroup.Configure(t.Context(), neighbor)
					if err := neighbor.Start(); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						_ = neighbor.Process.Kill()
						_ = neighbor.Wait()
					})
					neighborIdentity, err := procgroup.Inspect(neighbor)
					if err != nil {
						t.Fatal(err)
					}

					trap := ""
					if resist {
						trap = "trap '' TERM; "
					}
					grandchild := trap + "echo $$ > grandchild.pid; printf 'hook stdout\n'; printf 'hook stderr\n' >&2; : > grandchild.ready; " +
						"while [ ! -f release ]; do sleep 0.02; done; " +
						"printf late > completed; printf late > " + shellQuote(filepath.Join(workspacePath, "completed"))
					child := trap + "echo $$ > child.pid; sh -c " + shellQuote(grandchild) + "; wait"
					command := trap + "echo $$ > shell.pid; sh -c " + shellQuote(child) + "; wait"
					hooks := Hooks{Timeout: time.Minute}
					wantErr := context.Canceled
					if timeout {
						hooks.Timeout = time.Second
						wantErr = context.DeadlineExceeded
					}
					logger := slog.New(slog.NewTextHandler(io.Discard, nil))
					run := (&LocalGit{root: root, hooks: hooks, logger: logger}).runHook
					if kind == KindFilesystem {
						run = (&Filesystem{root: root, hooks: hooks, logger: logger}).runHook
					}
					ctx, cancel := context.WithCancel(t.Context())
					t.Cleanup(cancel)
					done := make(chan error, 1)
					go func() {
						done <- run(ctx, "before_run", command, Info{Path: workspacePath, Key: "DD-HOOK"}, Issue{Identifier: "DD-HOOK"})
					}()
					waitForFile(t, filepath.Join(workspacePath, "grandchild.ready"), 10*time.Second)
					identities := make([]procgroup.Identity, 0, 3)
					for _, role := range []string{"shell", "child", "grandchild"} {
						pidPath := filepath.Join(workspacePath, role+".pid")
						waitForFile(t, pidPath, 10*time.Second)
						pid, err := strconv.Atoi(strings.TrimSpace(readFile(t, pidPath)))
						if err != nil {
							t.Fatal(err)
						}
						process, err := os.FindProcess(pid)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _ = process.Kill() })
						identity, err := procgroup.Inspect(&exec.Cmd{Process: process})
						if err != nil {
							t.Fatalf("inspect %s: %v", role, err)
						}
						identities = append(identities, identity)
					}
					started := time.Now()
					if !timeout {
						cancel()
					}
					err = waitForError(t, done, 5*time.Second)
					if elapsed := time.Since(started); elapsed > 3*time.Second {
						t.Fatalf("hook shutdown took %s", elapsed)
					}
					var hookErr *HookError
					if !errors.As(err, &hookErr) || !errors.Is(err, wantErr) {
						t.Fatalf("runHook() error = %v, want HookError wrapping %v", err, wantErr)
					}
					if hookErr.ExitCode != -1 {
						t.Fatalf("hook exit code = %d, want signal exit", hookErr.ExitCode)
					}
					for _, want := range []string{"hook stdout", "hook stderr"} {
						if !strings.Contains(hookErr.Output, want) || !strings.Contains(readFile(t, hookErr.LogPath), want) {
							t.Fatalf("hook evidence missing %q: %v", want, hookErr)
						}
					}
					observations, err := procgroup.Observe(identities)
					if err != nil {
						t.Fatal(err)
					}
					for _, observation := range observations {
						if observation.Alive {
							t.Errorf("owned process group still running at hook return: %+v", observation)
						}
					}
					if alive, err := procgroup.Alive(neighborIdentity); err != nil || !alive {
						t.Fatalf("unrelated process alive = %t, error = %v", alive, err)
					}
					quarantinedPath := filepath.Join(root, "quarantined")
					if err := os.Rename(workspacePath, quarantinedPath); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(workspacePath, 0o700); err != nil {
						t.Fatal(err)
					}
					for _, path := range []string{workspacePath, quarantinedPath} {
						if err := os.WriteFile(filepath.Join(path, "release"), nil, 0o600); err != nil {
							t.Fatal(err)
						}
					}
					<-time.After(100 * time.Millisecond)
					for _, path := range []string{workspacePath, quarantinedPath} {
						if _, err := os.Stat(filepath.Join(path, "completed")); !errors.Is(err, os.ErrNotExist) {
							t.Errorf("hook wrote after workspace quarantine at %s: %v", path, err)
						}
					}
				})
			}
		}
	}
}

func TestHookWithExitedShell(t *testing.T) {

	for _, kind := range []string{KindLocalGit, KindFilesystem} {
		for _, mode := range []string{"success", "parent cancellation", "timeout"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				root := t.TempDir()
				hooks := Hooks{Timeout: time.Minute}
				if mode == "timeout" {
					hooks.Timeout = 500 * time.Millisecond
				}
				logger := slog.New(slog.NewTextHandler(io.Discard, nil))
				run := (&LocalGit{root: root, hooks: hooks, logger: logger}).runHook
				if kind == KindFilesystem {
					run = (&Filesystem{root: root, hooks: hooks, logger: logger}).runHook
				}
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				done := make(chan error, 1)
				go func() {
					done <- run(ctx, "before_run", "sleep 30 & echo $! > descendant.pid.tmp && mv descendant.pid.tmp descendant.pid", Info{Path: root, Key: "DD-HOOK"}, Issue{Identifier: "DD-HOOK"})
				}()
				pidPath := filepath.Join(root, "descendant.pid")
				waitForFile(t, pidPath, 10*time.Second)
				pid, err := strconv.Atoi(strings.TrimSpace(readFile(t, pidPath)))
				if err != nil {
					t.Fatal(err)
				}
				process, err := os.FindProcess(pid)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = process.Kill() })
				identity, err := procgroup.Inspect(&exec.Cmd{Process: process})
				if err != nil {
					t.Fatal(err)
				}
				if mode == "parent cancellation" {
					cancel()
				}
				err = waitForError(t, done, 3*time.Second)
				if mode == "success" {
					if err != nil {
						t.Fatalf("daemonized hook error = %v", err)
					}
					if alive, err := procgroup.Alive(identity); err != nil || !alive {
						t.Fatalf("successful background process alive = %t, error = %v", alive, err)
					}
					return
				}
				wantErr := context.Canceled
				if mode == "timeout" {
					wantErr = context.DeadlineExceeded
				}
				var hookErr *HookError
				if !errors.As(err, &hookErr) || !errors.Is(err, wantErr) {
					t.Fatalf("hook error = %v, want HookError wrapping %v", err, wantErr)
				}
				observations, err := procgroup.Observe([]procgroup.Identity{identity})
				if err != nil {
					t.Fatal(err)
				}
				if observations[0].Alive {
					t.Fatalf("descendant survived hook cancellation after shell exit: %+v", observations[0])
				}
			})
		}
	}
}

func TestLocalGitCreateReusesExistingWorktreeWithoutAfterCreate(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"existing", "native canonical", "legacy unpublished", "legacy recorded", "legacy wrong item", "legacy wrong branch", "legacy foreign source"} {
		t.Run(name, func(t *testing.T) {
			source := initSourceRepo(t)
			root := filepath.Join(t.TempDir(), "workspaces")
			tracePath := filepath.Join(t.TempDir(), "after-create.trace")
			backend, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true, Hooks: Hooks{
				AfterCreate: "printf 'after-create\n' >> " + shellQuote(tracePath), Timeout: hookCompletionTimeout(),
			}})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "DD-REUSE"}
			if name != "existing" {
				issue = Issue{ID: "wi_reuse", ProjectID: "local-alias", Identifier: "prj_example#1"}
			}
			var first Info
			if strings.HasPrefix(name, "legacy") {
				key := legacyIssueKey(issue)
				first = Info{Path: filepath.Join(root, key), Key: key, Branch: autoBranchPrefix + strings.ToLower(key)}
				origin := source
				if name == "legacy foreign source" {
					origin = initSourceRepo(t)
				}
				runGit(t, origin, "worktree", "add", "-b", first.Branch, first.Path, "HEAD")
				if err := backend.runHook(t.Context(), "after_create", backend.hooks.AfterCreate, first, issue); err != nil {
					t.Fatal(err)
				}
				if name == "legacy recorded" || name == "legacy wrong item" {
					recorded := issue
					if name == "legacy wrong item" {
						recorded.ID = "wi_other"
					}
					if err := backend.recordCleanupOwnership(t.Context(), first, recorded, true); err != nil {
						t.Fatal(err)
					}
				}
				if name == "legacy wrong branch" {
					runGit(t, first.Path, "checkout", "-b", "unrelated")
				}
			} else {
				first, err = backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
			}
			writeCheckpointFile(t, first.Path, "local-progress.txt", "keep\n")
			existing, existingErr := backend.Existing(issue)
			second, err := backend.Create(t.Context(), issue)
			if strings.HasPrefix(name, "legacy") {
				if !errors.Is(existingErr, ErrMissingWorkspace) {
					t.Fatalf("legacy cache was discovered as authoritative source: %#v, %v", existing, existingErr)
				}
				if err != nil || !second.Created || second.Path == first.Path || second.Key != issueKey(issue) {
					t.Fatalf("canonical workspace not created: %#v, %v", second, err)
				}
				if _, err := os.Stat(filepath.Join(second.Path, "local-progress.txt")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unpublished legacy progress adopted: %v", err)
				}
			} else if err != nil || existingErr != nil || existing.Path != first.Path || second.Created || second.Path != first.Path || second.Branch != first.Branch {
				t.Fatalf("workspace not reused: %#v, %#v, %v, %v", existing, second, existingErr, err)
			}
			if got := readFile(t, filepath.Join(first.Path, "local-progress.txt")); got != "keep\n" {
				t.Fatalf("local progress = %q", got)
			}
			wantHooks := 1
			if strings.HasPrefix(name, "legacy") {
				wantHooks = 2
			}
			if got := strings.Count(readFile(t, tracePath), "after-create"); got != wantHooks {
				t.Fatalf("after_create runs = %d, want %d", got, wantHooks)
			}
		})
	}
}

func TestLocalGitCreateRecoversCleanDetachedWorktree(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	tracePath := filepath.Join(t.TempDir(), "after-create.trace")

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Hooks: Hooks{
			AfterCreate: "printf 'after-create\n' >> " + shellQuote(tracePath),
			Timeout:     hookCompletionTimeout(),
		},
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	first, err := backend.Create(context.Background(), Issue{Identifier: "DD-DETACHED"})
	if err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
	runGit(t, first.Path, "switch", "--detach", "HEAD")

	second, err := backend.Create(context.Background(), Issue{Identifier: "DD-DETACHED"})
	if err != nil {
		t.Fatalf("second Create() error = %v", err)
	}

	if !second.Created {
		t.Fatal("second Create() Created = false, want true")
	}
	if second.Path != first.Path {
		t.Fatalf("second Create() Path = %q, want %q", second.Path, first.Path)
	}
	if got := strings.TrimSpace(runGit(t, second.Path, "branch", "--show-current")); got != "detent/dd-detached" {
		t.Fatalf("worktree branch = %q, want detent/dd-detached", got)
	}
	if got := strings.Count(readFile(t, tracePath), "after-create"); got != 2 {
		t.Fatalf("after_create runs = %d, want 2", got)
	}
}

func TestLocalGitCreateRecoversCleanWrongBranchWorktree(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	tracePath := filepath.Join(t.TempDir(), "after-create.trace")

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Hooks: Hooks{
			AfterCreate: "printf 'after-create\n' >> " + shellQuote(tracePath),
			Timeout:     hookCompletionTimeout(),
		},
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	info, err := backend.Create(context.Background(), Issue{Identifier: "DD-REUSE"})
	if err != nil {
		t.Fatalf("first Create() error = %v", err)
	}

	runGit(t, source, "branch", "detent/other")
	runGit(t, info.Path, "switch", "detent/other")

	second, err := backend.Create(context.Background(), Issue{Identifier: "DD-REUSE"})
	if err != nil {
		t.Fatalf("second Create() error = %v", err)
	}

	if !second.Created {
		t.Fatal("second Create() Created = false, want true")
	}
	if second.Path != info.Path {
		t.Fatalf("second Create() Path = %q, want %q", second.Path, info.Path)
	}
	if got := strings.TrimSpace(runGit(t, second.Path, "branch", "--show-current")); got != "detent/dd-reuse" {
		t.Fatalf("worktree branch = %q, want detent/dd-reuse", got)
	}
	if got := strings.Count(readFile(t, tracePath), "after-create"); got != 2 {
		t.Fatalf("after_create runs = %d, want 2", got)
	}
}

func TestLocalGitCreateQuarantinesDirtyDetachedWorktree(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	var logs strings.Builder

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	first, err := backend.Create(context.Background(), Issue{Identifier: "DD-DIRTY-DETACHED"})
	if err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
	runGit(t, first.Path, "switch", "--detach", "HEAD")
	if err := os.WriteFile(filepath.Join(first.Path, "local-progress.txt"), []byte("keep\n"), 0o600); err != nil {
		t.Fatalf("write local progress: %v", err)
	}

	second, err := backend.Create(context.Background(), Issue{Identifier: "DD-DIRTY-DETACHED"})
	if err != nil {
		t.Fatalf("second Create() error = %v", err)
	}

	if !second.Created {
		t.Fatal("second Create() Created = false, want true")
	}
	if second.Path != first.Path {
		t.Fatalf("second Create() Path = %q, want %q", second.Path, first.Path)
	}
	if got := strings.TrimSpace(runGit(t, second.Path, "branch", "--show-current")); got != "detent/dd-dirty-detached" {
		t.Fatalf("worktree branch = %q, want detent/dd-dirty-detached", got)
	}
	if _, err := os.Stat(filepath.Join(second.Path, "local-progress.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local progress exists in fresh workspace, stat error = %v", err)
	}

	quarantineDir := filepath.Join(root, ".detent", "quarantine")
	entries, err := os.ReadDir(quarantineDir)
	if err != nil {
		t.Fatalf("read quarantine dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("quarantine entries = %d, want 1", len(entries))
	}
	quarantinedPath := filepath.Join(quarantineDir, entries[0].Name())
	if got := readFile(t, filepath.Join(quarantinedPath, "local-progress.txt")); got != "keep\n" {
		t.Fatalf("quarantined local-progress.txt = %q, want keep", got)
	}
	if got := strings.TrimSpace(runGit(t, quarantinedPath, "branch", "--show-current")); got != "" {
		t.Fatalf("quarantined worktree branch = %q, want detached HEAD", got)
	}
	if got := logs.String(); !strings.Contains(got, "quarantined stale workspace") || !strings.Contains(got, quarantinedPath) {
		t.Fatalf("logs = %q, want quarantine report for %s", got, quarantinedPath)
	}
}

func TestLocalGitCreateQuarantinesCleanDetachedUnreferencedCommit(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	var logs strings.Builder

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	first, err := backend.Create(context.Background(), Issue{Identifier: "DD-DETACHED-COMMIT"})
	if err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
	runGit(t, first.Path, "switch", "--detach", "HEAD")
	if err := os.WriteFile(filepath.Join(first.Path, "local-commit.txt"), []byte("keep\n"), 0o600); err != nil {
		t.Fatalf("write local commit file: %v", err)
	}
	runGit(t, first.Path, "add", "local-commit.txt")
	runGit(t, first.Path, "commit", "-m", "local detached commit")
	if got := strings.TrimSpace(runGit(t, first.Path, "status", "--porcelain")); got != "" {
		t.Fatalf("detached worktree status = %q, want clean", got)
	}
	if got := strings.TrimSpace(runGit(t, first.Path, "branch", "--show-current")); got != "" {
		t.Fatalf("detached worktree branch = %q, want detached HEAD", got)
	}

	second, err := backend.Create(context.Background(), Issue{Identifier: "DD-DETACHED-COMMIT"})
	if err != nil {
		t.Fatalf("second Create() error = %v", err)
	}

	if !second.Created {
		t.Fatal("second Create() Created = false, want true")
	}
	if got := strings.TrimSpace(runGit(t, second.Path, "branch", "--show-current")); got != "detent/dd-detached-commit" {
		t.Fatalf("worktree branch = %q, want detent/dd-detached-commit", got)
	}
	if _, err := os.Stat(filepath.Join(second.Path, "local-commit.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local commit file exists in fresh workspace, stat error = %v", err)
	}

	quarantineDir := filepath.Join(root, ".detent", "quarantine")
	entries, err := os.ReadDir(quarantineDir)
	if err != nil {
		t.Fatalf("read quarantine dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("quarantine entries = %d, want 1", len(entries))
	}
	quarantinedPath := filepath.Join(quarantineDir, entries[0].Name())
	if got := readFile(t, filepath.Join(quarantinedPath, "local-commit.txt")); got != "keep\n" {
		t.Fatalf("quarantined local-commit.txt = %q, want keep", got)
	}
	if got := strings.TrimSpace(runGit(t, quarantinedPath, "branch", "--show-current")); got != "" {
		t.Fatalf("quarantined worktree branch = %q, want detached HEAD", got)
	}
	if got := logs.String(); !strings.Contains(got, "quarantined stale workspace") || !strings.Contains(got, quarantinedPath) {
		t.Fatalf("logs = %q, want quarantine report for %s", got, quarantinedPath)
	}
}

func TestLocalGitCreateRepairsWorkspacePreparationFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "quarantine releases canonical admin name",
			run: func(t *testing.T) {
				source := initSourceRepo(t)
				root := filepath.Join(t.TempDir(), "workspaces")
				backend, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatalf("NewLocalGit() error = %v", err)
				}

				issue := Issue{Identifier: "DD-ADMIN-NAME"}
				first, err := backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatalf("first Create() error = %v", err)
				}
				runGit(t, first.Path, "switch", "--detach", "HEAD")
				if err := os.WriteFile(filepath.Join(first.Path, "local-progress.txt"), []byte("keep\n"), 0o600); err != nil {
					t.Fatalf("write local progress: %v", err)
				}

				second, err := backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatalf("second Create() error = %v", err)
				}
				entries, err := os.ReadDir(filepath.Join(root, ".detent", "quarantine"))
				if err != nil || len(entries) != 1 {
					t.Fatalf("quarantine entries = %d, error = %v, want one", len(entries), err)
				}
				quarantinedPath := filepath.Join(root, ".detent", "quarantine", entries[0].Name())
				if got, want := filepath.Base(linkedWorktreeGitDir(t, second.Path)), filepath.Base(second.Path); got != want {
					t.Fatalf("replacement admin name = %q, want %q", got, want)
				}
				if got, want := filepath.Base(linkedWorktreeGitDir(t, quarantinedPath)), filepath.Base(quarantinedPath); got != want {
					t.Fatalf("quarantine admin name = %q, want %q", got, want)
				}
			},
		},
		{
			name: "dangling gitdir retains unverifiable source",
			run: func(t *testing.T) {
				for _, pending := range []bool{false, true} {
					name := "clean but unverifiable"
					if pending {
						name = "pending source refuses replacement"
					}
					t.Run(name, func(t *testing.T) {
						source := initSourceRepo(t)
						root := filepath.Join(t.TempDir(), "workspaces")
						backend, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
						if err != nil {
							t.Fatal(err)
						}
						issue := Issue{Identifier: "DD-DANGLING-GITDIR"}
						first, err := backend.Create(t.Context(), issue)
						if err != nil {
							t.Fatal(err)
						}
						adminDir := linkedWorktreeGitDir(t, first.Path)
						wantFiles := map[string]string{"README.md": "source repo\n"}
						if pending {
							writeFileDiffFile(t, first.Path, "README.md", "staged source B\n")
							runGit(t, first.Path, "add", "README.md")
							writeFileDiffFile(t, first.Path, "README.md", "pending working source\n")
							writeFileDiffFile(t, first.Path, "pending.go", "untracked sentinel\n")
							wantFiles["README.md"] = "pending working source\n"
							wantFiles["pending.go"] = "untracked sentinel\n"
						}
						gitPointer, err := os.ReadFile(filepath.Join(first.Path, ".git"))
						if err != nil {
							t.Fatal(err)
						}
						wantFiles[".git"] = string(gitPointer)
						orphanedAdminDir := adminDir + "-orphaned"
						var orphanedIndex []byte
						if pending {
							if err := os.RemoveAll(adminDir); err != nil {
								t.Fatal(err)
							}
						} else {
							orphanedIndex, err = os.ReadFile(filepath.Join(adminDir, "index"))
							if err != nil {
								t.Fatal(err)
							}
							if err := os.Rename(adminDir, orphanedAdminDir); err != nil {
								t.Fatal(err)
							}
						}
						second, createErr := backend.Create(t.Context(), issue)
						if !errors.Is(createErr, ErrWorkspacePreserved) || second.Created {
							t.Errorf("missing admin metadata did not refuse replacement: created=%t error=%v", second.Created, createErr)
						}
						for path, want := range wantFiles {
							got, readErr := os.ReadFile(filepath.Join(first.Path, path))
							if readErr != nil || string(got) != want {
								t.Errorf("replacement discarded surviving %s: got=%q want=%q error=%v", path, got, want, readErr)
							}
						}
						if !pending {
							got, readErr := os.ReadFile(filepath.Join(orphanedAdminDir, "index"))
							if readErr != nil || !slices.Equal(got, orphanedIndex) {
								t.Errorf("preparation discarded available orphaned index: %v", readErr)
							}
						}
					})
				}
			},
		},
		{
			name: "failed clean removal leaves recreatable path",
			run: func(t *testing.T) {
				source := initSourceRepo(t)
				root := filepath.Join(t.TempDir(), "workspaces")
				backend, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatalf("NewLocalGit() error = %v", err)
				}

				issue := Issue{Identifier: "DD-PARTIAL-REMOVE"}
				first, err := backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatalf("first Create() error = %v", err)
				}
				runGit(t, first.Path, "branch", "detent/other-partial-remove", "HEAD")
				runGit(t, first.Path, "switch", "detent/other-partial-remove")
				lockedDir := filepath.Join(first.Path, "locked")
				if err := os.MkdirAll(lockedDir, 0o700); err != nil {
					t.Fatalf("create locked directory: %v", err)
				}
				if err := os.WriteFile(filepath.Join(lockedDir, "tracked.txt"), []byte("tracked\n"), 0o600); err != nil {
					t.Fatalf("write tracked file: %v", err)
				}
				runGit(t, first.Path, "add", "locked/tracked.txt")
				runGit(t, first.Path, "commit", "-m", "add locked file")
				if err := os.Chmod(lockedDir, 0o500); err != nil {
					t.Fatalf("make tracked directory read-only: %v", err)
				}
				t.Cleanup(func() { restoreWritableTree(t, first.Path) })

				second, err := backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatalf("second Create() error = %v", err)
				}
				if !second.Created {
					t.Fatal("second Create() Created = false, want true")
				}
				if _, err := os.Stat(filepath.Join(second.Path, "locked")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stale locked directory stat error = %v, want absent", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestLocalGitBeforeAndAfterRunHooks(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	tracePath := filepath.Join(t.TempDir(), "run-hooks.trace")

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Hooks: Hooks{
			BeforeRun: "printf 'before:%s:%s\n' \"$PWD\" \"$WORKSPACE_KEY\" >> " + shellQuote(tracePath),
			AfterRun:  "printf 'after:%s:%s\n' \"$PWD\" \"$WORKSPACE_KEY\" >> " + shellQuote(tracePath),
			Timeout:   hookCompletionTimeout(),
		},
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	info, err := backend.Create(context.Background(), Issue{Identifier: "DD-RUN"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := backend.BeforeRun(context.Background(), info, Issue{Identifier: "DD-RUN"}); err != nil {
		t.Fatalf("BeforeRun() error = %v", err)
	}
	backend.AfterRun(context.Background(), info, Issue{Identifier: "DD-RUN"})

	want := "before:" + info.Path + ":DD-RUN\nafter:" + info.Path + ":DD-RUN\n"
	if got := readFile(t, tracePath); got != want {
		t.Fatalf("hook trace = %q, want %q", got, want)
	}
}

func TestLocalGitHookFailureSurfaces(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	hookCommand := "printf 'out\\n'; printf 'err\\n' >&2; exit 17"

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Hooks: Hooks{
			AfterCreate: hookCommand,
			Timeout:     hookCompletionTimeout(),
		},
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	_, err = backend.Create(context.Background(), Issue{Identifier: "DD-FAIL"})
	if err == nil {
		t.Fatal("Create() error = nil, want hook error")
	}
	var hookErr *HookError
	if !errors.As(err, &hookErr) {
		t.Fatalf("Create() error = %T, want *HookError", err)
	}
	if hookErr.Hook != "after_create" {
		t.Fatalf("HookError.Hook = %q, want after_create", hookErr.Hook)
	}
	if hookErr.ExitCode != 17 {
		t.Fatalf("HookError.ExitCode = %d, want 17", hookErr.ExitCode)
	}
	for _, want := range []string{"out", "err"} {
		if !strings.Contains(hookErr.Output, want) {
			t.Fatalf("HookError.Output = %q, want %q", hookErr.Output, want)
		}
	}
	if hookErr.Command != hookCommand {
		t.Fatalf("HookError.Command = %q, want hook command", hookErr.Command)
	}
	if filepath.Base(hookErr.Dir) != "DD-FAIL" {
		t.Fatalf("HookError.Dir = %q, want DD-FAIL workspace", hookErr.Dir)
	}
	if hookErr.LogPath == "" {
		t.Fatal("HookError.LogPath is empty")
	}
	wantLogDir := filepath.Join(backend.(*LocalGit).root, ".detent", "hook-logs", "DD-FAIL")
	if !strings.HasPrefix(hookErr.LogPath, wantLogDir) {
		t.Fatalf("HookError.LogPath = %q, want under root hook logs", hookErr.LogPath)
	}
	errorDetail := err.Error()
	for _, want := range []string{
		fmt.Sprintf("command %q", hookCommand),
		"working directory",
		"exit status 17",
		"hook log",
		"output (last",
		"out",
		"err",
	} {
		if !strings.Contains(errorDetail, want) {
			t.Fatalf("Create() error = %q, want %q", errorDetail, want)
		}
	}
	logContent := readFile(t, hookErr.LogPath)
	for _, want := range []string{
		"hook: after_create\n",
		"command: " + hookCommand + "\n",
		"exit_status: 17\n",
		"output:\nout\nerr\n",
	} {
		if !strings.Contains(logContent, want) {
			t.Fatalf("hook log = %q, want %q", logContent, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(root, "DD-FAIL")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed after_create workspace exists, stat error = %v", statErr)
	}
	quarantinedPath := singleQuarantinedWorkspace(t, root)
	if got := readFile(t, filepath.Join(quarantinedPath, "README.md")); got != "source repo\n" {
		t.Fatalf("quarantined README.md = %q, want source repo", got)
	}
	if got := strings.TrimSpace(runGit(t, quarantinedPath, "branch", "--show-current")); got != "" {
		t.Fatalf("quarantined worktree branch = %q, want detached HEAD", got)
	}
	if got := runGit(t, source, "worktree", "list", "--porcelain"); !strings.Contains(got, quarantinedPath) {
		t.Fatalf("git worktree list does not contain quarantined path:\n%s", got)
	}

	retryBackend, retryErr := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
	if retryErr != nil {
		t.Fatalf("NewLocalGit() retry error = %v", retryErr)
	}
	retried, retryErr := retryBackend.Create(context.Background(), Issue{Identifier: "DD-FAIL"})
	if retryErr != nil {
		t.Fatalf("retry Create() error = %v", retryErr)
	}
	if !retried.Created {
		t.Fatal("retry Create() Created = false, want true")
	}
}

func TestLocalGitPreserveFailedWorkspaceReleasesBranch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		identifier string
		ctx        func() context.Context
		prepare    func(*testing.T, string, string)
		wantStatus string
	}{
		{
			name:       "canceled creation context",
			identifier: "DD-CANCELED-PRESERVE",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
		},
		{
			name:       "unmerged index",
			identifier: "DD-UNMERGED-PRESERVE",
			ctx:        context.Background,
			prepare: func(t *testing.T, source string, workspace string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("workspace\n"), 0o600); err != nil {
					t.Fatalf("write workspace README.md: %v", err)
				}
				runGit(t, workspace, "add", "README.md")
				runGit(t, workspace, "commit", "-m", "workspace change")
				if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("source\n"), 0o600); err != nil {
					t.Fatalf("write source README.md: %v", err)
				}
				runGit(t, source, "add", "README.md")
				runGit(t, source, "commit", "-m", "source change")

				cmd := exec.CommandContext(t.Context(), "git", "-C", workspace, "merge", "main")
				output, err := cmd.CombinedOutput()
				if err == nil {
					t.Fatalf("git merge succeeded, want conflict:\n%s", output)
				}
			},
			wantStatus: "UU README.md",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := initSourceRepo(t)
			root := filepath.Join(t.TempDir(), "workspaces")
			backend, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatalf("NewLocalGit() error = %v", err)
			}
			info, err := backend.Create(context.Background(), Issue{Identifier: tt.identifier})
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			if tt.prepare != nil {
				tt.prepare(t, source, info.Path)
			}

			cause := errors.New("synthetic workspace failure")
			err = backend.preserveFailedWorkspace(tt.ctx(), info.Path, cause)
			if !errors.Is(err, cause) {
				t.Fatalf("preserveFailedWorkspace() error = %v, want cause", err)
			}
			if _, statErr := os.Stat(info.Path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed workspace remains at original path, stat error = %v", statErr)
			}

			quarantinedPath := singleQuarantinedWorkspace(t, root)
			if got := runGit(t, source, "worktree", "list", "--porcelain"); !strings.Contains(got, quarantinedPath) || strings.Contains(got, info.Path+"\n") {
				t.Fatalf("git worktree list does not register only quarantined path:\n%s", got)
			}
			if got := strings.TrimSpace(runGit(t, quarantinedPath, "branch", "--show-current")); got != "" {
				t.Fatalf("quarantined worktree branch = %q, want detached HEAD", got)
			}
			if tt.wantStatus != "" {
				if got := runGit(t, quarantinedPath, "status", "--porcelain"); !strings.Contains(got, tt.wantStatus) {
					t.Fatalf("quarantined worktree status = %q, want %q", got, tt.wantStatus)
				}
			}

			retried, retryErr := backend.Create(context.Background(), Issue{Identifier: tt.identifier})
			if retryErr != nil {
				t.Fatalf("retry Create() error = %v", retryErr)
			}
			if !retried.Created {
				t.Fatal("retry Create() Created = false, want true")
			}
		})
	}
}

func TestLocalGitPreserveFailedWorkspaceSurfacesBranchReleaseFailure(t *testing.T) {

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	backend, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatalf("NewLocalGit() error = %v", err)
	}
	info, err := backend.Create(context.Background(), Issue{Identifier: "DD-DETACH-FAILURE"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	installGitUpdateRefFailureWrapper(t)

	cause := errors.New("synthetic workspace failure")
	err = backend.preserveFailedWorkspace(context.Background(), info.Path, cause)
	if !errors.Is(err, cause) {
		t.Fatalf("preserveFailedWorkspace() error = %v, want cause", err)
	}
	for _, want := range []string{"workspace quarantined at", "failed to release its branch", "synthetic update-ref failure"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("preserveFailedWorkspace() error = %q, want %q", err, want)
		}
	}
	if _, statErr := os.Stat(info.Path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed workspace remains at original path, stat error = %v", statErr)
	}

	quarantinedPath := singleQuarantinedWorkspace(t, root)
	if !strings.Contains(err.Error(), quarantinedPath) {
		t.Fatalf("preserveFailedWorkspace() error = %q, want quarantine path %q", err, quarantinedPath)
	}
	if got := strings.TrimSpace(runGit(t, quarantinedPath, "branch", "--show-current")); got != info.Branch {
		t.Fatalf("quarantined worktree branch = %q, want %q after release failure", got, info.Branch)
	}
}

func TestLocalGitCreateQuarantinesFailedCreationState(t *testing.T) {

	tests := []struct {
		name           string
		mode           string
		identifier     string
		wantError      []string
		wantStandalone bool
	}{
		{
			name:       "partial worktree add failure",
			mode:       "partial_failure",
			identifier: "DD-PARTIAL-CREATE",
			wantError: []string{
				"synthetic worktree add failure",
				"workspace quarantined at",
			},
		},
		{
			name:       "successful add yields standalone repository",
			mode:       "standalone_after_add",
			identifier: "DD-STANDALONE",
			wantError: []string{
				"workspace worktree invariant failed",
				"registered with source: false",
				"workspace quarantined at",
			},
			wantStandalone: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := initSourceRepo(t)
			root := filepath.Join(t.TempDir(), "workspaces")
			target := filepath.Join(root, tt.identifier)
			tracePath := filepath.Join(t.TempDir(), "after-create.trace")
			installGitCreationWrapper(t, tt.mode, source, target)

			backend, err := NewLocalGit(LocalGitOptions{
				Root:       root,
				SourceRoot: source,
				AutoBranch: true,
				Hooks: Hooks{
					AfterCreate: "printf 'ran\\n' > " + shellQuote(tracePath),
					Timeout:     hookCompletionTimeout(),
				},
			})
			if err != nil {
				t.Fatalf("NewLocalGit() error = %v", err)
			}

			_, err = backend.Create(context.Background(), Issue{Identifier: tt.identifier})
			if err == nil {
				t.Fatal("Create() error = nil, want creation failure")
			}
			if tt.wantStandalone && !errors.Is(err, ErrWorktreeInvariant) {
				t.Errorf("Create() error = %v, want ErrWorktreeInvariant", err)
			}
			for _, want := range tt.wantError {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Create() error = %q, want %q", err, want)
				}
			}
			if tt.wantStandalone {
				for _, want := range []string{filepath.Join(target, ".git"), filepath.Join(source, ".git")} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("Create() error = %q, want common dir %q", err, want)
					}
				}
			}
			if _, statErr := os.Stat(tracePath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("after_create hook ran, stat error = %v", statErr)
			}
			if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed workspace remains at original path, stat error = %v", statErr)
			}

			quarantinedPath := singleQuarantinedWorkspace(t, root)
			if got := readFile(t, filepath.Join(quarantinedPath, "forensics.txt")); got != tt.mode+"\n" {
				t.Fatalf("quarantined forensics.txt = %q, want %q", got, tt.mode)
			}
			gitInfo, statErr := os.Stat(filepath.Join(quarantinedPath, ".git"))
			switch {
			case tt.wantStandalone && statErr != nil:
				t.Fatalf("stat quarantined .git: %v", statErr)
			case tt.wantStandalone && !gitInfo.IsDir():
				t.Fatalf("quarantined .git mode = %v, want directory", gitInfo.Mode())
			case !tt.wantStandalone && !errors.Is(statErr, os.ErrNotExist):
				t.Fatalf("partial quarantine .git stat error = %v, want absent", statErr)
			}
		})
	}
}

func TestLocalGitCreateReleasesStaleBranchHolders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		fresh     bool
		rebasing  bool
		vanished  bool
		wantFiles bool
		recorded  bool
	}{
		{name: "active attached holder", wantFiles: true},
		{name: "fresh recorded head", fresh: true, recorded: true, wantFiles: true},
		{name: "attached holder before a fresh checkout", fresh: true, wantFiles: true},
		{name: "holder stopped mid-rebase", fresh: true, rebasing: true, wantFiles: true},
		{name: "holder directory deleted", fresh: true, vanished: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			source := initSourceRepo(t)
			root := filepath.Join(t.TempDir(), "workspaces")
			backend, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatalf("NewLocalGit() error = %v", err)
			}
			branch := "detent/dd-stale"
			holder := filepath.Join(t.TempDir(), "old-layout", "dd-stale")
			runGit(t, source, "worktree", "add", "-b", branch, holder, "HEAD")
			if err := os.WriteFile(filepath.Join(holder, "kept.txt"), []byte("kept\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, holder, "add", "kept.txt")
			runGit(t, holder, "commit", "-m", "earlier attempt")
			if tt.rebasing {
				runGit(t, holder, "-c", "sequence.editor=sed -i.bak s/^pick/edit/", "rebase", "-i", "HEAD~1")
			}
			if tt.vanished {
				if err := os.RemoveAll(holder); err != nil {
					t.Fatal(err)
				}
			}

			head := strings.TrimSpace(runGit(t, source, "rev-parse", branch))
			issue := Issue{Identifier: "DD-STALE", BranchName: branch, FreshCheckout: tt.fresh}
			if tt.recorded {
				issue.BaseRef = head
				runGit(t, source, "commit", "--allow-empty", "-m", "advance default")
			}
			info, err := backend.Create(t.Context(), issue)
			if !tt.fresh {
				if err == nil {
					t.Fatal("Create() succeeded with an attached branch holder")
				}
				if got := strings.TrimSpace(runGit(t, source, "rev-parse", branch)); got != head {
					t.Fatalf("held branch = %s, want %s", got, head)
				}
				if got := strings.TrimSpace(runGit(t, holder, "branch", "--show-current")); got != branch {
					t.Fatalf("holder branch = %q, want %q", got, branch)
				}
				if got := readFile(t, filepath.Join(holder, "kept.txt")); got != "kept\n" {
					t.Fatalf("holder source = %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Create() error = %v, want the stale holder released", err)
			}
			if got := strings.TrimSpace(runGit(t, info.Path, "branch", "--show-current")); got != branch {
				t.Fatalf("new worktree branch = %q, want %q", got, branch)
			}
			if tt.recorded {
				if got := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got != head {
					t.Fatalf("fresh head = %s, want recorded %s", got, head)
				}
			}
			if !tt.wantFiles {
				return
			}
			if got := readFile(t, filepath.Join(holder, "kept.txt")); got != "kept\n" {
				t.Fatalf("holder kept.txt = %q, want the earlier attempt's files kept", got)
			}
			if got := strings.TrimSpace(runGit(t, holder, "branch", "--show-current")); got != "" {
				t.Fatalf("holder branch = %q, want detached", got)
			}
			if _, err := os.Stat(filepath.Join(source, ".git", "worktrees", filepath.Base(holder), "rebase-merge")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("holder rebase state stat error = %v, want removed", err)
			}
		})
	}
}

func TestLocalGitCleanupRemovesOnlyTargetWorktree(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	publishCleanupSource(t, source)
	root := filepath.Join(t.TempDir(), "workspaces")
	tracePath := filepath.Join(t.TempDir(), "cleanup.trace")

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Hooks: Hooks{
			BeforeRemove: "printf '%s\n' \"$WORKSPACE_KEY\" >> " + shellQuote(tracePath),
			Timeout:      hookCompletionTimeout(),
		},
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	target, err := backend.Create(context.Background(), Issue{Identifier: "DD-CLEAN"})
	if err != nil {
		t.Fatalf("target Create() error = %v", err)
	}
	other, err := backend.Create(context.Background(), Issue{Identifier: "DD-KEEP"})
	if err != nil {
		t.Fatalf("other Create() error = %v", err)
	}

	if err := backend.Cleanup(context.Background(), "DD-CLEAN"); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	if _, err := os.Stat(target.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target workspace exists after cleanup, stat error = %v", err)
	}
	if _, err := os.Stat(other.Path); err != nil {
		t.Fatalf("other workspace stat error = %v", err)
	}
	if got := strings.TrimSpace(readFile(t, tracePath)); got != "DD-CLEAN" {
		t.Fatalf("before_remove trace = %q, want DD-CLEAN", got)
	}
	if got := runGit(t, source, "worktree", "list", "--porcelain"); strings.Contains(got, target.Path) {
		t.Fatalf("git worktree list still contains removed path:\n%s", got)
	}
}

func TestLocalGitCleanupRemediatesGeneratedCachePermissions(t *testing.T) {

	t.Setenv("GIT_CEILING_DIRECTORIES", os.TempDir())

	enclosing := initSourceRepo(t)
	source := filepath.Join(enclosing, "source")
	initSourceRepoAt(t, source)
	publishCleanupSource(t, source)
	root := filepath.Join(enclosing, "workspaces")

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	info, err := backend.Create(context.Background(), Issue{Identifier: "DD-CACHE-PERM"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	t.Cleanup(func() {
		restoreWritableTree(t, info.Path)
	})

	writeFileDiffFile(t, source, ".git/info/exclude", "tmp/\n")
	cacheDir := filepath.Join(info.Path, "tmp", "_validation-cache", "go-mod", "modernc.org", "libc@v1.73.4")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}
	cacheFile := filepath.Join(cacheDir, "libc_amd64.go")
	if err := os.WriteFile(cacheFile, []byte("package libc\n"), 0o600); err != nil {
		t.Fatalf("write cache file: %v", err)
	}
	if err := os.Chmod(cacheFile, 0o444); err != nil {
		t.Fatalf("chmod cache file: %v", err)
	}
	if err := os.Chmod(cacheDir, 0o555); err != nil {
		t.Fatalf("chmod cache dir: %v", err)
	}

	if err := backend.Cleanup(context.Background(), "DD-CACHE-PERM"); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if _, err := os.Stat(info.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace exists after cleanup, stat error = %v", err)
	}
	if branchExists(t, source, "detent/dd-cache-perm") {
		t.Fatal("detent/dd-cache-perm branch still exists after cleanup")
	}
}

func TestWorkerScratchLifecycleRemediatesGeneratedCachePermissions(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	scratchPath, err := PrepareWorkerScratch(context.Background(), workspacePath)
	if err != nil {
		t.Fatalf("PrepareWorkerScratch() error = %v", err)
	}
	t.Cleanup(func() {
		restoreWritableTree(t, scratchPath)
		_ = os.RemoveAll(WorkerScratchRoot(workspacePath))
	})
	if scratchRoot := WorkerScratchRoot(workspacePath); !strings.HasPrefix(scratchPath, scratchRoot+string(filepath.Separator)) {
		t.Fatalf("scratch path = %q, want path under %q", scratchPath, scratchRoot)
	}

	cacheDir := filepath.Join(scratchPath, "go-mod", "modernc.org", "libc@v1.73.4")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}
	cacheFile := filepath.Join(cacheDir, "libc_amd64.go")
	if err := os.WriteFile(cacheFile, []byte("package libc\n"), 0o444); err != nil {
		t.Fatalf("write cache file: %v", err)
	}
	if err := os.Chmod(cacheDir, 0o555); err != nil {
		t.Fatalf("chmod cache dir: %v", err)
	}

	if err := CleanupWorkerScratch(workspacePath, scratchPath); err != nil {
		t.Fatalf("CleanupWorkerScratch() error = %v", err)
	}
	if _, err := os.Stat(scratchPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker scratch exists after cleanup, stat error = %v", err)
	}
}

func TestLocalGitRejectsSymlinkEscape(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	testRoot := t.TempDir()
	root := filepath.Join(testRoot, "workspaces")
	outside := filepath.Join(testRoot, "outside")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "DD-SYM")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	_, err = backend.Create(context.Background(), Issue{Identifier: "DD-SYM"})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Create() error = %v, want ErrUnsafePath", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "DD-SYM")); err != nil {
		t.Fatalf("symlink stat error = %v", err)
	}
}

func TestRunGitAtBoundsInheritedOutput(t *testing.T) {

	for _, tt := range []struct {
		name        string
		args        []string
		redirection string
		exitCode    int
		commit      bool
	}{
		{name: "stdout retained", redirection: "2>/dev/null"},
		{name: "stderr retained", redirection: ">/dev/null"},
		{name: "untracked listing stdout retained", args: []string{"ls-files", "--others", "--exclude-standard", "-z"}, redirection: "2>/dev/null"},
		{name: "untracked listing stderr retained", args: []string{"ls-files", "--others", "--exclude-standard", "-z"}, redirection: ">/dev/null"},
		{name: "failed command", exitCode: 23},
		{name: "output closed", redirection: ">/dev/null 2>&1"},
		{name: "worktree creation stdout retained", args: []string{"worktree", "add", "--detach", "new-worktree", "HEAD"}, redirection: "2>/dev/null"},
		{name: "worktree creation stderr retained", args: []string{"worktree", "add", "--detach", "new-worktree", "HEAD"}, redirection: ">/dev/null"},
		{name: "failed worktree creation retains diagnostics", args: []string{"worktree", "add", "--detach", "new-worktree", "HEAD"}, exitCode: 23},
		{name: "commit stdout retained", redirection: "2>/dev/null", commit: true},
		{name: "commit stderr retained", redirection: ">/dev/null", commit: true},
		{name: "failed commit retains diagnostics", commit: true, exitCode: 23},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			fifoPath := filepath.Join(dir, "release")
			makeFIFO(t, fifoPath)
			release, err := os.OpenFile(fifoPath, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer release.Close()
			donePath := filepath.Join(dir, "done")
			script := fmt.Sprintf(`#!/bin/sh
( read -r release < "$DETENT_PIPE_RELEASE"; printf done > "$DETENT_PIPE_DONE" ) %s &
printf 'git stdout\n'
printf 'git stderr\n' >&2
exit %d
`, tt.redirection, tt.exitCode)
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type result struct {
				output string
				err    error
			}
			results := make(chan result, 1)
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				args := tt.args
				if len(args) == 0 {
					args = []string{"worktree", "prune"}
				}
				env := []string{
					"DETENT_PIPE_RELEASE=" + fifoPath,
					"DETENT_PIPE_DONE=" + donePath,
				}
				var output string
				var err error
				if tt.commit {
					policy := commitPolicy{Report: CommitReport{Identity: CommitIdentity{Name: "Runner", Email: "runner@example.test"}, Signing: "unsigned"}}
					output, err = policy.Run(ctx, dir, env, "commit", "--allow-empty", "-m", "example")
				} else {
					output, err = runGitAtWithEnv(ctx, dir, env, args...)
				}
				results <- result{output: output, err: err}
			}()
			defer func() {
				cancel()
				defer func() { <-joined }()
				if _, err := release.WriteString("release\n"); err != nil {
					t.Errorf("release descendant: %v", err)
				}
				testenv.WaitUntil(t, t.Context(), func() error {
					_, err := os.Stat(donePath)
					return err
				})
			}()

			var got result
			select {
			case got = <-results:
			case <-testenv.WaitContext(t, t.Context()).Done():
				t.Fatal("Git command did not return while descendant retained output")
			}
			output := got.output
			if tt.exitCode != 0 {
				var commandErr *CommandError
				if !errors.As(got.err, &commandErr) {
					t.Fatalf("error = %v, want CommandError", got.err)
				}
				if got.output != "" {
					t.Fatalf("output = %q, want empty on failure", got.output)
				}
				output = commandErr.Output
				if tt.exitCode != 0 && commandErr.ExitCode != tt.exitCode {
					t.Fatalf("exit code = %d, want %d", commandErr.ExitCode, tt.exitCode)
				}
			} else if got.err != nil {
				t.Fatal(got.err)
			}
			if !strings.Contains(output, "git stdout") || !strings.Contains(output, "git stderr") {
				t.Fatalf("captured output = %q, want stdout and stderr", output)
			}
		})
	}
}

func TestGitQueriesCompleteWhenDescendantRetainsOutput(t *testing.T) {

	for _, tt := range []struct {
		name     string
		probe    func(context.Context, string) (string, error)
		exitCode int
	}{
		{name: "source common dir", probe: gitCommonDir},
		{name: "workspace common dir", probe: gitCommonDirWithinRoot},
		{name: "index path", probe: func(ctx context.Context, dir string) (string, error) {
			output, err := runGitAt(ctx, dir, "rev-parse", "--git-path", "index", "--verify", "HEAD")
			return strings.TrimSpace(output), err
		}},
		{name: "fetch", probe: func(ctx context.Context, dir string) (string, error) {
			output, err := runGitAt(ctx, dir, "fetch", "origin", "+refs/heads/main:refs/remotes/origin/main")
			return strings.TrimSpace(output), err
		}},
		{name: "remote listing", probe: func(ctx context.Context, dir string) (string, error) {
			output, err := (&LocalGit{sourceRoot: dir}).gitRemotes(ctx)
			return strings.TrimSpace(output), err
		}},
		{name: "failed remote listing", exitCode: 23, probe: func(ctx context.Context, dir string) (string, error) {
			return (&LocalGit{sourceRoot: dir}).gitRemotes(ctx)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			fifoPath := filepath.Join(dir, "release")
			makeFIFO(t, fifoPath)
			release, err := os.OpenFile(fifoPath, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer release.Close()
			donePath := filepath.Join(dir, "done")
			script := fmt.Sprintf(`#!/bin/sh
( read -r release < "$DETENT_PIPE_RELEASE"; printf done > "$DETENT_PIPE_DONE" ) &
printf '%%s\n' "$DETENT_COMMON_DIR"
exit %d
`, tt.exitCode)
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("DETENT_PIPE_RELEASE", fifoPath)
			t.Setenv("DETENT_PIPE_DONE", donePath)
			t.Setenv("DETENT_COMMON_DIR", dir)
			scratchDir := t.TempDir()
			t.Setenv("TMPDIR", scratchDir)

			output, commandErr := tt.probe(t.Context(), dir)
			if _, err := release.WriteString("release\n"); err != nil {
				t.Fatal(err)
			}
			deadline := time.After(10 * time.Second)
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				if _, err := os.Stat(donePath); err == nil {
					break
				}
				select {
				case <-deadline:
					t.Fatal("descendant did not acknowledge release")
				case <-ticker.C:
				}
			}
			if tt.exitCode != 0 {
				var gitErr *CommandError
				if !errors.As(commandErr, &gitErr) || gitErr.ExitCode != tt.exitCode {
					t.Fatalf("git query error = %v, want exit code %d", commandErr, tt.exitCode)
				}
				if output != "" {
					t.Fatalf("failed query output = %q, want empty", output)
				}
				output = strings.TrimSpace(gitErr.Output)
			} else if commandErr != nil {
				t.Fatalf("git query error = %v, want successful output after Git exits", commandErr)
			}
			canonicalDir, err := filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}
			canonicalOutput, err := filepath.EvalSymlinks(output)
			if err != nil {
				t.Fatalf("git query = %q: %v", output, err)
			}
			if canonicalOutput != canonicalDir {
				t.Fatalf("git query = %q, want %q", output, canonicalDir)
			}
			if entries, err := os.ReadDir(scratchDir); err != nil || len(entries) != 0 {
				t.Fatalf("query scratch remains: %v, %v", entries, err)
			}
		})
	}
}

func TestGitCommonDirCleanupFailureDoesNotInvalidateIdentity(t *testing.T) {

	commonDir := t.TempDir()
	scratchDir := t.TempDir()
	binDir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$DETENT_COMMON_DIR"
chmod 500 "$TMPDIR"
`
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMPDIR", scratchDir)
	t.Setenv("DETENT_COMMON_DIR", commonDir)
	t.Cleanup(func() {
		if err := os.Chmod(scratchDir, 0o700); err != nil {
			t.Error(err)
		}
	})

	got, err := gitCommonDir(t.Context(), commonDir)
	if err != nil {
		t.Fatalf("gitCommonDir() error = %v, want successful identity despite cleanup failure", err)
	}
	canonicalDir, err := filepath.EvalSymlinks(commonDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != canonicalDir {
		t.Fatalf("gitCommonDir() = %q, want %q", got, canonicalDir)
	}
}
