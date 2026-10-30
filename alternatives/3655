package workspace

import (
	"context"
	"os"
)

func scratchEnvironmentProcessIDs(ctx context.Context, root string) ([]int, error) {
	return procFSScratchEnvironmentProcessIDs(ctx, root, "/proc", os.ReadFile)
}
