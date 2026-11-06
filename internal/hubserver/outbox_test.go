package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestOutboxStepStopsAfterOneDeliveryOrRejection(t *testing.T) {
	failure := errors.New("delivery unavailable")
	for _, test := range []struct {
		name      string
		delivered bool
		failure   error
		canceled  bool
		github    int
	}{
		{name: "delivered", delivered: true},
		{name: "rejected", failure: failure},
		{name: "idle", github: 1},
		{name: "canceled", canceled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.canceled {
				cancel()
			}
			slack, github := 0, 0
			delivered, err := processOutboxStep(ctx, func(context.Context) (bool, error) {
				slack++
				return test.delivered, test.failure
			}, func(context.Context) (bool, error) {
				github++
				return true, nil
			})
			if github != test.github || test.canceled && slack != 0 || !test.canceled && slack != 1 {
				t.Fatalf("delivery budget: slack=%d github=%d", slack, github)
			}
			if test.canceled {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			} else if !errors.Is(err, test.failure) || delivered != (test.delivered || test.github > 0) {
				t.Fatalf("delivery outcome: delivered=%t err=%v", delivered, err)
			}
		})
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
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	returned chan struct{}
	once     sync.Once
}

func (b *blockingOutboxBackend) Execute(ctx context.Context, _ OutboxItem) error {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	close(b.canceled)
	<-b.release
	defer close(b.returned)
	return ctx.Err()
}

func (b *blockingOutboxBackend) VerifyAndExecute(ctx context.Context, item OutboxItem) error {
	return b.Execute(ctx, item)
}

func openManualOutboxService(t *testing.T, cfg Config, backend OutboxBackend) *Service {
	t.Helper()
	service := openTestService(t, cfg)
	service.outbox.stop()
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
