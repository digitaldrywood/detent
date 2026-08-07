package chat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

func Tools() []Tool {
	definitions := operatortool.Catalog()
	tools := make([]Tool, 0, len(definitions)+4)
	for _, definition := range definitions {
		tools = append(tools, Tool{Name: definition.Name, Description: definition.Description, InputSchema: definition.InputSchema})
	}
	return append(tools,
		tool("propose_move_item", "Propose moving an item to another configured board state. This never executes the move; operator confirmation is required.", `{"type":"object","required":["project_id","identifier","target_state"],"properties":{"project_id":{"type":"string"},"identifier":{"type":"string"},"target_state":{"type":"string"}},"additionalProperties":false}`),
		tool("propose_set_priority", "Propose setting an item's configured tracker priority. This never executes the change; operator confirmation is required.", `{"type":"object","required":["project_id","identifier","priority"],"properties":{"project_id":{"type":"string"},"identifier":{"type":"string"},"priority":{"type":"string"}},"additionalProperties":false}`),
		tool("propose_stop_run", "Propose stopping an active run and atomically routing its item to Blocked, Backlog, Cancelled, or Todo with priority. This never stops a run; operator confirmation is required.", `{"type":"object","required":["project_id","identifier","destination"],"properties":{"project_id":{"type":"string"},"identifier":{"type":"string"},"destination":{"type":"string","enum":["Blocked","Backlog","Cancelled","Todo"]},"priority":{"type":"integer","minimum":1,"maximum":4},"reason":{"type":"string","maxLength":280}},"additionalProperties":false}`),
		tool("propose_file_issue", "Propose filing a new issue or work item on a configured project. This never files it; operator confirmation is required.", `{"type":"object","required":["project_id","title","description"],"properties":{"project_id":{"type":"string"},"title":{"type":"string"},"description":{"type":"string"},"state":{"type":"string"},"labels":{"type":"array","items":{"type":"string"}},"priority":{"type":"integer","minimum":1,"maximum":4}},"additionalProperties":false}`),
	)
}

func ActionSummary(action Action) string {
	switch action.Kind {
	case ActionMoveItem:
		return fmt.Sprintf("Move %s from %s to %s", actionLabel(action), action.CurrentState, action.TargetState)
	case ActionSetPriority:
		return fmt.Sprintf("Set %s priority to %s", actionLabel(action), action.Priority)
	case ActionStopRun:
		summary := fmt.Sprintf("Stop %s and move it to %s", actionLabel(action), action.Destination)
		if action.Destination == "Todo" && action.Priority != "" {
			summary += " at " + action.Priority + " priority"
		}
		return summary
	case ActionFileIssue:
		return fmt.Sprintf("File %q on %s", action.Title, action.ProjectID)
	default:
		return "Unknown operator action"
	}
}

func tool(name string, description string, schema string) Tool {
	return Tool{Name: name, Description: description, InputSchema: json.RawMessage(schema)}
}

func actionLabel(action Action) string {
	if value := strings.TrimSpace(action.Identifier); value != "" {
		return value
	}
	if value := strings.TrimSpace(action.IssueID); value != "" {
		return value
	}
	return "item"
}
