package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestChangeWorkflowStateCommitsStateAndOutboxAtomically(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	repositoryID, issueID := seedProjection(t, service.database.db)
	workflowStateID := insertWorkflowState(t, service.database.db, repositoryID, "In Progress")
	mutation := WorkflowLabelMutation{
		IdempotencyKey: "issue-1-state-in-progress",
		RepositoryID:   repositoryID,
		IssueID:        issueID,
		Label:          "detent:in-progress",
	}

	item, err := service.ChangeWorkflowState(t.Context(), WorkflowStateChange{
		IssueID:         issueID,
		WorkflowStateID: workflowStateID,
		Mutation:        mutation,
	})
	if err != nil {
		t.Fatalf("ChangeWorkflowState() error = %v", err)
	}
	if item.IdempotencyKey != mutation.IdempotencyKey || item.Status != outboxPending {
		t.Fatalf("outbox item = %#v", item)
	}
	assertIssueWorkflowState(t, service.database.db, issueID, workflowStateID)
	assertOutboxCount(t, service.database.db, mutation.IdempotencyKey, 1)

	if _, err := service.ChangeWorkflowState(t.Context(), WorkflowStateChange{
		IssueID:         issueID,
		WorkflowStateID: workflowStateID + 1000,
		Mutation: WorkflowLabelMutation{
			IdempotencyKey: "issue-1-invalid-state",
			RepositoryID:   repositoryID,
			IssueID:        issueID,
			Label:          "detent:blocked",
		},
	}); err == nil {
		t.Fatal("ChangeWorkflowState(invalid) error = nil")
	}
	assertIssueWorkflowState(t, service.database.db, issueID, workflowStateID)
	assertOutboxCount(t, service.database.db, "issue-1-invalid-state", 0)
}

func TestAtomicMutationReplayDoesNotDuplicateLocalState(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	repositoryID, issueID := seedProjection(t, service.database.db)
	change := WorkEventChange{
		IssueID: issueID,
		Kind:    "progress",
		Payload: json.RawMessage(`{"detail":"implementing"}`),
		Mutation: WorkpadMutation{
			IdempotencyKey: "issue-1-progress-coding",
			RepositoryID:   repositoryID,
			IssueID:        issueID,
			Phase:          "coding",
			Body:           "## Codex Workpad\n\nImplementing.",
		},
	}

	first, err := service.AppendWorkEvent(t.Context(), change)
	if err != nil {
		t.Fatalf("AppendWorkEvent(first) error = %v", err)
	}
	second, err := service.AppendWorkEvent(t.Context(), change)
	if err != nil {
		t.Fatalf("AppendWorkEvent(replay) error = %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("replayed outbox id = %d, want %d", second.ID, first.ID)
	}
	assertTableCount(t, service.database.db, "work_events", 1)
	assertOutboxCount(t, service.database.db, change.Mutation.(WorkpadMutation).IdempotencyKey, 1)
}

func TestAppendWorkEventCoalescesPendingWorkpadUpdates(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	repositoryID, issueID := seedProjection(t, service.database.db)
	tests := []struct {
		key   string
		phase string
		body  string
	}{
		{key: "coding-1", phase: "coding", body: "Started coding"},
		{key: "coding-2", phase: "coding", body: "Coding complete"},
		{key: "testing-1", phase: "testing", body: "Started testing"},
	}
	for _, test := range tests {
		_, err := service.AppendWorkEvent(t.Context(), WorkEventChange{
			IssueID: issueID,
			Kind:    "progress",
			Mutation: WorkpadMutation{
				IdempotencyKey: test.key,
				RepositoryID:   repositoryID,
				IssueID:        issueID,
				Phase:          test.phase,
				Body:           workpadHeadingForTest(test.body),
			},
		})
		if err != nil {
			t.Fatalf("AppendWorkEvent(%s) error = %v", test.key, err)
		}
	}

	assertOutboxStatus(t, service.database.db, "coding-1", outboxSuperseded, 0)
	assertOutboxStatus(t, service.database.db, "coding-2", outboxSuperseded, 0)
	assertOutboxStatus(t, service.database.db, "testing-1", outboxPending, 0)
	assertTableCount(t, service.database.db, "work_events", 3)
}

func TestOutboxRetriesWithStableKeyAndExponentialJitter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	backend := &recordingOutboxBackend{failures: []error{errors.New("github unavailable"), errors.New("github unavailable")}}
	service := openManualOutboxService(t, Config{
		DatabasePath:      filepath.Join(t.TempDir(), "hub.db"),
		OutboxBaseBackoff: time.Second,
		OutboxMaxBackoff:  time.Minute,
		OutboxMaxAttempts: 3,
		now:               func() time.Time { return now },
		jitter:            func() float64 { return 0.25 },
	}, backend)
	repositoryID, issueID := seedProjection(t, service.database.db)
	appendTestWorkpadEvent(t, service, repositoryID, issueID, "stable-key", "coding")

	processed, err := service.ProcessOutbox(t.Context())
	if !processed || err == nil {
		t.Fatalf("ProcessOutbox(first) = %t, %v; want processed failure", processed, err)
	}
	assertOutboxRetryAt(t, service.database.db, "stable-key", now.Add(750*time.Millisecond), 1)
	if processed, err := service.ProcessOutbox(t.Context()); processed || err != nil {
		t.Fatalf("ProcessOutbox(before retry) = %t, %v; want idle", processed, err)
	}

	now = now.Add(750 * time.Millisecond)
	processed, err = service.ProcessOutbox(t.Context())
	if !processed || err == nil {
		t.Fatalf("ProcessOutbox(second) = %t, %v; want processed failure", processed, err)
	}
	assertOutboxRetryAt(t, service.database.db, "stable-key", now.Add(1500*time.Millisecond), 2)

	now = now.Add(1500 * time.Millisecond)
	processed, err = service.ProcessOutbox(t.Context())
	if !processed || err != nil {
		t.Fatalf("ProcessOutbox(third) = %t, %v; want success", processed, err)
	}
	assertOutboxStatus(t, service.database.db, "stable-key", outboxCompleted, 3)
	if got := backend.keys(); len(got) != 3 || got[0] != "stable-key" || got[1] != got[0] || got[2] != got[0] {
		t.Fatalf("backend keys = %v, want stable replay key", got)
	}
	if backend.effects != 1 {
		t.Fatalf("backend effects = %d, want one idempotent effect", backend.effects)
	}
}

