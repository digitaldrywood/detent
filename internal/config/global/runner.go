package global

import (
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

func removeRunnerPolicy(root map[string]any) []string {
	var removed []string
	remove := func(attrs map[string]any, key, prefix string) {
		if _, ok := attrs[key]; ok {
			removed = append(removed, prefix+key)
			delete(attrs, key)
		}
	}
	if projects, ok := root["projects"].([]any); ok {
		for i, value := range projects {
			if project, ok := value.(map[string]any); ok {
				for _, key := range []string{"weight", "priority"} {
					remove(project, key, fmt.Sprintf("projects[%d].", i))
				}
			}
		}
	}
	for _, key := range []string{"projects", "weight", "priority", "scheduling", "fair_share"} {
		remove(root, key, "")
	}
	client, _ := root["client"].(map[string]any)
	remove(client, "native_projects", "client.")
	global, _ := root["global"].(map[string]any)
	if agents, ok := global["agents"].(map[string]any); ok {
		remove(agents, "model_selection", "global.agents.")
	}
	for _, key := range []string{"weight", "priority", "scheduling", "fair_share", "agents", "worker", "budget", "agent_pools", "active_hours", "identity", "knowledge", "local_intake_enabled"} {
		remove(global, key, "global.")
	}
	slices.Sort(removed)
	return removed
}

func normalizeRunnerConfig(root map[string]any) []string {
	client, _ := root["client"].(map[string]any)
	if identity, _ := client["identity_file"].(string); identity == "" {
		return nil
	}
	removed := removeRunnerPolicy(root)
	root["projects"] = []any{}
	global, ok := root["global"].(map[string]any)
	if !ok {
		if _, exists := root["global"]; exists {
			return removed
		}
		global = map[string]any{}
		root["global"] = global
	}
	global["scheduling"] = SchedulingWeighted
	if _, exists := global["max_concurrent_agents"]; !exists {
		capacity, exists := client["capacity"]
		if !exists {
			capacity = defaultSettings().MaxConcurrentAgents
		}
		global["max_concurrent_agents"] = capacity
	}
	return removed
}

func (c Config) MarshalYAML() (any, error) {
	type plain Config
	if c.Client.IdentityFile == "" {
		return plain(c), nil
	}
	raw, err := yaml.Marshal(plain(c))
	if err != nil {
		return nil, err
	}
	var root map[string]any
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	removeRunnerPolicy(root)
	return root, nil
}
