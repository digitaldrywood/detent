package runner

import "context"

func emptyProcessSource(context.Context, string) ([]int, error) {
	return nil, nil
}
