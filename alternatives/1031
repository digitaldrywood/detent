//go:build !linux

package hostpressure

import "context"

func ReadMemory(context.Context) (Sample, error) {
	return Sample{}, ErrUnsupported
}

func ReadIO(context.Context) (Sample, error) {
	return Sample{}, ErrUnsupported
}

func ReadCPU(context.Context) (Sample, error) {
	return Sample{}, ErrUnsupported
}
