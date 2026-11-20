package codex

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/digitaldrywood/detent/internal/runner"
)

func TestReviewUnretiredToolAfterProtocolFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		primary error
		joined  bool
	}{
		{name: "protocol", primary: ErrInvalidResponse},
		{name: "handler", primary: errors.New("update handler failed")},
		{name: "operator stop", primary: runner.ErrOperatorStopped},
		{name: "review deadline", primary: runner.ErrTurnDurationExceeded},
		{name: "completed stream"},
		{name: "retired after protocol failure", primary: ErrInvalidResponse, joined: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, cancel := context.WithCancelCause(t.Context())
				done := make(chan struct{})
				if tc.joined {
					close(done)
				}
				transport := &toolTransport{cancel: cancel, done: done, unresolved: true}
				err := transport.finish(tc.primary)
				if tc.primary != nil && !errors.Is(err, tc.primary) {
					t.Fatalf("finish error = %v, want primary error %v", err, tc.primary)
				}
				if got := errors.Is(err, runner.ErrWorkerProcessReap); got == tc.joined {
					t.Fatalf("finish error = %v, joined = %v", err, tc.joined)
				}
				if runner.UnretiredProviderSession(err) == tc.joined {
					t.Fatalf("unexpected supervision fence for finish error %v, joined = %v", err, tc.joined)
				}
				wireErr := runner.NewSSHRunResponse(runner.RunResult{}, err).Err()
				if runner.UnretiredProviderSession(wireErr) == tc.joined {
					t.Fatalf("unexpected supervision fence over worker transport for %v, joined = %v", wireErr, tc.joined)
				}
			})
		})
	}
}
