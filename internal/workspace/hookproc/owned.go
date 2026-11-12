package hookproc

import (
	"context"
	"os"
)

type Scanner func(context.Context, string) ([]int, error)

func ScanOwned(ctx context.Context, path string, scan Scanner) ([]int, error) {
	pids, err := scan(ctx, path)
	owned := make([]int, 0, len(pids))
	seen := make(map[int]struct{}, len(pids))
	for _, pid := range pids {
		if pid <= 0 || pid == os.Getpid() {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		owned = append(owned, pid)
	}
	return owned, err
}
