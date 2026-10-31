package config

import (
	"errors"
	"strings"

	"gopkg.in/yaml.v3"
)

func RetiredKeyErrors(root map[string]any, paths []string) []string {
	var problems []string
	for _, path := range paths {
		current := root
		keys := strings.Split(path, ".")
		for i, key := range keys {
			value, ok := current[key]
			if !ok {
				break
			}
			if i == len(keys)-1 {
				problems = append(problems, path+": retired configuration key")
				break
			}
			current, ok = value.(map[string]any)
			if !ok {
				break
			}
		}
	}
	return problems
}

func validateRetiredWorkflowKeys(node *yaml.Node) error {
	var root map[string]any
	if err := node.Decode(&root); err != nil {
		return err
	}
	problems := RetiredKeyErrors(root, retiredWorkflowKeys)
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

var retiredWorkflowKeys = []string{
	"intake", "routines", "retro", "backlog_admission",
	"tracker.github_status_source", "tracker.project_slug", "tracker.status_field", "tracker.status_label_prefix", "tracker.write_probe_issue",
	"server.host", "server.port", "server.board_snapshot_stale_after_seconds",
}
