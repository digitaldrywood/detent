package hubserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedFleetRunnerViewBackendIsolationJSON(t *testing.T) {
	tests := []struct {
		name      string
		isolation map[string][]string
		want      string
	}{
		{name: "nil tiers become empty list", isolation: map[string][]string{"native/codex": nil}, want: `{"native/codex":[]}`},
		{name: "populated tiers kept", isolation: map[string][]string{"codex": {"sandbox", "native-trusted"}}, want: `{"codex":["sandbox","native-trusted"]}`},
		{name: "no backends", isolation: nil, want: `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			view := hostedFleetRunnerView(runnerauth.Runner{BackendIsolation: test.isolation}, "test", nil, time.Unix(0, 0))
			got, err := json.Marshal(view.BackendIsolation)
			if err != nil {
				t.Fatalf("marshal backend isolation: %v", err)
			}
			if string(got) != test.want {
				t.Fatalf("backend_isolation = %s, want %s", got, test.want)
			}
		})
	}
}

func TestHostedFleetProductionContract(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	response := hostedFleetResponse{Current: "v0.169.1", MinimumRunnerVersion: "v0.169.1", Usage: hostedFleetUsage{Allowances: map[string]hostedFleetAllowance{}}}
	for i, platform := range []string{"linux", "darwin", "linux"} {
		id := fmt.Sprintf("runner_%d", i)
		runner := runnerauth.Runner{OS: platform, Architecture: "arm64", Hostname: id + ".example.com", HostCapacity: 4, ReportedCapacity: 4, LastHeartbeatAt: now, Health: "online", ConnectionHealth: "online", BackendIsolation: map[string][]string{"native/codex": nil}}
		runner.RunnerID = id
		runner.MachineID = tracker.MachineID("machine_" + id)
		runner.Routing = (runnerauth.Routing{DisplayName: id, State: "active", CapacityLimit: 4, ProjectIDs: []tracker.ProjectID{"project_first", "project_second"}, ProjectRanks: map[tracker.ProjectID]int{"project_first": 1, "project_second": 2}, ProjectCapacityLimits: map[tracker.ProjectID]int{"project_first": 2}, UpdateRequest: &runnerauth.UpdateRequest{ID: "update_" + id, Version: "v0.169.1", RequestedAt: now, Service: "detent"}}).Normalized()
		percent := 42.0
		runner.ProviderCapacity = []providercapacity.View{{Report: providercapacity.Report{Provider: "openai", Backend: "native/codex", AccountAlias: "example", Billing: []string{"unknown", "subscription", "subscription"}[i], Availability: "available", ObservedAt: now, UsageWindows: []providercapacity.UsageWindow{{Kind: "primary", PercentUsed: &percent, ObservedAt: now, ResetAt: now.Add(time.Hour)}}}, State: "ready"}}
		runner.Problems = []runnerauth.Problem{{Code: "setup_failed", Message: "Example setup failed", FixHint: "Run setup", Check: "setup", ErrorOutput: "Example error", FixCommand: "make setup", FirstSeen: now, ReportedAt: now}}
		view := hostedFleetRunnerView(runner, "v0.169.0", nil, now)
		routing := runner.Routing
		routing.UpdateRequest = nil
		view.Routing = &routing
		view.Capacity = &runnerauth.CapacityView{RunnerID: id, Scope: "runner", Desired: 4, Status: "unavailable", Limits: []runnerauth.CapacityLimit{{Owner: "runner"}}, Projects: map[tracker.ProjectID]runnerauth.ProjectCapacityView{"project_first": {}, "project_second": {}}}
		response.Runners = append(response.Runners, view)
	}
	raw, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := "testdata/fleet-production.json"
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(expected), raw) {
		t.Fatal("fleet producer differs from shared web contract fixture")
	}
}
