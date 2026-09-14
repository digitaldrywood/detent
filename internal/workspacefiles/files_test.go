package workspacefiles_test

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspacefiles"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// newWorktree builds a small tree to serve, plus a sibling directory outside it
// that the containment tests aim at. The sibling is the whole point: every
// escape this package must refuse is an attempt to reach it.
func newWorktree(t *testing.T, deny ...string) (*workspacefiles.Service, string, string) {
	t.Helper()
	base := t.TempDir()
	worktree := filepath.Join(base, "worktree")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{
		worktree,
		filepath.Join(worktree, "internal"),
		filepath.Join(worktree, "internal", "hubserver"),
		filepath.Join(worktree, ".git"),
		filepath.Join(worktree, ".git", "objects"),
		filepath.Join(worktree, "node_modules"),
		outside,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(worktree, "README.md"):                       "# Detent\n",
		filepath.Join(worktree, "internal", "hubserver", "svc.go"): "package hubserver\n",
		filepath.Join(worktree, ".git", "HEAD"):                    "ref: refs/heads/main\n",
		filepath.Join(worktree, ".git", "config"):                  "[remote \"origin\"]\n\turl = git@example.com:secret.git\n",
		filepath.Join(worktree, ".git", "objects", "pack"):         "binary",
		filepath.Join(worktree, ".env"):                            "TOKEN=super-secret\n",
		filepath.Join(worktree, "node_modules", "react.js"):        "module.exports = {}\n",
		filepath.Join(outside, "passwords.txt"):                    "hunter2\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	list, err := workspacesession.NewDenylist(deny)
	if err != nil {
		t.Fatal(err)
	}
	service, err := workspacefiles.Open(worktree, list)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service, worktree, outside
}

func names(entries []workspacesession.FilesEntry) []string {
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Name)
	}
	return result
}

func TestListShowsTheWorktreeRoot(t *testing.T) {
	t.Parallel()
	service, _, _ := newWorktree(t)
	listed, err := service.List(t.Context(), workspacesession.FilesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(names(listed.Entries), ",")
	// .git is listable even though its object store and config are not: a
	// reader may see the directory exists and may not read what is inside it.
	for _, want := range []string{"README.md", "internal", ".git", ".env", "node_modules"} {
		if !strings.Contains(got, want) {
			t.Fatalf("listing = %q, want it to contain %q", got, want)
		}
	}
}

func TestListMarksDeniedEntriesWithoutHidingThem(t *testing.T) {
	t.Parallel()
	service, _, _ := newWorktree(t)
	listed, err := service.List(t.Context(), workspacesession.FilesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	denied := map[string]bool{}
	for _, entry := range listed.Entries {
		denied[entry.Name] = entry.Denied
	}
	for name, want := range map[string]bool{".env": true, "node_modules": true, "README.md": false, "internal": false} {
		if denied[name] != want {
			t.Errorf("entry %q denied = %v, want %v", name, denied[name], want)
		}
	}
}

func TestReadServesAFileAndItsMediaType(t *testing.T) {
	t.Parallel()
	service, _, _ := newWorktree(t)
	content, err := service.Read(t.Context(), workspacesession.FilesRequest{Path: "internal/hubserver/svc.go"})
	if err != nil {
		t.Fatal(err)
	}
	if content.Data != "package hubserver\n" || content.Truncated || content.Encoding != "" {
		t.Fatalf("content = %+v", content)
	}
	if content.Size != int64(len("package hubserver\n")) {
		t.Fatalf("size = %d", content.Size)
	}
}

func TestReadRefusesWhatTheDenylistCovers(t *testing.T) {
	t.Parallel()
	service, _, _ := newWorktree(t, "*.forbidden")
	tests := []struct {
		name string
		path string
		code string
	}{
		{name: "dotenv", path: ".env", code: workspacesession.CodeDenied},
		{name: "git config", path: ".git/config", code: workspacesession.CodeDenied},
		{name: "git objects", path: ".git/objects/pack", code: workspacesession.CodeDenied},
		{name: "node_modules", path: "node_modules/react.js", code: workspacesession.CodeDenied},
		// .git/HEAD is not the object store or the remote configuration, so
		// it is readable: the denylist is prefixes, not the whole directory.
		{name: "git head is readable", path: ".git/HEAD"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := service.Read(t.Context(), workspacesession.FilesRequest{Path: test.path})
			if test.code == "" {
				if err != nil {
					t.Fatalf("Read(%q) = %v, want it served", test.path, err)
				}
				return
			}
			if got := workspacefiles.ErrorCode(err); got != test.code {
				t.Fatalf("Read(%q) code = %q, want %q", test.path, got, test.code)
			}
		})
	}
}

// TestContainmentRefusesEveryEscape is the security argument of section 18.4
// stated as a test: no request, however it is spelled, reaches outside the
// worktree.
func TestContainmentRefusesEveryEscape(t *testing.T) {
	t.Parallel()
	service, worktree, outside := newWorktree(t)
	if err := os.Symlink(filepath.Join(outside, "passwords.txt"), filepath.Join(worktree, "escape.txt")); err != nil {
		t.Skipf("this platform does not support symlinks: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(worktree, "escape-dir")); err != nil {
		t.Fatal(err)
	}
	// A link to something inside the worktree is refused too: reading a
	// symlink at all is what a swapped link would exploit, so the rule is the
	// kind, not the target.
	if err := os.Symlink(filepath.Join(worktree, ".env"), filepath.Join(worktree, "inside-link")); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
	}{
		{name: "parent traversal", path: "../outside/passwords.txt"},
		{name: "traversal through a real directory", path: "internal/../../outside/passwords.txt"},
		{name: "absolute path", path: filepath.Join(outside, "passwords.txt")},
		{name: "symlink to a file outside", path: "escape.txt"},
		{name: "path through a symlinked directory", path: "escape-dir/passwords.txt"},
		{name: "symlink to a file inside", path: "inside-link"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			content, err := service.Read(t.Context(), workspacesession.FilesRequest{Path: test.path})
			if err == nil {
				t.Fatalf("Read(%q) served %q; the worktree must be the whole of what this surface can reach", test.path, content.Data)
			}
			if code := workspacefiles.ErrorCode(err); code != workspacesession.CodeForbidden && code != workspacesession.CodeNotFound && code != workspacesession.CodeDenied {
				t.Fatalf("Read(%q) code = %q, want a channel refusal", test.path, code)
			}
		})
	}
}

