package scheduleowner

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/coordination"
)

func TestManagersContendForOneProjectLease(t *testing.T) {
	t.Parallel()
	clock := newTestClock(time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
	store := newMemoryCoordinationStore(clock.Now)
	config := testConfig()
	alpha := newTestManager(t, config, "alpha", "alpha-token", store, clock)
	beta := newTestManager(t, config, "beta", "beta-token", store, clock)

	start := make(chan struct{})
	results := make(chan acquireResult, 2)
	for _, manager := range []*Manager{alpha, beta} {
		manager := manager
		go func() {
			<-start
			lease, acquired, err := manager.Acquire(t.Context())
			results <- acquireResult{lease: lease, acquired: acquired, err: err}
		}()
	}
	close(start)
	acquired := 0
	owner := ""
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("Acquire() error = %v", result.err)
		}
		if result.acquired {
			acquired++
			owner = result.lease.Owner
		}
	}
	if acquired != 1 || (owner != "alpha" && owner != "beta") {
		t.Fatalf("acquired = %d, owner = %q, want one owner", acquired, owner)
	}
}

func TestOwnerDeathAllowsOneSuccessorAfterExpiry(t *testing.T) {
	t.Parallel()
	clock := newTestClock(time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
	store := newMemoryCoordinationStore(clock.Now)
	config := testConfig()
	alpha := newTestManager(t, config, "alpha", "alpha-token", store, clock)
	beta := newTestManager(t, config, "beta", "beta-token", store, clock)

	first, acquired, err := alpha.Acquire(t.Context())
	if err != nil || !acquired {
		t.Fatalf("alpha Acquire() = %#v, %t, %v", first, acquired, err)
	}
	if _, acquired, err := beta.Acquire(t.Context()); err != nil || acquired {
		t.Fatalf("beta early Acquire() = %t, %v", acquired, err)
	}
	clock.Advance(config.LeaseTTL() + config.MaxClockSkew() + time.Second)
	second, acquired, err := beta.Acquire(t.Context())
	if err != nil || !acquired {
		t.Fatalf("beta failover Acquire() = %#v, %t, %v", second, acquired, err)
	}
	if second.Owner != "beta" || second.Generation != first.Generation+1 {
		t.Fatalf("successor lease = %#v, first = %#v", second, first)
	}
	if _, acquired, err := alpha.Acquire(t.Context()); err != nil || acquired {
		t.Fatalf("alpha post-failover Acquire() = %t, %v", acquired, err)
	}
}

func TestRunStopsWorkBeforeReleasingLease(t *testing.T) {
	t.Parallel()
	clock := newTestClock(time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
	store := newMemoryCoordinationStore(clock.Now)
	manager := newTestManager(t, testConfig(), "alpha", "alpha-token", store, clock)
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	stopped := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- manager.Run(ctx, func(workCtx context.Context) error {
			close(started)
			<-workCtx.Done()
			close(stopped)
			return workCtx.Err()
		})
	}()
	<-started
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("owned work was not stopped before Run returned")
	}
	status, err := manager.Current(t.Context())
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if status.Active || status.Owner != "" {
		t.Fatalf("Current() = %#v, want released", status)
	}
}

type acquireResult struct {
	lease    Lease
	acquired bool
	err      error
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(now time.Time) *testClock {
	return &testClock{now: now}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

type memoryCoordinationStore struct {
	mu      sync.Mutex
	now     func() time.Time
	version int
	records map[string]coordination.Record
}

func newMemoryCoordinationStore(now func() time.Time) *memoryCoordinationStore {
	return &memoryCoordinationStore{now: now, records: map[string]coordination.Record{}}
}

func (s *memoryCoordinationStore) Get(_ context.Context, key string) (coordination.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, found := s.records[key]
	record.Value = append([]byte(nil), record.Value...)
	return record, found, nil
}

func (s *memoryCoordinationStore) CompareAndSwap(
	_ context.Context,
	key string,
	expectedVersion string,
	value []byte,
) (coordination.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.records[key]
	if current.Version != expectedVersion {
		return coordination.Record{}, false, nil
	}
	s.version++
	record := coordination.Record{
		Value:      append([]byte(nil), value...),
		Version:    fmt.Sprintf("v%d", s.version),
		ModifiedAt: s.now().UTC(),
	}
	s.records[key] = record
	return record, true, nil
}

func testConfig() Config {
	return Config{
		Enabled:             true,
		Backend:             BackendGitHubRef,
		Key:                 "example/production",
		Repository:          "example/coordination",
		Branch:              "scheduler",
		LeaseSeconds:        10,
		HeartbeatSeconds:    2,
		RetrySeconds:        1,
		MaxClockSkewSeconds: 1,
	}
}

func newTestManager(
	t *testing.T,
	config Config,
	owner string,
	token string,
	store coordination.Store,
	clock *testClock,
) *Manager {
	t.Helper()
	manager, err := New(config, owner, store, Dependencies{
		Now:   clock.Now,
		Token: func() (string, error) { return token, nil },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return manager
}
