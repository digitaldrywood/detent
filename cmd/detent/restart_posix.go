//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import "syscall"

func restartProcess(binary string, args []string, env []string) error {
	return syscall.Exec(binary, args, env)
}
