package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/policy"
)

const PolicyCanonicalizationVersion = 2

func nativeAuthoredSources(sources ProjectDefinitionSources) (ProjectDefinitionSources, error) {
	for _, input := range []struct {
		raw      *[]byte
		markdown bool
	}{
		{&sources.Workflow, true}, {&sources.Config, false}, {&sources.LocalWorkflow, true}, {&sources.LocalConfig, false},
	} {
		if len(*input.raw) == 0 {
			continue
		}
		raw := *input.raw
		var document projectWorkflowDocument
		var err error
		if input.markdown {
			document, err = splitProjectWorkflow(raw)
			if err != nil {
				return sources, err
			}
			raw = document.frontmatter
		}
		var node yaml.Node
		if err := yaml.Unmarshal(raw, &node); err != nil {
			return sources, err
		}
		if len(node.Content) > 0 {
			expanded, err := expandAuthoredAliases(&node, make(map[*yaml.Node]bool))
			if err != nil {
				return sources, err
			}
			node = *expanded
			root := node.Content[0]
			retainAuthoredKeys(root, "schema", "review", "runners", "workpad", "deliverable", "dependencies", "recovery", "agent", "gate", "plan", "budget", "release", "retro", "operator", "backlog_admission", "tracker", "server", "worker")
			retainAuthoredPath(root, []string{"tracker"}, "kind", "repository", "lanes", "active_states", "observed_states", "terminal_states", "state_map", "priority_map", "dependency_auto_unblock", "blocked_recovery", "blocker_auto_promote")
			retainAuthoredPath(root, []string{"worker"}, "extra_network_domains", "allow_local_binding")
			retainAuthoredPath(root, []string{"server"}, "kanban")
			retainAuthoredPath(root, []string{"server", "kanban"}, "allowed_transitions")
			for _, path := range []string{"deliverable.output_root", "deliverable.review_url", "agent.max_concurrent_agents", "agent.max_concurrent_agents_by_state", "agent.rate_window_pacing", "agent.shutdown", "agent.lessons.path", "agent.knowledge.sources", "agent.skills.path", "agent.budget.pricing_path", "budget.pricing_path"} {
				removeAuthoredPath(root, strings.Split(path, "."))
			}
			sortAuthoredMappings(&node)
			raw, err = yaml.Marshal(&node)
			if err != nil {
				return sources, err
			}
		}
		if input.markdown && document.hasFrontmatter {
			*input.raw = append(append(append([]byte("---\n"), raw...), []byte("---\n")...), []byte(strings.TrimSpace(string(normalizeProjectDefinitionPrompt(document.prompt))))...)
		} else if input.markdown {
			*input.raw = []byte(strings.TrimSpace(string(normalizeProjectDefinitionPrompt(document.prompt))))
		} else {
			*input.raw = raw
		}
	}
	return sources, nil
}

func expandAuthoredAliases(node *yaml.Node, ancestors map[*yaml.Node]bool) (*yaml.Node, error) {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	if node == nil || ancestors[node] {
		return nil, errors.New("recursive authored YAML alias")
	}
	ancestors[node] = true
	defer delete(ancestors, node)
	copy := *node
	copy.Anchor, copy.Alias = "", nil
	copy.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		expanded, err := expandAuthoredAliases(child, ancestors)
		if err != nil {
			return nil, err
		}
		copy.Content[i] = expanded
	}
	return &copy, nil
}

func sortAuthoredMappings(node *yaml.Node) {
	node.Style = 0
	for _, child := range node.Content {
		sortAuthoredMappings(child)
	}
	if node.Kind != yaml.MappingNode {
		return
	}
	type pair struct{ key, value *yaml.Node }
	pairs := make([]pair, 0, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		pairs = append(pairs, pair{node.Content[i], node.Content[i+1]})
	}
	slices.SortFunc(pairs, func(a, b pair) int { return strings.Compare(a.key.Value, b.key.Value) })
	for i, pair := range pairs {
		node.Content[2*i], node.Content[2*i+1] = pair.key, pair.value
	}
}

func retainAuthoredKeys(node *yaml.Node, names ...string) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	var content []*yaml.Node
	for i := 0; i < len(node.Content); i += 2 {
		if slices.Contains(names, node.Content[i].Value) {
			content = append(content, node.Content[i], node.Content[i+1])
		}
	}
	node.Content = content
}

func retainAuthoredPath(root *yaml.Node, path []string, names ...string) {
	node := root
	for _, name := range path {
		node = mappingValue(node, name)
	}
	if node == nil {
		return
	}
	previous := len(node.Content)
	retainAuthoredKeys(node, names...)
	if previous > 0 && len(node.Content) == 0 {
		removeAuthoredPath(root, path)
	}
}

func removeAuthoredPath(node *yaml.Node, path []string) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	index := mappingKeyIndex(node, path[0])
	if index < 0 {
		return
	}
	if len(path) == 1 {
		node.Content = append(node.Content[:index], node.Content[index+2:]...)
		return
	}
	child := node.Content[index+1]
	previous := len(child.Content)
	removeAuthoredPath(child, path[1:])
	if child.Kind == yaml.MappingNode && previous > 0 && len(child.Content) == 0 {
		node.Content = append(node.Content[:index], node.Content[index+2:]...)
	}
}

