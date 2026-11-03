//go:build !linux

package procgroup

import (
	"context"
	"os/exec"
)

func TerminateHookOwner(context.Context, int, string) (bool, error) {
	return false, nil
}

func RunHook(cmd *exec.Cmd) error {
	if err := Start(cmd); err != nil {
		return err
	}
	return Wait(cmd)
}
