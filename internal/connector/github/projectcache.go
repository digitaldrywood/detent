package github

import (
	"strings"
	"sync"
	"time"
)

type projectCache struct {
	mu        sync.RWMutex
	ttl       time.Duration
	now       func() time.Time
	entries   map[string]map[string]projectItemCacheEntry
	scanned   map[string]time.Time
	revisions map[string]uint64
	refs      map[string]issueRefCacheEntry
}

type projectItemCacheEntry struct {
	projectItemFields
	fieldsKnown bool
	cachedAt    time.Time
}

type projectItemFields struct {
	itemID          string
	statusName      string
	priorityName    string
	statusUpdatedAt *time.Time
	fields          map[string]string
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
		ttl:       ttl,
		now:       now,
		entries:   map[string]map[string]projectItemCacheEntry{},
		scanned:   map[string]time.Time{},
		revisions: map[string]uint64{},
		refs:      map[string]issueRefCacheEntry{},
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
	if !ok || entry.itemID == "" {
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
	if entry, ok := projectEntries[issueID]; ok && entry.itemID == itemID && c.fresh(entry.cachedAt) {
		c.mu.Unlock()
		return
	}
	projectEntries[issueID] = projectItemCacheEntry{
		projectItemFields: projectItemFields{itemID: itemID},
		cachedAt:          c.now(),
	}
	c.revisions[projectID]++
	delete(c.scanned, projectID)
	c.mu.Unlock()
}

func (c *projectCache) GetProjectFields(projectID string, issueID string) (projectItemFields, bool, bool) {
	projectID = strings.TrimSpace(projectID)
	issueID = strings.TrimSpace(issueID)
	if projectID == "" || issueID == "" {
		return projectItemFields{}, false, false
	}

	c.mu.RLock()
	entry, found := c.entries[projectID][issueID]
	scannedAt, scanned := c.scanned[projectID]
	c.mu.RUnlock()

	if found && c.fresh(entry.cachedAt) {
		if !entry.fieldsKnown {
			return projectItemFields{}, false, false
		}
		return cloneProjectItemFields(entry.projectItemFields), true, true
	}
	if scanned && c.fresh(scannedAt) {
		return projectItemFields{}, false, true
	}
	return projectItemFields{}, false, false
}

func (c *projectCache) SetProjectFields(projectID string, issueID string, fields projectItemFields) {
	projectID = strings.TrimSpace(projectID)
	issueID = strings.TrimSpace(issueID)
	fields.itemID = strings.TrimSpace(fields.itemID)
	if projectID == "" || issueID == "" || fields.itemID == "" {
		return
	}

	c.mu.Lock()
	projectEntries := c.entries[projectID]
	if projectEntries == nil {
		projectEntries = map[string]projectItemCacheEntry{}
		c.entries[projectID] = projectEntries
	}
	c.revisions[projectID]++
	projectEntries[issueID] = projectItemCacheEntry{
		projectItemFields: cloneProjectItemFields(fields),
		fieldsKnown:       true,
		cachedAt:          c.now(),
	}
	c.mu.Unlock()
}

func (c *projectCache) ReplaceProjectFields(projectID string, fieldsByIssue map[string]projectItemFields, revision uint64) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return
	}

	cachedAt := c.now()
	next := make(map[string]projectItemCacheEntry, len(fieldsByIssue))
	for issueID, fields := range fieldsByIssue {
		issueID = strings.TrimSpace(issueID)
		fields.itemID = strings.TrimSpace(fields.itemID)
		if issueID == "" || fields.itemID == "" {
			continue
		}
		next[issueID] = projectItemCacheEntry{
			projectItemFields: cloneProjectItemFields(fields),
			fieldsKnown:       true,
			cachedAt:          cachedAt,
		}
	}

	c.mu.Lock()
	if c.revisions[projectID] != revision {
		c.mu.Unlock()
		return
	}
	c.entries[projectID] = next
	c.scanned[projectID] = cachedAt
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
	c.revisions[projectID]++
	delete(c.scanned, projectID)
	c.mu.Unlock()
}

func (c *projectCache) ClearProject(projectID string) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return
	}

	c.mu.Lock()
	delete(c.entries, projectID)
	c.revisions[projectID]++
	delete(c.scanned, projectID)
	c.mu.Unlock()
}

func (c *projectCache) InvalidateProjectFields(projectID, issueID string) {
	projectID = strings.TrimSpace(projectID)
	issueID = strings.TrimSpace(issueID)
	if projectID == "" || issueID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revisions[projectID]++
	if c.entries[projectID] == nil {
		c.entries[projectID] = map[string]projectItemCacheEntry{}
	}
	entry := c.entries[projectID][issueID]
	entry.fieldsKnown = false
	entry.cachedAt = c.now()
	c.entries[projectID][issueID] = entry
}

func (c *projectCache) ProjectFieldsScanned(projectID string) bool {
	c.mu.RLock()
	scannedAt, ok := c.scanned[projectID]
	c.mu.RUnlock()
	return ok && c.fresh(scannedAt)
}

func (c *projectCache) Revision(projectID string) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.revisions[projectID]
}

func (c *projectCache) fresh(cachedAt time.Time) bool {
	return c.ttl > 0 && c.now().Sub(cachedAt) < c.ttl
}

func cloneProjectItemFields(fields projectItemFields) projectItemFields {
	cloned := fields
	if fields.statusUpdatedAt != nil {
		updatedAt := *fields.statusUpdatedAt
		cloned.statusUpdatedAt = &updatedAt
	}
	if fields.fields != nil {
		cloned.fields = make(map[string]string, len(fields.fields))
		for name, value := range fields.fields {
			cloned.fields[name] = value
		}
	}
	return cloned
}
