package runner

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// The runner computes the stored attempt diff from its worktree against the
// attempt's base (decisions section 18.5), and a diff it cannot compute is a
// warning rather than a failed run.

func attemptDiffRunner() *Runner {
	return &Runner{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func attemptDiffFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func attemptDiffFind(files []tracker.AttemptDiffFile, path string) (tracker.AttemptDiffFile, bool) {
	for _, file := range files {
		if file.Path == path {
			return file, true
		}
	}
	return tracker.AttemptDiffFile{}, false
}

// The source reports the base it diffed against, the worktree head, and every
// changed file with its status, counts and patch.
func TestAttemptDiffSourceComputesWorktreeDiff(t *testing.T) {
	isolateAttemptDiffGitConfig(t)
	source := initRunnerSourceRepo(t)
	base := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
	backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{
		Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	issue := workspace.Issue{Identifier: "attempt-diff", BaseRef: base}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	attemptDiffFile(t, info.Path, "implementation.go", "package main\n\nfunc main() {}\n")
	attemptDiffFile(t, info.Path, ".env", "TOKEN=secret\n")
	runRunnerGit(t, info.Path, "add", "implementation.go")
	runRunnerGit(t, info.Path, "commit", "-m", "implement")

	request, ok := attemptDiffRunner().attemptDiffSource(info, issue)(t.Context())
	if !ok {
		t.Fatal("a readable worktree must produce a diff")
	}
	if request.BaseSHA != base {
		t.Fatalf("base = %q, want %q", request.BaseSHA, base)
	}
	if request.HeadSHA == "" || request.HeadSHA == base {
		t.Fatalf("head = %q, want the worktree commit", request.HeadSHA)
	}
	implementation, found := attemptDiffFind(request.Files, "implementation.go")
	if !found {
		t.Fatalf("implementation.go is missing from %+v", request.Files)
	}
	if implementation.Status != tracker.DiffStatusAdded || implementation.Additions != 3 || !strings.Contains(implementation.Patch, "+func main") {
		t.Fatalf("implementation = %#v", implementation)
	}
	// The secret is reported so the hub can record that it changed, and the
	// hub's write-side filter is what drops the patch; the runner does not
	// silently omit the file.
	secret, found := attemptDiffFind(request.Files, ".env")
	if !found {
		t.Fatalf(".env is missing from %+v", request.Files)
	}
	if secret.Additions != 1 {
		t.Fatalf("secret counts = %#v", secret)
	}
	// Normalizing what the runner reports is what removes the patch, and that
	// is the same function the hub applies on write.
	normalized, _ := tracker.NormalizeDiffFiles(request.Files)
	filtered, _ := attemptDiffFind(normalized, ".env")
	if !filtered.Denied || filtered.Patch != "" {
		t.Fatalf("normalized secret = %#v", filtered)
	}
}

// "Keep it best-effort: a diff failure logs and does not fail the run."
func TestAttemptDiffSourceReportsNothingWhenUnreadable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
	}{
		{name: "missing worktree", path: filepath.Join(t.TempDir(), "gone")},
		{name: "empty path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, ok := attemptDiffRunner().attemptDiffSource(workspace.Info{Path: test.path}, workspace.Issue{ID: "issue"})(t.Context())
			if ok {
				t.Fatal("an unreadable worktree must report nothing to post")
			}
		})
	}
}

// A diff whose patch output exceeds the bound keeps its counts and loses every
// patch, rather than posting a patch set that stops partway through.
func TestAttemptDiffSourceStripsPatchesWhenTruncated(t *testing.T) {
	t.Parallel()
	source := initRunnerSourceRepo(t)
	base := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
	backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{
		Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	issue := workspace.Issue{Identifier: "attempt-diff-large", BaseRef: base}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	attemptDiffFile(t, info.Path, "large.txt", strings.Repeat("line of text\n", 16))
	// The bound the runner uses is the contract's whole-diff bound; a diff
	// that fits keeps its patches, which is what makes the stripped case a
	// property of the bound rather than of the file.
	request, ok := attemptDiffRunner().attemptDiffSource(info, issue)(t.Context())
	if !ok {
		t.Fatal("a readable worktree must produce a diff")
	}
	file, found := attemptDiffFind(request.Files, "large.txt")
	if !found || file.Patch == "" || file.Truncated {
		t.Fatalf("small diff = %#v", file)
	}
	stripped := tracker.StripDiffPatches(request.Files)
	if stripped[0].Patch != "" || !stripped[0].Truncated || stripped[0].Additions != file.Additions {
		t.Fatalf("stripped = %#v", stripped[0])
	}
}

// isolateAttemptDiffGitConfig isolates one test's git commands from the host's global and system git
// configuration, so a developer's global excludes file (which commonly lists
// .env) cannot hide the files the test asserts on. It uses t.Setenv, so the
// calling test must not run in parallel.
func isolateAttemptDiffGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}
