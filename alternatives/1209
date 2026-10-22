package github

import (
	"maps"
	"strings"
	"sync"
	"time"
)

const githubCacheTTL = 5 * time.Minute

type statusMetadata struct {
	FieldID         string
	OptionIDsByName map[string]string
}

type statusCache struct {
	mu      sync.RWMutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]statusCacheEntry
}

type statusCacheEntry struct {
	metadata statusMetadata
	cachedAt time.Time
}

func newStatusCache(ttl time.Duration, now func() time.Time) *statusCache {
	if now == nil {
		now = time.Now
	}

	return &statusCache{
		ttl:     ttl,
		now:     now,
		entries: map[string]statusCacheEntry{},
	}
}

func (c *statusCache) Get(projectID string) (statusMetadata, bool) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return statusMetadata{}, false
	}

	c.mu.RLock()
	entry, ok := c.entries[projectID]
	c.mu.RUnlock()
	if !ok {
		return statusMetadata{}, false
	}
	if c.fresh(entry.cachedAt) {
		return cloneStatusMetadata(entry.metadata), true
	}

	c.mu.Lock()
	if current, ok := c.entries[projectID]; ok && c.fresh(current.cachedAt) {
		entry = current
	} else if ok {
		delete(c.entries, projectID)
	}
	c.mu.Unlock()

	if c.fresh(entry.cachedAt) {
		return cloneStatusMetadata(entry.metadata), true
	}

	return statusMetadata{}, false
}

func (c *statusCache) Set(projectID string, metadata statusMetadata) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return
	}

	c.mu.Lock()
	c.entries[projectID] = statusCacheEntry{
		metadata: cloneStatusMetadata(metadata),
		cachedAt: c.now(),
	}
	c.mu.Unlock()
}

func (c *statusCache) Clear(projectID string) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return
	}

	c.mu.Lock()
	delete(c.entries, projectID)
	c.mu.Unlock()
}

func (c *statusCache) fresh(cachedAt time.Time) bool {
	return c.ttl > 0 && c.now().Sub(cachedAt) < c.ttl
}

func cloneStatusMetadata(metadata statusMetadata) statusMetadata {
	return statusMetadata{
		FieldID:         metadata.FieldID,
		OptionIDsByName: cloneStringMap(metadata.OptionIDsByName),
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}

	cloned := make(map[string]string, len(values))
	maps.Copy(cloned, values)
	return cloned
}