func authoredSourceFiles(sources ProjectDefinitionSources) map[string]string {
	files := map[string]string{"WORKFLOW.md": string(sources.Workflow)}
	if sources.HasConfig {
		files["detent.yaml"] = string(sources.Config)
	}
	if sources.HasLocalWorkflow {
		files["WORKFLOW.local.md"] = string(sources.LocalWorkflow)
	}
	if sources.HasLocalConfig {
		files["detent.local.yaml"] = string(sources.LocalConfig)
	}
	if sources.HasAgents {
		files["AGENTS.md"] = string(sources.Agents)
	}
	return files
}

func sourcesFromAuthoredFiles(files map[string]string) ProjectDefinitionSources {
	sources := ProjectDefinitionSources{Workflow: []byte(files["WORKFLOW.md"]), WorkflowPath: "WORKFLOW.md", ConfigPath: "detent.yaml"}
	value, present := files["detent.yaml"]
	sources.Config, sources.HasConfig = []byte(value), present
	value, present = files["WORKFLOW.local.md"]
	sources.LocalWorkflow, sources.HasLocalWorkflow = []byte(value), present
	value, present = files["detent.local.yaml"]
	sources.LocalConfig, sources.HasLocalConfig = []byte(value), present
	value, present = files["AGENTS.md"]
	sources.Agents, sources.HasAgents = []byte(value), present
	return sources
}

func authoredProjectDefinitionVersion(sources ProjectDefinitionSources, version int) (*policy.Authored, error) {
	files := make(map[string]any)
	for _, source := range []struct {
		name     string
		raw      []byte
		present  bool
		markdown bool
	}{
		{"WORKFLOW.md", sources.Workflow, true, true},
		{"detent.yaml", sources.Config, sources.HasConfig, false},
		{"WORKFLOW.local.md", sources.LocalWorkflow, sources.HasLocalWorkflow, true},
		{"detent.local.yaml", sources.LocalConfig, sources.HasLocalConfig, false},
		{"AGENTS.md", sources.Agents, sources.HasAgents, true},
	} {
		if !source.present {
			continue
		}
		raw := source.raw
		prompt := ""
		if source.markdown {
			document, err := splitProjectWorkflow(raw)
			if err != nil {
				return nil, err
			}
			raw = document.frontmatter
			prompt = strings.TrimSpace(string(normalizeProjectDefinitionPrompt(document.prompt)))
		}
		var node yaml.Node
		if err := yaml.Unmarshal(raw, &node); err != nil {
			return nil, fmt.Errorf("canonicalize %s: %w", source.name, err)
		}
		value, err := canonicalAuthoredNode(&node)
		if err != nil {
			return nil, fmt.Errorf("canonicalize %s: %w", source.name, err)
		}
		files[source.name] = struct {
			Config any
			Prompt string
		}{value, prompt}
	}
	var value any
	switch version {
	case 1:
		value = files
	case 2:
		value = struct{ Files map[string]any }{files}
	default:
		return nil, fmt.Errorf("unsupported policy canonicalization version %d", version)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &policy.Authored{Version: version, Digest: policy.Digest(raw)}, nil
}

func canonicalAuthoredNode(node *yaml.Node) (result any, resultErr error) {
	defer func() {
		if node.HeadComment != "" || node.LineComment != "" || node.FootComment != "" {
			result = struct {
				Head, Line, Foot string
				Value            any
			}{node.HeadComment, node.LineComment, node.FootComment, result}
		}
	}()
	switch node.Kind {
	case 0:
		return json.RawMessage("null"), nil
	case yaml.DocumentNode:
		return canonicalAuthoredNode(node.Content[0])
	case yaml.MappingNode:
		type entry struct{ Key, Value any }
		entries := make(map[string]entry)
		keys := make([]string, 0, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			key, err := canonicalAuthoredNode(node.Content[i])
			if err != nil {
				return nil, err
			}
			raw, err := json.Marshal(key)
			if err != nil {
				return nil, err
			}
			name := string(raw)
			if _, exists := entries[name]; exists {
				return nil, fmt.Errorf("duplicate YAML key %s", name)
			}
			value, err := canonicalAuthoredNode(node.Content[i+1])
			if err != nil {
				return nil, err
			}
			entries[name] = entry{key, value}
			keys = append(keys, name)
		}
		sort.Strings(keys)
		result := make([]entry, 0, len(keys))
		for _, key := range keys {
			result = append(result, entries[key])
		}
		return result, nil
	case yaml.SequenceNode:
		result := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			value, err := canonicalAuthoredNode(child)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		return result, nil
	case yaml.ScalarNode:
		return struct{ Tag, Value string }{node.Tag, node.Value}, nil
	case yaml.AliasNode:
		return struct{ Alias string }{node.Value}, nil
	default:
		return nil, fmt.Errorf("unsupported YAML node %d", node.Kind)
	}
}
