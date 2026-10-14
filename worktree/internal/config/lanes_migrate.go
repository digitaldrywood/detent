package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

func MigrateTrackerLanes(raw []byte, laneOrder ...string) ([]byte, bool, error) {
	document, err := splitProjectWorkflow(raw)
	if err != nil {
		return nil, false, err
	}
	yamlRaw := raw
	if document.hasFrontmatter {
		yamlRaw = document.frontmatter
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(yamlRaw, &doc); err != nil {
		return nil, false, err
	}
	root, err := documentRoot(&doc)
	if err != nil {
		return nil, false, err
	}
	if root.Kind != yaml.MappingNode {
		return raw, false, nil
	}
	trackerIndex := mappingKeyIndex(root, "tracker")
	if trackerIndex < 0 {
		return raw, false, nil
	}
	trackerNode := root.Content[trackerIndex+1]
	indent := 2
	if trackerNode.Kind == yaml.MappingNode && trackerNode.Style&yaml.FlowStyle == 0 && len(trackerNode.Content) > 0 {
		if width := trackerNode.Content[0].Column - root.Content[trackerIndex].Column; width > 0 {
			indent = width
		}
	}
	cfg, err := decodeWorkflowConfig(root)
	if err != nil {
		return nil, false, err
	}
	if !cfg.Tracker.legacyStates {
		return raw, false, nil
	}
	var problems []string
	validateStateList("tracker.observed_states", cfg.Tracker.ObservedStates, &problems)
	validateStateList("tracker.active_states", cfg.Tracker.ActiveStates, &problems)
	validateStateList("tracker.terminal_states", cfg.Tracker.TerminalStates, &problems)
	if len(problems) > 0 {
		return nil, false, ValidationError{Problems: problems}
	}
	trackerNode, err = expandedTrackerMapping(trackerNode)
	if err != nil {
		return nil, false, err
	}
	root.Content[trackerIndex+1] = trackerNode
	lanes := cfg.Tracker.WorkflowLanes()
	if len(laneOrder) > 0 {
		byName := make(map[string]Lane, len(lanes))
		for _, lane := range lanes {
			byName[kanbanPolicyStateKey(lane.Name)] = lane
		}
		ordered := make([]Lane, 0, len(laneOrder))
		for _, name := range laneOrder {
			key := kanbanPolicyStateKey(name)
			lane, exists := byName[key]
			if !exists {
				return nil, false, fmt.Errorf("stored workflow lane %q is missing from Markdown", name)
			}
			ordered = append(ordered, lane)
			delete(byName, key)
		}
		if len(byName) > 0 {
			return nil, false, fmt.Errorf("markdown has %d lanes missing from stored workflow", len(byName))
		}
		lanes = ordered
	}
	var lanesNode yaml.Node
	if err := lanesNode.Encode(lanes); err != nil {
		return nil, false, err
	}
	content := make([]*yaml.Node, 0, len(trackerNode.Content))
	inserted := false
	for index := 0; index+1 < len(trackerNode.Content); index += 2 {
		key := trackerNode.Content[index]
		switch key.Value {
		case "observed_states", "active_states", "terminal_states":
			if !inserted {
				laneKey := *key
				laneKey.Value = "lanes"
				content = append(content, &laneKey, &lanesNode)
				inserted = true
			}
		default:
			content = append(content, key, trackerNode.Content[index+1])
		}
	}
	trackerNode.Content = content
	var encoded bytes.Buffer
	encoder := yaml.NewEncoder(&encoded)
	encoder.SetIndent(indent)
	if err := encoder.Encode(&doc); err != nil {
		return nil, false, err
	}
	if err := encoder.Close(); err != nil {
		return nil, false, err
	}
	out := encoded.Bytes()
	if bytes.Contains(yamlRaw, []byte("\r\n")) {
		out = bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
	}
	if document.hasFrontmatter {
		document.frontmatter = out
		out = legacyWorkflowBytes(document)
	}
	if bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) {
		out = append([]byte{0xef, 0xbb, 0xbf}, out...)
	}
	return out, true, nil
}

func MigrateTrackerLanesFile(path string) (changed bool, resultErr error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, root.Close())
	}()
	name := filepath.Base(path)
	raw, err := root.ReadFile(name)
	if err != nil {
		return false, err
	}
	out, changed, err := MigrateTrackerLanes(raw)
	if err != nil || !changed {
		return false, err
	}
	info, err := root.Stat(name)
	if err != nil {
		return false, err
	}
	if err := root.WriteFile(name, out, info.Mode().Perm()); err != nil {
		return false, err
	}
	return true, nil
}

func expandedTrackerMapping(node *yaml.Node) (*yaml.Node, error) {
	aliased := node.Kind == yaml.AliasNode
	for node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	copy := *node
	if aliased {
		copy.Anchor = ""
	}
	if mappingKeyIndex(node, "<<") < 0 {
		copy.Content = append([]*yaml.Node(nil), node.Content...)
		return &copy, nil
	}
	var fields map[string]yaml.Node
	if err := node.Decode(&fields); err != nil {
		return nil, err
	}
	copy.Content = nil
	for index := 0; index+1 < len(node.Content); index += 2 {
		key := node.Content[index]
		if key.Value == "<<" {
			continue
		}
		copy.Content = append(copy.Content, key, node.Content[index+1])
		delete(fields, key.Value)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := fields[key]
		copy.Content = append(copy.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &value)
	}
	return &copy, nil
}
