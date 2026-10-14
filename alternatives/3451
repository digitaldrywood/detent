//go:build !darwin

package workspaceterminal

import (
	"context"
	"os/exec"
)

const sandboxSupported = false

func validateSandboxRoot(string) error { return ErrSandboxIsolation }

func sandboxHostProbe(context.Context) error { return ErrSandboxIsolation }

func sandboxCommand(*exec.Cmd, string, string) error { return ErrSandboxIsolation }
