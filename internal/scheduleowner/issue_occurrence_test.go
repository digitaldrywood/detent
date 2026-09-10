package scheduleowner

import (
	"context"
	"errors"
	"testing"

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
