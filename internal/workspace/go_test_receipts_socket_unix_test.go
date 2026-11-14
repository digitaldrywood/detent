//go:build !windows

package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoTestReceiptLongSocketStaysInAssignedScratch(t *testing.T) {
	t.Parallel()
	backend, err := NewLocalGit(LocalGitOptions{Root: t.TempDir(), SourceRoot: initSourceRepo(t)})
	if err != nil {
		t.Fatal(err)
	}
	long := filepath.Join(t.TempDir(), strings.Repeat("scratch", 20))
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Run("scratch longer than a socket address", func(t *testing.T) { testGoTestReceiptSocket(t, backend, long, true) })
}
