//go:build !windows

package procgroup

import "os/exec"

func Start(cmd *exec.Cmd) error {
	return cmd.Start()
}
