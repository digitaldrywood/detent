package nativechange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeClaimLossReleasesStoppedWorker(t *testing.T) {
	for _, test := range []struct {
		name           string
		cleanupFailure bool
		controller     bool
		prepared       bool
		retained       bool
		terminal       bool
		expired        bool
		unavailable    bool
		superseded     bool
		releaseOutage  bool
		conflict       bool
		unreleased     bool
		preProvider    bool
	}{
		{name: "worker stopped"},
		{name: "cleanup failure", cleanupFailure: true},
		{name: "release outage retains cleanup ownership", releaseOutage: true},
		{name: "controller outlives worker", controller: true},
		{name: "superseded lease preserves its successor", controller: true, superseded: true},
		{name: "prepared completion without publisher", controller: true, prepared: true},
		{name: "prepared completion settles after claim loss", controller: true, prepared: true, retained: true},
		{name: "released completion reconciles a conflicting pending event", controller: true, prepared: true, retained: true, conflict: true},
		{name: "released setup reconciles a conflicting pending event", controller: true, prepared: true, retained: true, conflict: true, preProvider: true},
		{name: "conflicting pending event preserves an unreleased lease", controller: true, prepared: true, retained: true, conflict: true, unreleased: true},
		{name: "terminal item with unavailable settlement", controller: true, prepared: true, retained: true, terminal: true, unavailable: true},
		{name: "expired lease with unavailable settlement", controller: true, prepared: true, retained: true, expired: true, unavailable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newNativeChangeHub(t, true)
			issue := h.createInProgress(t, "Claim cleanup")
			h.claim(t, issue.ID)
			e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			if test.preProvider {
				at := time.Now().UTC()
				if err := e.ObserveRuntime(t.Context(), tracker.NativeRuntimeObservation{LocalAttemptID: 42, Generation: 2, Phase: "completed", HeartbeatAt: at}); err != nil {
					t.Fatal(err)
				}
				if err := e.PrepareFinish(t.Context(), "failed", "", &tracker.NativeTerminalFailure{Error: "setup failure", ErrorClass: "workspace_hook", ObservedAt: at}); err != nil {
					t.Fatal(err)
				}
				transport := &executionTransport{next: fixtureClient(h.native.Transport()).requests.Load()}
				fixtureClient(h.native.Transport()).requests.Store(transport)
				t.Cleanup(func() { fixtureClient(h.native.Transport()).requests.Store(transport.next) })
				transport.drop.Store(true)
				if err := e.Finish(t.Context(), "failed"); err == nil || e.pending == nil {
					t.Fatal("setup failure did not retain its lost acknowledgment")
				}
				e.pending.Data.Runtime.Pipeline = []gate.PipelineTiming{(gate.CommandResult{Command: "go test ./internal/example", StartedAt: at, FinishedAt: at.Add(time.Second)}).PipelineTiming("finalization")}
			} else if err := e.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			closed := false
			var stop func()
			if test.controller {
				_, release, err := e.Guard(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				stop = release
				defer func() {
					if stop != nil {
						stop()
					}
				}()
			}
			if test.prepared {
				e.preparedOutcome = "failed"
				if test.retained {
					e.RetainCompletion()
				}
				if !test.unreleased {
					if err := h.native.Release(t.Context(), e.claim.lease, "cancelled"); err != nil {
						t.Fatal(err)
					}
				}
			}
			var successor tracker.NativeLease
			if test.superseded {
				if err := h.native.ReleaseAttempt(t.Context(), e.claim.lease, "cancelled", true); err != nil {
					t.Fatal(err)
				}
				var err error
				successor, err = h.native.Claim(t.Context(), tracker.NativeClaim{PolicyID: h.descriptor.ID, WorkItemID: tracker.NativeWorkItemID(issue.ID), MachineID: fixtureSchedulerMachine(h.scheduler).ID, SessionID: "successor-session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
				if err != nil || successor.FencingToken <= e.claim.lease.FencingToken {
					t.Fatalf("claiming successor: lease=%+v error=%v", successor, err)
				}
			}
			fail := test.cleanupFailure
			if err := e.KeepWorker(func(context.Context) error {
				if fail {
					return errors.New("worker still alive")
				}
				closed = true
				return nil
			}, nil); err != nil {
				t.Fatal(err)
			}
			if test.terminal {
				item, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
				if err != nil {
					t.Fatal(err)
				}
				item, err = h.admin.Transition(t.Context(), item.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: item.Revision, State: "Done", Reason: "user_requested"})
				if err != nil || !item.Terminal {
					t.Fatalf("terminal transition = %+v, %v", item, err)
				}
			}
			if !test.expired {
				var err error
				if test.terminal {
					_, err = h.scheduler.RenewClaim(t.Context(), issue.ID, time.Now())
				} else {
					err = h.scheduler.ExecutionError(t.Context(), issue.ID, e.claim.lease.FencingToken, orchestrator.ErrSchedulingClaimLost)
				}
				if !errors.Is(err, orchestrator.ErrSchedulingClaimLost) || !e.hasWorkers() || len(fixtureClaims(h.scheduler)) != 1 {
					t.Fatalf("live worker lost ownership: %v", err)
				}
			}
			if test.cleanupFailure {
				if err := e.CloseWorkers(t.Context()); err == nil || !e.hasWorkers() || len(fixtureClaims(h.scheduler)) != 1 {
					t.Fatalf("unconfirmed cleanup dropped ownership: %v", err)
				}
				fail = false
			}
			transport := fixtureClient(h.native.Transport()).requests.Load()
			if test.releaseOutage {
				fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if strings.HasSuffix(request.URL.Path, "/release") {
						return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"unavailable"}`)), Request: request}, nil
					}
					return transport.RoundTrip(request)
				}))
				t.Cleanup(func() { fixtureClient(h.native.Transport()).requests.Store(transport) })
			}
			if err := e.CloseWorkers(t.Context()); err != nil {
				t.Fatal(err)
			}
			if test.releaseOutage {
				if claim, ok := fixtureClaims(h.scheduler)[issue.ID]; !ok || !claim.lost || claim.lease.ID != e.claim.lease.ID || e.hasWorkers() {
					t.Fatal("unconfirmed Hub release discarded cleanup ownership")
				}
				fixtureClient(h.native.Transport()).requests.Store(transport)
				h.scheduler.ForgetClaim(issue.ID)
			}
			if stop != nil {
				if len(fixtureClaims(h.scheduler)) != 1 {
					t.Fatal("live controller lost ownership before it stopped")
				}
				stop()
				stop = nil
			}
			if test.retained {
				if len(fixtureClaims(h.scheduler)) != 1 {
					t.Fatal("claim loss discarded the prepared completion publisher")
				}
				if test.conflict && !test.preProvider {
					data := e.data
					data.Sequence++
					e.pending = &tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: data.AttemptID + ":" + strconv.FormatInt(data.Sequence, 10)}, Type: "run.observed", SchemaVersion: 1, Data: data}
					original := fixtureClient(h.native.Transport()).requests.Load()
					fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						if strings.HasSuffix(request.URL.Path, "/events") {
							raw, err := io.ReadAll(request.Body)
							if err != nil {
								return nil, err
							}
							request.Body = io.NopCloser(strings.NewReader(string(raw)))
							var event tracker.NativeRunEvent
							if err := json.Unmarshal(raw, &event); err != nil {
								return nil, err
							}
							if event.Type == "run.observed" {
								return &http.Response{StatusCode: http.StatusConflict, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"idempotency_conflict","message":"different content"}`)), Request: request}, nil
							}
						}
						return original.RoundTrip(request)
					}))
					t.Cleanup(func() { fixtureClient(h.native.Transport()).requests.Store(original) })
					if test.unreleased {
						closed = false
						if err := e.KeepWorker(func(context.Context) error { closed = true; return nil }, nil); err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Second)
							defer cancel()
							if err := e.CloseWorkers(ctx); err != nil {
								t.Error(err)
							}
						})
						if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); hubErrorCode(err) != "idempotency_conflict" {
							t.Fatalf("unresolved publication = %v", err)
						}
						if _, err := h.native.ValidateLease(t.Context(), e.claim.lease); err != nil || len(fixtureClaims(h.scheduler)) != 1 || e.pending == nil {
							t.Fatalf("conflict discarded current authority: %v", err)
						}
						if closed || !e.hasWorkers() {
							t.Fatal("unreleased completion closed its source publisher")
						}
						return
					}
				}
				if test.unavailable {
					transport := &executionTransport{next: fixtureClient(h.native.Transport()).requests.Load()}
					fixtureClient(h.native.Transport()).requests.Store(transport)
					t.Cleanup(func() { fixtureClient(h.native.Transport()).requests.Store(transport.next) })
					transport.down.Store(true)
					if test.expired {
						claim := fixtureClaims(h.scheduler)[issue.ID]
						before := claim.deadline.Add(h.scheduler.LeaseDuration() - time.Nanosecond)
						fixtureSetNow(h.scheduler, before)
						if len(fixtureDiagnostics(h.scheduler, e.claim.source)) != 1 {
							t.Fatal("claim diagnostics expired before the TTL boundary")
						}
						after := claim.deadline.Add(h.scheduler.LeaseDuration() + time.Nanosecond)
						fixtureSetNow(h.scheduler, after)
						if fixtureClaims(h.scheduler)[issue.ID].lost {
							t.Fatal("expiry fixture already reported claim loss")
						}
					}
					if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); !nativeTransportUnavailable(err) {
						t.Fatalf("unavailable settlement = %v", err)
					}
					if claim, ok := fixtureClaims(h.scheduler)[issue.ID]; !ok || claim.execution != e || !e.completionOwner.Load() || e.hasWorkers() {
						t.Fatal("unavailable settlement lost its stopped completion publisher")
					}
					if records := fixtureDiagnostics(h.scheduler, e.claim.source); len(records) != 0 {
						t.Fatalf("stopped terminal or expired attempt reported as a native claim: %+v", records)
					}
					transport.down.Store(false)
				}
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
					t.Fatal(err)
				}
			}
			if !closed || e.hasWorkers() || len(fixtureClaims(h.scheduler)) != 0 || len(fixtureDiagnostics(h.scheduler, e.claim.source)) != 0 {
				t.Fatal("stopped worker retained its lost claim")
			}
			evidence, err := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(issue.ID), "")
			if err != nil || evidence.Attempt == nil {
				t.Fatalf("reading stopped attempt: %v", err)
			}
			wantStatus := "cancelled"
			if test.preProvider {
				wantStatus = "failed"
				if evidence.Attempt.Identity != nil || evidence.Attempt.TerminalFailure == nil || evidence.Attempt.TerminalFailure.ErrorClass != "workspace_hook" {
					t.Fatal("released setup lost its original pre-provider failure")
				}
			}
			if evidence.Attempt.ClaimReleasedAt == nil || evidence.Attempt.Status != wantStatus {
				t.Fatalf("stopped worker retained its Hub lease: lease=%+v attempt=%+v", evidence.CurrentLease, evidence.Attempt)
			}
			if test.superseded {
				if evidence.CurrentLease == nil || evidence.CurrentLease.ID != successor.ID {
					t.Fatalf("cleanup released the successor: %+v", evidence.CurrentLease)
				}
				if _, err := h.native.ValidateLease(t.Context(), successor); err != nil {
					t.Fatalf("successor lost execution authority: %v", err)
				}
			} else if evidence.CurrentLease != nil {
				t.Fatalf("stopped worker retained its Hub lease: %+v", evidence.CurrentLease)
			}
		})
	}
}
func TestPreProviderCancellationRecordsReleaseReason(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name           string
		reason         string
		runtime        bool
		unstarted      bool
		partialRuntime bool
	}{
		{name: "dispatch_deferred", reason: "dispatch_deferred", runtime: true},
		{name: "provider_capacity_unavailable", reason: "provider_capacity_unavailable", runtime: true},
		{name: "dispatch_deferred_without_runtime", reason: "dispatch_deferred"},
		{name: "provider_capacity_unavailable_without_runtime", reason: "provider_capacity_unavailable"},
		{name: "unstarted deferred claim", reason: "dispatch_deferred", unstarted: true},
		{name: "unstarted released claim", reason: "released", unstarted: true},
		{name: "partial recovery observation", reason: "dispatch_deferred", runtime: true, partialRuntime: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var events []tracker.NativeRunEvent
			released := false
			var release tracker.NativeLeaseMutation
			client, err := newTestClient(t, Config{URL: "https://hub.example.test", TokenSource: func() string { return "fixture-token" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				body := "{}"
				switch {
				case request.Method == http.MethodGet && request.URL.Path == "/api/v2/capabilities":
					body = `{"features":["native_pre_provider_failure"]}`
				case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events"):
					var event tracker.NativeRunEvent
					if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
						return nil, err
					}
					events = append(events, event)
				case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/release"):
					if err := json.NewDecoder(request.Body).Decode(&release); err != nil {
						return nil, err
					}
					released = true
				default:
					return nil, errors.New("unexpected cancellation fixture request")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			at := time.Date(2026, 10, 31, 15, 14, 0, 0, time.UTC)
			lease := tracker.NativeLease{ID: "lease_example", WorkItemID: "wi_example", FencingToken: 7, PolicyID: "policy_example"}
			scheduler, err := newTestScheduler(client, SchedulerConfig{Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}, Now: func() time.Time { return at }, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			claim := nativeClaim{source: fixtureConnector(native), lease: lease, deadline: at.Add(time.Minute)}
			execution := &nativeExecution{scheduler: authority(scheduler), claim: claim, data: tracker.NativeRunData{AttemptID: "attempt_example", RunID: "run_example", LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: lease.PolicyID, Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: 42, Generation: 1, Phase: "completed", HeartbeatAt: at}}}
			if test.partialRuntime {
				execution.data.Runtime.Phase = ""
				execution.data.Runtime.HeartbeatAt = time.Time{}
				execution.data.Runtime.Phases = []tracker.NativePhase{{Name: "implementation", StartedAt: at.Add(-time.Second)}, {}}
			}
			if !test.runtime {
				observation := *execution.data.Runtime
				execution.data.Runtime = nil
				if !test.unstarted {
					if err := execution.ObserveRuntime(t.Context(), observation); err != nil {
						t.Fatal(err)
					}
				}
			}
			claim.execution = execution
			fixtureInstallClaim(t, scheduler, claim, policy.Descriptor{}, "project")
			if err := scheduler.ReleaseClaim(t.Context(), string(lease.WorkItemID), test.reason); err != nil {
				t.Fatal(err)
			}
			if !released || release.Reason != "released" || release.FencingToken != lease.FencingToken || release.CancelAttempt {
				t.Fatalf("release lost execution fencing: released=%v mutation=%+v", released, release)
			}
			if test.unstarted {
				if len(events) != 0 {
					t.Fatalf("unstarted claim published execution events: %+v", events)
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("pre-provider cancellation missing: released=%v events=%+v", released, events)
			}
			event := events[0]
			if event.Type != "run.finished" || event.Data.Outcome != "cancelled" || event.Data.Identity != nil || event.Data.TerminalFailure == nil || event.Data.TerminalFailure.Stage != "pre_provider" || !strings.HasSuffix(event.Data.TerminalFailure.Error, "release reason: "+test.reason) {
				t.Fatalf("pre-provider cancellation lost its cause: %+v", event.Data)
			}
			if event.Data.LeaseID != lease.ID || event.Data.FencingToken != 7 || event.Data.Sequence != 1 {
				t.Fatalf("cancellation lost execution fencing: %+v", event.Data)
			}
			if (event.Data.Runtime != nil) != test.runtime {
				t.Fatalf("cancellation changed runtime presence: %+v", event.Data.Runtime)
			}
			if runtime := event.Data.Runtime; runtime != nil {
				if runtime.Phase != "completed" || !runtime.HeartbeatAt.Equal(at) {
					t.Fatalf("cancellation retained an incomplete runtime: %+v", runtime)
				}
				for _, phase := range runtime.Phases {
					if phase.Name == "" {
						t.Fatal("cancellation published an unobserved phase")
					}
				}
				if test.partialRuntime && (len(runtime.Phases) != 1 || runtime.Phases[0].Name != "implementation" || runtime.PhasesDropped != 1) {
					t.Fatalf("cancellation lost recorded phase history: %+v", runtime)
				}
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
		age          time.Duration
		terminal     bool
		landed       bool
		lane         string
		dependency   bool
		pending      bool
		controller   bool
		stopped      bool
		prepared     bool
		outage       bool
		exhausted    bool
		localWorker  bool
		staleProcess bool
		recovered    bool
		wantFailure  bool
		wantRetained bool
		wantRenewed  bool
		wantFinished bool
	}{
		{name: "live worker", phase: "implementation", localWorker: true, wantRenewed: true},
		{name: "fresh reported worker", phase: "implementation", wantRenewed: true},
		{name: "quiet long running merge worker", phase: "merging", age: 6 * time.Hour, localWorker: true, wantRenewed: true},
		{name: "local process identity no longer matches", phase: "implementation", localWorker: true, staleProcess: true, wantRetained: true},
		{name: "controller receives completion handoff", phase: "completed", age: 89 * time.Second, controller: true, wantRenewed: true},
		{name: "live controller finishes after the runtime heartbeat expires", phase: "completed", age: 20 * time.Minute, controller: true, wantRenewed: true, wantFinished: true},
		{name: "controller stopped without prepared outcome", phase: "completed", controller: true, stopped: true},
		{name: "prepared outcome awaiting publication", phase: "completed", age: 2 * time.Minute, controller: true, stopped: true, prepared: true, pending: true, wantRenewed: true},
		{name: "completed worker retains fresh cleanup", phase: "completed", localWorker: true, staleProcess: true, wantRenewed: true},
		{name: "completed worker retains long finalization cleanup", phase: "completed", age: 20 * time.Minute, localWorker: true, staleProcess: true, wantRenewed: true, wantFinished: true},
		{name: "publication outage retains completed result", phase: "completed", age: 2 * time.Minute, controller: true, stopped: true, prepared: true, outage: true, wantRenewed: true, wantFinished: true},
		{name: "publication outage bound releases completion", phase: "completed", age: 2 * time.Minute, controller: true, stopped: true, prepared: true, outage: true, exhausted: true, wantRetained: true, wantFailure: true},
		{name: "dead completed worker expires", phase: "completed", age: 20 * time.Minute, wantRetained: true, wantFailure: true},
		{name: "prepared outcome never published", phase: "completed", age: 2 * time.Minute, controller: true, stopped: true, prepared: true, wantRetained: true, wantFailure: true},
		{name: "worker exited after completion", phase: "completed"},
		{name: "worker heartbeat stale", phase: "implementation", age: 90 * time.Second},
		{name: "terminal work item", phase: "implementation", terminal: true},
		{name: "terminal work item retains running worker cleanup", phase: "implementation", terminal: true, localWorker: true, wantRetained: true},
		{name: "landing attempt finishes terminal item", phase: "merging", terminal: true, landed: true, wantRenewed: true},
		{name: "landing receipt does not renew dead worker", phase: "merging", terminal: true, landed: true, age: 90 * time.Second},
		{name: "landing receipt does not renew mismatched process", phase: "merging", terminal: true, landed: true, localWorker: true, staleProcess: true, wantRetained: true},
		{name: "recovered landing attempt finishes terminal item", phase: "merging", terminal: true, landed: true, recovered: true, wantRenewed: true},
		{name: "recovered landing receipt does not renew stale worker", phase: "merging", terminal: true, landed: true, recovered: true, age: 90 * time.Second},
		{name: "landing receipt preserves operator hold", phase: "merging", lane: "Blocked", landed: true},
		{name: "recovered completed worker", phase: "completed", recovered: true},
		{name: "recovered stale worker", phase: "implementation", age: 90 * time.Second, recovered: true},
		{name: "recovered operator hold", phase: "implementation", lane: "Blocked", recovered: true},
		{name: "recovered custom hold", phase: "implementation", lane: "Waiting", recovered: true},
		{name: "recovered unmet dependency", phase: "implementation", dependency: true, recovered: true},
		{name: "recovered rework", phase: "implementation", lane: "Rework", recovered: true, wantRenewed: true},
		{name: "live operator hold retains cleanup", phase: "implementation", lane: "Blocked", localWorker: true, wantRetained: true},
		{name: "host finalization retains ownership", phase: "completed", age: 2 * time.Minute, pending: true, wantRenewed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
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
			fixtureSetNow(h.scheduler, now)
			if test.wantFailure || test.wantFinished {
				fixtureSetNow(h.scheduler, now.Add(-test.age))
				role := "implement"
				if test.outage && test.exhausted {
					role = runner.RoleCode
				}
				if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: role, Backend: "codex", Model: "test"}); err != nil {
					t.Fatal(err)
				}
			}
			fixtureSetNow(h.scheduler, now)
			observation := tracker.NativeRuntimeObservation{LocalAttemptID: 42, Generation: 1, Phase: test.phase, HeartbeatAt: now.Add(-test.age), WorkerLaunch: &tracker.NativeWorkerLaunch{PID: identity.PID, GroupID: identity.GroupID, ProcessStart: identity.StartedAt, PolicyID: h.descriptor.ID, ExecutionVersion: "test", ControlVersion: "test", ExecutionCommit: strings.Repeat("a", 40), ExecutionSHA256: strings.Repeat("b", 64), Protocol: 1, SelectedAt: now}}
			if test.landed {
				observation.Landing = &tracker.NativeLandingReceipt{Landed: true, ChangeID: "change_landed", VersionID: "version_landed", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
			}
			if test.staleProcess {
				observation.WorkerLaunch.ProcessStart = identity.StartedAt.Add(time.Second)
			}
			if err := execution.ObserveRuntime(t.Context(), observation); err != nil {
				t.Fatal(err)
			}
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
				if test.prepared && (!test.outage || !test.exhausted) {
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
				claim := fixtureClaims(h.scheduler)[issue.ID]
				claim.recovery.Attempts = []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{LeaseID: claim.lease.ID, FencingToken: claim.lease.FencingToken, Runtime: &observation}, Status: "running"}}
				fixtureConfigureScheduler(t, h, func(*SchedulerConfig) {})
				fixtureInstallClaim(t, h.scheduler, nativeClaim{source: claim.source, lease: claim.lease, recovery: claim.recovery, deadline: claim.deadline}, h.descriptor, "local")
			}
			var outageSource *runner.NativeChange
			if test.outage && test.exhausted {
				head := nativeChangeFixtureHead("c")
				execution.SetRepository(nativeChangeRepository)
				execution.SetDiffSource(nativeChangeDiff(head, "README.md"))
				execution.SetChangeSource(func(ctx context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
					return nativeChangeSourceFixture(ctx, t, base, head), nil
				})
				if err := execution.Checkpoint(t.Context(), tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: head, ExternalEffect: "none", EffectState: "none"}); err != nil {
					t.Fatal(err)
				}
				if err := execution.PrepareFinish(t.Context(), "succeeded", "", nil); err != nil {
					t.Fatal(err)
				}
				outageSource = execution.NativeChange()
				if outageSource == nil || outageSource.VersionID == "" {
					t.Fatalf("outage fixture did not publish recoverable source: %+v", outageSource)
				}
			}
			if test.outage {
				execution.preparedOutcome = "succeeded"
				execution.change = &runner.NativeChange{Error: "Hub unavailable"}
				original := fixtureClient(h.native.Transport()).requests.Load()
				fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"unavailable"}`)), Request: request}, nil
				}))
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); !nativeTransportUnavailable(err) {
					t.Fatalf("completion outage = %v", err)
				}
				if test.exhausted {
					synctest.Test(t, func(t *testing.T) {
						elapsed := h.scheduler.LeaseDuration() + time.Second
						time.Sleep(elapsed)
						now = now.Add(elapsed)
						fixtureSetNow(h.scheduler, now)
					})
				}
				fixtureClient(h.native.Transport()).requests.Store(original)
				execution.change = nil
			}
			if test.pending {
				execution.HostOperation("workspace.finalize_native_work", now, nil, true)
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
			before := fixtureClaims(h.scheduler)[issue.ID].lease
			transport := fixtureClient(h.native.Transport()).requests.Load()
			var renewals, releases int
			fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if strings.HasSuffix(request.URL.Path, "/renew") {
					renewals++
				}
				if strings.HasSuffix(request.URL.Path, "/release") {
					releases++
				}
				return transport.RoundTrip(request)
			}))
			renewed, err := h.scheduler.RenewClaim(t.Context(), issue.ID, now)
			if test.wantRenewed {
				if err != nil || !renewed.LeaseRenewedAt.After(before.RenewedAt) {
					t.Fatalf("lease was not renewed: %+v, %v", renewed, err)
				}
				if fixtureClaims(h.scheduler)[issue.ID].lease.FencingToken != before.FencingToken {
					t.Fatal("renewal changed the fencing token")
				}
				if test.wantFinished {
					if err := execution.Finish(t.Context(), "succeeded"); err != nil {
						t.Fatal(err)
					}
					if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
						t.Fatal(err)
					}
					page, err := h.admin.AttemptsDetailPage(t.Context(), tracker.NativeWorkItemID(issue.ID), "", 10)
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
				if _, held := fixtureClaims(h.scheduler)[issue.ID]; held != test.wantRetained {
					t.Fatalf("inactive lease retained = %v, want %v", held, test.wantRetained)
				}
				wantReleases := 0
				if !test.wantRetained {
					wantReleases = 1
				}
				wantRenewals := 0
				if test.exhausted {
					wantRenewals = 1
				}
				if renewals != wantRenewals || releases != wantReleases {
					t.Fatalf("inactive lease received %d renewals and %d releases, want %d and %d", renewals, releases, wantRenewals, wantReleases)
				}
				if test.wantFailure {
					if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "failed"); err != nil {
						t.Fatal(err)
					}
					page, err := h.admin.AttemptsDetailPage(t.Context(), tracker.NativeWorkItemID(issue.ID), "", 10)
					if err != nil {
						t.Fatal(err)
					}
					wantError := "completed worker runtime expired before finalization"
					if test.outage {
						wantError = "Hub unavailable during completion finalization"
						if page.Items[0].TerminalFailure == nil || page.Items[0].TerminalFailure.Attribution != "instance" {
							t.Fatalf("outage blamed issue: %+v", page.Items[0].TerminalFailure)
						}
					}
					if len(page.Items) != 1 || page.Items[0].Status != "failed" || page.Items[0].TerminalFailure == nil || page.Items[0].TerminalFailure.Stage != "finalization" || page.Items[0].TerminalFailure.Error != wantError || page.Items[0].ClaimReleasedAt == nil {
						t.Fatalf("stalled completion did not settle with its cause: %+v", page.Items)
					}
					if outageSource != nil {
						if _, held := fixtureClaims(h.scheduler)[issue.ID]; held {
							t.Fatal("over-bound outage retained execution claim")
						}
						fixtureSetNow(h.scheduler, time.Time{})
						h.claim(t, issue.ID)
						resumed := h.scheduler.RunExecution(issue.ID).(runner.ChangeSourceExecution)
						recovered, err := resumed.RecoverChangeSource(t.Context())
						if err != nil || recovered.Version.ID != outageSource.VersionID || recovered.Version.HeadSHA != nativeChangeFixtureHead("c") || len(recovered.Bundle) == 0 {
							t.Fatalf("released outage source unavailable for resume: %+v, %v", recovered, err)
						}
						if err := recovered.Version.Source.Validate(recovered.Version.BaseSHA, recovered.Version.HeadSHA, recovered.Bundle); err != nil {
							t.Fatalf("resumed source integrity: %v", err)
						}
					}
				}
			}
		})
	}
}

func TestNativePublicationFailureReleasesMovedItem(t *testing.T) {
	for _, test := range []struct {
		name, state string
		revoked     bool
	}{
		{name: "operator hold", state: "Blocked"},
		{name: "moved to Todo", state: "Todo"},
		{name: "moved to Done", state: "Done"},
		{name: "revoked in place", state: "In Progress", revoked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done", "Todo", "Blocked"}},
				{Name: "In Review", Transitions: []string{"In Progress", "Done"}},
				{Name: "Blocked", Transitions: []string{"Todo"}},
				{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
			}, true)
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

			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil && !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
				t.Fatalf("moved item retained transient publication failure: %v", err)
			}
			current, err = h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil || current.State != test.state {
				t.Fatalf("completion changed operator intent: %+v %v", current, err)
			}
			if !closed || execution.hasWorkers() {
				t.Fatal("moved item retained its worker process")
			}
			recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].ClaimReleasedAt == nil {
				t.Fatalf("moved item retained its execution slot: %+v, %v", recovery.Attempts, err)
			}
			if _, retained := fixtureClaims(h.scheduler)[issue.ID]; retained {
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
			client := execution.claim.source.NativeClient().Transport()
			next := fixtureClient(client).requests.Load()
			var events []tracker.NativeRunEvent
			faulted := false
			fixtureClient(client).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
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
			}))
			err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed")
			if test.fault != "" {
				if !faulted || err == nil || fixtureClaims(h.scheduler)[issue.ID].lease.ID == "" {
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
			if _, retained := fixtureClaims(h.scheduler)[issue.ID]; retained {
				t.Fatal("terminal completion retained claim")
			}
			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || len(events) != wantEvents {
				t.Fatalf("refused event retried: %v, %d", err, len(events))
			}
		})
	}
}

func TestNativeRejectedReviewCheckpointReleasesClaim(t *testing.T) {
	for _, test := range []struct {
		name      string
		field     string
		outage    bool
		batched   bool
		done      bool
		transient bool
	}{
		{name: "validator payload refusal", field: "data.steps.validator"},
		{name: "review refusal without field"},
		{name: "terminal publication outage", field: "data.steps.validator", outage: true},
		{name: "batched observations", field: "data.steps.validator", batched: true},
		{name: "external merge", field: "data.steps.validator", outage: true, done: true},
		{name: "transient checkpoint publication", transient: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"Done"}},
				{Name: "Done", Terminal: true},
			}, true)
			issue := h.createInProgress(t, "Review checkpoint")
			item := tracker.NativeWorkItemID(issue.ID)
			change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: issue.Title})
			if err != nil {
				t.Fatal(err)
			}
			version := h.publish(t, item, change.ID, nativeChangeFixtureHead("b"))
			if _, err := h.admin.ReviewChange(t.Context(), item, change.ID, version.ID, tracker.ReviewChange{Mutation: nativeMutationKey(), Decision: "changes_requested", Body: "Required repair"}); err != nil {
				t.Fatal(err)
			}
			h.claim(t, issue.ID)
			e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			if err := e.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			source := tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: "clean", HeadSHA: version.HeadSHA, WorkspaceDigest: strings.Repeat("b", 64), ExternalEffect: "none", EffectState: "none"}
			if err := e.Checkpoint(t.Context(), source); err != nil {
				t.Fatal(err)
			}
			for _, step := range []tracker.NativeStepReceipt{
				{Step: "validation", Validation: &gate.CommandResult{Command: "make check-fast", HeadSHA: version.HeadSHA, ExitCode: 0}},
				{Step: "security_audit", SecurityAudit: &tracker.SecurityAuditReview{Verdict: "pass", VersionID: version.ID}},
			} {
				step.CompletedAt, step.Source = time.Now().UTC(), source
				step.Change = tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}
				if err := e.RecordStep(t.Context(), step); err != nil {
					t.Fatal(err)
				}
			}
			accepted, err := h.admin.Recovery(t.Context(), item)
			if err != nil {
				t.Fatal(err)
			}
			closed := false
			if err := e.KeepWorker(func(context.Context) error { closed = true; return nil }, nil); err != nil {
				t.Fatal(err)
			}
			if test.batched {
				e.claim.lease.RunDetailBatching = true
				e.data.Runtime = &tracker.NativeRuntimeObservation{Phase: "validation", HeartbeatAt: e.scheduler.now().UTC()}
				e.runtimePublishedAt = e.data.Runtime.HeartbeatAt
				if err := e.append(t.Context(), "run.observed", "", e.data.Handoff); err != nil || len(e.observations) != 1 {
					t.Fatalf("batch observation: %v", err)
				}
			}
			client := e.claim.source.NativeClient().Transport()
			next := fixtureClient(client).requests.Load()
			want := tracker.PublicationRejection{Category: "event_validation", Field: test.field}
			var events []tracker.NativeRunEvent
			outage := test.outage
			fixtureClient(client).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
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
					if event.Type != "run.finished" && test.transient {
						if len(events) == 1 {
							return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"unavailable"}`)), Request: request}, nil
						}
					} else if event.Type != "run.finished" {
						payload, err := json.Marshal(map[string]any{"code": "invalid_request", "message": "private refusal text", "details": map[string]any{"rejection": want}})
						if err != nil {
							t.Fatal(err)
						}
						return &http.Response{StatusCode: http.StatusUnprocessableEntity, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(payload))), Request: request}, nil
					}
					if outage {
						outage = false
						return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"unavailable"}`)), Request: request}, nil
					}
				}
				return next.RoundTrip(request)
			}))
			receipt := tracker.NativeStepReceipt{Step: "validator", CompletedAt: time.Now().UTC(), Source: source, Validator: &gate.ValidatorResult{Submitted: true, Verdict: "pass", Summary: "Review output"}}
			err = e.RecordStep(t.Context(), receipt)
			if err == nil {
				t.Fatal("checkpoint refusal was ignored")
			}
			if test.transient {
				if err := e.flush(t.Context()); err != nil {
					t.Fatal(err)
				}
				recovery, err := h.admin.Recovery(t.Context(), item)
				if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Status != "running" || recovery.Attempts[0].TerminalFailure != nil || len(recovery.Attempts[0].Steps) != 3 {
					t.Fatalf("transient publication did not resume: %+v, %v", recovery, err)
				}
				if len(events) != 2 || events[0].IdempotencyKey != events[1].IdempotencyKey {
					t.Fatal("transient retry changed event identity")
				}
				restored, err := e.Step(t.Context(), receipt)
				if err != nil || !reflect.DeepEqual(restored.Validator, receipt.Validator) {
					t.Fatalf("resumed review output changed: %+v, %v", restored.Validator, err)
				}
				if err := e.PrepareFinish(t.Context(), "failed", "", nil); err != nil {
					t.Fatal(err)
				}
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
					t.Fatal(err)
				}
				return
			}
			if test.done {
				current, err := h.admin.Issue(t.Context(), item)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: "Done", Reason: "user_requested"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.PrepareFinish(t.Context(), "failed", "", nil); err != nil {
				t.Fatal(err)
			}
			err = h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed")
			var rejected *nativeCompletionRejection
			if !errors.As(err, &rejected) || rejected.failure.Rejection == nil || *rejected.failure.Rejection != want {
				t.Fatalf("terminal publication: %v", err)
			}
			if !closed || e.hasWorkers() {
				t.Fatal("terminal attempt retained its worker")
			}
			if _, exists := fixtureClaims(h.scheduler)[issue.ID]; exists {
				t.Fatal("terminal attempt retained its claim")
			}
			recovery, err := h.admin.Recovery(t.Context(), item)
			if err != nil || len(recovery.Attempts) != 1 {
				t.Fatalf("recovery: %+v, %v", recovery, err)
			}
			attempt := recovery.Attempts[0]
			if attempt.Status != "failed" || attempt.ClaimReleasedAt == nil || attempt.TerminalFailure == nil || attempt.TerminalFailure.Rejection == nil || *attempt.TerminalFailure.Rejection != want {
				t.Fatalf("unsettled attempt: %+v", attempt)
			}
			if attempt.TerminalFailure.Stage != "event_publication" || attempt.TerminalFailure.Attribution != "instance" || attempt.TerminalFailure.Error != want.DiagnosticError() || e.data.Outcome != "failed" || e.pending != nil {
				t.Fatalf("local and Hub failure evidence differ: %+v", attempt.TerminalFailure)
			}
			if !reflect.DeepEqual(attempt.Steps, accepted.Attempts[0].Steps) || !reflect.DeepEqual(attempt.Checkpoint, accepted.Attempts[0].Checkpoint) {
				t.Fatal("terminal refusal lost accepted source or receipts")
			}
			detail, err := h.admin.Change(t.Context(), item, change.ID)
			if err != nil || detail.Change.CurrentVersion != version.ID || len(detail.Reviews) != 1 || detail.Reviews[0].Decision != "changes_requested" {
				t.Fatalf("retained Change was altered: %+v, %v", detail, err)
			}
			if test.done && h.state(t, issue.ID) != "Done" {
				t.Fatal("terminal settlement reopened merged item")
			}
			wantEvents := 2
			if test.outage {
				wantEvents++
			}
			if len(events) != wantEvents || events[0].Type != "run.checkpointed" || events[len(events)-1].Type != "run.finished" || events[len(events)-1].Data.Sequence != accepted.Attempts[0].Sequence+1 {
				t.Fatalf("refusal sequence: %+v", events)
			}
			if events[0].IdempotencyKey == events[1].IdempotencyKey || test.outage && events[1].IdempotencyKey != events[2].IdempotencyKey {
				t.Fatal("terminal replacement or retry changed event identity")
			}
		})
	}
}

func TestNativeDeterministicCompletionReleasesCapacity(t *testing.T) {
	for _, test := range []struct {
		name     string
		observed bool
		status   int
		code     string
	}{
		{"lost identical acknowledgment", true, 200, ""},
		{"review observation conflict", true, 409, "run_sequence_conflict"},
		{"changed idempotency payload", true, 409, "idempotency_conflict"},
		{"terminal issue contract rejection", false, 422, "invalid_request"},
		{"review observation validation rejection", true, 422, "invalid_request"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			issue := h.createInProgress(t, "Completion")
			h.claim(t, issue.ID)
			e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			if err := e.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			item := tracker.NativeWorkItemID(issue.ID)
			change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: issue.Title})
			if err != nil {
				t.Fatal(err)
			}
			version := h.publish(t, item, change.ID, nativeChangeFixtureHead("b"))
			source := tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: "clean", HeadSHA: version.HeadSHA, WorkspaceDigest: strings.Repeat("b", 64), ExternalEffect: "none", EffectState: "none"}
			if err := e.Checkpoint(t.Context(), source); err != nil {
				t.Fatal(err)
			}
			publication := tracker.NativePRPublication{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA, Repository: "https://github.com/owner/repository", Branch: "work/completion", External: tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: "https://github.com/owner/repository/pull/7"}}
			e.publication = &publication
			client := e.claim.source.NativeClient().Transport()
			next := fixtureClient(client).requests.Load()
			committed := false
			replacementInterrupted := false
			var retries []tracker.NativeRunEvent
			var original *tracker.NativeRunEvent
			fixtureClient(client).requests.Store(executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/events") {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					r.Body = io.NopCloser(strings.NewReader(string(body)))
					var event tracker.NativeRunEvent
					if err := json.Unmarshal(body, &event); err != nil {
						t.Fatal(err)
					}
					if original == nil {
						original = &event
					}
					if test.status == 200 {
						retries = append(retries, event)
						response, err := next.RoundTrip(r)
						if err != nil {
							return nil, err
						}
						if !committed {
							committed = true
							response.Body.Close()
							return nil, errors.New("acknowledgment lost")
						}
						return response, nil
					}
					if test.status == 422 && strings.HasSuffix(event.IdempotencyKey, ":rejected") && !replacementInterrupted {
						replacementInterrupted = true
						return nil, errors.New("replacement transport unavailable")
					}
					if test.status == 409 {
						if !committed {
							other := event
							if test.code != "idempotency_conflict" {
								other.IdempotencyKey += ":other"
							}
							other.Type = "run.observed"
							runtime := *other.Data.Runtime
							runtime.HeartbeatAt = runtime.HeartbeatAt.Add(time.Second)
							runtime.Phase = "implementation"
							other.Data.Runtime = &runtime
							encoded, err := json.Marshal(other)
							if err != nil {
								t.Fatal(err)
							}
							request := r.Clone(r.Context())
							request.Body = io.NopCloser(strings.NewReader(string(encoded)))
							request.ContentLength = int64(len(encoded))
							response, err := next.RoundTrip(request)
							if err != nil {
								t.Fatal(err)
							}
							response.Body.Close()
							if response.StatusCode != 200 {
								t.Fatalf("conflicting writer: %d", response.StatusCode)
							}
							committed = true
						}
						return next.RoundTrip(r)
					}
					return &http.Response{StatusCode: test.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"` + test.code + `","message":"Add or complete these required issue sections: Invariants touched, Safety regression"}`)), Request: r}, nil
				}
				return next.RoundTrip(r)
			}))
			if test.observed {
				e.mu.Lock()
				e.data.Runtime = &tracker.NativeRuntimeObservation{Phase: "validation", LocalAttemptID: 42, Generation: 1, HeartbeatAt: time.Now().UTC()}
				err := e.append(t.Context(), "run.observed", "", nil, true)
				e.mu.Unlock()
				if err == nil {
					t.Fatal("conflict ignored")
				}
			}
			if test.observed {
				e.data.Runtime.Phase = "completed"
			}
			_ = e.PrepareFinish(t.Context(), "failed", "", nil)
			if test.status == 422 && !test.observed {
				if err := e.Finish(t.Context(), "failed"); err == nil {
					t.Fatal("missing terminal refusal")
				}
			}
			if test.status != 200 {
				saved := e.CompletionState()
				fixtureConfigureScheduler(t, h, func(*SchedulerConfig) {})
				if _, err := h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor}, issue, saved); err != nil {
					t.Fatal(err)
				}
				e = h.scheduler.RunExecution(issue.ID).(*nativeExecution)
				if e.publication == nil || *e.publication != publication {
					t.Fatal("restart lost PR publication identity")
				}
			}
			if test.status == 200 {
				if len(retries) != 2 || !reflect.DeepEqual(retries[0], retries[1]) {
					t.Fatalf("identical replay changed payload: %+v", retries)
				}
				recovery, err := h.admin.Recovery(t.Context(), item)
				if err != nil || recovery.Attempts[0].Sequence != original.Data.Sequence {
					t.Fatalf("identical replay consumed another sequence: %+v %v", recovery, err)
				}
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
					t.Fatal(err)
				}
				return
			}
			if test.status == 409 {
				recovery, err := h.admin.Recovery(t.Context(), item)
				if err != nil || len(recovery.Attempts) != 1 {
					t.Fatalf("read conflicting attempt: %v", err)
				}
				attempt := recovery.Attempts[0]
				if attempt.Sequence != original.Data.Sequence || attempt.Outcome != "" || attempt.Runtime == nil || attempt.Runtime.Phase != "implementation" {
					t.Fatalf("conflict overwrote another writer or fabricated success: %+v", attempt)
				}
			}
			err = h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed")
			var rejection *nativeCompletionRejection
			if !errors.As(err, &rejection) {
				t.Fatalf("deterministic rejection did not settle: %v", err)
			}
			if test.status == 422 && !strings.Contains(rejection.failure.Error, "Invariants touched") {
				t.Fatalf("issue contract diagnostic lost: %s", rejection.failure.Error)
			}
			var saved nativeCompletionState
			if err := json.Unmarshal(e.CompletionState(), &saved); err != nil {
				t.Fatal(err)
			}
			retained := saved.Rejected
			if test.status == 409 {
				retained = saved.Pending
			}
			if !reflect.DeepEqual(retained, original) {
				t.Fatal("rejection lost original event identity")
			}
			if _, ok := fixtureClaims(h.scheduler)[issue.ID]; ok {
				t.Fatal("completed execution retained capacity")
			}
			recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
			detail, changeErr := h.admin.Change(t.Context(), item, change.ID)
			if changeErr != nil || detail.Change.CurrentVersion != version.ID {
				t.Fatalf("published source changed: %+v %v", detail, changeErr)
			}
			if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].ClaimReleasedAt == nil || recovery.Attempts[0].Checkpoint == nil || !reflect.DeepEqual(*recovery.Attempts[0].Checkpoint, source) {
				t.Fatalf("lease retained: %+v %v", recovery, err)
			}
		})
	}
}
func exerciseNativeExecution(t *testing.T, scheduler *Scheduler, native *NativeClient, issueID string) {
	t.Helper()
	transport := &executionTransport{next: fixtureClient(native.Transport()).requests.Load()}
	fixtureClient(native.Transport()).requests.Store(transport)
	t.Cleanup(func() { fixtureClient(native.Transport()).requests.Store(transport.next) })
	execution := scheduler.RunExecution(issueID)
	if execution == nil {
		t.Fatal("native claim has no execution lifecycle")
	}
	guarded, stop, err := execution.Guard(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if len(execution.Recovery().Discussion) != 1 {
		t.Fatal("native recovery replayed historical worker discussion")
	}
	identity := tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}
	// The stored attempt diff rides every checkpoint and the finish.
	// The source is installed the way the runner
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
				execution.(*nativeExecution).ProviderCompleted(scheduler.Now())
			}
			transport.drop.Store(true)
			err := test.operation()
			if test.name == "checkpoint" {
				if err != nil || execution.(*nativeExecution).pending == nil {
					t.Fatalf("checkpoint lost pending acknowledgment: %v", err)
				}
				at := scheduler.Now()
				execution.(*nativeExecution).RecordPipelineTiming((gate.CommandResult{Command: "go test ./internal/example", StartedAt: at, FinishedAt: at.Add(time.Second)}).PipelineTiming("finalization"))
			} else if err == nil {
				t.Fatal("acknowledgment loss was not injected")
			}
			if test.name == "finish" {
				owner := execution.(*nativeExecution)
				pending, sequence := owner.pending, owner.data.Sequence
				claims := fixtureDiagnostics(scheduler, owner.claim.source)
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
	if runtime := recovery.Attempts[0].Runtime; runtime == nil || len(runtime.Pipeline) != 1 || runtime.Pipeline[0].Stage != "finalization" {
		t.Fatal("completion lost timing recorded after the checkpoint acknowledgment was lost")
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
			if err := native.Transport().Request(t.Context(), http.MethodGet, native.Scope()+"/attempts/"+attempt+"/diff"+test.query, nil, &stored); err != nil {
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
	if err := native.Transport().Request(t.Context(), http.MethodGet, native.Scope()+"/attempts/"+attempt+"/diff?at=1", nil, &missing); err == nil {
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

func TestNativeCheckpointWithoutReadableDiff(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		state       string
		recovers    bool
		beforeStart bool
	}{
		{state: "dirty"}, {state: "clean"}, {state: "unpushed"}, {state: "dirty", recovers: true}, {state: "dirty", recovers: true, beforeStart: true},
	} {
		state := test.state
		for _, offline := range []bool{false, true} {
			name := fmt.Sprintf("%s/offline=%t", state, offline)
			if test.recovers {
				name += "/recovers"
			}
			if test.beforeStart {
				name += "/before-start"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				h := newNativeChangeHub(t, true)
				issue := h.createInProgress(t, "Recover the checkpoint")
				h.claim(t, issue.ID)
				execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
				if !test.beforeStart {
					if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleRework, Backend: "codex", Model: "test"}); err != nil {
						t.Fatal(err)
					}
				}
				sourceCalls := 0
				execution.SetDiffSource(func(context.Context) (tracker.AttemptDiffRequest, bool) {
					sourceCalls++
					if test.recovers && sourceCalls > 1 {
						return tracker.AttemptDiffRequest{BaseSHA: nativeChangeFixtureHead("a"), HeadSHA: nativeChangeFixtureHead("c"), Files: []tracker.AttemptDiffFile{{Path: "README.md", Status: tracker.DiffStatusModified}}}, true
					}
					return tracker.AttemptDiffRequest{}, false
				})
				transport := &executionTransport{next: fixtureClient(h.native.Transport()).requests.Load()}
				fixtureClient(h.native.Transport()).requests.Store(transport)
				transport.down.Store(offline)
				checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Availability: "available", Storage: "local_only", WorktreeState: state, HeadSHA: nativeChangeFixtureHead("c"), WorkspaceDigest: nativeChangeFixtureHead("d"), ExternalEffect: "none", EffectState: "none"}
				if err := execution.Checkpoint(t.Context(), checkpoint); err != nil {
					t.Fatal(err)
				}
				err := execution.Checkpoint(t.Context(), checkpoint)
				if state == "dirty" {
					if err != nil {
						t.Fatalf("dirty recovery checkpoint requires a final diff: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "final attempt diff is unavailable") {
					t.Fatalf("final %s checkpoint accepted an unavailable diff: %v", state, err)
				}
				if test.recovers && (execution.lastDiff == nil || execution.lastDiff.HeadSHA != checkpoint.HeadSHA || len(execution.lastDiff.Files) != 1) {
					t.Fatalf("readable dirty checkpoint did not retain its initial safe diff: %+v", execution.lastDiff)
				}
				transport.down.Store(false)
				if test.beforeStart {
					if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleRework, Backend: "codex", Model: "test"}); err != nil {
						t.Fatal(err)
					}
				}
				execution.SetDiffSource(nativeChangeDiff(checkpoint.HeadSHA, "README.md"))
				checkpoint.WorktreeState = "unpushed"
				checkpoint.WorkspaceDigest = nativeChangeFixtureHead("e")
				if err := execution.Checkpoint(t.Context(), checkpoint); err != nil {
					t.Fatalf("publish recovered checkpoint: %v", err)
				}
				var stored tracker.AttemptDiff
				err = h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+"/attempts/"+execution.data.AttemptID+"/diff", nil, &stored)
				if err != nil || stored.HeadSHA != checkpoint.HeadSHA || len(stored.Files) != 1 || stored.Files[0].Path != "README.md" {
					t.Fatalf("recovered final diff = %+v, error=%v", stored, err)
				}
			})
		}
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
			execution.claim.lease.RunDetailBatching = false
			expiry := &expiredLeaseHost{ExecutionHost: execution.scheduler.host}
			execution.scheduler.host = expiry
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
				expiry.expired.Store(true)
			}
			original := fixtureClient(h.native.Transport()).requests.Load()
			fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
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
			}))
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
				fixtureClient(h.native.Transport()).requests.Store(original)
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
			fixtureClient(h.native.Transport()).requests.Store(original)
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
			execution.claim.lease.RunDetailBatching = false
			ctx := t.Context()
			if err := execution.Start(ctx, tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			observation := tracker.NativeRuntimeObservation{LocalAttemptID: 42, Generation: 1, Phase: "implementation", HeartbeatAt: time.Now()}
			if err := execution.ObserveRuntime(ctx, observation); err != nil {
				t.Fatal(err)
			}
			before := execution.data
			original := fixtureClient(h.native.Transport()).requests.Load()
			fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(request.URL.Path, "/events") {
					return original.RoundTrip(request)
				}
				return &http.Response{StatusCode: http.StatusUnprocessableEntity, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"invalid_request"}`)), Request: request}, nil
			}))
			observation.Phase = phase
			if err := execution.ObserveRuntime(ctx, observation); err == nil {
				t.Fatal("legacy Hub accepted cache wait")
			}
			if execution.pending == nil {
				t.Fatal("rejected observation was not retained")
			}
			pending := *execution.pending
			fixtureClient(h.native.Transport()).requests.Store(original)
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

func TestCompletedAttemptSurvivesHubOutage(t *testing.T) {
	for _, status := range []int{502, 503, 0, -1} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				descriptor := clientTestPolicy()
				offline := true
				var finished tracker.NativeRunEvent
				client, err := newTestClient(t, Config{URL: "http://hub.example", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if offline {
						if status == -1 {
							return nil, fmt.Errorf("Client.Timeout exceeded while awaiting headers: %w", context.DeadlineExceeded)
						}
						if status == 0 {
							return nil, errors.New("connection refused")
						}
						return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"unavailable"}`)), Request: request}, nil
					}
					body := `{"profile":"native"}`
					if strings.HasSuffix(request.URL.Path, "/capabilities") {
						body = `{"protocol_majors":[2],"event_schema_versions":[1],"features":["native_issues","scoped_collaboration","repository_policy"]}`
					}
					if strings.HasSuffix(request.URL.Path, "/policy") {
						value, _ := json.Marshal(policy.Approval{Policy: descriptor})
						body = string(value)
					}
					if strings.HasSuffix(request.URL.Path, "/renew") {
						value, _ := json.Marshal(tracker.NativeLease{ID: "lease", WorkItemID: "wi_test", FencingToken: 1, PolicyID: descriptor.ID, ServerTime: time.Now(), ExpiresAt: time.Now().Add(10 * time.Minute)})
						body = string(value)
					}
					if strings.HasSuffix(request.URL.Path, "/events") {
						var event tracker.NativeRunEvent
						if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
							return nil, err
						}
						if event.Type == "run.finished" {
							finished = event
						}
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				scheduler, err := newTestScheduler(client, SchedulerConfig{Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}, LeaseTTL: 10 * time.Minute, HeartbeatInterval: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_test", "prj_test")
				if err != nil {
					t.Fatal(err)
				}
				source, _ := NewNativeConnector(native)
				claim := nativeClaim{source: source, lease: tracker.NativeLease{ID: "lease", WorkItemID: "wi_test", FencingToken: 1, PolicyID: descriptor.ID}, deadline: start.Add(10 * time.Minute)}
				fixtureInstallClaim(t, scheduler, claim, descriptor, "project")
				execution := scheduler.RunExecution("wi_test").(*nativeExecution)
				execution.data.Identity = &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}
				execution.data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: start.Add(-11 * time.Minute)}
				checkpoint := &tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "git_ref", Availability: "available", WorktreeState: "clean", Ref: tracker.CheckpointRefPrefix + "completed", CommitSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), HeadSHA: strings.Repeat("a", 40), WorkspaceDigest: strings.Repeat("c", 64), ExternalEffect: "none", EffectState: "none"}
				execution.data.Handoff = checkpoint
				execution.preparedOutcome = "succeeded"
				if err := scheduler.Heartbeat(t.Context()); !nativeTransportUnavailable(err) {
					t.Fatalf("heartbeat outage = %v", err)
				}
				if err := scheduler.ReleaseClaim(t.Context(), "wi_test", "completed"); !nativeTransportUnavailable(err) {
					t.Fatalf("outage = %v", err)
				}
				if execution.pending == nil {
					t.Fatal("completion timeout lost pending event")
				}
				pending := *execution.pending
				deadline := execution.completionOutageDeadline
				time.Sleep(2 * time.Minute)
				if err := scheduler.ReleaseClaim(t.Context(), "wi_test", "completed"); !nativeTransportUnavailable(err) {
					t.Fatalf("retry outage = %v", err)
				}
				if !execution.completionOutageDeadline.Equal(deadline) {
					t.Fatal("retry extended outage bound")
				}
				if execution.pending == nil || execution.pending.IdempotencyKey != pending.IdempotencyKey || !reflect.DeepEqual(execution.data.Handoff, checkpoint) || scheduler.RunExecution("wi_test") != execution {
					t.Fatal("retry replaced completion, checkpoint or execution")
				}
				offline = false
				if err := scheduler.Heartbeat(t.Context()); err != nil {
					t.Fatal(err)
				}
				if _, err := scheduler.RenewClaim(t.Context(), "wi_test", time.Now()); err != nil {
					t.Fatalf("renew completed attempt: %v", err)
				}
				if err := scheduler.ReleaseClaim(t.Context(), "wi_test", "completed"); err != nil {
					t.Fatal(err)
				}
				if finished.Data.Outcome != "succeeded" || finished.Data.TerminalFailure != nil {
					t.Fatalf("completion lost: %+v", finished)
				}
				if !reflect.DeepEqual(finished.Data.Handoff, checkpoint) || finished.Data.LeaseID != "lease" || finished.Data.FencingToken != 1 || finished.IdempotencyKey != pending.IdempotencyKey {
					t.Fatalf("completion retry changed checkpoint or fenced event: %+v", finished)
				}
				if len(fixtureClaims(scheduler)) != 0 {
					t.Fatal("completed claim retained")
				}
			})
		})
	}
}

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

type expiredLeaseHost struct {
	executioncontract.ExecutionHost
	expired atomic.Bool
}

func (h *expiredLeaseHost) RemainingExecution(lease tracker.NativeLease) time.Duration {
	if h.expired.Load() {
		return -time.Second
	}
	return h.ExecutionHost.RemainingExecution(lease)
}
func (h *expiredLeaseHost) RenewExecution(ctx context.Context, lease tracker.NativeLease, capacity int) error {
	err := h.ExecutionHost.RenewExecution(ctx, lease, capacity)
	if err == nil {
		h.expired.Store(false)
	}
	return err
}
