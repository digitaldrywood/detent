package pgtest

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

func checkBinaryIPC() error {
	check, err := purego.Dlsym(purego.RTLD_DEFAULT, "sandbox_check")
	if err != nil {
		return fmt.Errorf("resolve sandbox_check: %w", err)
	}
	const operation = "ipc-sysv-shm\x00"
	result, _, errno := purego.SyscallN(check, uintptr(os.Getpid()), uintptr(unsafe.Pointer(unsafe.StringData(operation))), 0)
	switch int32(result) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("macOS sandbox denies ipc-sysv-shm: %w", syscall.EPERM)
	default:
		return fmt.Errorf("sandbox_check for ipc-sysv-shm returned %d: %w", int32(result), syscall.Errno(errno))
	}
}
