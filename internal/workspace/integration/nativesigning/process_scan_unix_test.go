//go:build unix

package nativesigning_test

import (
	"context"

	"github.com/digitaldrywood/detent/internal/workspace/hookproc"
)

func stubReapProcessScanner() {
	hookproc.ReapProcessScanner = func(context.Context, string) ([]int, error) { return nil, nil }
}
