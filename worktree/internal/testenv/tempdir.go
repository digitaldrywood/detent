package testenv

import (
	"os"
	"testing"
)

// TempDir gives Git fixtures a short scratch path without the full subtest name.
// MkdirTemp honors the process temporary directory, including worker TMPDIR.
func TempDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "landing-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}
