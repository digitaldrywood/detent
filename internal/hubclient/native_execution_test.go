package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
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
	// The stored attempt diff rides every checkpoint and the finish
	// (decisions section 18.5). The source is installed the way the runner
	// installs it, once the worktree exists.
	diffs, ok := execution.(runner.DiffExecution)
	if !ok {
		t.Fatal("a native execution must be able to store attempt diffs")
	}
	diffs.SetDiffSource(func(context.Context) (tracker.AttemptDiffRequest, bool) {
		return tracker.AttemptDiffRequest{
			BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("c", 40),
			Files: []tracker.AttemptDiffFile{{Path: "main.go", Status: tracker.DiffStatusModified, Additions: 1, Patch: "@@ diff"}},
		}, true
	})
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
			err := test.operation()
			if test.name == "checkpoint" {
				if err != nil || execution.(*nativeExecution).pending == nil {
					t.Fatalf("checkpoint lost pending acknowledgment: %v", err)
				}
			} else if err == nil {
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
	// One diff per event that references it -- the checkpoint at sequence 2
	// and the finish at sequence 3 -- and none for run.started. A retried
	// event re-flushes the same pending event rather than appending again, so
	// the generations are not duplicated either.
	attempt := executionID("attempt", string(execution.Recovery().Lease.ID))
	for _, test := range []struct {
		name  string
		query string
		seq   int64
	}{
		{name: "latest is the finish generation", seq: 3},
		{name: "the checkpoint generation is still readable", query: "?at=2", seq: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stored tracker.AttemptDiff
			if err := native.client.request(t.Context(), http.MethodGet, native.base()+"/attempts/"+attempt+"/diff"+test.query, nil, &stored); err != nil {
				t.Fatalf("read stored diff: %v", err)
			}
			if stored.Generation.Seq != test.seq || len(stored.Files) != 1 || stored.Files[0].Path != "main.go" {
				t.Fatalf("stored diff = %#v", stored)
			}
			if stored.Producer.Kind != tracker.DiffSourceAttempt || stored.Producer.LeaseID != execution.Recovery().Lease.ID {
				t.Fatalf("producer = %#v", stored.Producer)
			}
		})
	}
	var missing tracker.AttemptDiff
	if err := native.client.request(t.Context(), http.MethodGet, native.base()+"/attempts/"+attempt+"/diff?at=1", nil, &missing); err == nil {
		t.Fatal("run.started carries no stored diff")
	}
	transport.down.Store(true)
	if err := execution.Validate(guarded); err != nil {
		t.Fatalf("outage validation = %v", err)
	}
	if guarded.Err() != nil {
		t.Fatal("temporary outage cancelled provider context")
	}
	transport.down.Store(false)
	if err := execution.Validate(guarded); err != nil {
		t.Fatalf("reconnected validation = %v", err)
	}
}

type executionRoundTrip func(*http.Request) (*http.Response, error)

func (f executionRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestNativeGuardDeadlineAndRenewal(t *testing.T) {
	for _, name := range []string{"expire", "renew", "renewal outage"} {
		renew := name == "renew"
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
				if name == "renewal outage" {
					client.httpClient.Transport = executionRoundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("renewal disconnected") })
					_, err := scheduler.renewNativeClaim(t.Context(), id, scheduler.nativeClaims[id])
					if err == nil || errors.Is(err, orchestrator.ErrSchedulingClaimLost) || errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
						t.Fatalf("transient renewal revoked current lease: %v", err)
					}
					if err := execution.Validate(guarded); err != nil || guarded.Err() != nil {
						t.Fatalf("outage ended valid worker: %v", err)
					}
				}
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

func TestNativeAvailabilityDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(strconv.FormatBool(deadline), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				descriptor := clientTestPolicy()
				transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					body, err := json.Marshal(policy.Approval{Policy: descriptor})
					if err != nil {
						return nil, err
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
				})
				client, err := New(Config{URL: "https://hub.example.test", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: transport}})
				if err != nil {
					t.Fatal(err)
				}
				scheduler, err := NewScheduler(client, SchedulerConfig{Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}, HeartbeatInterval: time.Second, LeaseTTL: 2 * time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_test", "prj_test")
				if err != nil {
					t.Fatal(err)
				}
				id := "wi_test"
				claim := nativeClaim{source: &NativeConnector{client: native}, lease: tracker.NativeLease{WorkItemID: tracker.NativeWorkItemID(id), ID: "lease", FencingToken: 1, PolicyID: descriptor.ID}, deadline: time.Now().Add(2 * time.Minute)}
				if deadline {
					claim.availabilityDeadline = time.Now().Add(30 * time.Second)
				}
				scheduler.nativeClaims[id] = claim
				scheduler.claimPolicies[id] = claimPolicy{project: "project", descriptor: descriptor}
				scheduler.nativeProjects["project"] = claim.source
				execution := scheduler.RunExecution(id)
				guarded, stop, err := execution.Guard(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				if err := execution.Start(guarded, tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
					t.Fatal(err)
				}
				time.Sleep(29 * time.Second)
				synctest.Wait()
				if guarded.Err() != nil {
					t.Fatal("job stopped before hard deadline")
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if deadline {
					if !runner.IsAvailabilityInterruption(context.Cause(guarded)) {
						t.Fatalf("cause = %v", context.Cause(guarded))
					}
					if errors.Is(execution.Validate(guarded), runner.ErrExecutionAuthorityUnavailable) {
						t.Fatal("availability deadline revoked execution authority")
					}
				} else if guarded.Err() != nil {
					t.Fatal("job without hard deadline stopped")
				}
				if err := execution.Finish(context.WithoutCancel(guarded), "interrupted"); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestNativeAvailabilityDeadlineRefresh(t *testing.T) {
	for _, change := range []string{"remove", "extend", "add", "shorten", "closed"} {
		t.Run(change, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				identityPath := filepath.Join(t.TempDir(), "private", "runner.json")
				file, err := runnerauth.Initialize(identityPath, "https://hub.example.test")
				if err != nil {
					t.Fatal(err)
				}
				descriptor := clientTestPolicy()
				transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					body, err := json.Marshal(policy.Approval{Policy: descriptor})
					if strings.HasSuffix(request.URL.Path, "/validate") {
						body, err = json.Marshal(runnerauth.Runner{Binding: file.Identity.Binding, OrganizationID: file.Identity.OrganizationID})
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, err
				})
				client, err := New(Config{URL: file.HubURL, TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: transport}})
				if err != nil {
					t.Fatal(err)
				}
				file.Identity.OrganizationID = "org_test"
				file.Identity.ExpiresAt = time.Now().Add(24 * time.Hour)
				client.runner = &runnerCredentialSource{path: identityPath}
				if err := runnerauth.Save(client.runner.path, file); err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				closeTime := now.Truncate(time.Minute).Add(time.Minute)
				availability := runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun " + now.Add(-time.Minute).Format("15:04") + "-" + closeTime.Format("15:04")}, HardDeadline: "30s"}
				if change == "add" {
					availability.HardDeadline = ""
				}
				if change == "shorten" {
					availability.Windows[0] = "Mon-Sun " + now.Add(-time.Minute).Format("15:04") + "-" + closeTime.Add(3*time.Minute).Format("15:04")
				}
				client.runner.setRouting(runnerauth.RoutingSnapshot{Routing: runnerauth.Routing{Availability: availability}})
				scheduler, err := NewScheduler(client, SchedulerConfig{Machine: Machine{ID: file.Identity.MachineID, Hostname: "host", Version: "test", Capacity: 1}, HeartbeatInterval: time.Second, LeaseTTL: 10 * time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_test", "prj_test")
				if err != nil {
					t.Fatal(err)
				}
				claim := nativeClaim{source: &NativeConnector{client: native}, lease: tracker.NativeLease{WorkItemID: "wi_test", ID: "lease", FencingToken: 1, PolicyID: descriptor.ID}, deadline: now.Add(10 * time.Minute)}
				scheduler.nativeClaims["wi_test"] = claim
				scheduler.claimPolicies["wi_test"] = claimPolicy{project: "project", descriptor: descriptor}
				scheduler.nativeProjects["project"] = claim.source
				execution := scheduler.RunExecution("wi_test")
				guarded, stop, err := execution.Guard(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				synctest.Wait()
				time.Sleep(30 * time.Second)
				switch change {
				case "remove":
					availability.HardDeadline = ""
				case "extend":
					availability.Windows[0] = "Mon-Sun " + now.Add(-time.Minute).Format("15:04") + "-" + closeTime.Add(3*time.Minute).Format("15:04")
				case "add":
					availability.HardDeadline = "30s"
				case "shorten":
					availability.Windows[0] = "Mon-Sun " + now.Add(-time.Minute).Format("15:04") + "-" + closeTime.Format("15:04")
				case "closed":
					availability.Windows[0] = "Mon-Sun " + now.Add(-2*time.Minute).Format("15:04") + "-" + now.Format("15:04")
				}
				client.runner.setRouting(runnerauth.RoutingSnapshot{Routing: runnerauth.Routing{Availability: availability}})
				synctest.Wait()
				time.Sleep(time.Until(closeTime.Add(30 * time.Second)))
				synctest.Wait()
				wantStopped := change != "remove" && change != "extend"
				if (guarded.Err() != nil) != wantStopped {
					t.Fatalf("stopped = %v, want %t", guarded.Err(), wantStopped)
				}
				if wantStopped && !runner.IsAvailabilityInterruption(context.Cause(guarded)) {
					t.Fatalf("interruption cause = %v", context.Cause(guarded))
				}
			})
		})
	}
}

func TestNativeExecutionTransportKeepsCurrentWorker(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		path   string
		status int
		body   string
		fatal  bool
	}{
		{name: "transport"},
		{name: "server", status: http.StatusServiceUnavailable, body: `{"code":"tenant_unavailable"}`},
		{name: "unauthorized policy", path: "/policy", status: http.StatusUnauthorized, body: `{"code":"unauthorized"}`, fatal: true},
		{name: "revoked lease", path: "/validate", status: http.StatusForbidden, body: `{"code":"forbidden"}`, fatal: true},
		{name: "malformed unauthorized", path: "/policy", status: http.StatusUnauthorized, body: "unauthorized", fatal: true},
		{name: "permanent protocol", path: "/policy", status: http.StatusBadRequest, body: `{"code":"invalid_request"}`, fatal: true},
		{name: "stale fencing", path: "/validate", status: http.StatusConflict, body: `{"code":"stale_fencing_token"}`, fatal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newNativeChangeHub(t, true)
			issue := h.createInProgress(t, "Transport survival")
			h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			guarded, stop, err := execution.Guard(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			identity := tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}
			if err := execution.Start(guarded, identity); err != nil {
				t.Fatal(err)
			}
			observation := tracker.NativeRuntimeObservation{LocalAttemptID: 42, Generation: 1, Phase: "implementation", HeartbeatAt: time.Now()}
			if err := execution.ObserveRuntime(guarded, observation); err != nil {
				t.Fatal(err)
			}
			before := execution.data
			original := h.native.client.httpClient.Transport
			h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if test.path != "" && !strings.HasSuffix(request.URL.Path, test.path) {
					return original.RoundTrip(request)
				}
				if test.status == 0 {
					return nil, errors.New("bounded disconnect")
				}
				return &http.Response{StatusCode: test.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body)), Request: request}, nil
			})
			if err := execution.Validate(guarded); test.fatal != errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || !test.fatal && err != nil {
				t.Fatalf("validation=%v, fatal=%t", err, test.fatal)
			}
			if test.fatal {
				if !errors.Is(context.Cause(guarded), runner.ErrExecutionAuthorityUnavailable) {
					t.Fatal("real refusal retained worker authority")
				}
				return
			}
			observation.Phase = "rework"
			if err := execution.ObserveRuntime(guarded, observation); err != nil {
				t.Fatalf("report ended worker: %v", err)
			}
			if execution.pending == nil || execution.pending.Data.Sequence != before.Sequence+1 {
				t.Fatal("outage lost pending event")
			}
			pending := *execution.pending
			if err := execution.ObserveRuntime(guarded, observation); err != nil {
				t.Fatal(err)
			}
			if execution.pending.Mutation.IdempotencyKey != pending.Mutation.IdempotencyKey || guarded.Err() != nil {
				t.Fatal("outage replaced worker or pending event")
			}
			h.native.client.httpClient.Transport = original
			if err := execution.Validate(guarded); err != nil {
				t.Fatal(err)
			}
			if err := execution.FlushRuntime(guarded); err != nil {
				t.Fatal(err)
			}
			if err := execution.FlushRuntime(guarded); err != nil {
				t.Fatal(err)
			}
			if execution.pending != nil || execution.data.Sequence != pending.Data.Sequence || execution.data.AttemptID != before.AttemptID || execution.data.LeaseID != before.LeaseID || *execution.data.Identity != identity || guarded.Err() != nil {
				t.Fatal("reconnect replaced worker or duplicated event")
			}
			recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Sequence != pending.Data.Sequence || recovery.Attempts[0].Status != "running" {
				t.Fatalf("recovery=%+v error=%v", recovery.Attempts, err)
			}
		})
	}
}
