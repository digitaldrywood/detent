//go:build !windows

package runnerauth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIdentityFilesRejectPublicPermissions(t *testing.T) {
	t.Parallel()
	testIdentityFilesRejectUnsafeInput(t, []identityFileMutation{
		{"public file", func(t *testing.T, path string) {
			t.Helper()
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"public directory", func(t *testing.T, path string) {
			t.Helper()
			if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	})
}
