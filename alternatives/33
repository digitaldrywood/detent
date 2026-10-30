//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package main

import "github.com/digitaldrywood/detent/internal/cli"

func automaticRestartRequest() *cli.RestartRequest {
	return cli.NewRestartRequest()
}

func restartProcess(string, []string, []string) error {
	return nil
}
