package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

func textOnlySettings(workspace string) map[string]any {
	settings := map[string]any{
		"features.network_proxy": false,
		"web_search":             "disabled",
		"default_permissions":    "detent-text-only",
		"permissions": map[string]any{"detent-text-only": map[string]any{
			"filesystem":      map[string]any{"/": "deny", ":workspace_roots": "read"},
			"workspace_roots": map[string]any{workspace: true},
			"network":         map[string]any{"enabled": false},
		}},
	}
	for _, feature := range []string{
		"shell_tool", "unified_exec", "shell_snapshot", "view_image", "code_mode", "code_mode_host",
		"apps", "plugins", "browser_use", "computer_use", "image_generation", "multi_agent", "multi_agent_v2",
		"goals", "tool_suggest", "skill_search", "hooks", "memories", "sleep_tool",
	} {
		settings["features."+feature] = false
	}
	return settings
}

func (s *AppServer) disableTextOnlyMCP(ctx context.Context, transport Transport, req *RunTurnRequest) error {
	if err := sendRequest(ctx, transport, configReadRequestID, "config/read", map[string]any{"cwd": req.Workspace, "includeLayers": false}); err != nil {
		return err
	}
	raw, err := s.awaitResponse(ctx, transport, configReadRequestID, nil, nil, nil)
	if err != nil {
		return fmt.Errorf("read text-only tool configuration: %w", err)
	}
	var response struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return errors.New("invalid text-only tool configuration")
	}
	if response.Config == nil {
		return errors.New("text-only tool configuration is unavailable")
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := response.Config["mcp_servers"]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return errors.New("invalid text-only MCP configuration")
		}
	}
	disabled := make(map[string]any, len(servers))
	for name := range servers {
		disabled[name] = map[string]any{"enabled": false}
	}
	if len(disabled) > 0 {
		req.Config["mcp_servers"] = disabled
	}
	return nil
}
