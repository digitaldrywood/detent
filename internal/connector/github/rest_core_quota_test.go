package github

import (
	"errors"
	"testing"
	"time"
)

func TestRESTCoreQuotaPreservesReservationsUntilReset(t *testing.T) {
	t.Parallel()
	const key = "example-credential"
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	registry := newRESTBackoffRegistry()
	if probe, err := registry.chargeCoreQuota(key, now, 1, true); err != nil || !probe {
		t.Fatalf("startup quota probe = %t, %v", probe, err)
	}
	registry.observeCoreQuota(key, 3, reset)
	for _, test := range []struct {
		name     string
		seen     int64
		resetAt  time.Time
		observed time.Time
		reserve  int64
		wantErr  error
	}{
		{"first verification", 3, reset, now, 1, nil},
		{"second verification", 3, reset, now, 1, nil},
		{"same window does not replenish quota", 3, reset, now, 1, ErrRESTBudgetReserved},
		{"older response does not replenish quota", 3, reset.Add(-time.Minute), now, 1, ErrRESTBudgetReserved},
		{"reserved unit is available to the outbox", 3, reset, now, 0, nil},
		{"new window replenishes quota", 3, reset.Add(time.Hour), reset, 1, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry.observeCoreQuota(key, test.seen, test.resetAt)
			if probe, err := registry.chargeCoreQuota(key, test.observed, test.reserve, true); probe || !errors.Is(err, test.wantErr) {
				t.Fatalf("quota probe = %t, error = %v, want no probe and error %v", probe, err, test.wantErr)
			}
		})
	}
}
