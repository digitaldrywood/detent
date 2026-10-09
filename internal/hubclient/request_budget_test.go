package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"
)

func TestClientHonorsRequestBudget(t *testing.T) {
	for _, test := range []struct {
		name, method, path string
		download, cancel   bool
		delay              time.Duration
	}{
		{"read waits", http.MethodGet, "/items", false, false, time.Minute},
		{"HTTP date waits", http.MethodGet, "/items", false, false, time.Minute},
		{"malformed header does not wait", http.MethodGet, "/items", false, false, 0},
		{"mutation waits", http.MethodPost, "/items", false, false, time.Minute},
		{"download waits", http.MethodGet, "/attachment", true, false, time.Minute},
		{"heartbeat exempt", http.MethodPost, "/machines/machine/heartbeat", false, false, 0},
		{"claim exempt", http.MethodPost, "/projects/project/claims", false, false, 0},
		{"lease renewal exempt", http.MethodPost, "/projects/project/leases/lease/renew", false, false, 0},
		{"credential renewal exempt", http.MethodPost, "/organizations/org/runners/runner/renew", false, false, 0},
		{"cancellation stops wait", http.MethodGet, "/items", false, true, 10 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "token" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
					calls++
					response := httptest.NewRecorder()
					if calls == 1 {
						header := "60"
						if test.name == "HTTP date waits" {
							header = time.Now().Add(time.Minute).UTC().Format(http.TimeFormat)
						}
						if test.name == "malformed header does not wait" {
							header = "invalid"
						}
						response.Header().Set("Retry-After", header)
						response.WriteHeader(http.StatusTooManyRequests)
						response.WriteString(`{"code":"runner_request_budget"}`)
					} else {
						response.WriteHeader(http.StatusNoContent)
					}
					return response.Result(), nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				err = client.request(t.Context(), http.MethodGet, "/items", nil, nil)
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Status != 429 {
					t.Fatalf("first request=%v", err)
				}
				start := time.Now()
				ctx := t.Context()
				if test.cancel {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
					defer cancel()
				}
				if test.download {
					_, _, err = client.download(ctx, test.path, 100)
				} else {
					err = client.request(ctx, test.method, test.path, nil, nil)
				}
				if test.cancel {
					if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
						t.Fatalf("cancel: calls=%d err=%v", calls, err)
					}
				} else if err != nil || calls != 2 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
				if elapsed := time.Since(start); elapsed != test.delay {
					t.Fatalf("elapsed=%s want=%s", elapsed, test.delay)
				}
			})
		})
	}
}
