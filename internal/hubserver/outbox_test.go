package hubserver

import (
	"context"
	"sync"
)

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

type githubOutboundSignal struct {
	calls chan OutboxItem
}

func (s githubOutboundSignal) Execute(ctx context.Context, item OutboxItem) error {
	select {
	case s.calls <- item:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s githubOutboundSignal) VerifyAndExecute(ctx context.Context, item OutboxItem) error {
	return s.Execute(ctx, item)
}
