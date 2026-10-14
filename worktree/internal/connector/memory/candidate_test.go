package memory

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestConnectorReadCandidatesFiltersSortsAndTruncates(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	c := New(Config{Issues: []connector.Issue{
		{ID: "3", Identifier: "DD-3", State: "Backlog", CreatedAt: timePointer(at.Add(3 * time.Hour))},
		{ID: "ignored", Identifier: "DD-0", State: "Todo", CreatedAt: timePointer(at)},
		{ID: "2", Identifier: "DD-2", State: "backlog", CreatedAt: timePointer(at.Add(2 * time.Hour))},
		{ID: "1", Identifier: "DD-1", State: "Backlog", CreatedAt: timePointer(at.Add(time.Hour))},
	}})

	got, err := c.ReadCandidates(context.Background(), connector.CandidateRequest{
		Selector: connector.CandidateSelectorStates,
		States:   []string{"BACKLOG"},
		Limit:    2,
		PageSize: 1,
	})
	if err != nil {
		t.Fatalf("ReadCandidates() error = %v", err)
	}
	ids := []string{got.Issues[0].ID, got.Issues[1].ID}
	if !reflect.DeepEqual(ids, []string{"1", "2"}) {
		t.Fatalf("candidate IDs = %#v, want [1 2]", ids)
	}
	if !got.Truncated {
		t.Fatalf("result = %#v, want truncation", got)
	}
}

func TestConnectorReadCandidatesSelectsAnyLabel(t *testing.T) {
	t.Parallel()

	c := New(Config{Issues: []connector.Issue{
		{ID: "1", Identifier: "DD-1", State: "Backlog", Labels: []string{"needs-decision"}},
		{ID: "2", Identifier: "DD-2", State: "Todo", Labels: []string{"SENTRY"}},
		{ID: "3", Identifier: "DD-3", State: "Done", Labels: []string{"unrelated"}},
	}})

	got, err := c.ReadCandidates(context.Background(), connector.CandidateRequest{
		Selector: connector.CandidateSelectorLabels,
		Labels:   []string{"sentry", "needs-decision"},
		Limit:    10,
	})
	if err != nil {
		t.Fatalf("ReadCandidates() error = %v", err)
	}
	ids := []string{got.Issues[0].ID, got.Issues[1].ID}
	if !reflect.DeepEqual(ids, []string{"1", "2"}) {
		t.Fatalf("candidate IDs = %#v, want [1 2]", ids)
	}
}

func timePointer(value time.Time) *time.Time {
	return &value
}
