package intake

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestManagerCreatesThenUpdatesDeduplicatedWebhookIssue(t *testing.T) {
	t.Parallel()

	store := &fakeIssueStore{}
	manager := newWebhookManager(t, store, "level:error")
	first, err := manager.IngestWebhook(context.Background(), "alerts", []byte(`{
  "summary":"Database unavailable",
  "details":"Primary connection failed",
  "fingerprint":"db-down",
  "level":"error"
}`))
	if err != nil {
		t.Fatalf("first IngestWebhook() error = %v", err)
	}
	if !first.Created || !first.Matched || first.Issue.ID != "issue-1" {
		t.Fatalf("first result = %#v", first)
	}
	if len(store.created) != 1 || store.created[0].Title != "[alerts] Database unavailable" {
		t.Fatalf("created drafts = %#v", store.created)
	}
	if !strings.Contains(store.created[0].Body, "Primary connection failed") || !strings.Contains(store.created[0].Body, "<!-- detent-intake:") || !strings.Contains(store.created[0].Body, pendingStateMarker) {
		t.Fatalf("created body = %q", store.created[0].Body)
	}
	if len(store.states) != 1 || store.states[0] != "issue-1:Backlog" {
		t.Fatalf("state writes = %#v, want one Backlog write", store.states)
	}

	second, err := manager.IngestWebhook(context.Background(), "alerts", []byte(`{
  "summary":"Database still unavailable",
  "details":"Replica also failed",
  "fingerprint":"db-down",
  "level":"error"
}`))
	if err != nil {
		t.Fatalf("second IngestWebhook() error = %v", err)
	}
	if second.Created || !second.Matched || second.Issue.ID != "issue-1" {
		t.Fatalf("second result = %#v", second)
	}
	if len(store.created) != 1 || len(store.updated) != 2 {
		t.Fatalf("create/update counts = %d/%d, want 1/2", len(store.created), len(store.updated))
	}
	if store.updated[1].Title != "[alerts] Database still unavailable" {
		t.Fatalf("updated title = %q", store.updated[1].Title)
	}
	if len(store.states) != 1 {
		t.Fatalf("state writes = %#v, recurring event reset starting state", store.states)
	}
}

func TestManagerSkipsUnmatchedWebhookEvent(t *testing.T) {
	t.Parallel()

	store := &fakeIssueStore{}
	manager := newWebhookManager(t, store, "level:error")
	result, err := manager.IngestWebhook(context.Background(), "alerts", []byte(`{
  "summary":"Deploy finished",
  "fingerprint":"deploy-1",
  "level":"info"
}`))
	if err != nil {
		t.Fatalf("IngestWebhook() error = %v", err)
	}
	if result.Matched || result.Created {
		t.Fatalf("result = %#v, want unmatched", result)
	}
	if len(store.created) != 0 || store.findCalls != 0 {
		t.Fatalf("store calls = created %d find %d, want none", len(store.created), store.findCalls)
	}
}

func TestManagerUpdatesDurableFingerprintMatch(t *testing.T) {
	t.Parallel()

	store := &fakeIssueStore{found: Issue{ID: "existing", Identifier: "example/repo#42", Number: 42}}
	manager := newWebhookManager(t, store, "")
	result, err := manager.IngestWebhook(context.Background(), "alerts", []byte(`{
  "summary":"Repeated alert",
  "fingerprint":"repeat-42"
}`))
	if err != nil {
		t.Fatalf("IngestWebhook() error = %v", err)
	}
	if result.Created || result.Issue.ID != "existing" {
		t.Fatalf("result = %#v, want existing issue update", result)
	}
	if len(store.updated) != 1 || len(store.states) != 0 {
		t.Fatalf("updates/states = %d/%d, want 1/0", len(store.updated), len(store.states))
	}
}

func TestManagerRequiresConfiguredDedupeField(t *testing.T) {
	t.Parallel()

	manager := newWebhookManager(t, &fakeIssueStore{}, "")
	_, err := manager.IngestWebhook(context.Background(), "alerts", []byte(`{"summary":"No identity"}`))
	if !errors.Is(err, ErrMissingFingerprint) {
		t.Fatalf("IngestWebhook() error = %v, want ErrMissingFingerprint", err)
	}
}

func TestManagerRetriesPendingStartingState(t *testing.T) {
	t.Parallel()

	store := &fakeIssueStore{stateErrors: []error{errors.New("temporary state failure")}}
	manager := newWebhookManager(t, store, "")
	payload := []byte(`{"summary":"Database down","fingerprint":"db-pending"}`)
	first, err := manager.IngestWebhook(context.Background(), "alerts", payload)
	if err == nil || !first.Created {
		t.Fatalf("first IngestWebhook() = %#v, %v, want created result with state error", first, err)
	}
	second, err := manager.IngestWebhook(context.Background(), "alerts", payload)
	if err != nil {
		t.Fatalf("second IngestWebhook() error = %v", err)
	}
	if second.Created || len(store.states) != 1 {
		t.Fatalf("second result/states = %#v/%#v, want deduplicated state retry", second, store.states)
	}
	if strings.Contains(second.Issue.Body, pendingStateMarker) {
		t.Fatalf("second issue body = %q, pending marker was not cleared", second.Issue.Body)
	}
}

