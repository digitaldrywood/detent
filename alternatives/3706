//go:build !unix

package workspacefiles

import "io/fs"

// openNoFollow is zero where the platform has no O_NOFOLLOW. Containment there
// rests on os.Root, which refuses a name that leaves the root, and on the
// explicit lstat that refuses a symbolic link before anything is opened; what
// is missing is only the atomicity of the kernel's own refusal.
const openNoFollow = 0

// fileDevice reports that the platform cannot answer which filesystem a file
// is on, so the mount-point check is skipped rather than guessed at.
func fileDevice(fs.FileInfo) (uint64, bool) { return 0, false }
