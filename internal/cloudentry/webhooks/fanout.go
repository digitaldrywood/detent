package webhooks

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

type fanoutStore interface {
	ReleaseGitHubStaging(context.Context, *platformstore.GitHubFanoutPage) error
	NextGitHubFanout(context.Context, time.Time, int) (*platformstore.GitHubFanoutPage, error)
	AdvanceGitHubFanout(context.Context, *platformstore.GitHubFanoutPage, bool, time.Time) error
}

func FanoutFeature(
	store fanoutStore,
	now func() time.Time,
	deliver func(context.Context, hubmaintenance.Tenant, *platformstore.GitHubFanoutPage) error,
	read func(context.Context, string, string, string) ([]byte, error),
	remove func(context.Context, string, string, string) error,
) hubmaintenance.Feature {
	const handoffBytes = 8 * cloudassert.MaxBodyBytes
	var page *platformstore.GitHubFanoutPage
	var advanced bool
	advance := func(ctx context.Context, workErr error) error {
		var err error
		if page.Staging && (errors.Is(workErr, artifact.ErrMissing) || errors.Is(workErr, artifact.ErrIntegrity)) {
			err = store.ReleaseGitHubStaging(ctx, page)
			workErr = nil
		} else {
			err = store.AdvanceGitHubFanout(ctx, page, workErr != nil, now())
		}
		advanced = err == nil && workErr == nil
		if errors.Is(err, corestore.ErrRevisionConflict) {
			return nil
		}
		return errors.Join(workErr, err)
	}
	return hubmaintenance.Feature{
		Name: "github-webhook-fanout", Lead: true, Bytes: handoffBytes,
		Continue: func() bool { return advanced },
		Due: func(ctx context.Context, _ int) ([]hubmaintenance.Tenant, error) {
			advanced = false
			var err error
			page, err = store.NextGitHubFanout(ctx, now(), 1)
			if err != nil || page == nil {
				return nil, err
			}
			if page.Staging {
				if err := hubmaintenance.ReserveDueBytes(ctx, handoffBytes); err != nil {
					return nil, err
				}
				err := runFanout(ctx, func(ctx context.Context) error {
					_, err := read(ctx, page.DeliveryID, page.Digest, page.ObjectKey)
					return err
				})
				return nil, advance(ctx, err)
			}
			if page.Cleanup {
				err = runFanout(ctx, func(ctx context.Context) error {
					return remove(ctx, page.DeliveryID, page.Digest, page.ObjectKey)
				})
				return nil, advance(ctx, err)
			}
			switch len(page.Targets) {
			case 0:
				return nil, advance(ctx, nil)
			case 1:
				return page.Targets, nil
			default:
				return nil, errors.New("GitHub fanout exceeded its binding page")
			}
		},
		Step: func(ctx context.Context, selected hubmaintenance.Tenant, _ hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
			if page == nil || len(page.Targets) != 1 || page.Targets[0] != selected {
				return hubmaintenance.Outcome{}, corestore.ErrTenantMismatch
			}
			if err := hubmaintenance.ReserveBytes(ctx, selected, handoffBytes); err != nil {
				return hubmaintenance.Outcome{}, err
			}
			err := runFanout(ctx, func(ctx context.Context) error { return deliver(ctx, selected, page) })
			return hubmaintenance.Outcome{Progress: err == nil, Items: 1}, advance(ctx, err)
		},
	}
}

func runFanout(ctx context.Context, work func(context.Context) error) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	defer func() {
		if recover() != nil {
			var callers [8]uintptr
			frames := runtime.CallersFrames(callers[:runtime.Callers(3, callers[:])])
			frame, _ := frames.Next()
			err = fmt.Errorf("GitHub webhook fanout panicked at %s:%d", frame.Function, frame.Line)
		}
	}()
	return work(ctx)
}
