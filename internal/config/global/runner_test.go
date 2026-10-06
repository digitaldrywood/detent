package global

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRunnerMachineConfiguration(t *testing.T) {
	for _, test := range []struct {
		name, root, global, client string
		warnings                   []string
		wantError                  string
	}{
		{name: "machine only"},
		{name: "projects", root: "projects: [{id: old, workflow: /missing/WORKFLOW.md, workdir: /missing, weight: invalid, priority: []}]\n", warnings: []string{"projects", "projects[0].weight", "projects[0].priority"}},
		{name: "malformed projects", root: "projects: false\n", warnings: []string{"projects"}},
		{name: "weight", global: "  weight: []\n", warnings: []string{"global.weight"}},
		{name: "priority", global: "  priority: []\n", warnings: []string{"global.priority"}},
		{name: "scheduling", global: "  scheduling: invalid\n", warnings: []string{"global.scheduling"}},
		{name: "fair share", global: "  fair_share: []\n", warnings: []string{"global.fair_share"}},
		{name: "model selection", global: "  agents: {model_selection: invalid}\n", warnings: []string{"global.agents.model_selection"}},
		{name: "local project mapping", client: "  native_projects: false\n", warnings: []string{"client.native_projects"}},
		{name: "policy defaults", global: "  worker: invalid\n  budget: invalid\n  agent_pools: invalid\n  active_hours: invalid\n", warnings: []string{"global.worker", "global.budget", "global.agent_pools", "global.active_hours"}},
		{name: "invalid machine setting", global: "  max_concurrent_agents: 0\n", wantError: "global.max_concurrent_agents"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			root := t.TempDir()
			workspace := filepath.Join(root, "checkouts")
			raw := "apiVersion: detent/v1\nkind: GlobalConfig\nworkspace_root: " + workspace + "\nclient:\n  hub_url: https://cloud.example.test\n  organization_id: org_example\n  identity_file: " + filepath.Join(root, "identity.json") + "\n  capacity: 3\n" + test.client + test.root + "global:\n  memory: {max_agent_rss_bytes: 123456}\n" + test.global
			cfg, err := Parse([]byte(raw), filepath.Join(root, "global.yaml"))
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("Parse() = %v, want %s", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Projects) != 0 || len(cfg.Client.NativeProjects) != 0 || cfg.Global.Agents.ModelSelection.Configured() || cfg.Global.MaxConcurrentAgents != 3 || cfg.Global.Memory.MaxAgentRSSBytes != 123456 || cfg.WorkspaceRoot != workspace {
				t.Fatalf("machine config = %+v", cfg)
			}
			for _, key := range test.warnings {
				if !strings.Contains(logs.String(), `"key":"`+key+`"`) {
					t.Errorf("missing warning for %s: %s", key, logs.String())
				}
			}
			if len(test.warnings) == 0 && logs.Len() != 0 {
				t.Fatalf("machine-only file warned: %s", logs.String())
			}
			encoded, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
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
