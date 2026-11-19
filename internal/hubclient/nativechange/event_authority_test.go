package nativechange

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func replayExecution(t *testing.T, send func(*http.Request) (int, string, error)) *nativeExecution {
	t.Helper()
	descriptor := clientTestPolicy()
	lease := tracker.NativeLease{ID: "lease_example", WorkItemID: "wi_example", FencingToken: 1, PolicyID: descriptor.ID}
	client, err := newTestClient(t, Config{URL: "http://hub.example", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
		status, body, err := send(r)
		if err != nil {
			return nil, err
		}
		header := make(http.Header)
		header.Set("Retry-After", "90")
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := newTestScheduler(client, SchedulerConfig{Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}, LeaseTTL: time.Minute, HeartbeatInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_example", "prj_example")
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewNativeConnector(native)
	if err != nil {
		t.Fatal(err)
	}
	fixtureInstallClaim(t, scheduler, nativeClaim{source: source, lease: lease, deadline: time.Now().Add(time.Minute)}, descriptor, "example")
	e := scheduler.RunExecution(string(lease.WorkItemID)).(*nativeExecution)
	e.data.Identity = &tracker.NativeExecutionIdentity{Role: "review", Backend: "codex", Model: "example"}
	e.SetDiffSource(func(ctx context.Context) (tracker.AttemptDiffRequest, bool) {
		diff, ok := nativeChangeDiff(nativeChangeFixtureHead("a"), "initial.go")(ctx)
		diff.WorkspaceDigest = strings.Repeat("d", 64)
		return diff, ok
	})
	return e
}

func TestReplayEventRequiresCurrentAuthority(t *testing.T) {
	for _, stage := range []string{"stopped finish", "stopped execution with fresh context", "expiry during diff", "expiry during request budget"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				posts, attempts := 0, 0
				e := replayExecution(t, func(r *http.Request) (int, string, error) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/diff"):
						if stage == "expiry during diff" {
							<-time.After(time.Minute)
						}
					case strings.HasSuffix(r.URL.Path, "/events"):
						var event tracker.NativeRunEvent
						if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
							return 0, "", err
						}
						attempts++
						if attempts == 1 && strings.HasPrefix(stage, "stopped") {
							return 0, "", ErrUnavailable
						}
						if attempts == 1 && stage == "expiry during request budget" {
							return 429, `{"code":"runner_request_budget"}`, nil
						}
						posts++
					}
					return 200, "{}", nil
				})
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				guarded := api.WithMutationAuthority(ctx, e.claim.source.NativeClient().Scope(), e.claim.lease, func() error { return context.Cause(ctx) })
				want := runner.ErrExecutionAuthorityUnavailable
				var err error
				if strings.HasPrefix(stage, "stopped") {
					if stage == "stopped execution with fresh context" {
						worker, stop, err := e.Guard(ctx)
						if err != nil {
							t.Fatal(err)
						}
						defer stop()
						guarded = worker
					}
					if err := e.Finish(guarded, "succeeded"); !nativeTransportUnavailable(err) {
						t.Fatalf("first finish = %v", err)
					}
					cancel(runner.ErrOperatorStopped)
					want = runner.ErrOperatorStopped
					cleanup := context.WithoutCancel(guarded)
					if stage == "stopped execution with fresh context" {
						cleanup = t.Context()
					}
					err = e.Finish(cleanup, "succeeded")
				} else {
					err = e.Checkpoint(guarded, tracker.NativeCheckpoint{HeadSHA: nativeChangeFixtureHead("a"), WorktreeState: "unpushed"})
				}
				if posts != 0 || !errors.Is(err, want) {
					t.Fatalf("event posts=%d error=%v, want zero and %v", posts, err, want)
				}
			})
		})
	}
}

