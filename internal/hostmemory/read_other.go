//go:build !linux

package hostmemory

import "context"

func Read(context.Context) (Sample, error) {
	return Sample{}, ErrUnsupported
}
