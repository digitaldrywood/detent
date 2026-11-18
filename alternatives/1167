package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeLaneEntryHistoryPages = 50

type nativeLaneEntry struct {
	revision   string
	transition connector.IssueStateTransition
	found      bool
}

type nativeLaneEntries struct {
	mu      sync.Mutex
	entries map[tracker.NativeWorkItemID]nativeLaneEntry
}

// CurrentLaneEntry reads the item's latest workflow transition into its
// current lane. Lane moves change the item revision, so a revision-keyed cache
// avoids rereading history on every dispatch tick.
func (c *NativeConnector) CurrentLaneEntry(ctx context.Context, issue connector.Issue) (connector.IssueStateTransition, bool, error) {
	id := tracker.NativeWorkItemID(strings.TrimSpace(issue.ID))
	if c == nil || c.client == nil || id == "" {
		return connector.IssueStateTransition{}, false, nil
	}
	revision := issue.Metadata["hub_revision"]
	if cached, ok := c.laneEntries.lookup(id, revision); ok {
		return cached.transition, cached.found, nil
	}
	path, err := NativeItemPath(id)
	if err != nil {
		return connector.IssueStateTransition{}, false, err
	}
	var latest *tracker.CollaborationEvent
	cursor := ""
	for range nativeLaneEntryHistoryPages {
		params := url.Values{"view": {"timeline"}, "direction": {"newest"}, "limit": {"200"}, "cursor": {cursor}}
		var page tracker.Page[tracker.CollaborationEvent]
		if err := c.client.client.request(ctx, http.MethodGet, c.client.base()+path+"/history?"+params.Encode(), nil, &page); err != nil {
			return connector.IssueStateTransition{}, false, err
		}
		for index := range page.Items {
			if page.Items[index].Type == "workflow.transitioned" {
				latest = &page.Items[index]
				break
			}
		}
		cursor = page.NextCursor
		if latest != nil || cursor == "" {
			break
		}
	}
	if latest == nil && cursor != "" {
		return connector.IssueStateTransition{}, false, errors.New("current lane entry history is incomplete")
	}
	entry := nativeLaneEntry{revision: revision}
	if latest != nil && strings.EqualFold(strings.TrimSpace(latest.Data.ToState), strings.TrimSpace(issue.State)) {
		entry.found = true
		entry.transition = connector.IssueStateTransition{
			EnteredAt: latest.RecordedAt.UTC(),
			Actor:     connector.IssueActor{Kind: latest.Actor.Kind, Login: latest.Actor.PrincipalID},
		}
	}
	c.laneEntries.store(id, entry)
	return entry.transition, entry.found, nil
}

func (e *nativeLaneEntries) lookup(id tracker.NativeWorkItemID, revision string) (nativeLaneEntry, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.entries[id]
	return entry, ok && revision != "" && entry.revision == revision
}

func (e *nativeLaneEntries) store(id tracker.NativeWorkItemID, entry nativeLaneEntry) {
	if entry.revision == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.entries == nil {
		e.entries = make(map[tracker.NativeWorkItemID]nativeLaneEntry)
	}
	e.entries[id] = entry
}
