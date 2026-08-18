//go:build linux

package hostmemory

import (
	"context"
	"os"
	"time"
)

func Read(ctx context.Context) (Sample, error) {
	select {
	case <-ctx.Done():
		return Sample{}, ctx.Err()
	default:
	}
	data, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return Sample{}, err
	}
	sample, err := Parse(string(data))
	if err != nil {
		return Sample{}, err
	}
	sample.ObservedAt = time.Now().UTC()
	return sample, nil
}
