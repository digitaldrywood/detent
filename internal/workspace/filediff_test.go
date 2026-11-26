package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fileDiffRepo returns a repository whose base commit already carries the
// files a rename and a deletion need, and the sha of that commit, so a test
// can diff a later worktree state against it.
func fileDiffRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := initSourceRepo(t)
	writeFileDiffFile(t, dir, "renamed-source.txt", "rename me\n")
	writeFileDiffFile(t, dir, "deleted.txt", "gone\n")
	runGit(t, dir, "add", "renamed-source.txt", "deleted.txt")
	runGit(t, dir, "commit", "-m", "base content")
	base := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	return dir, base
}

func writeFileDiffFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func fileDiffByPath(files []FileDiff, path string) (FileDiff, bool) {
	for _, file := range files {
		if file.Path == path {
			return file, true
		}
	}
	return FileDiff{}, false
}

// GitFileDiffs splits the change per file with its status, its counts and its
// own patch, and reports the base it was taken against and the worktree head.
func TestGitFileDiffsStatusesAndCounts(t *testing.T) {
	t.Parallel()
	dir, base := fileDiffRepo(t)
	// A commit after the base so the head is not the base, then worktree
	// changes of every status the contract names.
	writeFileDiffFile(t, dir, "committed.txt", "committed\n")
	runGit(t, dir, "add", "committed.txt")
	runGit(t, dir, "commit", "-m", "later commit")
	head := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	writeFileDiffFile(t, dir, "added.txt", "one\ntwo\n")
	writeFileDiffFile(t, dir, "README.md", "source repo\nchanged\n")
	runGit(t, dir, "mv", "renamed-source.txt", "renamed-target.txt")
	runGit(t, dir, "rm", "deleted.txt")

	diffs, err := GitFileDiffs(t.Context(), dir, base, 1<<20)
	if err != nil {
		t.Fatalf("GitFileDiffs: %v", err)
	}
	if diffs.BaseSHA != base {
		t.Fatalf("base = %q, want %q", diffs.BaseSHA, base)
	}
	if diffs.HeadSHA != head {
		t.Fatalf("head = %q, want %q", diffs.HeadSHA, head)
	}
	if diffs.Truncated {
		t.Fatal("a small diff must not be truncated")
	}
	tests := []struct {
		path      string
		status    string
		additions int
		deletions int
		oldPath   string
		patchHas  string
	}{
		{path: "added.txt", status: FileDiffAdded, additions: 2, patchHas: "+one"},
		{path: "README.md", status: FileDiffModified, additions: 1, patchHas: "+changed"},
		{path: "renamed-target.txt", status: FileDiffRenamed, oldPath: "renamed-source.txt"},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			file, ok := fileDiffByPath(diffs.Files, test.path)
			if !ok {
				t.Fatalf("%s is missing from %+v", test.path, diffs.Files)
			}
			if file.Status != test.status {
				t.Fatalf("status = %q, want %q", file.Status, test.status)
			}
			if file.OldPath != test.oldPath {
				t.Fatalf("old path = %q, want %q", file.OldPath, test.oldPath)
			}
			if test.additions != 0 && file.Additions != test.additions {
				t.Fatalf("additions = %d, want %d", file.Additions, test.additions)
			}
			if test.patchHas != "" && !strings.Contains(file.Patch, test.patchHas) {
				t.Fatalf("patch of %s does not contain %q: %q", test.path, test.patchHas, file.Patch)
			}
			if test.patchHas != "" && !strings.HasPrefix(file.Patch, "diff --git ") {
				t.Fatalf("patch of %s is not a whole file section: %q", test.path, file.Patch)
			}
		})
	}
	// A file staged for deletion is reported as deleted, with the lines it
	// removed, whether or not the working tree still has it.
	deleted, ok := fileDiffByPath(diffs.Files, "deleted.txt")
	if !ok {
		t.Fatalf("deleted.txt is missing from %+v", diffs.Files)
	}
	if deleted.Status != FileDiffDeleted || deleted.Deletions != 1 {
		t.Fatalf("deleted file = %+v", deleted)
	}
}

// A binary file reports no counts and no patch, so a renderer is never handed
// bytes it cannot draw.
func TestGitFileDiffsBinary(t *testing.T) {
	t.Parallel()
	dir, base := fileDiffRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0, 1, 2, 0, 3, 4}, 0o600); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	diffs, err := GitFileDiffs(t.Context(), dir, base, 1<<20)
	if err != nil {
		t.Fatalf("GitFileDiffs: %v", err)
	}
	file, ok := fileDiffByPath(diffs.Files, "blob.bin")
	if !ok {
		t.Fatalf("blob.bin is missing from %+v", diffs.Files)
	}
	if !file.Binary || file.Additions != 0 || file.Deletions != 0 {
		t.Fatalf("binary file = %+v", file)
	}
}

