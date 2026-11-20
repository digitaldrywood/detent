package runobserve

import (
	"context"
	"sync"

	"github.com/digitaldrywood/detent/internal/store"
)

func KeepProviderIdentity(ctx context.Context, write func(context.Context, store.SessionProviderIdentity) error, report func(error)) (func(store.SessionProviderIdentity), func(), func()) {
	ctx, cancel := context.WithCancel(ctx)
	var mu sync.Mutex
	var latest, acknowledged store.SessionProviderIdentity
	wake, done := make(chan struct{}, 1), make(chan struct{})
	retry := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return
			case <-wake:
				mu.Lock()
				identity, pending := latest, latest != acknowledged
				mu.Unlock()
				if !pending {
					continue
				}
				if err := write(ctx, identity); err != nil {
					if ctx.Err() == nil {
						report(err)
					}
					continue
				}
				mu.Lock()
				acknowledged = identity
				mu.Unlock()
			}
		}
	}()
	return func(identity store.SessionProviderIdentity) {
			mu.Lock()
			defer mu.Unlock()
			latest = identity
			if latest != acknowledged {
				retry()
			}
		}, retry, func() {
			cancel()
			<-done
		}
}