func TestStoppedValidationReceiptAuthority(t *testing.T) {
	for _, stage := range []string{"source changed", "lease expires before event", "receipt delivery recovers", "unsent observations"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var diffs, events int
				var sent tracker.NativeRunEvent
				e := replayExecution(t, func(r *http.Request) (int, string, error) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/diff"):
						diffs++
						if stage == "lease expires before event" {
							<-time.After(time.Minute)
						}
					case strings.HasSuffix(r.URL.Path, "/events"):
						events++
						if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
							return 0, "", err
						}
						if stage == "receipt delivery recovers" && events == 1 {
							return 0, "", ErrUnavailable
						}
					}
					return 200, "{}", nil
				})
				head := nativeChangeFixtureHead("a")
				if stage == "source changed" {
					e.SetDiffSource(nativeChangeDiff(nativeChangeFixtureHead("b"), "changed.go"))
				}
				if stage == "unsent observations" {
					e.claim.lease.RunDetailBatching = true
					if err := e.append(t.Context(), "run.observed", "", nil); err != nil {
						t.Fatal(err)
					}
				}
				ctx := api.WithMutationAuthority(t.Context(), e.claim.source.NativeClient().Scope(), e.claim.lease, func() error { return runner.ErrOperatorStopped })
				err := e.RecordStep(ctx, tracker.NativeStepReceipt{Step: "validation", Source: tracker.NativeCheckpoint{HeadSHA: head, WorkspaceDigest: strings.Repeat("d", 64)}, Validation: &gate.CommandResult{HeadSHA: head}})
				switch stage {
				case "unsent observations":
					if err != nil || events != 1 || sent.Type != "run.checkpointed" || sent.Data.Sequence != 1 || len(sent.PriorEvents) != 0 {
						t.Fatalf("buffered observations: sent=%s sequence=%d prior=%d events=%d error=%v", sent.Type, sent.Data.Sequence, len(sent.PriorEvents), events, err)
					}
				case "source changed":
					if !errors.Is(err, runner.ErrOperatorStopped) || diffs != 0 || events != 0 {
						t.Fatalf("changed source: diffs=%d events=%d error=%v", diffs, events, err)
					}
				case "lease expires before event":
					if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || diffs != 1 || events != 0 {
						t.Fatalf("expired lease: diffs=%d events=%d error=%v", diffs, events, err)
					}
				case "receipt delivery recovers":
					if !nativeTransportUnavailable(err) {
						t.Fatalf("first receipt delivery = %v", err)
					}
					if err := e.flush(ctx); err != nil || events != 2 || diffs != 1 {
						t.Fatalf("receipt retry: diffs=%d events=%d error=%v", diffs, events, err)
					}
				}
			})
		})
	}
}

