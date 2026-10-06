package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNewHubSchedulingRegistersRuntimeCapacityAndVersion(t *testing.T) {
	t.Setenv("HUB_WORKER_TOKEN", "worker-token")
	registered := make(chan hubclient.Machine, 1)
	descriptor := policy.Descriptor{SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")), Gates: policy.Gates{Kind: "human_review", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("stop")), MergeMethod: "squash"}}.WithID()
	claims := 0
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/repositories/acme/widgets/policy":
			_ = json.NewEncoder(response).Encode(policy.Approval{Policy: descriptor})
		case "/api/v1/machines/register":
			var machine hubclient.Machine
			if err := json.NewDecoder(request.Body).Decode(&machine); err != nil {
				t.Errorf("decode machine: %v", err)
			}
			registered <- machine
			_ = json.NewEncoder(response).Encode(machine)
		case "/api/v1/claims":
			var claim hubclient.ClaimRequest
			if err := json.NewDecoder(request.Body).Decode(&claim); err != nil {
				t.Errorf("decode claim: %v", err)
			}
			if len(claim.Repositories) != 1 || claim.Repositories[0] != "acme/widgets" || claim.PolicyID != descriptor.ID || claim.MachineID != "machine-a" {
				t.Errorf("legacy claim = %+v", claim)
			}
			claims++
			response.WriteHeader(http.StatusConflict)
			_, _ = response.Write([]byte(`{"code":"no_claimable_work","message":"none"}`))
		default:
			http.NotFound(response, request)
		}
	})
	previousTransport := http.DefaultTransport
	http.DefaultTransport = readinessRoundTripper(func(request *http.Request) (*http.Response, error) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Result(), nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	cfg := globalconfig.Config{
		Client: globalconfig.HubClient{URL: "http://hub.test", TokenEnvironment: "HUB_WORKER_TOKEN", MachineID: "machine-a"},
		Global: globalconfig.Settings{MaxConcurrentAgents: 4},
	}
	source, err := newHubScheduling(t.Context(), cfg, "")
	if err != nil {
		t.Fatalf("newHubScheduling(t.Context(), ) error = %v", err)
	}
	for _, projectID := range []string{"", "legacy"} {
		t.Run("project="+projectID, func(t *testing.T) {
			issues, err := source.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: projectID, Repository: "acme/widgets", Policy: descriptor})
			if err != nil || len(issues) != 0 {
				t.Fatalf("FetchCandidateIssues() = %#v, %v", issues, err)
			}
		})
	}
	if claims != 2 {
		t.Fatalf("legacy claims = %d, want 2", claims)
	}
	machine := <-registered
	if machine.ID != "machine-a" || machine.Capacity != 4 || machine.Version != "dev" || machine.Capabilities["os"] == "" || machine.Capabilities["arch"] == "" {
		t.Fatalf("registered machine = %#v", machine)
	}
}

func TestHubSchedulingUsesEnrolledIdentity(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(path, "https://hub.example.test")
	if err != nil {
		t.Fatal(err)
	}
	file.Identity.OrganizationID = "org_example"
	file.Identity.ProjectIDs = []tracker.ProjectID{"prj_example"}
	if err := runnerauth.Save(path, file); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*globalconfig.HubClient)
		valid  bool
	}{
		{"enrolled", func(*globalconfig.HubClient) {}, true},
		{"hostname identity override", func(c *globalconfig.HubClient) { c.MachineID = "hostname" }, false},
		{"foreign organization", func(c *globalconfig.HubClient) { c.OrganizationID = "org_other" }, false},
		{"foreign project", func(c *globalconfig.HubClient) { c.NativeProjects = map[string]string{"native": "prj_other"} }, false},
		{"Cloud project discovery pending", func(c *globalconfig.HubClient) { c.NativeProjects = nil }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := globalconfig.Config{Client: globalconfig.HubClient{URL: file.HubURL, IdentityFile: path, OrganizationID: "org_example", NativeProjects: map[string]string{"native": "prj_example"}}, Global: globalconfig.Settings{MaxConcurrentAgents: 1}}
			test.change(&cfg.Client)
			_, err := newHubScheduling(t.Context(), cfg, "test")
			if (err == nil) != test.valid {
				t.Fatalf("configured=%v, want %v: %v", err == nil, test.valid, err)
			}
		})
	}
}

func TestNewHubSchedulingRequiresConfiguredToken(t *testing.T) {
	t.Setenv("EMPTY_HUB_TOKEN", "")
	_, err := newHubScheduling(t.Context(), globalconfig.Config{Client: globalconfig.HubClient{URL: "https://hub.example.test", TokenEnvironment: "EMPTY_HUB_TOKEN"}}, "dev")
	if err == nil {
		t.Fatal("newHubScheduling(t.Context(), ) error = nil, want missing token error")
	}
}
