package nativechange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDetachedPublicationStopsOnLeaseLoss(t *testing.T) {
	for _, route := range []string{"phases", "progress", "events", "diff", "pending checkpoint"} {
		event := route == "events" || route == "pending checkpoint"
		finishing := event || route == "diff"
		endpoint := route
		if event {
			endpoint = "events"
		}
		for _, scenario := range []struct {
			name     string
			revoked  bool
			callback bool
			expired  bool
			bare     bool
		}{
			{name: "renewal rejected"},
			{name: "revoked", revoked: true},
			{name: "callback renewal rejected", callback: true},
			{name: "callback revoked", revoked: true, callback: true},
			{name: "expired", expired: true},
			{name: "host finalization revoked", revoked: true, bare: true},
			{name: "host finalization expired after guard stops", expired: true, bare: true},
		} {
			if finishing && scenario.callback || !finishing && scenario.bare {
				continue
			}
			t.Run(route+"/"+scenario.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					start := time.Now()
					lost := errors.New(scenario.name)
					calls := 0
					client, err := newTestClient(t, Config{URL: "https://hub.example.test", TokenSource: func() string { return "fixture" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						response := httptest.NewRecorder()
						switch {
						case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/renew"):
							response.WriteHeader(http.StatusConflict)
							io.WriteString(response, `{"code":"stale_fencing_token","message":"`+scenario.name+`"}`)
						case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/release"):
							response.WriteHeader(http.StatusConflict)
							io.WriteString(response, `{"code":"stale_fencing_token"}`)
						case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/validate"):
							io.WriteString(response, "{}")
						case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/"+endpoint):
							calls++
							if calls == 1 {
								response.Header().Set("Retry-After", "60")
								response.WriteHeader(http.StatusTooManyRequests)
								io.WriteString(response, `{"code":"runner_request_budget"}`)
							} else {
								response.WriteHeader(http.StatusBadRequest)
							}
						default:
							t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
						}
						return response.Result(), nil
					})}})
					if err != nil {
						t.Fatal(err)
					}
					native, err := client.Native("org_example", "prj_example")
					if err != nil {
						t.Fatal(err)
					}
					scheduler, err := newTestScheduler(client, SchedulerConfig{Now: time.Now, HeartbeatInterval: time.Second, LeaseTTL: time.Hour, Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}})
					if err != nil {
						t.Fatal(err)
					}
					descriptor := clientTestPolicy()
					lease := tracker.NativeLease{WorkItemID: "wi_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ID: "lease", FencingToken: 1, PolicyID: descriptor.ID}
					deadline := start.Add(time.Hour)
					if scenario.expired {
						deadline = start.Add(10 * time.Second)
					}
					execution := NewExecution(scheduler, executioncontract.ExecutionClaim{Source: fixtureConnector(native), Lease: lease}).(*nativeExecution)
					fixtureInstallClaim(t, scheduler, nativeClaim{source: fixtureConnector(native), lease: lease, deadline: deadline, execution: execution}, descriptor, "project")
					guarded, stop, err := execution.Guard(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if scenario.bare && scenario.expired {
						stop()
					} else {
						defer stop()
					}
					if scenario.revoked {
						timer := time.AfterFunc(10*time.Second, func() { execution.Cancel(errors.Join(runner.ErrExecutionAuthorityUnavailable, lost)) })
						defer timer.Stop()
					} else if !scenario.expired {
						timer := time.AfterFunc(10*time.Second, func() { _, _ = scheduler.RenewClaimHeartbeat(t.Context(), string(lease.WorkItemID), time.Now()) })
						defer timer.Stop()
					}
					parent := context.WithoutCancel(guarded)
					wantElapsed := 10 * time.Second
					if scenario.bare {
						parent = t.Context()
						if scenario.expired {
							wantElapsed = time.Minute
						}
					}
					finalization, cancel := context.WithTimeout(parent, 3*time.Minute)
					defer cancel()
					var peerContext context.Context
					wantCalls := 1
					if scenario.callback {
						left, right := net.Pipe()
						defer left.Close()
						defer right.Close()
						callback := runner.NewSSHCallbackHandler(runner.RunRequest{Execution: execution}, api.NewRunnerSessions(native), nil, nil)
						central := runner.NewSSHPeer(finalization, left, left, callback)
						worker := runner.NewSSHPeer(finalization, right, right, nil)
						defer central.Close()
						defer worker.Close()
						peerContext = central.Context()
						if route == "phases" {
							err = worker.Call(finalization, "execution.RecordPhaseReceipt", nil, "project_gate", "branch", "head", "tree", "gate")
						} else {
							err = worker.Call(finalization, "execution.RecordRoundVerdict", nil, progresscheck.RoundInput{RoundID: "round"})
						}
					} else if route == "phases" {
						err = execution.RecordPhaseReceipt(finalization, "project_gate", "branch", "head", "tree", "gate")
					} else if route == "progress" {
						err = execution.RecordRoundVerdict(finalization, progresscheck.RoundInput{RoundID: "round"})
					} else {
						execution.data.Identity = &tracker.NativeExecutionIdentity{Role: "triage", Backend: "codex", Model: "example"}
						if event {
							execution.storedSeq = execution.data.Sequence + 1
						} else {
							wantCalls = 0
							execution.SetDiffSource(nativeChangeDiff(nativeChangeFixtureHead("c"), "README.md"))
							client.DeferRequestBudget("/diff", start.Add(time.Minute))
						}
						if route == "pending checkpoint" {
							data := execution.data
							data.Sequence++
							execution.pending = &tracker.NativeRunEvent{
								Mutation: tracker.Mutation{IdempotencyKey: data.AttemptID + ":1"},
								Type:     "run.checkpointed", SchemaVersion: 1, Data: data,
							}
						}
						err = execution.Finish(finalization, "succeeded")
					}
					observedLoss := errors.Is(err, lost)
					if scenario.expired {
						observedLoss = errors.Is(err, runner.ErrExecutionAuthorityUnavailable)
					} else if !scenario.revoked {
						observedLoss = errors.Is(err, runner.ErrExecutionAuthorityUnavailable) && strings.Contains(err.Error(), scenario.name)
					}
					if scenario.callback {
						observedLoss = err != nil && strings.Contains(err.Error(), lost.Error())
						if peerContext.Err() != nil {
							t.Fatalf("callback peer ended: %v", peerContext.Err())
						}
					}
					if !observedLoss || !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || calls != wantCalls || time.Since(start) != wantElapsed || finalization.Err() != nil {
						t.Fatalf("publication returned %v after %s and %d POSTs, finalization error = %v", err, time.Since(start), calls, finalization.Err())
					}
				})
			})
		}
	}
}

