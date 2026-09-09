//go:build unix

package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func procFSScratchEnvironmentProcessIDs(ctx context.Context, root string, procRoot string, readEnvironment func(string) ([]byte, error)) ([]int, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	var owned []int
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) {
			continue
		}
		data, err := readEnvironment(filepath.Join(procRoot, entry.Name(), "environ"))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect worker scratch ownership for process %d: %w", pid, err)
		}
		if workerScratchEnvironmentMatches(root, strings.Split(string(data), "\x00")) {
			owned = append(owned, pid)
		}
	}
	return owned, nil
}
