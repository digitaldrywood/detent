package workspacefiles

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestOpeningRefusesADirectorySwappedForASymlinkAfterTheCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		request string
		open    func(s *Service, relative, resolved string) error
	}{
		{name: "read", request: "sub/config", open: func(s *Service, _, resolved string) error {
			handle, err := s.openRegular(resolved)
			if err == nil {
				_ = handle.Close()
			}
			return err
		}},
		{name: "stat", request: "sub/config", open: func(s *Service, _, resolved string) error {
			_, err := s.lstat(resolved)
			return err
		}},
		{name: "list", request: "sub/objects", open: func(s *Service, relative, resolved string) error {
			handle, _, err := s.openDirectory(relative, resolved)
			if err == nil {
				_ = handle.Close()
			}
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			worktree := t.TempDir()
			for _, dir := range []string{"sub/objects", ".git/objects"} {
				if err := os.MkdirAll(filepath.Join(worktree, filepath.FromSlash(dir)), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, file := range []string{"sub/config", ".git/config"} {
				if err := os.WriteFile(filepath.Join(worktree, filepath.FromSlash(file)), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			service, err := Open(worktree, workspacesession.Denylist{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = service.Close() })

			relative, resolved, err := service.resolve(test.request)
			if err != nil {
				t.Fatalf("resolve(%q) = %v", test.request, err)
			}
			if err := os.Rename(filepath.Join(worktree, "sub"), filepath.Join(worktree, "moved")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(".git", filepath.Join(worktree, "sub")); err != nil {
				t.Skipf("this platform does not support symlinks: %v", err)
			}
			err = test.open(service, relative, resolved)
			if got := ErrorCode(err); got != workspacesession.CodeForbidden {
				t.Fatalf("open after the swap code = %q (err %v), want %q", got, err, workspacesession.CodeForbidden)
			}
		})
	}
}
