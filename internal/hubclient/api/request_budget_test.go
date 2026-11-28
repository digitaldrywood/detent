package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func budgetClient(t *testing.T, respond func(call int, response *httptest.ResponseRecorder)) (*Client, *int) {
	t.Helper()
	calls := 0
	var mutationBody []byte
	client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "token" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/runner-sessions") || strings.HasSuffix(r.URL.Path, "/events")) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if mutationBody != nil && !bytes.Equal(mutationBody, body) {
				t.Fatal("budget retry changed the mutation body")
			}
			mutationBody = body
		}
		response := httptest.NewRecorder()
		respond(calls, response)
		return response.Result(), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	return client, &calls
}

func refuseBudget(response *httptest.ResponseRecorder, retryAfter string) {
	if retryAfter != "" {
		response.Header().Set("Retry-After", retryAfter)
	}
	response.Header().Set("Content-Type", "application/json; charset=UTF-8")
	response.WriteHeader(http.StatusTooManyRequests)
	response.WriteString(`{"code":"runner_request_budget","message":"Runner request budget exceeded; retry after the current minute"}`)
}

func isBudgetRefusal(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr != nil && apiErr.Status == http.StatusTooManyRequests && apiErr.Code == requestBudgetCode
}

func TestClientWaitsOutRequestBudgetRefusal(t *testing.T) {
	for _, test := range []struct {
		name, method, path string
		retryAfter         func() string
		download           bool
	}{
		{"read", http.MethodGet, "/items", func() string { return "60" }, false},
		{"session update", http.MethodPost, "/api/v2/organizations/org/projects/project/runner-sessions", func() string { return "60" }, false},
		{"usage event", http.MethodPost, "/api/v2/organizations/org/projects/project/work-items/wi_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/events", func() string { return "60" }, false},
		{"HTTP date", http.MethodGet, "/items", func() string { return time.Now().Add(time.Minute).UTC().Format(http.TimeFormat) }, false},
		{"malformed header waits for the next minute", http.MethodGet, "/items", func() string { return "invalid" }, false},
		{"missing header waits for the next minute", http.MethodGet, "/items", func() string { return "" }, false},
		{"download", http.MethodGet, "/attachment", func() string { return "60" }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				refusals := 1
				if test.method == http.MethodPost {
					refusals = 4
				}
				client, calls := budgetClient(t, func(call int, response *httptest.ResponseRecorder) {
					if call <= refusals {
						refuseBudget(response, test.retryAfter())
						return
					}
					response.WriteHeader(http.StatusNoContent)
				})
				start := time.Now()
				var err error
				if test.download {
					_, _, err = client.download(t.Context(), test.path, 4096)
				} else {
					input := any(map[string]string{"k": "v"})
					if strings.HasSuffix(test.path, "/runner-sessions") {
						input = runnersession.Mutation{Key: "example-key", SessionID: 1, Provider: &store.SessionProviderIdentity{ThreadID: "example-thread"}}
					} else if strings.HasSuffix(test.path, "/events") {
						input = tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "attempt:1"}, Type: "run.observed", Data: tracker.NativeRunData{Usage: []tracker.NativeUsage{{Provider: "example", Model: "example-model", Input: 10}}}}
					}
					err = client.request(t.Context(), test.method, test.path, input, nil)
				}
				if err != nil || *calls != refusals+1 {
					t.Fatalf("calls=%d err=%v", *calls, err)
				}
				if elapsed := time.Since(start); elapsed != time.Duration(refusals)*time.Minute {
					t.Fatalf("elapsed=%s want=%s", elapsed, time.Duration(refusals)*time.Minute)
				}
			})
		})
	}
}

func TestClientRequestBudgetRefusalStopsOnCancellation(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, calls := budgetClient(t, func(_ int, response *httptest.ResponseRecorder) { refuseBudget(response, "60") })
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				start := time.Now()
				err := client.request(ctx, method, "/api/v2/organizations/org/projects/project/runner-sessions", runnersession.Mutation{SessionID: 1}, nil)
				if !errors.Is(err, context.DeadlineExceeded) || *calls != 1 {
					t.Fatalf("calls=%d err=%v", *calls, err)
				}
				if elapsed := time.Since(start); elapsed != 10*time.Second {
					t.Fatalf("elapsed=%s", elapsed)
				}
			})
		})
	}
}

