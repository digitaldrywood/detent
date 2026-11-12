package hubserver

import (
	"slices"
	"sync"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

type activityDurationSample struct {
	ID      string
	Status  string
	Role    string
	Seconds float64
	Partial bool
}

type activityDurationEntry struct {
	project string
	samples []activityDurationSample
	typical []operatortool.ActivityTypicalDuration
	partial bool
}

type activityDurationCache struct {
	mu      sync.Mutex
	entries []activityDurationEntry
}

func (c *activityDurationCache) typical(project string, samples []activityDurationSample, partial bool) []operatortool.ActivityTypicalDuration {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.entries {
		if entry.project == project && entry.partial == partial && slices.Equal(entry.samples, samples) {
			return slices.Clone(entry.typical)
		}
	}
	typical := activityTypicalDurations(project, samples, partial)
	if len(c.entries) == 64 {
		c.entries = c.entries[1:]
	}
	c.entries = append(c.entries, activityDurationEntry{project: project, samples: samples, typical: typical, partial: partial})
	return slices.Clone(typical)
}
