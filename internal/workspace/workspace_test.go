package workspace

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/gotestreceipt"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

func TestMain(m *testing.M) {
	if gotestreceipt.IsWrapperInvocation(os.Args[0]) {
		os.Exit(gotestreceipt.RunWrapper(os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) == 2 && os.Args[1] == toolcache.ProgramArgument {
		if err := toolcache.RunProgram(os.Getenv("GOCACHE"), os.TempDir(), os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if err := flag.Set("test.parallel", "8"); err != nil {
		panic(err)
	}
	os.Exit(runWorkspaceTests(m))
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
	publishCleanupSource(t, source)
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

func TestLocalGitCreatePrunesMissingRegisteredWorktree(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	backend, err := NewLocalGit(LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewLocalGit() error = %v", err)
	}
	issue := Issue{Identifier: "DD-STALE-REGISTRATION"}
	first, err := backend.Create(context.Background(), issue)
	if err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
	movedPath := filepath.Join(t.TempDir(), "moved-workspace")
	if err := os.Rename(first.Path, movedPath); err != nil {
		t.Fatalf("move registered worktree: %v", err)
	}
	if got := runGit(t, source, "worktree", "list", "--porcelain"); !strings.Contains(got, filepath.ToSlash(first.Path)) {
		t.Fatalf("git worktree list does not contain missing registered path:\n%s", got)
	}

	second, err := backend.Create(context.Background(), issue)
	if err != nil {
		t.Fatalf("second Create() error = %v", err)
	}
	if !second.Created {
		t.Fatal("second Create() Created = false, want true")
	}
	if second.Path != first.Path {
		t.Fatalf("second Create() Path = %q, want %q", second.Path, first.Path)
	}
	if got := strings.TrimSpace(runGit(t, second.Path, "branch", "--show-current")); got != first.Branch {
		t.Fatalf("second worktree branch = %q, want %q", got, first.Branch)
	}
}

func TestRunWorktreeAddWithPrune(t *testing.T) {
	t.Parallel()

	initialPruneErr := errors.New("initial prune failed")
	retryPruneErr := errors.New("retry prune failed")
	nonRetryErr := errors.New("exit status 1")
	exit128Err := errors.New("exit status 128")
	retryAddErr := errors.New("retry add failed")
	nonRetryCommandErr := &CommandError{ExitCode: 1, Err: nonRetryErr}
	exit128CommandErr := &CommandError{ExitCode: 128, Err: exit128Err}

	tests := []struct {
		name        string
		pruneErrors []error
		addErrors   []error
		wantPrunes  int
		wantAdds    int
		wantErrors  []error
	}{
		{
			name:       "successful add",
			addErrors:  []error{nil},
			wantPrunes: 1,
			wantAdds:   1,
		},
		{
			name:        "initial prune fails",
			pruneErrors: []error{initialPruneErr},
			wantPrunes:  1,
			wantErrors:  []error{initialPruneErr},
		},
		{
			name:       "non-128 add is not retried",
			addErrors:  []error{nonRetryCommandErr},
			wantPrunes: 1,
			wantAdds:   1,
			wantErrors: []error{nonRetryErr},
		},
		{
			name:        "exit 128 succeeds after prune",
			pruneErrors: []error{nil, nil},
			addErrors:   []error{exit128CommandErr, nil},
			wantPrunes:  2,
			wantAdds:    2,
		},
		{
			name:        "retry prune fails",
			pruneErrors: []error{nil, retryPruneErr},
			addErrors:   []error{exit128CommandErr},
			wantPrunes:  2,
			wantAdds:    1,
			wantErrors:  []error{exit128Err, retryPruneErr},
		},
		{
			name:        "retry add fails without another retry",
			pruneErrors: []error{nil, nil},
			addErrors:   []error{exit128CommandErr, retryAddErr},
			wantPrunes:  2,
			wantAdds:    2,
			wantErrors:  []error{retryAddErr},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pruneCalls := 0
			addCalls := 0
			err := runWorktreeAddWithPrune(func() error {
				call := pruneCalls
				pruneCalls++
				if call >= len(tt.pruneErrors) {
					return nil
				}
				return tt.pruneErrors[call]
			}, func() error {
				call := addCalls
				addCalls++
				if call >= len(tt.addErrors) {
					return nil
				}
				return tt.addErrors[call]
			})

			if len(tt.wantErrors) == 0 && err != nil {
				t.Fatalf("runWorktreeAddWithPrune() error = %v, want nil", err)
			}
			if len(tt.wantErrors) > 0 && err == nil {
				t.Fatal("runWorktreeAddWithPrune() error = nil, want error")
			}
			for _, wantErr := range tt.wantErrors {
				if !errors.Is(err, wantErr) {
					t.Errorf("runWorktreeAddWithPrune() error = %v, want errors.Is(%v)", err, wantErr)
				}
			}
			if pruneCalls != tt.wantPrunes {
				t.Errorf("prune calls = %d, want %d", pruneCalls, tt.wantPrunes)
			}
			if addCalls != tt.wantAdds {
				t.Errorf("add calls = %d, want %d", addCalls, tt.wantAdds)
			}
		})
	}
}

func TestLocalGitCreateDoesNotFallBackWhenOriginIsUnavailable(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	runGit(t, source, "remote", "add", "origin", filepath.Join(t.TempDir(), "missing.git"))
	backend, err := NewLocalGit(LocalGitOptions{
		Root:       filepath.Join(t.TempDir(), "workspaces"),
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewLocalGit() error = %v", err)
	}
	issue := Issue{Identifier: "DD-UNAVAILABLE-ORIGIN"}

	_, err = backend.Create(context.Background(), issue)
	if err == nil {
		t.Fatal("Create() error = nil, want unavailable origin error")
	}
	if !strings.Contains(err.Error(), "inspect published workspace branch") {
		t.Fatalf("Create() error = %v, want published branch lookup context", err)
	}
	info, infoErr := backend.infoForIssue(issue)
	if infoErr != nil {
		t.Fatalf("infoForIssue() error = %v", infoErr)
	}
	if _, statErr := os.Stat(info.Path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("workspace stat error = %v, want workspace absent", statErr)
	}
	if branchExists(t, source, info.Branch) {
		t.Fatalf("branch %q exists after failed Create()", info.Branch)
	}
}

func TestLocalGitPrepareMergeRebasesAndPushesCleanBranch(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	remote := initBareRemote(t)
	runGit(t, source, "remote", "add", "origin", remote)
	runGit(t, source, "push", "-u", "origin", "main")

	root := filepath.Join(t.TempDir(), "workspaces")
	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	preparer, ok := backend.(MergePreparer)
	if !ok {
		t.Fatal("backend does not implement MergePreparer")
	}
	issue := Issue{Identifier: "DD-MERGE"}
	info, err := backend.Create(context.Background(), issue)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, "feature.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	runGit(t, info.Path, "add", "feature.txt")
	runGit(t, info.Path, "commit", "-m", "feature")
	runGit(t, info.Path, "push", "origin", "HEAD:refs/heads/"+info.Branch)

	if err := os.WriteFile(filepath.Join(source, "main.txt"), []byte("main\n"), 0o600); err != nil {
		t.Fatalf("write main: %v", err)
	}
	runGit(t, source, "add", "main.txt")
	runGit(t, source, "commit", "-m", "main change")
	runGit(t, source, "push", "origin", "main")

	result, err := preparer.PrepareMerge(context.Background(), info, issue, MergePrepareOptions{})
	if err != nil {
		t.Fatalf("PrepareMerge() error = %v", err)
	}
	if result.Status != MergePrepareStatusClean {
		t.Fatalf("PrepareMerge() status = %q, want clean", result.Status)
	}
	if !result.DiffStat.IsEmpty() {
		t.Fatalf("PrepareMerge() DiffStat = %#v, want zero", result.DiffStat)
	}
	if !result.HeadChanged {
		t.Fatal("PrepareMerge() HeadChanged = false, want true after rebase")
	}
	if got := readFile(t, filepath.Join(info.Path, "main.txt")); got != "main\n" {
		t.Fatalf("main.txt = %q, want rebased main file", got)
	}
	head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
	remoteHead := strings.Fields(runGit(t, source, "ls-remote", "origin", "refs/heads/"+info.Branch))
	if len(remoteHead) == 0 || remoteHead[0] != head {
		t.Fatalf("remote branch head = %#v, want %s", remoteHead, head)
	}

	result, err = preparer.PrepareMerge(context.Background(), info, issue, MergePrepareOptions{})
	if err != nil {
		t.Fatalf("second PrepareMerge() error = %v", err)
	}
	if result.HeadChanged {
		t.Fatal("second PrepareMerge() HeadChanged = true, want false for unchanged head")
	}
}

func TestLocalGitPrepareMergeUsesDevBranch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		remoteDefault string
		opts          MergePrepareOptions
	}{
		{name: "remote default", remoteDefault: "dev"},
		{name: "explicit target", remoteDefault: "main", opts: MergePrepareOptions{TargetBranch: "dev"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			testLocalGitPrepareMergeUsesDevBranch(t, tt.remoteDefault, tt.opts)
		})
	}
}

func testLocalGitPrepareMergeUsesDevBranch(t *testing.T, remoteDefault string, opts MergePrepareOptions) {
	t.Helper()

	source := initSourceRepo(t)
	remote := initBareRemote(t)
	runGit(t, source, "remote", "add", "origin", remote)
	runGit(t, source, "push", "-u", "origin", "main")
	runGit(t, source, "switch", "-c", "dev")
	if err := os.WriteFile(filepath.Join(source, "dev.txt"), []byte("dev\n"), 0o600); err != nil {
		t.Fatalf("write dev file: %v", err)
	}
	runGit(t, source, "add", "dev.txt")
	runGit(t, source, "commit", "-m", "dev branch")
	runGit(t, source, "push", "-u", "origin", "dev")
	runGit(t, source, "config", "--replace-all", "remote.origin.fetch", "+refs/heads/main:refs/remotes/origin/main")
	runGit(t, source, "update-ref", "-d", "refs/remotes/origin/dev")
	runGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/"+remoteDefault)

	root := filepath.Join(t.TempDir(), "workspaces")
	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	preparer, ok := backend.(MergePreparer)
	if !ok {
		t.Fatal("backend does not implement MergePreparer")
	}
	issue := Issue{Identifier: "DD-DEFAULT-BRANCH"}
	info, err := backend.Create(context.Background(), issue)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, "feature.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	runGit(t, info.Path, "add", "feature.txt")
	runGit(t, info.Path, "commit", "-m", "feature")
	runGit(t, info.Path, "push", "origin", "HEAD:refs/heads/"+info.Branch)

	runGit(t, source, "switch", "main")
	if err := os.WriteFile(filepath.Join(source, "main-latest.txt"), []byte("main\n"), 0o600); err != nil {
		t.Fatalf("write latest main file: %v", err)
	}
	runGit(t, source, "add", "main-latest.txt")
	runGit(t, source, "commit", "-m", "main latest")
	runGit(t, source, "push", "origin", "main")
	runGit(t, source, "switch", "dev")
	if err := os.WriteFile(filepath.Join(source, "dev-latest.txt"), []byte("dev latest\n"), 0o600); err != nil {
		t.Fatalf("write latest dev file: %v", err)
	}
	runGit(t, source, "add", "dev-latest.txt")
	runGit(t, source, "commit", "-m", "dev latest")
	runGit(t, source, "push", "origin", "dev")

	result, err := preparer.PrepareMerge(context.Background(), info, issue, opts)
	if err != nil {
		t.Fatalf("PrepareMerge() error = %v", err)
	}
	if result.Status != MergePrepareStatusClean {
		t.Fatalf("PrepareMerge() status = %q, want clean", result.Status)
	}
	if got := readFile(t, filepath.Join(info.Path, "dev-latest.txt")); got != "dev latest\n" {
		t.Fatalf("dev-latest.txt = %q, want rebased dev file", got)
	}
	if _, err := os.Stat(filepath.Join(info.Path, "main-latest.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("main-latest.txt stat error = %v, want file absent", err)
	}
}

type mergeConflictCase struct {
	name         string
	commits      int
	failSubjects bool
}

func TestLocalGitPrepareMergeAbortsConflictingRebase(t *testing.T) {
	testLocalGitPrepareMergeAbortsConflictingRebase(t, []mergeConflictCase{
		{name: "target intent", commits: 1},
		{name: "bounded target history", commits: 42},
	})
}

func testLocalGitPrepareMergeAbortsConflictingRebase(t *testing.T, tests []mergeConflictCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.failSubjects {
				t.Parallel()
			}
			source := initSourceRepo(t)
			remote := initBareRemote(t)
			runGit(t, source, "remote", "add", "origin", remote)
			runGit(t, source, "push", "-u", "origin", "main")

			root := filepath.Join(t.TempDir(), "workspaces")
			backend, err := NewBackend(KindLocalGit, LocalGitOptions{
				Root:       root,
				SourceRoot: source,
				AutoBranch: true,
			})
			if err != nil {
				t.Fatalf("NewBackend() error = %v", err)
			}
			preparer, ok := backend.(MergePreparer)
			if !ok {
				t.Fatal("backend does not implement MergePreparer")
			}
			issue := Issue{Identifier: "DD-CONFLICT"}
			info, err := backend.Create(context.Background(), issue)
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("feature\n"), 0o600); err != nil {
				t.Fatalf("write feature README: %v", err)
			}
			runGit(t, info.Path, "add", "README.md")
			runGit(t, info.Path, "commit", "-m", "feature conflict")

			if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("main\n"), 0o600); err != nil {
				t.Fatalf("write main README: %v", err)
			}
			if err := os.WriteFile(filepath.Join(source, "other.txt"), []byte("unrelated"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, source, "add", "other.txt")
			runGit(t, source, "commit", "-m", "unrelated target behavior")
			runGit(t, source, "add", "README.md")
			runGit(t, source, "commit", "-m", "main conflict")
			for n := 1; n < tt.commits; n++ {
				if err := os.WriteFile(filepath.Join(source, "README.md"), []byte(fmt.Sprintf("target %d\n", n)), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, source, "add", "README.md")
				runGit(t, source, "commit", "-m", fmt.Sprintf("target conflict %d", n))
			}
			runGit(t, source, "push", "origin", "main")

			if tt.failSubjects {
				realGit, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				directory := t.TempDir()
				script := "#!/bin/sh\nif [ \"$4\" = \"log\" ]; then exit 29; fi\nexec " + shellQuote(realGit) + " \"$@\"\n"
				if err := os.WriteFile(filepath.Join(directory, "git"), []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
				subjects, err := mergeTargetCommitSubjects(t.Context(), info.Path, "HEAD", "main", []string{"README.md"})
				if len(subjects) != 0 || err == nil || !strings.Contains(err.Error(), "read merge target commit subjects") {
					t.Fatalf("failed subject lookup = %v, %v", subjects, err)
				}
				var commandErr *CommandError
				if !errors.As(err, &commandErr) || commandErr.ExitCode != 29 {
					t.Fatalf("subject lookup error = %v, want wrapped exit code 29", err)
				}
			}
			result, err := preparer.PrepareMerge(context.Background(), info, issue, MergePrepareOptions{})
			if err != nil {
				t.Fatalf("PrepareMerge() error = %v", err)
			}
			if result.Status != MergePrepareStatusConflict {
				t.Fatalf("PrepareMerge() status = %q, want conflict", result.Status)
			}
			if tt.failSubjects && len(result.TargetCommitSubjects) != 0 {
				t.Fatalf("failed subject lookup = %v, want empty", result.TargetCommitSubjects)
			}
			if !tt.failSubjects && tt.commits == 1 && !slices.Equal(result.TargetCommitSubjects, []string{"main conflict"}) {
				t.Fatalf("target commit subjects = %v", result.TargetCommitSubjects)
			}
			if tt.commits > 40 && (len(result.TargetCommitSubjects) != 41 || result.TargetCommitSubjects[0] != "target conflict 41" || result.TargetCommitSubjects[40] != "[additional target commits truncated]") {
				t.Fatalf("target commit truncation = %v", result.TargetCommitSubjects)
			}
			if len(result.ConflictPaths) != 1 || result.ConflictPaths[0] != "README.md" {
				t.Fatalf("conflict paths = %v, want README.md", result.ConflictPaths)
			}
			if got := strings.TrimSpace(runGit(t, info.Path, "status", "--short")); got != "" {
				t.Fatalf("status after aborted rebase = %q, want clean", got)
			}
			if got := strings.TrimSpace(runGit(t, info.Path, "branch", "--show-current")); got != info.Branch {
				t.Fatalf("branch after aborted rebase = %q, want %q", got, info.Branch)
			}
			if got := readFile(t, filepath.Join(info.Path, "README.md")); got != "feature\n" {
				t.Fatalf("README after aborted rebase = %q, want feature branch content", got)
			}
			inProgress, err := rebaseInProgress(context.Background(), info.Path)
			if err != nil {
				t.Fatalf("rebaseInProgress() error = %v", err)
			}
			if inProgress {
				t.Fatal("rebase still in progress after PrepareMerge conflict")
			}
		})
	}
}

func TestLocalGitQuarantineWorkspaceCount(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	parent := filepath.Join(root, ".detent", "quarantine")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatalf("create quarantine directory: %v", err)
	}
	base := "detent-acme_widgets_42"
	stamp := "20260820T140000.000000000Z"
	for _, name := range []string{
		base + "-" + stamp,
		base + "-" + stamp + "-1",
		base + "-" + stamp + "-2",
		base + "-malformed",
		base + "-other-" + stamp,
		base + "0-" + stamp,
	} {
		if err := os.MkdirAll(filepath.Join(parent, name), 0o700); err != nil {
			t.Fatalf("create quarantine entry %q: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(parent, base+"-20260820T150000.000000000Z"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("create quarantine file: %v", err)
	}
	backend := &LocalGit{root: root}

	got, err := backend.quarantineWorkspaceCount(filepath.Join(root, base))
	if err != nil {
		t.Fatalf("quarantineWorkspaceCount() error = %v", err)
	}
	if got != 3 {
		t.Fatalf("quarantineWorkspaceCount() = %d, want 3", got)
	}
}

func TestHookErrorOmitsPrivateDiagnostics(t *testing.T) {
	t.Parallel()
	err := &HookError{Hook: "after_create", Command: "private command", Dir: "private path", Output: "private output", Err: errors.New("private cause"), ExitCode: 17}
	if got, want := err.Error(), fmt.Sprintf("%s: after_create (exit status 17)", ErrHookFailed); got != want {
		t.Fatalf("hook failure = %q, want %q", got, want)
	}
}

func TestLocalGitRepositoryDiscoveryStaysWithinCandidate(t *testing.T) {
	t.Parallel()

	enclosing := initSourceRepo(t)
	source := filepath.Join(enclosing, "source")
	initSourceRepoAt(t, source)
	sourceSubdir := filepath.Join(source, "subdir")
	if err := os.MkdirAll(sourceSubdir, 0o700); err != nil {
		t.Fatalf("mkdir source subdirectory: %v", err)
	}
	root := filepath.Join(enclosing, "workspaces")

	backend, err := NewLocalGit(LocalGitOptions{
		Root:       root,
		SourceRoot: sourceSubdir,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewLocalGit() error = %v", err)
	}
	managed, err := backend.Create(context.Background(), Issue{Identifier: "DD-MANAGED"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	foreign := filepath.Join(root, "DD-FOREIGN")
	initSourceRepoAt(t, foreign)
	unmanaged := filepath.Join(root, "DD-UNMANAGED")
	if err := os.MkdirAll(unmanaged, 0o700); err != nil {
		t.Fatalf("mkdir unmanaged workspace: %v", err)
	}

	tests := []struct {
		name       string
		path       string
		wantGit    bool
		wantSource bool
	}{
		{name: "managed worktree", path: managed.Path, wantGit: true, wantSource: true},
		{name: "foreign repository", path: foreign, wantGit: true, wantSource: false},
		{name: "unmanaged nested directory", path: unmanaged, wantGit: false, wantSource: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := backend.isGitWorkspace(context.Background(), test.path); got != test.wantGit {
				t.Errorf("isGitWorkspace() = %t, want %t", got, test.wantGit)
			}
			if got := backend.isSourceWorktree(context.Background(), test.path); got != test.wantSource {
				t.Errorf("isSourceWorktree() = %t, want %t", got, test.wantSource)
			}
		})
	}
}

func TestCleanupOwnedPathConfinesRemovalToRegisteredRoot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       func(root string, outside string) string
		wantErr    bool
		wantExists bool
	}{
		{
			name: "registered descendant",
			path: func(root string, _ string) string {
				return filepath.Join(root, "run-2011")
			},
		},
		{
			name: "cleanup root",
			path: func(root string, _ string) string {
				return root
			},
			wantErr:    true,
			wantExists: true,
		},
		{
			name: "outside root",
			path: func(_ string, outside string) string {
				return outside
			},
			wantErr:    true,
			wantExists: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			outside := t.TempDir()
			path := tt.path(root, outside)
			if path != root && path != outside {
				if err := os.MkdirAll(filepath.Join(path, "cache"), 0o700); err != nil {
					t.Fatalf("create cleanup path: %v", err)
				}
			}
			err := CleanupOwnedPath(root, path)
			if got := err != nil; got != tt.wantErr {
				t.Fatalf("CleanupOwnedPath() error = %v, want error %v", err, tt.wantErr)
			}
			_, statErr := os.Stat(path)
			if got := statErr == nil; got != tt.wantExists {
				t.Fatalf("cleanup path exists = %v, want %v, stat error = %v", got, tt.wantExists, statErr)
			}
		})
	}
}

func TestPrepareWorkerScratchPreservesPriorAttempt(t *testing.T) {
	t.Parallel()
	workspacePath := t.TempDir()
	t.Cleanup(func() { _ = os.RemoveAll(WorkerScratchRoot(workspacePath)) })
	prior, err := PrepareWorkerScratch(t.Context(), workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(prior, "fixture")
	if err := os.WriteFile(fixture, []byte("prior attempt"), 0o600); err != nil {
		t.Fatal(err)
	}
	resumed, err := PrepareWorkerScratch(t.Context(), workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixture); err != nil {
		t.Fatalf("resumed attempt removed prior fixture: %v", err)
	}
	if resumed == prior {
		t.Fatal("attempts share scratch ownership")
	}
	if err := CleanupOwnedPath(workspacePath, prior); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(resumed); err != nil {
		t.Fatalf("prior cleanup removed resumed scratch: %v", err)
	}
}

func TestPrepareWorkerScratchPreservesGitExclude(t *testing.T) {
	t.Parallel()

	workspacePath := initSourceRepo(t)
	excludePath := filepath.Join(workspacePath, ".git", "info", "exclude")
	before, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(WorkerScratchRoot(workspacePath)) })
	scratchPath, err := PrepareWorkerScratch(context.Background(), workspacePath)
	if err != nil {
		t.Fatalf("PrepareWorkerScratch() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(scratchPath, "cache"), []byte("temporary"), 0o600); err != nil {
		t.Fatalf("write worker scratch: %v", err)
	}

	status := runCommand(t, workspacePath, "git", "status", "--short", "--untracked-files=all")
	if strings.Contains(status, ".detent/worker-tmp") {
		t.Fatalf("git status includes worker scratch:\n%s", status)
	}
	after, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read git exclude: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("scratch preparation changed git exclude: %q", after)
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
			Timeout:      hookCompletionTimeout(),
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

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat %s: %v", path, err)
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", path)
		}
	}
}

