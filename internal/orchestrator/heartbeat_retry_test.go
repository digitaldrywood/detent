package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
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

type retryClaimHeartbeat struct {
	claimCleanupScheduling
	renewals int
}

func (s *retryClaimHeartbeat) RenewClaimHeartbeat(_ context.Context, _ string, now time.Time) (Claimed, error) {
	s.renewals++
	return Claimed{LeaseRenewedAt: now, LocalLeaseDeadline: now.Add(2 * time.Minute)}, nil
}

func TestHeartbeatWriteRetryErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		failures int
		expires  bool
		retired  bool
	}{
		{name: "transient persistence failure", err: errors.New("temporarily unavailable"), failures: 7},
		{name: "expired before retry", err: context.DeadlineExceeded, failures: 1, expires: true},
		{name: "retired attempt", err: store.ErrNotFound, failures: 1, retired: true},
		{name: "terminal attempt", err: store.ErrWorkAttemptTerminal, failures: 1, retired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			const interval = 10 * time.Second
			backend := &errorHeartbeatStore{err: tc.err}
			cfg := Config{Claiming: ClaimingConfig{LeaseTTL: time.Minute, HeartbeatInterval: interval}}
			source := &retryClaimHeartbeat{}
			manager := newHeartbeatManager(cfg, nil, backend, func() time.Time { return now }, nil, source)
			expires := now.Add(time.Minute)
			issue := connector.Issue{ID: "worker", State: "In Progress"}
			target := heartbeatTarget{issueID: issue.ID, localLeaseDeadline: now.Add(2 * time.Minute), workAttemptHeartbeat: store.WorkAttemptHeartbeat{AttemptID: 6004, LeaseExpiresAt: expires}}
			manager.upsert(target)
			state := newState(cfg)
			cancelled := false
			state.Running[issue.ID] = Running{Issue: issue, WorkAttemptID: 6004, cancel: func() { cancelled = true }}
			state.Claimed[issue.ID] = Claimed{Issue: issue, LocalLeaseDeadline: target.localLeaseDeadline}
			orch := &Orchestrator{cfg: cfg, scheduling: source, heartbeats: manager}
			checkRetired := func(result heartbeatResult) {
				t.Helper()
				manager.upsert(target)
				now = now.Add(interval)
				for _, due := range manager.due(now) {
					manager.execute(t.Context(), due)
				}
				if source.renewals != tc.failures || backend.calls != tc.failures {
					t.Fatalf("retired attempt: claim renewals=%d attempt writes=%d, want %d of each", source.renewals, backend.calls, tc.failures)
				}
				if !result.claimOwnerLost || result.claimRenewed {
					t.Fatalf("retirement result: lost=%t renewed=%t", result.claimOwnerLost, result.claimRenewed)
				}
				orch.handleHeartbeatResult(&state, result)
				if !cancelled || len(source.forgotten) != 1 || source.forgotten[0] != issue.ID ||
					knownDispatchWaitReason(issue, &state, nil, nil) != "" {
					t.Fatalf("retired attempt kept local dispatch ownership: canceled=%t claims=%d running=%d forgotten=%v", cancelled, len(state.Claimed), len(state.Running), source.forgotten)
				}
			}
			now = now.Add(interval)
			for i := range tc.failures {
				due := manager.due(now)
				if len(due) != 1 {
					t.Fatalf("due=%v", due)
				}
				manager.execute(t.Context(), due[0])
				result := <-manager.results
				if !errors.Is(result.workAttemptError, tc.err) || backend.calls != i+1 {
					t.Fatalf("calls=%d write error=%v, want %d calls and %v", backend.calls, result.workAttemptError, i+1, tc.err)
				}
				if tc.retired {
					checkRetired(result)
					return
				}
				manager.upsert(target)
				delay := manager.nextDelay(now)
				maximum := min(time.Second<<i, interval, expires.Sub(now)/3)
				if delay < maximum-maximum/8 || delay > maximum {
					t.Fatalf("failure %d delay=%s maximum=%s", i+1, delay, maximum)
				}
				now = now.Add(delay)
			}
			if tc.expires {
				now = expires
				due := manager.due(now)
				if len(due) != 1 {
					t.Fatalf("due=%v", due)
				}
				manager.execute(t.Context(), due[0])
				result := <-manager.results
				if !errors.Is(result.workAttemptError, store.ErrNotFound) {
					t.Fatalf("expired attempt: calls=%d write error=%v", backend.calls, result.workAttemptError)
				}
				checkRetired(result)
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
