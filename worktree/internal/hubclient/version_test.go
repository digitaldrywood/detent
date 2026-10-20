package hubclient

import (
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestHubVersion(t *testing.T) {
	t.Parallel()
	unavailable := errors.New("Hub transport unavailable")
	tests := []struct {
		name      string
		url       string
		runner    bool
		wantPath  string
		body      string
		status    int
		transport error
		want      string
		wantError bool
		retried   bool
	}{
		{name: "Cloud runner origin", url: "https://cloud.detent.build", runner: true, wantPath: "/organizations/org_example/api/v2/capabilities", body: `{"version":"v1.2.4","minimum_runner_version":"1.2.3"}`, want: "v1.2.4"},
		{name: "staging Cloud runner origin", url: "https://staging.cloud.detent.build", runner: true, wantPath: "/organizations/org_example/api/v2/capabilities", body: `{"version":"v1.2.4","minimum_runner_version":"1.2.3"}`, want: "v1.2.4"},
		{name: "legacy Cloud runner origin", url: "https://hub.detent.build", runner: true, wantPath: "/organizations/org_example/api/v2/capabilities", body: `{"version":"v1.2.4","minimum_runner_version":"1.2.3"}`, want: "v1.2.4"},
		{name: "legacy staging runner origin", url: "https://staging.hub.detent.build", runner: true, wantPath: "/organizations/org_example/api/v2/capabilities", body: `{"version":"v1.2.4","minimum_runner_version":"1.2.3"}`, want: "v1.2.4"},
		{name: "Cloud runner organization path", url: "https://cloud.detent.build/organizations/org_example", runner: true, wantPath: "/organizations/org_example/api/v2/capabilities", body: `{"version":"v1.2.4","minimum_runner_version":"1.2.3"}`, want: "v1.2.4"},
		{name: "self hosted runner", url: "https://hub.example.test", runner: true, wantPath: "/api/v2/capabilities", body: `{"version":"v1.2.4","minimum_runner_version":"1.2.3"}`, want: "v1.2.4"},
		{name: "self hosted runner path", url: "https://hub.example.test/prefix", runner: true, wantPath: "/prefix/api/v2/capabilities", body: `{"version":"v1.2.4","minimum_runner_version":"1.2.3"}`, want: "v1.2.4"},
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
			base := test.url
			if base == "" {
				base = "https://hub.example.test"
			}
			wantPath := test.wantPath
			if wantPath == "" {
				wantPath = "/api/v2/capabilities"
			}
			token := "test-worker-token"
			identityPath := ""
			if test.runner {
				identityPath = filepath.Join(t.TempDir(), "runner", "identity.json")
				file, err := runnerauth.Initialize(identityPath, base)
				if err != nil {
					t.Fatal(err)
				}
				file.Identity.OrganizationID = "org_example"
				file.Identity.Operations = []string{runnerauth.Read}
				file.Identity.ExpiresAt = time.Now().Add(runnerauth.CredentialTTL)
				if err := runnerauth.Save(identityPath, file); err != nil {
					t.Fatal(err)
				}
				token = file.Credential
			}
			client, err := New(Config{
				URL:          base,
				IdentityFile: identityPath,
				TokenSource:  func() string { return token },
				HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					requests++
					if request.URL.Path != wantPath || request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer "+token {
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
			if test.runner {
				minimum, err := client.MinimumRunnerVersion(t.Context())
				if err != nil || minimum != "1.2.3" {
					t.Fatalf("MinimumRunnerVersion() = %q, %v", minimum, err)
				}
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
