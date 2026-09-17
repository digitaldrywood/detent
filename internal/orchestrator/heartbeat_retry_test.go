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
	first    error
	calls    int
	cancel   context.CancelFunc
	recorded store.WorkAttemptHeartbeat
}

func (s *errorHeartbeatStore) RecordWorkAttemptHeartbeat(_ context.Context, heartbeat store.WorkAttemptHeartbeat) error {
	s.calls++
	if s.calls == 1 {
		if s.cancel != nil {
			s.cancel()
		}
		return s.first
	}
	s.recorded = heartbeat
	return nil
}

func TestHeartbeatWriteRetryErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		first     error
		cancel    bool
		wantCalls int
		wantErr   error
	}{
		{"failed write retries", errors.New("temporary write failure"), false, 2, nil},
		{"cancellation does not retry", context.Canceled, false, 1, context.Canceled},
		{"shutdown does not retry", context.DeadlineExceeded, true, 1, context.Canceled},
		{"missing attempt does not retry", store.ErrNotFound, false, 1, store.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), heartbeatOperationTimeout)
			defer cancel()
			backend := &errorHeartbeatStore{first: tc.first}
			if tc.cancel {
				backend.cancel = cancel
			}
			now := time.Now().UTC()
			manager := newHeartbeatManager(Config{}, nil, backend, func() time.Time { return now }, nil)
			got, err := manager.persistHeartbeat(ctx, heartbeatTarget{workAttemptHeartbeat: store.WorkAttemptHeartbeat{AttemptID: 6004}}, manager.settingsSnapshot(), now)
			if !errors.Is(err, tc.wantErr) || backend.calls != tc.wantCalls {
				t.Fatalf("calls=%d err=%v", backend.calls, err)
			}
			if err == nil && (backend.recorded.AttemptID != 6004 || !backend.recorded.HeartbeatAt.Equal(now) || !got.LeaseExpiresAt.After(now)) {
				t.Fatalf("heartbeat not persisted: %+v", backend.recorded)
			}
		})
	}
}
