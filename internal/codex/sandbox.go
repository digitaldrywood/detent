package codex

import (
	"encoding/json"
	"maps"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
)

type Options struct {
	ApprovalPolicy    any
	ThreadSandbox     string
	TurnSandboxPolicy any
}

func OptionsFromConfig(cfg config.Codex) Options {
	return Options{
		ApprovalPolicy:    stringOrMapValue(cfg.ApprovalPolicy),
		ThreadSandbox:     cfg.ThreadSandbox,
		TurnSandboxPolicy: cfg.TurnSandboxPolicy,
	}
}

func turnSandboxPolicyForWorkspace(threadSandbox string, policy any, extraWritableRoots []string) any {
	policyMap, ok := workspaceWriteSandboxPolicyMap(threadSandbox, policy)
	if !ok {
		return policy
	}
	if len(extraWritableRoots) == 0 {
		return policy
	}
	return mergeSandboxWritableRoots(policyMap, extraWritableRoots)
}

func workspaceWriteSandboxPolicyMap(threadSandbox string, policy any) (map[string]any, bool) {
	policyMap, ok := sandboxPolicyMap(policy)
	if !ok {
		return nil, false
	}
	policyKind := strings.TrimSpace(policyType(policyMap))
	if isWorkspaceWriteSandboxName(policyKind) {
		return policyMap, true
	}
	if policyKind != "" || !isWorkspaceWriteSandboxName(threadSandbox) {
		return nil, false
	}
	policyMap["type"] = "workspaceWrite"
	return policyMap, true
}

func sandboxPolicyMap(policy any) (map[string]any, bool) {
	if policy == nil {
		return map[string]any{}, true
	}
	switch value := policy.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		maps.Copy(out, value)
		return out, true
	case json.RawMessage:
		return decodeSandboxPolicyMap(value)
	case []byte:
		return decodeSandboxPolicyMap(value)
	case string:
		return decodeSandboxPolicyMap([]byte(value))
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, false
		}
		return decodeSandboxPolicyMap(raw)
	}
}

func decodeSandboxPolicyMap(raw []byte) (map[string]any, bool) {
	var policy map[string]any
	if err := json.Unmarshal(raw, &policy); err != nil {
		return nil, false
	}
	if policy == nil {
		policy = map[string]any{}
	}
	return policy, true
}

func policyType(policy map[string]any) string {
	value, ok := policy["type"]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return text
}

func isWorkspaceWriteSandboxName(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	return normalized == "workspace-write" || normalized == "workspacewrite"
}

func mergeSandboxWritableRoots(policy map[string]any, roots []string) map[string]any {
	merged := []string{}
	merged = appendPolicyStringSlice(merged, policy["writableRoots"])
	merged = appendPolicyStringSlice(merged, policy["writable_roots"])
	merged = appendUniqueStrings(merged, roots...)
	if strings.TrimSpace(policyType(policy)) == "" {
		policy["type"] = "workspaceWrite"
	}
	policy["writableRoots"] = merged
	delete(policy, "writable_roots")
	return policy
}

func appendPolicyStringSlice(out []string, value any) []string {
	switch values := value.(type) {
	case []string:
		return appendUniqueStrings(out, values...)
	case []any:
		for _, item := range values {
			text, ok := item.(string)
			if !ok {
				continue
			}
			out = appendUniqueStrings(out, text)
		}
	}
	return out
}

func appendUniqueStrings(out []string, values ...string) []string {
	seen := make(map[string]struct{}, len(out)+len(values))
	for _, value := range out {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func stringOrMapValue(value config.StringOrMap) any {
	if value.IsMap {
		return cloneMap(value.Map)
	}
	if value.IsString {
		return value.String
	}
	return nil
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}

	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var cloned map[string]any
	if err := json.Unmarshal(data, &cloned); err != nil {
		return value
	}
	return cloned
}
