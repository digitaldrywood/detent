package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
)

func threadToolsMatch(path, threadID string, tools []DynamicTool) (matches bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	var metadata struct {
		Type    string `json:"type"`
		Payload struct {
			ID           string        `json:"id"`
			DynamicTools []DynamicTool `json:"dynamic_tools"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(io.LimitReader(file, 4<<20)).Decode(&metadata); err != nil {
		return false, err
	}
	if metadata.Type != "session_meta" || metadata.Payload.ID != threadID {
		return false, fmt.Errorf("codex rollout metadata does not identify thread %q", threadID)
	}
	if len(metadata.Payload.DynamicTools) != len(tools) {
		return false, nil
	}
	saved := make(map[string]DynamicTool, len(tools))
	for _, tool := range metadata.Payload.DynamicTools {
		saved[tool.Name] = tool
	}
	for _, tool := range tools {
		previous, ok := saved[tool.Name]
		if !ok || previous.Type != tool.Type || previous.Description != tool.Description {
			return false, nil
		}
		var previousSchema, currentSchema any
		if err := json.Unmarshal(previous.InputSchema, &previousSchema); err != nil {
			return false, err
		}
		if err := json.Unmarshal(tool.InputSchema, &currentSchema); err != nil {
			return false, err
		}
		if !reflect.DeepEqual(previousSchema, currentSchema) {
			return false, nil
		}
		delete(saved, tool.Name)
	}
	return len(saved) == 0, nil
}
