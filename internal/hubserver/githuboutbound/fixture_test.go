package githuboutbound

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
)

func outboundDeps() Deps {
	return Deps{Now: time.Now, Jitter: func() float64 { return 1 }, NewLeaseID: func() string { return hubapi.NewID("lease") },
		OutboxPollInterval: time.Second, OutboxProcessingTimeout: 5 * time.Minute, OutboxBaseBackoff: 2 * time.Second, OutboxMaxBackoff: 15 * time.Minute,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Disabled: errors.New("hub github outbox is disabled"), Unavailable: errors.New("configured Hub feature is unavailable with PostgreSQL storage"),
		SplitRepository: func(fullName string) (string, string, bool) {
			parts := strings.Split(strings.TrimSpace(fullName), "/")
			if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
				return "", "", false
			}
			return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), true
		},
		NewTimer: func(delay time.Duration) Timer {
			timer := time.NewTimer(delay)
			return Timer{Ticks: timer.C, Reset: func(next time.Duration) { timer.Reset(next) }, Stop: func() { timer.Stop() }}
		},
	}
}
func outboundFixture(t *testing.T) (*Service, *time.Time, struct{}) {
	t.Helper()
	deps := outboundDeps()
	deps.Repository, deps.Details = hubtest.Repository(t), hubtest.NewDetails()
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	deps.Now = func() time.Time { return now }
	deps.Organization = "org"
	hubtest.Organization(t, deps.Repository, hubtest.OrganizationOptions{ID: deps.Organization})
	hubtest.Project(t, deps.Repository, deps.Organization, hubtest.ProjectOptions{ID: "first"})
	hubtest.Project(t, deps.Repository, deps.Organization, hubtest.ProjectOptions{ID: "second"})
	return New(deps), &now, struct{}{}
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

func (b *recordingOutboxBackend) Execute(_ context.Context, item githubcontract.OutboxItem) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.executeCalls++
	return b.record(item, &b.failures)
}

func (b *recordingOutboxBackend) VerifyAndExecute(_ context.Context, item githubcontract.OutboxItem) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.verifyCalls++
	return b.record(item, &b.verifyFailures)
}

func (b *recordingOutboxBackend) record(item githubcontract.OutboxItem, failures *[]error) error {
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
