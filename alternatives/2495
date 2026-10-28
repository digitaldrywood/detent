//go:build unix && !linux

package procgroup

import "golang.org/x/sys/unix"

func processNice(pid int) (int, error) {
	return unix.Getpriority(unix.PRIO_PROCESS, pid)
}
