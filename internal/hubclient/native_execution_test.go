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
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type executionTransport struct {
	next http.RoundTripper
	drop atomic.Bool
	down atomic.Bool
}

func (t *executionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.down.Load() {
		return nil, errors.New("injected Hub outage")
	}
	response, err := t.next.RoundTrip(request)
	if err == nil && strings.HasSuffix(request.URL.Path, "/events") && t.drop.Swap(false) {
		if closeErr := response.Body.Close(); closeErr != nil {
			return nil, closeErr
		}
		return nil, errors.New("injected acknowledgment loss after commit")
	}
	return response, err
}

func exerciseNativeExecution(t *testing.T, scheduler *Scheduler, native *NativeClient, issueID string) {
	t.Helper()
	transport := &executionTransport{next: native.client.httpClient.Transport}
	native.client.httpClient.Transport = transport
	t.Cleanup(func() { native.client.httpClient.Transport = transport.next })
	execution := scheduler.RunExecution(issueID)
	if execution == nil {
		t.Fatal("native claim has no execution lifecycle")
	}
	guarded, stop, err := execution.Guard(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if len(execution.Recovery().Discussion) != 23 {
		t.Fatal("native recovery omitted discussion")
	}
	identity := tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}
	checkpoint := tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "dirty", HeadSHA: strings.Repeat("a", 40), WorkspaceDigest: strings.Repeat("b", 64), ExternalEffect: "none", EffectState: "none"}
	for _, test := range []struct {
		name      string
		operation func() error
	}{
		{"start", func() error { return execution.Start(guarded, identity) }},
		{"checkpoint", func() error { return execution.Checkpoint(guarded, checkpoint) }},
		{"finish", func() error { return execution.Finish(guarded, "succeeded") }},
	} {
		t.Run("lost "+test.name+" acknowledgment", func(t *testing.T) {
			transport.drop.Store(true)
			if err := test.operation(); err == nil {
				t.Fatal("acknowledgment loss was not injected")
			}
			if err := test.operation(); err != nil {
				t.Fatal(err)
			}
			if err := test.operation(); err != nil {
				t.Fatal(err)
			}
		})
	}
	recovery, err := native.Recovery(t.Context(), tracker.NativeWorkItemID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.Attempts) != 1 || recovery.Attempts[0].Sequence != 3 || recovery.Attempts[0].Status != "succeeded" || recovery.Attempts[0].Checkpoint.WorktreeState != "dirty" {
		t.Fatalf("retries duplicated/lost progress: %#v", recovery.Attempts)
	}
	transport.down.Store(true)
	if err := execution.Validate(guarded); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
		t.Fatalf("outage validation = %v", err)
	}
	if !errors.Is(context.Cause(guarded), runner.ErrExecutionAuthorityUnavailable) {
		t.Fatal("outage did not cancel provider context")
	}
	transport.down.Store(false)
	if err := execution.Validate(guarded); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
		t.Fatal("stopped execution resumed after reconnect")
	}
}

type executionRoundTrip func(*http.Request) (*http.Response, error)

func (f executionRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestNativeGuardDeadlineAndRenewal(t *testing.T) {
	for _, renew := range []bool{false, true} {
		name := "expire"
		if renew {
			name = "renew"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				descriptor := clientTestPolicy()
				transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					body := []byte(`{}`)
					if strings.HasSuffix(request.URL.Path, "/policy") {
						var err error
						body, err = json.Marshal(policy.Approval{Policy: descriptor})
						if err != nil {
							return nil, err
						}
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
				})
				client, err := New(Config{URL: "http://hub.example", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: transport}})
				if err != nil {
					t.Fatal(err)
				}
				scheduler, err := NewScheduler(client, SchedulerConfig{Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native(tracker.OrganizationID("org_"+strings.Repeat("a", 32)), tracker.ProjectID("prj_"+strings.Repeat("b", 32)))
				if err != nil {
					t.Fatal(err)
				}
				source := &NativeConnector{client: native}
				id := "wi_" + strings.Repeat("c", 32)
				scheduler.nativeProjects["project"] = source
				scheduler.nativeClaims[id] = nativeClaim{source: source, lease: tracker.NativeLease{WorkItemID: tracker.NativeWorkItemID(id), ID: "lease", FencingToken: 1, PolicyID: descriptor.ID}, deadline: time.Now().Add(time.Minute)}
				scheduler.claimPolicies[id] = claimPolicy{project: "project", descriptor: descriptor}
				execution := scheduler.RunExecution(id)
				guarded, stop, err := execution.Guard(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				time.Sleep(30 * time.Second)
				if renew {
					scheduler.mu.Lock()
					claim := scheduler.nativeClaims[id]
					claim.deadline = time.Now().Add(time.Minute)
					scheduler.nativeClaims[id] = claim
					scheduler.mu.Unlock()
				}
				time.Sleep(25 * time.Second)
				synctest.Wait()
				if renew {
					if guarded.Err() != nil {
						t.Fatal("renewed owner stopped at old deadline")
					}
					time.Sleep(30 * time.Second)
					synctest.Wait()
				}
				if !errors.Is(context.Cause(guarded), runner.ErrExecutionAuthorityUnavailable) {
					t.Fatalf("expiry cause = %v", context.Cause(guarded))
				}
			})
		})
	}
}

