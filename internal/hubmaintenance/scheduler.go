package hubmaintenance

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
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
	Name     string
	Bytes    int64
	Lead     bool
	Continue func() bool
	Due      func(context.Context, int) ([]Tenant, error)
	Step     func(context.Context, Tenant, Budget) (Outcome, error)
}

func (f Feature) runStep(ctx context.Context, t Tenant, b Budget) (out Outcome, err error) {
	defer func() {
		if recover() != nil {
			var callers [8]uintptr
			frames := runtime.CallersFrames(callers[:runtime.Callers(3, callers[:])])
			frame, _ := frames.Next()
			err = fmt.Errorf("maintenance step panicked at %s:%d", frame.Function, frame.Line)
		}
	}()
	return f.Step(ctx, t, b)
}

type Scheduler struct {
	registry        Registry
	features        []Feature
	leads           []Feature
	leadCursors     []string
	active          atomic.Bool
	running         atomic.Bool
	familyCursors   []string
	interleavePages bool
	reserveRotation bool
	family          int
	wait            func(context.Context, time.Duration) error
	dispatchWindow  func(context.Context) (context.Context, context.CancelFunc)
}

func New(registry Registry, features []Feature) (*Scheduler, error) {
	if registry == nil || len(features) == 0 || len(features) > 64 {
		return nil, errors.New("maintenance requires a registry and bounded feature families")
	}
	names := make(map[string]bool, len(features))
	for _, feature := range features {
		if feature.Name == "" || names[feature.Name] || feature.Step == nil || feature.Bytes <= 0 || feature.Bytes > ByteLimit {
			return nil, errors.New("invalid maintenance feature")
		}
		names[feature.Name] = true
	}
	scheduler := &Scheduler{registry: registry, wait: waitForWake, dispatchWindow: func(ctx context.Context) (context.Context, context.CancelFunc) {
		return context.WithTimeout(ctx, time.Second)
	}}
	for _, feature := range features {
		if feature.Lead {
			scheduler.leads = append(scheduler.leads, feature)
		} else {
			scheduler.features = append(scheduler.features, feature)
		}
	}
	if len(scheduler.leads) > ItemLimit/(2*MinItems) {
		return nil, errors.New("maintenance lead families exceed reserved items")
	}
	if len(scheduler.features) == 0 {
		return nil, errors.New("maintenance requires a rotating feature family")
	}
	scheduler.leadCursors = make([]string, len(scheduler.leads))
	scheduler.familyCursors = make([]string, len(scheduler.features))
	return scheduler, nil
}

func NewShared(registry Registry, features []Feature) (*Scheduler, error) {
	scheduler, err := New(registry, features)
	if err != nil {
		return nil, err
	}
	scheduler.interleavePages = true
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
	continued := 0
	for {
		if continued == 0 {
			if err := s.wait(ctx, jitteredWake()); err != nil {
				return err
			}
		}
		more, err := s.tick(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && report != nil {
			report(err)
		}
		continued++
		if !more || err != nil || continued >= TenantLimit {
			continued = 0
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
	_, err := s.tick(ctx)
	return err
}

func (s *Scheduler) tick(ctx context.Context) (bool, error) {
	if !s.active.CompareAndSwap(false, true) {
		return false, errors.New("maintenance tick already running")
	}
	defer s.active.Store(false)
	envelope := tickEnvelope{bytes: ByteLimit, items: make(map[string]int)}
	if s.reserveRotation {
		s.reserveRotation = false
		err := s.rotate(ctx, &envelope, ItemLimit/2, 1)
		more, leadErr := s.serveLeads(ctx, &envelope, ItemLimit/2)
		return more, errors.Join(err, leadErr, ctx.Err())
	}
	more, err := s.serveLeads(ctx, &envelope, 0)
	rotationErr := s.rotate(ctx, &envelope, ItemLimit, ProbeLimit)
	return more, errors.Join(err, rotationErr, ctx.Err())
}

func (s *Scheduler) serveLeads(ctx context.Context, envelope *tickEnvelope, priorItems int) (bool, error) {
	var failure error
	moreDue := false
	for index, feature := range s.leads {
		if ctx.Err() != nil {
			break
		}
		cursor := s.leadCursors[index]
		limit := priorItems + (index+1)*ItemLimit/(2*len(s.leads))
		result, err := s.serve(ctx, envelope, feature, cursor, limit)
		if err == nil && result.completed && result.served == 0 && cursor != "" {
			result, err = s.serve(ctx, envelope, feature, "", limit)
		}
		if feature.Due == nil {
			s.leadCursors[index] = result.cursor
		} else if err == nil && (result.served == TenantLimit && result.progress || feature.Continue != nil && feature.Continue()) {
			moreDue = true
		}
		failure = errors.Join(failure, err)
	}
	return moreDue, failure
}

func (s *Scheduler) rotate(ctx context.Context, envelope *tickEnvelope, itemLimit, probes int) error {
	for range probes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		result, err := s.serve(ctx, envelope, s.features[s.family], s.familyCursors[s.family], itemLimit)
		if result.byteBlocked {
			s.reserveRotation = len(s.leads) > 0
			break
		}
		s.familyCursors[s.family] = result.cursor
		if result.completed || s.interleavePages {
			s.family = (s.family + 1) % len(s.features)
		}
		if err != nil || result.progress || result.exhausted {
			return err
		}
	}
	return nil
}

type tickEnvelope struct {
	mu    sync.Mutex
	bytes int64
	items map[string]int
}

type dueBudgetKey struct{}

func ReserveDueBytes(ctx context.Context, bytes int64) error {
	envelope, ok := ctx.Value(dueBudgetKey{}).(*tickEnvelope)
	if !ok {
		return errors.New("maintenance requires a due-work byte budget")
	}
	envelope.mu.Lock()
	defer envelope.mu.Unlock()
	if bytes <= 0 || bytes > envelope.bytes {
		return ErrByteBudget
	}
	envelope.bytes -= bytes
	return nil
}

type batchResult struct {
	byteBlocked bool
	progress    bool
	exhausted   bool
	completed   bool
	served      int
	cursor      string
}

func (s *Scheduler) serve(ctx context.Context, envelope *tickEnvelope, feature Feature, cursor string, itemLimit int) (batchResult, error) {
	ctx, cancelBatch := context.WithTimeout(ctx, 30*time.Second)
	defer cancelBatch()
	result := batchResult{cursor: cursor}
	var page []Tenant
	var err error
	if feature.Due != nil {
		page, err = feature.Due(context.WithValue(ctx, dueBudgetKey{}, envelope), TenantLimit)
		if errors.Is(err, ErrByteBudget) {
			result.byteBlocked = true
			return result, nil
		}
	} else {
		page, err = s.registry.Page(ctx, cursor, TenantLimit)
	}
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
	result.byteBlocked = len(page) > 0 && count == 0
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
				outcome, err := feature.runStep(context.WithValue(ctx, budgetKey{}, allowance), tenant, budget)
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
	dispatchCtx, stopDispatch := s.dispatchWindow(ctx)
dispatch:
	for _, tenant := range page[:count] {
		if dispatchCtx.Err() != nil {
			break
		}
		select {
		case <-dispatchCtx.Done():
			break dispatch
		case jobs <- tenant:
			result.cursor = tenant.Organization
			dispatched++
		}
	}
	close(jobs)
	workers.Wait()
	close(failures)
	result.exhausted = dispatched < len(page) || dispatchCtx.Err() != nil
	stopDispatch()
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
