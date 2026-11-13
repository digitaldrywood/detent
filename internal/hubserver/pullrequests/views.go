package pullrequests

import (
	"github.com/digitaldrywood/detent/internal/tracker"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// ViewTTL is how long an assembled view is served from memory
	// before the join is run again (the pull request contract, "caches the connector view
	// for 60 seconds").
	ViewTTL = 60 * time.Second
	// RefreshInterval is the minimum spacing between forced
	// refreshes, per organization.
	RefreshInterval = 10 * time.Second
)

// Mergeable is the pull request contract's three-valued mergeability: true, false, or
// the string "unknown" when the connector has not said.
type Mergeable = tracker.PullRequestMergeable

// MergeableFromState maps GitHub's mergeable_state onto the pull request contract's three values.
// An empty or "unknown" state is unknown; "dirty" is the only state that means
// the branch cannot merge; everything else is mergeable as far as the
// projection can tell.
func MergeableFromState(state string) Mergeable {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", "unknown":
		return Mergeable{}
	case "dirty":
		return Mergeable{Known: true}
	default:
		return Mergeable{Known: true, Value: true}
	}
}

type Ref = tracker.PullRequestRef
type Check = tracker.PullRequestCheck
type Review = tracker.PullRequestReview
type Connector = tracker.PullRequestConnector
type View = tracker.PullRequestView

// Cache serves an assembled view for ViewTTL and paces
// forced refreshes per organization. Both are in-process: a restart loses a
// cached view, which costs one extra join, and loses a refresh budget, which
// costs at most one extra hydration request.
type Cache struct {
	mu        sync.Mutex
	views     map[string]cacheEntry
	refreshed map[tracker.OrganizationID]time.Time
}

type cacheEntry struct {
	views    []View
	cachedAt time.Time
}

func NewCache() *Cache {
	return &Cache{views: map[string]cacheEntry{}, refreshed: map[tracker.OrganizationID]time.Time{}}
}

func CacheKey(organization tracker.OrganizationID, project tracker.ProjectID, item string) string {
	return string(organization) + "/" + string(project) + "/" + item
}

// get returns a cached view that is still fresh. The slice is cloned so a
// caller cannot rewrite what the next reader sees.
func (c *Cache) Get(key string, now time.Time) ([]View, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.views[key]
	if !ok || now.Sub(entry.cachedAt) >= ViewTTL {
		return nil, false
	}
	return append([]View(nil), entry.views...), true
}

func (c *Cache) Invalidate(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.views, key)
}

func (c *Cache) Set(key string, views []View, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// The cache is bounded by dropping everything once it grows past a page
	// of work items: the entries are cheap to rebuild and a hub that is
	// browsing thousands of issues should not hold them all.
	if len(c.views) >= 512 {
		clear(c.views)
	}
	c.views[key] = cacheEntry{views: append([]View(nil), views...), cachedAt: now}
}

// allowRefresh reports whether the organization may force a refresh now, and
// records the grant when it may.
func (c *Cache) AllowRefresh(organization tracker.OrganizationID, now time.Time) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if last, ok := c.refreshed[organization]; ok && now.Sub(last) < RefreshInterval {
		return false
	}
	c.refreshed[organization] = now
	return true
}

// ExternalNumber reads the pull request number a change's GitHub
// external reference names, or zero when it names none.
func ExternalNumber(reference tracker.ChangeExternalReference) int {
	if !strings.EqualFold(strings.TrimSpace(reference.Provider), "github") {
		return 0
	}
	number, err := strconv.Atoi(strings.TrimSpace(reference.ID))
	if err != nil || number <= 0 {
		return 0
	}
	return number
}

// ConnectorState normalizes GitHub's state onto the pull request contract's vocabulary.
func ConnectorState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "merged":
		return "merged"
	case "closed":
		return "closed"
	default:
		return "open"
	}
}