// The whole-diff byte cap keeps the file list and its counts and drops every
// patch, rather than serving a patch set that stops partway through.
func TestGitFileDiffsTruncatesAtLimit(t *testing.T) {
	t.Parallel()
	dir, base := fileDiffRepo(t)
	writeFileDiffFile(t, dir, "large.txt", strings.Repeat("line of text\n", 2000))
	diffs, err := GitFileDiffs(t.Context(), dir, base, 64)
	if err != nil {
		t.Fatalf("GitFileDiffs: %v", err)
	}
	if !diffs.Truncated {
		t.Fatal("a diff past the limit must report truncated")
	}
	file, ok := fileDiffByPath(diffs.Files, "large.txt")
	if !ok {
		t.Fatalf("large.txt is missing from %+v", diffs.Files)
	}
	if file.Additions != 2000 {
		t.Fatalf("additions = %d, want 2000", file.Additions)
	}
	if file.Patch != "" {
		t.Fatalf("a truncated diff must carry no patch, got %d bytes", len(file.Patch))
	}
}

// maxBytes zero asks for counts only, which is what a caller that just wants
// the file list pays for.
func TestGitFileDiffsCountsOnly(t *testing.T) {
	t.Parallel()
	dir, base := fileDiffRepo(t)
	writeFileDiffFile(t, dir, "added.txt", "one\n")
	diffs, err := GitFileDiffs(t.Context(), dir, base, 0)
	if err != nil {
		t.Fatalf("GitFileDiffs: %v", err)
	}
	if len(diffs.Files) != 1 || diffs.Files[0].Patch != "" {
		t.Fatalf("files = %+v", diffs.Files)
	}
	if !diffs.Truncated {
		t.Fatal("a counts-only diff with changes reports truncated")
	}
}

// A clean worktree answers an empty file list, not an error.
func TestGitFileDiffsClean(t *testing.T) {
	t.Parallel()
	dir, base := fileDiffRepo(t)
	diffs, err := GitFileDiffs(t.Context(), dir, base, 1<<20)
	if err != nil {
		t.Fatalf("GitFileDiffs: %v", err)
	}
	if len(diffs.Files) != 0 || diffs.Truncated {
		t.Fatalf("clean diff = %+v", diffs)
	}
}

// Secrets and vendored trees still appear with their counts; stripping their
// patch is the hub's write-side filter, not the runner's git call, so the
// runner reports them and the filter decides.
func TestGitFileDiffsReportsDeniedPathsForTheFilter(t *testing.T) {
	isolateFileDiffGitConfig(t)
	dir, base := fileDiffRepo(t)
	writeFileDiffFile(t, dir, ".env", "TOKEN=secret\n")
	writeFileDiffFile(t, dir, "web/node_modules/pkg/index.js", "module.exports = 1\n")
	diffs, err := GitFileDiffs(t.Context(), dir, base, 1<<20)
	if err != nil {
		t.Fatalf("GitFileDiffs: %v", err)
	}
	for _, path := range []string{".env", "web/node_modules/pkg/index.js"} {
		if _, ok := fileDiffByPath(diffs.Files, path); !ok {
			t.Fatalf("%s is missing from %+v", path, diffs.Files)
		}
	}
}

func isolateFileDiffGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func TestAttemptBase(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		remote    bool
		localHead bool
	}{
		{name: "no remote uses the current commit"},
		{name: "a tracked remote default branch uses the merge base", remote: true},
		{name: "a local origin HEAD answers without the remote", remote: true, localHead: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dir := initSourceRepo(t)
			want := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
			if test.remote {
				origin := filepath.Join(t.TempDir(), "origin.git")
				runGit(t, dir, "init", "--bare", "-b", "main", origin)
				runGit(t, dir, "remote", "add", "origin", origin)
				runGit(t, dir, "push", "-u", "origin", "HEAD:main")
				writeFileDiffFile(t, dir, "ahead.txt", "ahead\n")
				runGit(t, dir, "add", "ahead.txt")
				runGit(t, dir, "commit", "-m", "ahead of the remote")
				if test.localHead {
					runGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
					runGit(t, dir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "unreachable.git"))
				}
			}
			if got := AttemptBase(t.Context(), dir); got != want {
				t.Fatalf("AttemptBase() = %q, want %q", got, want)
			}
		})
	}
}
