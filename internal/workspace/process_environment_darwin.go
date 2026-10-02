package workspace

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func scratchEnvironmentProcessIDs(ctx context.Context, root string) ([]int, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Geteuid())
	if err != nil {
		return nil, err
	}
	return darwinScratchEnvironmentProcessIDs(ctx, root, processes,
		func(pid int) ([]byte, error) {
			return unix.SysctlRaw("kern.procargs2", pid)
		})
}

func darwinScratchEnvironmentProcessIDs(
	ctx context.Context,
	root string,
	processes []unix.KinfoProc,
	read func(int) ([]byte, error),
) ([]int, error) {
	var owned []int
	var result error
	ownerRoots := workerScratchOwnerRoots(root)
	for _, process := range processes {
		if err := ctx.Err(); err != nil {
			return owned, errors.Join(result, err)
		}
		pid := int(process.Proc.P_pid)
		if pid <= 0 || pid == os.Getpid() || process.Proc.P_stat == 5 {
			continue
		}
		data, err := read(pid)
		if ctx.Err() != nil {
			return owned, errors.Join(result, ctx.Err())
		}
		if err != nil {
			if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EIO) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
				continue
			}
			result = errors.Join(result, fmt.Errorf("inspect worker scratch ownership for process %d: %w", pid, err))
			continue
		}
		if workerScratchEnvironmentMatches(ownerRoots, darwinProcessEnvironment(data)) {
			owned = append(owned, pid)
		}
	}
	return owned, result
}

func darwinProcessEnvironment(data []byte) []string {
	if len(data) < 4 {
		return nil
	}
	arguments := binary.LittleEndian.Uint32(data[:4])
	_, data, ok := bytes.Cut(data[4:], []byte{0})
	if !ok {
		return nil
	}
	data = bytes.TrimLeft(data, "\x00")
	for range arguments {
		_, data, ok = bytes.Cut(data, []byte{0})
		if !ok {
			return nil
		}
	}
	return strings.Split(string(data), "\x00")
}
