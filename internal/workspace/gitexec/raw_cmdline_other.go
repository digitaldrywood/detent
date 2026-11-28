//go:build !windows

package gitexec

import "os/exec"

func hasRawCommandLine(*exec.Cmd) bool {
	return false
}
