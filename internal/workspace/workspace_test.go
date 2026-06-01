package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLocalGitCreateCreatesWorktreeBranchAndRunsAfterCreateHook(t *testing.T) {
	t.Parallel()
	skipWindows(t)

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	tracePath := filepath.Join(t.TempDir(), "after-create.trace")

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Hooks: Hooks{
			AfterCreate: "printf '%s|%s|%s|%s|%s\n' \"$PWD\" \"$(git branch --show-current)\" \"$DETENT_ISSUE_IDENTIFIER\" \"$DETENT_WORKSPACE_KEY\" \"$DETENT_BRANCH\" >> " + shellQuote(tracePath),
			Timeout:     time.Second,
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
	if len(fields) != 5 {
		t.Fatalf("after_create trace = %q, want five fields", trace)
	}
	if fields[0] != info.Path {
		t.Fatalf("after_create cwd = %q, want %q", fields[0], info.Path)
	}
	if fields[1] != "detent/dd_19" {
		t.Fatalf("after_create branch = %q, want detent/dd_19", fields[1])
	}
	if fields[2] != "DD/19" {
		t.Fatalf("DETENT_ISSUE_IDENTIFIER = %q, want DD/19", fields[2])
	}
	if fields[3] != "DD_19" {
		t.Fatalf("DETENT_WORKSPACE_KEY = %q, want DD_19", fields[3])
	}
	if fields[4] != "detent/dd_19" {
		t.Fatalf("DETENT_BRANCH = %q, want detent/dd_19", fields[4])
	}
}

func TestLocalGitHooksUseNonLoginShell(t *testing.T) {
	skipWindows(t)

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
			Timeout:     5 * time.Second,
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
	skipWindows(t)

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	tracePath := filepath.Join(t.TempDir(), "after-create.trace")
	argsPath := filepath.Join(t.TempDir(), "shell-args.trace")
	shellPath := filepath.Join(t.TempDir(), "custom-sh")
	shellScript := "#!/bin/sh\nprintf '%s\\n' \"$0|$1|$2\" > " + shellQuote(argsPath) + "\nexec /bin/sh \"$@\"\n"
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
			Timeout:     5 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	if _, err := backend.Create(context.Background(), Issue{Identifier: "DD-SHELL-CFG"}); err != nil {
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

func TestLocalGitCreateReusesExistingWorktreeWithoutAfterCreate(t *testing.T) {
	t.Parallel()
	skipWindows(t)

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	tracePath := filepath.Join(t.TempDir(), "after-create.trace")

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Hooks: Hooks{
			AfterCreate: "printf 'after-create\n' >> " + shellQuote(tracePath),
			Timeout:     time.Second,
		},
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	first, err := backend.Create(context.Background(), Issue{Identifier: "DD-REUSE"})
	if err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(first.Path, "local-progress.txt"), []byte("keep\n"), 0o600); err != nil {
		t.Fatalf("write local progress: %v", err)
	}

	second, err := backend.Create(context.Background(), Issue{Identifier: "DD-REUSE"})
	if err != nil {
		t.Fatalf("second Create() error = %v", err)
	}

	if second.Created {
		t.Fatal("second Create() Created = true, want false")
	}
	if second.Path != first.Path {
		t.Fatalf("second Create() Path = %q, want %q", second.Path, first.Path)
	}
	if got := readFile(t, filepath.Join(second.Path, "local-progress.txt")); got != "keep\n" {
		t.Fatalf("local-progress.txt = %q, want keep", got)
	}
	if got := strings.Count(readFile(t, tracePath), "after-create"); got != 1 {
		t.Fatalf("after_create runs = %d, want 1", got)
	}
}

func TestLocalGitBeforeAndAfterRunHooks(t *testing.T) {
	t.Parallel()
	skipWindows(t)

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	tracePath := filepath.Join(t.TempDir(), "run-hooks.trace")

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Hooks: Hooks{
			BeforeRun: "printf 'before:%s:%s\n' \"$PWD\" \"$DETENT_WORKSPACE_KEY\" >> " + shellQuote(tracePath),
			AfterRun:  "printf 'after:%s:%s\n' \"$PWD\" \"$DETENT_WORKSPACE_KEY\" >> " + shellQuote(tracePath),
			Timeout:   time.Second,
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
	skipWindows(t)

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Hooks: Hooks{
			AfterCreate: "echo nope && exit 17",
			Timeout:     time.Second,
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
	if !strings.Contains(hookErr.Output, "nope") {
		t.Fatalf("HookError.Output = %q, want nope", hookErr.Output)
	}
	if _, statErr := os.Stat(filepath.Join(root, "DD-FAIL")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed after_create workspace exists, stat error = %v", statErr)
	}
}

func TestLocalGitCleanupRemovesOnlyTargetWorktree(t *testing.T) {
	t.Parallel()
	skipWindows(t)

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	tracePath := filepath.Join(t.TempDir(), "cleanup.trace")

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Hooks: Hooks{
			BeforeRemove: "printf '%s\n' \"$DETENT_WORKSPACE_KEY\" >> " + shellQuote(tracePath),
			Timeout:      time.Second,
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

func TestLocalGitCleanupRejectsForeignGitRepoWithoutBeforeRemove(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	foreign := filepath.Join(root, "DD-FOREIGN")
	tracePath := filepath.Join(t.TempDir(), "cleanup.trace")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatalf("mkdir foreign repo: %v", err)
	}
	runCommand(t, foreign, "git", "init", "-b", "main")

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Hooks: Hooks{
			BeforeRemove: "printf 'ran\n' > " + shellQuote(tracePath),
			Timeout:      time.Second,
		},
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	err = backend.Cleanup(context.Background(), "DD-FOREIGN")
	if err == nil {
		t.Fatal("Cleanup() error = nil, want foreign repo error")
	}
	if !strings.Contains(err.Error(), "not managed by source") {
		t.Fatalf("Cleanup() error = %v, want not managed by source", err)
	}
	if _, err := os.Stat(tracePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("before_remove hook ran, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(foreign, ".git")); err != nil {
		t.Fatalf("foreign repo was removed, stat error = %v", err)
	}
}

func TestLocalGitRejectsSymlinkEscape(t *testing.T) {
	t.Parallel()
	skipWindows(t)

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

func TestLocalGitRejectsExistingGitRepoFromDifferentSource(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	foreign := filepath.Join(root, "DD-FOREIGN")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatalf("mkdir foreign repo: %v", err)
	}
	runCommand(t, foreign, "git", "init", "-b", "main")

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	_, err = backend.Create(context.Background(), Issue{Identifier: "DD-FOREIGN"})
	if err == nil {
		t.Fatal("Create() error = nil, want foreign repo error")
	}
	if !strings.Contains(err.Error(), "not managed by source") {
		t.Fatalf("Create() error = %v, want not managed by source", err)
	}
	if _, err := os.Stat(filepath.Join(foreign, ".git")); err != nil {
		t.Fatalf("foreign repo was removed, stat error = %v", err)
	}
}

func initSourceRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	runCommand(t, dir, "git", "init", "-b", "main")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("source repo\n"), 0o600); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "initial")

	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return runCommand(t, dir, "git", args...)
}

func runCommand(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()

	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return string(output)
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func skipWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a UNIX test environment")
	}
}