func TestClientRequestBudgetContinuesAfterRepeatedRefusals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, calls := budgetClient(t, func(call int, response *httptest.ResponseRecorder) {
			if call <= 4 {
				refuseBudget(response, "60")
				return
			}
			response.WriteHeader(http.StatusNoContent)
		})
		start := time.Now()
		err := client.request(t.Context(), http.MethodGet, "/items", nil, nil)
		if err != nil || *calls != 5 {
			t.Fatalf("calls=%d err=%v", *calls, err)
		}
		if elapsed := time.Since(start); elapsed != 4*time.Minute {
			t.Fatalf("elapsed=%s", elapsed)
		}
	})
}

func TestFencedRequestRechecksAuthorityAfterTokenResolution(t *testing.T) {
	client, calls := budgetClient(t, func(_ int, response *httptest.ResponseRecorder) {
		response.WriteHeader(http.StatusNoContent)
	})
	var fenceErr error
	client.tokenSource = func() string {
		fenceErr = runner.ErrExecutionAuthorityUnavailable
		return "token"
	}
	ctx := WithRequestFence(t.Context(), nil, func() error { return fenceErr })
	if err := client.request(ctx, http.MethodPost, "/attempts/attempt_example/diff", nil, nil); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || *calls != 0 {
		t.Fatalf("request reached transport %d times, error = %v", *calls, err)
	}
}

func TestDetachedPublicationRechecksAuthorityAfterTokenResolution(t *testing.T) {
	for _, route := range []string{"phases", "progress"} {
		for _, loss := range []string{"expiry", "revocation"} {
			t.Run(route+"/"+loss, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					client, calls := budgetClient(t, func(_ int, response *httptest.ResponseRecorder) {
						response.WriteHeader(http.StatusNoContent)
					})
					item := tracker.NativeWorkItemID("wi_example")
					lease := tracker.NativeLease{ID: "lease", FencingToken: 1, WorkItemID: item}
					mutation := tracker.Mutation{IdempotencyKey: "example", LeaseID: lease.ID, FencingToken: lease.FencingToken}
					native, err := client.Native("org_example", "prj_example")
					if err != nil {
						t.Fatal(err)
					}
					live, revoke := context.WithCancelCause(t.Context())
					defer revoke(nil)
					deadline := time.Now().Add(time.Second)
					resolved := false
					client.tokenSource = func() string {
						resolved = true
						if loss == "expiry" {
							<-time.After(time.Until(deadline))
						} else {
							revoke(runner.ErrExecutionAuthorityUnavailable)
						}
						return "fixture"
					}
					ctx := context.WithoutCancel(WithMutationAuthority(live, native.Scope(), lease, func() error {
						if !time.Now().Before(deadline) {
							return runner.ErrExecutionAuthorityUnavailable
						}
						return context.Cause(live)
					}))
					if route == "phases" {
						_, err = native.PublishPhaseReceipt(ctx, item, tracker.NativePhaseReceipt{Mutation: mutation})
					} else {
						_, err = native.PublishRoundVerdict(ctx, item, progresscheck.Publication{Mutation: mutation})
					}
					if !resolved || ctx.Err() != nil || !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || *calls != 0 {
						t.Fatalf("token resolved = %t, request reached transport %d times, error = %v", resolved, *calls, err)
					}
				})
			})
		}
	}
}

