package darwinproc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const ScratchFD = 5

func AttemptScratch(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	id, ok := strings.CutPrefix(filepath.Base(path), "attempt-")
	if !ok || len(id) != 36 || uuid.Validate(id) != nil {
		return false
	}
	parent := filepath.Dir(path)
	base := filepath.Base(filepath.Dir(filepath.Dir(parent)))
	return base == fmt.Sprintf("detent-worker-scratch-%d", os.Getuid()) || base == "detent-worker-scratch" || (filepath.Base(parent) == "worker-tmp" && filepath.Base(filepath.Dir(parent)) == ".detent")
}

func OpenScratch(env []string) (*os.File, error) {
	var path string
	for _, entry := range env {
		if strings.HasPrefix(entry, "DETENT_HOOK_OWNER=") && entry != "DETENT_HOOK_OWNER=" {
			return nil, nil
		}
		if value, ok := strings.CutPrefix(entry, "DETENT_WORKER_SCRATCH="); ok {
			path = value
		}
	}
	if !AttemptScratch(path) {
		return nil, nil
	}
	fd, err := unix.Open(path, unix.O_EVTONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open attempt scratch descriptor: %w", err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

type vnodeFDInfo struct {
	OpenFlags uint32
	_         [20]byte
	Device    uint32
	Mode      uint16
	_         uint16
	Inode     uint64
	_         [136]byte
}

var vnodeFD = sync.OnceValues(func() (func(int32, int32, int32, *vnodeFDInfo, int32) int32, error) {
	var inspect func(int32, int32, int32, *vnodeFDInfo, int32) int32
	symbol, err := purego.Dlsym(purego.RTLD_DEFAULT, "proc_pidfdinfo")
	if err == nil {
		purego.RegisterFunc(&inspect, symbol)
	}
	return inspect, err
})

func HoldsScratch(pid int, dir os.FileInfo) (bool, error) {
	inspect, err := vnodeFD()
	if err != nil {
		return false, err
	}
	var vnode vnodeFDInfo
	size := int32(unsafe.Sizeof(vnode))
	const procPIDFDVnodeInfo = 1
	n := inspect(int32(pid), ScratchFD, procPIDFDVnodeInfo, &vnode, size)
	if n == 0 {
		return false, nil
	}
	if n != size {
		return false, errors.New("incomplete process descriptor identity")
	}
	stat := dir.Sys().(*syscall.Stat_t)
	return vnode.OpenFlags&unix.O_EVTONLY != 0 && vnode.Mode&unix.S_IFMT == unix.S_IFDIR && vnode.Device == uint32(stat.Dev) && vnode.Inode == stat.Ino, nil
}
