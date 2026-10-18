package web

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRequestKanbanRefreshRetryScheduling(t *testing.T) {
	t.Parallel()

	errRefresh := errors.New("kanban refresh failed")
	retryAt := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		results    []kanbanRefreshRetryResult
		wantTimers int
		wantLog    string
	}{
		{
			name: "error schedules exactly one retry",
			results: []kanbanRefreshRetryResult{
				{err: errRefresh},
				{err: errRefresh},
			},
			wantTimers: 1,
			wantLog:    "kanban refresh request failed",
		},
		{
			name: "refused does not retry",
			results: []kanbanRefreshRetryResult{
				{response: RefreshResponse{Refused: true, RetryAt: &retryAt}},
			},
			wantLog: "kanban refresh request refused",
		},
		{
			name: "queued does not retry",
			results: []kanbanRefreshRetryResult{
				{response: RefreshResponse{Queued: true}},
			},
		},
		{
			name: "coalesced does not retry",
			results: []kanbanRefreshRetryResult{
				{response: RefreshResponse{Coalesced: true}},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logs bytes.Buffer
			refresher := &kanbanRefreshRetryProbe{results: tt.results}
			afterFunc := &fakeKanbanAfterFunc{}
			server := &Server{
				refresher: refresher,
				logger:    slog.New(slog.NewTextHandler(&logs, nil)),
				afterFunc: afterFunc.afterFunc,
			}

			server.requestKanbanRefresh(context.Background())

			if got := afterFunc.count(); got != tt.wantTimers {
				t.Fatalf("scheduled timers = %d, want %d", got, tt.wantTimers)
			}
			if got := refresher.calls(); got != 1 {
				t.Fatalf("refresher calls after request = %d, want 1", got)
			}
			if tt.wantTimers == 1 {
				if got := afterFunc.delay(0); got != kanbanRefreshRetryDelay {
					t.Fatalf("retry delay = %v, want %v", got, kanbanRefreshRetryDelay)
				}
				afterFunc.run(t, 0)
				if got := refresher.calls(); got != 2 {
					t.Fatalf("refresher calls after retry = %d, want 2", got)
				}
				if got := afterFunc.count(); got != 1 {
					t.Fatalf("scheduled timers after retry = %d, want 1", got)
				}
			}
			if tt.wantLog != "" && !strings.Contains(logs.String(), tt.wantLog) {
				t.Fatalf("log output %q does not contain %q", logs.String(), tt.wantLog)
			}
			if tt.name == "refused does not retry" && !strings.Contains(logs.String(), "retry_at") {
				t.Fatalf("log output %q does not contain retry_at", logs.String())
			}
		})
	}
}

func TestRequestKanbanRefreshRetryUsesWithoutCancel(t *testing.T) {
	t.Parallel()

	errRefresh := errors.New("kanban refresh failed")
	refresher := &kanbanRefreshRetryProbe{
		results: []kanbanRefreshRetryResult{
			{err: errRefresh},
			{response: RefreshResponse{Queued: true}},
		},
	}
	afterFunc := &fakeKanbanAfterFunc{}
	server := &Server{
		refresher: refresher,
		logger:    slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		afterFunc: afterFunc.afterFunc,
	}
	type contextKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "value"))

	server.requestKanbanRefresh(ctx)
	cancel()
	afterFunc.run(t, 0)

	contexts := refresher.capturedContexts()
	if len(contexts) != 2 {
		t.Fatalf("refresher contexts = %d, want 2", len(contexts))
	}
	select {
	case <-contexts[1].Done():
		t.Fatal("retry context was canceled with original request context")
	default:
	}
	if got := contexts[1].Value(contextKey{}); got != "value" {
		t.Fatalf("retry context value = %v, want value", got)
	}
}

func TestRequestKanbanRefreshConcurrentFailuresShareRetry(t *testing.T) {
	t.Parallel()

	errRefresh := errors.New("kanban refresh failed")
	refresher := &kanbanRefreshRetryProbe{
		defaultResult: kanbanRefreshRetryResult{err: errRefresh},
	}
	afterFunc := &fakeKanbanAfterFunc{}
	server := &Server{
		refresher: refresher,
		logger:    slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		afterFunc: afterFunc.afterFunc,
	}

	const requests = 64
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(requests)
	for range requests {
		go func() {
			defer wg.Done()
			<-start
			server.requestKanbanRefresh(context.Background())
		}()
	}
	close(start)
	wg.Wait()

	if got := afterFunc.count(); got != 1 {
		t.Fatalf("scheduled timers = %d, want 1", got)
	}
	afterFunc.run(t, 0)
	if got := afterFunc.count(); got != 1 {
		t.Fatalf("scheduled timers after retry = %d, want 1", got)
	}
	if got := refresher.calls(); got != requests+1 {
		t.Fatalf("refresher calls = %d, want %d", got, requests+1)
	}
}

type kanbanRefreshRetryResult struct {
	response RefreshResponse
	err      error
}

type kanbanRefreshRetryProbe struct {
	mu            sync.Mutex
	results       []kanbanRefreshRetryResult
	defaultResult kanbanRefreshRetryResult
	contexts      []context.Context
	callCount     int
}

func (p *kanbanRefreshRetryProbe) RequestRefresh(ctx context.Context) (RefreshResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.callCount++
	p.contexts = append(p.contexts, ctx)
	index := p.callCount - 1
	if index < len(p.results) {
		result := p.results[index]
		return result.response, result.err
	}
	return p.defaultResult.response, p.defaultResult.err
}

func (p *kanbanRefreshRetryProbe) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.callCount
}

func (p *kanbanRefreshRetryProbe) capturedContexts() []context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]context.Context(nil), p.contexts...)
}

type fakeKanbanAfterFunc struct {
	mu    sync.Mutex
	calls []fakeKanbanAfterFuncCall
}

type fakeKanbanAfterFuncCall struct {
	delay time.Duration
	fn    func()
}

func (f *fakeKanbanAfterFunc) afterFunc(delay time.Duration, fn func()) *time.Timer {
	f.mu.Lock()
	f.calls = append(f.calls, fakeKanbanAfterFuncCall{delay: delay, fn: fn})
	f.mu.Unlock()
	return stoppedKanbanRefreshTimer()
}

func (f *fakeKanbanAfterFunc) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeKanbanAfterFunc) delay(index int) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[index].delay
}

func (f *fakeKanbanAfterFunc) run(t *testing.T, index int) {
	t.Helper()

	f.mu.Lock()
	if index >= len(f.calls) {
		f.mu.Unlock()
		t.Fatalf("timer index %d out of range", index)
	}
	fn := f.calls[index].fn
	f.mu.Unlock()
	fn()
}

func stoppedKanbanRefreshTimer() *time.Timer {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	return timer
}