func TestFencedPublicationRequestBudget(t *testing.T) {
	for _, route := range []string{"phases", "progress", "changes", "versions", "landing"} {
		for _, scenario := range []struct {
			name       string
			wantCalls  int
			wantWrites int
			wait       time.Duration
		}{
			{name: "accepted", wantCalls: 3, wantWrites: 1, wait: 2 * time.Minute},
			{name: "cancelled", wantCalls: 1, wait: 10 * time.Second},
			{name: "stale fence", wantCalls: 2, wait: time.Minute},
			{name: "unkeyed", wantCalls: 1},
		} {
			t.Run(route+"/"+scenario.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					mutation := tracker.Mutation{IdempotencyKey: "attempt:publication", LeaseID: "lease", FencingToken: 7}
					if scenario.name == "unkeyed" {
						mutation.IdempotencyKey = ""
					}
					item := tracker.NativeWorkItemID("wi_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
					receipt := tracker.NativePhaseReceipt{Mutation: mutation, Phase: "implementation", AttemptID: "attempt"}
					verdict := progresscheck.Publication{Mutation: mutation, AttemptID: "attempt", RoundInput: progresscheck.RoundInput{RoundID: "round"}}
					wantPath := "/api/v2/organizations/org_example/projects/prj_example/work-items/" + string(item) + "/" + route
					switch route {
					case "versions":
						wantPath = "/api/v2/organizations/org_example/projects/prj_example/work-items/" + string(item) + "/changes/change_example/versions"
					case "landing":
						wantPath = "/api/v2/organizations/org_example/projects/prj_example/work-items/" + string(item) + "/changes/change_example/versions/version_example/landing"
					}
					var sent []byte
					calls, writes := 0, 0
					transport := executionRoundTrip(func(req *http.Request) (*http.Response, error) {
						calls++
						if req.Method != http.MethodPost || req.URL.Path != wantPath {
							t.Fatalf("publication request = %s %s, want POST %s", req.Method, req.URL.Path, wantPath)
						}
						body, err := io.ReadAll(req.Body)
						if err != nil {
							t.Fatal(err)
						}
						var actual tracker.Mutation
						if err := json.Unmarshal(body, &actual); err != nil || actual != mutation {
							t.Fatalf("publication mutation = %#v, error = %v", actual, err)
						}
						if sent != nil && !bytes.Equal(body, sent) {
							t.Fatal("budget replay changed the publication body")
						}
						sent = body
						response := httptest.NewRecorder()
						switch {
						case calls == 1 || calls == 2 && scenario.name == "accepted":
							refuseBudget(response, "60")
						case calls == 2 && scenario.name == "stale fence":
							response.WriteHeader(http.StatusConflict)
							response.WriteString(`{"code":"stale_fencing_token"}`)
						default:
							writes++
							switch route {
							case "changes", "landing":
								response.WriteString(`{"change_id":"change_example"}`)
							case "versions":
								response.WriteString(`{"version_id":"version_example"}`)
							default:
								response.Write(body)
							}
						}
						return response.Result(), nil
					})
					client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "token" }, HTTPClient: &http.Client{Transport: transport}})
					if err != nil {
						t.Fatal(err)
					}
					native, err := client.Native("org_example", "prj_example")
					if err != nil {
						t.Fatal(err)
					}
					ctx := t.Context()
					if scenario.name == "cancelled" {
						var cancel context.CancelFunc
						ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
						defer cancel()
					}
					start := time.Now()
					switch route {
					case "phases":
						var recorded tracker.NativePhaseReceipt
						recorded, err = native.PublishPhaseReceipt(ctx, item, receipt)
						if err == nil && recorded != receipt {
							t.Fatalf("recorded receipt = %#v, want %#v", recorded, receipt)
						}
					case "progress":
						var recorded progresscheck.RoundVerdict
						recorded, err = native.PublishRoundVerdict(ctx, item, verdict)
						if err == nil && recorded.RoundID != verdict.RoundID {
							t.Fatalf("recorded verdict round = %q, want %q", recorded.RoundID, verdict.RoundID)
						}
					case "changes":
						var recorded tracker.ChangeRequest
						recorded, err = native.CreateChange(ctx, item, tracker.CreateChange{Mutation: mutation, Title: "example"})
						if err == nil && recorded.ID != "change_example" {
							t.Fatalf("recorded change = %#v", recorded)
						}
					case "versions":
						var recorded tracker.ChangeVersion
						recorded, err = native.PublishChangeVersion(ctx, item, "change_example", tracker.PublishChangeVersion{Mutation: mutation})
						if err == nil && recorded.ID != "version_example" {
							t.Fatalf("recorded version = %#v", recorded)
						}
					case "landing":
						var recorded tracker.ChangeRequest
						recorded, err = native.LandChangeVersion(ctx, item, "change_example", "version_example", tracker.LandChangeVersion{Mutation: mutation})
						if err == nil && recorded.ID != "change_example" {
							t.Fatalf("recorded landed change = %#v", recorded)
						}
					}
					switch scenario.name {
					case "accepted":
						if err != nil {
							t.Fatalf("publication failed after %d requests: %v", calls, err)
						}
					case "cancelled":
						if !errors.Is(err, context.DeadlineExceeded) {
							t.Fatalf("cancelled publication error = %v", err)
						}
					case "stale fence":
						if !ClaimLost(err) {
							t.Fatalf("stale publication error = %v", err)
						}
					case "unkeyed":
						if !isBudgetRefusal(err) {
							t.Fatalf("unkeyed publication error = %v", err)
						}
					}
					if calls != scenario.wantCalls || writes != scenario.wantWrites || time.Since(start) != scenario.wait {
						t.Fatalf("requests = %d, writes = %d, elapsed = %s", calls, writes, time.Since(start))
					}
				})
			})
		}
	}
}

