package hookproc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/digitaldrywood/detent/internal/procstart"
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
	if _, err := dir.Lstat(filepath.Join(marker, "retired")); err == nil {
		return true, incomplete
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if ready, err := dir.Lstat(filepath.Join(marker, "ready")); err == nil {
		if !ready.Mode().IsRegular() {
			return false, errors.Join(incomplete, errors.New("retained hook ready marker is not a regular file"))
		}
		if u.ownsRetainedSetup(path) {
			return false, nil
		}
		if err := verifyWorkspaceSetupExited(dir, marker, ready); err != nil {
			return false, errors.Join(incomplete, err)
		}
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return false, incomplete
}

func (u *Registry) CheckWorkspaceSetupRemoval(root, path string) error {
	retry, err := u.CheckWorkspaceSetup(root, path)
	var unverified *HookExitUnverifiedError
	if retry || errors.As(err, &unverified) && u.ownsRetainedSetup(path) {
		return nil
	}
	return err
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

type retainedSetup struct {
	DescriptorDir string         `json:"descriptor_dir"`
	Descriptor    string         `json:"descriptor"`
	Processes     map[int]string `json:"processes"`
}

func recordWorkspaceSetupReady(root, path string, lifetime *Lifetime) (returnErr error) {
	processes := make(map[int]string, len(lifetime.witnesses)+len(lifetime.starts))
	maps.Copy(processes, lifetime.witnesses)
	maps.Copy(processes, lifetime.starts)
	identity, err := lifetime.fileIdentity()
	if err != nil {
		return err
	}
	record := retainedSetup{DescriptorDir: filepath.Base(lifetime.dir), Descriptor: identity, Processes: processes}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, dir.Close()) }()
	marker := pendingWorkspaceSetupPath(path)
	temporary := filepath.Join(marker, record.DescriptorDir, "ready")
	file, err := dir.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := errors.Join(json.NewEncoder(file).Encode(record), file.Close()); err != nil {
		return errors.Join(err, dir.Remove(temporary))
	}
	if err := dir.Rename(temporary, filepath.Join(marker, "ready")); err != nil {
		return errors.Join(err, dir.Remove(temporary))
	}
	return nil
}

func revokeWorkspaceSetupReady(root, path string) error {
	dir, err := os.OpenRoot(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	removeErr := dir.Remove(filepath.Join(pendingWorkspaceSetupPath(path), "ready"))
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(removeErr, dir.Close())
}

func verifyWorkspaceSetupExited(dir *os.Root, marker string, ready fs.FileInfo) (returnErr error) {
	readyPath := filepath.Join(marker, "ready")
	file, err := openRetainedFile(dir, readyPath, ready)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	var record retainedSetup
	if err := json.NewDecoder(io.LimitReader(file, 128<<10)).Decode(&record); err != nil {
		return fmt.Errorf("read retained hook lifetime: %w", err)
	}
	if !filepath.IsLocal(record.DescriptorDir) || filepath.Base(record.DescriptorDir) != record.DescriptorDir ||
		!strings.HasPrefix(record.DescriptorDir, "detent-hook-") || record.Descriptor == "" || len(record.Processes) == 0 {
		return errors.New("invalid retained hook lifetime")
	}
	for pid, start := range record.Processes {
		if pid <= 0 || start == "" {
			return errors.New("invalid retained hook process identity")
		}
		alive, err := procstart.Matches(pid, start)
		if err != nil {
			return err
		}
		if alive {
			return errors.New("retained workspace hook process is still running")
		}
	}
	lifetime, err := openRetainedLifetime(dir, filepath.Join(marker, record.DescriptorDir, "lifetime"))
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, lifetime.reader.Close()) }()
	identity, err := lifetime.fileIdentity()
	if err != nil {
		return err
	}
	if identity != record.Descriptor {
		return errors.New("retained workspace hook descriptor does not match its recorded identity")
	}
	active, err := lifetime.Active()
	if err != nil {
		return err
	}
	if active {
		return errors.New("retained workspace hook descriptor still has a writer")
	}
	current, err := dir.Lstat(readyPath)
	if err != nil {
		return err
	}
	if !os.SameFile(ready, current) {
		return errors.New("retained hook ready marker changed during verification")
	}
	return nil
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