func waitForError(t *testing.T, result <-chan error, timeout time.Duration) error {
	t.Helper()

	select {
	case err := <-result:
		return err
	case <-time.After(timeout):
		t.Fatal("timed out waiting for result")
		return nil
	}
}

func linkedWorktreeGitDir(t *testing.T, workspacePath string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(workspacePath, ".git"))
	if err != nil {
		t.Fatalf("read linked worktree .git file: %v", err)
	}
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok || strings.TrimSpace(gitDir) == "" {
		t.Fatalf("linked worktree .git file = %q, want gitdir path", data)
	}
	return mustCanonicalExistingPath(t, gitDir)
}

func branchExists(t *testing.T, dir string, branch string) bool {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
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

func installGitCreationWrapper(t *testing.T, mode string, source string, target string) {
	t.Helper()

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("locate git: %v", err)
	}
	wrapperDir := t.TempDir()
	wrapperPath := filepath.Join(wrapperDir, "git")
	var action string
	switch mode {
	case "partial_failure":
		action = "mkdir -p " + shellQuote(target) + "\n" +
			"printf 'partial_failure\\n' > " + shellQuote(filepath.Join(target, "forensics.txt")) + "\n" +
			"printf 'synthetic worktree add failure\\n' >&2\n" +
			"exit 23"
	case "standalone_after_add":
		action = shellQuote(realGit) + " \"$@\"\n" +
			"status=$?\n" +
			"if [ \"$status\" -ne 0 ]; then exit \"$status\"; fi\n" +
			shellQuote(realGit) + " -C " + shellQuote(source) + " worktree remove --force " + shellQuote(target) + "\n" +
			shellQuote(realGit) + " clone --quiet " + shellQuote(source) + " " + shellQuote(target) + "\n" +
			"printf 'standalone_after_add\\n' > " + shellQuote(filepath.Join(target, "forensics.txt")) + "\n" +
			"exit 0"
	default:
		t.Fatalf("unknown git wrapper mode %q", mode)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"-C\" ] && [ \"$3\" = \"worktree\" ] && [ \"$4\" = \"add\" ]; then\n" +
		action + "\n" +
		"fi\n" +
		"exec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(wrapperPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write git wrapper: %v", err)
	}
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func installGitUpdateRefFailureWrapper(t *testing.T) {
	t.Helper()

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("locate git: %v", err)
	}
	wrapperDir := t.TempDir()
	wrapperPath := filepath.Join(wrapperDir, "git")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"-C\" ] && [ \"$3\" = \"update-ref\" ] && [ \"$4\" = \"--no-deref\" ] && [ \"$5\" = \"HEAD\" ]; then\n" +
		"printf 'synthetic update-ref failure\\n' >&2\n" +
		"exit 29\n" +
		"fi\n" +
		"exec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(wrapperPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write git wrapper: %v", err)
	}
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func singleQuarantinedWorkspace(t *testing.T, root string) string {
	t.Helper()

	parent := filepath.Join(root, ".detent", "quarantine")
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read quarantine directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("quarantine entries = %d, want 1", len(entries))
	}
	return filepath.Join(parent, entries[0].Name())
}

