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
			AfterCreate: "printf '%s|%s|%s|%s|%s|%s|%s|%s\n' \"$PWD\" \"$(git branch --show-current)\" \"$ISSUE_IDENTIFIER\" \"$WORKSPACE_KEY\" \"$BRANCH\" \"$DETENT_ISSUE_IDENTIFIER\" \"$DETENT_WORKSPACE_KEY\" \"$DETENT_BRANCH\" >> " + shellQuote(tracePath),
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

func TestLocalGitInfoForIssueNamespacesKeysByProjectID(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	backend := &LocalGit{
		root:       root,
		autoBranch: true,
	}

	tests := []struct {
		name          string
		issue         Issue
		wantKey       string
		wantKeyPrefix string
	}{
		{
			name:    "legacy issue identifier without project id",
			issue:   Issue{Identifier: "digitaldrywood/detent#42"},
			wantKey: "digitaldrywood_detent_42",
		},
		{
			name:    "reserved detent metadata key",
			issue:   Issue{Identifier: ".detent"},
			wantKey: "issue",
		},
		{
			name:          "alpha project",
			issue:         Issue{ProjectID: "alpha", Identifier: "digitaldrywood/detent#42"},
			wantKeyPrefix: "alpha-digitaldrywood_detent_42-",
		},
		{
			name:          "bravo project same identifier",
			issue:         Issue{ProjectID: "bravo", Identifier: "digitaldrywood/detent#42"},
			wantKeyPrefix: "bravo-digitaldrywood_detent_42-",
		},
		{
			name:          "project ids with same safe key",
			issue:         Issue{ProjectID: "foo/bar", Identifier: "baz"},
			wantKeyPrefix: "foo_bar-baz-",
		},
		{
			name:          "second project id with same safe key",
			issue:         Issue{ProjectID: "foo_bar", Identifier: "baz"},
			wantKeyPrefix: "foo_bar-baz-",
		},
		{
			name:          "separator ambiguity left",
			issue:         Issue{ProjectID: "foo", Identifier: "bar-baz"},
			wantKeyPrefix: "foo-bar-baz-",
		},
		{
			name:          "separator ambiguity right",
			issue:         Issue{ProjectID: "foo-bar", Identifier: "baz"},
			wantKeyPrefix: "foo-bar-baz-",
		},
	}

	keys := make(map[string]string, len(tests))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := backend.infoForIssue(tt.issue)
			if err != nil {
				t.Fatalf("infoForIssue() error = %v", err)
			}
			switch {
			case tt.wantKey != "" && info.Key != tt.wantKey:
				t.Fatalf("Key = %q, want %q", info.Key, tt.wantKey)
			case tt.wantKeyPrefix != "" && !strings.HasPrefix(info.Key, tt.wantKeyPrefix):
				t.Fatalf("Key = %q, want prefix %q", info.Key, tt.wantKeyPrefix)
			case tt.wantKeyPrefix != "" && len(info.Key) == len(tt.wantKeyPrefix):
				t.Fatalf("Key = %q, want digest suffix", info.Key)
			}
			if filepath.Base(info.Path) != info.Key {
				t.Fatalf("Path basename = %q, want %q", filepath.Base(info.Path), info.Key)
			}
			wantBranch := "detent/" + strings.ToLower(info.Key)
			if info.Branch != wantBranch {
				t.Fatalf("Branch = %q, want %q", info.Branch, wantBranch)
			}

			keys[tt.name] = info.Key
		})
	}

	for leftName, leftKey := range keys {
		for rightName, rightKey := range keys {
			if leftName >= rightName {
				continue
			}
			if leftKey == rightKey {
				t.Fatalf("%s and %s both produced key %q", leftName, rightName, leftKey)
			}
		}
	}
}

