package workspacehub

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestWorkspaceSweepRecoversFromExpiredPageRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 5 * time.Second
		ctx, cancel := context.WithCancel(t.Context())
		stop, stopped := make(chan struct{}), make(chan struct{})
		firstRead := make(chan error, 1)
		completed := make(chan context.Context, 1)
		reads := 0
		service := New(Deps{
			Organization: func() tracker.OrganizationID { return "org_example" },
			Repository: sweepReadRepository{read: func(ctx context.Context) error {
				reads++
				started := time.Now()
				deadline, ok := ctx.Deadline()
				if !ok || !deadline.Equal(started.Add(interval)) {
					err := errors.New("workspace page read lacks a full tick budget")
					if reads == 1 {
						firstRead <- err
					}
					t.Error(err)
					return err
				}
				if reads == 1 {
					<-ctx.Done()
					if elapsed := time.Since(started); elapsed != interval {
						t.Errorf("page read expired after %v, want %v", elapsed, interval)
					}
					firstRead <- ctx.Err()
					return ctx.Err()
				}
				return ctx.Err()
			}},
		})
		deps := SweepDeps{
			Now:             time.Now,
			Due:             service.Due,
			SweepActions:    func(context.Context) {},
			DispatchActions: func(context.Context) {},
			SweepRelay:      func(context.Context, time.Time) {},
			SweepTickets: func(ctx context.Context, _ time.Time) error {
				if ctx.Err() != nil {
					t.Errorf("later tick reached ticket sweep with %v", ctx.Err())
				}
				completed <- ctx
				return nil
			},
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		go Maintain(ctx, stop, stopped, func() time.Duration { return interval }, time.NewTimer, func(ctx context.Context) {
			Sweep(ctx, deps)
		})
		defer func() {
			cancel()
			<-stopped
		}()
		if err := <-firstRead; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("first page read returned %v, want deadline exceeded", err)
		}
		tickCtx := <-completed
		synctest.Wait()
		if !errors.Is(tickCtx.Err(), context.Canceled) || ctx.Err() != nil {
			t.Fatalf("after sweep, tick error = %v, maintenance error = %v", tickCtx.Err(), ctx.Err())
		}
	})
}

type sweepReadRepository struct {
	tenantstore.Repository
	read func(context.Context) error
}

func (r sweepReadRepository) ReadTenant(ctx context.Context, _ tracker.OrganizationID, _ corestore.Workload, _ func(tenantstore.Reader) error) error {
	return r.read(ctx)
}
