package global

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

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
	client := runnerMapping(root["client"])
	remove(client, "native_projects", "client.")
	global := runnerMapping(root["global"])
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
	client := runnerMapping(root["client"])
	if projectStringValue(client, "identity_file") == "" {
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

func runnerCheckoutLocations(root map[string]any, opts options) []Project {
	client := runnerMapping(root["client"])
	if projectStringValue(client, "identity_file") == "" {
		return nil
	}
	projects, ok := root["projects"].([]any)
	if !ok {
		return nil
	}
	var retained []Project
	for _, value := range projects {
		attrs := runnerMapping(value)
		name := projectStringValue(attrs, "id")
		workdir := projectStringValue(attrs, "workdir")
		if strings.TrimSpace(name) == "" || strings.TrimSpace(workdir) == "" {
			continue
		}
		expanded, err := expandPath(workdir, opts)
		if err != nil || !filepath.IsAbs(expanded) {
			continue
		}
		retained = append(retained, Project{ID: name, Workdir: expanded})
	}
	return retained
}

func runnerMapping(value any) map[string]any {
	attrs, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return attrs
}