func TestOutboxDeadLettersPermanentAndExhaustedFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		failures    []error
		maxAttempts int
	}{
		{name: "permanent", failures: []error{Permanent(errors.New("target was deleted"))}, maxAttempts: 8},
		{name: "exhausted", failures: []error{errors.New("unavailable"), errors.New("still unavailable")}, maxAttempts: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
			backend := &recordingOutboxBackend{failures: test.failures}
			service := openManualOutboxService(t, Config{
				DatabasePath:      filepath.Join(t.TempDir(), "hub.db"),
				OutboxBaseBackoff: time.Second,
				OutboxMaxAttempts: test.maxAttempts,
				now:               func() time.Time { return now },
				jitter:            func() float64 { return 0.5 },
			}, backend)
			repositoryID, issueID := seedProjection(t, service.database.db)
			appendTestWorkpadEvent(t, service, repositoryID, issueID, "dead-key", "coding")

			for attempt := range len(test.failures) {
				processed, err := service.ProcessOutbox(t.Context())
				if !processed || err == nil {
					t.Fatalf("ProcessOutbox(%d) = %t, %v; want failure", attempt, processed, err)
				}
				now = now.Add(time.Second)
			}
			assertOutboxStatus(t, service.database.db, "dead-key", outboxDeadLetter, len(test.failures))
			health, err := service.OutboxHealth(t.Context())
			if err != nil {
				t.Fatalf("OutboxHealth() error = %v", err)
			}
			if health.DeadLetters != 1 || len(health.OperatorActions) != 1 || health.OperatorActions[0].Action == "" {
				t.Fatalf("OutboxHealth() = %#v, want one operator-visible dead letter", health)
			}
		})
	}
}

