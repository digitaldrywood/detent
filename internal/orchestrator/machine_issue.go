package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/runner"
)

func (o *Orchestrator) attachMachineIssueTool(request *RunRequest) {
	backend, ok := o.connector.(intake.IssueStore)
	if !ok {
		return
	}
	previous := request.AgentToolHandler
	source := strconv.FormatInt(request.WorkAttemptID, 10)
	request.AgentTools = append(request.AgentTools, runner.AgentTool{
		Name:        "file_machine_issue",
		Description: "File a machine-discovered follow-up in this repository's Backlog, or comment on an open issue with the same fingerprint. Use a stable problem key shared across occurrences, excluding attempt IDs, wording variations and timestamps. Inspect existing issues for their fingerprint first.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["title","body","fingerprint"],"properties":{"title":{"type":"string","minLength":1},"body":{"type":"string","minLength":1},"fingerprint":{"type":"string","minLength":1}}}`),
	})
	request.AgentToolHandler = func(ctx context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
		if call.Name != "file_machine_issue" {
			if previous != nil {
				return previous(ctx, call)
			}
			return runner.AgentToolResult{Content: "unsupported tool"}, nil
		}
		var input struct {
			Title       string `json:"title"`
			Body        string `json:"body"`
			Fingerprint string `json:"fingerprint"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(call.Arguments)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			return runner.AgentToolResult{Content: err.Error()}, nil
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) || strings.TrimSpace(input.Title) == "" || strings.TrimSpace(input.Body) == "" || strings.TrimSpace(input.Fingerprint) == "" {
			return runner.AgentToolResult{Content: "one request with title, body and fingerprint is required"}, nil
		}
		body := issueorigin.Stamp(input.Body, issueorigin.Origin{Kind: "worker", Source: source, Fingerprint: strings.TrimSpace(input.Fingerprint)})
		issue, err := backend.CreateIntakeIssue(ctx, intake.IssueDraft{Title: input.Title, Body: body})
		if err != nil {
			return runner.AgentToolResult{Content: err.Error()}, nil
		}
		if !issue.Reused {
			if err := backend.SetIntakeIssueState(ctx, issue.ID, "Backlog"); err != nil {
				return runner.AgentToolResult{Content: err.Error()}, nil
			}
		}
		return runner.AgentToolResult{Success: true, Content: issue.Identifier + " " + issue.URL}, nil
	}
}
