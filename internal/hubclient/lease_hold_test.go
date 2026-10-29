package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
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

func TestSchedulerLeaseHoldSpansCandidateFetch(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name             string
		noWork           bool
		hydrationFailure bool
	}{
		{name: "claim through execution"},
		{name: "empty fetch", noWork: true},
		{name: "hydration failure", hydrationFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newNativeChangeHub(t, true)
			var item tracker.NativeWorkItemID
			if !test.noWork {
				item = tracker.NativeWorkItemID(h.createInProgress(t, "Native execution").ID)
			}
			var held, acquired atomic.Int64
			h.scheduler.leaseHold = func(context.Context) (func(), error) {
				acquired.Add(1)
				held.Add(1)
				return func() { held.Add(-1) }, nil
			}
			transport := h.native.client.httpClient.Transport
			var claims, recoveries atomic.Int64
			h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				claim := request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/claims")
				recovery := request.URL.Query().Get("view") == "recovery"
				if claim || recovery {
					if held.Load() != 1 {
						t.Errorf("%s %s has no scheduler hold", request.Method, request.URL.Path)
					}
					if claim {
						claims.Add(1)
					} else {
						recoveries.Add(1)
						if test.hydrationFailure {
							return nil, errors.New("recovery unavailable")
						}
					}
				}
				return transport.RoundTrip(request)
			})
			request := orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, AdmissionLimit: 1, WorkflowStates: []string{"Todo", "In Progress"}}
			issues, err := h.scheduler.FetchCandidateIssues(t.Context(), request)
			if (err != nil) != test.hydrationFailure || claims.Load() == 0 {
				t.Fatalf("fetch: issues=%+v error=%v claims=%d", issues, err, claims.Load())
			}
			if test.noWork || test.hydrationFailure {
				if len(issues) != 0 || held.Load() != 0 {
					t.Fatalf("idle fetch retained issues=%+v holds=%d", issues, held.Load())
				}
				if test.hydrationFailure {
					h.native.client.httpClient.Transport = transport
					lease, err := h.native.Claim(t.Context(), tracker.NativeClaim{PolicyID: h.descriptor.ID, WorkItemID: item, MachineID: h.scheduler.machine.ID, SessionID: "after-hydration-failure", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
					if err != nil {
						t.Fatalf("hydration failure retained the old claim: %v", err)
					}
					if err := h.native.Release(t.Context(), lease, "cancelled"); err != nil {
						t.Fatal(err)
					}
				}
				return
			}
			if len(issues) != 1 || recoveries.Load() == 0 || held.Load() != 1 {
				t.Fatalf("claimed fetch: issues=%+v recoveries=%d holds=%d", issues, recoveries.Load(), held.Load())
			}
			execution := h.scheduler.RunExecution(string(item)).(*nativeExecution)
			if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			if held.Load() != 1 {
				t.Fatal("execution lost its scheduler hold")
			}
			if err := execution.Finish(t.Context(), "failed"); err != nil {
				t.Fatal(err)
			}
			if err := h.scheduler.ReleaseClaim(t.Context(), string(item), "orchestrator_release"); err != nil {
				t.Fatal(err)
			}
			if held.Load() != 1 || acquired.Load() != 1 {
				t.Fatal("completion dropped the hold before the next fetch")
			}
			current, err := h.admin.Issue(t.Context(), item)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: "Done", Reason: "user_requested"}); err != nil {
				t.Fatal(err)
			}
			issues, err = h.scheduler.FetchCandidateIssues(t.Context(), request)
			if err != nil || len(issues) != 0 || held.Load() != 0 || acquired.Load() != 1 {
				t.Fatalf("completed execution's empty fetch: issues=%+v holds=%d acquired=%d error=%v", issues, held.Load(), acquired.Load(), err)
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

func TestSchedulerLeaseHoldConcurrentFetches(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		started, ready := make(chan struct{}), make(chan struct{})
		acquired, released := 0, 0
		s := &Scheduler{nativeFetches: 2}
		s.leaseHold = func(context.Context) (func(), error) {
			acquired++
			close(started)
			<-ready
			return func() { released++ }, nil
		}
		done := make(chan struct{}, 2)
		go func() { s.syncLeaseHold(t.Context()); done <- struct{}{} }()
		<-started
		go func() { s.syncLeaseHold(t.Context()); done <- struct{}{} }()
		synctest.Wait()
		if len(done) != 0 {
			t.Fatal("candidate fetch proceeded before its task was acquired")
		}
		close(ready)
		<-done
		<-done
		s.mu.Lock()
		s.nativeFetches--
		s.mu.Unlock()
		s.syncLeaseHold(t.Context())
		if acquired != 1 || released != 0 || s.leaseHoldRelease == nil {
			t.Fatal("first empty fetch dropped the other fetch's hold")
		}
		s.mu.Lock()
		s.nativeFetches--
		s.mu.Unlock()
		s.syncLeaseHold(t.Context())
		if released != 1 || s.leaseHoldRelease != nil {
			t.Fatal("last empty fetch did not release its task")
		}
	})
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
			releaseRequests := 0
			transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				status, body := http.StatusOK, `{}`
				switch {
				case strings.HasSuffix(request.URL.Path, "/claims"):
					status, body = http.StatusConflict, `{"code":"no_claimable_work","message":"No work"}`
				case strings.HasSuffix(request.URL.Path, "/release"):
					releaseRequests++
					var mutation tracker.NativeLeaseMutation
					if err := json.NewDecoder(request.Body).Decode(&mutation); err != nil {
						t.Fatal(err)
					}
					wantReason := "released"
					if tt.operation == "lost" {
						wantReason = "cancelled"
					}
					if !strings.HasSuffix(request.URL.Path, "/leases/lease/release") || mutation.FencingToken != lease.FencingToken || mutation.Reason != wantReason || mutation.CancelAttempt != (tt.operation == "lost") || !time.Now().Before(lease.ExpiresAt) {
						t.Fatalf("release lost exact lease authority or deadline: path=%s mutation=%+v", request.URL.Path, mutation)
					}
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
				if releaseRequests != 1 {
					t.Fatalf("release requests=%d, want 1 before lease expiry", releaseRequests)
				}
			case "lost":
				err := scheduler.nativeClaimError(t.Context(), id, lease.FencingToken, &APIError{Status: http.StatusConflict, Code: "stale_fencing_token"})
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
