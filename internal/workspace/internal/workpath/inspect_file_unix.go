//go:build unix

package workpath

import "syscall"

const InspectFileOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
