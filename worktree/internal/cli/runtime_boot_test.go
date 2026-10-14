package cli

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestRetryBootGitHubToken(t *testing.T) {
	for _, tt := range []struct {
		name     string
		failures int
		empty    bool
		cancel   bool
	}{
		{name: "ready"},
		{name: "keyring unavailable", failures: 7},
		{name: "empty token", failures: 2, empty: true},
		{name: "shutdown during wait", failures: 1, cancel: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			var delays []time.Duration
			token, err := retryBootGitHubToken(ctx, func(context.Context) (string, error) {
				calls++
				if calls <= tt.failures {
					if tt.empty {
						return "  ", nil
					}
					return "", errors.New("secret service unavailable")
				}
				return "test-token", nil
			}, func(ctx context.Context, delay time.Duration) error {
				delays = append(delays, delay)
				if tt.cancel {
					cancel()
					return ctx.Err()
				}
				return nil
			})
			if tt.cancel {
				if !errors.Is(err, context.Canceled) || calls != 1 {
					t.Fatalf("token=%q err=%v calls=%d", token, err, calls)
				}
				return
			}
			if err != nil || token != "test-token" || calls != tt.failures+1 {
				t.Fatalf("token=%q err=%v calls=%d", token, err, calls)
			}
			var want []time.Duration
			delay := 5 * time.Second
			for range tt.failures {
				want = append(want, delay)
				delay = min(delay*2, time.Minute)
			}
			if !reflect.DeepEqual(delays, want) {
				t.Fatalf("delays=%v want %v", delays, want)
			}
		})
	}
}

func TestWaitBootGitHubToken(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "elapsed", true: "canceled"}[canceled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			delay := time.Duration(0)
			if canceled {
				cancel()
				delay = time.Hour
			}
			err := waitBootGitHubToken(ctx, delay)
			if canceled && !errors.Is(err, context.Canceled) || !canceled && err != nil {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestBootRuntimeCredentialsCancellation(t *testing.T) {
	for _, tt := range []struct {
		name           string
		canceledBefore bool
	}{
		{name: "before resolution", canceledBefore: true},
		{name: "during resolution"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			opts := defaultOptions()
			opts.ghAuthToken = func(context.Context) (string, error) {
				calls++
				cancel()
				return "", errors.New("keyring unavailable")
			}
			if tt.canceledBefore {
				cancel()
			}
			_, err := resolveConfiguredGitHubToken(ctx, "gh", bootRuntimeDeps(opts))
			if !errors.Is(err, context.Canceled) || errors.Is(err, ErrGitHubAuth) {
				t.Fatalf("error=%v", err)
			}
			want := 1
			if tt.canceledBefore {
				want = 0
			}
			if calls != want {
				t.Fatalf("calls=%d want %d", calls, want)
			}
		})
	}
}
