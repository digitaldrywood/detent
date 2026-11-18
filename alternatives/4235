package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

type ConnectionExecutor struct {
	tools   []operatortool.Definition
	execute func(context.Context, operatortool.Call) (operatortool.Result, error)
}

func NewConnectionExecutor(tools []operatortool.Definition, execute func(context.Context, operatortool.Call) (operatortool.Result, error)) (*ConnectionExecutor, error) {
	if execute == nil || len(tools) == 0 || len(tools) > 256 {
		return nil, errors.New("invalid connection tool catalog")
	}
	result := &ConnectionExecutor{execute: execute}
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) == "" || result.hasTool(tool.Name) || !isJSONObject(tool.InputSchema) {
			return nil, errors.New("invalid connection tool definition")
		}
		tool.InputSchema = append(json.RawMessage(nil), tool.InputSchema...)
		result.tools = append(result.tools, tool)
	}
	return result, nil
}

func (e *ConnectionExecutor) hasTool(name string) bool {
	for _, tool := range e.tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func (e *ConnectionExecutor) Execute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	if !e.hasTool(call.Name) {
		return operatortool.Result{}, operatortool.ErrUnknownTool
	}
	return e.execute(ctx, call)
}
