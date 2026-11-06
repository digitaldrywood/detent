package workspaceterminal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const sandboxSupported = true

func sandboxHostProbe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", "(version 1)(allow default)(deny file-write*)", "/usr/bin/true")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: host denied sandbox launch: %w", ErrSandboxIsolation, err)
	}
	return nil
}

func validateSandboxRoot(root string) error {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || canonical != root || !filepath.IsAbs(root) || root == "/" {
		return fmt.Errorf("%w: worktree identity changed", ErrSandboxIsolation)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("%w: runner home is unknown", ErrSandboxIsolation)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil || containsPath(root, home) {
		return fmt.Errorf("%w: worktree contains the runner home", ErrSandboxIsolation)
	}
	count, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return fmt.Errorf("%w: inspect mounts: %w", ErrSandboxIsolation, err)
	}
	mounts := make([]unix.Statfs_t, count+1)
	count, err = unix.Getfsstat(mounts, unix.MNT_NOWAIT)
	if err != nil || count >= len(mounts) {
		return fmt.Errorf("%w: mounts could not be resolved", ErrSandboxIsolation)
	}
	for _, mount := range mounts[:count] {
		path := unix.ByteSliceToString(mount.Mntonname[:])
		within, err := mountWithinRoot(root, path)
		if err != nil {
			return fmt.Errorf("%w: resolve mount: %w", ErrSandboxIsolation, err)
		}
		if within {
			return fmt.Errorf("%w: worktree contains a mount", ErrSandboxIsolation)
		}
	}
	return nil
}

func mountWithinRoot(root, mount string) (bool, error) {
	canonical, err := filepath.EvalSymlinks(mount)
	if err != nil {
		return false, err
	}
	return containsPath(root, canonical), nil
}

func sandboxCommand(cmd *exec.Cmd, root, tty string) error {
	if err := validateSandboxRoot(root); err != nil {
		return err
	}
	args := []string{"/usr/bin/sandbox-exec", "-p", sandboxProfile(root, tty), "/bin/sh", "-c",
		`cd "$1" || exit 125; shift; printf '\001' >&3 || exit 125; exec 3>&-; exec "$@"`, "terminal", cmd.Dir, cmd.Path}
	cmd.Args = append(args, cmd.Args[1:]...)
	cmd.Path = "/usr/bin/sandbox-exec"
	cmd.Dir = root
	return nil
}

func sandboxProfile(root, tty string) string {
	var profile strings.Builder
	profile.WriteString(`(version 1)
(deny default)
(allow syscall-unix)
(deny syscall-unix (syscall-number SYS_setsid SYS_setpgid SYS_posix_spawn))
(allow process-fork process-exec)
(allow signal (target same-sandbox))
(allow sysctl-read
 (sysctl-name-prefix "hw.")
 (sysctl-name "kern.ostype") (sysctl-name "kern.osrelease")
 (sysctl-name "kern.osversion") (sysctl-name "kern.argmax"))
(allow file-read* file-map-executable
 (subpath "/bin") (subpath "/usr/bin")
 (subpath "/usr/lib") (subpath "/System/Library")
 (subpath "/Library/Apple/usr/lib"))
(allow file-read-metadata`)
	for parent := filepath.Dir(root); ; parent = filepath.Dir(parent) {
		profile.WriteString(" (literal " + strconv.Quote(parent) + ")")
		if parent == "/" {
			break
		}
	}
	profile.WriteString(")\n(allow file-read* file-write* file-map-executable (subpath " + strconv.Quote(root) + "))\n")
	profile.WriteString(`(allow file-read* file-write-data
 (literal "/dev/null") (literal "/dev/zero"))
(allow file-read* (literal "/dev/random") (literal "/dev/urandom"))
(allow file-read* file-write-data file-ioctl (literal "/dev/tty") (literal ` + strconv.Quote(tty) + "))\n")
	return profile.String()
}