func TestLocalGitCreateAndCleanupWithoutHooks(t *testing.T) {
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

	info, err := backend.Create(context.Background(), Issue{Identifier: "DD-NATIVE"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if !info.Created {
		t.Fatal("Create() Created = false, want true")
	}
	if got := strings.TrimSpace(runGit(t, info.Path, "branch", "--show-current")); got != "detent/dd-native" {
		t.Fatalf("worktree branch = %q, want detent/dd-native", got)
	}
	if got := readFile(t, filepath.Join(info.Path, "README.md")); got != "source repo\n" {
		t.Fatalf("README.md = %q, want source repo", got)
	}

	if err := backend.Cleanup(context.Background(), "DD-NATIVE"); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if _, err := os.Stat(info.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace exists after cleanup, stat error = %v", err)
	}
	if got := runGit(t, source, "worktree", "list", "--porcelain"); strings.Contains(got, info.Path) {
		t.Fatalf("git worktree list still contains removed path:\n%s", got)
	}
	if branchExists(t, source, "detent/dd-native") {
		t.Fatal("detent/dd-native branch still exists after cleanup")
	}
}

func TestGitMetadataWritableRootsForLinkedWorktree(t *testing.T) {
	t.Parallel()
	skipWindows(t)

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

	wantRoots := []string{
		mustCanonicalExistingPath(t, strings.TrimSpace(runGit(t, info.Path, "rev-parse", "--git-dir"))),
		mustCanonicalExistingPath(t, strings.TrimSpace(runGit(t, info.Path, "rev-parse", "--git-path", "objects"))),
		mustCanonicalExistingPath(t, filepath.Dir(strings.TrimSpace(runGit(t, info.Path, "rev-parse", "--git-path", "refs/heads/detent/dd-git-roots")))),
		mustCanonicalExistingPath(t, filepath.Dir(strings.TrimSpace(runGit(t, info.Path, "rev-parse", "--git-path", "logs/refs/heads/detent/dd-git-roots")))),
	}
	for _, want := range wantRoots {
		if !containsString(roots, want) {
			t.Fatalf("GitMetadataWritableRoots() = %#v, missing %q", roots, want)
		}
	}
	if commonDir := mustCanonicalExistingPath(t, strings.TrimSpace(runGit(t, info.Path, "rev-parse", "--git-common-dir"))); containsString(roots, commonDir) {
		t.Fatalf("GitMetadataWritableRoots() = %#v, should not allow entire common git dir %q", roots, commonDir)
	}

	if err := os.WriteFile(filepath.Join(info.Path, "agent.txt"), []byte("agent edit\n"), 0o600); err != nil {
		t.Fatalf("write agent edit: %v", err)
	}
	runGit(t, info.Path, "add", "agent.txt")
	runGit(t, info.Path, "commit", "-m", "agent commit")
	if got := strings.TrimSpace(runGit(t, info.Path, "log", "-1", "--pretty=%s")); got != "agent commit" {
		t.Fatalf("latest commit subject = %q, want agent commit", got)
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

func TestRunGitAtWithEnvCancellationReturnsPromptly(t *testing.T) {
	skipWindows(t)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	gitPath := filepath.Join(binDir, "git")
	gitScript := "#!/bin/sh\nsleep 4 &\nwait\n"
	if err := os.WriteFile(gitPath, []byte(gitScript), 0o700); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := runGitAtWithEnv(ctx, t.TempDir(), nil, "status")
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("runGitAtWithEnv() error = nil, want cancellation error")
	}
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		t.Fatalf("runGitAtWithEnv() error = %T, want *CommandError", err)
	}
	if !errors.Is(commandErr.Err, context.DeadlineExceeded) {
		t.Fatalf("CommandError.Err = %v, want context deadline exceeded", commandErr.Err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("runGitAtWithEnv() elapsed = %s, want cancellation within wait delay", elapsed)
	}
}

func TestLocalGitHookCancellationReturnsPromptly(t *testing.T) {
	skipWindows(t)

	workspacePath := t.TempDir()
	backend := &LocalGit{
		hooks:  Hooks{Timeout: 20 * time.Millisecond},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	started := time.Now()
	err := backend.runHook(
		context.Background(),
		"before_run",
		"sleep 4 & wait",
		Info{Path: workspacePath, Key: "DD-HOOK", Branch: "detent/dd-hook"},
		Issue{Identifier: "DD-HOOK"},
	)
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("runHook() error = nil, want cancellation error")
	}
	var hookErr *HookError
	if !errors.As(err, &hookErr) {
		t.Fatalf("runHook() error = %T, want *HookError", err)
	}
	if !errors.Is(hookErr.Err, context.DeadlineExceeded) {
		t.Fatalf("HookError.Err = %v, want context deadline exceeded", hookErr.Err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("runHook() elapsed = %s, want cancellation within wait delay", elapsed)
	}
}

func TestLocalGitHookAllowsDaemonizedSuccess(t *testing.T) {
	skipWindows(t)

	workspacePath := t.TempDir()
	backend := &LocalGit{
		hooks:  Hooks{Timeout: 3 * time.Second},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	started := time.Now()
	err := backend.runHook(
		context.Background(),
		"before_run",
		"sleep 4 &",
		Info{Path: workspacePath, Key: "DD-HOOK", Branch: "detent/dd-hook"},
		Issue{Identifier: "DD-HOOK"},
	)
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("runHook() error = %v, want nil", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("runHook() elapsed = %s, want daemonized hook within wait delay", elapsed)
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

func TestLocalGitCreateRecoversCleanDetachedWorktree(t *testing.T) {
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
	skipWindows(t)

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
			BeforeRun: "printf 'before:%s:%s\n' \"$PWD\" \"$WORKSPACE_KEY\" >> " + shellQuote(tracePath),
			AfterRun:  "printf 'after:%s:%s\n' \"$PWD\" \"$WORKSPACE_KEY\" >> " + shellQuote(tracePath),
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
	hookCommand := "printf 'out\\n'; printf 'err\\n' >&2; exit 17"

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
		Hooks: Hooks{
			AfterCreate: hookCommand,
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
}

func TestHookErrorErrorIncludesBoundedOutputTail(t *testing.T) {
	t.Parallel()

	output := "prefix-" + strings.Repeat("x", hookOutputTailBytes) + "-tail"
	err := (&HookError{
		Hook:     "after_create",
		Command:  "bootstrap",
		Dir:      "/workspaces/DD-TAIL",
		ExitCode: 1,
		LogPath:  "/workspaces/.detent/hook-logs/DD-TAIL/hook.log",
		Output:   output,
		Err:      errors.New("exit status 1"),
	}).Error()

	for _, want := range []string{
		"command \"bootstrap\"",
		"working directory \"/workspaces/DD-TAIL\"",
		"exit status 1",
		"hook log \"/workspaces/.detent/hook-logs/DD-TAIL/hook.log\"",
		"output (last 16 KiB)",
		"truncated to last 16 KiB",
		"-tail",
	} {
		if !strings.Contains(err, want) {
			t.Fatalf("HookError.Error() = %q, want %q", err, want)
		}
	}
	if strings.Contains(err, "prefix-") {
		t.Fatalf("HookError.Error() includes truncated prefix: %q", err)
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
			BeforeRemove: "printf '%s\n' \"$WORKSPACE_KEY\" >> " + shellQuote(tracePath),
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
	runGit(t, dir, "config", "core.autocrlf", "false")
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

func branchExists(t *testing.T, dir string, branch string) bool {
	t.Helper()

	cmd := exec.Command("git", "-C", dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	err := cmd.Run()
	if err == nil {
		return true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false
	}
	t.Fatalf("git show-ref failed: %v", err)
	return false
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

func mustCanonicalExistingPath(t *testing.T, path string) string {
	t.Helper()

	canonical, err := canonicalExistingPath(path)
	if err != nil {
		t.Fatalf("canonicalExistingPath(%q) error = %v", path, err)
	}
	return canonical
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