func TestListReportsASymlinkWithoutItsTarget(t *testing.T) {
	t.Parallel()
	service, worktree, outside := newWorktree(t)
	if err := os.Symlink(filepath.Join(outside, "passwords.txt"), filepath.Join(worktree, "escape.txt")); err != nil {
		t.Skipf("this platform does not support symlinks: %v", err)
	}
	listed, err := service.List(t.Context(), workspacesession.FilesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range listed.Entries {
		if entry.Name != "escape.txt" {
			continue
		}
		found = true
		if entry.Kind != workspacesession.KindSymlink {
			t.Fatalf("symlink listed as %q, want %q", entry.Kind, workspacesession.KindSymlink)
		}
	}
	if !found {
		t.Fatal("the symlink must be listed; it is reading one that is refused")
	}
	// Naming what it points at would leak a path outside the worktree to a
	// reader who may not see it, so the entry carries no target field at all.
	for _, entry := range listed.Entries {
		if strings.Contains(entry.Name, outside) {
			t.Fatalf("entry %+v names a path outside the worktree", entry)
		}
	}
}

func TestStatDescribesEachKind(t *testing.T) {
	t.Parallel()
	service, _, _ := newWorktree(t)
	tests := []struct {
		name string
		path string
		kind string
	}{
		{name: "the worktree root", path: "", kind: workspacesession.KindDir},
		{name: "a directory", path: "internal", kind: workspacesession.KindDir},
		{name: "a file", path: "README.md", kind: workspacesession.KindFile},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result, err := service.Stat(t.Context(), workspacesession.FilesRequest{Path: test.path})
			if err != nil {
				t.Fatal(err)
			}
			if result.Kind != test.kind {
				t.Fatalf("Stat(%q).Kind = %q, want %q", test.path, result.Kind, test.kind)
			}
		})
	}
}

func TestStatAndReadAnswerNotFoundForAMissingPath(t *testing.T) {
	t.Parallel()
	service, _, _ := newWorktree(t)
	if _, err := service.Stat(t.Context(), workspacesession.FilesRequest{Path: "nowhere.go"}); workspacefiles.ErrorCode(err) != workspacesession.CodeNotFound {
		t.Fatalf("Stat of a missing path = %v", err)
	}
	if _, err := service.Read(t.Context(), workspacesession.FilesRequest{Path: "nowhere.go"}); workspacefiles.ErrorCode(err) != workspacesession.CodeNotFound {
		t.Fatalf("Read of a missing path = %v", err)
	}
}

