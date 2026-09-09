//go:build unix

package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

func TestProcFSScratchEnvironmentProcessIDs(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		readErr error
		wantErr error
	}{
		{name: "unrelated process"},
		{name: "permission boundary", readErr: os.ErrPermission},
		{name: "operation not permitted", readErr: syscall.EPERM},
		{name: "exited during scan", readErr: os.ErrNotExist},
		{name: "missing process", readErr: syscall.ESRCH},
		{name: "inspection failure", readErr: syscall.EIO, wantErr: syscall.EIO},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, procRoot := t.TempDir(), t.TempDir()
			for _, pid := range []string{"1000000001", "1000000002"} {
				if err := os.Mkdir(filepath.Join(procRoot, pid), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			pids, err := procFSScratchEnvironmentProcessIDs(t.Context(), root, procRoot, func(path string) ([]byte, error) {
				if filepath.Base(filepath.Dir(path)) == "1000000001" {
					return []byte("TMPDIR=/unrelated"), tt.readErr
				}
				return []byte("DETENT_WORKER_SCRATCH=" + root + "\x00"), nil
			})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("scan error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && !reflect.DeepEqual(pids, []int{1000000002}) {
				t.Fatalf("owned processes = %v, want owned descendant", pids)
			}
		})
	}
}

func TestWorkerScratchEnvironmentMatchesPathAlias(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"TMPDIR", "DETENT_WORKER_SCRATCH"} {
		t.Run(key, func(t *testing.T) {
			if !workerScratchEnvironmentMatches(root, []string{key + "=" + alias}) {
				t.Fatal("scratch ownership was lost through a parent directory alias")
			}
		})
	}
}

func TestWorkspaceProcessIDsRejectsRootAlias(t *testing.T) {
	t.Parallel()
	alias := filepath.Join(t.TempDir(), "filesystem-root")
	if err := os.Symlink(string(filepath.Separator), alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{string(filepath.Separator), alias} {
		t.Run(path, func(t *testing.T) {
			if _, err := workspaceProcessIDs(t.Context(), path); err == nil {
				t.Fatal("filesystem root scan was permitted")
			}
		})
	}
}
