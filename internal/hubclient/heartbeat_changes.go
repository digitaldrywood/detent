package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type heartbeatProject struct {
	mu          sync.Mutex
	changes     *runnerauth.HeartbeatChanges
	cursor      string
	refresh     bool
	loaded      bool
	freed       bool
	items       map[tracker.NativeWorkItemID]tracker.NativeIssue
	pending     map[tracker.NativeWorkItemID]runnerauth.HeartbeatItem
	approval    policy.Approval
	approvalErr error
	approvalSet bool
}

func (c *Client) forgetHeartbeatItems(path string) {
	ids := nativeItemIDPattern.FindAllString(path, -1)
	if len(ids) == 0 {
		return
	}
	c.heartbeatMu.Lock()
	defer c.heartbeatMu.Unlock()
	for base, state := range c.heartbeatProjects {
		if !strings.HasPrefix(path, base+"/") {
			continue
		}
		state.mu.Lock()
		if state.changes != nil {
			for _, id := range ids {
				item := tracker.NativeWorkItemID(id)
				delete(state.items, item)
				state.pending[item] = runnerauth.HeartbeatItem{ID: item}
			}
		}
		state.mu.Unlock()
	}
}

func (c *NativeClient) heartbeatProject() *heartbeatProject {
	c.client.heartbeatMu.Lock()
	defer c.client.heartbeatMu.Unlock()
	if c.client.heartbeatProjects == nil {
		c.client.heartbeatProjects = make(map[string]*heartbeatProject)
	}
	state := c.client.heartbeatProjects[c.base()]
	if state == nil {
		state = &heartbeatProject{}
		c.client.heartbeatProjects[c.base()] = state
	}
	return state
}

func (c *NativeClient) applyHeartbeatChanges(changes *runnerauth.HeartbeatChanges) {
	state := c.heartbeatProject()
	state.mu.Lock()
	defer state.mu.Unlock()
	if changes == nil || changes.Cursor == "" {
		state.changes, state.cursor, state.loaded, state.approvalSet = nil, "", false, false
	} else {
		if state.changes == nil || changes.Reset {
			state.refresh = true
			state.pending = make(map[tracker.NativeWorkItemID]runnerauth.HeartbeatItem)
		}
		if state.changes == nil || state.changes.PolicyID != changes.PolicyID {
			state.approvalSet = false
		}
		for _, item := range changes.Items {
			state.pending[item.ID] = item
		}
		copyChanges := *changes
		copyChanges.Items = slices.Clone(changes.Items)
		state.changes = &copyChanges
	}
	c.client.capabilitiesMu.Lock()
	defer c.client.capabilitiesMu.Unlock()
	digest := ""
	if state.changes != nil {
		digest = state.changes.CapabilitiesDigest
	}
	if digest != c.client.capabilitiesDigest {
		c.client.capabilitiesAt = time.Time{}
	}
	c.client.capabilitiesDigest = digest
}

func (c *NativeClient) changeCursor() string {
	state := c.heartbeatProject()
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.cursor
}

func (c *NativeClient) claimHint() bool {
	state := c.heartbeatProject()
	state.mu.Lock()
	defer state.mu.Unlock()
	allowed := state.changes == nil || state.changes.Claimable || state.freed
	state.freed = false
	return allowed
}

func (c *NativeClient) heartbeatChangesAvailable() bool {
	state := c.heartbeatProject()
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.changes != nil
}

func (c *NativeClient) slotFreed() {
	state := c.heartbeatProject()
	state.mu.Lock()
	defer state.mu.Unlock()
	state.freed = true
}

func (c *NativeClient) claimsExhausted() {
	state := c.heartbeatProject()
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.changes != nil {
		state.changes.Claimable = false
	}
}

func (c *NativeClient) heartbeatIssues(ctx context.Context) ([]tracker.NativeIssue, bool, error) {
	state := c.heartbeatProject()
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.changes == nil {
		return nil, false, nil
	}
	refreshed := state.refresh || !state.loaded
	if refreshed {
		items := make(map[tracker.NativeWorkItemID]tracker.NativeIssue)
		cursor := ""
		for {
			page, err := c.Issues(ctx, url.Values{"limit": {"200"}, "cursor": {cursor}, "archived": {"all"}})
			if err != nil {
				return nil, true, err
			}
			for _, item := range page.Items {
				items[item.WorkItemID] = item
				c.client.reads.forget(c.base() + "/work-items/" + string(item.WorkItemID))
			}
			if page.NextCursor == "" {
				break
			}
			if page.NextCursor == cursor {
				return nil, true, errors.New("hub repeated issue cursor")
			}
			cursor = page.NextCursor
		}
		state.items, state.loaded, state.refresh = items, true, false
	}
	for id, change := range state.pending {
		if item, ok := state.items[id]; refreshed && ok && item.Revision == change.Revision && item.LastActivityAt.Equal(change.LastActivityAt) {
			delete(state.pending, id)
			continue
		}
		c.client.reads.forget(c.base() + "/work-items/" + string(id))
		item, err := c.Issue(ctx, id)
		var failure *APIError
		if errors.As(err, &failure) && failure.Status == http.StatusNotFound {
			delete(state.items, id)
		} else if err != nil {
			return nil, true, err
		} else {
			state.items[id] = item
		}
		delete(state.pending, id)
	}
	state.cursor = state.changes.Cursor
	items := make([]tracker.NativeIssue, 0, len(state.items))
	for _, item := range state.items {
		items = append(items, item)
	}
	slices.SortFunc(items, func(a, b tracker.NativeIssue) int {
		if a.Number < b.Number {
			return -1
		}
		if a.Number > b.Number {
			return 1
		}
		return 0
	})
	return items, true, nil
}
