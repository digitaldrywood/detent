//go:build linux

package procgroup

import "golang.org/x/sys/unix"

func processNice(pid int) (int, error) {
	priority, err := unix.Getpriority(unix.PRIO_PROCESS, pid)
	if err != nil {
		return 0, err
	}
	return 20 - priority, nil
}