func TestStoppedReceiptReconcilesStoredDiff(t *testing.T) {
	for _, mismatch := range []string{"", "stripped", "producer", "runner", "files", "generation"} {
		t.Run("mismatch="+mismatch, func(t *testing.T) {
			var stored tracker.AttemptDiff
			var attempt tracker.NativeAttempt
			var writes, reads, receipts int
			e := replayExecution(t, func(r *http.Request) (int, string, error) {
				if strings.HasSuffix(r.URL.Path, "/events") {
					receipts++
					return 200, "{}", nil
				}
				if r.Method == http.MethodGet && !strings.HasSuffix(r.URL.Path, "/diff") {
					body, err := json.Marshal(attempt)
					return 200, string(body), err
				}
				if !strings.HasSuffix(r.URL.Path, "/diff") {
					return 200, "{}", nil
				}
				if r.Method == http.MethodGet {
					reads++
					switch mismatch {
					case "runner":
						stored.Producer.RunnerID = "runner_other"
					case "producer":
						stored.Producer.FencingToken++
					case "files":
						stored.Files[0].Patch = "+different"
					case "generation":
						stored.Generation.Seq++
					}
					body, err := json.Marshal(stored)
					return 200, string(body), err
				}
				writes++
				if mismatch == "stripped" && writes == 1 {
					return 413, `{"code":"diff_too_large"}`, nil
				}
				if writes == 1 || mismatch == "stripped" && writes == 2 {
					if err := json.NewDecoder(r.Body).Decode(&stored); err != nil {
						t.Fatal(err)
					}
					if mismatch == "stripped" && (len(stored.Files) != 1 || !stored.Files[0].Truncated || stored.Files[0].Patch != "") {
						t.Fatal("expected the fallback write to omit the patch")
					}
					stored.AttemptID = stored.Producer.ID
					stored.Producer.RunnerID = "runner_example"
					return 0, "", ErrUnavailable
				}
				return 409, `{"code":"stale_generation"}`, nil
			})
			attempt.NativeRunData = e.data
			attempt.RunnerID = "runner_example"
			head := nativeChangeFixtureHead("a")
			ctx := api.WithMutationAuthority(t.Context(), e.claim.source.NativeClient().Scope(), e.claim.lease, func() error { return runner.ErrOperatorStopped })
			if err := e.RecordStep(ctx, tracker.NativeStepReceipt{Step: "validation", Source: tracker.NativeCheckpoint{HeadSHA: head, WorkspaceDigest: strings.Repeat("d", 64)}, Validation: &gate.CommandResult{HeadSHA: head}}); !nativeTransportUnavailable(err) {
				t.Fatalf("first delivery = %v", err)
			}
			err := e.flush(ctx)
			wantWrites := 2
			if mismatch == "stripped" {
				wantWrites++
			}
			matches := mismatch == "" || mismatch == "stripped"
			if writes != wantWrites || reads != 1 || matches && (err != nil || receipts != 1) || !matches && (err == nil || receipts != 0) {
				t.Fatalf("writes=%d reads=%d receipts=%d error=%v", writes, reads, receipts, err)
			}
		})
	}
}

func TestStoppedReceiptAfterUnacknowledgedObservation(t *testing.T) {
	for _, delivery := range []string{"committed", "never received", "gate during outage", "replaced lease"} {
		t.Run(delivery, func(t *testing.T) {
			var observed, received tracker.NativeRunEvent
			var posts, reads int
			e := replayExecution(t, func(r *http.Request) (int, string, error) {
				switch {
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/attempts/"):
					reads++
					if reads == 1 && delivery != "gate during outage" {
						return 0, "", ErrUnavailable
					}
					attempt := tracker.NativeAttempt{NativeRunData: observed.Data}
					if delivery == "never received" {
						attempt.Sequence--
					}
					if delivery == "replaced lease" {
						attempt.FencingToken++
					}
					body, err := json.Marshal(attempt)
					return 200, string(body), err
				case strings.HasSuffix(r.URL.Path, "/events"):
					posts++
					if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
						t.Fatal(err)
					}
					if received.Type == "run.observed" {
						observed = received
						return 0, "", ErrUnavailable
					}
				}
				return 200, "{}", nil
			})
			if err := e.append(t.Context(), "run.observed", "", nil, true); !nativeTransportUnavailable(err) {
				t.Fatal(err)
			}
			head := nativeChangeFixtureHead("a")
			ctx := api.WithMutationAuthority(t.Context(), e.claim.source.NativeClient().Scope(), e.claim.lease, func() error { return runner.ErrOperatorStopped })
			recordCtx := ctx
			if delivery == "gate during outage" {
				recordCtx = t.Context()
			}
			err := e.RecordStep(recordCtx, tracker.NativeStepReceipt{Step: "validation", Source: tracker.NativeCheckpoint{HeadSHA: head, WorkspaceDigest: strings.Repeat("d", 64)}, Validation: &gate.CommandResult{HeadSHA: head}})
			if !nativeTransportUnavailable(err) {
				t.Fatalf("receipt reconciliation during outage = %v", err)
			}
			err = e.flush(ctx)
			if delivery == "replaced lease" {
				if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || posts != 1 {
					t.Fatalf("replaced owner: posts=%d error=%v", posts, err)
				}
				return
			}
			want := observed.Data.Sequence + 1
			if delivery == "never received" {
				want--
			}
			wantPosts := 2
			if delivery == "gate during outage" {
				wantPosts++
			}
			if err != nil || posts != wantPosts || received.Type != "run.checkpointed" || received.Data.Sequence != want || len(received.PriorEvents) != 0 || len(received.Data.Steps) != 1 || received.Data.Steps[0].Validation.HeadSHA != head {
				t.Fatalf("posts=%d receipt=%+v error=%v", posts, received, err)
			}
		})
	}
}

