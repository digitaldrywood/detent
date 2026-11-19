package coordinator

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

const AgentName = "Luna"

func Agent(model string) string {
	if model == "" {
		return AgentName
	}
	return AgentName + " (" + model + ")"
}

// coordinatorProposalAgent reads the agent from the proposal the coordinator
// stored, never from the submitted action, so a client cannot relabel who
// acted.
func ProposalAgent(ctx context.Context, r tenantstore.Reader, action chat.Action) (string, error) {
	agent := AgentName
	page, err := r.FilteredMessages(ctx, conversationstore.MessageFilter{ConversationID: action.ConversationID, OperatorActionID: action.ID, Limit: 1})
	if err != nil {
		return agent, err
	}
	if len(page.Items) > 0 && page.Items[0].OperatorAgent != nil && *page.Items[0].OperatorAgent != "" {
		agent = *page.Items[0].OperatorAgent
	}
	return agent, nil
}

func WithAgent(ctx context.Context, agent string) context.Context {
	if resolve, ok := ctx.Value(hubscope.NativeOperatorScopeKey{}).(func(context.Context) (hubscope.NativeScope, error)); ok {
		ctx = context.WithValue(ctx, hubscope.NativeOperatorScopeKey{}, func(ctx context.Context) (hubscope.NativeScope, error) {
			scope, err := resolve(ctx)
			scope.Credential.Agent = agent
			return scope, err
		})
	}
	return ctx
}
