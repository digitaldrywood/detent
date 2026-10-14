package scheduleowner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/intake"
)

func TestIssueCoordinatorCreateRacePostsOnce(t *testing.T) {
	t.Parallel()
	clock := newTestClock(time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
	store := newMemoryCoordinationStore(clock.Now)
	backend := &blockingIssueBackend{
		marker:        "<!-- detent-intake:fingerprint -->",
		createStarted: make(chan struct{}),
		createRelease: make(chan struct{}),
	}
	alpha := newTestIssueCoordinator(t, testConfig(), "alpha", store, clock)
	beta := newTestIssueCoordinator(t, testConfig(), "beta", store, clock)
	draft := intake.IssueDraft{Title: "One", Body: "body\n\n" + backend.marker}

	results := make(chan ensureResult, 2)
	go func() {
		issue, created, err := alpha.Ensure(t.Context(), backend.marker, draft, backend)
		results <- ensureResult{issue: issue, created: created, err: err}
	}()
	<-backend.createStarted
	go func() {
		issue, created, err := beta.Ensure(t.Context(), backend.marker, draft, backend)
		results <- ensureResult{issue: issue, created: created, err: err}
	}()
	second := <-results
	close(backend.createRelease)
	first := <-results
	for _, result := range []ensureResult{first, second} {
		if result.err != nil || result.issue.ID != "issue-1" {
			t.Fatalf("Ensure() = %#v", result)
		}
	}
	if first.created == second.created {
		t.Fatalf("created results = %t, %t, want one creator", first.created, second.created)
	}
	if got := backend.CreateCount(); got != 1 {
		t.Fatalf("CreateIntakeIssue() calls = %d, want 1", got)
	}
}

func TestIssueCoordinatorDoesNotStealUncertainCreate(t *testing.T) {
	t.Parallel()
	clock := newTestClock(time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
	store := newMemoryCoordinationStore(clock.Now)
	config := testConfig()
	marker := "<!-- detent-intake:uncertain -->"
	value, err := json.Marshal(effectState{Schema: effectSchema, Status: effectCreating, Token: "dead-owner"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if _, swapped, err := store.CompareAndSwap(t.Context(), effectPath(config.Key, marker), "", value); err != nil || !swapped {
		t.Fatalf("seed CompareAndSwap() = %t, %v", swapped, err)
	}
	clock.Advance(config.LeaseTTL() + config.MaxClockSkew() + time.Second)
	coordinator := newTestIssueCoordinator(t, config, "successor", store, clock)
	backend := &blockingIssueBackend{marker: marker}
	if _, _, err := coordinator.Ensure(t.Context(), marker, intake.IssueDraft{Title: "Uncertain"}, backend); !errors.Is(err, ErrIssueCreateUncertain) {
		t.Fatalf("Ensure() error = %v, want ErrIssueCreateUncertain", err)
	}
	if got := backend.CreateCount(); got != 0 {
		t.Fatalf("CreateIntakeIssue() calls = %d, want 0", got)
	}
}

func TestIssueCoordinatorCompletesAfterCreateContextCancellation(t *testing.T) {
	t.Parallel()
	clock := newTestClock(time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
	store := newMemoryCoordinationStore(clock.Now)
	coordinator := newTestIssueCoordinator(t, testConfig(), "alpha", store, clock)
	marker := "<!-- detent-intake:canceled-create -->"
	ctx, cancel := context.WithCancel(t.Context())
	backend := &cancelingIssueBackend{cancel: cancel, marker: marker}

	issue, created, err := coordinator.Ensure(ctx, marker, intake.IssueDraft{Title: "One", Body: marker}, backend)
	if err != nil || !created || issue.ID != "issue-1" {
		t.Fatalf("Ensure() = %#v, %t, %v", issue, created, err)
	}
	second := newTestIssueCoordinator(t, testConfig(), "beta", store, clock)
	issue, created, err = second.Ensure(t.Context(), marker, intake.IssueDraft{Title: "One", Body: marker}, backend)
	if err != nil || created || issue.ID != "issue-1" {
		t.Fatalf("second Ensure() = %#v, %t, %v", issue, created, err)
	}
}

func TestIssueCoordinatorRecurringIssueReplacesClosedCompletion(t *testing.T) {
	t.Parallel()
	clock := newTestClock(time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
	store := newMemoryCoordinationStore(clock.Now)
	coordinator := newTestIssueCoordinator(t, testConfig(), "alpha", store, clock)
	marker := "<!-- detent-routine:recurring -->"
	backend := &blockingIssueBackend{marker: marker}
	draft := intake.IssueDraft{Title: "Recurring", Body: marker}

	first, created, err := coordinator.EnsureRecurring(t.Context(), marker, draft, backend)
	if err != nil || !created || first.ID != "issue-1" {
		t.Fatalf("first EnsureRecurring() = %#v, %t, %v", first, created, err)
	}
	backend.Close(first.ID)
	second, created, err := coordinator.EnsureRecurring(t.Context(), marker, draft, backend)
	if err != nil || !created || second.ID != "issue-2" {
		t.Fatalf("second EnsureRecurring() = %#v, %t, %v", second, created, err)
	}
	third, created, err := coordinator.EnsureRecurring(t.Context(), marker, draft, backend)
	if err != nil || created || third.ID != second.ID {
		t.Fatalf("third EnsureRecurring() = %#v, %t, %v", third, created, err)
	}
	if got := backend.CreateCount(); got != 2 {
		t.Fatalf("CreateIntakeIssue() calls = %d, want 2", got)
	}
}

type ensureResult struct {
	issue   intake.Issue
	created bool
	err     error
}

type blockingIssueBackend struct {
	mu            sync.Mutex
	marker        string
	issue         intake.Issue
	creates       int
	createStarted chan struct{}
	createRelease chan struct{}
	startedOnce   sync.Once
}

type cancelingIssueBackend struct {
	cancel context.CancelFunc
	marker string
	issue  intake.Issue
}

func (b *cancelingIssueBackend) FindIntakeIssue(_ context.Context, marker string) (intake.Issue, bool, error) {
	return b.issue, marker == b.marker && b.issue.ID != "", nil
}

func (b *cancelingIssueBackend) CreateIntakeIssue(_ context.Context, draft intake.IssueDraft) (intake.Issue, error) {
	b.issue = intake.Issue{ID: "issue-1", Identifier: "example#1", Body: draft.Body}
	b.cancel()
	return b.issue, nil
}

func (b *blockingIssueBackend) FindIntakeIssue(_ context.Context, marker string) (intake.Issue, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if marker != b.marker || b.issue.ID == "" {
		return intake.Issue{}, false, nil
	}
	return b.issue, true, nil
}

func (b *blockingIssueBackend) CreateIntakeIssue(ctx context.Context, draft intake.IssueDraft) (intake.Issue, error) {
	b.mu.Lock()
	b.creates++
	b.issue = intake.Issue{ID: fmt.Sprintf("issue-%d", b.creates), Identifier: fmt.Sprintf("example#%d", b.creates), Body: draft.Body}
	issue := b.issue
	b.mu.Unlock()
	if b.createStarted != nil {
		b.startedOnce.Do(func() { close(b.createStarted) })
	}
	if b.createRelease != nil {
		select {
		case <-ctx.Done():
			return intake.Issue{}, ctx.Err()
		case <-b.createRelease:
		}
	}
	return issue, nil
}

func (b *blockingIssueBackend) IntakeIssueClosed(_ context.Context, issueID string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.issue.ID == issueID && b.issue.Closed, nil
}

func (b *blockingIssueBackend) Close(issueID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.issue.ID == issueID {
		b.issue.Closed = true
	}
}

func (b *blockingIssueBackend) CreateCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.creates
}

func newTestIssueCoordinator(
	t *testing.T,
	config Config,
	token string,
	store *memoryCoordinationStore,
	clock *testClock,
) *IssueCoordinator {
	t.Helper()
	coordinator, err := NewIssueCoordinator(config, store, Dependencies{
		Now:   clock.Now,
		Token: func() (string, error) { return token, nil },
	})
	if err != nil {
		t.Fatalf("NewIssueCoordinator() error = %v", err)
	}
	return coordinator
}
