package hubmaintenance

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	TenantLimit = 32
	WorkerLimit = 2
	ItemLimit   = 64
	ByteLimit   = 16 << 30
	ProbeLimit  = 4
	MinItems    = 8
)

type Tenant struct {
	Organization string
	Generation   int64
}

type Budget struct {
	Items int
	Bytes int64
}

type Registry interface {
	Page(context.Context, string, int) ([]Tenant, error)
}

type Outcome struct {
	Progress bool
	Items    int
}

type Feature struct {
	Name  string
	Bytes int64
	Lead  bool
	Step  func(context.Context, Tenant, Budget) (Outcome, error)
}

type Scheduler struct {
	registry    Registry
	features    []Feature
	leads       []Feature
	leadCursors []string
	active      atomic.Bool
	running     atomic.Bool
	cursor      string
	family      int
	wait        func(context.Context, time.Duration) error
}

func New(registry Registry, features []Feature) (*Scheduler, error) {
	if registry == nil || len(features) == 0 || len(features) > 16 {
		return nil, errors.New("maintenance requires a registry and bounded feature families")
	}
	names := make(map[string]bool, len(features))
	for _, feature := range features {
		if feature.Name == "" || names[feature.Name] || feature.Step == nil || feature.Bytes <= 0 || feature.Bytes > ByteLimit {
			return nil, errors.New("invalid maintenance feature")
		}
		names[feature.Name] = true
	}
	scheduler := &Scheduler{registry: registry, wait: waitForWake}
	for _, feature := range features {
		if feature.Lead {
			scheduler.leads = append(scheduler.leads, feature)
		} else {
			scheduler.features = append(scheduler.features, feature)
		}
	}
	if len(scheduler.features) == 0 {
		return nil, errors.New("maintenance requires a rotating feature family")
	}
	scheduler.leadCursors = make([]string, len(scheduler.leads))
	return scheduler, nil
}

func waitForWake(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Scheduler) Run(ctx context.Context, report func(error)) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("maintenance scheduler already running")
	}
	defer s.running.Store(false)
	for {
		if err := s.wait(ctx, jitteredWake()); err != nil {
			return err
		}
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil && report != nil {
			report(err)
		}
	}
}

func jitteredWake() time.Duration {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return time.Second
	}
	return 900*time.Millisecond + time.Duration(binary.LittleEndian.Uint64(value[:])%uint64(200*time.Millisecond))
}

func (s *Scheduler) Tick(ctx context.Context) error {
	if !s.active.CompareAndSwap(false, true) {
		return errors.New("maintenance tick already running")
	}
	defer s.active.Store(false)
	envelope := tickEnvelope{bytes: ByteLimit, items: make(map[string]int)}
	leads := tickEnvelope{bytes: ByteLimit, items: envelope.items}
	var failure error
	for index, feature := range s.leads {
		if err := ctx.Err(); err != nil {
			break
		}
		cursor := s.leadCursors[index]
		result, err := s.serve(ctx, &leads, feature, cursor, ItemLimit/2)
		if err == nil && result.completed && result.served == 0 && cursor != "" {
			result, err = s.serve(ctx, &leads, feature, "", ItemLimit/2)
		}
		s.leadCursors[index] = result.cursor
		failure = errors.Join(failure, err)
	}
	for range ProbeLimit {
		if err := ctx.Err(); err != nil {
			break
		}
		result, err := s.serve(ctx, &envelope, s.features[s.family], s.cursor, ItemLimit)
		s.cursor = result.cursor
		if result.completed {
			s.family = (s.family + 1) % len(s.features)
		}
		failure = errors.Join(failure, err)
		if err != nil || result.progress || result.exhausted {
			break
		}
	}
	return errors.Join(failure, ctx.Err())
}

type tickEnvelope struct {
	mu    sync.Mutex
	bytes int64
	items map[string]int
}

type batchResult struct {
	progress  bool
	exhausted bool
	completed bool
	served    int
	cursor    string
}

func (s *Scheduler) serve(ctx context.Context, envelope *tickEnvelope, feature Feature, cursor string, itemLimit int) (batchResult, error) {
	result := batchResult{cursor: cursor}
	page, err := s.registry.Page(ctx, cursor, TenantLimit)
	if err != nil {
		return result, fmt.Errorf("page maintenance tenants: %w", err)
	}
	if len(page) > TenantLimit {
		return result, errors.New("maintenance registry exceeded page budget")
	}
	after := cursor
	for _, tenant := range page {
		if tenant.Organization <= after || len(tenant.Organization) > 128 || tenant.Generation <= 0 {
			return result, errors.New("invalid maintenance tenant page")
		}
		after = tenant.Organization
	}
	count := int(min(int64(len(page)), envelope.bytes/feature.Bytes))
	for index, tenant := range page[:count] {
		if itemLimit-envelope.items[tenant.Organization] < MinItems {
			count = index
			break
		}
	}
	result.exhausted = count < len(page)
	jobs := make(chan Tenant)
	failures := make(chan error, TenantLimit)
	var progress atomic.Bool
	var workers sync.WaitGroup
	for range min(WorkerLimit, count) {
		workers.Go(func() {
			for tenant := range jobs {
				if err := ctx.Err(); err != nil {
					failures <- err
					continue
				}
				envelope.mu.Lock()
				items := itemLimit - envelope.items[tenant.Organization]
				envelope.mu.Unlock()
				budget := Budget{Items: items, Bytes: feature.Bytes}
				allowance := &byteBudget{tenant: tenant, allowance: feature.Bytes, remaining: feature.Bytes}
				outcome, err := feature.Step(context.WithValue(ctx, budgetKey{}, allowance), tenant, budget)
				if err == nil && (outcome.Items < 0 || outcome.Items > budget.Items) {
					err = errors.New("maintenance step reported items outside its budget")
				}
				allowance.mu.Lock()
				consumed := allowance.allowance - allowance.remaining
				allowance.mu.Unlock()
				envelope.mu.Lock()
				envelope.items[tenant.Organization] += max(outcome.Items, 1)
				envelope.bytes -= consumed
				envelope.mu.Unlock()
				if err != nil {
					failures <- fmt.Errorf("maintenance tenant %s feature %s: %w", tenant.Organization, feature.Name, err)
					continue
				}
				if outcome.Progress {
					progress.Store(true)
				}
			}
		})
	}
	dispatched := 0
dispatch:
	for _, tenant := range page[:count] {
		select {
		case <-ctx.Done():
			break dispatch
		case jobs <- tenant:
			result.cursor = tenant.Organization
			dispatched++
		}
	}
	close(jobs)
	workers.Wait()
	close(failures)
	result.served = dispatched
	if dispatched == len(page) && len(page) < TenantLimit {
		result.cursor, result.completed = "", true
	}
	var failure error
	for err := range failures {
		failure = errors.Join(failure, err)
	}
	result.progress = progress.Load()
	return result, failure
}
