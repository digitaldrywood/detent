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
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	checkpointstate "github.com/digitaldrywood/detent/internal/hubclient/checkpoint"
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
			routing  bool
		}{
			{name: "renewal rejected"},
			{name: "revoked", revoked: true},
			{name: "callback renewal rejected", callback: true},
			{name: "callback revoked", revoked: true, callback: true},
			{name: "expired", expired: true},
			{name: "host finalization revoked", revoked: true, bare: true},
			{name: "host finalization expired after guard stops", expired: true, bare: true},
			{name: "host finalization after routing authority loss and guard cleanup", bare: true, routing: true},
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
					if scenario.routing {
						enrolled, err := newTestClient(t, Config{
							URL: "https://hub.example.test", TokenSource: func() string { return "fixture" },
							Enrollment: api.NewRunnerState(filepath.Join(t.TempDir(), "identity.json")),
						})
						if err != nil {
							t.Fatal(err)
						}
						execution.scheduler.client = enrolled
					}
					guarded, stop, err := execution.Guard(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					var routingLoss error
					if scenario.routing {
						<-guarded.Done()
						routingLoss = context.Cause(guarded)
						if !errors.Is(routingLoss, runner.ErrExecutionAuthorityUnavailable) {
							t.Fatalf("invalid routing state stopped the guard with %v", routingLoss)
						}
						stop()
						if !execution.claimActive() {
							t.Fatal("routing loss invalidated the fixture's Hub lease")
						}
					} else if scenario.bare && scenario.expired {
						stop()
					} else {
						defer stop()
					}
					var rejected <-chan error
					if scenario.revoked {
						timer := time.AfterFunc(10*time.Second, func() { execution.Cancel(errors.Join(runner.ErrExecutionAuthorityUnavailable, lost)) })
						defer timer.Stop()
					} else if !scenario.expired && !scenario.routing {
						renewed := make(chan error, 1)
						rejected = renewed
						timer := time.AfterFunc(10*time.Second, func() {
							_, err := scheduler.RenewClaimHeartbeat(t.Context(), string(lease.WorkItemID), time.Now())
							renewed <- err
						})
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
					if scenario.routing {
						wantElapsed = 0
					}
					finalization, cancel := context.WithTimeout(parent, 3*time.Minute)
					defer cancel()
					var peerContext context.Context
					wantCalls := 1
					if scenario.routing {
						wantCalls = 0
					}
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
					if rejected != nil {
						renewalErr := <-rejected
						var refusal *APIError
						if !errors.As(renewalErr, &refusal) || refusal.Code != "stale_fencing_token" {
							t.Fatalf("renewal returned %v, want stale fencing token", renewalErr)
						}
					}
					observedLoss := true
					if scenario.routing {
						observedLoss = errors.Is(err, routingLoss)
					} else if scenario.revoked {
						observedLoss = errors.Is(err, lost)
						if scenario.callback {
							observedLoss = err != nil && strings.Contains(err.Error(), lost.Error())
						}
					}
					if scenario.callback && peerContext.Err() != nil {
						t.Fatalf("callback peer ended: %v", peerContext.Err())
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

func TestCheckpointDiffRecoversBeforeItsEventIsPublished(t *testing.T) {
	for _, test := range []struct {
		name         string
		readStatus   int
		readBody     string
		readErr      error
		lostFinalAck bool
		invalidPatch bool
	}{
		{name: "stalled"},
		{name: "unavailable"},
		{name: "overlapping checkpoints"},
		{name: "restored overlapping checkpoints"},
		{name: "committed final diff loses acknowledgement", lostFinalAck: true},
		{name: "committed final diff with malformed UTF-8 loses acknowledgement", lostFinalAck: true, invalidPatch: true},
		{name: "checkpoint read-back unauthorized", readStatus: http.StatusUnauthorized, readBody: `{"code":"unauthorized"}`},
		{name: "checkpoint read-back forbidden", readStatus: http.StatusForbidden, readBody: `{"code":"insufficient_scope"}`},
		{name: "checkpoint read-back forbidden with truncated JSON", readStatus: http.StatusForbidden, readBody: `{"code":`},
		{name: "checkpoint read-back unauthorized with interrupted body", readStatus: http.StatusUnauthorized, readErr: io.ErrUnexpectedEOF},
	} {
		scenario := test.name
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				descriptor := clientTestPolicy()
				lease := tracker.NativeLease{WorkItemID: "wi_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ID: "lease", FencingToken: 17, PolicyID: descriptor.ID}
				checkpoint := tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: nativeChangeFixtureHead("c"), ExternalEffect: "none", EffectState: "none"}
				second := checkpoint
				second.HeadSHA = nativeChangeFixtureHead("d")
				restore := scenario == "restored overlapping checkpoints"
				lostDiffAck := test.lostFinalAck
				offline := scenario == "overlapping checkpoints" || restore || lostDiffAck
				var attempts []tracker.AttemptDiffRequest
				stored := make(map[int64]tracker.AttemptDiffRequest)
				events := 0
				var published tracker.NativeRunEvent
				var publishedCheckpoints []tracker.NativeRunEvent
				var completion *tracker.NativeRunEvent
				finalDiffCommits, diffReads := 0, 0
				client, err := newTestClient(t, Config{URL: "https://hub.example.test", TokenSource: func() string { return "fixture" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					response := httptest.NewRecorder()
					switch {
					case strings.HasSuffix(request.URL.Path, "/validate"):
						io.WriteString(response, "{}")
					case strings.HasSuffix(request.URL.Path, "/diff"):
						if request.Method == http.MethodGet {
							diffReads++
							if test.readStatus != 0 {
								if request.URL.Query().Get("at") != "2" || len(stored) != 1 || stored[2].HeadSHA != checkpoint.HeadSHA {
									t.Fatalf("unexpected checkpoint read-back: %s", request.URL)
								}
								response.Header().Set("Content-Type", "application/json")
								response.WriteHeader(test.readStatus)
								io.WriteString(response, test.readBody)
								result := response.Result()
								if test.readErr != nil {
									result.Body = io.NopCloser(iotest.ErrReader(test.readErr))
								}
								return result, nil
							}
							if !lostDiffAck || request.URL.Query().Get("at") != "4" || finalDiffCommits != 1 {
								t.Fatalf("unexpected diff read: %s", request.URL)
							}
							stored := stored[4]
							if err := json.NewEncoder(response).Encode(tracker.AttemptDiff{ID: "diff_final", AttemptID: stored.Producer.ID, Producer: stored.Producer, Generation: stored.Generation, BaseSHA: stored.BaseSHA, HeadSHA: stored.HeadSHA, Files: stored.Files, FileCount: len(stored.Files)}); err != nil {
								return nil, err
							}
							return response.Result(), nil
						}
						var diff tracker.AttemptDiffRequest
						if err := json.NewDecoder(request.Body).Decode(&diff); err != nil {
							return nil, err
						}
						attempts = append(attempts, diff)
						if offline {
							return nil, io.ErrUnexpectedEOF
						}
						if len(attempts) == 1 {
							if test.readStatus != 0 {
								stored[diff.Generation.Seq] = diff
							}
							if scenario == "stalled" {
								<-request.Context().Done()
								return nil, request.Context().Err()
							}
							return nil, io.ErrUnexpectedEOF
						}
						if test.readStatus != 0 {
							response.Header().Set("Content-Type", "application/json")
							response.WriteHeader(http.StatusConflict)
							io.WriteString(response, `{"code":"stale_generation"}`)
							return response.Result(), nil
						}
						if lostDiffAck && diff.Generation.Seq == 4 {
							if test.invalidPatch && diff.Files[0].Patch != "+caf\uFFFD\uFFFD\n" {
								t.Errorf("final patch on the wire = %q", diff.Files[0].Patch)
							}
							if finalDiffCommits == 0 {
								stored[4] = diff
								finalDiffCommits++
								return nil, io.ErrUnexpectedEOF
							}
							response.Header().Set("Content-Type", "application/json")
							response.WriteHeader(http.StatusConflict)
							io.WriteString(response, `{"code":"stale_generation"}`)
							return response.Result(), nil
						}
						stored[diff.Generation.Seq] = diff
						io.WriteString(response, "{}")
					case strings.HasSuffix(request.URL.Path, "/events"):
						events++
						if err := request.Context().Err(); err != nil {
							return nil, err
						}
						published = tracker.NativeRunEvent{}
						if err := json.NewDecoder(request.Body).Decode(&published); err != nil {
							t.Fatal(err)
						}
						diff, exists := stored[published.Data.Sequence]
						if !exists || published.Data.Handoff != nil && published.Data.Handoff.HeadSHA != diff.HeadSHA {
							t.Errorf("event %s sequence %d reached the Hub before its matching diff", published.Type, published.Data.Sequence)
						}
						if published.Type == "run.checkpointed" {
							publishedCheckpoints = append(publishedCheckpoints, published)
						}
						if published.Type == "run.finished" {
							if completion == nil {
								completion = new(published)
								return nil, io.ErrUnexpectedEOF
							}
							if !reflect.DeepEqual(*completion, published) {
								t.Errorf("terminal replay changed the applied completion: %+v", published)
							}
						}
						response.WriteHeader(http.StatusNoContent)
					case test.readStatus != 0 && strings.HasSuffix(request.URL.Path, "/release"):
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
				execution := NewExecution(scheduler, executioncontract.ExecutionClaim{Source: source, Lease: lease}).(*nativeExecution)
				fixtureInstallClaim(t, scheduler, nativeClaim{source: source, lease: lease, deadline: start.Add(5 * time.Minute), execution: execution}, descriptor, "project")
				execution.data.Identity = &tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}
				execution.data.Sequence = 1
				if offline && !lostDiffAck {
					execution.data.Identity.Role = "triage"
				}
				execution.settled = lostDiffAck
				liveHead := checkpoint.HeadSHA
				execution.SetDiffSource(func(ctx context.Context) (tracker.AttemptDiffRequest, bool) {
					if liveHead == "" {
						return tracker.AttemptDiffRequest{}, false
					}
					file := "README.md"
					if liveHead == second.HeadSHA {
						file = "other.go"
					}
					diff, ok := nativeChangeDiff(liveHead, file)(ctx)
					diff.CommitBound = true
					if test.invalidPatch && liveHead == second.HeadSHA {
						diff.Files[0].Patch = "+caf\xe9\xff\n"
					}
					return diff, ok
				})
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				var guarded context.Context
				if test.readStatus != 0 {
					var stop func()
					guarded, stop, err = execution.Guard(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer stop()
					ctx = context.WithoutCancel(guarded)
				}
				if err := execution.Checkpoint(ctx, checkpoint); err != nil {
					t.Fatal(err)
				}
				if ctx.Err() != nil || len(attempts) != 1 || published.Type != "" {
					t.Fatalf("checkpoint advanced before storing its diff: event=%s, diff attempts=%d, context=%v", published.Type, len(attempts), ctx.Err())
				}
				if scenario == "stalled" && time.Since(start) != checkpointDiffTimeout {
					t.Fatalf("stalled checkpoint took %v, want %v", time.Since(start), checkpointDiffTimeout)
				}
				if test.readStatus != 0 {
					for range 2 {
						err := execution.FlushRuntime(ctx)
						var cause *runner.CancellationCause
						if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) ||
							!errors.As(context.Cause(guarded), &cause) || cause.Reason != "execution_authority_unavailable" ||
							events != 0 || diffReads != 1 || len(attempts) != 2 {
							t.Fatalf("checkpoint read refusal did not fence publication: error=%v cause=%v events=%d diff reads=%d diff writes=%d", err, context.Cause(guarded), events, diffReads, len(attempts))
						}
					}
					return
				}
				if offline {
					liveHead = second.HeadSHA
					if err := execution.Checkpoint(ctx, second); err != nil {
						t.Fatal(err)
					}
					if events != 0 {
						t.Fatalf("published %d events during the outage", events)
					}
					liveHead, offline = "", false
					if restore {
						var state checkpointstate.State
						if err := json.Unmarshal(execution.CompletionState(), &state); err != nil {
							t.Fatal(err)
						}
						recovered, err := NewRecoveredExecution(scheduler, ctx, executioncontract.ExecutionClaim{Source: source, Lease: lease}, state)
						if err != nil {
							t.Fatal(err)
						}
						execution = recovered.(*nativeExecution)
					}
					if lostDiffAck {
						if err := execution.Finish(ctx, "succeeded"); !api.NativePublicationUnavailable(err) || completion != nil || finalDiffCommits != 1 {
							t.Fatalf("lost diff acknowledgement = %v, commits=%d, completion=%v", err, finalDiffCommits, completion)
						}
					}
					if err := execution.Finish(ctx, "succeeded"); !api.NativePublicationUnavailable(err) {
						t.Errorf("first completion should lose its acknowledgement: %v", err)
					}
					if err := execution.Finish(ctx, "succeeded"); err != nil {
						t.Fatal(err)
					}
					if err := execution.Finish(ctx, "succeeded"); err != nil {
						t.Fatal(err)
					}
					if len(publishedCheckpoints) != 2 || publishedCheckpoints[0].Data.Sequence != 2 || publishedCheckpoints[0].Data.Handoff == nil || *publishedCheckpoints[0].Data.Handoff != checkpoint ||
						publishedCheckpoints[1].Data.Sequence != 3 || publishedCheckpoints[1].Data.Handoff == nil || *publishedCheckpoints[1].Data.Handoff != second ||
						stored[2].HeadSHA != checkpoint.HeadSHA || stored[3].HeadSHA != second.HeadSHA || stored[4].HeadSHA != second.HeadSHA ||
						completion == nil || completion.Data.Outcome != "succeeded" || completion.Data.Sequence != 4 || events != 4 {
						t.Errorf("checkpoints=%+v, heads by sequence=%q/%q/%q, completion=%+v, requests=%d", publishedCheckpoints, stored[2].HeadSHA, stored[3].HeadSHA, stored[4].HeadSHA, completion, events)
					}
					for _, sequence := range []int64{3, 4} {
						files := stored[sequence].Files
						if len(files) != 1 || files[0].Path != "other.go" {
							t.Errorf("sequence %d stored files %+v, want other.go", sequence, files)
						}
					}
					if lostDiffAck && (diffReads != 1 || finalDiffCommits != 1) {
						t.Errorf("diff reconciliation read %d times and committed %d times", diffReads, finalDiffCommits)
					}
					return
				}
				execution.SetDiffSource(nativeChangeDiff(nativeChangeFixtureHead("d"), "other.go"))
				if err := execution.FlushRuntime(ctx); err != nil {
					t.Fatal(err)
				}
				if len(stored) != 1 || events != 1 || len(attempts) != 2 || !reflect.DeepEqual(attempts[0], attempts[1]) || attempts[1].HeadSHA != checkpoint.HeadSHA ||
					attempts[1].Generation.Seq != 2 || published.Type != "run.checkpointed" || published.Data.Sequence != 2 ||
					published.Data.Handoff == nil || *published.Data.Handoff != checkpoint {
					t.Fatalf("recovered checkpoint = %s %+v, diff attempts = %+v", published.Type, published.Data.Handoff, attempts)
				}
			})
		})
	}
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
		refuseChange   bool
		refuseSession  string
	}{
		{name: "successful diff", outcome: "succeeded"},
		{name: "failed turn diff", outcome: "failed"},
		{name: "expired before renewal acknowledgement", outcome: "succeeded", expire: true},
		{name: "revoked on reconnect", outcome: "succeeded", revoke: true},
		{name: "revoked during failed turn diff", outcome: "failed", refuseDiff: true},
		{name: "refused while opening a Change", outcome: "succeeded", refuseChange: true},
		{name: "revoked during guarded session progress", outcome: "failed", refuseSession: "guarded"},
		{name: "revoked during rebound session progress", outcome: "failed", refuseSession: "rebound"},
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
				changeCreates, versionWrites := 0, 0
				sessionReads, sessionWrites := 0, 0
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
					case strings.HasSuffix(request.URL.Path, "/runner-sessions/42"):
						sessionReads++
						io.WriteString(response, `{"id":42,"revision":1}`)
					case strings.HasSuffix(request.URL.Path, "/runner-sessions"):
						sessionWrites++
						if sessionWrites == 2 {
							response.WriteHeader(http.StatusConflict)
							io.WriteString(response, `{"code":"stale_fencing_token"}`)
							return response.Result(), nil
						}
						io.WriteString(response, `{"id":42,"revision":1}`)
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
					case tc.refuseChange && strings.HasSuffix(request.URL.Path, "/changes") && request.Method == http.MethodGet:
						io.WriteString(response, "[]")
					case tc.refuseChange && strings.HasSuffix(request.URL.Path, "/changes") && request.Method == http.MethodPost:
						changeCreates++
						response.Header().Set("Content-Type", "application/json")
						response.WriteHeader(http.StatusForbidden)
						io.WriteString(response, `{"code":"insufficient_scope"}`)
					case tc.refuseChange && strings.HasSuffix(request.URL.Path, "/versions") && request.Method == http.MethodPost:
						versionWrites++
						response.WriteHeader(http.StatusForbidden)
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
				if tc.refuseChange {
					execution.data.Identity.Role = runner.RoleCode
					execution.worktreeState, execution.worktreeHead = "unpushed", head
				}
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
				if tc.refuseSession != "" {
					sessions := api.NewRunnerSessions(native)
					id, err := sessions.StartSession(guarded, store.SessionStart{})
					if err != nil {
						t.Fatal(err)
					}
					ctx := guarded
					if tc.refuseSession == "rebound" {
						ctx = sessions.BindExecution(t.Context(), execution)
					}
					if err := sessions.SaveSessionProgress(ctx, id, store.SessionProgress{}); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
						t.Fatalf("session refusal = %v", err)
					}
					var cause *runner.CancellationCause
					if !errors.As(context.Cause(guarded), &cause) || cause.Reason != "execution_authority_unavailable" {
						t.Errorf("guard cancellation cause = %v", context.Cause(guarded))
					}
					for _, ctx := range []context.Context{detached, sessions.BindExecution(t.Context(), execution)} {
						if err := sessions.FinishSession(ctx, id, store.SessionFinish{}); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
							t.Errorf("detached session finish = %v", err)
						}
						if err := execution.Finish(ctx, tc.outcome); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
							t.Errorf("native finish = %v", err)
						}
					}
					if sessionReads != 0 || sessionWrites != 2 || diffWrites != 1 || completionRequests != 0 {
						t.Errorf("writes after session refusal: session reads=%d session writes=%d diff writes=%d completion requests=%d", sessionReads, sessionWrites, diffWrites, completionRequests)
					}
					return
				}
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
				if tc.refuseChange {
					at := time.Now()
					for _, ctx := range []context.Context{detached, t.Context()} {
						err := execution.Finish(ctx, tc.outcome)
						var cause *runner.CancellationCause
						if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) ||
							!errors.As(context.Cause(guarded), &cause) || cause.Reason != "execution_authority_unavailable" {
							t.Fatalf("Change creation refusal did not cancel the lease guard: error=%v cause=%v", err, context.Cause(guarded))
						}
					}
					if changeCreates != 1 || versionWrites != 0 || completionRequests != 0 || diffWrites != 2 || !time.Now().Equal(at) {
						t.Fatalf("publication after Change refusal: creates=%d versions=%d completions=%d diffs=%d elapsed=%v",
							changeCreates, versionWrites, completionRequests, diffWrites, time.Since(at))
					}
					return
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
