package intake

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type occurrenceStore struct {
	*fakeIssueStore
	comments []string
	err      error
}

func (s *occurrenceStore) CreateComment(_ context.Context, _, body string) error {
	if s.err != nil {
		return s.err
	}
	s.comments = append(s.comments, body)
	return nil
}

func TestIntakeDuplicateComments(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		fail bool
	}{{"repeat", false}, {"comment failure", true}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &occurrenceStore{fakeIssueStore: &fakeIssueStore{}}
			manager := newWebhookManager(t, store, "")
			payload := []byte(`{"summary":"Disk full","fingerprint":"disk"}`)
			first, err := manager.IngestWebhook(t.Context(), "alerts", payload)
			if err != nil || !first.Created {
				t.Fatalf("first = %+v, %v", first, err)
			}
			if tt.fail {
				store.err = errors.New("comment unavailable")
			}
			second, err := manager.IngestWebhook(t.Context(), "alerts", payload)
			if (err != nil) != tt.fail || second.Created || len(store.created) != 1 || len(store.updated) != 1 || len(store.states) != 1 {
				t.Fatalf("second = %+v, %v; creates=%d updates=%d states=%d", second, err, len(store.created), len(store.updated), len(store.states))
			}
			if !tt.fail && (len(store.comments) != 1 || !strings.Contains(store.comments[0], "New machine occurrence")) {
				t.Fatalf("comments = %v", store.comments)
			}
		})
	}
}

func TestOccurrenceRequiresCommentSupport(t *testing.T) {
	t.Parallel()
	if err := CommentOccurrence(t.Context(), nil, "issue", "body"); err == nil {
		t.Fatal("missing comment support accepted")
	}
}