func TestStoppedReceiptAfterBufferedObservation(t *testing.T) {
	for _, test := range []struct {
		name         string
		committed    bool
		wantSequence int64
	}{
		{"Hub committed", true, 3},
		{"never arrived", false, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var attempt tracker.NativeAttempt
			var observations, checkpoints int
			var checkpoint tracker.NativeRunEvent
			e := replayExecution(t, func(r *http.Request) (int, string, error) {
				switch {
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/attempts/"):
					body, err := json.Marshal(attempt)
					return 200, string(body), err
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events"):
					var event tracker.NativeRunEvent
					if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
						t.Fatal(err)
					}
					switch event.Type {
					case "run.observed":
						observations++
						if event.Data.Sequence != 2 || len(event.PriorEvents) != 1 || event.PriorEvents[0].Data.Sequence != 1 {
							t.Fatalf("unexpected observation batch: %+v", event)
						}
						if test.committed {
							attempt.NativeRunData = event.Data
						}
						return 0, "", ErrUnavailable
					case "run.checkpointed":
						checkpoints++
						checkpoint = event
					default:
						t.Fatalf("unexpected event %q", event.Type)
					}
				}
				return 200, "{}", nil
			})
			attempt.NativeRunData = e.data
			e.claim.lease.RunDetailBatching = true
			worker, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			ctx := api.WithMutationAuthority(worker, e.claim.source.NativeClient().Scope(), e.claim.lease, func() error { return context.Cause(worker) })
			for range 2 {
				if err := e.append(ctx, "run.observed", "", nil); err != nil {
					t.Fatal(err)
				}
			}
			if observations != 0 {
				t.Fatalf("observation POSTs before RecordStep = %d", observations)
			}
			head := nativeChangeFixtureHead("a")
			gateResult := gate.CommandResult{Command: "make check", HeadSHA: head, ExitCode: 1}
			err := e.RecordStep(ctx, tracker.NativeStepReceipt{Step: "validation", Source: tracker.NativeCheckpoint{HeadSHA: head, WorkspaceDigest: strings.Repeat("d", 64)}, Validation: &gateResult})
			if !nativeTransportUnavailable(err) || observations != 1 || checkpoints != 0 {
				t.Fatalf("RecordStep: observation POSTs=%d checkpoint POSTs=%d error=%v", observations, checkpoints, err)
			}
			cancel(runner.ErrOperatorStopped)
			cleanup := context.WithoutCancel(ctx)
			for range 2 {
				if err := e.flush(cleanup); err != nil {
					t.Fatalf("stopped flush: checkpoint POSTs=%d error=%v", checkpoints, err)
				}
			}
			if observations != 1 || checkpoints != 1 || checkpoint.Data.Sequence != test.wantSequence || len(checkpoint.PriorEvents) != 0 {
				t.Fatalf("observation POSTs=%d checkpoint POSTs=%d sequence=%d prior=%d", observations, checkpoints, checkpoint.Data.Sequence, len(checkpoint.PriorEvents))
			}
			steps := checkpoint.Data.Steps
			if len(steps) != 1 || steps[0].Step != "validation" || steps[0].Validation == nil || steps[0].Validation.HeadSHA != head || steps[0].Validation.ExitCode != 1 || steps[0].Validation.Command != gateResult.Command {
				t.Fatalf("checkpoint lost gate result: %+v", steps)
			}
		})
	}
}

