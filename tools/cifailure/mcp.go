package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

type cloudMCP struct {
	tools      map[string]operatortool.Definition
	diagnostic bool
	endpoint   string
	token      string
	session    string
	version    string
	sequence   int
	client     *http.Client
}

func (m *cloudMCP) request(ctx context.Context, method string, params any, notification bool, result any) error {
	m.sequence++
	message := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if !notification {
		message["id"] = m.sequence
	}
	body, err := json.Marshal(message)
	if err != nil {
		return errors.New("scheduled MCP request could not be encoded")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("scheduled MCP request could not be constructed")
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if m.session != "" {
		req.Header.Set("Mcp-Session-Id", m.session)
	}
	if m.version != "" {
		req.Header.Set("Mcp-Protocol-Version", m.version)
	}
	response, err := m.client.Do(req)
	if err != nil {
		return errors.Join(errors.New("scheduled MCP transport unavailable; retained evidence requires retry"), ctx.Err())
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("scheduled MCP request denied or unavailable (HTTP %d)", response.StatusCode)
	}
	if method == "initialize" {
		m.session = response.Header.Get("Mcp-Session-Id")
	}
	if notification {
		return nil
	}
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&envelope); err != nil || envelope.JSONRPC != "2.0" || envelope.ID != m.sequence {
		return errors.Join(errors.New("scheduled MCP returned an invalid or failed response"), ctx.Err())
	}
	if len(envelope.Error) != 0 && string(envelope.Error) != "null" {
		if m.diagnostic {
			return fmt.Errorf("MCP server error: %s", m.redact(envelope.Error))
		}
		var rpcError struct {
			Code int `json:"code"`
		}
		if json.Unmarshal(envelope.Error, &rpcError) == nil {
			switch rpcError.Code {
			case -32700, -32600, -32601, -32602, -32603:
				return fmt.Errorf("scheduled MCP request rejected (JSON-RPC code %d)", rpcError.Code)
			}
		}
		return errors.New("scheduled MCP returned an invalid or failed response")
	}
	if len(envelope.Result) == 0 {
		return errors.New("scheduled MCP returned an invalid or failed response")
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return errors.New("scheduled MCP result could not be decoded")
	}
	return nil
}

func (m *cloudMCP) initialize(ctx context.Context) error {
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := m.request(ctx, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "detent-scheduled-ci", "version": "1"}}, false, &initialized); err != nil {
		return err
	}
	if initialized.ProtocolVersion != "2025-06-18" || m.session == "" {
		return errors.New("scheduled MCP connection did not negotiate the supported transport")
	}
	m.version = initialized.ProtocolVersion
	if err := m.request(ctx, "notifications/initialized", map[string]any{}, true, nil); err != nil {
		return err
	}
	available := map[string]bool{}
	m.tools = map[string]operatortool.Definition{}
	cursor := ""
	for {
		var page struct {
			Tools      []operatortool.Definition `json:"tools"`
			NextCursor string                    `json:"nextCursor"`
		}
		args := map[string]any{}
		if cursor != "" {
			args["cursor"] = cursor
		}
		if err := m.request(ctx, "tools/list", args, false, &page); err != nil {
			return err
		}
		for _, tool := range page.Tools {
			available[tool.Name] = true
			m.tools[tool.Name] = tool
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			return errors.New("scheduled MCP discovery did not advance")
		}
		cursor = page.NextCursor
	}
	for _, name := range []string{"work_config", "work_list", "work_item", "work_comments", "file_issue", "edit_item", "move_item", "add_comment"} {
		if !available[name] {
			return fmt.Errorf("scheduled MCP connection lacks %s authority", name)
		}
	}
	return nil
}

func (m *cloudMCP) call(ctx context.Context, name string, args map[string]any, result any) error {
	var response struct {
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := m.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, false, &response); err != nil {
		return err
	}
	if response.IsError {
		if m.diagnostic {
			raw, encodeErr := json.Marshal(response)
			if encodeErr != nil {
				return errors.New("MCP server error could not be decoded")
			}
			return fmt.Errorf("MCP server error: %s", m.redact(raw))
		}
		return fmt.Errorf("scheduled Cloud %s denied or unavailable; retained evidence requires retry", name)
	}
	content := response.StructuredContent
	if len(content) == 0 && len(response.Content) == 1 && response.Content[0].Type == "text" {
		content = []byte(response.Content[0].Text)
	}
	if len(content) == 0 || string(content) == "null" || json.Unmarshal(content, result) != nil {
		return errors.New("scheduled Cloud result could not be decoded")
	}
	return nil
}

func (m *cloudMCP) redact(raw []byte) string {
	value := string(raw)
	if m.token != "" {
		value = strings.ReplaceAll(value, m.token, "[redacted]")
	}
	if len(value) > 4096 {
		value = value[:4096] + " [truncated]"
	}
	return value
}
