package hookproc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

func (l *Lifetime) platformWriters(ctx context.Context) ([]int, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Geteuid())
	if err != nil {
		return nil, fmt.Errorf("list hook process candidates: %w", err)
	}
	var marker unix.Stat_t
	if err := unix.Fstat(int(l.reader.Fd()), &marker); err != nil {
		return nil, err
	}
	var pids []int
	for _, process := range processes {
		if err := ctx.Err(); err != nil {
			return pids, err
		}
		pid := int(process.Proc.P_pid)
		if pid <= 0 || pid == os.Getpid() || process.Proc.P_stat == 5 {
			continue
		}
		writer, err := holdsDarwinFIFOWriter(pid, marker)
		if err != nil {
			return pids, err
		}
		if writer {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func (l *Lifetime) platformHoldsWriter(pid int) (bool, error) {
	var marker unix.Stat_t
	if err := unix.Fstat(int(l.reader.Fd()), &marker); err != nil {
		return false, err
	}
	return holdsDarwinFIFOWriter(pid, marker)
}

func holdsDarwinFIFOWriter(pid int, marker unix.Stat_t) (bool, error) {
	const (
		procInfoPIDFDInfo  = 3
		procPIDFDVnodeInfo = 1
		hookDescriptor     = 4
		fileWrite          = 2
		fileAccessMask     = 3
	)
	var vnode struct {
		openFlags    uint32
		fileInfo     [20]byte
		device       uint32
		mode         uint16
		links        uint16
		inode        uint64
		statAndVnode [136]byte
	}
	size, _, err := unix.Syscall6(unix.SYS_PROC_INFO, procInfoPIDFDInfo, uintptr(pid), procPIDFDVnodeInfo, hookDescriptor, uintptr(unsafe.Pointer(&vnode)), unsafe.Sizeof(vnode))
	if err != 0 {
		if errors.Is(err, unix.EBADF) || errors.Is(err, unix.ESRCH) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			return false, nil
		}
		return false, fmt.Errorf("inspect hook descriptor for PID %d: %w", pid, err)
	}
	if size != unsafe.Sizeof(vnode) {
		return false, fmt.Errorf("inspect hook descriptor for PID %d: unexpected vnode info size %d", pid, size)
	}
	return vnode.openFlags&fileAccessMask == fileWrite && vnode.mode&unix.S_IFMT == unix.S_IFIFO && vnode.device == uint32(marker.Dev) && vnode.inode == marker.Ino, nil
}
