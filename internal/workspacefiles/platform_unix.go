//go:build unix

package workspacefiles

import (
	"io/fs"
	"syscall"
)

// openNoFollow refuses to open a final component that is a symbolic link.
// Section 18.4 asks for it by name, and it is the half of the containment
// argument that a check on the request string cannot make: the link can be
// swapped in between the check and the open, and only the kernel can refuse
// atomically.
const openNoFollow = syscall.O_NOFOLLOW

// fileDevice reports the filesystem a file lives on, so a mount point under
// the worktree can be refused. os.Root explicitly does not stop traversal of
// filesystem boundaries, and a bind mount inside a worktree is exactly how a
// reader would be shown something the worktree does not contain.
func fileDevice(info fs.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Dev), true //nolint:unconvert // Stat_t.Dev is int32 on darwin and uint64 on linux.
}
