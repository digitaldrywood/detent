package hookproc

import (
	"context"
	"os"
	"strconv"
	"strings"
)

func scratchEnvironmentProcessIDs(ctx context.Context, root string) ([]int, error) {
	return procFSScratchEnvironmentProcessIDs(ctx, root, "/proc", os.ReadFile)
}

func processEnvironment(pid int) ([]string, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, err
	}
	return strings.Split(string(data), "\x00"), nil
}
