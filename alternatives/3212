package hubserver

import (
	"sync"
	"testing"
	"time"

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