func TestReadTruncatesAtTheCapAndPagesWithOffset(t *testing.T) {
	t.Parallel()
	service, worktree, _ := newWorktree(t)
	large := strings.Repeat("x", workspacesession.MaxReadBytes+1024)
	if err := os.WriteFile(filepath.Join(worktree, "large.txt"), []byte(large), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := service.Read(t.Context(), workspacesession.FilesRequest{Path: "large.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Data) != workspacesession.MaxReadBytes || !first.Truncated {
		t.Fatalf("first read length = %d truncated = %v, want the 2 MB cap", len(first.Data), first.Truncated)
	}
	if first.Size != int64(len(large)) {
		t.Fatalf("reported size = %d, want %d", first.Size, len(large))
	}
	second, err := service.Read(t.Context(), workspacesession.FilesRequest{Path: "large.txt", Offset: int64(workspacesession.MaxReadBytes)})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Data) != 1024 || second.Truncated {
		t.Fatalf("second read length = %d truncated = %v", len(second.Data), second.Truncated)
	}
}

func TestReadBase64EncodesBinaryContent(t *testing.T) {
	t.Parallel()
	service, worktree, _ := newWorktree(t)
	binary := []byte{0x00, 0x01, 0x02, 0xff, 0xfe}
	if err := os.WriteFile(filepath.Join(worktree, "blob.bin"), binary, 0o600); err != nil {
		t.Fatal(err)
	}
	content, err := service.Read(t.Context(), workspacesession.FilesRequest{Path: "blob.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if content.Encoding != "base64" {
		t.Fatalf("encoding = %q, want base64", content.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(content.Data)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != string(binary) {
		t.Fatalf("decoded = %v, want %v", decoded, binary)
	}
}

func TestReadRefusesTheWorktreeRootAndADirectory(t *testing.T) {
	t.Parallel()
	service, _, _ := newWorktree(t)
	for _, path := range []string{"", "internal"} {
		if _, err := service.Read(t.Context(), workspacesession.FilesRequest{Path: path}); workspacefiles.ErrorCode(err) != workspacesession.CodeForbidden {
			t.Fatalf("Read(%q) = %v, want forbidden", path, err)
		}
	}
}

func TestListPagesAtTheDirectoryLimit(t *testing.T) {
	t.Parallel()
	service, worktree, _ := newWorktree(t)
	crowded := filepath.Join(worktree, "many")
	if err := os.MkdirAll(crowded, 0o755); err != nil {
		t.Fatal(err)
	}
	total := workspacesession.DirectoryPage + 5
	for index := range total {
		name := filepath.Join(crowded, "file-"+strings.Repeat("0", 4-len(itoa(index)))+itoa(index)+".txt")
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	first, err := service.List(t.Context(), workspacesession.FilesRequest{Path: "many"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != workspacesession.DirectoryPage || first.NextCursor == "" {
		t.Fatalf("first page = %d entries, cursor %q", len(first.Entries), first.NextCursor)
	}
	second, err := service.List(t.Context(), workspacesession.FilesRequest{Path: "many", Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Entries) != 5 || second.NextCursor != "" {
		t.Fatalf("second page = %d entries, cursor %q", len(second.Entries), second.NextCursor)
	}
	// The cursor is a name rather than an offset, so the two pages cannot
	// overlap however the directory changed between them.
	if second.Entries[0].Name <= first.Entries[len(first.Entries)-1].Name {
		t.Fatalf("page two starts at %q, which is not after %q", second.Entries[0].Name, first.Entries[len(first.Entries)-1].Name)
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

func TestListHonoursGitignoreUnlessTheReaderAsksForIt(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available; the ignore decoration is git's own answer")
	}
	service, worktree, _ := newWorktree(t)
	if err := os.WriteFile(filepath.Join(worktree, ".gitignore"), []byte("ignored.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "ignored.log"), []byte("noise\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"init"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "Test"}} {
		command := exec.CommandContext(t.Context(), "git", append([]string{"-C", worktree}, argv...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v: %s", argv, err, output)
		}
	}
	hidden, err := service.List(t.Context(), workspacesession.FilesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(names(hidden.Entries), ","), "ignored.log") {
		t.Fatal("an ignored file must not be listed until the reader turns on show ignored")
	}
	shown, err := service.List(t.Context(), workspacesession.FilesRequest{ShowIgnored: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range shown.Entries {
		if entry.Name == "ignored.log" {
			found = true
			if !entry.Ignored {
				t.Fatal("an ignored file shown on request must still say it is ignored")
			}
		}
	}
	if !found {
		t.Fatal("show ignored must include the ignored file")
	}
}
