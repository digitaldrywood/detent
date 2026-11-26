package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

const (
	securityAuditMaxToolCalls   = 24
	securityAuditMaxToolBytes   = 256 * 1024
	securityAuditMaxFileLines   = 400
	securityAuditMaxFileBytes   = 32 * 1024
	securityAuditMaxReadBuffer  = 512 * 1024
	securityAuditMaxSearchLines = 50
	securityAuditMaxSearchBytes = 16 * 1024
	securityAuditGitToolTimeout = 10 * time.Second
)

func securityAuditRepoTools(repoPath, headSHA string) ([]AgentTool, AgentToolHandler) {
	tools := []AgentTool{
		{
			Name:        "read_repo_file",
			Description: "Read a UTF-8 text file directly from the audited Git commit tree (head_sha). Untracked and ignored files are not accessible.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Repository-relative file path"},"start_line":{"type":"integer","description":"1-based start line (default 1)"},"end_line":{"type":"integer","description":"1-based inclusive end line (default start_line+399)"}},"required":["path"],"additionalProperties":false}`),
		},
		{
			Name:        "search_repo",
			Description: "Search for a literal substring directly in the audited Git commit tree (head_sha). Untracked and ignored files are not searched.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Literal search pattern"},"path_prefix":{"type":"string","description":"Optional repository-relative path or directory prefix"}},"required":["pattern"],"additionalProperties":false}`),
		},
	}
	handler := func(ctx context.Context, call AgentToolCall) (AgentToolResult, error) {
		switch call.Name {
		case "read_repo_file":
			return handleSecurityAuditReadRepoFile(ctx, repoPath, headSHA, call.Arguments), nil
		case "search_repo":
			return handleSecurityAuditSearchRepo(ctx, repoPath, headSHA, call.Arguments), nil
		default:
			return AgentToolResult{Success: false, Content: "unsupported security audit tool: " + call.Name}, nil
		}
	}
	return tools, handler
}

func cleanAuditRepoPath(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.ContainsRune(trimmed, 0) || strings.Contains(trimmed, ":") || strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "-") {
		return "", errors.New("invalid repository-relative path")
	}
	cleaned := path.Clean(strings.ReplaceAll(trimmed, "\\", "/"))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", errors.New("path escapes repository root")
	}
	return cleaned, nil
}

func runSecurityAuditGit(ctx context.Context, repoPath string, maxBytes int, args ...string) ([]byte, int, bool, error) {
	gitCtx, cancel := context.WithTimeout(ctx, securityAuditGitToolTimeout)
	defer cancel()
	fullArgs := append([]string{"--no-replace-objects", "-C", repoPath}, args...)
	cmd := exec.CommandContext(gitCtx, "git", fullArgs...)
	cmd.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, -1, false, fmt.Errorf("git %s stdout pipe: %w", args[0], err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, -1, false, fmt.Errorf("git %s start: %w", args[0], err)
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, int64(maxBytes)+1))
	truncated := len(data) > maxBytes
	if truncated {
		data = data[:maxBytes]
		cancel()
	}
	waitErr := cmd.Wait()
	if readErr != nil && !truncated {
		return nil, -1, false, fmt.Errorf("git %s read: %w", args[0], readErr)
	}
	if truncated {
		return data, 0, true, nil
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return data, exitErr.ExitCode(), false, nil
		}
		return nil, -1, false, fmt.Errorf("git %s: %w", args[0], waitErr)
	}
	return data, 0, false, nil
}

