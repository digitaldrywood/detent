package global

import (
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRunnerMachineConfiguration(t *testing.T) {
	for _, test := range []struct {
		name, root, global, client string
		wantKeys                   []string
		wantError                  string
		wantMemory                 int64
	}{
		{name: "machine only"},
		{name: "retired dashboard access", root: "dashboard_access: []\n", wantError: "dashboard_access: retired configuration key"},
		{name: "retired loopback read", root: "trust_loopback_peer_read: false\n", wantError: "trust_loopback_peer_read: retired configuration key"},
		{name: "retired local intake", global: "  local_intake_enabled: false\n", wantError: "global.local_intake_enabled: retired configuration key"},

		{name: "memory limit", global: "  memory: {max_agent_rss_bytes: 123456}\n", wantMemory: 123456},
		{name: "projects", root: "projects: [{id: old, workflow: /missing/WORKFLOW.md, workdir: /missing, weight: invalid, priority: []}]\n", wantKeys: []string{"projects"}},
		{name: "malformed projects", root: "projects: false\n", wantKeys: []string{"projects"}},
		{name: "top-level weight", root: "weight: invalid\n", wantKeys: []string{"weight"}},
		{name: "top-level priority", root: "priority: []\n", wantKeys: []string{"priority"}},
		{name: "top-level scheduling", root: "scheduling: invalid\n", wantKeys: []string{"scheduling"}},
		{name: "top-level fair share", root: "fair_share: []\n", wantKeys: []string{"fair_share"}},
		{name: "weight", global: "  weight: []\n", wantKeys: []string{"global.weight"}},
		{name: "priority", global: "  priority: []\n", wantKeys: []string{"global.priority"}},
		{name: "scheduling", global: "  scheduling: invalid\n", wantKeys: []string{"global.scheduling"}},
		{name: "fair share", global: "  fair_share: []\n", wantKeys: []string{"global.fair_share"}},
		{name: "model selection", global: "  agents: {model_selection: invalid}\n", wantKeys: []string{"global.agents.model_selection"}},
		{name: "local project mapping", client: "  native_projects: false\n", wantKeys: []string{"client.native_projects"}},
		{name: "policy defaults", global: "  worker: invalid\n  budget: invalid\n  agent_pools: invalid\n  active_hours: invalid\n", wantKeys: []string{"global.worker", "global.budget", "global.agent_pools", "global.active_hours"}},
		{name: "unknown top-level key", root: "unexpected: secret-value\n", wantKeys: []string{"unexpected"}},
		{name: "unknown client key", client: "  capcity: 4\n", wantKeys: []string{"client.capcity"}},
		{name: "unknown global key", global: "  unexpected: true\n", wantKeys: []string{"global.unexpected"}},
		{name: "unknown memory key", global: "  memory: {poll_intervl_ms: 1000}\n", wantKeys: []string{"global.memory.poll_intervl_ms"}},
		{name: "unknown CPU key", global: "  cpu: {unexpected: true}\n", wantKeys: []string{"global.cpu.unexpected"}},
		{name: "unknown IO key", global: "  io: {unexpected: true}\n", wantKeys: []string{"global.io.unexpected"}},
		{name: "unknown cache key", global: "  cache: {unexpected: true}\n", wantKeys: []string{"global.cache.unexpected"}},
		{name: "unknown startup key", global: "  startup: {unexpected: true}\n", wantKeys: []string{"global.startup.unexpected"}},
		{name: "unknown pacing key", global: "  rate_window_pacing: {unexpected: true}\n", wantKeys: []string{"global.rate_window_pacing.unexpected"}},
		{name: "unknown nested notification key", root: "notifications: {health: {webhook: {unexpected: true}}}\n", wantKeys: []string{"notifications.health.webhook.unexpected"}},
		{name: "unknown profiling capture key", root: "profiling: {capture: {unexpected: true}}\n", wantKeys: []string{"profiling.capture.unexpected"}},
		{name: "removed and unknown keys", root: "weight: 1\nupdate: {unexpected: true}\n", wantKeys: []string{"weight", "update.unexpected"}},
		{name: "unknown merged key", root: "<<: {unexpected: true}\n", wantKeys: []string{"unexpected"}},
		{name: "machine controls", global: "  cpu: {go_build_budget: 2}\n  io: {poll_interval_ms: 500}\n  cache: {max_age: 24h}\n  startup: {jitter_seconds: 0, max_spawn_per_second: 2, max_concurrent_starts: 2}\n", root: "log_level: warn\nnotifications: {health: {webhook: {url: https://example.test/hook, headers: {X-Example: value}}}}\n"},
		{name: "invalid machine setting", global: "  max_concurrent_agents: 0\n", wantError: "global.max_concurrent_agents"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "checkouts")
			globalAttrs := "{}\n"
			if test.global != "" {
				globalAttrs = "\n" + test.global
			}
			raw := "apiVersion: detent/v1\nkind: GlobalConfig\nworkspace_root: " + workspace + "\nclient:\n  hub_url: https://cloud.example.test\n  organization_id: org_example\n  identity_file: " + filepath.Join(root, "identity.json") + "\n  capacity: 3\n" + test.client + test.root + "global: " + globalAttrs
			cfg, err := Parse([]byte(raw), filepath.Join(root, "global.yaml"))
			if len(test.wantKeys) > 0 {
				if err == nil {
					t.Fatal("Parse() accepted unsupported runner keys")
				}
				if strings.Contains(err.Error(), "secret-value") {
					t.Fatal("validation error exposed a configuration value")
				}
				for _, key := range test.wantKeys {
					if !strings.Contains(err.Error(), key+": ") {
						t.Errorf("Parse() = %v, want key %s", err, key)
					}
				}
				return
			}
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("Parse() = %v, want %s", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantMemory := test.wantMemory
			if wantMemory == 0 {
				wantMemory = DefaultMaxAgentRSSBytes
			}
			if len(cfg.Projects) != 0 || len(cfg.Client.NativeProjects) != 0 || cfg.Global.Agents.ModelSelection.Configured() || cfg.Global.MaxConcurrentAgents != 3 || cfg.Global.Memory.MaxAgentRSSBytes != wantMemory || cfg.WorkspaceRoot != workspace {
				t.Fatalf("machine config = %+v", cfg)
			}
			encoded, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(encoded, cfg.Path); err != nil {
				t.Fatalf("Parse(marshaled machine config) = %v", err)
			}
			var persisted map[string]any
			if err := yaml.Unmarshal(encoded, &persisted); err != nil {
				t.Fatal(err)
			}
			global := persisted["global"].(map[string]any)
			client := persisted["client"].(map[string]any)
			for _, key := range []string{"scheduling", "fair_share", "agents"} {
				if _, exists := global[key]; exists {
					t.Errorf("machine config persisted %s", key)
				}
			}
			if persisted["projects"] != nil || client["native_projects"] != nil {
				t.Fatal("machine config persisted project routing")
			}
		})
	}
}
