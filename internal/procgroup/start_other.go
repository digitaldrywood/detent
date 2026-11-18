//go:build !windows

package procgroup

import (
	"fmt"
	"os"
	"os/exec"
)

func Start(cmd *exec.Cmd) error {
	file, slot, err := scratchLaunchFile(cmd.Env)
	if err != nil {
		return err
	}
	if file != nil {
		defer file.Close()
		original := cmd.ExtraFiles
		if slot < len(original) && original[slot] != nil {
			return fmt.Errorf("attempt scratch descriptor %d already in use", slot+3)
		}
		files := make([]*os.File, max(slot+1, len(original)))
		copy(files, original)
		files[slot] = file
		cmd.ExtraFiles = files
		defer func() { cmd.ExtraFiles = original }()
	}
	return cmd.Start()
}

func Wait(cmd *exec.Cmd) error {
	return cmd.Wait()
}
