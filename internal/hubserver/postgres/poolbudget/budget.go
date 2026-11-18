package poolbudget

import (
	"context"
	"errors"
	"sync"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrWorkload = errors.New("unknown PostgreSQL workload")

type Budget struct {
	serving    chan struct{}
	background chan struct{}
	tenants    servingAdmission
}

func New(serving, background int) *Budget {
	return &Budget{serving: make(chan struct{}, serving), background: make(chan struct{}, background)}
}

func (b *Budget) Capacities() (serving, background int) {
	return cap(b.serving), cap(b.background)
}

func (b *Budget) Acquire(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload) (func(), error) {
	slots := b.serving
	switch workload {
	case corestore.Serving:
		if corestore.ContextWorkload(ctx) == corestore.Background {
			slots = b.background
		}
	case corestore.Background:
		slots = b.background
	default:
		return nil, ErrWorkload
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if org != "" && slots == b.serving {
		return b.tenants.acquire(ctx, org, slots)
	}
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type servingAdmission struct {
	mu      sync.Mutex
	tenants map[tracker.OrganizationID]int
	total   int
}

func (a *servingAdmission) acquire(ctx context.Context, org tracker.OrganizationID, slots chan struct{}) (func(), error) {
	a.mu.Lock()
	if a.tenants[org] >= max(1, cap(slots)-1) || a.total >= 2*cap(slots) {
		a.mu.Unlock()
		return nil, corestore.ErrServingCapacity
	}
	if a.tenants == nil {
		a.tenants = make(map[tracker.OrganizationID]int)
	}
	a.tenants[org]++
	a.total++
	a.mu.Unlock()

	done := func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.total--
		a.tenants[org]--
		if a.tenants[org] == 0 {
			delete(a.tenants, org)
		}
	}
	select {
	case slots <- struct{}{}:
		return func() {
			<-slots
			done()
		}, nil
	case <-ctx.Done():
		done()
		return nil, ctx.Err()
	}
}
