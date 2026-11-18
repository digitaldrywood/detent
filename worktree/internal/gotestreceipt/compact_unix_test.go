//go:build unix

package gotestreceipt

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestTreeInputsRefuseSpecialFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pkg")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := Environment{Root: root, DigestKey: testDigestKey}
	inputs := []string{"tree\tm\tpkg"}
	if _, err := Digest(inputs, env); err == nil {
		t.Fatal("Digest() hashed a tree containing a FIFO")
	}
	if err := ModifiedSince(inputs, env, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("ModifiedSince() accepted a tree containing a FIFO")
	}
}
