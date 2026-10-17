package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/orchestrator"
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

func TestSchedulerLeaseHoldOutlivesReleasedLease(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		operation    string
		wantReleased int
	}{
		{name: "released lease keeps the hold until the next empty fetch", operation: "release", wantReleased: 1},
		{name: "lost lease keeps the hold until the next empty fetch", operation: "lost", wantReleased: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			descriptor := clientTestPolicy()
			id := "wi_" + strings.Repeat("b", 32)
			lease := tracker.NativeLease{ID: "lease", WorkItemID: tracker.NativeWorkItemID(id), FencingToken: 1, PolicyID: descriptor.ID, ServerTime: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}
			transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				status, body := http.StatusOK, `{}`
				switch {
				case strings.HasSuffix(request.URL.Path, "/claims"):
					status, body = http.StatusConflict, `{"code":"no_claimable_work","message":"No work"}`
				case strings.HasSuffix(request.URL.Path, "/release"):
					encoded, err := json.Marshal(lease)
					if err != nil {
						return nil, err
					}
					body = string(encoded)
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			})
			client, err := New(Config{URL: "http://hub.example", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			acquired, released := 0, 0
			scheduler, err := NewScheduler(client, SchedulerConfig{
				Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute,
				LeaseHold: func(context.Context) (func(), error) {
					acquired++
					return func() { released++ }, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_test", "prj_test")
			if err != nil {
				t.Fatal(err)
			}
			source := &NativeConnector{client: native}
			scheduler.nativeProjects["project"] = source
			scheduler.nativeHeartbeats[native.project] = time.Now()
			scheduler.nativeClaims[id] = nativeClaim{source: source, lease: lease}
			scheduler.claims[id] = nativeTrackerLease(lease)
			scheduler.claimPolicies[id] = claimPolicy{project: "project", descriptor: descriptor}
			scheduler.syncLeaseHold(t.Context())
			if acquired != 1 || released != 0 {
				t.Fatalf("after claim: acquired=%d released=%d", acquired, released)
			}
			switch tt.operation {
			case "release":
				if err := scheduler.ReleaseClaim(t.Context(), id, "completed"); err != nil {
					t.Fatal(err)
				}
			case "lost":
				err := scheduler.nativeClaimError(id, lease.FencingToken, &APIError{Status: http.StatusConflict, Code: "stale_fencing_token"})
				if !errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
					t.Fatalf("claim loss = %v", err)
				}
			}
			if len(scheduler.nativeClaims) != 0 || released != 0 || scheduler.leaseHoldRelease == nil {
				t.Fatalf("after %s: claims=%d released=%d held=%t, want the hold kept with no lease", tt.operation, len(scheduler.nativeClaims), released, scheduler.leaseHoldRelease != nil)
			}
			issues, err := scheduler.fetchNativeCandidate(t.Context(), orchestrator.SchedulingRequest{ProjectID: "project", Policy: descriptor, AdmissionLimit: 1}, source)
			if err != nil || len(issues) != 0 {
				t.Fatalf("fetchNativeCandidate() = %#v, %v", issues, err)
			}
			if acquired != 1 || released != tt.wantReleased || scheduler.leaseHoldRelease != nil {
				t.Fatalf("after empty fetch: acquired=%d released=%d held=%t", acquired, released, scheduler.leaseHoldRelease != nil)
			}
		})
	}
}
