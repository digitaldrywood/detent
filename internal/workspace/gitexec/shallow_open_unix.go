//go:build unix

package gitexec

import "syscall"

const shallowOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
