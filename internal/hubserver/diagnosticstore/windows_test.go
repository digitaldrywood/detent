package diagnosticstore

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type delayedReader struct {
	loads   atomic.Int32
	release chan struct{}
}

func (*delayedReader) DiagnosticsReadVersion(context.Context) (string, error) { return "same", nil }

func (r *delayedReader) DiagnosticsSnapshot(ctx context.Context, _ []tracker.ProjectID, _, _, _ time.Time, _ string) (Snapshot, error) {
	r.loads.Add(1)
	select {
	case <-r.release:
		return Snapshot{Attempts: 7}, nil
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
}

func TestConcurrentDiagnosticsWindows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var windows Windows
		reader := &delayedReader{release: make(chan struct{})}
		defer func() {
			if reader.release != nil {
				close(reader.release)
			}
		}()
		now := time.Now().UTC()
		window := ReportingWindow{From: now.Add(-time.Hour), To: now, Observed: now, Sampled: true}
		read := func(ctx context.Context, org tracker.OrganizationID, offset time.Duration) error {
			request := window
			request.To, request.Observed = now.Add(offset), now.Add(offset)
			got, err := windows.Read(ctx, reader, org, 1, []tracker.ProjectID{"project"}, request, "")
			if err == nil && (got.Attempts != 7 || !got.To.Equal(now)) {
				t.Errorf("response = %+v, want first observed window and seven attempts", got)
			}
			return err
		}
		results := make(chan error, 10)
		for _, org := range []tracker.OrganizationID{"first", "second"} {
			go func() { results <- read(t.Context(), org, 0) }()
			synctest.Wait()
			for range 4 {
				go func() { results <- read(t.Context(), org, time.Second) }()
			}
		}
		synctest.Wait()
		if got := reader.loads.Load(); got != 2 {
			t.Errorf("concurrent diagnostics reads = %d, want 1 per organization", got)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := read(ctx, "first", 0); !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled waiter returned %v", err)
		}
		close(reader.release)
		synctest.Wait()
		reader.release = nil
		for range cap(results) {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
	})
}
