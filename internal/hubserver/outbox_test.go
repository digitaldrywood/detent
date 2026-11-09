package hubserver

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestOutboxStepAdvancesBothDeliveryOwners(t *testing.T) {
	failure := errors.New("delivery unavailable")
	for _, test := range []struct {
		name      string
		delivered bool
		failure   error
		canceled  bool
		github    int
	}{
		{name: "delivered", delivered: true, github: 1},
		{name: "rejected", failure: failure, github: 1},
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
