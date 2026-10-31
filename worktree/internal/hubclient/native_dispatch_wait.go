package hubclient

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Scheduler) WaitCandidateChanges(ctx context.Context, project string, wake chan<- struct{}) {
	source := s.nativeProject(project)
	if source == nil {
		return
	}
	var cursor string
	backoff := time.Second
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if source.client.heartbeatChangesAvailable() {
			routing := source.client.client.runner
			if routing == nil {
				return
			}
			_, changed, err := routing.availabilityState()
			if err != nil {
				return
			}
			select {
			case wake <- struct{}{}:
			default:
			}
			select {
			case <-ctx.Done():
				return
			case <-changed:
			}
			timer.Reset(0)
			continue
		}
		started := time.Now()
		var page struct {
			Cursor string `json:"cursor"`
		}
		var err error
		if cursor == "" {
			var supported bool
			supported, err = source.client.HubFeature(ctx, tracker.NativeDispatchWaitCapability)
			if err == nil && !supported {
				return
			}
		}
		if err == nil {
			query := url.Values{"after": {cursor}, "wait": {"30"}}
			err = source.client.client.requestWithDeadline(ctx, 40*time.Second, http.MethodGet, source.client.base()+"/claims/wait?"+query.Encode(), nil, nil, &page)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil || page.Cursor == "" {
			timer.Reset(backoff)
			backoff = min(30*time.Second, backoff*2)
			continue
		}
		backoff = time.Second
		if page.Cursor != cursor {
			cursor = page.Cursor
			select {
			case wake <- struct{}{}:
			default:
			}
		}
		timer.Reset(max(0, time.Second-time.Since(started)))
	}
}
