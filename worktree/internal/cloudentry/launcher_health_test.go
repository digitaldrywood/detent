package cloudentry

import (
	"testing"
	"time"
)

func TestExecLauncherHealth(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name         string
		entry        *tenantProcessHealth
		wantUptime   int64
		wantRestarts int
	}{
		{name: "unobserved", wantUptime: -1, wantRestarts: -1},
		{name: "first start", entry: &tenantProcessHealth{startedAt: now.Add(-time.Hour), observed: true}, wantUptime: 3600},
		{name: "down", entry: &tenantProcessHealth{observed: true, starts: []time.Time{now.Add(-time.Minute)}}, wantUptime: -1, wantRestarts: 1},
		{name: "rolling window", entry: &tenantProcessHealth{startedAt: now.Add(-time.Minute), observed: true, starts: []time.Time{now.Add(-25 * time.Hour), now.Add(-24 * time.Hour), now.Add(-time.Minute)}}, wantUptime: 60, wantRestarts: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			launcher := &ExecLauncher{health: map[string]*tenantProcessHealth{"org_one": test.entry}}
			uptime, restarts := launcher.Health("org_one", now)
			if (uptime == nil) != (test.wantUptime < 0) || uptime != nil && *uptime != test.wantUptime ||
				(restarts == nil) != (test.wantRestarts < 0) || restarts != nil && *restarts != test.wantRestarts {
				t.Fatalf("health = %v, %v; want %d, %d", uptime, restarts, test.wantUptime, test.wantRestarts)
			}
		})
	}
}
