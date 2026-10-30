package workspace

import "context"

type cleanupBatchKey struct{}

type cleanupBatch struct {
	remaining   int
	inventories map[*cleanupInventoryScanner]cleanupInventory
}

// WithCleanupBatch bounds workspace removal attempts across the existing cleanup
// and retention passes. The context belongs to one sequential sweep.
func WithCleanupBatch(ctx context.Context, size int) context.Context {
	return context.WithValue(ctx, cleanupBatchKey{}, &cleanupBatch{remaining: size, inventories: make(map[*cleanupInventoryScanner]cleanupInventory)})
}

func WithCleanupAdmission(ctx, admission context.Context) context.Context {
	return context.WithValue(ctx, cleanupAdmissionKey{}, admission)
}

type cleanupAdmissionKey struct{}

func cleanupAdmissionExpired(ctx context.Context) bool {
	admission, ok := ctx.Value(cleanupAdmissionKey{}).(context.Context)
	return ok && admission.Err() != nil
}

func takeCleanupSlot(ctx context.Context) bool {
	if ctx.Err() != nil || cleanupAdmissionExpired(ctx) {
		return false
	}
	batch, ok := ctx.Value(cleanupBatchKey{}).(*cleanupBatch)
	if !ok {
		return true
	}
	if batch.remaining <= 0 {
		return false
	}
	batch.remaining--
	return true
}

func cleanupBatchExhausted(ctx context.Context) bool {
	if ctx.Err() != nil || cleanupAdmissionExpired(ctx) {
		return true
	}
	batch, ok := ctx.Value(cleanupBatchKey{}).(*cleanupBatch)
	return ok && batch.remaining <= 0
}
