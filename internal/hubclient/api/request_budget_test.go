package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/store"
)

func budgetClient(t *testing.T, respond func(call int, response *httptest.ResponseRecorder)) (*Client, *int) {
	t.Helper()
	calls := 0
	var mutationBody []byte
	client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "token" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/runner-sessions") {
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
