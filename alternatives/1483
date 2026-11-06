package hubserver

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func exactBoardDisplay(row viewstore.BoardRecord, source tracker.NativeBoardCard) bool {
	projected := projectedBoardCard(row)
	for _, card := range []*tracker.NativeBoardCard{&source, &projected} {
		issue := &card.Issue
		issue.WebURL = ""
		issue.Dependencies, issue.Blockers, issue.Dependents, issue.Related = nil, nil, nil, nil
		issue.DependencyReady = false
		issue.BlockedReason, issue.UnblocksReason = tracker.DependencyReason{}, tracker.DependencyReason{}
	}
	times := []time.Time{source.Issue.CreatedAt, source.Issue.UpdatedAt, source.Issue.LastActivityAt}
	if p := source.Issue.Provenance; p != nil {
		times = append(times, p.CreatedAt, p.UpdatedAt, p.ObservedAt)
	}
	if source.Issue.ClosedAt != nil {
		times = append(times, *source.Issue.ClosedAt)
	}
	if source.Attempt != nil {
		times = append(times, source.Attempt.StartedAt, source.Attempt.ExpiresAt)
	}
	if source.Execution != nil {
		times = append(times, source.Execution.ExpiresAt)
		if source.Execution.StartedAt != nil {
			times = append(times, *source.Execution.StartedAt)
		}
	}
	for _, at := range times {
		if at.Format(time.RFC3339Nano) != at.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano) {
			return false
		}
	}
	want, err := json.Marshal(source)
	if err != nil {
		return false
	}
	got, err := json.Marshal(projected)
	return err == nil && bytes.Equal(want, got)
}
