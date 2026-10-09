package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseDiffStat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		output  string
		want    DiffStat
		wantErr bool
	}{
		{name: "empty", output: "", want: DiffStat{}},
		{
			name: "full stat output",
			output: " README.md | 1 -\n added.txt | 2 ++\n" +
				" 2 files changed, 2 insertions(+), 1 deletion(-)\n",
			want: DiffStat{Files: 2, Added: 2, Removed: 1},
		},
		{
			name:   "insertions only",
			output: " 1 file changed, 5 insertions(+)\n",
			want:   DiffStat{Files: 1, Added: 5},
		},
		{
			name:   "deletions only",
			output: " 3 files changed, 8 deletions(-)\n",
			want:   DiffStat{Files: 3, Removed: 8},
		},
		{
			name:   "no line changes",
			output: " 1 file changed\n",
			want:   DiffStat{Files: 1},
		},
		{
			name:    "malformed",
			output:  "not a diff stat\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseDiffStat(tt.output)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ParseDiffStat() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDiffStat() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ParseDiffStat() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLocalGitSeedReviewHead(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	for _, tt := range []struct {
		name       string
		advance    bool
		dirty      bool
		missingRef bool
		wantErr    bool
	}{
		{name: "stable head"},
		{name: "missing PR ref with published branch", missingRef: true, wantErr: true},
		{name: "matching head with dirty review workspace", dirty: true, wantErr: true},
		{name: "head changes before checkout", advance: true},
		{name: "dirty review workspace", advance: true, dirty: true, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := initSourceRepo(t)
			remote := filepath.Join(t.TempDir(), "origin.git")
			runGit(t, t.TempDir(), "clone", "--bare", source, remote)
			runGit(t, source, "remote", "add", "origin", remote)
			backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "review-head"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			a := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			runGit(t, info.Path, "push", "-u", "origin", info.Branch)
			wanted := a
			if tt.advance {
				writer := filepath.Join(t.TempDir(), "writer")
				runGit(t, t.TempDir(), "clone", "--branch", info.Branch, remote, writer)
				if err := os.WriteFile(filepath.Join(writer, "review.txt"), []byte("B\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, writer, "add", "review.txt")
				runGit(t, writer, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "B")
				wanted = strings.TrimSpace(runGit(t, writer, "rev-parse", "HEAD"))
				runGit(t, writer, "push", "origin", info.Branch)
			}
			if tt.dirty {
				if err := os.WriteFile(filepath.Join(info.Path, "dirty.txt"), []byte("local"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			issue.PullRequestHeadSHA = wanted
			if tt.missingRef {
				issue.PullRequestNumber = 4681
				issue.PullRequestBranch = info.Branch
			}
			err = backend.SeedReviewHead(t.Context(), info, issue)
			if (err != nil) != tt.wantErr {
				t.Fatalf("SeedReviewHead() error = %v, want error %t", err, tt.wantErr)
			}
			if tt.missingRef {
				var refusal *LandRefusal
				var command *CommandError
				if !errors.As(err, &refusal) || refusal.Kind != LandRefusalMissingHead || !errors.As(err, &command) || command.ExitCode != 128 || !strings.Contains(refusal.Reason, "origin ref refs/pull/4681/head") || !strings.Contains(refusal.Reason, "fatal: couldn't find remote ref refs/pull/4681/head") {
					t.Fatalf("missing PR ref error = %v, want refusal with Git stderr", err)
				}
			}
			head, err := backend.Head(t.Context(), info, issue)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantErr {
				wanted = a
			}
			if strings.TrimSpace(head) != wanted {
				t.Fatalf("HEAD = %s, want %s", head, wanted)
			}
		})
	}
}

func TestLocalGitVerifyReviewTreeAfterSeeding(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	source := initSourceRepo(t)
	if runtime.GOOS != "windows" {
		script, err := os.ReadFile("../../scripts/check-evidence.sh")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "check-evidence.sh"), script, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "Makefile"), []byte("lint:\n\t@echo lint-evidence\n\t@exit $${DETENT_CHECK_EVIDENCE_FIXTURE_EXIT:-0}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, source, "add", "check-evidence.sh", "Makefile")
		runGit(t, source, "commit", "-m", "check evidence fixture")
	}
	backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	issue := Issue{Identifier: "review-tree"}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.VerifyReviewTree(t.Context(), info, issue); err != nil {
		t.Fatalf("clean review tree: %v", err)
	}
	head, err := backend.Head(t.Context(), info, issue)
	if err != nil {
		t.Fatal(err)
	}
	issue.PullRequestHeadSHA = strings.TrimSpace(head)
	failedCommand := "echo failed; exit 7"
	if runtime.GOOS == "windows" {
		failedCommand = "echo failed & exit /b 7"
	}
	for _, test := range []struct {
		name, command, head string
		exit                int
		wantErr             bool
	}{
		{name: "passing observed output", command: "echo verified"},
		{name: "failed command", command: failedCommand, exit: 7},
		{name: "wrong immutable head", command: "echo must-not-run", head: strings.Repeat("f", 40), wantErr: true},
		{name: "missing command", wantErr: true},
		{name: "dirty result", command: "echo changed > untracked.txt", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := issue
			if test.head != "" {
				request.PullRequestHeadSHA = test.head
			}
			receipt, err := backend.RunReviewCommand(t.Context(), info, request, test.command)
			if (err != nil) != test.wantErr {
				t.Fatalf("receipt=%+v error=%v", receipt, err)
			}
			if !test.wantErr && (receipt.Command != test.command || receipt.HeadSHA != issue.PullRequestHeadSHA || receipt.TreeSHA == "" || receipt.ExitCode != test.exit || receipt.Output == "") {
				t.Fatalf("missing command evidence: %+v", receipt)
			}
		})
	}
	if err := os.Remove(filepath.Join(info.Path, "untracked.txt")); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, resultCode := range []string{"0", "7"} {
			t.Run("command scope result "+resultCode, func(t *testing.T) {
				t.Setenv("DETENT_CHECK_EVIDENCE_FIXTURE_EXIT", resultCode)
				receipt, err := backend.RunReviewCommand(t.Context(), info, issue, "bash -c 'source ./check-evidence.sh; check_with_evidence lint make lint'")
				if err != nil || receipt.Evidence == nil || len(receipt.Evidence.Checks) != 1 {
					t.Fatalf("lost execution evidence: %+v %v", receipt, err)
				}
				checked := receipt.Evidence.Checks[0]
				if checked.HeadSHA != receipt.HeadSHA || checked.TreeSHA != receipt.TreeSHA || checked.ExitCode != receipt.ExitCode || checked.Scope != "lint" || !checked.Environment.Known() || checked.DurationResolutionNS != 1e9 {
					t.Fatalf("inaccurate execution evidence: %+v %+v", receipt, checked)
				}
				if (resultCode == "0") != (receipt.ExitCode == 0) {
					t.Fatalf("failed command result not preserved: %+v", receipt)
				}
			})
		}
	}
	if err := os.WriteFile(filepath.Join(info.Path, "untracked.txt"), []byte("hook change"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backend.VerifyReviewTree(t.Context(), info, issue); err == nil {
		t.Fatal("untracked hook change accepted")
	}
}

func TestLocalGitSeedReviewHeadUsesPullRequestRef(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	source := initSourceRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, t.TempDir(), "clone", "--bare", source, remote)
	runGit(t, source, "remote", "add", "origin", remote)
	backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	issue := Issue{Identifier: "review-pr-ref"}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	writer := filepath.Join(t.TempDir(), "writer")
	runGit(t, t.TempDir(), "clone", remote, writer)
	if err := os.WriteFile(filepath.Join(writer, "review.txt"), []byte("PR head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, writer, "add", "review.txt")
	runGit(t, writer, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "PR head")
	wanted := strings.TrimSpace(runGit(t, writer, "rev-parse", "HEAD"))
	runGit(t, writer, "push", "origin", "HEAD:refs/pull/119/head")
	issue.PullRequestHeadSHA = wanted
	issue.PullRequestNumber = 119
	issue.PullRequestBranch = "human-authored-branch"
	if err := backend.SeedReviewHead(t.Context(), info, issue); err != nil {
		t.Fatal(err)
	}
	head, err := backend.Head(t.Context(), info, issue)
	if err != nil || strings.TrimSpace(head) != wanted {
		t.Fatalf("HEAD = %q, error = %v; want %q", head, err, wanted)
	}
}

func TestLocalGitSeedReviewHeadUsesPullRequestRepository(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	source := initSourceRepo(t)
	fork := filepath.Join(t.TempDir(), "fork.git")
	base := filepath.Join(t.TempDir(), "base.git")
	runGit(t, t.TempDir(), "clone", "--bare", source, fork)
	runGit(t, t.TempDir(), "clone", "--bare", source, base)
	runGit(t, source, "remote", "add", "origin", fork)
	backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	issue := Issue{Identifier: "review-base-ref"}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	writer := filepath.Join(t.TempDir(), "writer")
	runGit(t, t.TempDir(), "clone", base, writer)
	if err := os.WriteFile(filepath.Join(writer, "review.txt"), []byte("PR head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, writer, "add", "review.txt")
	runGit(t, writer, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "PR head")
	wanted := strings.TrimSpace(runGit(t, writer, "rev-parse", "HEAD"))
	runGit(t, writer, "push", "origin", "HEAD:refs/pull/119/head")
	runGit(t, info.Path, "config", "url."+base+".insteadOf", "https://github.com/acme/base.git")
	issue.PullRequestRepository = "acme/base"
	issue.PullRequestHeadSHA = wanted
	issue.PullRequestNumber = 119
	if err := backend.SeedReviewHead(t.Context(), info, issue); err != nil {
		t.Fatal(err)
	}
	head, err := backend.Head(t.Context(), info, issue)
	if err != nil || strings.TrimSpace(head) != wanted {
		t.Fatalf("HEAD = %q, error = %v; want %q", head, err, wanted)
	}
}

func TestLocalGitDiffStat(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

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

	info, err := backend.Create(context.Background(), Issue{Identifier: "DD-DIFF"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
	before := time.Now()
	clean, err := backend.DiffStat(context.Background(), info, Issue{Identifier: "DD-DIFF"})
	if err != nil {
		t.Fatalf("clean DiffStat() error = %v", err)
	}
	if !clean.IsEmpty() || clean.HeadSHA != head || clean.HeadObservedAt.Before(before) || clean.HeadObservedAt.After(time.Now()) {
		t.Fatalf("clean DiffStat() = %+v, want clean observation of %s", clean, head)
	}
	before = time.Now()
	runGit(t, info.Path, "commit", "--allow-empty", "-m", "change checked-out head")
	head = strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
	changedHead, err := backend.DiffStat(t.Context(), info, Issue{Identifier: "DD-DIFF"})
	if err != nil || !changedHead.IsEmpty() || changedHead.HeadSHA != head || changedHead.HeadSHA == clean.HeadSHA || !changedHead.HeadObservedAt.After(clean.HeadObservedAt) || changedHead.HeadObservedAt.Before(before) {
		t.Fatalf("changed head DiffStat() = %+v, %v", changedHead, err)
	}

	if err := os.WriteFile(filepath.Join(info.Path, "added.txt"), []byte("first\nsecond\n"), 0o600); err != nil {
		t.Fatalf("write added file: %v", err)
	}
	if err := os.Remove(filepath.Join(info.Path, "README.md")); err != nil {
		t.Fatalf("remove README.md: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(info.Path, ".detent"), 0o700); err != nil {
		t.Fatalf("mkdir .detent: %v", err)
	}
	for _, name := range []string{"notes.md", "lessons.md"} {
		if err := os.WriteFile(filepath.Join(info.Path, ".detent", name), []byte("handoff\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(info.Path, ".detent", "tmp"), 0o700); err != nil {
		t.Fatalf("mkdir worker scratch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, ".detent", "tmp", "scratch"), []byte("temporary\n"), 0o600); err != nil {
		t.Fatalf("write worker scratch: %v", err)
	}

	trace := filepath.Join(t.TempDir(), "git-events.json")
	t.Setenv("GIT_TRACE2_EVENT", trace)
	got, err := backend.DiffStat(context.Background(), info, Issue{Identifier: "DD-DIFF"})
	if err != nil {
		t.Fatalf("DiffStat() error = %v", err)
	}
	if got.Files != 4 || got.Added != 4 || got.Removed != 1 || got.Fingerprint == "" {
		t.Fatalf("DiffStat() = %+v, want 4 files, 4 added, 1 removed, and a fingerprint", got)
	}
	if got.HeadSHA != head || !got.HeadObservedAt.After(changedHead.HeadObservedAt) {
		t.Fatalf("nonempty head observation = %+v", got)
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	commands := 0
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var event struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Event != "start" {
			continue
		}
		commands++
		if slices.Contains(event.Argv, "diff") && !slices.Contains(event.Argv, head) {
			t.Fatalf("diff is not pinned to captured head: %v", event.Argv)
		}
	}
	if commands != 4 {
		t.Fatalf("Git processes per nonempty refresh = %d, want 4", commands)
	}
	originalFingerprint := got.Fingerprint

	if err := os.WriteFile(filepath.Join(info.Path, "added.txt"), []byte("third\nfourth\n"), 0o600); err != nil {
		t.Fatalf("rewrite added file: %v", err)
	}
	contentChanged, err := backend.DiffStat(context.Background(), info, Issue{Identifier: "DD-DIFF"})
	if err != nil {
		t.Fatalf("content-changed DiffStat() error = %v", err)
	}
	if contentChanged.Files != got.Files || contentChanged.Added != got.Added || contentChanged.Removed != got.Removed {
		t.Fatalf("content-changed DiffStat() = %+v, want unchanged counts from %+v", contentChanged, got)
	}
	if contentChanged.Fingerprint == originalFingerprint {
		t.Fatalf("content-changed fingerprint = %q, want different from original", contentChanged.Fingerprint)
	}

	if err := os.Remove(filepath.Join(info.Path, "added.txt")); err != nil {
		t.Fatalf("remove added file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, "other.txt"), []byte("third\nfourth\n"), 0o600); err != nil {
		t.Fatalf("write other file: %v", err)
	}
	fileSetChanged, err := backend.DiffStat(context.Background(), info, Issue{Identifier: "DD-DIFF"})
	if err != nil {
		t.Fatalf("file-set-changed DiffStat() error = %v", err)
	}
	if fileSetChanged.Files != contentChanged.Files || fileSetChanged.Added != contentChanged.Added || fileSetChanged.Removed != contentChanged.Removed {
		t.Fatalf("file-set-changed DiffStat() = %+v, want unchanged counts from %+v", fileSetChanged, contentChanged)
	}
	if fileSetChanged.Fingerprint == contentChanged.Fingerprint {
		t.Fatalf("file-set-changed fingerprint = %q, want different from content-changed fingerprint", fileSetChanged.Fingerprint)
	}

	status := runGit(t, info.Path, "status", "--short")
	if !strings.Contains(status, "?? other.txt") {
		t.Fatalf("git status = %q, want other.txt to remain untracked", status)
	}
	if !strings.Contains(runGit(t, info.Path, "status", "--short", "--untracked-files=all"), ".detent/notes.md") {
		t.Fatal("diagnostics changed repository ignore rules for runtime files")
	}
	for _, relative := range []bool{false, true} {
		t.Run(fmt.Sprintf("index pathname/relative=%t", relative), func(t *testing.T) {
			name := " index pathname "
			if runtime.GOOS != "windows" {
				name += "\nfinal record\n "
			}
			path := filepath.Join(t.TempDir(), name)
			if relative {
				var err error
				path, err = filepath.Rel(info.Path, path)
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("GIT_INDEX_FILE", path)
			want := path
			if relative {
				want = filepath.Join(info.Path, path)
			}
			got, err := gitIndexPath(t.Context(), info.Path)
			if err != nil || got != want {
				t.Fatalf("path-only index=%q, %v; want %q", got, err, want)
			}
			index, err := gitIndexLookup(t.Context(), info.Path, true)
			if err != nil || index.Path != want || index.HeadSHA != head || index.HeadObservedAt.IsZero() {
				t.Fatalf("index observation=%+v, %v", index, err)
			}
		})
	}
	for _, format := range []string{"sha1", "sha256"} {
		t.Run("object format/"+format, func(t *testing.T) {
			path := t.TempDir()
			runGit(t, path, "init", "--object-format="+format)
			runGit(t, path, "config", "user.name", "Test User")
			runGit(t, path, "config", "user.email", "test@example.com")
			writeFileDiffFile(t, path, "file.txt", "base\n")
			runGit(t, path, "add", "file.txt")
			runGit(t, path, "commit", "-m", "base")
			want := strings.TrimSpace(runGit(t, path, "rev-parse", "HEAD"))
			stat, err := GitDiffStat(t.Context(), path)
			if err != nil || !stat.IsEmpty() || stat.HeadSHA != want || stat.HeadObservedAt.IsZero() {
				t.Fatalf("%s observation=%+v, %v", format, stat, err)
			}
		})
	}
}

// Diagnostics in linked worktrees must not rewrite their shared exclusions or
// the worker's real index, and must filter runtime paths even when tracked.
func TestWorkspaceDiagnosticsPreserveSharedGitMetadata(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	source := initSourceRepo(t)
	name := " source whitespace"
	if runtime.GOOS != "windows" {
		name += " \npath\n "
	}
	moved := filepath.Join(t.TempDir(), name)
	if err := os.Rename(source, moved); err != nil {
		t.Fatal(err)
	}
	source = moved
	writeFileDiffFile(t, source, ".detent/notes.md", "old human knowledge\n")
	writeFileDiffFile(t, source, ".detent/tmp/tracked", "old scratch\n")
	runGit(t, source, "add", ".detent/notes.md", ".detent/tmp/tracked")
	runGit(t, source, "commit", "-m", "tracked documentation and scratch fixture")
	excludePath := filepath.Join(source, ".git", "info", "exclude")
	exclude := "# operator exclusions without a final newline\n*.operator-local\n.detent/worker-tmp"
	writeFileDiffFile(t, source, ".git/info/exclude", exclude)
	before, err := os.Stat(excludePath)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	worktrees := []string{filepath.Join(root, "first"), filepath.Join(root, "second")}
	indexes := make([][]byte, len(worktrees))
	indexPaths := make([]string, len(worktrees))
	for i, path := range worktrees {
		runGit(t, source, "worktree", "add", "--detach", path, "HEAD")
		resolved := strings.TrimSpace(runGit(t, path, "rev-parse", "--git-path", "info/exclude"))
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(path, resolved)
		}
		shared, err := os.Stat(resolved)
		if err != nil || !os.SameFile(before, shared) {
			t.Fatalf("linked worktree does not share info/exclude: %v", err)
		}
		writeFileDiffFile(t, path, "README.md", "source repo\nchanged\n")
		writeFileDiffFile(t, path, "new file.txt", "worktree "+filepath.Base(path)+"\n")
		writeFileDiffFile(t, path, "secret.operator-local", "ignored\n")
		for _, document := range []string{".detent/notes.md", ".detent/lessons.md"} {
			writeFileDiffFile(t, path, document, "human knowledge\n")
		}
		if i == 1 && runtime.GOOS != "windows" {
			if err := os.Symlink(t.TempDir(), filepath.Join(path, ".detent", "worker-tmp")); err != nil {
				t.Fatal(err)
			}
		}
		for _, runtime := range []string{".detent/tmp/tracked", ".detent/tmp/nested/scratch", ".detent/worker-tmp/attempt/scratch"} {
			writeFileDiffFile(t, path, runtime, "runtime\n")
		}
		indexPaths[i], err = gitIndexPath(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		indexes[i], err = os.ReadFile(indexPaths[i])
		if err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name string
		run  func(*testing.T, string)
	}{
		{"diffstat", func(t *testing.T, path string) {
			stat, err := GitDiffStat(t.Context(), path)
			if err != nil || stat.Files != 4 || stat.Added != 4 || stat.Removed != 1 || stat.Fingerprint == "" || stat.HeadSHA == "" || stat.HeadObservedAt.IsZero() {
				t.Errorf("GitDiffStat = %+v, %v; want source and documentation changes with fingerprint", stat, err)
			}
		}},
		{"diff", func(t *testing.T, path string) {
			diff, err := GitDiff(t.Context(), path, 1<<20)
			if err != nil || diff.Stat.Files != 4 || diff.Truncated || strings.Contains(diff.Patch, ".detent/tmp/") || strings.Contains(diff.Patch, ".detent/worker-tmp/") ||
				!strings.Contains(diff.Patch, "a/.detent/notes.md") || !strings.Contains(diff.Patch, "b/.detent/lessons.md") ||
				!strings.Contains(diff.Patch, "+worktree "+filepath.Base(path)) || !strings.Contains(diff.Patch, "+changed") {
				t.Errorf("GitDiff = %+v, %v; want this worktree's source and documentation patches", diff, err)
			}
		}},
		{"filediff", func(t *testing.T, path string) {
			diff, err := GitFileDiffs(t.Context(), path, "", 1<<20)
			if err != nil || len(diff.Files) != 4 {
				t.Errorf("GitFileDiffs = %+v, %v; want source files and human documentation", diff, err)
			}
			file, ok := fileDiffByPath(diff.Files, "new file.txt")
			if !ok || !strings.Contains(file.Patch, "+worktree "+filepath.Base(path)) {
				t.Errorf("missing worktree-specific file patch: %+v", file)
			}
		}},
		{"recovery paths", func(t *testing.T, path string) {
			tracked, err := gitTrackedPaths(t.Context(), path)
			if err != nil || !slices.Equal(tracked, []string{".detent/notes.md", "README.md"}) {
				t.Errorf("tracked paths = %v, %v", tracked, err)
			}
			untracked, err := gitUntrackedPaths(t.Context(), path)
			if err != nil || !slices.Equal(untracked, []string{".detent/lessons.md", "new file.txt"}) {
				t.Errorf("untracked paths = %v, %v", untracked, err)
			}
		}},
	}
	for _, test := range tests {
		for i, path := range worktrees {
			t.Run(test.name+"/"+filepath.Base(path), func(t *testing.T) {
				t.Parallel()
				test.run(t, path)
				got, err := os.ReadFile(excludePath)
				if err != nil || string(got) != exclude {
					t.Errorf("shared info/exclude changed: %q, %v", got, err)
				}
				after, err := os.Stat(excludePath)
				if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
					t.Errorf("shared info/exclude metadata changed: %v", err)
				}
				index, err := os.ReadFile(indexPaths[i])
				if err != nil || !slices.Equal(index, indexes[i]) {
					t.Errorf("worker index changed: %v", err)
				}
			})
		}
	}
}

func TestLocalGitRecoveryStateDetectsStrandedWork(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

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
	provider, ok := backend.(RecoveryStateProvider)
	if !ok {
		t.Fatal("NewBackend() did not return a RecoveryStateProvider")
	}
	issue := Issue{Identifier: "DD-RECOVERY"}
	info, err := backend.Create(context.Background(), issue)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	issue.PullRequestHeadSHA = strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))

	if err := os.WriteFile(filepath.Join(info.Path, "committed.txt"), []byte("committed\n"), 0o600); err != nil {
		t.Fatalf("write committed file: %v", err)
	}
	runGit(t, info.Path, "add", "committed.txt")
	runGit(t, info.Path, "commit", "-m", "test: add committed work")
	commitSHA := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(info.Path, "dirty.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}
	runGit(t, info.Path, "add", "dirty.txt")

	got, err := provider.RecoveryState(context.Background(), info, issue)
	if err != nil {
		t.Fatalf("RecoveryState() error = %v", err)
	}
	if got.UnpushedCommits != 1 || got.DiffStat.Files != 1 || got.DiffStat.Added != 1 {
		t.Fatalf("RecoveryState() = %+v, want one unpushed commit and one dirty file", got)
	}
	if len(got.TrackedPaths) != 1 || got.TrackedPaths[0] != "dirty.txt" {
		t.Fatalf("RecoveryState().TrackedPaths = %v, want [dirty.txt]", got.TrackedPaths)
	}
	if !got.PullRequestComparisonAvailable || len(got.CommitsNotInPullRequest) != 1 ||
		!strings.Contains(got.CommitsNotInPullRequest[0], commitSHA) ||
		!strings.Contains(got.CommitsNotInPullRequest[0], "test: add committed work") {
		t.Fatalf("RecoveryState() pull request commit evidence = %v, available=%t, want %s and subject", got.CommitsNotInPullRequest, got.PullRequestComparisonAvailable, commitSHA)
	}
	if len(got.UnpushedCommitRefs) != 1 || !strings.Contains(got.UnpushedCommitRefs[0], commitSHA) {
		t.Fatalf("RecoveryState().UnpushedCommitRefs = %v, want %s", got.UnpushedCommitRefs, commitSHA)
	}
	if want := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got.HeadSHA != want {
		t.Fatalf("RecoveryState().HeadSHA = %q, want %q", got.HeadSHA, want)
	}
	runGit(t, info.Path, "push", "-u", "origin", "HEAD:"+info.Branch)
	pushed, err := provider.RecoveryState(context.Background(), info, issue)
	if err != nil {
		t.Fatalf("RecoveryState() after push error = %v", err)
	}
	if pushed.UnpushedCommits != 0 || pushed.DiffStat.Files != got.DiffStat.Files || pushed.DiffStat.Added != got.DiffStat.Added || pushed.DiffStat.Removed != got.DiffStat.Removed || pushed.DiffStat.Fingerprint != got.DiffStat.Fingerprint {
		t.Fatalf("RecoveryState() after push = %+v, want no unpushed commits and unchanged dirty diff %+v", pushed, got.DiffStat)
	}
}

func TestLocalGitRecoveryStateReportsUntrackedPaths(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()

	source := initSourceRepo(t)
	remote := initBareRemote(t)
	runGit(t, source, "remote", "add", "origin", remote)
	runGit(t, source, "push", "-u", "origin", "main")
	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	provider := backend.(RecoveryStateProvider)
	issue := Issue{Identifier: "DD-UNTRACKED"}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	issue.PullRequestHeadSHA = strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(info.Path, "session-marker.ses"), []byte("1722600000000 01234567-89ab-cdef-0123-456789abcdef\n"), 0o600); err != nil {
		t.Fatalf("write untracked session marker: %v", err)
	}

	got, err := provider.RecoveryState(t.Context(), info, issue)
	if err != nil {
		t.Fatalf("RecoveryState() error = %v", err)
	}
	if got.DiffStat.Files != 1 || got.UnpushedCommits != 0 {
		t.Fatalf("RecoveryState() = %+v, want one aggregate diff file and no unpushed commits", got)
	}
	if len(got.TrackedPaths) != 0 || !slices.Equal(got.UntrackedPaths, []string{"session-marker.ses"}) || len(got.CommitsNotInPullRequest) != 0 || !got.PullRequestComparisonAvailable {
		t.Fatalf("RecoveryState() evidence = tracked %v untracked %v commits %v available=%t", got.TrackedPaths, got.UntrackedPaths, got.CommitsNotInPullRequest, got.PullRequestComparisonAvailable)
	}
}

func TestLocalGitDeliverableState(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()

	tests := []struct {
		name               string
		push               bool
		advanceRemoteBase  bool
		deleteRemoteBranch bool
		wantCommitsAhead   int
		wantRemoteBranch   bool
	}{
		{name: "zero ahead without remote branch"},
		{name: "pushed commit", push: true, wantCommitsAhead: 1, wantRemoteBranch: true},
		{name: "pushed commit after remote base advances", push: true, advanceRemoteBase: true, wantCommitsAhead: 1, wantRemoteBranch: true},
		{name: "pushed then deleted remote branch", push: true, deleteRemoteBranch: true, wantCommitsAhead: 1},
	}
	for _, tt := range tests {
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
				t.Fatalf("NewBackend() error = %v", err)
			}
			provider := backend.(DeliverableStateProvider)
			issue := Issue{Identifier: "DD-DELIVERABLE-" + tt.name}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			if tt.push {
				path := filepath.Join(info.Path, "delivered.txt")
				if err := os.WriteFile(path, []byte("delivered\n"), 0o600); err != nil {
					t.Fatalf("write delivered file: %v", err)
				}
				runGit(t, info.Path, "add", "delivered.txt")
				runGit(t, info.Path, "commit", "-m", "test: add delivered work")
				runGit(t, info.Path, "push", "-u", "origin", "HEAD:"+info.Branch)
			}
			if tt.advanceRemoteBase {
				path := filepath.Join(source, "remote-base.txt")
				if err := os.WriteFile(path, []byte("remote base\n"), 0o600); err != nil {
					t.Fatalf("write remote base file: %v", err)
				}
				runGit(t, source, "add", "remote-base.txt")
				runGit(t, source, "commit", "-m", "test: advance remote base")
				runGit(t, source, "push", "origin", "main")
			}
			if tt.deleteRemoteBranch {
				runGit(t, info.Path, "push", "origin", "--delete", info.Branch)
			}

			got, err := provider.DeliverableState(t.Context(), info, issue)
			if err != nil {
				t.Fatalf("DeliverableState() error = %v", err)
			}
			if got.CommitsAhead != tt.wantCommitsAhead || got.RemoteBranchExists != tt.wantRemoteBranch {
				t.Fatalf("DeliverableState() = %+v, want commits ahead=%d remote branch=%t", got, tt.wantCommitsAhead, tt.wantRemoteBranch)
			}
			localHead := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			if got.Remote != "origin" || got.RemoteRef != "refs/heads/"+info.Branch || got.LocalHeadSHA != localHead {
				t.Fatalf("DeliverableState() ref evidence = %+v, want origin refs/heads/%s local %s", got, info.Branch, localHead)
			}
			if tt.wantRemoteBranch {
				remoteFields := strings.Fields(runGit(t, info.Path, "ls-remote", "origin", "refs/heads/"+info.Branch))
				if len(remoteFields) < 1 || got.RemoteHeadSHA != remoteFields[0] {
					t.Fatalf("DeliverableState().RemoteHeadSHA = %q, want ls-remote fields %v", got.RemoteHeadSHA, remoteFields)
				}
			} else if got.RemoteHeadSHA != "" {
				t.Fatalf("DeliverableState().RemoteHeadSHA = %q, want empty", got.RemoteHeadSHA)
			}
		})
	}
}

func TestLocalGitRecoveryStateDetectsAmendedCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()

	source := initSourceRepo(t)
	remote := initBareRemote(t)
	runGit(t, source, "remote", "add", "origin", remote)
	runGit(t, source, "push", "-u", "origin", "main")
	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       filepath.Join(t.TempDir(), "workspaces"),
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	provider := backend.(RecoveryStateProvider)
	issue := Issue{Identifier: "DD-RECOVERY-AMEND"}
	info, err := backend.Create(context.Background(), issue)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	path := filepath.Join(info.Path, "work.txt")
	if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatalf("write work file: %v", err)
	}
	runGit(t, info.Path, "add", "work.txt")
	runGit(t, info.Path, "commit", "-m", "test: add work")
	first, err := provider.RecoveryState(context.Background(), info, issue)
	if err != nil {
		t.Fatalf("RecoveryState() error = %v", err)
	}

	if err := os.WriteFile(path, []byte("other\n"), 0o600); err != nil {
		t.Fatalf("update work file: %v", err)
	}
	runGit(t, info.Path, "add", "work.txt")
	runGit(t, info.Path, "commit", "--amend", "--no-edit")
	amended, err := provider.RecoveryState(context.Background(), info, issue)
	if err != nil {
		t.Fatalf("RecoveryState() after amend error = %v", err)
	}
	if first.UnpushedCommits != amended.UnpushedCommits || !first.DiffStat.IsEmpty() || !amended.DiffStat.IsEmpty() {
		t.Fatalf("amended recovery counts = %+v, want unchanged from %+v", amended, first)
	}
	if first.WorkspaceFingerprint == "" || first.WorkspaceFingerprint == amended.WorkspaceFingerprint {
		t.Fatalf("amended workspace fingerprint = %q, want change from %q", amended.WorkspaceFingerprint, first.WorkspaceFingerprint)
	}
}

func TestLocalGitDiffIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

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
	provider, ok := backend.(DiffProvider)
	if !ok {
		t.Fatal("NewBackend() did not return a DiffProvider")
	}

	info, err := backend.Create(context.Background(), Issue{Identifier: "DD-FULL-DIFF"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, "added.txt"), []byte("first\nsecond\n"), 0o600); err != nil {
		t.Fatalf("write added file: %v", err)
	}
	if err := os.Remove(filepath.Join(info.Path, "README.md")); err != nil {
		t.Fatalf("remove README.md: %v", err)
	}

	tests := []struct {
		name          string
		maxBytes      int
		wantTruncated bool
		wantPatch     []string
		forbidden     []string
	}{
		{
			name:          "inline under limit",
			maxBytes:      4096,
			wantTruncated: false,
			wantPatch: []string{
				"diff --git a/README.md b/README.md",
				"diff --git a/added.txt b/added.txt",
				"+first",
				"+second",
			},
		},
		{name: "zero bytes", maxBytes: 0, wantTruncated: true},
		{
			name:          "stat only over limit",
			maxBytes:      1,
			wantTruncated: true,
			forbidden:     []string{"diff --git", "+first"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diff, err := provider.Diff(context.Background(), info, Issue{Identifier: "DD-FULL-DIFF"}, tt.maxBytes)
			if err != nil {
				t.Fatalf("Diff() error = %v", err)
			}
			if diff.Stat != (DiffStat{Files: 2, Added: 2, Removed: 1}) {
				t.Fatalf("Diff().Stat = %+v, want 2 files, 2 added, 1 removed", diff.Stat)
			}
			if diff.Truncated != tt.wantTruncated {
				t.Fatalf("Diff().Truncated = %v, want %v", diff.Truncated, tt.wantTruncated)
			}
			for _, want := range tt.wantPatch {
				if !strings.Contains(diff.Patch, want) {
					t.Fatalf("Diff().Patch missing %q:\n%s", want, diff.Patch)
				}
			}
			for _, forbidden := range tt.forbidden {
				if strings.Contains(diff.Patch, forbidden) {
					t.Fatalf("Diff().Patch contains %q:\n%s", forbidden, diff.Patch)
				}
			}
		})
	}

	status := runGit(t, info.Path, "status", "--short")
	if !strings.Contains(status, "?? added.txt") {
		t.Fatalf("git status = %q, want added.txt to remain untracked", status)
	}
}

func TestGitDiffStopErrorIgnoresCompletedProcess(t *testing.T) {
	t.Parallel()

	waitErr := errors.New("wait")

	tests := []struct {
		name    string
		killErr error
		waitErr error
		wantErr bool
	}{
		{
			name: "no kill error",
		},
		{
			name:    "already done",
			killErr: os.ErrProcessDone,
			waitErr: waitErr,
		},
		{
			name:    "kill denied after successful wait",
			killErr: os.ErrPermission,
		},
		{
			name:    "kill denied before failed wait",
			killErr: os.ErrPermission,
			waitErr: waitErr,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := gitDiffStopError(tt.killErr, tt.waitErr)
			if tt.wantErr {
				if err == nil {
					t.Fatal("gitDiffStopError() error = nil, want error")
				}
				if !errors.Is(err, tt.killErr) {
					t.Fatalf("gitDiffStopError() error = %v, want wrapped %v", err, tt.killErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("gitDiffStopError() error = %v, want nil", err)
			}
		})
	}
}

func TestLocalGitDiffUsesBaseRefForCleanBranch(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

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
	provider, ok := backend.(DiffProvider)
	if !ok {
		t.Fatal("NewBackend() did not return a DiffProvider")
	}

	info, err := backend.Create(context.Background(), Issue{Identifier: "DD-PR-DIFF"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	baseRef := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("source repo\nvalidator diff\n"), 0o600); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runGit(t, info.Path, "add", "README.md")
	runGit(t, info.Path, "commit", "-m", "change readme")

	cleanStatus := runGit(t, info.Path, "status", "--short")
	if cleanStatus != "" {
		t.Fatalf("git status = %q, want clean branch", cleanStatus)
	}

	withoutBase, err := provider.Diff(context.Background(), info, Issue{Identifier: "DD-PR-DIFF"}, 4096)
	if err != nil {
		t.Fatalf("Diff() without base error = %v", err)
	}
	if withoutBase.Stat != (DiffStat{}) || withoutBase.Patch != "" {
		t.Fatalf("Diff() without base = %+v, want clean HEAD diff", withoutBase)
	}

	zero, err := provider.Diff(t.Context(), info, Issue{Identifier: "DD-PR-DIFF"}, 0)
	if err != nil || zero.Truncated || !zero.Stat.IsEmpty() {
		t.Fatalf("zero-length clean diff = %+v, %v", zero, err)
	}
	withBase, err := provider.Diff(context.Background(), info, Issue{Identifier: "DD-PR-DIFF", BaseRef: baseRef}, 4096)
	if err != nil {
		t.Fatalf("Diff() with base error = %v", err)
	}
	if withBase.Stat != (DiffStat{Files: 1, Added: 1}) {
		t.Fatalf("Diff().Stat = %+v, want 1 file, 1 added", withBase.Stat)
	}
	if withBase.Truncated {
		t.Fatal("Diff().Truncated = true, want false")
	}
	if !strings.Contains(withBase.Patch, "+validator diff") {
		t.Fatalf("Diff().Patch missing committed branch change:\n%s", withBase.Patch)
	}
}

func TestGitRecoveryBaseFingerprintUsesConfiguredBase(t *testing.T) {
	t.Parallel()

	if got := gitRecoveryBaseFingerprint(t.Context(), "", "base-sha"); got != "base-sha" {
		t.Fatalf("gitRecoveryBaseFingerprint() = %q, want base-sha", got)
	}
}

func TestGitDiffStatMissingWorkspaceIsClassified(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()

	for _, name := range []string{"missing", "not git", "nested missing git", "unborn", "missing index", "canceled"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "repo")
			if name != "missing" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if name == "unborn" {
				runGit(t, path, "init")
				if _, err := gitIndexPath(t.Context(), path); err != nil {
					t.Fatalf("path-only lookup in unborn repository: %v", err)
				}
			}
			if name == "nested missing git" {
				path = filepath.Join(initSourceRepo(t), "child")
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				runGit(t, path, "init")
				if err := os.RemoveAll(filepath.Join(path, ".git")); err != nil {
					t.Fatal(err)
				}
			}
			if name == "missing index" || name == "canceled" {
				path = initSourceRepo(t)
				if name == "missing index" {
					if err := os.Remove(filepath.Join(path, ".git", "index")); err != nil {
						t.Fatal(err)
					}
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if name == "canceled" {
				cancel()
			}
			stat, err := GitDiffStat(ctx, path)
			if err == nil || !stat.IsEmpty() || stat.HeadSHA != "" || !stat.HeadObservedAt.IsZero() {
				t.Fatalf("unavailable DiffStat = %+v, %v", stat, err)
			}
			if name == "missing" && (!IsMissingWorkspaceError(err) || !errors.Is(err, ErrMissingWorkspace)) {
				t.Fatalf("missing workspace classification: %v", err)
			}
			if name == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
		})
	}
}

func TestIsMissingWorkspaceErrorIgnoresUnmarkedNotExist(t *testing.T) {
	t.Parallel()

	err := &os.PathError{Op: "read", Path: filepath.Join(t.TempDir(), "index"), Err: os.ErrNotExist}
	if IsMissingWorkspaceError(err) {
		t.Fatalf("IsMissingWorkspaceError(%v) = true, want false", err)
	}
}

func TestLocalGitRecoveryStateExcludesBaseCommits(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	for _, withWork := range []bool{false, true} {
		t.Run(fmt.Sprintf("unpushed_work_%t", withWork), func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			remote := initBareRemote(t)
			runGit(t, source, "remote", "add", "origin", remote)
			runGit(t, source, "push", "-u", "origin", "main")
			oldHead := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			runGit(t, source, "commit", "--allow-empty", "-m", "base advancement")
			runGit(t, source, "push", "origin", "main")
			backend, err := NewBackend(KindLocalGit, LocalGitOptions{
				Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "base-comparison", BaseRef: oldHead, ProgressBaseRef: "main", PullRequestHeadSHA: oldHead}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if withWork {
				runGit(t, info.Path, "commit", "--allow-empty", "-m", "issue work")
			}
			got, err := backend.(RecoveryStateProvider).RecoveryState(t.Context(), info, issue)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if withWork {
				want = 1
			}
			if !got.PullRequestComparisonAvailable || len(got.CommitsNotInPullRequest) != want || got.UnpushedCommits != want {
				t.Fatalf("RecoveryState = %+v, want %d work commits", got, want)
			}
			if withWork && !strings.Contains(got.CommitsNotInPullRequest[0], "issue work") {
				t.Fatal(got.CommitsNotInPullRequest)
			}
		})
	}
}
