package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReleaseTerminalRetriesOnlyUnavailableReleases(t *testing.T) {
	unavailable := func(w *httptest.ResponseRecorder) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.WriteString(`{"code":"service_unavailable","message":"Database transaction was aborted; retry the request"}`)
	}
	released := func(w *httptest.ResponseRecorder) { w.WriteHeader(http.StatusNoContent) }
	for _, test := range []struct {
		name      string
		responses []func(*httptest.ResponseRecorder)
		parent    func(context.Context) context.Context
		calls     int
		released  bool
		lost      bool
	}{
		{name: "released first time", responses: []func(*httptest.ResponseRecorder){released}, calls: 1, released: true},
		{name: "deadlock retried at once", responses: []func(*httptest.ResponseRecorder){unavailable, released}, calls: 2, released: true},
		{name: "repeated outage backs off", responses: []func(*httptest.ResponseRecorder){unavailable, unavailable, unavailable, released}, calls: 4, released: true},
		{name: "spent caller budget", responses: []func(*httptest.ResponseRecorder){released}, parent: func(ctx context.Context) context.Context {
			spent, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
			cancel()
			return spent
		}, calls: 1, released: true},
		{name: "committed earlier release", responses: []func(*httptest.ResponseRecorder){unavailable, func(w *httptest.ResponseRecorder) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.WriteString(`{"code":"stale_fencing_token"}`)
		}}, calls: 2, lost: true},
		{name: "definite refusal not retried", responses: []func(*httptest.ResponseRecorder){func(w *httptest.ResponseRecorder) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.WriteString(`{"code":"invalid_request"}`)
		}}, calls: 1},
		{name: "outage bounded", responses: []func(*httptest.ResponseRecorder){unavailable}, calls: 8},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "token" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
					if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/leases/lease_test/release") {
						t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
					}
					respond := test.responses[min(calls, len(test.responses)-1)]
					calls++
					response := httptest.NewRecorder()
					respond(response)
					return response.Result(), nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_test", "prj_test")
				if err != nil {
					t.Fatal(err)
				}
				ctx := t.Context()
				if test.parent != nil {
					ctx = test.parent(ctx)
				}
				started := time.Now()
				err = native.ReleaseTerminal(ctx, tracker.NativeLease{ID: "lease_test", WorkItemID: "wi_test", FencingToken: 3}, "released")
				switch {
				case test.released && err != nil:
					t.Fatalf("release failed: %v", err)
				case test.lost && !ClaimLost(err):
					t.Fatalf("committed release was not reported as lost: %v", err)
				case !test.released && !test.lost && err == nil:
					t.Fatal("refused release reported success")
				}
				if test.calls == 8 {
					if calls < 3 || calls > 16 || time.Since(started) > terminalReleaseTimeout {
						t.Fatalf("outage retries = %d over %s", calls, time.Since(started))
					}
					var apiErr *APIError
					if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
						t.Fatalf("outage lost primary error: %v", err)
					}
					return
				}
				if calls != test.calls {
					t.Fatalf("calls = %d, want %d", calls, test.calls)
				}
			})
		})
	}
}
