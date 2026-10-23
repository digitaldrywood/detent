//go:build unix && !linux

package procgroup

import (
	"context"
	"os/exec"
)

func configureSandboxCapabilities(context.Context, *exec.Cmd) error {
	return nil
}
