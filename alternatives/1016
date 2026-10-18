package global

import (
	"fmt"
	"reflect"
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

func runnerKeyErrors(root map[string]any) []string {
	if projectStringValue(runnerMapping(root["client"]), "identity_file") == "" {
		return nil
	}
	var problems []string
	for _, key := range removeRunnerPolicy(root) {
		problems = append(problems, key+": unsupported runner configuration key; project and model policy belongs in Cloud")
	}
	problems = append(problems, unknownRunnerKeys(root, reflect.TypeFor[Config](), "")...)
	slices.Sort(problems)
	return problems
}

func unknownRunnerKeys(value any, schema reflect.Type, prefix string) []string {
	for schema.Kind() == reflect.Pointer {
		schema = schema.Elem()
	}
	attrs, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	fields := make(map[string]reflect.Type)
	if schema.Kind() == reflect.Struct {
		for field := range schema.Fields() {
			key := strings.Split(field.Tag.Get("yaml"), ",")[0]
			if field.IsExported() && key != "-" {
				if key == "" {
					key = strings.ToLower(field.Name)
				}
				fields[key] = field.Type
			}
		}
	} else if prefix == "global.startup." {
		for _, key := range []string{"jitter_seconds", "max_spawn_per_second", "max_concurrent_starts"} {
			fields[key] = reflect.TypeFor[int]()
		}
	} else if schema.Kind() == reflect.Map {
		return nil
	}
	var problems []string
	for key, nested := range attrs {
		typ, exists := fields[key]
		if !exists {
			problems = append(problems, prefix+key+": unknown runner configuration key")
			continue
		}
		problems = append(problems, unknownRunnerKeys(nested, typ, prefix+key+".")...)
	}
	return problems
}

func normalizeRunnerConfig(root map[string]any) {
	client := runnerMapping(root["client"])
	if projectStringValue(client, "identity_file") == "" {
		return
	}
	root["projects"] = []any{}
	global, ok := root["global"].(map[string]any)
	if !ok {
		if _, exists := root["global"]; exists {
			return
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

func runnerMapping(value any) map[string]any {
	attrs, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return attrs
}