func handleSecurityAuditReadRepoFile(ctx context.Context, repoPath, headSHA string, rawArgs json.RawMessage) AgentToolResult {
	var args struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return AgentToolResult{Success: false, Content: "invalid arguments: " + err.Error()}
	}
	cleanPath, err := cleanAuditRepoPath(args.Path)
	if err != nil {
		return AgentToolResult{Success: false, Content: err.Error()}
	}
	out, code, streamTruncated, err := runSecurityAuditGit(ctx, repoPath, securityAuditMaxReadBuffer, "cat-file", "blob", headSHA+":"+cleanPath)
	if err != nil {
		return AgentToolResult{Success: false, Content: err.Error()}
	}
	if code != 0 {
		return AgentToolResult{Success: false, Content: fmt.Sprintf("file not found in commit %s: %s", headSHA, cleanPath)}
	}
	if bytes.IndexByte(out, 0) >= 0 {
		return AgentToolResult{Success: false, Content: "binary file cannot be displayed: " + cleanPath}
	}
	lines := strings.Split(string(out), "\n")
	start := max(1, args.StartLine)
	end := args.EndLine
	if end <= 0 || end < start {
		end = start + securityAuditMaxFileLines - 1
	}
	if end-start+1 > securityAuditMaxFileLines {
		end = start + securityAuditMaxFileLines - 1
	}
	if start > len(lines) {
		if streamTruncated {
			return AgentToolResult{Success: true, Content: fmt.Sprintf("%s exceeded the %d-byte read limit before line %d (total line count unknown)", cleanPath, securityAuditMaxReadBuffer, start)}
		}
		return AgentToolResult{Success: true, Content: fmt.Sprintf("%s has %d lines (requested start_line %d)", cleanPath, len(lines), start)}
	}
	if end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	truncated := streamTruncated
	for i := start; i <= end; i++ {
		line := fmt.Sprintf("%d: %s\n", i, lines[i-1])
		if b.Len()+len(line) > securityAuditMaxFileBytes {
			truncated = true
			if b.Len() == 0 {
				b.WriteString(line[:securityAuditMaxFileBytes])
				b.WriteString("\n")
			}
			break
		}
		b.WriteString(line)
	}
	if truncated {
		b.WriteString("... (truncated)\n")
	}
	return AgentToolResult{Success: true, Content: b.String()}
}

func handleSecurityAuditSearchRepo(ctx context.Context, repoPath, headSHA string, rawArgs json.RawMessage) AgentToolResult {
	var args struct {
		Pattern    string `json:"pattern"`
		PathPrefix string `json:"path_prefix"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return AgentToolResult{Success: false, Content: "invalid arguments: " + err.Error()}
	}
	pattern := strings.TrimSpace(args.Pattern)
	if pattern == "" || strings.ContainsRune(pattern, 0) {
		return AgentToolResult{Success: false, Content: "pattern is required"}
	}
	gitArgs := []string{"grep", "-n", "-I", "-F", "-e", pattern, headSHA, "--"}
	if strings.TrimSpace(args.PathPrefix) != "" {
		cleanPrefix, err := cleanAuditRepoPath(args.PathPrefix)
		if err != nil {
			return AgentToolResult{Success: false, Content: err.Error()}
		}
		gitArgs = append(gitArgs, cleanPrefix)
	}
	out, code, streamTruncated, err := runSecurityAuditGit(ctx, repoPath, securityAuditMaxSearchBytes*2, gitArgs...)
	if err != nil {
		return AgentToolResult{Success: false, Content: err.Error()}
	}
	if code == 1 {
		return AgentToolResult{Success: true, Content: "no matches"}
	}
	if code != 0 {
		return AgentToolResult{Success: false, Content: fmt.Sprintf("search failed with exit code %d", code)}
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	prefix := headSHA + ":"
	var b strings.Builder
	count := 0
	truncated := streamTruncated
	for _, line := range lines {
		if line == "" {
			continue
		}
		trimmed := strings.TrimPrefix(line, prefix) + "\n"
		if count >= securityAuditMaxSearchLines || b.Len()+len(trimmed) > securityAuditMaxSearchBytes {
			truncated = true
			if count == 0 {
				limit := min(len(trimmed), securityAuditMaxSearchBytes)
				b.WriteString(trimmed[:limit])
				if !strings.HasSuffix(trimmed[:limit], "\n") {
					b.WriteString("\n")
				}
				count = 1
			}
			break
		}
		b.WriteString(trimmed)
		count++
	}
	if count == 0 {
		return AgentToolResult{Success: true, Content: "no matches"}
	}
	if truncated {
		b.WriteString("... (truncated)\n")
	}
	return AgentToolResult{Success: true, Content: b.String()}
}