type publicationRenewalHost struct {
	executioncontract.ExecutionHost
	err error
}

func (h publicationRenewalHost) RenewExecution(context.Context, tracker.NativeLease, int) error {
	return h.err
}

func TestReviewCheckpointRetriesItsRunDataFence(t *testing.T) {
	for _, step := range []string{"review", "validator"} {
		t.Run(step, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				descriptor := clientTestPolicy()
				lease := tracker.NativeLease{WorkItemID: "wi_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ID: "lease", FencingToken: 17, PolicyID: descriptor.ID}
				var sent []byte
				calls := 0
				client, err := newTestClient(t, Config{URL: "https://hub.example.test", TokenSource: func() string { return "fixture" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					response := httptest.NewRecorder()
					switch {
					case strings.HasSuffix(request.URL.Path, "/validate"):
						io.WriteString(response, "{}")
					case strings.HasSuffix(request.URL.Path, "/events"):
						calls++
						body, err := io.ReadAll(request.Body)
						if err != nil {
							t.Fatal(err)
						}
						var event tracker.NativeRunEvent
						if err := json.Unmarshal(body, &event); err != nil || event.Type != "run.checkpointed" || event.IdempotencyKey == "" ||
							event.LeaseID != "" || event.FencingToken != 0 || event.Data.LeaseID != lease.ID || event.Data.FencingToken != lease.FencingToken {
							t.Fatalf("checkpoint = %+v, %v", event, err)
						}
						if calls == 1 {
							sent = body
							return nil, io.ErrUnexpectedEOF
						}
						if !bytes.Equal(sent, body) {
							t.Fatal("checkpoint replay changed the request")
						}
						response.WriteHeader(http.StatusNoContent)
					default:
						t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
					}
					return response.Result(), nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_example", "prj_example")
				if err != nil {
					t.Fatal(err)
				}
				scheduler, err := newTestScheduler(client, SchedulerConfig{Now: time.Now, HeartbeatInterval: 5 * time.Second, LeaseTTL: 75 * time.Second, Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}})
				if err != nil {
					t.Fatal(err)
				}
				source := fixtureConnector(native)
				fixtureInstallClaim(t, scheduler, nativeClaim{source: source, lease: lease, deadline: start.Add(75 * time.Second)}, descriptor, "project")
				execution := NewExecution(scheduler, executioncontract.ExecutionClaim{Source: source, Lease: lease}).(*nativeExecution)
				execution.storedSeq = execution.data.Sequence + 1
				ctx, stop, err := execution.Guard(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				err = execution.RecordStep(ctx, tracker.NativeStepReceipt{Step: step})
				if err != nil || calls != 2 || execution.data.Sequence != 1 {
					t.Fatalf("checkpoint sent %d POSTs, sequence = %d, result = %v", calls, execution.data.Sequence, err)
				}
			})
		})
	}
}

func TestCheckpointPublishesWhenAttemptDiffStalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		descriptor := clientTestPolicy()
		lease := tracker.NativeLease{WorkItemID: "wi_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ID: "lease", FencingToken: 17, PolicyID: descriptor.ID}
		checkpoint := tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: nativeChangeFixtureHead("c"), ExternalEffect: "none", EffectState: "none"}
		var diffTimedOut bool
		var published tracker.NativeRunEvent
		client, err := newTestClient(t, Config{URL: "https://hub.example.test", TokenSource: func() string { return "fixture" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
			response := httptest.NewRecorder()
			switch {
			case strings.HasSuffix(request.URL.Path, "/validate"):
				io.WriteString(response, "{}")
			case strings.HasSuffix(request.URL.Path, "/diff"):
				<-request.Context().Done()
				diffTimedOut = errors.Is(request.Context().Err(), context.DeadlineExceeded)
				return nil, request.Context().Err()
			case strings.HasSuffix(request.URL.Path, "/events"):
				if err := request.Context().Err(); err != nil {
					return nil, err
				}
				if err := json.NewDecoder(request.Body).Decode(&published); err != nil {
					t.Fatal(err)
				}
				response.WriteHeader(http.StatusNoContent)
			default:
				t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
			}
			return response.Result(), nil
		})}})
		if err != nil {
			t.Fatal(err)
		}
		native, err := client.Native("org_example", "prj_example")
		if err != nil {
			t.Fatal(err)
		}
		scheduler, err := newTestScheduler(client, SchedulerConfig{Now: time.Now, HeartbeatInterval: 5 * time.Second, LeaseTTL: 5 * time.Minute, Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}})
		if err != nil {
			t.Fatal(err)
		}
		source := fixtureConnector(native)
		fixtureInstallClaim(t, scheduler, nativeClaim{source: source, lease: lease, deadline: start.Add(5 * time.Minute)}, descriptor, "project")
		execution := NewExecution(scheduler, executioncontract.ExecutionClaim{Source: source, Lease: lease}).(*nativeExecution)
		execution.data.Identity = &tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}
		execution.data.Sequence = 1
		execution.SetDiffSource(nativeChangeDiff(checkpoint.HeadSHA, "README.md"))
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		if err := execution.Checkpoint(ctx, checkpoint); err != nil {
			t.Fatal(err)
		}
		if !diffTimedOut || ctx.Err() != nil || published.Type != "run.checkpointed" || published.Data.Handoff == nil || *published.Data.Handoff != checkpoint {
			t.Fatalf("checkpoint after stalled diff = %s %+v, diff timed out = %t, checkpoint context = %v", published.Type, published.Data.Handoff, diffTimedOut, ctx.Err())
		}
	})
}