func TestIrreversibleOutboxUsesFreshVerificationPathAndFailsClosed(t *testing.T) {
	t.Parallel()

	backend := &recordingOutboxBackend{verifyFailures: []error{Permanent(errors.New("fresh checks unavailable"))}}
	service := openManualOutboxService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")}, backend)
	repositoryID, issueID := seedProjection(t, service.database.db)
	_, err := service.AppendWorkEvent(t.Context(), WorkEventChange{
		IssueID: issueID,
		Kind:    "merge_requested",
		Mutation: MergePullRequestMutation{
			IdempotencyKey:    "merge-pr-1-abc",
			RepositoryID:      repositoryID,
			IssueID:           issueID,
			PullRequestNumber: 1,
			HeadSHA:           "abc",
			MergeMethod:       "squash",
		},
	})
	if err != nil {
		t.Fatalf("AppendWorkEvent() error = %v", err)
	}

	processed, err := service.ProcessOutbox(t.Context())
	if !processed || err == nil {
		t.Fatalf("ProcessOutbox() = %t, %v; want fail-closed error", processed, err)
	}
	if backend.executeCalls != 0 || backend.verifyCalls != 1 || backend.effects != 0 {
		t.Fatalf("backend calls execute=%d verify=%d effects=%d", backend.executeCalls, backend.verifyCalls, backend.effects)
	}
	assertOutboxStatus(t, service.database.db, "merge-pr-1-abc", outboxDeadLetter, 1)
}

func TestOutboxReclaimsInterruptedProcessingAfterTimeout(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	backend := &recordingOutboxBackend{}
	service := openManualOutboxService(t, Config{
		DatabasePath:            filepath.Join(t.TempDir(), "hub.db"),
		OutboxProcessingTimeout: time.Minute,
		now:                     func() time.Time { return now },
	}, backend)
	repositoryID, issueID := seedProjection(t, service.database.db)
	appendTestWorkpadEvent(t, service, repositoryID, issueID, "interrupted-key", "coding")
	if _, err := service.database.db.ExecContext(t.Context(), `
		UPDATE github_outbox
		SET status = ?, processing_started_at = ?, attempts = 1
		WHERE idempotency_key = ?`, outboxProcessing, formatOutboxTime(now.Add(-2*time.Minute)), "interrupted-key"); err != nil {
		t.Fatalf("seed interrupted item: %v", err)
	}

	processed, err := service.ProcessOutbox(t.Context())
	if !processed || err != nil {
		t.Fatalf("ProcessOutbox() = %t, %v; want reclaimed success", processed, err)
	}
	assertOutboxStatus(t, service.database.db, "interrupted-key", outboxCompleted, 2)
}

func TestServiceCloseCancelsAndDrainsOutboxWorker(t *testing.T) {
	t.Parallel()

	backend := &blockingOutboxBackend{started: make(chan struct{})}
	service, err := Open(t.Context(), Config{
		DatabasePath:       filepath.Join(t.TempDir(), "hub.db"),
		Logger:             discardLogger(),
		OutboxBackend:      backend,
		OutboxPollInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	repositoryID, issueID := seedProjection(t, service.database.db)
	appendTestWorkpadEvent(t, service, repositoryID, issueID, "blocking-key", "coding")
	select {
	case <-backend.started:
	case <-time.After(2 * time.Second):
		service.Close()
		t.Fatal("outbox worker did not start delivery")
	}

	closed := make(chan error, 1)
	go func() {
		closed <- service.Close()
	}()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not cancel and drain the outbox worker")
	}
}

type recordingOutboxBackend struct {
	mu             sync.Mutex
	failures       []error
	verifyFailures []error
	deliveredKeys  []string
	seen           map[string]struct{}
	executeCalls   int
	verifyCalls    int
	effects        int
}

func (b *recordingOutboxBackend) Execute(_ context.Context, item OutboxItem) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.executeCalls++
	return b.record(item, &b.failures)
}

func (b *recordingOutboxBackend) VerifyAndExecute(_ context.Context, item OutboxItem) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.verifyCalls++
	return b.record(item, &b.verifyFailures)
}

func (b *recordingOutboxBackend) record(item OutboxItem, failures *[]error) error {
	b.deliveredKeys = append(b.deliveredKeys, item.IdempotencyKey)
	if len(*failures) > 0 {
		err := (*failures)[0]
		*failures = (*failures)[1:]
		if err != nil {
			return err
		}
	}
	if b.seen == nil {
		b.seen = make(map[string]struct{})
	}
	if _, exists := b.seen[item.IdempotencyKey]; !exists {
		b.seen[item.IdempotencyKey] = struct{}{}
		b.effects++
	}
	return nil
}

