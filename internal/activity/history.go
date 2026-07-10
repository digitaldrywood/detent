package activity

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var ErrHistoryNotFound = errors.New("rollout history not found")

type HistoryReader interface {
	Page(context.Context, HistoryQuery) (HistoryPage, error)
}

type HistoryQuery struct {
	BackendKind       string
	ProviderThreadID  string
	ProviderSessionID string
	Offset            int
	Limit             int
}

type HistoryPage struct {
	Events  []Event
	Offset  int
	Limit   int
	HasMore bool
}

type rolloutHistoryReader struct {
	codexRoot  string
	claudeRoot string
}

func NewRolloutHistoryReader(codexRoot string, claudeRoot string) HistoryReader {
	home := ""
	if resolvedHome, err := os.UserHomeDir(); err == nil {
		home = resolvedHome
	}
	if codexRoot = strings.TrimSpace(codexRoot); codexRoot == "" {
		codexRoot = strings.TrimSpace(os.Getenv("CODEX_HOME"))
		if codexRoot == "" && home != "" {
			codexRoot = filepath.Join(home, ".codex")
		}
	}
	if claudeRoot = strings.TrimSpace(claudeRoot); claudeRoot == "" && home != "" {
		claudeRoot = filepath.Join(home, ".claude")
	}
	return &rolloutHistoryReader{codexRoot: codexRoot, claudeRoot: claudeRoot}
}

func (r *rolloutHistoryReader) Page(ctx context.Context, query HistoryQuery) (HistoryPage, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := query.Offset
	if offset < 0 {
		offset = 0
	}
	root, sessionID := r.historyLocation(query)
	if root == "" || sessionID == "" {
		return HistoryPage{}, ErrHistoryNotFound
	}
	path, err := newestMatchingHistoryFile(ctx, root, sessionID)
	if err != nil {
		return HistoryPage{}, err
	}
	events, err := readHistoryEvents(ctx, path, offset, limit+1)
	if err != nil {
		return HistoryPage{}, err
	}
	page := HistoryPage{Events: events, Offset: offset, Limit: limit}
	if len(page.Events) > limit {
		page.Events = page.Events[:limit]
		page.HasMore = true
	}
	return page, nil
}

func (r *rolloutHistoryReader) historyLocation(query HistoryQuery) (string, string) {
	if strings.Contains(strings.ToLower(query.BackendKind), "claude") {
		return filepath.Join(r.claudeRoot, "projects"), strings.TrimSpace(query.ProviderSessionID)
	}
	return filepath.Join(r.codexRoot, "sessions"), firstHistoryValue(query.ProviderThreadID, query.ProviderSessionID)
}

func newestMatchingHistoryFile(ctx context.Context, root string, sessionID string) (string, error) {
	type candidate struct {
		path    string
		modTime time.Time
	}
	candidates := make([]candidate, 0, 1)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".jsonl") || !strings.Contains(entry.Name(), sessionID) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate{path: path, modTime: info.ModTime()})
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrHistoryNotFound
		}
		return "", fmt.Errorf("scan rollout history: %w", err)
	}
	if len(candidates) == 0 {
		return "", ErrHistoryNotFound
	}
	sort.Slice(candidates, func(i int, j int) bool { return candidates[i].modTime.After(candidates[j].modTime) })
	return candidates[0].path, nil
}

func readHistoryEvents(ctx context.Context, path string, offset int, limit int) (result []Event, resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open rollout history: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()

	events := make([]Event, 0, limit)
	seen := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, event := range historyEventsFromLine(scanner.Bytes()) {
			if seen < offset {
				seen++
				continue
			}
			events = append(events, event)
			seen++
			if len(events) >= limit {
				return events, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read rollout history: %w", err)
	}
	return events, nil
}

func historyEventsFromLine(line []byte) []Event {
	var envelope struct {
		Timestamp string          `json:"timestamp"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
		Message   json.RawMessage `json:"message"`
	}
	if json.Unmarshal(line, &envelope) != nil {
		return nil
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(envelope.Timestamp))
	if err != nil {
		at = time.Now().UTC()
	}
	if len(envelope.Message) > 0 {
		return historyMessageEvents(at, envelope.Type, envelope.Message)
	}
	if len(envelope.Payload) == 0 {
		return nil
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(envelope.Payload, &payload) != nil {
		return nil
	}
	typeName := rawString(payload["type"])
	switch typeName {
	case "agent_message":
		return historyTextEvent(at, "assistant", "Agent", rawString(payload["message"]))
	case "user_message":
		return historyTextEvent(at, "user", "Prompt", rawString(payload["message"]))
	case "message":
		return historyContentEvents(at, rawString(payload["role"]), payload["content"])
	case "function_call", "custom_tool_call":
		return historyTextEvent(at, "tool_started", "Tool started · "+firstHistoryValue(rawString(payload["name"]), rawString(payload["tool"])), firstHistoryValue(rawString(payload["arguments"]), rawString(payload["input"])))
	case "function_call_output", "custom_tool_call_output":
		return historyTextEvent(at, "tool_output", "Tool output", firstHistoryValue(rawString(payload["output"]), rawJSON(payload["output"])))
	default:
		return nil
	}
}

func historyMessageEvents(at time.Time, eventType string, raw json.RawMessage) []Event {
	var message map[string]json.RawMessage
	if json.Unmarshal(raw, &message) != nil {
		return nil
	}
	role := firstHistoryValue(rawString(message["role"]), eventType)
	return historyContentEvents(at, role, message["content"])
}

func historyContentEvents(at time.Time, role string, raw json.RawMessage) []Event {
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return historyTextEvent(at, role, historyRoleTitle(role), rawString(raw))
	}
	events := make([]Event, 0, len(blocks))
	for _, block := range blocks {
		typeName := rawString(block["type"])
		switch typeName {
		case "text", "output_text", "input_text":
			events = append(events, historyTextEvent(at, role, historyRoleTitle(role), firstHistoryValue(rawString(block["text"]), rawString(block["content"])))...)
		case "tool_use":
			events = append(events, historyTextEvent(at, "tool_started", "Tool started · "+rawString(block["name"]), rawJSON(block["input"]))...)
		case "tool_result":
			events = append(events, historyTextEvent(at, "tool_output", "Tool output", firstHistoryValue(rawString(block["content"]), rawJSON(block["content"])))...)
		}
	}
	return events
}

func historyTextEvent(at time.Time, kind string, title string, content string) []Event {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	return []Event{{At: at, Kind: kind, Title: title, Content: content}}
}

func historyRoleTitle(role string) string {
	if strings.EqualFold(strings.TrimSpace(role), "assistant") {
		return "Agent"
	}
	return "Prompt"
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	return ""
}

func rawJSON(raw json.RawMessage) string {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func firstHistoryValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
