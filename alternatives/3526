//go:build unix

package workspace

import (
	"context"
	"sync"
	"testing"
)

var realProcessScanRoots sync.Map

func useRealProcessScan(t *testing.T, root string) {
	t.Helper()
	if testing.Short() {
		t.Skip("real host process scan integration")
	}

	canonical, err := canonicalExistingPath(root)
	if err != nil {
		t.Fatal(err)
	}
	realProcessScanRoots.Store(canonical, struct{}{})
	t.Cleanup(func() { realProcessScanRoots.Delete(canonical) })
}

func stubReapProcessScanner() {
	reapProcessScanner = func(ctx context.Context, path string) ([]int, error) {
		canonical, err := canonicalExistingPath(path)
		if err != nil {
			return nil, nil
		}
		real := false
		realProcessScanRoots.Range(func(key, _ any) bool {
			root := key.(string)
			if root == canonical || pathInside(root, canonical) {
				real = true
				return false
			}
			return true
		})
		if !real {
			return nil, nil
		}
		return workspaceProcessIDs(ctx, path)
	}
}
