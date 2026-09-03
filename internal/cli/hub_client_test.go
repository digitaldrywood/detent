package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/orchestrator"
)

func TestNewHubSchedulingRegistersRuntimeCapacityAndVersion(t *testing.T) {
	t.Setenv("HUB_WORKER_TOKEN", "worker-token")
	registered := make(chan hubclient.Machine, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/machines/register":
			var machine hubclient.Machine
			if err := json.NewDecoder(request.Body).Decode(&machine); err != nil {
				t.Errorf("decode machine: %v", err)
			}
			registered <- machine
			_ = json.NewEncoder(response).Encode(machine)
		case "/api/v1/claims":
			response.WriteHeader(http.StatusConflict)
			_, _ = response.Write([]byte(`{"code":"no_claimable_work","message":"none"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	cfg := globalconfig.Config{
		Client: globalconfig.HubClient{URL: server.URL, TokenEnvironment: "HUB_WORKER_TOKEN", MachineID: "machine-a"},
		Global: globalconfig.Settings{MaxConcurrentAgents: 4},
	}
	source, err := newHubScheduling(cfg, "")
	if err != nil {
		t.Fatalf("newHubScheduling() error = %v", err)
	}
	issues, err := source.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Repository: "acme/widgets"})
	if err != nil || len(issues) != 0 {
		t.Fatalf("FetchCandidateIssues() = %#v, %v", issues, err)
	}
	machine := <-registered
	if machine.ID != "machine-a" || machine.Capacity != 4 || machine.Version != "dev" || machine.Capabilities["os"] == "" || machine.Capabilities["arch"] == "" {
		t.Fatalf("registered machine = %#v", machine)
	}
}

func TestNewHubSchedulingRequiresConfiguredToken(t *testing.T) {
	t.Setenv("EMPTY_HUB_TOKEN", "")
	_, err := newHubScheduling(globalconfig.Config{Client: globalconfig.HubClient{URL: "https://hub.example.test", TokenEnvironment: "EMPTY_HUB_TOKEN"}}, "dev")
	if err == nil {
		t.Fatal("newHubScheduling() error = nil, want missing token error")
	}
}
