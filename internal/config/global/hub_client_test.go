package global

import (
	"strings"
	"testing"
	"time"
)

func TestParseHubClient(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(`apiVersion: detent/v1
kind: GlobalConfig
client:
  hub_url: https://hub.example.test/
  token_env: HUB_TOKEN
  machine_id: machine-a
  display_name: Worker A
  capacity: 3
  heartbeat_interval_seconds: 20
  lease_ttl_seconds: 75
  request_timeout_ms: 2500
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects: []
`), "hub-client.yaml")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	client := cfg.Client.Normalized()
	if client.URL != "https://hub.example.test" || client.TokenEnvironment != "HUB_TOKEN" || client.MachineID != "machine-a" || client.DisplayName != "Worker A" || client.Capacity != 3 {
		t.Fatalf("Client = %#v", client)
	}
	if client.HeartbeatInterval() != 20*time.Second || client.LeaseTTL() != 75*time.Second || client.RequestTimeout() != 2500*time.Millisecond {
		t.Fatalf("Client durations = heartbeat %s TTL %s timeout %s", client.HeartbeatInterval(), client.LeaseTTL(), client.RequestTimeout())
	}
}

func TestHubClientValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{name: "mapping", config: "client: enabled", want: "client: must be a mapping"},
		{name: "URL required", config: "client:\n  capacity: 2", want: "client.hub_url is required"},
		{name: "absolute URL", config: "client:\n  hub_url: hub.internal", want: "client.hub_url must be an absolute"},
		{name: "token environment", config: "client:\n  hub_url: https://hub.example.test\n  token_env: invalid-name", want: "client.token_env must be an environment variable name"},
		{name: "heartbeat before lease", config: "client:\n  hub_url: https://hub.example.test\n  heartbeat_interval_seconds: 90\n  lease_ttl_seconds: 90", want: "client.heartbeat_interval_seconds must be shorter"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(`apiVersion: detent/v1
kind: GlobalConfig
`+test.config+`
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects: []
`), test.name+".yaml")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, test.want)
			}
		})
	}
}
