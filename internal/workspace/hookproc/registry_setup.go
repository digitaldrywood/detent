package hookproc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/digitaldrywood/detent/internal/workspace/internal/workpath"
)

func pendingWorkspaceSetupPath(path string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return filepath.Join(".detent", "setup-pending", hex.EncodeToString(sum[:12]))
}

func MarkWorkspaceSetupPending(root, path, command string) error {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	if err := ClearWorkspaceSetupPending(root, path); err != nil {
		return err
	}
	return workpath.EnsurePrivateDirectories(root, filepath.Join(root, pendingWorkspaceSetupPath(path)))
}

func ClearWorkspaceSetupPending(root, path string) error {
	dir, err := os.OpenRoot(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.RemoveAll(pendingWorkspaceSetupPath(path))
}

func (u *Registry) CheckWorkspaceSetup(root, path string) (retryCleanup bool, err error) {
	incomplete := &HookExitUnverifiedError{Err: fmt.Errorf("%w: workspace setup incomplete for %s", ErrWorkspacePreserved, path)}
	if u.setupFailed(path) {
		return true, incomplete
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return false, err
	}
	defer dir.Close()
	marker := pendingWorkspaceSetupPath(path)
	if _, err := dir.Lstat(marker); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if exists, _, err := workpath.PathExists(path); err != nil || !exists {
		return false, err
	}
	if _, err := dir.Lstat(filepath.Join(marker, "ready")); err == nil && u.ownsRetainedSetup(path) {
		return false, nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if _, err := dir.Lstat(filepath.Join(marker, "retired")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	} else {
		return err == nil, incomplete
	}
}

func (u *Registry) CheckWorkspaceSetupRemoval(root, path string) error {
	if retry, err := u.CheckWorkspaceSetup(root, path); !retry {
		return err
	}
	return nil
}

func MarkWorkspaceSetupRetired(root, path string) error {
	return recordWorkspaceSetupStatus(root, path, "retired")
}

func recordWorkspaceSetupStatus(root, path, status string) error {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	file, err := dir.OpenFile(filepath.Join(pendingWorkspaceSetupPath(path), status), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) || status == "retired" && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return file.Close()
}

type workspaceSetup struct {
	root     string
	retained atomic.Bool
}

type workspaceSetupKey struct{}

func RunWorkspaceSetup(ctx context.Context, root, path, command string, run func(context.Context) error) error {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	setup := &workspaceSetup{root: root}
	if err := run(context.WithValue(ctx, workspaceSetupKey{}, setup)); err != nil {
		return err
	}
	if setup.retained.Load() {
		return nil
	}
	return ClearWorkspaceSetupPending(root, path)
}
