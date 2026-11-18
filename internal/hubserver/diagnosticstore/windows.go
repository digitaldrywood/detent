package diagnosticstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

var errInterrupted = errors.New("diagnostics refresh interrupted")

type ReportingWindow struct {
	From, To, Observed time.Time
}

type Windows struct {
	mu      sync.Mutex
	loading map[windowKey]*windowRead
}

type windowKey struct {
	organization                  tracker.OrganizationID
	generation                    int64
	revision, projects, principal string
	from, to, observed            time.Time
}

type windowRead struct {
	ready    chan struct{}
	snapshot Snapshot
	err      error
}

func (w *Windows) Read(ctx context.Context, reader Reader, org tracker.OrganizationID, generation int64, projects []tracker.ProjectID, window ReportingWindow, principal string) (Snapshot, error) {
	load := func() (Snapshot, error) {
		return reader.DiagnosticsSnapshot(ctx, projects, window.From, window.To, window.Observed, principal)
	}
	versioned, ok := reader.(interface {
		DiagnosticsReadVersion(context.Context) (string, error)
	})
	if !ok {
		return load()
	}
	revision, err := versioned.DiagnosticsReadVersion(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if revision == "" {
		return load()
	}
	key := windowKey{
		organization: org, generation: generation, revision: revision,
		projects: fmt.Sprintf("%q", projects), principal: principal,
		from: window.From.UTC(), to: window.To.UTC(), observed: window.Observed.UTC(),
	}
	w.mu.Lock()
	if pending := w.loading[key]; pending != nil {
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		case <-pending.ready:
			return pending.snapshot, pending.err
		}
	}
	if w.loading == nil {
		w.loading = make(map[windowKey]*windowRead)
	}
	result := &windowRead{ready: make(chan struct{}), err: errInterrupted}
	w.loading[key] = result
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.loading, key)
		close(result.ready)
		w.mu.Unlock()
	}()
	result.snapshot, result.err = load()
	return result.snapshot, result.err
}
