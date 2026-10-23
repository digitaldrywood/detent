package workspace

import "context"

type cleanupBatchKey struct{}

// WithCleanupBatch bounds workspace removal attempts across the existing cleanup
// and retention passes. The context belongs to one sequential sweep.
func WithCleanupBatch(ctx context.Context, size int) context.Context {
	return context.WithValue(ctx, cleanupBatchKey{}, &size)
}

func takeCleanupSlot(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	remaining, ok := ctx.Value(cleanupBatchKey{}).(*int)
	if !ok {
		return true
	}
	if *remaining <= 0 {
		return false
	}
	*remaining--
	return true
}

func cleanupBatchExhausted(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	remaining, ok := ctx.Value(cleanupBatchKey{}).(*int)
	return ok && *remaining <= 0
}
