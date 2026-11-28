//go:build windows

package gitexec

import "os/exec"

func hasRawCommandLine(cmd *exec.Cmd) bool {
	return cmd != nil && cmd.SysProcAttr != nil && cmd.SysProcAttr.CmdLine != ""
}