func TestClientFailsFastOnOtherRefusals(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"other 429 code", http.StatusTooManyRequests, `{"code":"rate_limited"}`},
		{"other 4xx", http.StatusConflict, `{"code":"conflict"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, calls := budgetClient(t, func(call int, response *httptest.ResponseRecorder) {
					if call > 1 {
						response.WriteHeader(http.StatusNoContent)
						return
					}
					response.Header().Set("Retry-After", "60")
					response.WriteHeader(test.status)
					response.WriteString(test.body)
				})
				start := time.Now()
				err := client.request(t.Context(), http.MethodGet, "/items", nil, nil)
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Status != test.status || *calls != 1 {
					t.Fatalf("calls=%d err=%v", *calls, err)
				}
				if err := client.request(t.Context(), http.MethodGet, "/other", nil, nil); err != nil || *calls != 2 {
					t.Fatalf("later request calls=%d err=%v", *calls, err)
				}
				if elapsed := time.Since(start); elapsed != 0 {
					t.Fatalf("a non-budget refusal braked later requests: elapsed=%s", elapsed)
				}
			})
		})
	}
}

func TestClientBudgetRefusalBrakesLaterRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, calls := budgetClient(t, func(call int, response *httptest.ResponseRecorder) {
			if call == 1 {
				refuseBudget(response, "60")
				return
			}
			response.WriteHeader(http.StatusNoContent)
		})
		if err := client.request(t.Context(), http.MethodPost, "/items", map[string]string{"k": "v"}, nil); !isBudgetRefusal(err) {
			t.Fatalf("mutation err=%v", err)
		}
		start := time.Now()
		if err := client.request(t.Context(), http.MethodGet, "/other", nil, nil); err != nil || *calls != 2 {
			t.Fatalf("calls=%d err=%v", *calls, err)
		}
		if elapsed := time.Since(start); elapsed != time.Minute {
			t.Fatalf("elapsed=%s want=%s", elapsed, time.Minute)
		}
	})
}

func TestClientDoesNotReplayMutationsAfterBudgetRefusal(t *testing.T) {
	for _, test := range []struct {
		name, method, path string
		input              any
	}{
		{"JSON POST", http.MethodPost, "/items", map[string]string{"k": "v"}},
		{"JSON PATCH", http.MethodPatch, "/items", map[string]string{"k": "v"}},
		{"JSON PUT", http.MethodPut, "/items", map[string]string{"k": "v"}},
		{"DELETE", http.MethodDelete, "/items", nil},
		{"typed session on another route", http.MethodPost, "/items", runnersession.Mutation{SessionID: 1}},
		{"streaming session POST", http.MethodPost, "/api/v2/organizations/org/projects/project/runner-sessions", strings.NewReader("stream")},
		{"streaming POST", http.MethodPost, "/items", strings.NewReader("stream")},
		{"unkeyed run event", http.MethodPost, "/projects/project/work-items/item/events", tracker.NativeRunEvent{Type: "run.observed"}},
		{"run event on another route", http.MethodPost, "/items", tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "attempt:1"}}},
		{"run event on another events route", http.MethodPost, "/conversation/events", tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "attempt:1"}}},
		{"streaming run event", http.MethodPost, "/projects/project/work-items/item/events", strings.NewReader("stream")},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, calls := budgetClient(t, func(_ int, response *httptest.ResponseRecorder) { refuseBudget(response, "60") })
				start := time.Now()
				err := client.request(t.Context(), test.method, test.path, test.input, nil)
				if !isBudgetRefusal(err) || *calls != 1 {
					t.Fatalf("calls=%d err=%v", *calls, err)
				}
				if elapsed := time.Since(start); elapsed != 0 {
					t.Fatalf("elapsed=%s", elapsed)
				}
			})
		})
	}
}

func TestClientRequestBudgetExemptions(t *testing.T) {
	for _, test := range []struct {
		name, method, path string
		delay              time.Duration
	}{
		{"read waits", http.MethodGet, "/items", time.Minute},
		{"heartbeat exempt", http.MethodPost, "/machines/machine/heartbeat", 0},
		{"claim exempt", http.MethodPost, "/projects/project/claims", 0},
		{"lease renewal exempt", http.MethodPost, "/projects/project/leases/lease/renew", 0},
		{"credential renewal exempt", http.MethodPost, "/organizations/org/runners/runner/renew", 0},
		{"receipt lookup has its own window", http.MethodGet, "/projects/project/go-test-receipts?key=key", 0},
		{"receipt attestation has its own window", http.MethodPost, "/projects/project/go-test-receipts/attestations", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, calls := budgetClient(t, func(_ int, response *httptest.ResponseRecorder) { response.WriteHeader(http.StatusNoContent) })
				client.requestBudgetUntil = time.Now().Add(time.Minute)
				start := time.Now()
				if err := client.request(t.Context(), test.method, test.path, nil, nil); err != nil || *calls != 1 {
					t.Fatalf("calls=%d err=%v", *calls, err)
				}
				if elapsed := time.Since(start); elapsed != test.delay {
					t.Fatalf("elapsed=%s want=%s", elapsed, test.delay)
				}
			})
		})
	}
}

func TestValidatorReviewBudgetWaitStopsOnLeaseLoss(t *testing.T) {
	for _, test := range []struct {
		name        string
		bytes       int
		archiveWait bool
		tokenLookup bool
		expire      bool
		wantArchive int
		wantReview  int
	}{
		{name: "inline review wait", bytes: 1024, wantReview: 1},
		{name: "archived review wait", bytes: maxResponseBytes + 1024, wantArchive: 1, wantReview: 1},
		{name: "archive budget wait expires", bytes: maxResponseBytes + 1024, archiveWait: true, expire: true},
		{name: "archive budget wait revoked", bytes: maxResponseBytes + 1024, archiveWait: true},
		{name: "archive token lookup expires", bytes: maxResponseBytes + 1024, tokenLookup: true, expire: true},
		{name: "archive token lookup revoked", bytes: maxResponseBytes + 1024, tokenLookup: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const lossAfter = 10 * time.Second
				lease := tracker.NativeLease{WorkItemID: "wi_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ID: "lease", FencingToken: 7}
				lost := errors.Join(runner.ErrExecutionAuthorityUnavailable, errors.New("revoked"))
				archivePosts, reviewPosts := 0, 0
				transport := executionRoundTrip(func(req *http.Request) (*http.Response, error) {
					response := httptest.NewRecorder()
					switch {
					case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/archives"):
						archivePosts++
						raw, err := io.ReadAll(req.Body)
						if err != nil {
							t.Fatal(err)
						}
						stored, _, err := attachment.PrepareArchive("org_example", 1, bytes.NewReader(raw))
						if err != nil {
							t.Fatal(err)
						}
						if err := json.NewEncoder(response).Encode(stored); err != nil {
							t.Fatal(err)
						}
					case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/versions/version_example/reviews"):
						reviewPosts++
						refuseBudget(response, "60")
					default:
						t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
					}
					return response.Result(), nil
				})
				client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "token" }, HTTPClient: &http.Client{Transport: transport}})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_example", "prj_example")
				if err != nil {
					t.Fatal(err)
				}
				var guard context.Context
				if test.expire {
					lost = errors.Join(runner.ErrExecutionAuthorityUnavailable, errors.New("expired"))
					var cancelExpiry context.CancelFunc
					guard, cancelExpiry = context.WithTimeoutCause(t.Context(), lossAfter, lost)
					defer cancelExpiry()
				} else {
					var revoke context.CancelCauseFunc
					guard, revoke = context.WithCancelCause(t.Context())
					defer revoke(nil)
					timer := time.AfterFunc(lossAfter, func() { revoke(lost) })
					defer timer.Stop()
				}
				if test.archiveWait {
					client.DeferRequestBudget("/archives", time.Now().Add(time.Minute))
				}
				tokenLookupStarted := false
				if test.tokenLookup {
					client.tokenSource = func() string {
						tokenLookupStarted = true
						<-guard.Done()
						return "token"
					}
				}
				guarded := WithMutationAuthority(guard, native.base(), lease, nil)
				finalization, cancelFinalization := context.WithTimeout(context.WithoutCancel(guarded), 3*time.Minute)
				defer cancelFinalization()
				payload := &gate.ValidatorResult{VersionID: "version_example", SessionID: 41, Verdict: "pass", Commands: []gate.CommandResult{{Command: "make test", Output: strings.Repeat("x", test.bytes)}}}
				start := time.Now()
				err = native.PublishValidator(finalization, lease.WorkItemID, "change_example", "version_example", tracker.ReviewChange{
					Mutation:          tracker.Mutation{IdempotencyKey: "attempt:validator", LeaseID: lease.ID, FencingToken: lease.FencingToken},
					ExpectedVersionID: "version_example",
					Decision:          "approved",
					Validator:         payload,
				})
				if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || !errors.Is(err, lost) ||
					archivePosts != test.wantArchive || reviewPosts != test.wantReview || tokenLookupStarted != test.tokenLookup ||
					time.Since(start) != lossAfter || finalization.Err() != nil {
					t.Fatalf("PublishValidator returned %v after %s, archive POSTs=%d, review POSTs=%d, token lookup=%t, finalization error=%v",
						err, time.Since(start), archivePosts, reviewPosts, tokenLookupStarted, finalization.Err())
				}
			})
		})
	}
}
