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

const darwinScratchEnvironmentRetryTimeout = time.Second

func scratchEnvironmentProcessIDs(ctx context.Context, root string) ([]int, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Geteuid())
	if err != nil {
		return nil, err
	}
	return darwinScratchEnvironmentProcessIDs(ctx, root, processes, darwinScratchEnvironmentRetryTimeout,
		func(pid int) ([]byte, error) {
			return unix.SysctlRaw("kern.procargs2", pid)
		}, func(pid int) (bool, error) {
			current, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
			return len(current) > 0 && current[0].Proc.P_stat != 5, err
		})
}

func darwinScratchEnvironmentProcessIDs(
	ctx context.Context,
	root string,
	processes []unix.KinfoProc,
	retryTimeout time.Duration,
	read func(int) ([]byte, error),
	alive func(int) (bool, error),
) ([]int, error) {
	var owned []int
	var result error
	for _, process := range processes {
		if err := ctx.Err(); err != nil {
			return owned, errors.Join(result, err)
		}
		pid := int(process.Proc.P_pid)
		if pid <= 0 || pid == os.Getpid() || process.Proc.P_stat == 5 {
			continue
		}
		data, err := readDarwinScratchEnvironment(ctx, retryTimeout, func() ([]byte, error) {
			return read(pid)
		}, func() (bool, error) {
			return alive(pid)
		})
		if err != nil {
			result = errors.Join(result, fmt.Errorf("inspect worker scratch ownership for process %d: %w", pid, err))
			continue
		}
		if workerScratchEnvironmentMatches(root, darwinProcessEnvironment(data)) {
			owned = append(owned, pid)
		}
	}
	return owned, result
}

func readDarwinScratchEnvironment(ctx context.Context, retryTimeout time.Duration, read func() ([]byte, error), alive func() (bool, error)) ([]byte, error) {
	// XNU can report EINVAL while exec or exit leaves a live process without a
	// readable user stack. Bound each process separately so a stalled transition
	// cannot consume the reap operation's entire deadline before the scan advances.
	ctx, cancel := context.WithTimeout(ctx, retryTimeout)
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
