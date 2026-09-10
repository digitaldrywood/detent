package scheduleowner

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/issueorigin"
)

type occurrenceBackend struct {
	IssueBackend
	comments int
	err      error
}

func (b *occurrenceBackend) CreateComment(context.Context, string, string) error {
	b.comments++
	return b.err
}

func TestCoordinatedOccurrence(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name             string
		machine, failure bool
		comments         int
	}{
		{"machine", true, false, 1},
		{"failure", true, true, 1},
		{"legacy coordination", false, false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backend := &occurrenceBackend{}
			if tt.failure {
				backend.err = errors.New("comment failed")
			}
			draft := intake.IssueDraft{Body: "body"}
			if tt.machine {
				draft.Body = issueorigin.Stamp(draft.Body, issueorigin.Origin{Kind: "routine", Fingerprint: "problem"})
			}
			issue, err := commentExistingOccurrence(t.Context(), backend, intake.Issue{ID: "issue"}, draft)
			if (err != nil) != tt.failure || backend.comments != tt.comments || issue.Reused != (tt.machine && !tt.failure) {
				t.Fatalf("issue = %+v, error = %v, comments = %d", issue, err, backend.comments)
			}
		})
	}
}

func TestCoordinatedOccurrenceDuringPublication(t *testing.T) {
	t.Parallel()
	for _, state := range []string{effectCreating, effectComplete} {
		t.Run(state, func(t *testing.T) {
			clock := newTestClock(time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC))
			store := newMemoryCoordinationStore(clock.Now)
			config := testConfig()
			marker := "machine:disk"
			existing := intake.Issue{ID: "issue-1", Body: "original"}
			value, err := json.Marshal(effectState{Schema: effectSchema, Status: state, Token: "creator", Issue: existing})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.CompareAndSwap(t.Context(), effectPath(config.Key, marker), "", value); err != nil {
				t.Fatal(err)
			}
			underlying := &blockingIssueBackend{marker: marker, issue: existing}
			backend := &occurrenceBackend{IssueBackend: underlying}
			coordinator := newTestIssueCoordinator(t, config, "second", store, clock)
			issue, created, err := coordinator.Ensure(t.Context(), marker, intake.IssueDraft{Body: issueorigin.Stamp("again", issueorigin.Origin{Kind: "worker", Fingerprint: "disk"})}, backend)
			if err != nil || created || !issue.Reused || backend.comments != 1 || underlying.CreateCount() != 0 {
				t.Fatalf("issue=%+v created=%v err=%v comments=%d", issue, created, err, backend.comments)
			}
		})
	}
}
