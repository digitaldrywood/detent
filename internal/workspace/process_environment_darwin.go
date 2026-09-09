package workspace

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func scratchEnvironmentProcessIDs(ctx context.Context, root string) ([]int, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Geteuid())
	if err != nil {
		return nil, err
	}
	var owned []int
	for _, process := range processes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid := int(process.Proc.P_pid)
		if pid <= 0 || pid == os.Getpid() || process.Proc.P_stat == 5 {
			continue
		}
		data, err := readDarwinScratchEnvironment(ctx, func() ([]byte, error) {
			return unix.SysctlRaw("kern.procargs2", pid)
		}, func() (bool, error) {
			current, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
			return len(current) > 0 && current[0].Proc.P_stat != 5, err
		})
		if err != nil {
			return nil, fmt.Errorf("inspect worker scratch ownership for process %d: %w", pid, err)
		}
		if workerScratchEnvironmentMatches(root, darwinProcessEnvironment(data)) {
			owned = append(owned, pid)
		}
	}
	return owned, nil
}

func readDarwinScratchEnvironment(ctx context.Context, read func() ([]byte, error), alive func() (bool, error)) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := read()
		if err == nil || errors.Is(err, unix.ESRCH) {
			return data, nil
		}
		if !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.EIO) {
			return nil, err
		}
		running, inspectErr := alive()
		if errors.Is(inspectErr, unix.ESRCH) || inspectErr == nil && !running {
			return nil, nil
		}
		if inspectErr != nil {
			return nil, errors.Join(err, inspectErr)
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
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
