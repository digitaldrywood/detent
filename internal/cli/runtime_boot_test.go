package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
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

func TestBootRuntimeCredentials(t *testing.T) {
	for _, tt := range []struct {
		name           string
		canceledBefore bool
		cancelDuring   bool
		deferred       bool
		token          string
		resolveErr     error
	}{
		{name: "before resolution", canceledBefore: true},
		{name: "during resolution", cancelDuring: true},
		{name: "deferred before resolution", deferred: true, canceledBefore: true},
		{name: "deferred during resolution", deferred: true, cancelDuring: true},
		{name: "deferred missing gh", deferred: true, resolveErr: errors.New("gh was not found on PATH")},
		{name: "deferred failed gh login", deferred: true, resolveErr: errors.New("not logged in")},
		{name: "deferred empty token", deferred: true, token: "  "},
		{name: "deferred configured token", deferred: true, token: "configured-token"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				opts := defaultOptions()
				opts.lookupEnv = mapLookup(nil)
				opts.ghAuthToken = func(context.Context) (string, error) {
					calls++
					if calls > 1 {
						t.Fatal("deferred startup retried optional GitHub authentication")
					}
					if tt.cancelDuring {
						cancel()
					}
					return tt.token, tt.resolveErr
				}
				if tt.canceledBefore {
					cancel()
				}
				deps := bootRuntimeDeps(opts)
				deps.loadWorkflow = func(string) (workflowconfig.Workflow, error) {
					if tt.deferred {
						t.Fatal("deferred startup requested project authentication")
					}
					return workflowconfig.Workflow{Config: githubWorkflow("")}, nil
				}
				cfg := githubRuntimeConfig("gh")
				got, err := resolveRuntimeSettings(ctx, runtimeInput{
					Config: &cfg, DeferProjectValidation: tt.deferred,
					Flags: runtimeFlags{Port: runtimeIntFlag{Set: true}},
				}, deps)
				if tt.canceledBefore || tt.cancelDuring {
					if !errors.Is(err, context.Canceled) || errors.Is(err, ErrGitHubAuth) {
						t.Fatalf("error=%v", err)
					}
				} else if err != nil || got.GitHubToken.Value != strings.TrimSpace(tt.token) {
					t.Fatalf("credential resolution failed: error=%v, token present=%v", err, got.GitHubToken.Value != "")
				}
				want := 1
				if tt.canceledBefore {
					want = 0
				}
				if calls != want {
					t.Fatalf("calls=%d want %d", calls, want)
				}
			})
		})
	}
}
