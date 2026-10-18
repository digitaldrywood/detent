package procgroup

import (
	"context"
	"os"
	"time"
)

type Usage struct {
	PeakMemoryBytes  int64   `json:"peak_memory_bytes"`
	UserCPUSeconds   float64 `json:"user_cpu_seconds"`
	SystemCPUSeconds float64 `json:"system_cpu_seconds"`
	WallSeconds      float64 `json:"wall_seconds"`
}

type usageHandlerKey struct{}

func WithUsageHandler(ctx context.Context, handler func(Usage)) context.Context {
	return context.WithValue(ctx, usageHandlerKey{}, handler)
}

func UsageHandler(ctx context.Context) func(Usage) {
	if ctx == nil {
		return nil
	}
	handler, ok := ctx.Value(usageHandlerKey{}).(func(Usage))
	if !ok {
		return nil
	}
	return handler
}

func RecordUsage(handler func(Usage), state *os.ProcessState, startedAt time.Time) {
	if handler == nil {
		return
	}
	if usage := UsageFromState(state, time.Since(startedAt)); usage != nil {
		handler(*usage)
	}
}

func NormalizeUsage[RSS ~int | ~int32 | ~int64](platform string, maxRSS RSS, userCPU, systemCPU, wall time.Duration) *Usage {
	peakMemory := int64(maxRSS)
	switch platform {
	case "darwin":
	case "linux":
		peakMemory *= 1024
	default:
		return nil
	}
	return &Usage{
		PeakMemoryBytes:  peakMemory,
		UserCPUSeconds:   userCPU.Seconds(),
		SystemCPUSeconds: systemCPU.Seconds(),
		WallSeconds:      wall.Seconds(),
	}
}