func restoreWritableTree(t *testing.T, path string) {
	t.Helper()

	err := filepath.WalkDir(path, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return os.Chmod(path, 0o600)
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore writable tree: %v", err)
	}
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

type mergeResolvedHeadCase struct {
	name             string
	before           string
	gate             string
	wantError        string
	wantStage        string
	wantPushed       bool
	mode             string
	baseGo           string
	resolvedGo       string
	description      string
	wantMissing      string
	prGo             string
	targetGo         string
	goFile           string
	moveFile         bool
	deleteFile       bool
	wantMissingCount int
	baseFiles        map[string]string
	prFiles          map[string]string
	targetFiles      map[string]string
	resolvedFiles    map[string]string
	gitFailure       string
	mergeCleanly     bool
}

func TestLocalGitPrepareMergeValidatesResolvedHead(t *testing.T) {
	testLocalGitPrepareMergeValidatesResolvedHead(t, []mergeResolvedHeadCase{
		{name: "drops a platform function variant", goFile: "watch_linux.go", baseGo: "//go:build linux\n\npackage example\nfunc Watch() {}\n", baseFiles: map[string]string{"watch_windows.go": "//go:build windows\n\npackage example\nfunc Watch() {}\n"}, resolvedGo: "//go:build linux\n\npackage example\nfunc Watch() {}\n", resolvedFiles: map[string]string{"watch_windows.go": ""}, wantMissing: "Watch"},
		{name: "drops a platform receiver variant", goFile: "watch_linux.go", baseGo: "//go:build linux\n\npackage example\ntype Watcher struct{}\nfunc (w *Watcher) Watch() {}\n", baseFiles: map[string]string{"watch_windows.go": "//go:build windows\n\npackage example\ntype Watcher struct{}\nfunc (w *Watcher) Watch() {}\n"}, resolvedGo: "//go:build linux\n\npackage example\ntype Watcher struct{}\nfunc (w *Watcher) Watch() {}\n", resolvedFiles: map[string]string{"watch_windows.go": ""}, wantMissing: "Watcher.Watch"},
		{name: "platform file rename preserves variants", goFile: "watch_linux.go", baseGo: "//go:build linux\n\npackage example\nfunc Watch() {}\n", baseFiles: map[string]string{"watch_windows.go": "//go:build windows\n\npackage example\nfunc Watch() {}\n"}, resolvedGo: "//go:build linux\n\npackage example\nfunc Watch() {}\n", moveFile: true, wantPushed: true},
		{name: "PR intentionally deletes a platform variant", goFile: "watch_linux.go", baseGo: "//go:build linux\n\npackage example\nfunc Watch() {}\n", baseFiles: map[string]string{"watch_windows.go": "//go:build windows\n\npackage example\nfunc Watch() {}\n"}, prFiles: map[string]string{"watch_windows.go": ""}, resolvedGo: "//go:build linux\n\npackage example\nfunc Watch() {}\n", mergeCleanly: true, wantPushed: true},
		{name: "do not removal bullet", resolvedGo: "package example\nfunc PR() {}\n", description: "Do not:\n- Remove `Target`.", wantMissing: "Target"},
		{name: "out of scope removal bullet", resolvedGo: "package example\nfunc PR() {}\n", description: "## Out of scope\n- Remove `Target`.", wantMissing: "Target"},
		{name: "do not rename bullet", resolvedGo: "package example\nfunc PR() {}\nfunc Renamed() {}\n", description: "Do not:\n- Rename `Target` to `Renamed`.", wantMissing: "Target"},
		{name: "out of scope bare instruction", resolvedGo: "package example\nfunc PR() {}\n", description: "Out of scope:\nRemove `Target`.", wantMissing: "Target"},
		{name: "nested context cannot authorize removal", resolvedGo: "package example\nfunc PR() {}\n", description: "## Out of scope\n### Acceptance criteria\n- Remove `Target`.", wantMissing: "Target"},
		{name: "standalone removal bullet", resolvedGo: "package example\nfunc PR() {}\n", description: "- Remove `Target`.", wantPushed: true},
		{name: "later context makes removal ambiguous", resolvedGo: "package example\nfunc PR() {}\n", description: "Remove `Target`.\nDo not remove `Target`.", wantMissing: "Target"},
		{name: "indented example cannot authorize removal", resolvedGo: "package example\nfunc PR() {}\n", description: "    Remove `Target`.", wantMissing: "Target"},
		{name: "PR deletes a file", mergeCleanly: true, prFiles: map[string]string{"example.go": ""}, deleteFile: true, resolvedGo: "deleted", wantPushed: true},
		{name: "target deletes a file", mergeCleanly: true, targetFiles: map[string]string{"example.go": ""}, deleteFile: true, resolvedGo: "deleted", wantPushed: true},
		{name: "PR deletes a function", mergeCleanly: true, prGo: "package example\nfunc Target() {}\n", resolvedGo: "package example\nfunc Target() {}\n", wantPushed: true},
		{name: "target deletes a function", mergeCleanly: true, targetGo: "package example\nfunc PR() {}\n", resolvedGo: "package example\nfunc PR() {}\n", wantPushed: true},
		{name: "PR renames a function", mergeCleanly: true, prGo: "package example\nfunc Renamed() {}\nfunc Target() {}\n", resolvedGo: "package example\nfunc Renamed() {}\nfunc Target() {}\n", wantPushed: true},
		{name: "target renames a function", mergeCleanly: true, targetGo: "package example\nfunc PR() {}\nfunc Renamed() {}\n", resolvedGo: "package example\nfunc PR() {}\nfunc Renamed() {}\n", wantPushed: true},
		{name: "PR moves a function to another package", mergeCleanly: true, prGo: "package example\nfunc Target() {}\n", prFiles: map[string]string{"moved/example.go": "package moved\nfunc PR() {}\n"}, resolvedGo: "package example\nfunc Target() {}\n", wantPushed: true},
		{name: "target moves a function to another package", mergeCleanly: true, targetGo: "package example\nfunc PR() {}\n", targetFiles: map[string]string{"moved/example.go": "package moved\nfunc Target() {}\n"}, resolvedFiles: map[string]string{"moved/example.go": "package moved\nfunc Target() {}\n"}, resolvedGo: "package example\nfunc PR() {}\n", wantPushed: true},
		{name: "declarations added after the base", baseGo: "package example\n", prGo: "package example\nfunc PR() {}\n", targetGo: "package example\nfunc Target() {}\n", resolvedGo: "package example\nfunc PR() {}\n", wantPushed: true},
		{name: "generated templ declarations", goFile: "example_templ.go", baseGo: "// Code generated by templ - DO NOT EDIT.\npackage example\nfunc Target() {}\n", resolvedGo: "// Code generated by templ - DO NOT EDIT.\npackage example\n", wantPushed: true},
		{name: "generated sqlc declarations", baseGo: "// Code generated by sqlc. DO NOT EDIT.\npackage example\nfunc Target() {}\n", resolvedGo: "// Code generated by sqlc. DO NOT EDIT.\npackage example\n", wantPushed: true},
		{name: "unparseable generated resolution", baseGo: "// Code generated by templ - DO NOT EDIT.\npackage example\nfunc Target() {}\n", resolvedGo: "// Code generated by templ - DO NOT EDIT.\npackage example\nPARTIAL", wantPushed: true},
		{name: "renamed init file", baseGo: "package example\nfunc init() {}\nfunc init() {}\n", resolvedGo: "package example\nfunc init() {}\nfunc init() {}\n", moveFile: true, wantPushed: true},
		{name: "init moves within package", baseGo: "package example\nfunc init() {}\nfunc init() {}\n", resolvedGo: "package example\nfunc init() {}\n", resolvedFiles: map[string]string{"other.go": "package example\nfunc init() {}\n"}, wantPushed: true},
		{name: "drops one init", baseGo: "package example\nfunc init() {}\nfunc init() {}\n", resolvedGo: "package example\nfunc init() {}\n", wantMissing: "init"},
		{name: "unparseable PR blob", prGo: "//go:build fixture\n\npackage example\nPARTIAL", resolvedGo: "package example\nfunc PR() {}\nfunc Target() {}\n", wantPushed: true},
		{name: "unparseable target blob", targetGo: "package example\nPARTIAL", resolvedGo: "package example\nfunc PR() {}\nfunc Target() {}\n", wantPushed: true},
		{name: "unparseable base blob", baseGo: "package example\nPARTIAL", prGo: "package example\nfunc PR() {}\n", targetGo: "package example\nfunc Target() {}\n", resolvedGo: "package example\nfunc PR() {}\nfunc Target() {}\n", wantPushed: true},
		{name: "file rename preserves both parents", moveFile: true, resolvedGo: "package example\nfunc PR() {}\nfunc Target() {}\n", wantPushed: true},
		{name: "deleted file loses both parents", deleteFile: true, resolvedGo: "deleted", wantMissing: "PR", wantMissingCount: 2},
		{name: "invalid Go resolution", resolvedGo: "package example\nCUSTOMER_PRIVATE_CONTENT", wantError: "cannot parse Go declarations"},
		{name: "drops PR function", resolvedGo: "package example\nfunc Target() {}\n", wantMissing: "PR"},
		{name: "drops target function", resolvedGo: "package example\nfunc PR() {}\n", wantMissing: "Target"},
		{name: "preserves both parents", resolvedGo: "package example\nfunc PR() {}\nfunc Target() {}\n", wantPushed: true},
		{name: "renames a function", resolvedGo: "package example\nfunc Renamed() {}\nfunc Target() {}\n", description: "Rename `PR` to `Renamed`.", wantPushed: true},
		{name: "authorized removal", resolvedGo: "package example\nfunc PR() {}\n", description: "Remove `Target`.", wantPushed: true},
		{name: "negated removal", resolvedGo: "package example\nfunc PR() {}\n", description: "Do not remove `Target`.", wantMissing: "Target"},
		{name: "drops generic receiver method", baseGo: "package example\nfunc PR() {}\ntype Widget[T any] struct{}\nfunc (w *Widget[T]) Save() {}\n", resolvedGo: "package example\nfunc PR() {}\ntype Widget[T any] struct{}\n", wantMissing: "Widget.Save"},
		{name: "drops target test", goFile: "example_test.go", baseGo: "package example\nimport \"testing\"\nfunc PR() {}\nfunc TestTarget(t *testing.T) {}\n", resolvedGo: "package example\nfunc PR() {}\n", wantMissing: "TestTarget"},
		{name: "rename without replacement", resolvedGo: "package example\nfunc Target() {}\n", description: "Rename `PR` to `Renamed`.", wantMissing: "PR"},
		{name: "conditional removal is ambiguous", resolvedGo: "package example\nfunc PR() {}\n", description: "Remove `Target` if unused.", wantMissing: "Target"},
		{name: "shorter fence cannot authorize removal", resolvedGo: "package example\nfunc PR() {}\n", description: "````\n```\nRemove `Target`.\n````", wantMissing: "Target"},
		{name: "removal mentioned as an example", resolvedGo: "package example\nfunc PR() {}\n", description: "```\nRemove `Target`.\n```", wantMissing: "Target"},
		{name: "legacy rolling resolved head rejects failed gate", mode: gate.LandingRollingBarrier, gate: "exit 1", wantError: "merge-fallback verification gate failed", wantStage: gate.StageMergeFallbackVerification},
		{name: "resolved committed head", wantPushed: true},
		{name: "already pushed head", before: "push"},
		{name: "gate failure", gate: "git detent-invalid-gate", wantError: "merge-fallback verification gate failed", wantStage: gate.StageMergeFallbackVerification},
		{name: "push failure", before: "reject push", wantError: "push validated merge resolution"},
		{name: "dirty resolution", before: "dirty", wantError: "not source-clean"},
		{name: "stale target", before: "base", wantError: "does not contain"},
		{name: "replaced remote", before: "remote", wantError: "remote branch changed before"},
		{name: "gate changes head", gate: "git commit --allow-empty -m changed", wantError: "changed during"},
		{name: "gate dirties source", gate: "git rm README.md", wantError: "not source-clean"},
		{name: "gate changes branch", gate: "git checkout -b other", wantError: "owned workspace branch"},
		{name: "gate changes target", gate: "base", wantError: "does not contain"},
		{name: "gate replaces remote", gate: "remote", wantError: "changed during"},
	})
}

func testLocalGitPrepareMergeValidatesResolvedHead(t *testing.T, tests []mergeResolvedHeadCase) {
	t.Helper()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.gitFailure == "" {
				t.Parallel()
			}
			goFile := tt.goFile
			if goFile == "" {
				goFile = "example.go"
			}
			baseGo := tt.baseGo
			if baseGo == "" {
				baseGo = "package example\nfunc PR() {}\nfunc Target() {}\n"
			}
			prGo := tt.prGo
			if prGo == "" {
				prGo = baseGo
			}
			targetGo := tt.targetGo
			if targetGo == "" {
				targetGo = baseGo
			}
			writeGoFiles := func(directory string, files map[string]string) {
				t.Helper()
				for name, content := range files {
					if content == "" {
						runGit(t, directory, "rm", name)
						continue
					}
					if err := os.MkdirAll(filepath.Dir(filepath.Join(directory, name)), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
					runGit(t, directory, "add", name)
				}
			}
			source := initSourceRepo(t)
			if tt.resolvedGo != "" {
				writeGoFiles(source, map[string]string{goFile: baseGo})
				writeGoFiles(source, tt.baseFiles)
				runGit(t, source, "commit", "-m", "base behavior")
			}
			remote := initBareRemote(t)
			runGit(t, source, "remote", "add", "origin", remote)
			runGit(t, source, "push", "-u", "origin", "main")
			backend, err := NewBackend(KindLocalGit, LocalGitOptions{
				Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "DD-RESOLVED"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if tt.resolvedGo != "" {
				writeGoFiles(info.Path, map[string]string{goFile: prGo})
				writeGoFiles(info.Path, tt.prFiles)
				runGit(t, info.Path, "commit", "--allow-empty", "-m", "PR behavior")
				writeGoFiles(source, map[string]string{goFile: targetGo})
				writeGoFiles(source, tt.targetFiles)
				runGit(t, source, "commit", "--allow-empty", "-m", "target behavior")
				runGit(t, source, "push", "origin", "main")
			}
			runGit(t, info.Path, "push", "origin", "HEAD:"+info.Branch)
			expectedRemote := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			if tt.resolvedGo != "" {
				if tt.mergeCleanly {
					runGit(t, info.Path, "merge", "--no-edit", "main")
				} else {
					runGit(t, info.Path, "merge", "-s", "ours", "--no-edit", "main")
				}
				if tt.deleteFile {
					runGit(t, info.Path, "rm", "--ignore-unmatch", goFile)
				} else {
					if tt.moveFile {
						runGit(t, info.Path, "mv", goFile, "renamed.go")
						goFile = "renamed.go"
					}
					if err := os.WriteFile(filepath.Join(info.Path, goFile), []byte(tt.resolvedGo), 0o600); err != nil {
						t.Fatal(err)
					}
					runGit(t, info.Path, "add", goFile)
				}
			}
			writeGoFiles(info.Path, tt.resolvedFiles)
			runGit(t, info.Path, "commit", "--allow-empty", "-m", "resolved")
			head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			mutateRemote := func(branch string) {
				runGit(t, source, "commit", "--allow-empty", "-m", "external")
				runGit(t, source, "push", "origin", "HEAD:"+branch)
			}
			switch tt.before {
			case "push":
				runGit(t, info.Path, "push", "origin", "HEAD:"+info.Branch)
			case "dirty":
				runGit(t, info.Path, "rm", "README.md")
			case "base":
				mutateRemote("main")
			case "remote":
				mutateRemote(info.Branch)
			case "reject push":
				if err := os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			remoteBefore := strings.TrimSpace(runGit(t, source, "ls-remote", "origin", "refs/heads/"+info.Branch))
			command := "git config detent.validation passed"
			switch tt.gate {
			case "base", "remote":
				branch := "main"
				if tt.gate == "remote" {
					branch = info.Branch
				}
				relativeSource, err := filepath.Rel(info.Path, source)
				if err != nil {
					t.Fatal(err)
				}
				relativeSource = filepath.ToSlash(relativeSource)
				command += " && git -C " + relativeSource + " commit --allow-empty -m external && git -C " + relativeSource + " push origin HEAD:" + branch
			default:
				if tt.gate != "" {
					command += " && " + tt.gate
				}
			}
			if tt.gitFailure != "" {
				realGit, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				directory := t.TempDir()
				failure := "case \"$*\" in *" + shellQuote(" "+tt.gitFailure+" ") + "*) exit 29;; esac\n"
				if tt.gitFailure == "merge-base" {
					failure = "if [ \"$3\" = \"merge-base\" ] && [ \"$4\" != \"--is-ancestor\" ]; then exit 29; fi\n"
				}
				script := "#!/bin/sh\n" + failure + "exec " + shellQuote(realGit) + " \"$@\"\n"
				if err := os.WriteFile(filepath.Join(directory, "git"), []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			result, err := backend.(MergePreparer).PrepareMerge(t.Context(), info, issue, MergePrepareOptions{
				LandingMode: tt.mode, TargetBranch: "main", VerifyResolution: true, ValidationCommand: command, ExpectedRemoteHead: expectedRemote, IssueDescription: tt.description,
			})
			if tt.wantMissing != "" {
				count := tt.wantMissingCount
				if count == 0 {
					count = 1
				}
				if len(result.Findings) != count {
					t.Fatalf("missing declarations = %v, want %d", result.Findings, count)
				}
				if err != nil || result.Status != MergePrepareStatusConflict || !strings.Contains(strings.Join(result.Findings, "\n"), tt.wantMissing) {
					t.Fatalf("missing declaration verification = %#v, %v", result, err)
				}
				if got := strings.TrimSpace(runGit(t, source, "ls-remote", "origin", "refs/heads/"+info.Branch)); got != remoteBefore {
					t.Fatal("published a lossy resolution")
				}
				if out, _ := runGitAt(t.Context(), info.Path, "config", "--get", "detent.validation"); out != "" {
					t.Fatal("ran gate before refusing loss")
				}
				return
			}
			if tt.wantError != "" {
				if tt.gitFailure != "" {
					var commandErr *CommandError
					if !errors.As(err, &commandErr) || commandErr.ExitCode != 29 {
						t.Fatalf("Git error = %v, want wrapped exit code 29", err)
					}
				}
				if err != nil && strings.Contains(err.Error(), "CUSTOMER_PRIVATE_CONTENT") {
					t.Fatal("error exposed customer content")
				}
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("PrepareMerge() = %#v, %v; want error containing %q", result, err, tt.wantError)
				}
				if tt.wantStage != "" {
					var validation *ValidationError
					if !errors.As(err, &validation) || validation.Stage != tt.wantStage || validation.Error() != tt.wantError+": exit status 1" {
						t.Fatalf("PrepareMerge() validation = %+v, %v; want stage %q and message %q", validation, err, tt.wantStage, tt.wantError+": exit status 1")
					}
				}
				if tt.gate != "remote" {
					if got := strings.TrimSpace(runGit(t, source, "ls-remote", "origin", "refs/heads/"+info.Branch)); got != remoteBefore {
						t.Fatalf("remote changed on failed validation: %q, want %q", got, remoteBefore)
					}
				}
				return
			}
			if err != nil || result.Status != MergePrepareStatusClean || result.HeadSHA != head || result.HeadChanged != tt.wantPushed {
				t.Fatalf("PrepareMerge() = %#v, %v; want validated head %s, pushed %t", result, err, head, tt.wantPushed)
			}
			if got := strings.TrimSpace(runGit(t, info.Path, "config", "--get", "detent.validation")); got != "passed" {
				t.Fatalf("local gate evidence = %q", got)
			}

			if got := strings.Fields(runGit(t, source, "ls-remote", "origin", "refs/heads/"+info.Branch))[0]; got != head {
				t.Fatalf("remote head = %s, want %s", got, head)
			}
		})
	}
}

func TestLocalGitMergeFallbackRetryRevalidatesAheadHead(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name        string
		failedGate  bool
		advanceBase bool
	}{
		{name: "gate failure", failedGate: true},
		{name: "push rejection"},
		{name: "target advances after gate failure", failedGate: true, advanceBase: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			remote := initBareRemote(t)
			runGit(t, source, "remote", "add", "origin", remote)
			runGit(t, source, "push", "-u", "origin", "main")
			backend, err := NewBackend(KindLocalGit, LocalGitOptions{
				Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "DD-FALLBACK-RETRY"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("feature\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, info.Path, "add", "README.md")
			runGit(t, info.Path, "commit", "-m", "feature")
			runGit(t, info.Path, "push", "origin", "HEAD:"+info.Branch)
			remoteHead := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("main\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, source, "add", "README.md")
			runGit(t, source, "commit", "-m", "main conflict")
			runGit(t, source, "push", "origin", "main")
			preparer := backend.(MergePreparer)
			opts := MergePrepareOptions{TargetBranch: "main", ExpectedRemoteHead: remoteHead, ValidationCommand: "git config detent.validation passed"}
			precheck, err := preparer.PrepareMerge(t.Context(), info, issue, opts)
			if err != nil || precheck.Status != MergePrepareStatusConflict {
				t.Fatalf("conflict precheck = %#v, %v", precheck, err)
			}
			if _, err := runGitAt(t.Context(), info.Path, "merge", "--no-edit", "origin/main"); err == nil {
				t.Fatal("expected worker merge conflict")
			}
			if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("resolved\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, info.Path, "add", "README.md")
			runGit(t, info.Path, "commit", "-m", "resolve main conflict")
			resolvedHead := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			hook := filepath.Join(remote, "hooks", "pre-receive")
			if tt.failedGate {
				opts.ValidationCommand = "git detent-invalid-gate"
			} else if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			verify := opts
			verify.VerifyResolution = true
			if _, err := preparer.PrepareMerge(t.Context(), info, issue, verify); err == nil {
				t.Fatal("expected initial verification failure")
			}
			if tt.advanceBase {
				runGit(t, source, "commit", "--allow-empty", "-m", "advance target during retry")
				runGit(t, source, "push", "origin", "main")
			}
			retry, err := preparer.PrepareMerge(t.Context(), info, issue, opts)
			if tt.advanceBase {
				if err != nil || retry.Status != MergePrepareStatusConflict {
					t.Fatalf("advanced target retry = %#v, %v; want worker fallback", retry, err)
				}
			} else if err == nil {
				t.Fatal("retry published an unverified head")
			}
			if got := strings.Fields(runGit(t, source, "ls-remote", "origin", "refs/heads/"+info.Branch))[0]; got != remoteHead {
				t.Fatalf("remote head after failed retry = %s, want %s", got, remoteHead)
			}
			if got := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got != resolvedHead {
				t.Fatalf("local head after failed retry = %s, want %s", got, resolvedHead)
			}
			if tt.advanceBase {
				return
			}
			if !tt.failedGate {
				if err := os.Remove(hook); err != nil {
					t.Fatal(err)
				}
			}
			opts.ValidationCommand = "git config detent.validation retried"
			result, err := preparer.PrepareMerge(t.Context(), info, issue, opts)
			if err != nil || result.Status != MergePrepareStatusClean || result.HeadSHA != resolvedHead || !result.HeadChanged {
				t.Fatalf("validated retry = %#v, %v; want published resolved head", result, err)
			}
			if got := strings.TrimSpace(runGit(t, info.Path, "config", "--get", "detent.validation")); got != "retried" {
				t.Fatalf("retry gate evidence = %q", got)
			}
			if got := strings.Fields(runGit(t, source, "ls-remote", "origin", "refs/heads/"+info.Branch))[0]; got != resolvedHead {
				t.Fatalf("remote head after validated retry = %s, want %s", got, resolvedHead)
			}
		})
	}
}

func TestLocalGitQuarantineReleasesBranch(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"checked-out", "rebase", "am", "merge", "cherry-pick", "detached"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "quarantine-release"}
			first, err := backend.Create(context.Background(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "rebase" || operation == "am" || operation == "merge" || operation == "cherry-pick" {
				for _, dir := range []string{first.Path, source} {
					if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(dir+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					runGit(t, dir, "add", "README.md")
					runGit(t, dir, "commit", "-m", "conflicting change")
				}
				args := []string{operation, "main"}
				if operation == "am" {
					patch := filepath.Join(t.TempDir(), "change.patch")
					if err := os.WriteFile(patch, []byte(runGit(t, source, "format-patch", "-1", "--stdout")), 0o600); err != nil {
						t.Fatal(err)
					}
					args = []string{"am", "--3way", patch}
				}
				if _, err := runGitAt(context.Background(), first.Path, args...); err == nil {
					t.Fatal("want operation conflict")
				}
			}
			if operation == "detached" {
				runGit(t, first.Path, "switch", "--detach")
			}
			if err := os.WriteFile(filepath.Join(first.Path, "forensics.txt"), []byte("preserved\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			status := runGit(t, first.Path, "status", "--porcelain")
			readme := readFile(t, filepath.Join(first.Path, "README.md"))
			branchHead := strings.TrimSpace(runGit(t, source, "rev-parse", first.Branch))
			quarantined, err := backend.quarantineWorktree(context.Background(), first.Path)
			if err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, filepath.Join(quarantined, "forensics.txt")); got != "preserved\n" {
				t.Fatalf("forensics = %q", got)
			}
			if got := strings.TrimSpace(runGit(t, source, "rev-parse", first.Branch)); got != branchHead {
				t.Fatalf("branch moved: %s != %s", got, branchHead)
			}
			if got := runGit(t, quarantined, "status", "--porcelain"); got != status {
				t.Fatalf("quarantine changed index: %q != %q", got, status)
			}
			if got := readFile(t, filepath.Join(quarantined, "README.md")); got != readme {
				t.Fatal("quarantine changed tracked file")
			}
			fresh := filepath.Join(t.TempDir(), "fresh")
			runGit(t, source, "worktree", "add", fresh, first.Branch)
			runGit(t, source, "worktree", "remove", fresh)
			retry, err := backend.Create(context.Background(), issue)
			if err != nil {
				t.Fatalf("retry Create: %v", err)
			}
			if !retry.Created {
				t.Fatal("retry did not create workspace")
			}
		})
	}
}

func TestLocalGitStaleQuarantineReleasesBranch(t *testing.T) {
	t.Parallel()
	for _, failDetach := range []bool{false, true} {
		t.Run(fmt.Sprintf("detach_failure=%t", failDetach), func(t *testing.T) {
			source := initSourceRepo(t)
			root := filepath.Join(t.TempDir(), "workspaces")
			backend, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "DD-STALE-REUSE"}
			first, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			branch := strings.TrimSpace(runGit(t, first.Path, "branch", "--show-current"))
			head := runGit(t, first.Path, "rev-parse", "HEAD")
			if err := os.WriteFile(filepath.Join(first.Path, "progress.txt"), []byte("keep\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if failDetach {
				if err := os.WriteFile(filepath.Join(linkedWorktreeGitDir(t, first.Path), "HEAD.lock"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err = backend.recoverStaleSourceWorktree(t.Context(), first.Path, branch, branch)
			quarantined := singleQuarantinedWorkspace(t, root)
			if got := readFile(t, filepath.Join(quarantined, "progress.txt")); got != "keep\n" {
				t.Fatalf("preserved progress = %q", got)
			}
			if got := runGit(t, quarantined, "rev-parse", "HEAD"); got != head {
				t.Fatalf("HEAD = %q, want %q", got, head)
			}
			if failDetach {
				canonicalQuarantine := mustCanonicalExistingPath(t, quarantined)
				if err == nil || !strings.Contains(err.Error(), "detach quarantined worktree HEAD") || !strings.Contains(err.Error(), strconv.Quote(canonicalQuarantine)) {
					t.Fatalf("recovery error = %v, want detach failure and quarantine path", err)
				}
				if _, err := os.Stat(first.Path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("replacement path exists: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			second, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatalf("same-pass branch reuse: %v", err)
			}
			if got := strings.TrimSpace(runGit(t, second.Path, "branch", "--show-current")); got != branch {
				t.Fatalf("replacement branch = %q, want %q", got, branch)
			}
			if got := strings.TrimSpace(runGit(t, quarantined, "branch", "--show-current")); got != "" {
				t.Fatalf("quarantine branch = %q, want detached", got)
			}
		})
	}
}

// hookCompletionTimeout is a deadlock budget for shell hooks, not a timeout
// behavior assertion. Allow extra scheduling time when tests oversubscribe CPUs.
func hookCompletionTimeout() time.Duration {
	parallel, err := strconv.Atoi(flag.Lookup("test.parallel").Value.String())
	if err != nil {
		panic(err)
	}
	return hookCompletionBudget(parallel, runtime.GOMAXPROCS(0))
}

func hookCompletionBudget(parallel, procs int) time.Duration {
	return 10 * time.Second * time.Duration(max(1, (parallel+procs-1)/procs))
}

func TestHookCompletionBudget(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name            string
		parallel, procs int
		want            time.Duration
	}{
		{"serial", 1, 8, 10 * time.Second},
		{"one per CPU", 8, 8, 10 * time.Second},
		{"oversubscribed", 8, 2, 40 * time.Second},
		{"round up", 5, 2, 30 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := hookCompletionBudget(tt.parallel, tt.procs); got != tt.want {
				t.Fatalf("budget = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLocalGitPrepareMergeValidatesTheCleanHeadItPushes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		validate      bool
		command       string
		wantStatus    MergePrepareStatus
		wantValidated bool
		wantPublished bool
		mode          string
	}{
		{name: "gate passes on the rebased head", validate: true, command: "test -f main.txt", wantStatus: MergePrepareStatusClean, wantValidated: true, wantPublished: true},
		{name: "gate fails on the rebased head", validate: true, command: "test ! -f main.txt", wantStatus: MergePrepareStatusConflict},
		{name: "legacy rolling mode rejects failed validation", mode: gate.LandingRollingBarrier, validate: true, command: "false", wantStatus: MergePrepareStatusConflict},
		{name: "validation not requested", validate: false, command: "false", wantStatus: MergePrepareStatusClean, wantPublished: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			remote := initBareRemote(t)
			runGit(t, source, "remote", "add", "origin", remote)
			runGit(t, source, "push", "-u", "origin", "main")
			backend, err := NewBackend(KindLocalGit, LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatalf("NewBackend() error = %v", err)
			}
			preparer := backend.(MergePreparer)
			issue := Issue{Identifier: "DD-GATE"}
			info, err := backend.Create(context.Background(), issue)
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			if err := os.WriteFile(filepath.Join(info.Path, "feature.txt"), []byte("feature\n"), 0o600); err != nil {
				t.Fatalf("write feature: %v", err)
			}
			runGit(t, info.Path, "add", "feature.txt")
			runGit(t, info.Path, "commit", "-m", "feature")
			runGit(t, info.Path, "push", "origin", "HEAD:refs/heads/"+info.Branch)
			published := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			if err := os.WriteFile(filepath.Join(source, "main.txt"), []byte("main\n"), 0o600); err != nil {
				t.Fatalf("write main: %v", err)
			}
			runGit(t, source, "add", "main.txt")
			runGit(t, source, "commit", "-m", "main change")
			runGit(t, source, "push", "origin", "main")

			result, err := preparer.PrepareMerge(context.Background(), info, issue, MergePrepareOptions{LandingMode: tt.mode, ValidateHead: tt.validate, ValidationCommand: tt.command, ExpectedRemoteHead: published})
			if err != nil {
				t.Fatalf("PrepareMerge() error = %v", err)
			}
			if result.Status != tt.wantStatus || result.Validated != tt.wantValidated {
				t.Fatalf("PrepareMerge() = status %q validated %v, want %q %v (%s)", result.Status, result.Validated, tt.wantStatus, tt.wantValidated, result.Message)
			}
			local := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			remoteHead := strings.Fields(runGit(t, source, "ls-remote", "origin", "refs/heads/"+info.Branch))[0]
			if tt.wantPublished {
				if remoteHead != local || result.HeadSHA != local || local == published {
					t.Fatalf("published head: remote %s, local %s, result %s, before %s", remoteHead, local, result.HeadSHA, published)
				}
				return
			}
			if local != published || remoteHead != published || result.HeadSHA != "" {
				t.Fatalf("a failed gate must leave the published head: local %s, remote %s, result %q, want %s", local, remoteHead, result.HeadSHA, published)
			}
			if !strings.Contains(result.Message, "configured gate failed") {
				t.Fatalf("conflict message = %q", result.Message)
			}
		})
	}
}

func TestPrepareWorkerScratchIsOutsideWorkspace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		workspace func(t *testing.T) string
	}{
		{name: "plain directory", workspace: func(t *testing.T) string { return t.TempDir() }},
		{name: "git repository", workspace: initSourceRepo},
		{name: "legacy scratch present", workspace: func(t *testing.T) string {
			path := t.TempDir()
			if err := os.MkdirAll(filepath.Join(path, filepath.FromSlash(workerScratchRelativePath), "attempt-legacy"), 0o700); err != nil {
				t.Fatal(err)
			}
			return path
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			workspacePath := tt.workspace(t)
			scratchPath, err := PrepareWorkerScratch(t.Context(), workspacePath)
			if err != nil {
				t.Fatalf("PrepareWorkerScratch() error = %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(WorkerScratchRoot(workspacePath)) })
			canonicalWorkspace := mustCanonicalExistingPath(t, workspacePath)
			if pathWithin(canonicalWorkspace, scratchPath) {
				t.Fatalf("scratch path %q is inside workspace %q", scratchPath, canonicalWorkspace)
			}
			if !pathWithin(workerScratchBase(), scratchPath) {
				t.Fatalf("scratch path %q is outside the OS temp scratch base %q", scratchPath, workerScratchBase())
			}
			if filepath.Dir(scratchPath) != WorkerScratchRoot(workspacePath) || !strings.HasPrefix(filepath.Base(scratchPath), "attempt-") {
				t.Fatalf("scratch path %q is not an attempt of %q", scratchPath, WorkerScratchRoot(workspacePath))
			}
			info, err := os.Stat(scratchPath)
			if err != nil || !info.IsDir() {
				t.Fatalf("scratch stat = %v, %v", info, err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
				t.Fatalf("scratch mode = %v, want 0700", info.Mode().Perm())
			}
			if err := CleanupWorkerScratch(workspacePath, scratchPath); err != nil {
				t.Fatalf("CleanupWorkerScratch() error = %v", err)
			}
			if _, err := os.Stat(scratchPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("scratch remains after cleanup: %v", err)
			}
		})
	}
}

func TestPrepareWorkerScratchIgnoresAnotherAccountsScratchBase(t *testing.T) {
	tempRoot := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, tempRoot)
	}
	if err := os.Mkdir(filepath.Join(tempRoot, workerScratchBaseName), 0o500); err != nil {
		t.Fatal(err)
	}
	workspacePath := t.TempDir()
	scratchPath, err := PrepareWorkerScratch(t.Context(), workspacePath)
	if err != nil {
		t.Fatalf("PrepareWorkerScratch() error = %v, want scratch outside the shared base another account owns", err)
	}
	if err := CleanupWorkerScratch(workspacePath, scratchPath); err != nil {
		t.Fatalf("CleanupWorkerScratch() error = %v", err)
	}
}

func TestWorkerScratchRootSeparatesWorkspaces(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	tests := []struct {
		name  string
		left  string
		right string
		same  bool
	}{
		{name: "same workspace", left: filepath.Join(parent, "a"), right: filepath.Join(parent, "a") + string(filepath.Separator), same: true},
		{name: "sibling workspaces", left: filepath.Join(parent, "a"), right: filepath.Join(parent, "b")},
		{name: "same name different roots", left: filepath.Join(parent, "one", "a"), right: filepath.Join(parent, "two", "a")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			left, right := WorkerScratchRoot(tt.left), WorkerScratchRoot(tt.right)
			if (left == right) != tt.same {
				t.Fatalf("WorkerScratchRoot(%q) = %q, WorkerScratchRoot(%q) = %q, want same %v", tt.left, left, tt.right, right, tt.same)
			}
			for _, root := range []string{left, right} {
				if pathWithin(parent, root) {
					t.Fatalf("scratch root %q is inside the workspace tree %q", root, parent)
				}
			}
		})
	}
}

func TestCleanupOwnedPathRemovesExternalWorkerScratch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		removeWorkspace  bool
		otherWorkspace   bool
		shared           bool
		wantErr          bool
		wantScratchGone  bool
		wantParentExists bool
	}{
		{name: "workspace present", wantScratchGone: true, wantParentExists: true},
		{name: "shared base scratch", shared: true, wantScratchGone: true, wantParentExists: true},
		{name: "shared base scratch with workspace removed", shared: true, removeWorkspace: true, wantScratchGone: true, wantParentExists: true},
		{name: "workspace removed", removeWorkspace: true, wantScratchGone: true, wantParentExists: true},
		{name: "other workspace scratch", otherWorkspace: true, wantErr: true, wantParentExists: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			t.Cleanup(func() { _ = os.RemoveAll(workerScratchGroup(parent)) })
			workspacePath := filepath.Join(parent, "workspace")
			otherPath := filepath.Join(parent, "other")
			for _, path := range []string{workspacePath, otherPath} {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(WorkerScratchRoot(path)) })
			}
			owner := workspacePath
			if tt.otherWorkspace {
				owner = otherPath
			}
			scratchPath, err := PrepareWorkerScratch(t.Context(), owner)
			if err != nil {
				t.Fatal(err)
			}
			if tt.shared {
				group := workerScratchGroupIn(sharedWorkerScratchBase(), bestEffortCanonicalPath(parent))
				t.Cleanup(func() { _ = os.RemoveAll(group) })
				scratchPath = filepath.Join(group, "workspace", "attempt-test")
				if err := ensurePrivateDirectories(sharedWorkerScratchBase(), scratchPath); err != nil {
					t.Fatal(err)
				}
			}
			if tt.removeWorkspace {
				if err := os.RemoveAll(workspacePath); err != nil {
					t.Fatal(err)
				}
			}
			err = CleanupOwnedPath(workspacePath, scratchPath)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CleanupOwnedPath() error = %v, want error %v", err, tt.wantErr)
			}
			if _, err := os.Stat(scratchPath); errors.Is(err, os.ErrNotExist) != tt.wantScratchGone {
				t.Fatalf("scratch removed = %v, want %v", err, tt.wantScratchGone)
			}
			if _, err := os.Stat(filepath.Dir(scratchPath)); (err == nil) != tt.wantParentExists {
				t.Fatalf("scratch parent exists = %v, want %v", err, tt.wantParentExists)
			}
		})
	}
}

func TestRemoveWorkerScratchRootKeepsSharedGroup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		sibling bool
	}{
		{name: "only workspace"},
		{name: "sibling scratch remains", sibling: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			workspacePath := filepath.Join(parent, "workspace")
			siblingPath := filepath.Join(parent, "sibling")
			t.Cleanup(func() { _ = os.RemoveAll(workerScratchGroup(parent)) })
			paths := []string{workspacePath}
			if tt.sibling {
				paths = append(paths, siblingPath)
			}
			for _, path := range paths {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				scratch, err := PrepareWorkerScratch(t.Context(), path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(scratch, "retained"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := RemoveWorkerScratchRoot(workspacePath); err != nil {
				t.Fatalf("RemoveWorkerScratchRoot() error = %v", err)
			}
			if _, err := os.Stat(WorkerScratchRoot(workspacePath)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("scratch root remains: %v", err)
			}
			if info, err := os.Stat(workerScratchGroup(parent)); err != nil || !info.IsDir() {
				t.Fatalf("shared scratch group stat = %v, %v", info, err)
			}
			if tt.sibling {
				if entries, err := os.ReadDir(WorkerScratchRoot(siblingPath)); err != nil || len(entries) != 1 {
					t.Fatalf("sibling attempts = %v, %v", entries, err)
				}
			}
		})
	}
}

func TestPrepareWorkerScratchToleratesConcurrentSiblingCleanup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		sameWorkspace bool
		removeRoot    bool
	}{
		{name: "sibling attempts"},
		{name: "same workspace attempts", sameWorkspace: true},
		{name: "sibling workspace roots", removeRoot: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			t.Cleanup(func() { _ = os.RemoveAll(workerScratchGroup(parent)) })
			var wg sync.WaitGroup
			errs := make(chan error, 8)
			for i := range 8 {
				name := strconv.Itoa(i)
				if tt.sameWorkspace {
					name = "workspace"
				}
				workspacePath := filepath.Join(parent, name)
				if err := os.MkdirAll(workspacePath, 0o700); err != nil {
					t.Fatal(err)
				}
				wg.Go(func() {
					for range 50 {
						scratch, err := PrepareWorkerScratch(t.Context(), workspacePath)
						if err == nil {
							if tt.removeRoot {
								err = RemoveWorkerScratchRoot(workspacePath)
							} else {
								err = CleanupWorkerScratch(workspacePath, scratch)
							}
						}
						if err != nil {
							errs <- err
							return
						}
					}
				})
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Errorf("concurrent scratch lifecycle: %v", err)
			}
		})
	}
}

func TestWorkerArtifactCleanupRootScratchBases(t *testing.T) {
	t.Parallel()
	workspacePath := filepath.Join(bestEffortCanonicalPath(t.TempDir()), "workspace")
	shared := filepath.Join(workerScratchGroupIn(sharedWorkerScratchBase(), filepath.Dir(workspacePath)), "workspace")
	for _, test := range []struct {
		name, parent string
		refused      bool
	}{
		{name: "per-user base", parent: WorkerScratchRoot(workspacePath)},
		{name: "base shared by an earlier runner", parent: shared},
		{name: "unrelated temporary directory", parent: filepath.Join(bestEffortCanonicalPath(os.TempDir()), "unrelated"), refused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root, err := WorkerArtifactCleanupRoot(workspacePath, filepath.Join(test.parent, "attempt-test"))
			if test.refused {
				if err == nil {
					t.Fatalf("cleanup root = %q, want refusal", root)
				}
				return
			}
			if err != nil || root != test.parent {
				t.Fatalf("cleanup root = %q, %v; want %q", root, err, test.parent)
			}
		})
	}
}
