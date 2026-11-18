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
	Sampled            bool
}

type Observation struct {
	Snapshot
	To time.Time
}

type Windows struct {
	mu      sync.Mutex
	loading map[windowKey]*windowRead
	recent  [8]*windowRead
	next    int
}

type windowKey struct {
	organization                  tracker.OrganizationID
	generation                    int64
	revision, projects, principal string
	from, to, observed            time.Time
	sampled                       bool
}

type windowRead struct {
	key         windowKey
	ready       chan struct{}
	observation Observation
	err         error
}

func (w *Windows) Read(ctx context.Context, reader Reader, org tracker.OrganizationID, generation int64, projects []tracker.ProjectID, window ReportingWindow, principal string) (Observation, error) {
	load := func() (Observation, error) {
		snapshot, err := reader.DiagnosticsSnapshot(ctx, projects, window.From, window.To, window.Observed, principal)
		return Observation{Snapshot: snapshot, To: window.To}, err
	}
	versioned, ok := reader.(interface {
		DiagnosticsReadVersion(context.Context) (string, error)
	})
	if !ok {
		return load()
	}
	revision, err := versioned.DiagnosticsReadVersion(ctx)
	if err != nil {
		return Observation{}, err
	}
	if revision == "" {
		return load()
	}
	to, observed := window.To.UTC(), window.Observed.UTC()
	if window.Sampled {
		if to.Equal(observed) {
			to = to.Truncate(30 * time.Second)
		}
		observed = observed.Truncate(30 * time.Second)
	}
	if observed.Before(to) {
		observed = to
	}
	key := windowKey{organization: org, generation: generation, revision: revision, projects: fmt.Sprintf("%q", projects), principal: principal, from: window.From.UTC(), to: to, observed: observed, sampled: window.Sampled}
	w.mu.Lock()
	for _, result := range w.recent {
		if result != nil && result.key == key {
			w.mu.Unlock()
			return result.observation, nil
		}
	}
	if pending := w.loading[key]; pending != nil {
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return Observation{}, ctx.Err()
		case <-pending.ready:
			return pending.observation, pending.err
		}
	}
	if w.loading == nil {
		w.loading = make(map[windowKey]*windowRead)
	}
	result := &windowRead{key: key, ready: make(chan struct{}), err: errInterrupted}
	w.loading[key] = result
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		if result.err == nil {
			w.recent[w.next] = result
			w.next = (w.next + 1) % len(w.recent)
		}
		delete(w.loading, key)
		close(result.ready)
		w.mu.Unlock()
	}()
	result.observation, result.err = load()
	return result.observation, result.err
}
