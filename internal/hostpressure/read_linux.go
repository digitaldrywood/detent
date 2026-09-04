//go:build linux

package hostpressure

import (
	"context"
	"os"
	"time"
)

func ReadMemory(ctx context.Context) (Sample, error) {
	return read(ctx, "/proc/pressure/memory")
}

func ReadIO(ctx context.Context) (Sample, error) {
	return read(ctx, "/proc/pressure/io")
}

func ReadCPU(ctx context.Context) (Sample, error) {
	return read(ctx, "/proc/pressure/cpu")
}

func read(ctx context.Context, path string) (Sample, error) {
	select {
	case <-ctx.Done():
		return Sample{}, ctx.Err()
	default:
	}
	data, err := os.ReadFile(path)
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
