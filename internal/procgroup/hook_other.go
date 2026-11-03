//go:build !linux

package procgroup

import (
	"context"
	"errors"
	"os/exec"
)

func TerminateHookOwner(context.Context, int, string) (bool, error) {
	return false, nil
}

func RunHook(_ context.Context, cmd *exec.Cmd, started func() error) error {
	if err := Start(cmd); err != nil {
		return err
	}
	if started != nil {
		if err := started(); err != nil {
			return errors.Join(err, TerminateTree(cmd, GroupID(cmd)), Wait(cmd))
		}
	}
	return Wait(cmd)
}
