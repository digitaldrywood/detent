package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
)

type coordinatorProposal struct {
	ProjectID tracker.ProjectID `json:"project_id"`
	Title     string            `json:"title"`
	Objective string            `json:"objective"`
}

// proposeIssue validates a proposal and records it as an assistant status
// message so the client can render a confirmation card. It creates nothing.
func ProposeIssue(ctx context.Context, record conversations.ConversationRecord, readable []tracker.ProjectID, title, objective, projectID string, publish func(context.Context, string, conversations.ConversationMessageRecord) error) (any, error) {
	title = strings.TrimSpace(title)
	objective = strings.TrimSpace(objective)
	if title == "" || len(title) > proposalTitleBytes {
		return nil, fmt.Errorf("%w: title must contain 1 to %d bytes", ErrToolArguments, proposalTitleBytes)
	}
	if objective == "" {
		return nil, fmt.Errorf("%w: objective is required", ErrToolArguments)
	}
	objective = conversations.BoundRunes(objective, proposalBodyRunes)
	target := record.ProjectID
	if strings.TrimSpace(projectID) != "" {
		target = tracker.ProjectID(strings.TrimSpace(projectID))
	}
	if !slices.Contains(readable, target) {
		return nil, fmt.Errorf("project %s is not readable in this conversation", target)
	}
	proposal := coordinatorProposal{ProjectID: target, Title: title, Objective: objective}
	data, err := json.Marshal(map[string]any{"proposal": proposal})
	if err != nil {
		return nil, fmt.Errorf("encode proposal: %w", err)
	}
	message := conversations.ConversationMessageRecord{
		Role:     conversation.RoleAssistant,
		Kind:     conversation.MessageStatus,
		Text:     "Proposed issue: " + title,
		Data:     data,
		Delivery: conversation.DeliveryCompleted,
		Actor:    conversation.Actor{Kind: conversation.ActorCoordinator},
	}
	err = publish(ctx, record.ID, message)
	if err != nil {
		return nil, fmt.Errorf("record proposal: %w", err)
	}
	return map[string]any{"proposal": proposal}, nil
}