func TestRunnerSessionStartSurvivesHubOutageWithSameFence(t *testing.T) {
	for _, test := range []struct {
		name    string
		ttl     time.Duration
		revoked bool
	}{
		{name: "recovers within lease", ttl: 75 * time.Second},
		{name: "expires during outage", ttl: 8 * time.Second},
		{name: "revoked on reconnect", ttl: 75 * time.Second, revoked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				const outage = 16 * time.Second
				descriptor := clientTestPolicy()
				lease := tracker.NativeLease{WorkItemID: "wi_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ID: "lease", FencingToken: 17, PolicyID: descriptor.ID}
				var sent []byte
				calls := 0
				outageStarted := false
				client, err := newTestClient(t, Config{URL: "https://hub.example.test", TokenSource: func() string { return "fixture" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					response := httptest.NewRecorder()
					if strings.HasSuffix(request.URL.Path, "/runner-sessions") {
						calls++
						body, err := io.ReadAll(request.Body)
						if err != nil {
							t.Fatal(err)
						}
						var mutation runnersession.Mutation
						if err := json.Unmarshal(body, &mutation); err != nil || mutation.Key == "" || mutation.LeaseID != lease.ID || mutation.FencingToken != lease.FencingToken || mutation.Worker == nil {
							t.Fatalf("session mutation = %+v, %v", mutation, err)
						}
						if sent != nil && !bytes.Equal(sent, body) {
							t.Fatal("session replay changed the request")
						}
						sent = body
						if time.Since(start) < outage {
							return nil, io.ErrUnexpectedEOF
						}
						io.WriteString(response, `{"id":42,"revision":1}`)
						return response.Result(), nil
					}
					if outageStarted && time.Since(start) < outage {
						return nil, io.ErrUnexpectedEOF
					}
					switch {
					case strings.HasSuffix(request.URL.Path, "/validate"):
						io.WriteString(response, "{}")
					case test.revoked && (strings.HasSuffix(request.URL.Path, "/renew") || strings.HasSuffix(request.URL.Path, "/release")):
						response.WriteHeader(http.StatusConflict)
						io.WriteString(response, `{"code":"stale_fencing_token"}`)
					case strings.HasSuffix(request.URL.Path, "/renew"):
						confirmed := lease
						confirmed.ServerTime, confirmed.RenewedAt = time.Now(), time.Now()
						confirmed.ExpiresAt = confirmed.ServerTime.Add(test.ttl)
						if err := json.NewEncoder(response).Encode(confirmed); err != nil {
							return nil, err
						}
					default:
						t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
					}
					return response.Result(), nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_example", "prj_example")
				if err != nil {
					t.Fatal(err)
				}
				scheduler, err := newTestScheduler(client, SchedulerConfig{Now: time.Now, HeartbeatInterval: time.Second, LeaseTTL: test.ttl, Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}})
				if err != nil {
					t.Fatal(err)
				}
				source := fixtureConnector(native)
				host := publicationRenewalHost{ExecutionHost: scheduler, err: api.ErrUnavailable}
				execution := NewExecution(host, executioncontract.ExecutionClaim{Source: source, Lease: lease}).(*nativeExecution)
				fixtureInstallClaim(t, scheduler, nativeClaim{source: source, lease: lease, deadline: start.Add(test.ttl), execution: execution}, descriptor, "project")
				guarded, stop, err := execution.Guard(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				outageStarted = true
				heartbeatDone := make(chan error, 1)
				timer := time.AfterFunc(min(test.ttl, outage), func() {
					_, err := scheduler.RenewClaimHeartbeat(t.Context(), string(lease.WorkItemID), time.Now())
					heartbeatDone <- err
				})
				defer timer.Stop()
				sessions := api.NewRunnerSessions(native)
				id, err := sessions.StartSession(sessions.BindExecution(guarded, execution), store.SessionStart{Worker: &store.WorkerProcessRegistration{}})
				if test.revoked || test.ttl < outage {
					if id != 0 || calls != 1 || !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || time.Since(start) != min(test.ttl, outage) {
						t.Fatalf("lost session = %d, calls = %d, elapsed = %v, error = %v", id, calls, time.Since(start), err)
					}
					return
				}
				if err != nil || id != 42 || calls != 2 || time.Since(start) != outage {
					t.Fatalf("session = %d, calls = %d, elapsed = %v, error = %v", id, calls, time.Since(start), err)
				}
				if err := <-heartbeatDone; err != nil {
					t.Fatalf("lease heartbeat did not recover: %v", err)
				}
			})
		})
	}
}

func TestFinalDiffAndCompletionSurviveHubOutage(t *testing.T) {
	for _, tc := range []struct {
		name, outcome  string
		expire, revoke bool
		refuseDiff     bool
	}{
		{name: "successful diff", outcome: "succeeded"},
		{name: "failed turn diff", outcome: "failed"},
		{name: "expired before renewal acknowledgement", outcome: "succeeded", expire: true},
		{name: "revoked on reconnect", outcome: "succeeded", revoke: true},
		{name: "revoked during failed turn diff", outcome: "failed", refuseDiff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const ttl = time.Minute
				started := time.Now()
				descriptor := clientTestPolicy()
				lease := tracker.NativeLease{ID: "lease", WorkItemID: "wi_example", FencingToken: 1, PolicyID: descriptor.ID}
				offline := false
				holdRenewal := make(chan struct{})
				var diff tracker.AttemptDiffRequest
				var finished tracker.NativeRunEvent
				diffWrites, completionRequests, completions := 0, 0, 0
				client, err := newTestClient(t, Config{URL: "https://hub.example.test", TokenSource: func() string { return "fixture" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					response := httptest.NewRecorder()
					switch {
					case strings.HasSuffix(request.URL.Path, "/validate"):
						io.WriteString(response, "{}")
					case strings.HasSuffix(request.URL.Path, "/renew"):
						if offline {
							return nil, io.ErrUnexpectedEOF
						}
						if tc.expire {
							<-holdRenewal
						}
						if tc.revoke {
							response.WriteHeader(http.StatusConflict)
							io.WriteString(response, `{"code":"stale_fencing_token"}`)
							return response.Result(), nil
						}
						renewed := lease
						renewed.ServerTime, renewed.RenewedAt = time.Now(), time.Now()
						renewed.ExpiresAt = renewed.ServerTime.Add(ttl)
						if err := json.NewEncoder(response).Encode(renewed); err != nil {
							return nil, err
						}
					case strings.HasSuffix(request.URL.Path, "/diff"):
						diffWrites++
						if offline {
							return nil, io.ErrUnexpectedEOF
						}
						if tc.refuseDiff {
							response.WriteHeader(http.StatusForbidden)
							io.WriteString(response, `{"code":"insufficient_scope"}`)
							return response.Result(), nil
						}
						if err := json.NewDecoder(request.Body).Decode(&diff); err != nil {
							return nil, err
						}
						io.WriteString(response, "{}")
					case strings.HasSuffix(request.URL.Path, "/events"):
						var event tracker.NativeRunEvent
						if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
							return nil, err
						}
						completionRequests++
						if completions == 0 {
							finished = event
							completions++
							return nil, io.ErrUnexpectedEOF
						}
						if !reflect.DeepEqual(event, finished) {
							t.Fatalf("completion retry changed the committed event: first=%+v retry=%+v", finished, event)
						}
						response.WriteHeader(http.StatusNoContent)
					case strings.HasSuffix(request.URL.Path, "/release"):
						response.WriteHeader(http.StatusNoContent)
					default:
						t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
					}
					return response.Result(), nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_example", "prj_example")
				if err != nil {
					t.Fatal(err)
				}
				scheduler, err := newTestScheduler(client, SchedulerConfig{Now: time.Now, LeaseTTL: ttl, HeartbeatInterval: time.Second, Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}})
				if err != nil {
					t.Fatal(err)
				}
				source := fixtureConnector(native)
				execution := NewExecution(scheduler, executioncontract.ExecutionClaim{Source: source, Lease: lease}).(*nativeExecution)
				fixtureInstallClaim(t, scheduler, nativeClaim{source: source, lease: lease, deadline: started.Add(ttl), execution: execution}, descriptor, "project")
				execution.data.Identity = &tracker.NativeExecutionIdentity{Role: "triage", Backend: "codex", Model: "example"}
				head := nativeChangeFixtureHead("c")
				execution.SetDiffSource(nativeChangeDiff(head, "README.md"))
				guarded, stop, err := execution.Guard(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				detached := context.WithoutCancel(guarded)
				offline = true
				if _, err := scheduler.RenewClaimHeartbeat(t.Context(), string(lease.WorkItemID), time.Now()); !errors.Is(err, api.ErrUnavailable) || guarded.Err() != nil {
					t.Fatalf("renewal outage stopped an active lease: renewal=%v guard=%v", err, context.Cause(guarded))
				}
				if err := execution.Finish(detached, tc.outcome); !nativeTransportUnavailable(err) || completionRequests != 0 || diffWrites != 1 {
					t.Fatalf("published without final diff: error=%v completions=%d diff writes=%d", err, completions, diffWrites)
				}
				offline = false
				if tc.expire || tc.revoke {
					renewal := make(chan error, 1)
					go func() {
						_, err := scheduler.RenewClaimHeartbeat(t.Context(), string(lease.WorkItemID), time.Now())
						renewal <- err
					}()
					if tc.expire {
						<-guarded.Done()
						close(holdRenewal)
					}
					if err := <-renewal; err == nil {
						t.Fatal("renewal succeeded after loss of authority")
					}
					var cause *runner.CancellationCause
					if !errors.As(context.Cause(guarded), &cause) || cause.Reason != "execution_authority_unavailable" {
						t.Fatalf("guard cancellation cause = %v", context.Cause(guarded))
					}
					for _, ctx := range []context.Context{t.Context(), detached} {
						if err := execution.Finish(ctx, tc.outcome); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || completions != 0 || diffWrites != 1 {
							t.Fatalf("lost lease published: error=%v completions=%d diff writes=%d", err, completions, diffWrites)
						}
					}
					return
				}
				if _, err := scheduler.RenewClaimHeartbeat(t.Context(), string(lease.WorkItemID), time.Now()); err != nil {
					t.Fatal(err)
				}
				if tc.refuseDiff {
					for range 2 {
						err := execution.Finish(detached, tc.outcome)
						var cause *runner.CancellationCause
						if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) ||
							!errors.As(context.Cause(guarded), &cause) || cause.Reason != "execution_authority_unavailable" ||
							diffWrites != 2 || completionRequests != 0 {
							t.Fatalf("diff refusal did not fence completion: error=%v cause=%v diff writes=%d event requests=%d", err, context.Cause(guarded), diffWrites, completionRequests)
						}
					}
					return
				}
				if err := execution.Finish(detached, tc.outcome); !nativeTransportUnavailable(err) || completions != 1 {
					t.Fatalf("lost completion response: error=%v completions=%d", err, completions)
				}
				for range 2 {
					if err := execution.Finish(detached, tc.outcome); err != nil {
						t.Fatal(err)
					}
				}
				if completionRequests != 2 || completions != 1 || diffWrites != 2 || diff.HeadSHA != head || diff.Generation.Seq != finished.Data.Sequence || finished.Data.Outcome != tc.outcome || finished.IdempotencyKey != finished.Data.AttemptID+":1" {
					t.Fatalf("diff was not reconciled with one terminal event: diff=%+v event=%+v requests=%d completions=%d", diff, finished, completionRequests, completions)
				}
			})
		})
	}
}
