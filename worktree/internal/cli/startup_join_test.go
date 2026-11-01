package cli

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/web"
)

func TestRunStartupAndServeJoinsIdentityFailure(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"listener", "healthy"} {
		for _, heldWorker := range []string{"startup", "serve"} {
			if failure == "healthy" && heldWorker == "startup" {
				continue
			}
			t.Run(failure+"/"+heldWorker, func(t *testing.T) {
				t.Parallel()
				identityErr := errors.New("unexpected restarted identity")
				entered := make(chan struct{})
				canceled := make(chan struct{})
				release := make(chan struct{})
				done := make(chan error, 1)
				var finished atomic.Bool
				held := func(ctx context.Context) error {
					close(entered)
					<-ctx.Done()
					close(canceled)
					<-release
					finished.Store(true)
					return ctx.Err()
				}
				startup := func(context.Context) error { return nil }
				serve := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
				if heldWorker == "startup" {
					startup = held
				} else {
					serve = held
				}
				readiness := startupReadiness{
					AwaitServe: func(context.Context) error {
						<-entered
						if failure == "listener" {
							return identityErr
						}
						return nil
					},
					MarkHealthy: func(context.Context) error { return identityErr },
				}
				// Observe completion inside the owner, before reporting it, so scheduler
				// ordering cannot hide a return that raced the worker's cleanup.
				go func() {
					err := runStartupAndServe(t.Context(), web.NewStartupLifecycle(), startup, readiness, serve)
					if !finished.Load() {
						err = errors.Join(err, errors.New("returned before worker cleanup"))
					}
					done <- err
				}()
				<-canceled
				select {
				case err := <-done:
					close(release)
					t.Fatalf("returned while worker cleanup blocked: %v", err)
				default:
				}
				close(release)
				err := <-done
				if !errors.Is(err, identityErr) || err.Error() != "verify restarted listener: "+identityErr.Error() && err.Error() != "verify healthy restarted build: "+identityErr.Error() {
					t.Fatalf("runStartupAndServe = %v, want identity failure after joined cleanup", err)
				}
			})
		}
	}
}
