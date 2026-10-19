//go:build !darwin && !linux

package procstart

import (
	"errors"
	"os"
	"os/exec"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

func identity(pid int) (string, error) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return "", ErrNotRunning
	}
	inspected, err := procgroup.Inspect(&exec.Cmd{Process: process})
	if errors.Is(err, procgroup.ErrProcessNotRunning) {
		return "", ErrNotRunning
	}
	if err != nil {
		return "", err
	}
	return "procgroup:" + inspected.StartedAt.UTC().Format(time.RFC3339Nano), nil
}
