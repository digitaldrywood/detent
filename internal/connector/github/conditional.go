package github

import (
	"net/http"
	"strings"
	"time"
)

const restConditionalCacheMaxEntries = 1024

type restCacheEntry struct {
	etag     string
	body     []byte
	headers  http.Header
	cachedAt time.Time
}

func (c *Client) restConditionalEntry(method string, path string) (restCacheEntry, bool) {
	if c == nil || !c.conditionalRequests || !strings.EqualFold(strings.TrimSpace(method), http.MethodGet) {
		return restCacheEntry{}, false
	}
	key := restCacheKey(method, path)
	c.mu.RLock()
	entry, ok := c.restCache[key]
	c.mu.RUnlock()
	if !ok || strings.TrimSpace(entry.etag) == "" {
		return restCacheEntry{}, false
	}
	return cloneRESTCacheEntry(entry), true
}

func (c *Client) storeRESTConditionalEntry(method string, path string, headers http.Header, body []byte) {
	if c == nil || !c.conditionalRequests || !strings.EqualFold(strings.TrimSpace(method), http.MethodGet) {
		return
	}
	etag := strings.TrimSpace(headers.Get("ETag"))
	if etag == "" {
		return
	}
	entry := restCacheEntry{
		etag:     etag,
		body:     append([]byte(nil), body...),
		headers:  headers.Clone(),
		cachedAt: time.Now(),
	}
	key := restCacheKey(method, path)
	c.mu.Lock()
	if _, exists := c.restCache[key]; !exists && len(c.restCache) >= restConditionalCacheMaxEntries {
		c.evictOldestRESTConditionalEntryLocked()
	}
	c.restCache[key] = entry
	c.mu.Unlock()
}

func (c *Client) deleteRESTConditionalEntriesForEndpoint(method string, path string) {
	if c == nil {
		return
	}
	methodPrefix := strings.ToUpper(strings.TrimSpace(method)) + " "
	endpointPath := strings.TrimSpace(path)
	if queryIndex := strings.IndexByte(endpointPath, '?'); queryIndex >= 0 {
		endpointPath = endpointPath[:queryIndex]
	}
	exactKey := methodPrefix + endpointPath
	queryPrefix := exactKey + "?"
	c.mu.Lock()
	for key := range c.restCache {
		if key == exactKey || strings.HasPrefix(key, queryPrefix) {
			delete(c.restCache, key)
		}
	}
	c.mu.Unlock()
}

func (c *Client) evictOldestRESTConditionalEntryLocked() {
	oldestKey := ""
	oldestAt := time.Time{}
	for key, entry := range c.restCache {
		if oldestKey == "" || entry.cachedAt.Before(oldestAt) {
			oldestKey = key
			oldestAt = entry.cachedAt
		}
	}
	if oldestKey != "" {
		delete(c.restCache, oldestKey)
	}
}

func restCacheKey(method string, path string) string {
	return strings.ToUpper(strings.TrimSpace(method)) + " " + strings.TrimSpace(path)
}

func cloneRESTCacheEntry(entry restCacheEntry) restCacheEntry {
	return restCacheEntry{
		etag:     entry.etag,
		body:     append([]byte(nil), entry.body...),
		headers:  entry.headers.Clone(),
		cachedAt: entry.cachedAt,
	}
}

func mergeRESTHeaders(cached http.Header, fresh http.Header) http.Header {
	merged := cached.Clone()
	for key, values := range fresh {
		merged[key] = append([]string(nil), values...)
	}
	return merged
}
