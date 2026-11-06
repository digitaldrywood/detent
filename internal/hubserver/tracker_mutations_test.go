package hubserver

import (
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func seedHubMachine(t *testing.T, service *Service, machineID tracker.MachineID, now time.Time) {
	t.Helper()
	if _, err := service.database.db.ExecContext(t.Context(), `
INSERT INTO machines (id, hostname, display_name, capacity, version, last_heartbeat_at, registered_at, updated_at)
VALUES (?, ?, ?, 1, 'test', ?, ?, ?)`, machineID, machineID, machineID, formatHubTime(now), formatHubTime(now), formatHubTime(now)); err != nil {
		t.Fatalf("insert machine %s: %v", machineID, err)
	}
}

func runQueuedHubMutation(t *testing.T, service *Service, afterQueued func(), mutate func() error) error {
	t.Helper()
	blocker, err := service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin blocking transaction: %v", err)
	}
	defer func() { _ = blocker.Rollback() }()

	waitsBefore := service.database.db.Stats().WaitCount
	finished := make(chan error, 1)
	go func() {
		finished <- mutate()
	}()

	waitContext := testenv.WaitContext(t, t.Context())
	for service.database.db.Stats().WaitCount == waitsBefore {
		select {
		case mutationErr := <-finished:
			t.Fatalf("queued mutation completed before the blocking transaction was released: %v", mutationErr)
			return mutationErr
		case <-waitContext.Done():
			t.Fatal("queued mutation did not wait for the blocking transaction")
			return nil
		default:
			runtime.Gosched()
		}
	}

	afterQueued()
	if err := blocker.Rollback(); err != nil {
		t.Fatalf("release blocking transaction: %v", err)
	}
	return <-finished
}

type leaseTestClock struct {
	mu    sync.Mutex
	value time.Time
}

func (c *leaseTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

func (c *leaseTestClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = c.value.Add(duration)
}
