package runnersession

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubclient/transport"
	"github.com/digitaldrywood/detent/internal/store"
)

type WorkerObservation struct {
	pending store.WorkerProcessRegistration
}

func (o *WorkerObservation) Report(ctx context.Context, sessionID int64, process store.WorkerProcessIdentity, root, path string, write func(context.Context, int64, store.WorkerProcessRegistration) error) error {
	if sessionID <= 0 {
		return nil
	}
	if process.PID > 0 && !process.StartedAt.IsZero() {
		o.pending = store.WorkerProcessRegistration{WorkerProcessIdentity: process, CleanupRoot: strings.TrimSpace(root), CleanupPath: strings.TrimSpace(path)}
	}
	if o.pending.PID == 0 {
		return nil
	}
	err := write(ctx, sessionID, o.pending)
	if transport.DeferObservation(ctx, err) {
		return nil
	}
	o.pending = store.WorkerProcessRegistration{}
	if err != nil {
		return fmt.Errorf("update agent session worker process: %w", err)
	}
	return nil
}
