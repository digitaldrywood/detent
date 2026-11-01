package readtools

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
)

type Reader interface {
	ReadAgentTool(context.Context, runner.AgentToolCall, time.Time) (operatortool.Result, error)
}
type Authority interface{ Validate(context.Context) error }

func Tools(reader Reader, host Authority, now func() time.Time, evidence runner.AgentTool, attach func(context.Context, json.RawMessage) (runner.AgentToolResult, error)) ([]runner.AgentTool, runner.AgentToolHandler) {
	var tools []runner.AgentTool
	definitions := append(operatortool.WorkReadCatalog(), operatortool.ChangeCatalog()...)
	definitions = append(definitions, operatortool.AttachmentCatalog()...)
	definitions = append(definitions, operatortool.LocalProjectCatalog()...)
	for _, definition := range definitions {
		switch definition.Name {
		case operatortool.WorkList, operatortool.WorkBulkExport:
			definition.Description += " Use cursor, not offset, for paging."
			tools = append(tools, runner.AgentTool{Name: definition.Name, Description: definition.Description, InputSchema: definition.InputSchema})
		case operatortool.WorkItem, operatortool.WorkComments, operatortool.WorkHistory, operatortool.WorkRuns, operatortool.BoardActivity, operatortool.WorkAttemptReceipt:
			switch definition.Name {
			case operatortool.WorkRuns:
				definition.Description = "Read a bounded page of native work-item attempt summaries. Full runtime detail is available through work_attempt_receipt."
			case operatortool.BoardActivity:
				definition.Description = "Read a bounded page of durable native work-item activity from the existing history owner."
			case operatortool.WorkAttemptReceipt:
				definition.Description += " Returns recorded native runtime evidence with freshness and unavailable fields; no provider conversation transcript is supplied."
			}
			definition.Description += " reference must be a canonical native work-item ID beginning with wi_; numbers, titles and URLs are not supported. Use cursor, not offset, for paging."
			tools = append(tools, runner.AgentTool{Name: definition.Name, Description: definition.Description, InputSchema: definition.InputSchema})
		case operatortool.RunnerProjectDiagnostics, operatortool.ListChanges, operatortool.GetChange, operatortool.ReadAttachmentMetadata, operatortool.ReadAttachment:
			if definition.Name == operatortool.GetChange {
				definition.Description += " For complete retained records, including oversized reviews, full command output and historical frozen policy, use work_item with view=recovery and follow its record fragment cursors."
			}
			tools = append(tools, runner.AgentTool{Name: definition.Name, Description: definition.Description, InputSchema: definition.InputSchema})
		}
	}
	tools = append(tools, evidence)
	return tools, func(ctx context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
		if call.Name == operatortool.RunnerProjectDiagnostics {
			result, err := reader.ReadAgentTool(ctx, call, now())
			if err != nil {
				return runner.AgentToolResult{}, err
			}
			return runner.AgentToolResult{Content: string(result.Content), Success: true}, nil
		}
		if err := host.Validate(ctx); err != nil {
			return runner.AgentToolResult{Content: "Native execution authority is unavailable"}, err
		}
		if call.Name == "attach_evidence" {
			return attach(ctx, call.Arguments)
		}
		result, err := reader.ReadAgentTool(ctx, call, now())
		if err != nil {
			var refusal *operatortool.RequestError
			if errors.As(err, &refusal) {
				if encoded, encodeErr := operatortool.EncodeResult(refusal); encodeErr == nil {
					return runner.AgentToolResult{Content: string(encoded.Content)}, err
				}
			}
			return runner.AgentToolResult{Content: err.Error()}, err
		}
		return runner.AgentToolResult{Content: string(result.Content), Success: true}, nil
	}
}
