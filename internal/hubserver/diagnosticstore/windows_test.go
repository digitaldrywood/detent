package diagnosticstore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type delayedReader struct {
	revision string
	release  chan struct{}
}

func (r delayedReader) DiagnosticsReadVersion(context.Context) (string, error) {
	return r.revision, nil
}

func (r delayedReader) DiagnosticsSnapshot(ctx context.Context, _ []tracker.ProjectID, _, _, _ time.Time, _ string) (Snapshot, error) {
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
		now := time.Now().UTC()
		w := ReportingWindow{From: now.Add(-time.Hour), To: now, Observed: now}
		reader := delayedReader{revision: "first", release: make(chan struct{})}
		release := sync.OnceFunc(func() { close(reader.release) })
		defer release()
		primaryDone := make(chan error, 1)
		go func() {
			_, err := windows.Read(t.Context(), reader, "first", 1, []tracker.ProjectID{"project"}, w, "reporter")
			primaryDone <- err
		}()
		synctest.Wait()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := windows.Read(ctx, reader, "first", 1, []tracker.ProjectID{"project"}, w, "reporter"); !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled waiter: %v", err)
		}
		ready := make(chan struct{})
		close(ready)
		from, to, observed := w, w, w
		from.From = from.From.Add(time.Microsecond)
		to.To = to.To.Add(time.Microsecond)
		observed.Observed = observed.Observed.Add(time.Microsecond)
		for _, tc := range []struct {
			name                string
			org                 tracker.OrganizationID
			generation          int64
			project             tracker.ProjectID
			principal, revision string
			window              ReportingWindow
		}{
			{"organization", "second", 1, "project", "reporter", "first", w},
			{"generation", "first", 2, "project", "reporter", "first", w},
			{"project", "first", 1, "other", "reporter", "first", w},
			{"principal", "first", 1, "project", "other", "first", w},
			{"visibility", "first", 1, "project", "reporter", "second", w},
			{"start", "first", 1, "project", "reporter", "first", from},
			{"end", "first", 1, "project", "reporter", "first", to},
			{"observation", "first", 1, "project", "reporter", "first", observed},
		} {
			finished := make(chan error, 1)
			go func() {
				_, err := windows.Read(t.Context(), delayedReader{revision: tc.revision, release: ready}, tc.org, tc.generation, []tracker.ProjectID{tc.project}, tc.window, tc.principal)
				finished <- err
			}()
			synctest.Wait()
			select {
			case err := <-finished:
				if err != nil {
					t.Error(err)
				}
			default:
				t.Errorf("different %s joined the active read", tc.name)
			}
		}
		release()
		if err := <-primaryDone; err != nil {
			t.Fatal(err)
		}
	})
}
