package hubclient

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSchedulerLeaseHoldFollowsNativeClaims(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		steps        []string
		acquireErr   error
		wantAcquired int
		wantReleased int
		wantHeld     bool
	}{
		{name: "first claim acquires once", steps: []string{"add:a", "add:b"}, wantAcquired: 1, wantHeld: true},
		{name: "hold survives until the last claim leaves", steps: []string{"add:a", "add:b", "del:a"}, wantAcquired: 1, wantHeld: true},
		{name: "last release drops the hold", steps: []string{"add:a", "del:a"}, wantAcquired: 1, wantReleased: 1},
		{name: "claim lost then regained reacquires", steps: []string{"add:a", "del:a", "add:a"}, wantAcquired: 2, wantReleased: 1, wantHeld: true},
		{name: "acquire failure is retried on the next claim", steps: []string{"add:a", "add:b"}, acquireErr: errors.New("socket"), wantAcquired: 2},
		{name: "no claims never acquires", steps: []string{"del:a"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			acquired, released := 0, 0
			s := &Scheduler{nativeClaims: map[string]nativeClaim{}, claims: map[string]tracker.Lease{}, claimPolicies: map[string]claimPolicy{}}
			s.leaseHold = func(context.Context) (func(), error) {
				acquired++
				if tt.acquireErr != nil {
					return nil, tt.acquireErr
				}
				return func() { released++ }, nil
			}
			for _, step := range tt.steps {
				id := step[4:]
				s.mu.Lock()
				if step[:3] == "add" {
					s.nativeClaims[id] = nativeClaim{}
				} else {
					delete(s.nativeClaims, id)
				}
				s.mu.Unlock()
				s.syncLeaseHold(t.Context())
			}
			if acquired != tt.wantAcquired || released != tt.wantReleased || (s.leaseHoldRelease != nil) != tt.wantHeld {
				t.Fatalf("acquired=%d released=%d held=%t, want %d %d %t", acquired, released, s.leaseHoldRelease != nil, tt.wantAcquired, tt.wantReleased, tt.wantHeld)
			}
		})
	}
}

func TestSchedulerLeaseHoldReleasesWhenClaimsVanishDuringAcquire(t *testing.T) {
	t.Parallel()
	released := 0
	s := &Scheduler{nativeClaims: map[string]nativeClaim{"a": {}}}
	s.leaseHold = func(context.Context) (func(), error) {
		s.mu.Lock()
		delete(s.nativeClaims, "a")
		s.mu.Unlock()
		return func() { released++ }, nil
	}
	s.syncLeaseHold(t.Context())
	if released != 1 || s.leaseHoldRelease != nil {
		t.Fatalf("released=%d held=%t, want 1 false", released, s.leaseHoldRelease != nil)
	}
}
