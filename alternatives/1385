package hubclient

import (
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const maxCachedNativeItems = 4096

var nativeItemIDPattern = regexp.MustCompile(`wi_[0-9a-zA-Z]+`)

type nativeItemVersion struct {
	activity time.Time
	revision tracker.Revision
}

type cachedBlockerContext struct {
	version  nativeItemVersion
	attempts []tracker.NativeAttempt
	history  []tracker.CollaborationEvent
}

type cachedChanges struct {
	version nativeItemVersion
	changes []tracker.ChangeRequest
}

type cachedComments struct {
	version  nativeItemVersion
	comments []connector.IssueComment
}

// nativeReadCache keeps per-item evidence that only changes when the item's
// revision or last activity moves, so refreshes reread it only then.
type nativeReadCache struct {
	observed map[tracker.NativeWorkItemID]nativeItemVersion
	blockers map[tracker.NativeWorkItemID]cachedBlockerContext
	changes  map[tracker.NativeWorkItemID]cachedChanges
	comments map[tracker.NativeWorkItemID]cachedComments
	mu       sync.Mutex
}

func newNativeReadCache() *nativeReadCache {
	return &nativeReadCache{
		observed: map[tracker.NativeWorkItemID]nativeItemVersion{},
		blockers: map[tracker.NativeWorkItemID]cachedBlockerContext{},
		changes:  map[tracker.NativeWorkItemID]cachedChanges{},
		comments: map[tracker.NativeWorkItemID]cachedComments{},
	}
}

func versionOf(native tracker.NativeIssue) (nativeItemVersion, bool) {
	if native.LastActivityAt.IsZero() {
		return nativeItemVersion{}, false
	}
	return nativeItemVersion{revision: native.Revision, activity: native.LastActivityAt.UTC()}, true
}

func (c *nativeReadCache) observe(native tracker.NativeIssue) (nativeItemVersion, bool) {
	version, ok := versionOf(native)
	if c == nil || !ok {
		return version, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.observed) >= maxCachedNativeItems {
		clear(c.observed)
		clear(c.blockers)
		clear(c.changes)
		clear(c.comments)
	}
	c.observed[native.WorkItemID] = version
	return version, true
}

func (c *nativeReadCache) blockerContext(id tracker.NativeWorkItemID, version nativeItemVersion) ([]tracker.NativeAttempt, []tracker.CollaborationEvent, bool) {
	if c == nil {
		return nil, nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.blockers[id]
	if !ok || cached.version != version {
		return nil, nil, false
	}
	return slices.Clone(cached.attempts), slices.Clone(cached.history), true
}

func (c *nativeReadCache) storeBlockerContext(id tracker.NativeWorkItemID, version nativeItemVersion, attempts []tracker.NativeAttempt, history []tracker.CollaborationEvent) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blockers[id] = cachedBlockerContext{version: version, attempts: slices.Clone(attempts), history: slices.Clone(history)}
}

func (c *nativeReadCache) itemChanges(id tracker.NativeWorkItemID, version nativeItemVersion) ([]tracker.ChangeRequest, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.changes[id]
	if !ok || cached.version != version {
		return nil, false
	}
	return slices.Clone(cached.changes), true
}

func (c *nativeReadCache) storeChanges(id tracker.NativeWorkItemID, version nativeItemVersion, changes []tracker.ChangeRequest) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.changes[id] = cachedChanges{version: version, changes: slices.Clone(changes)}
}

func (c *nativeReadCache) itemComments(id tracker.NativeWorkItemID) ([]connector.IssueComment, nativeItemVersion, bool) {
	if c == nil {
		return nil, nativeItemVersion{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	version, observed := c.observed[id]
	if !observed {
		return nil, nativeItemVersion{}, false
	}
	cached, ok := c.comments[id]
	if !ok || cached.version != version {
		return nil, version, false
	}
	return slices.Clone(cached.comments), version, true
}

func (c *nativeReadCache) storeComments(id tracker.NativeWorkItemID, version nativeItemVersion, comments []connector.IssueComment) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if current, ok := c.observed[id]; !ok || current != version {
		return
	}
	c.comments[id] = cachedComments{version: version, comments: slices.Clone(comments)}
}

// forget drops cached evidence for every work item a write names, so the
// writer's next read observes its own change.
func (c *nativeReadCache) forget(path string) {
	if c == nil {
		return
	}
	ids := nativeItemIDPattern.FindAllString(path, -1)
	if len(ids) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		item := tracker.NativeWorkItemID(id)
		delete(c.observed, item)
		delete(c.blockers, item)
		delete(c.changes, item)
		delete(c.comments, item)
	}
}
