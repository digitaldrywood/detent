package hubserver

import (
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func seedHubMachine(t *testing.T, service *Service, organization tracker.OrganizationID, machineID tracker.MachineID) {
	t.Helper()
	if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: string(machineID), Hostname: string(machineID), DisplayName: string(machineID), Capacity: 1, Version: "test"})
	}); err != nil {
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
