package hubserver

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
)

func (t *coordinatorToolset) workflowEvidence(ctx context.Context, record conversationRecord) map[string]any {
	evidence := map[string]any{"organization_id": record.OrganizationID, "project_id": record.ProjectID}
	for _, name := range []string{"get_project_integration", "get_project_policy", "get_runners", operatortool.LocalProjectConfiguration} {
		result, err := t.execute(ctx, runner.AgentToolCall{Name: name, Arguments: json.RawMessage(`{}`)})
		if err != nil {
			evidence[name] = map[string]string{"unavailable": coordinatorActionError(err).Error()}
		} else {
			evidence[name] = result
		}
	}
	return evidence
}
