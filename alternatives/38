//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"os"
	"syscall"

	"github.com/digitaldrywood/detent/internal/cli"
)

func automaticRestartRequest() *cli.RestartRequest {
	if request := cli.ControllerRestartRequest(); request != nil {
		return request
	}
	return cli.NewProcessRestartRequest(func(binary string) error {
		return restartProcess(binary, os.Args, os.Environ())
	})
}

func restartProcess(binary string, args []string, env []string) error {
	return syscall.Exec(binary, args, env) // #nosec G204,G702 -- binary is the already-running Detent executable and arguments bypass a shell.
}
