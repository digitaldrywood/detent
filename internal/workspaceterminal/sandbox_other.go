//go:build !darwin

package workspaceterminal

import "os/exec"

const sandboxSupported = false

func validateSandboxRoot(string) error { return ErrSandboxIsolation }

func sandboxHostProbe() error { return ErrSandboxIsolation }

func sandboxCommand(*exec.Cmd, string, string) error { return ErrSandboxIsolation }