func TestManagerRunsScheduledScannerThroughFactory(t *testing.T) {
	t.Parallel()

	store := &fakeIssueStore{}
	manager, err := New(Config{Sources: []Source{{
		Name: "todos",
		Kind: KindSchedule,
		Cron: "0 6 * * 1",
		Scan: "custom",
		Creates: Creates{
			Status: "Backlog",
			Title:  "{summary}",
		},
	}}}, store, Dependencies{
		Root: "/source",
		ScannerFactory: fakeScannerFactory{events: []Event{{
			Summary:     "TODO in main.go",
			Details:     "Location: main.go:10",
			Fingerprint: "main.go:todo",
		}}},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	results, err := manager.RunScheduled(context.Background(), "todos")
	if err != nil {
		t.Fatalf("RunScheduled() error = %v", err)
	}
	if len(results) != 1 || !results[0].Created {
		t.Fatalf("results = %#v, want one created issue", results)
	}
}

func TestManagerPrepareRejectsUnknownSourceWithoutChangingRuntime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		source Source
		want   error
	}{
		{
			name: "webhook adapter",
			source: Source{
				Name:   "new-alerts",
				Kind:   "pagerduty",
				Secret: "secret",
			},
			want: ErrUnknownAdapter,
		},
		{
			name: "scheduled scanner",
			source: Source{
				Name: "new-scan",
				Kind: KindSchedule,
				Cron: "0 6 * * 1",
				Scan: "custom",
			},
			want: ErrUnknownScanner,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := &fakeIssueStore{}
			manager := newWebhookManager(t, store, "")
			_, err := manager.Prepare(Config{Sources: []Source{tt.source}}, store, t.TempDir())
			if !errors.Is(err, tt.want) {
				t.Fatalf("Prepare() error = %v, want %v", err, tt.want)
			}
			if _, ok := manager.Source("alerts"); !ok {
				t.Fatal("Prepare() removed the active source")
			}
			if _, ok := manager.Source(tt.source.Name); ok {
				t.Fatalf("Prepare() applied invalid source %q", tt.source.Name)
			}
		})
	}
}

func newWebhookManager(t *testing.T, store IssueStore, match string) *Manager {
	t.Helper()
	manager, err := New(Config{Sources: []Source{{
		Name:     "alerts",
		Kind:     KindWebhook,
		Secret:   "secret",
		Match:    match,
		DedupeBy: "fingerprint",
		Creates: Creates{
			Status: "Backlog",
			Labels: []string{"bug"},
			Title:  "[{source}] {summary}",
			Body:   "{details}",
		},
	}}}, store, Dependencies{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return manager
}

type fakeIssueStore struct {
	found       Issue
	findCalls   int
	created     []IssueDraft
	updated     []IssueDraft
	states      []string
	stateErrors []error
}

func (s *fakeIssueStore) FindIntakeIssue(context.Context, string) (Issue, bool, error) {
	s.findCalls++
	return s.found, s.found.ID != "", nil
}

func (s *fakeIssueStore) CreateIntakeIssue(_ context.Context, draft IssueDraft) (Issue, error) {
	s.created = append(s.created, draft)
	return Issue{ID: "issue-1", Identifier: "example/repo#1", Number: 1, Body: draft.Body}, nil
}

func (s *fakeIssueStore) UpdateIntakeIssue(_ context.Context, issueID string, draft IssueDraft) (Issue, error) {
	s.updated = append(s.updated, draft)
	return Issue{ID: issueID, Identifier: "example/repo#1", Number: 1, Body: draft.Body}, nil
}

func (s *fakeIssueStore) SetIntakeIssueState(_ context.Context, issueID string, state string) error {
	if len(s.stateErrors) > 0 {
		err := s.stateErrors[0]
		s.stateErrors = s.stateErrors[1:]
		if err != nil {
			return err
		}
	}
	s.states = append(s.states, issueID+":"+state)
	return nil
}

type fakeScannerFactory struct {
	events []Event
}

func (f fakeScannerFactory) New(string, string) (Scanner, error) {
	return fakeScanner(f), nil
}

type fakeScanner struct {
	events []Event
}

func (s fakeScanner) Scan(context.Context) ([]Event, error) {
	return append([]Event(nil), s.events...), nil
}
