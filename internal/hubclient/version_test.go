package hubclient

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestHubVersion(t *testing.T) {
	t.Parallel()
	unavailable := errors.New("Hub transport unavailable")
	tests := []struct {
		name      string
		body      string
		status    int
		transport error
		want      string
		wantError bool
		retried   bool
	}{
		{name: "running Hub version", body: `{"version":" v1.2.4 "}`, want: "v1.2.4"},
		{name: "old Hub omits version", body: `{"features":["native_issues"]}`, wantError: true},
		{name: "empty Hub version", body: `{"version":" "}`, wantError: true},
		{name: "invalid response", body: `{`, wantError: true, retried: true},
		{name: "Hub unavailable", body: `{"code":"unavailable"}`, status: http.StatusServiceUnavailable, wantError: true, retried: true},
		{name: "transport unavailable", transport: unavailable, wantError: true, retried: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			requests := 0
			client, err := New(Config{
				URL:         "https://hub.example.test",
				TokenSource: func() string { return "test-worker-token" },
				HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					requests++
					if request.URL.Path != "/api/v2/capabilities" || request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer test-worker-token" {
						t.Fatalf("unexpected version request: %s %s", request.Method, request.URL.Path)
					}
					if test.transport != nil {
						return nil, test.transport
					}
					status := test.status
					if status == 0 {
						status = http.StatusOK
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(test.body)), Header: make(http.Header)}, nil
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			version, err := client.Version(t.Context())
			if version != test.want || (err != nil) != test.wantError || requests != 1 {
				t.Fatalf("Version() = %q, %v, requests %d; want %q, error %t, one request", version, err, requests, test.want, test.wantError)
			}
			if test.transport != nil && (!errors.Is(err, unavailable) || !errors.Is(err, ErrUnavailable)) {
				t.Fatalf("Version() lost transport error: %v", err)
			}
			again, err := client.Version(t.Context())
			wantRequests := 1
			if test.retried {
				wantRequests = 2
			}
			if again != test.want || (err != nil) != test.wantError || requests != wantRequests {
				t.Fatalf("second Version() = %q, %v, requests %d; want %q, error %t, %d requests", again, err, requests, test.want, test.wantError, wantRequests)
			}
		})
	}
}