func TestNativeLeaseReceiptDoesNotExtendReplayedClaim(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		server    time.Time
		remaining time.Duration
	}{
		{"fresh claim", started.Add(time.Hour), time.Minute},
		{"replayed claim", started.Add(time.Hour + 50*time.Second), 10 * time.Second},
		{"missing server time", time.Time{}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			lease := tracker.NativeLease{ServerTime: test.server, RenewedAt: started.Add(time.Hour), ExpiresAt: started.Add(time.Hour + time.Minute)}
			if got := nativeLeaseDeadline(started, lease); !got.Equal(started.Add(test.remaining)) {
				t.Fatalf("local deadline = %v", got)
			}
		})
	}
}

func TestNativeMissingClaimNeverRunsUnguarded(t *testing.T) {
	t.Parallel()
	scheduler := &Scheduler{nativeClaims: make(map[string]nativeClaim)}
	execution := scheduler.RunExecution("wi_" + strings.Repeat("a", 32))
	if execution == nil {
		t.Fatal("missing native claim disabled the execution guard")
	}
	_, stop, err := execution.Guard(t.Context())
	defer stop()
	if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
		t.Fatalf("guard = %v", err)
	}
	if scheduler.RunExecution("github-node") != nil {
		t.Fatal("legacy execution acquired a native guard")
	}
}

func TestNativeExecutionContextKeepsItsOriginalFence(t *testing.T) {
	t.Parallel()
	client := &Client{}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	old := tracker.NativeLease{ID: "old", WorkItemID: "wi_test", FencingToken: 1}
	current := old
	current.ID, current.FencingToken = "new", 2
	client.nativeLeases.Store(native.base()+"/wi_test", current)
	bound := context.WithValue(t.Context(), nativeMutationAuthorityKey{}, nativeMutationAuthority{scope: native.base(), lease: old})
	for _, test := range []struct {
		name  string
		ctx   func() context.Context
		token tracker.FencingToken
	}{
		{"old execution", func() context.Context { return bound }, 1},
		{"detached local epilogue", func() context.Context { return context.WithoutCancel(bound) }, 1},
		{"current controller", t.Context, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutation := native.fencedMutation(test.ctx(), "wi_test", tracker.Mutation{IdempotencyKey: test.name})
			if mutation.FencingToken != test.token {
				t.Fatalf("mutation acquired another execution's authority: %#v", mutation)
			}
		})
	}
}

func TestNativeDelayedResponsesPreserveSuccessor(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"renew", "release", "lost"} {
		t.Run(operation, func(t *testing.T) {
			descriptor := clientTestPolicy()
			id := "wi_" + strings.Repeat("a", 32)
			lease := tracker.NativeLease{ID: "old", WorkItemID: tracker.NativeWorkItemID(id), FencingToken: 1, PolicyID: descriptor.ID, ServerTime: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}
			next := lease
			next.ID, next.FencingToken = "new", 2
			var scheduler *Scheduler
			transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				var payload any = policy.Approval{Policy: descriptor}
				if strings.HasSuffix(request.URL.Path, "/"+operation) {
					scheduler.mu.Lock()
					claim := scheduler.nativeClaims[id]
					claim.lease = next
					scheduler.nativeClaims[id] = claim
					scheduler.claims[id] = nativeTrackerLease(next)
					scheduler.mu.Unlock()
					payload = lease
				}
				body, err := json.Marshal(payload)
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
			})
			client, err := New(Config{URL: "http://hub.example", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err = NewScheduler(client, SchedulerConfig{Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
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
			switch operation {
			case "renew":
				_, err = scheduler.renewNativeClaim(t.Context(), id, scheduler.nativeClaims[id])
				if !errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
					t.Fatalf("old renewal = %v", err)
				}
			case "release":
				if err := scheduler.ReleaseClaim(t.Context(), id, "released"); err != nil {
					t.Fatal(err)
				}
			case "lost":
				claim := scheduler.nativeClaims[id]
				claim.lease = next
				scheduler.nativeClaims[id], scheduler.claims[id] = claim, nativeTrackerLease(next)
				err = scheduler.nativeClaimError(id, lease.FencingToken, &APIError{Status: http.StatusConflict, Code: "stale_fencing_token"})
				if !errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
					t.Fatalf("old claim loss = %v", err)
				}
			}
			if scheduler.nativeClaims[id].lease.FencingToken != next.FencingToken || scheduler.claims[id].FencingToken != next.FencingToken {
				t.Fatal("delayed response changed the successor")
			}
		})
	}
}
