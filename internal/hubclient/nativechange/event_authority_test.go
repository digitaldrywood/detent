package nativechange

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
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
	e.SetDiffSource(nativeChangeDiff(nativeChangeFixtureHead("a"), "initial.go"))
	return e
}

func TestReplayEventRequiresCurrentAuthority(t *testing.T) {
	for _, stage := range []string{"stopped finish", "expiry during diff", "expiry during request budget"} {
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
						if attempts == 1 && stage == "stopped finish" {
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
				if stage == "stopped finish" {
					if err := e.Finish(guarded, "succeeded"); !nativeTransportUnavailable(err) {
						t.Fatalf("first finish = %v", err)
					}
					cancel(runner.ErrOperatorStopped)
					want = runner.ErrOperatorStopped
					err = e.Finish(context.WithoutCancel(guarded), "succeeded")
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
				err := e.RecordStep(ctx, tracker.NativeStepReceipt{Step: "validation", Source: tracker.NativeCheckpoint{HeadSHA: head}, Validation: &gate.CommandResult{HeadSHA: head}})
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
	for _, mismatch := range []string{"", "producer", "runner", "files", "generation"} {
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
				if writes == 1 {
					if err := json.NewDecoder(r.Body).Decode(&stored); err != nil {
						t.Fatal(err)
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
			if err := e.RecordStep(ctx, tracker.NativeStepReceipt{Step: "validation", Source: tracker.NativeCheckpoint{HeadSHA: head}, Validation: &gate.CommandResult{HeadSHA: head}}); !nativeTransportUnavailable(err) {
				t.Fatalf("first delivery = %v", err)
			}
			err := e.flush(ctx)
			if writes != 2 || reads != 1 || mismatch == "" && (err != nil || receipts != 1) || mismatch != "" && (err == nil || receipts != 0) {
				t.Fatalf("writes=%d reads=%d receipts=%d error=%v", writes, reads, receipts, err)
			}
		})
	}
}

func TestStoppedReceiptAfterUnacknowledgedObservation(t *testing.T) {
	for _, delivery := range []string{"committed", "never received", "replaced lease"} {
		t.Run(delivery, func(t *testing.T) {
			var observed, received tracker.NativeRunEvent
			var posts, reads int
			e := replayExecution(t, func(r *http.Request) (int, string, error) {
				switch {
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/attempts/"):
					reads++
					if reads == 1 {
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
			err := e.RecordStep(ctx, tracker.NativeStepReceipt{Step: "validation", Source: tracker.NativeCheckpoint{HeadSHA: head}, Validation: &gate.CommandResult{HeadSHA: head}})
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
			if err != nil || posts != 2 || received.Type != "run.checkpointed" || received.Data.Sequence != want || len(received.PriorEvents) != 0 || len(received.Data.Steps) != 1 || received.Data.Steps[0].Validation.HeadSHA != head {
				t.Fatalf("posts=%d receipt=%+v error=%v", posts, received, err)
			}
		})
	}
}
