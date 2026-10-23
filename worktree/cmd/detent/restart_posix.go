//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import "syscall"

func restartProcess(binary string, args []string, env []string) error {
	return syscall.Exec(binary, args, env) // #nosec G204,G702 -- binary is the already-running Detent executable and arguments bypass a shell.
}
