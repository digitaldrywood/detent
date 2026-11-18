package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
)

type errorHeartbeatStore struct {
	store.WorkAttemptStore
	calls    int
	err      error
	recorded store.WorkAttemptHeartbeat
}

func (s *errorHeartbeatStore) RecordWorkAttemptHeartbeat(_ context.Context, heartbeat store.WorkAttemptHeartbeat) error {
	s.calls++
	if s.err == nil {
		s.recorded = heartbeat
	}
	return s.err
}

func TestHeartbeatWriteRetryErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		failures int
	}{
		{"transient persistence failure", errors.New("temporarily unavailable"), 7},
		{"expired before retry", context.DeadlineExceeded, 1},
		{"retired attempt", store.ErrNotFound, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			const interval = 10 * time.Second
			backend := &errorHeartbeatStore{err: tc.err}
			manager := newHeartbeatManager(Config{Claiming: ClaimingConfig{LeaseTTL: time.Minute, HeartbeatInterval: interval}}, nil, backend, func() time.Time { return now }, nil)
			expires := now.Add(time.Minute)
			target := heartbeatTarget{issueID: "worker", workAttemptHeartbeat: store.WorkAttemptHeartbeat{AttemptID: 6004, LeaseExpiresAt: expires}}
			manager.upsert(target)
			now = now.Add(interval)
			for i := range tc.failures {
				due := manager.due(now)
				if len(due) != 1 {
					t.Fatalf("due=%v", due)
				}
				manager.execute(t.Context(), due[0])
				if result := <-manager.results; !errors.Is(result.workAttemptError, tc.err) || backend.calls != i+1 {
					t.Fatalf("calls=%d result=%+v", backend.calls, result)
				}
				manager.upsert(target)
				delay := manager.nextDelay(now)
				if errors.Is(tc.err, store.ErrNotFound) {
					if delay != interval {
						t.Fatalf("retired attempt retry in %s", delay)
					}
					return
				}
				maximum := min(time.Second<<i, interval, expires.Sub(now)/2)
				if delay < maximum-maximum/8 || delay > maximum {
					t.Fatalf("failure %d delay=%s maximum=%s", i+1, delay, maximum)
				}
				now = now.Add(delay)
			}
			if tc.name == "expired before retry" {
				now = expires
				due := manager.due(now)
				if len(due) != 1 {
					t.Fatalf("due=%v", due)
				}
				manager.execute(t.Context(), due[0])
				if result := <-manager.results; !result.claimOwnerLost || backend.calls != tc.failures {
					t.Fatalf("expired lease renewed: calls=%d result=%+v", backend.calls, result)
				}
				return
			}
			backend.err = nil
			due := manager.due(now)
			if len(due) != 1 {
				t.Fatalf("due=%v", due)
			}
			manager.execute(t.Context(), due[0])
			result := <-manager.results
			if !result.workAttemptRenewed || !now.Before(expires) || !backend.recorded.LeaseExpiresAt.After(expires) || manager.nextDelay(now) != interval {
				t.Fatalf("renewal=%+v at %s original expiry %s", result, now, expires)
			}
		})
	}
}