func TestStoppedGateRequiresVerifiedWorkspace(t *testing.T) {
	for _, stage := range []string{"no live source", "different workspace digest", "missing gate digest", "files change before receipt", "base changes before receipt"} {
		t.Run(stage, func(t *testing.T) {
			var diffs, events int
			e := replayExecution(t, func(r *http.Request) (int, string, error) {
				if strings.HasSuffix(r.URL.Path, "/diff") {
					diffs++
				}
				if strings.HasSuffix(r.URL.Path, "/events") {
					events++
				}
				return 200, "{}", nil
			})
			head := nativeChangeFixtureHead("a")
			original, _ := e.diffSource(t.Context())
			e.SetDiffSource(func(ctx context.Context) (tracker.AttemptDiffRequest, bool) {
				if diffs != 0 && stage == "files change before receipt" {
					return nativeChangeDiff(head, "different.go")(ctx)
				}
				current := original
				if stage == "different workspace digest" {
					current.WorkspaceDigest = strings.Repeat("e", 64)
				}
				if diffs != 0 && stage == "base changes before receipt" {
					current.BaseSHA = nativeChangeFixtureHead("c")
				}
				return current, true
			})
			if stage == "no live source" {
				e.lastDiff = &original
				e.SetDiffSource(nil)
			}
			digest := strings.Repeat("d", 64)
			if stage == "missing gate digest" {
				digest = ""
			}
			ctx := api.WithMutationAuthority(t.Context(), e.claim.source.NativeClient().Scope(), e.claim.lease, func() error { return runner.ErrOperatorStopped })
			err := e.RecordStep(ctx, tracker.NativeStepReceipt{Step: "validation", Source: tracker.NativeCheckpoint{HeadSHA: head, WorkspaceDigest: digest}, Validation: &gate.CommandResult{HeadSHA: head}})
			wantDiffs := 0
			if strings.Contains(stage, "before receipt") {
				wantDiffs = 1
			}
			if !errors.Is(err, runner.ErrOperatorStopped) || diffs != wantDiffs || events != 0 {
				t.Fatalf("diffs=%d events=%d error=%v", diffs, events, err)
			}
		})
	}
}

func TestReleaseStoppedClaimWithPendingCheckpoint(t *testing.T) {
	h := newNativeChangeHub(t, true)
	issue := h.createInProgress(t, "Stop with a pending checkpoint")
	h.claim(t, issue.ID)
	execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	guarded, stop, err := execution.Guard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stop = sync.OnceFunc(stop)
	defer stop()
	if err := execution.Start(guarded, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "example"}); err != nil {
		t.Fatal(err)
	}
	if err := execution.PrepareFinish(guarded, "cancelled", "", nil); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("c", 40)
	execution.SetDiffSource(nativeChangeDiff(head, "example.go"))
	transport := fixtureClient(h.native.Transport()).requests.Load()
	checkpoints := 0
	fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/events") {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			req.Body = io.NopCloser(strings.NewReader(string(body)))
			var event tracker.NativeRunEvent
			if err := json.Unmarshal(body, &event); err != nil {
				return nil, err
			}
			if event.Type == "run.checkpointed" {
				checkpoints++
				if checkpoints == 1 {
					return nil, errors.New("checkpoint delivery interrupted")
				}
			}
		}
		return transport.RoundTrip(req)
	}))
	if err := execution.Checkpoint(guarded, tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: head, ExternalEffect: "none", EffectState: "none"}); err != nil || checkpoints != 1 {
		t.Fatalf("checkpoint setup: posts=%d error=%v", checkpoints, err)
	}
	cancel(runner.ErrOperatorStopped)
	stop()
	if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, runner.FinalStateOperatorStopped); err != nil {
		t.Fatal(err)
	}
	recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
	if err != nil || checkpoints != 1 || len(recovery.Attempts) != 1 || recovery.Attempts[0].ClaimReleasedAt == nil || recovery.Attempts[0].Checkpoint != nil {
		t.Fatalf("checkpoint POSTs=%d attempts=%+v error=%v", checkpoints, recovery.Attempts, err)
	}
}