func (b *recordingOutboxBackend) keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.deliveredKeys...)
}

type blockingOutboxBackend struct {
	started chan struct{}
	once    sync.Once
}

func (b *blockingOutboxBackend) Execute(ctx context.Context, _ OutboxItem) error {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	return ctx.Err()
}

func (b *blockingOutboxBackend) VerifyAndExecute(ctx context.Context, item OutboxItem) error {
	return b.Execute(ctx, item)
}

func openManualOutboxService(t *testing.T, cfg Config, backend OutboxBackend) *Service {
	t.Helper()
	service := openTestService(t, cfg)
	service.config.OutboxBackend = backend
	return service
}

func appendTestWorkpadEvent(t *testing.T, service *Service, repositoryID, issueID int64, key, phase string) {
	t.Helper()
	_, err := service.AppendWorkEvent(t.Context(), WorkEventChange{
		IssueID: issueID,
		Kind:    "progress",
		Mutation: WorkpadMutation{
			IdempotencyKey: key,
			RepositoryID:   repositoryID,
			IssueID:        issueID,
			Phase:          phase,
			Body:           workpadHeadingForTest(phase),
		},
	})
	if err != nil {
		t.Fatalf("AppendWorkEvent() error = %v", err)
	}
}

func workpadHeadingForTest(body string) string {
	return "## Codex Workpad\n\n" + body
}

func insertWorkflowState(t *testing.T, db *sql.DB, repositoryID int64, name string) int64 {
	t.Helper()
	result, err := db.ExecContext(t.Context(), `
		INSERT INTO workflow_states (
			repository_id, github_node_id, source_name, detent_state,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		repositoryID, "WS_"+name, name, name, testTimestamp, testTimestamp,
	)
	if err != nil {
		t.Fatalf("insert workflow state: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("workflow state id: %v", err)
	}
	return id
}

func assertIssueWorkflowState(t *testing.T, db *sql.DB, issueID, want int64) {
	t.Helper()
	var got int64
	if err := db.QueryRowContext(t.Context(), "SELECT workflow_state_id FROM issues WHERE id = ?", issueID).Scan(&got); err != nil {
		t.Fatalf("query issue workflow state: %v", err)
	}
	if got != want {
		t.Fatalf("issue workflow state = %d, want %d", got, want)
	}
}

func assertOutboxCount(t *testing.T, db *sql.DB, key string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM github_outbox WHERE idempotency_key = ?", key).Scan(&got); err != nil {
		t.Fatalf("count outbox key %q: %v", key, err)
	}
	if got != want {
		t.Fatalf("outbox count for %q = %d, want %d", key, got, want)
	}
}

func assertTableCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var got int
	query := "SELECT COUNT(*) FROM " + table
	if err := db.QueryRowContext(t.Context(), query).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Fatalf("%s count = %d, want %d", table, got, want)
	}
}

func assertOutboxStatus(t *testing.T, db *sql.DB, key, wantStatus string, wantAttempts int) {
	t.Helper()
	var status string
	var attempts int
	if err := db.QueryRowContext(t.Context(), "SELECT status, attempts FROM github_outbox WHERE idempotency_key = ?", key).Scan(&status, &attempts); err != nil {
		t.Fatalf("query outbox status for %q: %v", key, err)
	}
	if status != wantStatus || attempts != wantAttempts {
		t.Fatalf("outbox %q = status %q attempts %d, want %q %d", key, status, attempts, wantStatus, wantAttempts)
	}
}

func assertOutboxRetryAt(t *testing.T, db *sql.DB, key string, want time.Time, wantAttempts int) {
	t.Helper()
	var retryAt string
	var attempts int
	if err := db.QueryRowContext(t.Context(), "SELECT next_retry_at, attempts FROM github_outbox WHERE idempotency_key = ?", key).Scan(&retryAt, &attempts); err != nil {
		t.Fatalf("query outbox retry for %q: %v", key, err)
	}
	got, err := parseOutboxTime(retryAt)
	if err != nil {
		t.Fatalf("parse outbox retry for %q: %v", key, err)
	}
	if !got.Equal(want) || attempts != wantAttempts {
		t.Fatalf("outbox %q retry = %s attempts %d, want %s %d", key, got, attempts, want, wantAttempts)
	}
}
