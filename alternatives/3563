package testenv

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
)

func TestWaitContextInsideSynctestBubble(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		if ctx := WaitContext(t, t.Context()); ctx != t.Context() {
			t.Fatal("wait context inside a bubble must return its parent")
		}
	})
}

type recordingTB struct {
	failure string
}

func (r *recordingTB) Helper()        {}
func (r *recordingTB) Cleanup(func()) {}
func (r *recordingTB) Fatalf(format string, args ...any) {
	r.failure = fmt.Sprintf(format, args...)
}

func TestWaitUntil(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		canceled    bool
		passAfter   int
		wantCalls   int
		wantFailure string
	}{
		{name: "condition already holds", passAfter: 1, wantCalls: 1},
		{name: "condition holds after polling", passAfter: 3, wantCalls: 3},
		{name: "context ends first", canceled: true, passAfter: 100, wantCalls: 1, wantFailure: "not yet 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			if tt.canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			tb := &recordingTB{}
			calls := 0
			WaitUntil(tb, ctx, func() error {
				calls++
				if calls >= tt.passAfter {
					return nil
				}
				return fmt.Errorf("not yet %d", calls)
			})
			if calls != tt.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, tt.wantCalls)
			}
			if tt.wantFailure == "" && tb.failure != "" || !strings.Contains(tb.failure, tt.wantFailure) {
				t.Fatalf("failure = %q, want containing %q", tb.failure, tt.wantFailure)
			}
		})
	}
}

func TestWaitContextEndsWithTest(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	tb := &recordingTB{}
	ctx := WaitContext(t, parent)
	if _, hasDeadline := t.Deadline(); hasDeadline {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("wait context ignored the go test deadline")
		}
	}
	cancel()
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("wait context error = %v", ctx.Err())
	}
	if WaitContext(tb, parent) != parent {
		t.Fatal("wait context without a test deadline must return its parent")
	}
}
