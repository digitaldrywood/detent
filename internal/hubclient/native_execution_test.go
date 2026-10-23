package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
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
	if _, err := scheduler.RenewClaim(guarded, issueID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(execution.Recovery().Discussion) != 1 {
		t.Fatal("native recovery replayed historical worker discussion")
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
			if test.name == "finish" {
				execution.(*nativeExecution).ProviderCompleted(scheduler.now())
			}
			transport.drop.Store(true)
			err := test.operation()
			if test.name == "checkpoint" {
				if err != nil || execution.(*nativeExecution).pending == nil {
					t.Fatalf("checkpoint lost pending acknowledgment: %v", err)
				}
			} else if err == nil {
				t.Fatal("acknowledgment loss was not injected")
			}
			if test.name == "finish" {
				owner := execution.(*nativeExecution)
				pending, sequence := owner.pending, owner.data.Sequence
				claims := scheduler.projectDiagnosticClaims(owner.claim.source)
				if len(claims) != 1 || claims[0].HostOperation != "append.run.finished" || !claims[0].OperationPending || claims[0].ProviderCompletedAt.IsZero() || claims[0].LatestError == "" {
					t.Fatalf("completion diagnostic=%+v", claims)
				}
				if owner.pending != pending || owner.data.Sequence != sequence {
					t.Fatal("diagnostic advanced completion")
				}
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
	for _, name := range []string{"expire", "renew", "renewal outage", "renew during native finish", "guard during native finish", "budget wait", "budget wait loses lease"} {
		renew := name == "renew"
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				descriptor := clientTestPolicy()
				var offline atomic.Bool
				var budgetWait atomic.Bool
				var budgetReads, budgetRenewals atomic.Int32
				var finished tracker.NativeRunEvent
				transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if offline.Load() {
						return nil, errors.New("renewal disconnected")
					}
					body := []byte(`{}`)
					if strings.HasSuffix(request.URL.Path, "/renew") {
						if budgetWait.Load() {
							budgetRenewals.Add(1)
							if name == "budget wait" {
								lease := tracker.NativeLease{ID: "lease", FencingToken: 1, PolicyID: descriptor.ID, ServerTime: time.Now(), RenewedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}
								body, err := json.Marshal(lease)
								return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, err
							}
						}
						return &http.Response{StatusCode: http.StatusConflict, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"stale_fencing_token"}`)), Request: request}, nil
					}
					if strings.HasSuffix(request.URL.Path, "/policy") {
						if budgetWait.Load() && budgetReads.Add(1) == 1 {
							return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"60"}}, Body: io.NopCloser(strings.NewReader(`{"code":"runner_request_budget","message":"Runner request budget exceeded; retry after the current minute"}`)), Request: request}, nil
						}
						var err error
						body, err = json.Marshal(policy.Approval{Policy: descriptor})
						if err != nil {
							return nil, err
						}
					}
					if strings.HasPrefix(name, "budget wait") && strings.HasSuffix(request.URL.Path, "/events") {
						var event tracker.NativeRunEvent
						if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
							return nil, err
						}
						if event.Type == "run.finished" {
							finished = event
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
				scheduler.claims[id] = nativeTrackerLease(scheduler.nativeClaims[id].lease)
				scheduler.claimPolicies[id] = claimPolicy{project: "project", descriptor: descriptor}
				execution := scheduler.RunExecution(id)
				guarded := t.Context()
				if name != "renew during native finish" && name != "guard during native finish" {
					var stop func()
					guarded, stop, err = execution.Guard(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer stop()
				}
				if strings.HasPrefix(name, "budget wait") {
					scheduler.heartbeatInterval = 2 * time.Minute
					scheduler.nativeHeartbeats[native.project] = time.Now()
					if err := execution.Start(guarded, tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
						t.Fatal(err)
					}
					budgetWait.Store(true)
					start := time.Now()
					execution.(*nativeExecution).HostOperation("policy.read", start, nil, true)
					_, err := native.ProjectPolicy(guarded)
					execution.(*nativeExecution).HostOperation("policy.read", time.Now(), err, false)
					if budgetRenewals.Load() != 1 {
						t.Fatalf("budget wait blocked lease renewal: renewals=%d reads=%d elapsed=%s error=%v cause=%v", budgetRenewals.Load(), budgetReads.Load(), time.Since(start), err, context.Cause(guarded))
					}
					if name == "budget wait loses lease" {
						if !errors.Is(err, context.Canceled) || !errors.Is(context.Cause(guarded), runner.ErrExecutionAuthorityUnavailable) || budgetReads.Load() != 1 || time.Since(start) != 55*time.Second {
							t.Fatalf("wait outlived lease: error=%v cause=%v reads=%d elapsed=%s", err, context.Cause(guarded), budgetReads.Load(), time.Since(start))
						}
						return
					}
					if err != nil || guarded.Err() != nil || budgetReads.Load() != 2 || time.Since(start) != time.Minute {
						t.Fatalf("budget retry: error=%v cause=%v reads=%d elapsed=%s", err, context.Cause(guarded), budgetReads.Load(), time.Since(start))
					}
					if err := execution.Finish(guarded, "succeeded"); err != nil {
						t.Fatal(err)
					}
					if finished.Data.Outcome != "succeeded" || finished.Data.Sequence != 2 {
						t.Fatalf("attempt did not complete: %+v", finished)
					}
					return
				}
				if name == "renew during native finish" || name == "guard during native finish" {
					claim := scheduler.nativeClaims[id]
					scheduler.claims[id] = nativeTrackerLease(claim.lease)
					scheduler.nativeHeartbeats[native.project] = time.Now()
					nativeExecution := execution.(*nativeExecution)
					nativeExecution.data.Identity = &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}
					nativeExecution.preparedOutcome = "failed"
					var stopGuard func()
					defer func() {
						if stopGuard != nil {
							stopGuard()
						}
					}()
					started, allowFinish := make(chan struct{}), make(chan struct{})
					defer func() {
						select {
						case <-allowFinish:
						default:
							close(allowFinish)
						}
					}()
					original := client.httpClient.Transport
					finishedEvents := 0
					client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						if strings.HasSuffix(request.URL.Path, "/renew") {
							lease := claim.lease
							lease.ServerTime, lease.RenewedAt = time.Now(), time.Now()
							lease.ExpiresAt = time.Now().Add(time.Minute)
							body, err := json.Marshal(lease)
							if err != nil {
								return nil, err
							}
							return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
						}
						if strings.HasSuffix(request.URL.Path, "/events") {
							var event tracker.NativeRunEvent
							if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
								return nil, err
							}
							if event.Type == "run.finished" {
								finishedEvents++
								close(started)
								<-allowFinish
							}
						}
						return original.RoundTrip(request)
					})
					if name == "guard during native finish" {
						guarded, stopGuard, err = execution.Guard(t.Context())
						if err != nil {
							t.Fatal(err)
						}
						synctest.Wait()
					}
					finished := make(chan error, 1)
					go func() { finished <- scheduler.ReleaseClaim(guarded, id, "released") }()
					<-started
					for range 7 {
						time.Sleep(20 * time.Second)
						if name == "guard during native finish" {
							synctest.Wait()
							scheduler.mu.Lock()
							deadline := scheduler.nativeClaims[id].deadline
							scheduler.mu.Unlock()
							if deadline.Before(time.Now()) {
								t.Fatal("completion publication blocked the worker lease guard")
							}
							continue
						}
						renewed := make(chan error, 1)
						go func() {
							_, err := scheduler.RenewClaim(guarded, id, time.Now())
							renewed <- err
						}()
						synctest.Wait()
						select {
						case err := <-renewed:
							if err != nil {
								t.Fatalf("renew existing native lease during Finish: %v", err)
							}
						default:
							t.Fatal("renewal waited for completion publication")
						}
						current := scheduler.nativeClaims[id]
						if current.lease.ID != claim.lease.ID || current.lease.FencingToken != claim.lease.FencingToken {
							t.Fatal("slow Finish replaced existing lease authority")
						}
						if guarded.Err() != nil || nativeExecution.remaining() <= 0 {
							t.Fatal("slow Finish lost the existing fenced owner")
						}
					}
					close(allowFinish)
					if err := <-finished; err != nil || finishedEvents != 1 {
						t.Fatalf("Finish publication: events=%d error=%v", finishedEvents, err)
					}
					if stopGuard != nil {
						stopGuard()
						stopGuard = nil
					}
					if nativeExecution.remaining() != 0 || len(scheduler.nativeClaims) != 0 || len(scheduler.claims) != 0 {
						t.Fatal("terminal acknowledgement retained lease ownership")
					}
					return
				}
				time.Sleep(30 * time.Second)
				if name == "renewal outage" {
					offline.Store(true)
					_, err := scheduler.renewNativeClaim(t.Context(), id, scheduler.nativeClaims[id], 0)
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
				if name == "renewal outage" {
					time.Sleep(4 * time.Minute)
					synctest.Wait()
					if guarded.Err() != nil {
						t.Fatalf("outage canceled worker: %v", context.Cause(guarded))
					}
					offline.Store(false)
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

func TestNativeLeaseRenewalRequiresActiveWork(t *testing.T) {
	identity, err := procgroup.Inspect(&exec.Cmd{Process: &os.Process{Pid: os.Getpid()}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name         string
		phase        string
		outcome      string
		age          time.Duration
		terminal     bool
		lane         string
		dependency   bool
		pending      bool
		controller   bool
		stopped      bool
		prepared     bool
		localWorker  bool
		staleProcess bool
		recovered    bool
		killed       bool
		lost         bool
		wantRenewed  bool
		wantFinished bool
	}{
		{name: "deferred interrupted review", phase: "completed", outcome: "interrupted", wantRenewed: true},
		{name: "published terminal attempt", phase: "completed", outcome: "failed"},
		{name: "killed worker", phase: "implementation", localWorker: true, killed: true},
		{name: "claim lost with worker", phase: "implementation", localWorker: true, lost: true},
		{name: "restarted fresh worker", phase: "implementation", recovered: true},
		{name: "live worker", phase: "implementation", localWorker: true, wantRenewed: true},
		{name: "fresh reported worker without owner", phase: "implementation"},
		{name: "quiet long running merge worker", phase: "merging", age: 6 * time.Hour, localWorker: true, wantRenewed: true},
		{name: "local process identity no longer matches", phase: "implementation", localWorker: true, staleProcess: true},
		{name: "controller receives completion handoff", phase: "completed", age: 89 * time.Second, controller: true, wantRenewed: true},
		{name: "live controller finishes after the runtime heartbeat expires", phase: "completed", age: 20 * time.Minute, controller: true, wantRenewed: true, wantFinished: true},
		{name: "controller stopped without prepared outcome", phase: "completed", controller: true, stopped: true},
		{name: "prepared outcome awaiting publication", phase: "completed", age: 2 * time.Minute, controller: true, stopped: true, prepared: true, pending: true, wantRenewed: true},
		{name: "dead completed worker cleanup removed", phase: "completed", localWorker: true, staleProcess: true},
		{name: "dead completed worker old cleanup removed", phase: "completed", age: 20 * time.Minute, localWorker: true, staleProcess: true},
		{name: "dead completed worker expires", phase: "completed", age: 20 * time.Minute},
		{name: "deferred publication owned by live scheduler", phase: "completed", age: 2 * time.Minute, controller: true, stopped: true, prepared: true, wantRenewed: true},
		{name: "worker exited after completion", phase: "completed"},
		{name: "worker heartbeat stale", phase: "implementation", age: 90 * time.Second},
		{name: "terminal work item", phase: "implementation", terminal: true},
		{name: "terminal work item retains running worker cleanup", phase: "implementation", terminal: true, localWorker: true},
		{name: "recovered completed worker", phase: "completed", recovered: true},
		{name: "recovered stale worker", phase: "implementation", age: 90 * time.Second, recovered: true},
		{name: "recovered operator hold", phase: "implementation", lane: "Blocked", recovered: true},
		{name: "recovered custom hold", phase: "implementation", lane: "Waiting", recovered: true},
		{name: "recovered unmet dependency", phase: "implementation", dependency: true, recovered: true},
		{name: "recovered rework without owner", phase: "implementation", lane: "Rework", recovered: true},
		{name: "live operator hold retains cleanup", phase: "implementation", lane: "Blocked", localWorker: true},
		{name: "failed diagnostic without owner", phase: "implementation", pending: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newNativeChangeHubDependencies(t, "In Review", []tracker.NativeState{
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"Done", "Blocked", "Waiting", "Rework"}},
				{Name: "Blocked", Transitions: []string{"In Progress"}},
				{Name: "Waiting", Transitions: []string{"In Progress"}},
				{Name: "Rework", Dispatchable: true, Transitions: []string{"In Progress"}},
				{Name: "Done", Terminal: true},
			}, true, true)
			issue := h.createInProgress(t, test.name)
			h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			now := time.Now().UTC()
			h.scheduler.now = func() time.Time { return now }
			if test.wantFinished {
				h.scheduler.now = func() time.Time { return now.Add(-test.age) }
				if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
					t.Fatal(err)
				}
			}
			h.scheduler.now = func() time.Time { return now }
			observation := tracker.NativeRuntimeObservation{LocalAttemptID: 42, Generation: 1, Phase: test.phase, HeartbeatAt: now.Add(-test.age), WorkerLaunch: &tracker.NativeWorkerLaunch{PID: identity.PID, GroupID: identity.GroupID, ProcessStart: identity.StartedAt, PolicyID: h.descriptor.ID, ExecutionVersion: "test", ControlVersion: "test", ExecutionCommit: strings.Repeat("a", 40), ExecutionSHA256: strings.Repeat("b", 64), Protocol: 1, SelectedAt: now}}
			if test.staleProcess {
				observation.WorkerLaunch.ProcessStart = identity.StartedAt.Add(time.Second)
			}
			if err := execution.ObserveRuntime(t.Context(), observation); err != nil {
				t.Fatal(err)
			}
			execution.data.Outcome = test.outcome
			if test.localWorker {
				if err := execution.KeepWorker(func(context.Context) error { return nil }, nil); err != nil {
					t.Fatal(err)
				}
			}
			if test.controller {
				_, stop, err := execution.Guard(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if test.prepared {
					if err := execution.PrepareFinish(t.Context(), "failed", "", nil); err != nil {
						t.Fatal(err)
					}
				}
				if test.stopped {
					stop()
				} else {
					t.Cleanup(stop)
				}
			}
			if test.recovered {
				claim := h.scheduler.nativeClaims[issue.ID]
				claim.execution = nil
				claim.recovery.Attempts = []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{LeaseID: claim.lease.ID, FencingToken: claim.lease.FencingToken, Runtime: &observation}, Status: "running"}}
				h.scheduler.nativeClaims[issue.ID] = claim
			}
			if test.pending {
				if err := execution.postDiff(t.Context(), 1); err == nil {
					t.Fatal("missing diff source did not fail postDiff")
				}
				if execution.diagnostic.OperationPending {
					t.Fatal("failed diagnostic remained pending")
				}
			}
			if test.terminal {
				if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Done"); err != nil {
					t.Fatal(err)
				}
			}
			if test.lane != "" {
				current, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.admin.Transition(t.Context(), current.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: test.lane, Reason: "user_requested"}); err != nil {
					t.Fatal(err)
				}
			}
			if test.dependency {
				blocker, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Prerequisite", State: "In Progress"})
				if err != nil {
					t.Fatal(err)
				}
				current, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.admin.Dependency(t.Context(), current.WorkItemID, tracker.DependencyMutation{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}); err != nil {
					t.Fatal(err)
				}
			}
			before := h.scheduler.nativeClaims[issue.ID].lease
			transport := h.native.client.httpClient.Transport
			var renewals, releases int
			h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if strings.HasSuffix(request.URL.Path, "/renew") {
					renewals++
				}
				if strings.HasSuffix(request.URL.Path, "/release") {
					releases++
				}
				return transport.RoundTrip(request)
			})
			if test.killed {
				if err := execution.CloseWorkers(t.Context()); err != nil {
					t.Fatal(err)
				}
				if execution.hasWorkers() {
					t.Fatal("killed worker remained registered")
				}
			}
			if test.lost {
				h.scheduler.ForgetClaim(issue.ID)
				if _, held := h.scheduler.nativeClaims[issue.ID]; held {
					t.Fatal("lost claim retained")
				}
				return
			}
			renewed, err := h.scheduler.RenewClaim(t.Context(), issue.ID, now)
			if test.wantRenewed {
				if err != nil || !renewed.LeaseRenewedAt.After(before.RenewedAt) {
					t.Fatalf("lease was not renewed: %+v, %v", renewed, err)
				}
				if h.scheduler.nativeClaims[issue.ID].lease.FencingToken != before.FencingToken {
					t.Fatal("renewal changed the fencing token")
				}
				if test.wantFinished {
					if err := execution.Finish(t.Context(), "succeeded"); err != nil {
						t.Fatal(err)
					}
					if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
						t.Fatal(err)
					}
					page, err := h.admin.AttemptsPage(t.Context(), tracker.NativeWorkItemID(issue.ID), "", 10)
					if err != nil {
						t.Fatal(err)
					}
					if len(page.Items) != 1 || page.Items[0].Status != "succeeded" || page.Items[0].TerminalFailure != nil || page.Items[0].ClaimReleasedAt == nil {
						t.Fatalf("live finalization lost its completion: %+v", page.Items)
					}
				}
			} else {
				if !errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
					t.Fatalf("inactive renewal = %v, want claim lost", err)
				}
				if execution.hasWorkers() {
					if _, held := h.scheduler.claims[issue.ID]; held {
						t.Fatal("lost claim retained renewal authority")
					}
					if _, err := h.scheduler.RenewClaim(t.Context(), issue.ID, now); !errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
						t.Fatalf("cleanup-only claim renewed: %v", err)
					}
					if err := execution.CloseWorkers(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				if _, held := h.scheduler.nativeClaims[issue.ID]; held {
					t.Fatal("inactive lease retained")
				}
				if renewals != 0 || releases != 0 {
					t.Fatalf("inactive lease received %d renewals and %d forced releases", renewals, releases)
				}
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
	if err := scheduler.ReleaseClaim(t.Context(), "wi_"+strings.Repeat("a", 32), "completed"); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
		t.Fatalf("missing native completion owner reported settlement: %v", err)
	}
	if err := scheduler.ReleaseClaim(t.Context(), "github-node", "completed"); err != nil {
		t.Fatalf("legacy idempotent release = %v", err)
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
	for _, operation := range []string{"renew", "renew_availability", "release", "release_finish_lost", "release_finish_ack_retry", "lost"} {
		t.Run(operation, func(t *testing.T) {
			descriptor := clientTestPolicy()
			id := "wi_" + strings.Repeat("a", 32)
			lease := tracker.NativeLease{ID: "old", WorkItemID: tracker.NativeWorkItemID(id), FencingToken: 1, PolicyID: descriptor.ID, ServerTime: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}
			next := lease
			next.ID, next.FencingToken = "new", 2
			availabilityDeadline := time.Now().Add(30 * time.Second)
			if operation == "renew_availability" {
				next = lease
			}
			var scheduler *Scheduler
			var releaseCalls, finishedEvents int
			transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				var payload any = policy.Approval{Policy: descriptor}
				if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/work-items/") {
					payload = tracker.NativeIssue{SchedulingEligible: new(true), DependencyReady: true}
				}
				if strings.HasSuffix(request.URL.Path, "/events") {
					var event tracker.NativeRunEvent
					if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
						t.Fatal(err)
					}
					if event.Type == "run.finished" {
						finishedEvents++
					}
				}
				if strings.HasSuffix(request.URL.Path, "/release") && operation == "release_finish_ack_retry" {
					releaseCalls++
					if releaseCalls == 1 {
						return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"tenant_unavailable"}`)), Request: request}, nil
					}
				}
				if strings.HasSuffix(request.URL.Path, "/"+strings.Split(operation, "_")[0]) {
					scheduler.mu.Lock()
					claim := scheduler.nativeClaims[id]
					claim.lease = next
					if operation == "renew_availability" {
						claim.availabilityDeadline = availabilityDeadline
					}
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
			if strings.HasPrefix(operation, "renew") {
				if err := scheduler.RunExecution(id).(*nativeExecution).KeepWorker(func(context.Context) error { return nil }, nil); err != nil {
					t.Fatal(err)
				}
			}
			switch operation {
			case "renew_availability":
				_, err = scheduler.renewNativeClaim(t.Context(), id, scheduler.nativeClaims[id], 0)
				if err != nil || !scheduler.nativeClaims[id].availabilityDeadline.Equal(availabilityDeadline) {
					t.Fatalf("delayed renewal replaced refreshed availability: %v, %v", scheduler.nativeClaims[id].availabilityDeadline, err)
				}
			case "renew":
				_, err = scheduler.renewNativeClaim(t.Context(), id, scheduler.nativeClaims[id], 0)
				if !errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
					t.Fatalf("old renewal = %v", err)
				}
			case "release_finish_ack_retry":
				claim := scheduler.nativeClaims[id]
				claim.deadline = time.Now().Add(time.Minute)
				scheduler.nativeClaims[id] = claim
				execution := scheduler.RunExecution(id).(*nativeExecution)
				execution.preparedOutcome = "failed"
				execution.data.Identity = &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}
				cleanupCalls := 0
				if err := execution.KeepWorker(func(context.Context) error {
					cleanupCalls++
					if execution.data.Outcome != "failed" || finishedEvents != 1 {
						t.Fatal("worker ownership ended before terminal publication was acknowledged")
					}
					return nil
				}, nil); err != nil {
					t.Fatal(err)
				}
				if err := scheduler.ReleaseClaim(t.Context(), id, "released"); err == nil || errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
					t.Fatalf("release outage = %v", err)
				}
				if execution.data.Outcome != "failed" || finishedEvents != 1 {
					t.Fatalf("Finish acknowledgement = %q, events = %d", execution.data.Outcome, finishedEvents)
				}
				claim = scheduler.nativeClaims[id]
				claim.deadline = time.Now().Add(-time.Second)
				scheduler.nativeClaims[id] = claim
				if err := scheduler.ReleaseClaim(t.Context(), id, "released"); err != nil {
					t.Fatalf("acknowledged Finish lost on release retry: %v", err)
				}
				if finishedEvents != 1 {
					t.Fatalf("Finish published again: %d events", finishedEvents)
				}
				if cleanupCalls != 1 {
					t.Fatalf("worker cleanup repeated after a lease-release retry: %d", cleanupCalls)
				}
			case "release", "release_finish_lost":
				if operation == "release_finish_lost" {
					claim := scheduler.nativeClaims[id]
					claim.deadline = time.Now().Add(-time.Second)
					scheduler.nativeClaims[id] = claim
					execution := scheduler.RunExecution(id).(*nativeExecution)
					execution.preparedOutcome = "failed"
				}
				err := scheduler.ReleaseClaim(t.Context(), id, "released")
				if operation == "release_finish_lost" {
					if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
						t.Fatalf("failed Finish reported settlement: %v", err)
					}
				} else if err != nil {
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
	if testing.Short() {
		t.Skip("real-time lifecycle and timeout integration")
	}

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
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name         string
		path         string
		status       int
		body         string
		fatal        bool
		interruption error
		expired      bool
	}{
		{name: "already canceled", interruption: context.Canceled},
		{name: "already timed out", interruption: context.DeadlineExceeded},
		{name: "canceled lease request", path: "/validate", interruption: context.Canceled},
		{name: "timed out lease request", path: "/validate", interruption: context.DeadlineExceeded},
		{name: "canceled renewal retains expired lease", interruption: context.Canceled, expired: true},
		{name: "canceled revoked authority", interruption: runner.ErrExecutionAuthorityUnavailable, fatal: true},
		{name: "transport"},
		{name: "server", status: http.StatusServiceUnavailable, body: `{"code":"tenant_unavailable"}`},
		{name: "unauthorized lease", path: "/validate", status: http.StatusUnauthorized, body: `{"code":"unauthorized"}`, fatal: true},
		{name: "revoked lease", path: "/validate", status: http.StatusForbidden, body: `{"code":"forbidden"}`, fatal: true},
		{name: "malformed unauthorized", path: "/validate", status: http.StatusUnauthorized, body: "unauthorized", fatal: true},
		{name: "permanent protocol", path: "/validate", status: http.StatusBadRequest, body: `{"code":"invalid_request"}`, fatal: true},
		{name: "stale fencing", path: "/validate", status: http.StatusConflict, body: `{"code":"stale_fencing_token"}`, fatal: true},
		{name: "policy mismatch", path: "/validate", status: http.StatusConflict, body: `{"code":"policy_mismatch"}`, fatal: true},
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
			validationCtx := guarded
			if test.interruption != nil && test.path == "" {
				if errors.Is(test.interruption, context.DeadlineExceeded) {
					var cancel context.CancelFunc
					validationCtx, cancel = context.WithDeadline(guarded, time.Now().Add(-time.Second))
					defer cancel()
				} else {
					var cancel context.CancelCauseFunc
					validationCtx, cancel = context.WithCancelCause(guarded)
					cancel(test.interruption)
				}
			}
			if test.expired {
				h.scheduler.mu.Lock()
				claim := h.scheduler.nativeClaims[issue.ID]
				claim.deadline = time.Now().Add(-time.Second)
				h.scheduler.nativeClaims[issue.ID] = claim
				h.scheduler.mu.Unlock()
			}
			original := h.native.client.httpClient.Transport
			h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if test.path != "" && !strings.HasSuffix(request.URL.Path, test.path) {
					return original.RoundTrip(request)
				}
				if test.interruption != nil {
					return nil, test.interruption
				}
				if test.status == 0 {
					return nil, errors.New("bounded disconnect")
				}
				return &http.Response{StatusCode: test.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body)), Request: request}, nil
			})
			err = execution.Validate(validationCtx)
			if test.fatal != errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || !test.fatal && test.interruption == nil && err != nil {
				t.Fatalf("validation=%v, fatal=%t", err, test.fatal)
			}
			if test.interruption != nil && !errors.Is(err, test.interruption) {
				t.Fatalf("validation=%v, want interruption %v", err, test.interruption)
			}
			if test.fatal {
				if !errors.Is(context.Cause(guarded), runner.ErrExecutionAuthorityUnavailable) {
					t.Fatal("real refusal retained worker authority")
				}
				return
			}
			if test.interruption != nil {
				h.native.client.httpClient.Transport = original
				if guarded.Err() != nil || execution.data.LeaseID != before.LeaseID || execution.data.FencingToken != before.FencingToken || h.scheduler.RunExecution(issue.ID) != execution {
					t.Fatal("interruption replaced current worker authority")
				}
				if err := execution.Validate(guarded); err != nil {
					t.Fatalf("interruption prevented validation with original lease: %v", err)
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
			if execution.pending.IdempotencyKey != pending.IdempotencyKey || guarded.Err() != nil {
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

func TestNativeCacheWaitRuntimeRecovery(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"waiting_cache_admission", "waiting_cache_consumers"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			h := newNativeChangeHub(t, true)
			issue := h.createInProgress(t, "Cache wait recovery")
			h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			ctx := t.Context()
			if err := execution.Start(ctx, tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			observation := tracker.NativeRuntimeObservation{LocalAttemptID: 42, Generation: 1, Phase: "implementation", HeartbeatAt: time.Now()}
			if err := execution.ObserveRuntime(ctx, observation); err != nil {
				t.Fatal(err)
			}
			before := execution.data
			original := h.native.client.httpClient.Transport
			h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(request.URL.Path, "/events") {
					return original.RoundTrip(request)
				}
				return &http.Response{StatusCode: http.StatusUnprocessableEntity, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"invalid_request"}`)), Request: request}, nil
			})
			observation.Phase = phase
			if err := execution.ObserveRuntime(ctx, observation); err == nil {
				t.Fatal("legacy Hub accepted cache wait")
			}
			if execution.pending == nil {
				t.Fatal("rejected observation was not retained")
			}
			pending := *execution.pending
			h.native.client.httpClient.Transport = original
			observation.Phase = "implementation"
			if err := execution.ObserveRuntime(ctx, observation); err != nil {
				t.Fatalf("pending cache wait prevented resumed execution: %v", err)
			}
			observation.Phase = "completed"
			if err := execution.ObserveRuntime(ctx, observation); err != nil {
				t.Fatal(err)
			}
			if execution.pending != nil || execution.data.Sequence != pending.Data.Sequence+2 || execution.data.AttemptID != before.AttemptID || execution.data.LeaseID != before.LeaseID || execution.data.Runtime.Phase != "completed" {
				t.Fatal("cache wait recovery lost ordered runtime evidence or ownership")
			}
			if len(execution.data.Runtime.Phases) != 4 || execution.data.Runtime.Phases[1].Name != phase {
				t.Fatal("cache wait interval was lost")
			}
			observation.Phase = "waiting_cache_unknown"
			var apiErr *APIError
			if err := execution.ObserveRuntime(ctx, observation); !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity {
				t.Fatalf("unknown runtime phase was not rejected: %v", err)
			}
		})
	}
}

func TestNativePublicationFailureReleasesMovedItem(t *testing.T) {
	for _, test := range []struct {
		name, state string
		revoked     bool
	}{
		{name: "moved to Todo", state: "Todo"},
		{name: "moved to Done", state: "Done"},
		{name: "revoked in place", state: "In Progress", revoked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			issue := h.createInProgress(t, "publication recovery")
			h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			guarded, stop, err := execution.Guard(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			if err := execution.Start(guarded, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			closed := false
			if err := execution.KeepWorker(func(context.Context) error { closed = true; return nil }, nil); err != nil {
				t.Fatal(err)
			}
			h.failChanges.fail.Store(true)
			execution.mu.Lock()
			execution.preparedOutcome = "succeeded"
			execution.worktreeState = "unpushed"
			execution.storedSeq = execution.data.Sequence + 1
			diff, _ := nativeChangeDiff(nativeChangeFixtureHead("c"), "README.md")(t.Context())
			execution.lastDiff = &diff
			execution.change = &runner.NativeChange{Error: "required GitHub PR has an ambiguous publication response"}
			execution.mu.Unlock()
			current, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil {
				t.Fatal(err)
			}
			if current.State != test.state {
				if _, err := h.admin.Transition(t.Context(), current.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: test.state, Reason: "user_requested"}); err != nil {
					t.Fatal(err)
				}
			}
			if test.revoked {
				if err := h.native.Release(t.Context(), execution.claim.lease, "cancelled"); err != nil {
					t.Fatal(err)
				}
			}
			h.scheduler.mu.Lock()
			claim := h.scheduler.nativeClaims[issue.ID]
			claim.deadline = time.Now().Add(time.Hour)
			h.scheduler.nativeClaims[issue.ID] = claim
			h.scheduler.mu.Unlock()
			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil && !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
				t.Fatalf("moved item retained transient publication failure: %v", err)
			}
			if !closed || execution.hasWorkers() {
				t.Fatal("moved item retained its worker process")
			}
			recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].ClaimReleasedAt == nil {
				t.Fatalf("moved item retained its execution slot: %+v, %v", recovery.Attempts, err)
			}
			if _, retained := h.scheduler.claims[issue.ID]; retained {
				t.Fatal("moved item retained its scheduling claim")
			}
		})
	}
}

func TestNativeRejectedCompletionReleasesClaim(t *testing.T) {
	for _, test := range []struct{ name, category, fault string }{
		{"event", "event_validation", ""}, {"runtime", "runtime_validation", ""},
		{"execution", "execution_validation", ""}, {"owner", "owner_validation", ""},
		{"usage", "usage_validation", ""}, {"decode", "body_decode", ""},
		{"replacement unavailable", "event_validation", "replacement"},
		{"replacement rate limited", "event_validation", "rate_limit"},
		{"replacement transport", "event_validation", "transport"},
		{"release unavailable", "event_validation", "release"},
		{"accepted runtime unavailable", "event_validation", "lookup"},
	} {
		t.Run(test.name, func(t *testing.T) {
			category := test.category
			h := newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"Done"}},
				{Name: "Done", Terminal: true},
			}, true)
			issue := h.createInProgress(t, "completion")
			h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			if err := execution.PrepareFinish(t.Context(), "succeeded", "private completion text", nil); err != nil {
				t.Fatal(err)
			}
			field := map[string]string{"event_validation": "data.outcome", "runtime_validation": "data.runtime.activity", "execution_validation": "data.finalization", "owner_validation": "data.finalization", "usage_validation": "data.usage"}[category]
			want := tracker.PublicationRejection{Category: category, Field: field}
			client := execution.claim.source.client.client
			next := client.httpClient.Transport
			var events []tracker.NativeRunEvent
			faulted := false
			client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if strings.HasSuffix(request.URL.Path, "/events") {
					body, err := io.ReadAll(request.Body)
					if err != nil {
						t.Fatal(err)
					}
					request.Body = io.NopCloser(strings.NewReader(string(body)))
					var event tracker.NativeRunEvent
					if err := json.Unmarshal(body, &event); err != nil {
						t.Fatal(err)
					}
					events = append(events, event)
					if len(events) == 1 {
						payload, err := json.Marshal(map[string]any{"code": "invalid_request", "message": "private refusal text", "details": map[string]any{"rejection": want}})
						if err != nil {
							t.Fatal(err)
						}
						return &http.Response{StatusCode: http.StatusUnprocessableEntity, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(payload))), Request: request}, nil
					}
				}
				if !faulted && (test.fault == "lookup" && strings.Contains(request.URL.Path, "/attempts/") || test.fault == "release" && strings.HasSuffix(request.URL.Path, "/release") || (test.fault == "replacement" || test.fault == "transport" || test.fault == "rate_limit") && len(events) == 2 && strings.HasSuffix(request.URL.Path, "/events")) {
					faulted = true
					if test.fault == "transport" {
						return nil, errors.New("connection reset by peer")
					}
					status := http.StatusServiceUnavailable
					if test.fault == "rate_limit" {
						status = http.StatusTooManyRequests
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(`{"code":"unavailable"}`)), Request: request}, nil
				}
				return next.RoundTrip(request)
			})
			err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed")
			if test.fault != "" {
				if !faulted || err == nil || h.scheduler.claims[issue.ID].ID == "" {
					t.Fatalf("outage did not preserve claim: %v", err)
				}
				err = h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed")
			}
			var rejection *nativeCompletionRejection
			if !errors.As(err, &rejection) || rejection.failure.Rejection == nil || *rejection.failure.Rejection != want {
				t.Fatalf("completion rejection = %v", err)
			}
			wantEvents := 2
			if test.fault == "replacement" || test.fault == "transport" || test.fault == "rate_limit" {
				wantEvents = 3
			}
			if len(events) != wantEvents || events[0].IdempotencyKey == events[1].IdempotencyKey || events[1].Data.Sequence != events[0].Data.Sequence || events[1].Data.Outcome != "failed" || events[1].Data.CompletionBody != "" {
				t.Fatalf("refused completion was not replaced: %+v", events)
			}
			if wantEvents == 3 && events[1].IdempotencyKey != events[2].IdempotencyKey {
				t.Fatal("replacement retry changed idempotency key")
			}
			evidence, err := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(issue.ID), "")
			if err != nil || evidence.Attempt == nil {
				t.Fatalf("attempt = %+v, %v", evidence, err)
			}
			attempt := evidence.Attempt
			if attempt.Status != "failed" || attempt.ClaimReleasedAt == nil || attempt.TerminalFailure == nil || attempt.TerminalFailure.Rejection == nil || *attempt.TerminalFailure.Rejection != want {
				t.Fatalf("terminal failure not recorded: %+v", attempt)
			}
			if _, retained := h.scheduler.claims[issue.ID]; retained {
				t.Fatal("terminal completion retained claim")
			}
			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || len(events) != wantEvents {
				t.Fatalf("refused event retried: %v, %d", err, len(events))
			}
		})
	}
}
