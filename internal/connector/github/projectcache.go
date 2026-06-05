package github

import (
	"strings"
	"sync"
	"time"
)

type projectCache struct {
	mu      sync.RWMutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]map[string]projectItemCacheEntry
	refs    map[string]issueRefCacheEntry
}

type projectItemCacheEntry struct {
	itemID   string
	cachedAt time.Time
}

type issueRefCacheEntry struct {
	ref      issueRef
	cachedAt time.Time
}

type issueRef struct {
	Owner  string
	Name   string
	Number int
}

func newProjectCache(ttl time.Duration, now func() time.Time) *projectCache {
	if now == nil {
		now = time.Now
	}

	return &projectCache{
		ttl:     ttl,
		now:     now,
		entries: map[string]map[string]projectItemCacheEntry{},
		refs:    map[string]issueRefCacheEntry{},
	}
}

func (c *projectCache) GetItemID(projectID string, issueID string) (string, bool) {
	projectID = strings.TrimSpace(projectID)
	issueID = strings.TrimSpace(issueID)
	if projectID == "" || issueID == "" {
		return "", false
	}

	c.mu.RLock()
	projectEntries, ok := c.entries[projectID]
	if !ok {
		c.mu.RUnlock()
		return "", false
	}
	entry, ok := projectEntries[issueID]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	if c.fresh(entry.cachedAt) {
		return entry.itemID, true
	}

	c.mu.Lock()
	projectEntries = c.entries[projectID]
	if current, ok := projectEntries[issueID]; ok && c.fresh(current.cachedAt) {
		entry = current
	} else if ok {
		delete(projectEntries, issueID)
		if len(projectEntries) == 0 {
			delete(c.entries, projectID)
		}
	}
	c.mu.Unlock()

	if c.fresh(entry.cachedAt) {
		return entry.itemID, true
	}

	return "", false
}

func (c *projectCache) SetItemID(projectID string, issueID string, itemID string) {
	projectID = strings.TrimSpace(projectID)
	issueID = strings.TrimSpace(issueID)
	itemID = strings.TrimSpace(itemID)
	if projectID == "" || issueID == "" || itemID == "" {
		return
	}

	c.mu.Lock()
	projectEntries := c.entries[projectID]
	if projectEntries == nil {
		projectEntries = map[string]projectItemCacheEntry{}
		c.entries[projectID] = projectEntries
	}
	projectEntries[issueID] = projectItemCacheEntry{
		itemID:   itemID,
		cachedAt: c.now(),
	}
	c.mu.Unlock()
}

func (c *projectCache) GetIssueRef(issueID string) (issueRef, bool) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return issueRef{}, false
	}

	c.mu.RLock()
	entry, ok := c.refs[issueID]
	c.mu.RUnlock()
	if !ok {
		return issueRef{}, false
	}
	if c.fresh(entry.cachedAt) {
		return entry.ref, true
	}

	c.mu.Lock()
	if current, ok := c.refs[issueID]; ok && c.fresh(current.cachedAt) {
		entry = current
	} else if ok {
		delete(c.refs, issueID)
	}
	c.mu.Unlock()

	if c.fresh(entry.cachedAt) {
		return entry.ref, true
	}
	return issueRef{}, false
}

func (c *projectCache) SetIssueRef(issueID string, ref issueRef) {
	issueID = strings.TrimSpace(issueID)
	ref.Owner = strings.TrimSpace(ref.Owner)
	ref.Name = strings.TrimSpace(ref.Name)
	if issueID == "" || ref.Owner == "" || ref.Name == "" || ref.Number <= 0 {
		return
	}

	c.mu.Lock()
	c.refs[issueID] = issueRefCacheEntry{
		ref:      ref,
		cachedAt: c.now(),
	}
	c.mu.Unlock()
}

func (c *projectCache) ClearItemID(projectID string, issueID string) {
	projectID = strings.TrimSpace(projectID)
	issueID = strings.TrimSpace(issueID)
	if projectID == "" || issueID == "" {
		return
	}

	c.mu.Lock()
	if projectEntries := c.entries[projectID]; projectEntries != nil {
		delete(projectEntries, issueID)
		if len(projectEntries) == 0 {
			delete(c.entries, projectID)
		}
	}
	c.mu.Unlock()
}

func (c *projectCache) ClearProject(projectID string) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return
	}

	c.mu.Lock()
	delete(c.entries, projectID)
	c.mu.Unlock()
}

func (c *projectCache) fresh(cachedAt time.Time) bool {
	return c.ttl > 0 && c.now().Sub(cachedAt) < c.ttl
}
