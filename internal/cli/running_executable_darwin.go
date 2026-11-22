package cli

import (
	"errors"
	"io"
	"os"
	"reflect"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

type executableRegion struct {
	protection uint32
	_          [76]byte
	address    uint64
	size       uint64
	device     int32
	_          [4]byte
	inode      uint64
	_          [96]byte
	generation uint32
	_          [1060]byte
}

func copyRunningExecutable(dst io.Writer, path string) (copyErr error) {
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { copyErr = errors.Join(copyErr, file.Close()) }()
	var before unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &before); err != nil {
		return err
	}
	const procInfoPID = 2
	const procPIDRegionPathInfo = 8
	pid := os.Getpid()
	address := reflect.ValueOf(copyRunningExecutable).Pointer()
	var region executableRegion
	size, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoPID, uintptr(pid), procPIDRegionPathInfo, address, uintptr(unsafe.Pointer(&region)), unsafe.Sizeof(region))
	if errno != 0 {
		return errno
	}
	if size != unsafe.Sizeof(region) || region.protection&unix.PROT_EXEC == 0 || uint64(address) < region.address || uint64(address)-region.address >= region.size ||
		before.Mode&unix.S_IFMT != unix.S_IFREG || region.inode == 0 || region.device != before.Dev || region.inode != before.Ino || region.generation != before.Gen {
		return errors.New("executable file does not back the running image")
	}
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return err
	}
	started := process.Proc.P_starttime
	if !time.Unix(before.Ctim.Sec, before.Ctim.Nsec).Before(time.Unix(started.Sec, int64(started.Usec)*int64(time.Microsecond))) {
		return errors.New("executable file changed since process start")
	}
	if _, err := io.Copy(dst, file); err != nil {
		return err
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &after); err != nil {
		return err
	}
	if before.Ctim != after.Ctim || before.Mtim != after.Mtim || before.Size != after.Size {
		return errors.New("executable file changed while hashing the running image")
	}
	return nil
}
