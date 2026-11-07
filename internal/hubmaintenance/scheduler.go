package hubmaintenance

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const (
	TenantLimit = 32
	WorkerLimit = 2
	ItemLimit   = 64
	ByteLimit   = 16 << 30
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

type Feature struct {
	Name  string
	Bytes int64
	Step  func(context.Context, Tenant, Budget) error
}

type Scheduler struct {
	registry Registry
	features []Feature
	active   atomic.Bool
	running  atomic.Bool
	cursor   string
	family   int
	wait     func(context.Context, time.Duration) error
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
	return &Scheduler{registry: registry, features: slices.Clone(features), wait: waitForWake}, nil
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
	if err := ctx.Err(); err != nil {
		return err
	}
	page, err := s.registry.Page(ctx, s.cursor, TenantLimit)
	if err != nil {
		return fmt.Errorf("page maintenance tenants: %w", err)
	}
	if len(page) > TenantLimit {
		return errors.New("maintenance registry exceeded page budget")
	}
	after := s.cursor
	for _, tenant := range page {
		if tenant.Organization <= after || len(tenant.Organization) > 128 || tenant.Generation <= 0 {
			return errors.New("invalid maintenance tenant page")
		}
		after = tenant.Organization
	}
	feature := s.features[s.family]
	count := int(min(int64(len(page)), ByteLimit/feature.Bytes))
	jobs := make(chan Tenant)
	failures := make(chan error, TenantLimit)
	var workers sync.WaitGroup
	for range min(WorkerLimit, count) {
		workers.Go(func() {
			for tenant := range jobs {
				if err := ctx.Err(); err != nil {
					failures <- err
					continue
				}
				budget := Budget{Items: ItemLimit, Bytes: feature.Bytes}
				if err := feature.Step(budget.Context(ctx, tenant), tenant, budget); err != nil {
					failures <- fmt.Errorf("maintenance tenant %s feature %s: %w", tenant.Organization, feature.Name, err)
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
			s.cursor = tenant.Organization
			dispatched++
		}
	}
	close(jobs)
	workers.Wait()
	close(failures)
	if dispatched == len(page) && len(page) < TenantLimit {
		s.cursor = ""
		s.family = (s.family + 1) % len(s.features)
	}
	var failure error
	for err := range failures {
		failure = errors.Join(failure, err)
	}
	return errors.Join(failure, ctx.Err())
}
